// Package main: svc-quota — two-phase quota reservation
// (03-backend-services.md §4.3.2).
//
// Owns the occupy/release protocol (pkg-go/quota). Provisioning is async and
// can fail minutes after the order is placed, so a reservation token reserves
// capacity up front; the fulfilment saga then commits it (resource exists) or
// releases it (it didn't). Both failure directions are bounded — an
// uncommitted reservation's TTL is swept back by a background sweeper
// (sweepInterval) that runs for the life of the process. The invariant the service is
// built around: in-flight reservations count against the limit, so two
// concurrent orders cannot both claim the last slot.
//
// Routes (gateway-authorized, X-Euler-Account-Id injected):
//
//	POST /api/v1/quota/occupy  — two-phase reserve (body: productCode, resourceType, count,
//	                             optional idempotencyKey: one key = one reservation, forever)
//	POST /api/v1/quota/release — release a reservation (body: reservationId)
//	POST /api/v1/quota/commit  — commit a reservation into used capacity (body: reservationId)
//	GET  /api/v1/quota/usage   — usage position (?productCode=, ?region=)
//
// Internal (service-to-service, never routed by the gateway):
//
//	POST /internal/quota/free  — return committed capacity when a resource is
//	                             released (body: occupy's keys + amount;
//	                             idempotent by idempotencyKey, else bizKey)
//
// stdlib-HTTP service. Envelope {RequestId,Code,Data} (03§9.3). In-memory store
// (MySQL sharded by account_id + Redis read cache in production).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/quota"
	"github.com/qifalab/euler-platform/runmode"
	"github.com/qifalab/euler-platform/storage"
)

// accountIDHeader is injected by the API gateway after authentication (see
// TRUST NOTE in accountIDFrom).
const accountIDHeader = "X-Euler-Account-Id"

// maxBodyBytes caps JSON request bodies.
const maxBodyBytes = 1 << 20 // 1 MiB

// quotaStore is the service's quota persistence facade: it owns the token-id
// sequence (process-local by design) and delegates everything else to backend —
// quota.SQLStore against support_db when EULER_DB_DSN is set, memQuotaStore
// otherwise. Client idempotency keys live in the backend too
// (quota.IdempotentStore): each keyed occupy/free records its key in the same
// atomic step that moves the counter, so a key applies once across replicas and
// restarts — not once per process.
//
// The handlers pass the facade itself to quota.NewManager, so the two-phase
// protocol is wired identically with and without persistence.
type quotaStore struct {
	backend quota.Store
	// nextTokenID mints a process-unique reservation id. The service builds a
	// quota.Manager per request, and a Manager's default generator restarts at
	// "qt-1" every time — with a store-wide token table that made the second
	// request overwrite the first request's token (leaked occupancy,
	// cross-account lookups).
	nextTokenID func() string
	// persistent reports whether backend is MySQL; it is what the startup log
	// tells an operator.
	persistent bool
}

// newQuotaStore wires the persistence backend. Persistence is opt-in
// (pkg-go/storage doc): with EULER_DB_DSN set, definitions, usage counters and
// reservation tokens live in support_db, so a restart keeps both the counters and
// the in-flight reservations. Unset, the demo store is used.
//
// A configured DSN that cannot be reached — or a schema sqlmigrate never touched —
// is a startup failure: an "healthy" process whose first occupy 500s is worse than
// one that refuses to come up.
func newQuotaStore(ctx context.Context) (*quotaStore, error) {
	db, ok, err := storage.MustOpenFor(ctx, "support_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newInMemoryQuotaStore(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "support_db"); err != nil {
		return nil, err
	}
	return newSQLQuotaStore(ctx, db)
}

// newSQLQuotaStore wires the facade to a migrated support_db pool. Two facades
// on one database behave as two replicas (or one process across a restart).
func newSQLQuotaStore(ctx context.Context, db *sql.DB) (*quotaStore, error) {
	s := &quotaStore{}
	// Reservation ids come from the 号段 allocator (support_db.id_sequence,
	// 04§6.6): an in-process counter restarts at 1 and would let PutToken
	// overwrite a live reservation after a restart.
	ids, err := storage.OpenSequence(ctx, db, "quota_token", 1000)
	if err != nil {
		return nil, err
	}
	next := ids.NextFunc()
	s.backend = quota.NewSQLStore(db)
	s.nextTokenID = func() string { return fmt.Sprintf("qt-%d", next()) }
	s.persistent = true
	return s, nil
}

// newInMemoryQuotaStore builds the demo store: in-memory backend, local token-id
// counter, seeded definitions. Handler tests construct it directly, so they never
// depend on whether the developer's shell has EULER_DB_DSN set.
func newInMemoryQuotaStore() *quotaStore {
	mem := newMemQuotaStore()
	return &quotaStore{
		backend:     mem,
		nextTokenID: mem.nextTokenID,
	}
}

// The facade satisfies quota.Store by delegation, so handlers hand it to
// quota.NewManager exactly as they handed the in-memory store before.
func (s *quotaStore) GetDefinition(quotaCode string) (quota.Definition, error) {
	return s.backend.GetDefinition(quotaCode)
}

func (s *quotaStore) GetUsage(accountID int64, quotaCode, region string) (quota.Usage, error) {
	return s.backend.GetUsage(accountID, quotaCode, region)
}

func (s *quotaStore) UpdateUsage(u quota.Usage, expectedVersion int) error {
	return s.backend.UpdateUsage(u, expectedVersion)
}

func (s *quotaStore) PutToken(t quota.Token) error { return s.backend.PutToken(t) }

func (s *quotaStore) GetToken(tokenID string) (quota.Token, error) {
	return s.backend.GetToken(tokenID)
}

func (s *quotaStore) DeleteToken(tokenID string) error { return s.backend.DeleteToken(tokenID) }

func (s *quotaStore) ListExpiredTokens(now time.Time) ([]quota.Token, error) {
	return s.backend.ListExpiredTokens(now)
}

// The keyed operations delegate too; a backend that cannot record keys
// atomically (a test double wrapping only quota.Store) refuses them rather than
// falling back to a check-then-act that two replicas could both pass.
func (s *quotaStore) idempotent() (quota.IdempotentStore, error) {
	b, ok := s.backend.(quota.IdempotentStore)
	if !ok {
		return nil, quota.ErrIdempotencyUnsupported
	}
	return b, nil
}

func (s *quotaStore) GetIdempotency(accountID int64, op quota.IdempotencyOp, key string) (quota.IdempotencyRecord, bool, error) {
	b, err := s.idempotent()
	if err != nil {
		return quota.IdempotencyRecord{}, false, err
	}
	return b.GetIdempotency(accountID, op, key)
}

func (s *quotaStore) OccupyOnce(u quota.Usage, expectedVersion int, tok quota.Token, rec quota.IdempotencyRecord) error {
	b, err := s.idempotent()
	if err != nil {
		return err
	}
	return b.OccupyOnce(u, expectedVersion, tok, rec)
}

func (s *quotaStore) UpdateUsageOnce(u quota.Usage, expectedVersion int, rec quota.IdempotencyRecord) error {
	b, err := s.idempotent()
	if err != nil {
		return err
	}
	return b.UpdateUsageOnce(u, expectedVersion, rec)
}

// memQuotaStore is the in-memory quota.IdempotentStore: definitions, usage,
// tokens and client idempotency keys in maps under one lock. It is what keeps
// the demo and `go test` dependency-free.
type memQuotaStore struct {
	mu     sync.Mutex
	defs   map[string]quota.Definition
	usage  map[string]quota.Usage
	tokens map[string]quota.Token
	idem   map[memIdemKey]quota.IdempotencyRecord
	// tokenSeq mints reservation ids in the in-memory mode.
	tokenSeq int64
}

func newMemQuotaStore() *memQuotaStore {
	s := &memQuotaStore{
		defs:   make(map[string]quota.Definition),
		usage:  make(map[string]quota.Usage),
		tokens: make(map[string]quota.Token),
		idem:   make(map[memIdemKey]quota.IdempotencyRecord),
	}
	s.seed()
	return s
}

// nextTokenID mints a process-unique reservation id, handed to every Manager so
// the ids do not restart at 1 per request.
func (s *memQuotaStore) nextTokenID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenSeq++
	return fmt.Sprintf("qt-%d", s.tokenSeq)
}

func (s *memQuotaStore) seed() {
	// Seed the euecs-instance quota definition (limit 20, region-scoped) and a
	// pre-seeded usage row for account 100123 so GET /usage returns the real
	// shape before any occupy.
	s.defs["quota_euecs_instance"] = quota.Definition{
		QuotaCode:    "quota_euecs_instance",
		ProductCode:  "euecs",
		DefaultValue: 20,
		Scope:        quota.ScopeRegion,
		Adjustable:   true,
	}
	// Global-scope definition too, so the productCode→quotaCode path is real for
	// euoss buckets as well (and to demonstrate scope handling).
	s.defs["quota_euoss_bucket"] = quota.Definition{
		QuotaCode:    "quota_euoss_bucket",
		ProductCode:  "euoss",
		DefaultValue: 100,
		Scope:        quota.ScopeGlobal,
	}
	// Pre-seed account 100123's usage at the default limit of 20. NewManager's
	// CheckAndOccupy would lazily create this on first reserve from the
	// definition; seeding it makes the usage endpoint return real data before
	// any reservation exists, matching the console-bff seed for account 100123.
	s.usage[usageKey(100123, "quota_euecs_instance", "cn-north-1")] = quota.Usage{
		AccountID: 100123, QuotaCode: "quota_euecs_instance", Region: "cn-north-1",
		Used: 0, Occupying: 0, HardLimit: 20, Version: 0,
	}
}

func usageKey(accountID int64, quotaCode, region string) string {
	return fmt.Sprintf("%d|%s|%s", accountID, quotaCode, region)
}

func (s *memQuotaStore) GetDefinition(code string) (quota.Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.defs[code]
	if !ok {
		return quota.Definition{}, quota.ErrUnknownQuota
	}
	return d, nil
}

func (s *memQuotaStore) GetUsage(accountID int64, quotaCode, region string) (quota.Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.usage[usageKey(accountID, quotaCode, region)]
	if !ok {
		return quota.Usage{AccountID: accountID, QuotaCode: quotaCode, Region: region}, nil
	}
	return u, nil
}

func (s *memQuotaStore) UpdateUsage(u quota.Usage, expectedVersion int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := usageKey(u.AccountID, u.QuotaCode, u.Region)
	cur, ok := s.usage[k]
	curVersion := 0
	if ok {
		curVersion = cur.Version
	}
	if curVersion != expectedVersion {
		return quota.ErrVersionConflict
	}
	u.Version = curVersion + 1
	s.usage[k] = u
	return nil
}

func (s *memQuotaStore) PutToken(t quota.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[t.TokenID] = t
	return nil
}

func (s *memQuotaStore) GetToken(id string) (quota.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok {
		return quota.Token{}, quota.ErrTokenNotFound
	}
	return t, nil
}

// DeleteToken claims a reservation and, like quota.SQLStore, reports
// ErrTokenNotFound when it is already gone: the Manager's claim-first protocol
// (commit/release/sweep) relies on exactly one deleter winning.
func (s *memQuotaStore) DeleteToken(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[id]; !ok {
		return quota.ErrTokenNotFound
	}
	delete(s.tokens, id)
	return nil
}

func (s *memQuotaStore) ListExpiredTokens(now time.Time) ([]quota.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []quota.Token
	for _, t := range s.tokens {
		if t.Expired(now) {
			out = append(out, t)
		}
	}
	quota.SortTokens(out)
	return out, nil
}

// memIdemKey scopes a client idempotency key by account and operation, like
// quota_idempotency's uk_acc_op_key.
type memIdemKey struct {
	accountID int64
	op        quota.IdempotencyOp
	key       string
}

var _ quota.IdempotentStore = (*memQuotaStore)(nil)

func (s *memQuotaStore) GetIdempotency(accountID int64, op quota.IdempotencyOp, key string) (quota.IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.idem[memIdemKey{accountID, op, key}]
	return rec, ok, nil
}

// applyOnceLocked records the key and moves the usage row as one step (the
// caller holds s.mu): the in-memory rendering of SQLStore's transaction.
func (s *memQuotaStore) applyOnceLocked(u quota.Usage, expectedVersion int, rec quota.IdempotencyRecord) error {
	k := memIdemKey{rec.AccountID, rec.Op, rec.Key}
	if _, taken := s.idem[k]; taken {
		return quota.ErrIdempotencyKeyTaken
	}
	uk := usageKey(u.AccountID, u.QuotaCode, u.Region)
	if s.usage[uk].Version != expectedVersion {
		return quota.ErrVersionConflict
	}
	u.Version = expectedVersion + 1
	s.usage[uk] = u
	s.idem[k] = rec
	return nil
}

func (s *memQuotaStore) OccupyOnce(u quota.Usage, expectedVersion int, tok quota.Token, rec quota.IdempotencyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyOnceLocked(u, expectedVersion, rec); err != nil {
		return err
	}
	s.tokens[tok.TokenID] = tok
	return nil
}

func (s *memQuotaStore) UpdateUsageOnce(u quota.Usage, expectedVersion int, rec quota.IdempotencyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyOnceLocked(u, expectedVersion, rec)
}

// quotaCodeFor maps a productCode to its quota code. Each product has exactly
// one primary resource-count quota; this mapping is what turns a provisioning
// request ("reserve 3 euecs instances") into a quota code.
func quotaCodeFor(productCode string) string {
	switch productCode {
	case "euecs":
		return "quota_euecs_instance"
	case "euoss":
		return "quota_euoss_bucket"
	default:
		return "quota_" + productCode + "_instance"
	}
}

type occupyRequest struct {
	ProductCode  string `json:"productCode"`
	ResourceType string `json:"resourceType"` // informational: instance/bucket/...
	Region       string `json:"region"`
	Count        int    `json:"count"`
	BizKey       string `json:"bizKey"` // the order/saga this reservation belongs to
	// IdempotencyKey is a client-supplied retry key: a repeated occupy with the
	// same key returns the original reservation instead of double-counting
	// Occupying — on any replica, after any restart, and whatever became of the
	// reservation since (one key names one reservation, forever). Optional
	// (empty = every call reserves anew).
	IdempotencyKey string `json:"idempotencyKey"`
}

// maxBizKeyLen is quota_token.biz_key / quota_idempotency.biz_key.
const maxBizKeyLen = 64

// versionAttempts bounds the optimistic-lock retries of one request. The
// Manager's contract is that the loser of a version race retries against fresh
// state; surfacing the first loss as a 409 turns a benign concurrent write
// into a failed order.
const versionAttempts = 5

// withVersionRetry runs fn under storage.WithVersionRetry. The in-memory
// backend reports quota.ErrVersionConflict alone (SQLStore wraps both), so the
// loss is normalized to the shared storage signal the helper recognizes; the
// final error still matches quota.ErrVersionConflict.
func withVersionRetry(fn func() error) error {
	return storage.WithVersionRetry(versionAttempts, func() error {
		err := fn()
		if errors.Is(err, quota.ErrVersionConflict) && !errors.Is(err, storage.ErrVersionConflict) {
			return fmt.Errorf("%w: %w", err, storage.ErrVersionConflict)
		}
		return err
	})
}

func (s *quotaStore) handleOccupy(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req occupyRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.ProductCode == "" {
		writeErr(w, "Common.InvalidParameter", 400, "productCode is required")
		return
	}
	if req.Count <= 0 {
		writeErr(w, "Common.InvalidParameter", 400, "count must be positive")
		return
	}
	if len(req.BizKey) > maxBizKeyLen || len(req.IdempotencyKey) > quota.MaxIdempotencyKeyLen {
		writeErr(w, "Common.InvalidParameter", 400,
			fmt.Sprintf("bizKey must be at most %d bytes and idempotencyKey at most %d", maxBizKeyLen, quota.MaxIdempotencyKeyLen))
		return
	}
	if req.Region == "" {
		req.Region = "cn-north-1"
	}
	if req.BizKey == "" {
		req.BizKey = fmt.Sprintf("acct-%d", acct)
	}
	mgr := quota.NewManager(s, time.Now, s.nextTokenID)
	var (
		tok      quota.Token
		replayed bool
	)
	err := withVersionRetry(func() error {
		var err error
		if req.IdempotencyKey == "" {
			tok, err = mgr.CheckAndOccupy(acct, quotaCodeFor(req.ProductCode), req.Region, req.Count, req.BizKey)
			return err
		}
		// Keyed: the key is recorded in the same atomic step as the
		// reservation, so concurrent first attempts — on this replica or
		// another — reserve once, and the losers replay the winner's.
		tok, replayed, err = mgr.CheckAndOccupyOnce(acct, quotaCodeFor(req.ProductCode), req.Region, req.Count, req.BizKey, req.IdempotencyKey)
		return err
	})
	if err != nil {
		switch {
		case errors.Is(err, quota.ErrQuotaExceeded):
			writeErr(w, "Quota.Exceeded", 409, err.Error())
		case errors.Is(err, quota.ErrVersionConflict):
			writeErr(w, "Quota.VersionConflict", 409, err.Error())
		case errors.Is(err, quota.ErrIdempotencyMismatch):
			writeErr(w, "Quota.IdempotencyConflict", 409, err.Error())
		case errors.Is(err, quota.ErrUnknownQuota):
			writeErr(w, "Quota.UnknownQuota", 404, err.Error())
		case errors.Is(err, quota.ErrInvalidAmount), errors.Is(err, quota.ErrInvalidIdempotencyKey):
			writeErr(w, "Common.InvalidParameter", 400, err.Error())
		default:
			slog.Error("quota occupy failed", "account", acct, "err", err)
			writeErr(w, "Quota.OccupyFailed", 500, "配额预占失败")
		}
		return
	}
	active := true
	if replayed {
		// The original reservation, whatever became of it: still pending
		// (active), or committed, released or swept since. A caller whose
		// reservation ended reserves again under a new key.
		if _, gerr := s.GetToken(tok.TokenID); gerr != nil {
			if !errors.Is(gerr, quota.ErrTokenNotFound) {
				slog.Error("quota token lookup failed", "account", acct, "reservationId", tok.TokenID, "err", gerr)
				writeErr(w, "Quota.OccupyFailed", 500, "配额预占失败")
				return
			}
			active = false
		}
	}
	writeOccupyOK(w, tok, replayed, active)
}

func writeOccupyOK(w http.ResponseWriter, tok quota.Token, replayed, active bool) {
	writeJSON(w, "OK", map[string]any{
		"reservationId": tok.TokenID,
		"quotaCode":     tok.QuotaCode,
		"region":        tok.Region,
		"amount":        tok.Amount,
		"expiresAt":     tok.ExpiresAt.Format(time.RFC3339),
		"bizKey":        tok.BizKey,
		// replayed: this answer is an idempotent replay of an earlier occupy;
		// active: the reservation is still pending (commit/release apply).
		"replayed": replayed,
		"active":   active,
	})
}

// freeRequest is the body of POST /internal/quota/free: the occupy keys plus
// the amount of committed capacity a released resource gives back.
type freeRequest struct {
	ProductCode  string `json:"productCode"`
	ResourceType string `json:"resourceType"` // informational, as in occupy
	Region       string `json:"region"`
	Amount       int    `json:"amount"`
	// Count is accepted as an alias of amount, matching occupy's field name.
	Count  int    `json:"count"`
	BizKey string `json:"bizKey"` // the released resource
	// IdempotencyKey makes the free exactly-once; without it the bizKey (the
	// released resource) is the key. One of the two is required: a decrement
	// that a retry can apply twice under-counts Used, which is oversell.
	IdempotencyKey string `json:"idempotencyKey"`
}

// handleFree implements POST /internal/quota/free: when a resource is released
// its committed capacity (Used) goes back to the account. svc-orchestrator calls
// it from the release path and retries until it succeeds, so it is idempotent by
// key: a replay answers 200 with replayed=true and leaves Used alone.
//
// It lives under /internal/, which the gateway never routes. /api/v1/quota/* is
// tenant-reachable through APISIX (console-quota route), and a tenant that could
// free its own committed usage could create resources beyond its quota.
func (s *quotaStore) handleFree(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req freeRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.ProductCode == "" {
		writeErr(w, "Common.InvalidParameter", 400, "productCode is required")
		return
	}
	amount := req.Amount
	if amount == 0 {
		amount = req.Count
	} else if req.Count != 0 && req.Count != amount {
		writeErr(w, "Common.InvalidParameter", 400, "amount and count disagree")
		return
	}
	if amount <= 0 {
		writeErr(w, "Common.InvalidParameter", 400, "amount must be positive")
		return
	}
	key := req.IdempotencyKey
	if key == "" && req.BizKey != "" {
		key = "biz:" + req.BizKey
	}
	if key == "" {
		writeErr(w, "Common.InvalidParameter", 400, "idempotencyKey (or bizKey) is required: a quota free must be idempotent")
		return
	}
	if len(req.BizKey) > maxBizKeyLen || len(key) > quota.MaxIdempotencyKeyLen {
		writeErr(w, "Common.InvalidParameter", 400,
			fmt.Sprintf("bizKey must be at most %d bytes and idempotencyKey at most %d", maxBizKeyLen, quota.MaxIdempotencyKeyLen))
		return
	}
	if req.Region == "" {
		req.Region = "cn-north-1"
	}
	mgr := quota.NewManager(s, time.Now, s.nextTokenID)
	var (
		rec      quota.IdempotencyRecord
		replayed bool
	)
	err := withVersionRetry(func() error {
		var err error
		rec, replayed, err = mgr.FreeCommitted(acct, quotaCodeFor(req.ProductCode), req.Region, amount, req.BizKey, key)
		return err
	})
	if err != nil {
		switch {
		case errors.Is(err, quota.ErrVersionConflict):
			writeErr(w, "Quota.VersionConflict", 409, err.Error())
		case errors.Is(err, quota.ErrIdempotencyMismatch):
			writeErr(w, "Quota.IdempotencyConflict", 409, err.Error())
		case errors.Is(err, quota.ErrUnknownQuota):
			writeErr(w, "Quota.UnknownQuota", 404, err.Error())
		case errors.Is(err, quota.ErrInvalidAmount), errors.Is(err, quota.ErrInvalidIdempotencyKey):
			writeErr(w, "Common.InvalidParameter", 400, err.Error())
		default:
			slog.Error("quota free failed", "account", acct, "err", err)
			writeErr(w, "Quota.FreeFailed", 500, "配额归还失败")
		}
		return
	}
	writeJSON(w, "OK", map[string]any{
		"quotaCode": rec.QuotaCode,
		"region":    rec.Region,
		"amount":    rec.Amount,
		"bizKey":    rec.BizKey,
		"freed":     true,
		"replayed":  replayed,
	})
}

type releaseRequest struct {
	ReservationId string `json:"reservationId"`
}

func (s *quotaStore) handleRelease(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req releaseRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.ReservationId == "" {
		writeErr(w, "Common.InvalidParameter", 400, "reservationId is required")
		return
	}
	mgr := quota.NewManager(s, time.Now, s.nextTokenID)
	// ReleaseOccupy is saga-compensation: it succeeds for unknown/already-released
	// tokens. But the token, if it exists, must belong to this account — a
	// cross-account release is a real error, not a silent no-op.
	tok, err := s.GetToken(req.ReservationId)
	if err == nil && tok.AccountID != acct {
		writeErr(w, "Quota.NotFound", 404, "reservation does not belong to this account")
		return
	}
	if err := withVersionRetry(func() error { return mgr.ReleaseOccupy(req.ReservationId) }); err != nil {
		if errors.Is(err, quota.ErrVersionConflict) {
			writeErr(w, "Quota.VersionConflict", 409, err.Error())
			return
		}
		slog.Error("quota release failed", "account", acct, "reservationId", req.ReservationId, "err", err)
		writeErr(w, "Quota.ReleaseFailed", 500, "配额释放失败")
		return
	}
	writeJSON(w, "OK", map[string]any{"reservationId": req.ReservationId, "released": true})
}

type commitRequest struct {
	ReservationId string `json:"reservationId"`
}

// handleCommit converts a reservation into used capacity (phase 2, success):
// the fulfilment saga calls it once the resource exists. Unlike release it is
// not a silent no-op for an unknown token — a commit that finds nothing (already
// committed, released or swept) must tell the saga, and an expired one must not
// be counted (its capacity already went back to the pool).
func (s *quotaStore) handleCommit(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req commitRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.ReservationId == "" {
		writeErr(w, "Common.InvalidParameter", 400, "reservationId is required")
		return
	}
	// Same ownership rule as release: another account's reservation reads as
	// not found.
	tok, err := s.GetToken(req.ReservationId)
	switch {
	case errors.Is(err, quota.ErrTokenNotFound):
		writeErr(w, "Quota.NotFound", 404, "reservation not found")
		return
	case err != nil:
		slog.Error("quota commit lookup failed", "account", acct, "reservationId", req.ReservationId, "err", err)
		writeErr(w, "Quota.CommitFailed", 500, "配额确认失败")
		return
	case tok.AccountID != acct:
		writeErr(w, "Quota.NotFound", 404, "reservation does not belong to this account")
		return
	}
	mgr := quota.NewManager(s, time.Now, s.nextTokenID)
	if err := withVersionRetry(func() error { return mgr.CommitOccupy(req.ReservationId) }); err != nil {
		switch {
		case errors.Is(err, quota.ErrTokenExpired):
			writeErr(w, "Quota.ReservationExpired", 409, err.Error())
		case errors.Is(err, quota.ErrTokenNotFound):
			writeErr(w, "Quota.NotFound", 404, "reservation not found")
		case errors.Is(err, quota.ErrVersionConflict):
			writeErr(w, "Quota.VersionConflict", 409, err.Error())
		default:
			slog.Error("quota commit failed", "account", acct, "reservationId", req.ReservationId, "err", err)
			writeErr(w, "Quota.CommitFailed", 500, "配额确认失败")
		}
		return
	}
	writeJSON(w, "OK", map[string]any{"reservationId": req.ReservationId, "committed": true})
}

// sweepInterval is how often expired reservations are swept back. The token
// TTL is quota.DefaultTokenTTL (15 min), so a one-minute cadence returns a dead
// saga's capacity within about a minute of its expiry.
const sweepInterval = time.Minute

// sweepOnce returns expired reservations to the pool and logs the outcome.
func (s *quotaStore) sweepOnce(now func() time.Time) (int, error) {
	mgr := quota.NewManager(s, now, s.nextTokenID)
	n, err := mgr.SweepExpired()
	if err != nil {
		slog.Error("quota sweep failed", "err", err)
		return n, err
	}
	if n > 0 {
		slog.Info("quota sweep reclaimed expired reservations", "count", n)
	}
	return n, nil
}

// runSweeper sweeps every interval until ctx is cancelled (shutdown).
func (s *quotaStore) runSweeper(ctx context.Context, every time.Duration, now func() time.Time) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = s.sweepOnce(now)
		}
	}
}

func (s *quotaStore) handleUsage(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	productCode := r.URL.Query().Get("productCode")
	if productCode == "" {
		productCode = "euecs"
	}
	region := r.URL.Query().Get("region")
	if region == "" {
		region = "cn-north-1"
	}
	mgr := quota.NewManager(s, time.Now, s.nextTokenID)
	u, err := mgr.Describe(acct, quotaCodeFor(productCode), region)
	if err != nil {
		if errors.Is(err, quota.ErrUnknownQuota) {
			writeErr(w, "Quota.UnknownQuota", 404, err.Error())
			return
		}
		writeErr(w, "Quota.DescribeFailed", 500, err.Error())
		return
	}
	writeJSON(w, "OK", map[string]any{
		"accountId": u.AccountID,
		"quotaCode": u.QuotaCode,
		"region":    u.Region,
		"used":      u.Used,
		"occupying": u.Occupying,
		"hardLimit": u.HardLimit,
		"available": u.Available(),
		"warn":      quota.ShouldWarn(u),
		"version":   u.Version,
	})
}

func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	// TRUST NOTE: X-Euler-Account-Id is injected by the API gateway after
	// authentication; this service relies on network isolation (and optionally
	// internalTokenMiddleware) rather than re-authenticating.
	raw := r.Header.Get(accountIDHeader)
	if raw == "" {
		writeErr(w, "Common.MissingAccountId", 403, "X-Euler-Account-Id header is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed X-Euler-Account-Id")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, code string, data any) {
	rid := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Data": data})
}

func writeErr(w http.ResponseWriter, code string, status int, msg string) {
	rid := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Message": msg})
}

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := httpmw.SanitizeRequestID(r.Header.Get("X-Euler-TraceId"))
		if id == "" {
			id = fmt.Sprintf("quota-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Euler-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

// internalTokenMiddleware optionally enforces an internal shared secret: when
// the EULER_INTERNAL_TOKEN env var is set, every request must carry a matching
// X-Euler-Internal-Token header (defense-in-depth for the gateway-injected
// X-Euler-Account-Id trust). Unset (dev default) = no check.
func internalTokenMiddleware(h http.Handler) http.Handler {
	return httpmw.InternalToken(h)
}

func recoverMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "rec", rec, "path", r.URL.Path)
				writeErr(w, "Common.InternalError", 500, "internal error")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("http", ":9208", "HTTP listen address")
	flag.Parse()
	// Production refuses to start without the internal shared secret: the
	// X-Euler-Account-Id this service trusts must provably come from the gateway.
	runtimeRequireSecrets()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	store, err := newQuotaStore(context.Background())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	// Tell the operator where the counters live: in-memory counters are lost on
	// restart, and a reservation that disappears with them is capacity the next
	// order cannot see.
	slog.Info("svc-quota store ready", "persistent", store.persistent)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/quota/occupy", store.handleOccupy)
	mux.HandleFunc("POST /api/v1/quota/release", store.handleRelease)
	mux.HandleFunc("POST /api/v1/quota/commit", store.handleCommit)
	mux.HandleFunc("GET /api/v1/quota/usage", store.handleUsage)
	// svc-orchestrator's return of committed capacity on resource release
	// (internal: the gateway never routes /internal/*).
	mux.HandleFunc("POST /internal/quota/free", store.handleFree)

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(internalTokenMiddleware(mux))), ReadHeaderTimeout: 5 * time.Second}
	// The TTL sweeper is what bounds a reservation whose saga died between
	// occupy and commit (03§4.3.2: 占用令牌带 TTL 防止悬挂).
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		store.runSweeper(sweepCtx, sweepInterval, time.Now)
	}()
	go func() {
		slog.Info("svc-quota listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutdown signal received, draining")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
	stopSweep()
	<-sweepDone
}

// runtimeRequireSecrets enforces the production secret set (pkg-go/runmode).
func runtimeRequireSecrets() {
	runmode.MustRequireSecrets(httpmw.InternalTokenEnv)
}
