// Package main: svc-quota — two-phase quota reservation
// (03-backend-services.md §4.3.2).
//
// Owns the occupy/release protocol (pkg-go/quota). Provisioning is async and
// can fail minutes after the order is placed, so a reservation token reserves
// capacity up front; the fulfilment saga then commits it (resource exists) or
// releases it (it didn't). Both failure directions are bounded — an
// uncommitted reservation's TTL is swept back. The invariant the service is
// built around: in-flight reservations count against the limit, so two
// concurrent orders cannot both claim the last slot.
//
// Routes (gateway-authorized, X-Euler-Account-Id injected):
//
//	POST /api/v1/quota/occupy  — two-phase reserve (body: productCode, resourceType, count)
//	POST /api/v1/quota/release — release a reservation (body: reservationId)
//	GET  /api/v1/quota/usage   — usage position (?productCode=, ?region=)
//
// stdlib-HTTP service. Envelope {RequestId,Code,Data} (03§9.3). In-memory store
// (MySQL sharded by account_id + Redis read cache in production).
package main

import (
	"context"
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

	"github.com/qifalab/euler-platform/quota"
	"github.com/qifalab/euler-platform/storage"
)

// accountIDHeader is injected by the API gateway after authentication (see
// TRUST NOTE in accountIDFrom).
const accountIDHeader = "X-Euler-Account-Id"

// maxBodyBytes caps JSON request bodies.
const maxBodyBytes = 1 << 20 // 1 MiB

// quotaStore is the service's quota persistence facade: it owns the two pieces of
// state that are process-local by design (the client-idempotency map and the
// token-id sequence) and delegates everything else to backend — quota.SQLStore
// against support_db when EULER_DB_DSN is set, memQuotaStore otherwise.
//
// The handlers pass the facade itself to quota.NewManager, so the two-phase
// protocol is wired identically with and without persistence.
type quotaStore struct {
	backend quota.Store
	// idem maps a client idempotency key (scoped by account) to the token id it
	// produced, so a retried occupy with the same key does not double-count
	// Occupying. It is process-local: a restart forgets the keys it has seen, and
	// the exactly-once guarantee then rests on the token row in the store.
	idemMu sync.Mutex
	idem   map[string]string
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
	s := &quotaStore{idem: make(map[string]string)}
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
		idem:        make(map[string]string),
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

// memQuotaStore is the in-memory quota.Store: definitions, usage and tokens in
// maps under one lock. It is what keeps the demo and `go test` dependency-free.
type memQuotaStore struct {
	mu     sync.Mutex
	defs   map[string]quota.Definition
	usage  map[string]quota.Usage
	tokens map[string]quota.Token
	// tokenSeq mints reservation ids in the in-memory mode.
	tokenSeq int64
}

func newMemQuotaStore() *memQuotaStore {
	s := &memQuotaStore{
		defs:   make(map[string]quota.Definition),
		usage:  make(map[string]quota.Usage),
		tokens: make(map[string]quota.Token),
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

func (s *memQuotaStore) DeleteToken(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	// Occupying. Optional (empty = every call reserves anew).
	IdempotencyKey string `json:"idempotencyKey"`
}

// idemKey scopes a client idempotency key by account.
func idemKey(accountID int64, key string) string {
	return fmt.Sprintf("%d|%s", accountID, key)
}

// lookupIdem returns the still-live token previously produced for this
// idempotency key, if any.
func (s *quotaStore) lookupIdem(accountID int64, key string) (quota.Token, bool) {
	s.idemMu.Lock()
	tokID, ok := s.idem[idemKey(accountID, key)]
	s.idemMu.Unlock()
	if !ok {
		return quota.Token{}, false
	}
	// The token itself lives in the backend — in-memory in the demo, support_db
	// when persistence is on. A token the sweeper has already reclaimed reads as
	// a miss, which is exactly what lets the retry reserve afresh instead of
	// handing back a dead reservation id.
	tok, err := s.GetToken(tokID)
	if err != nil {
		return quota.Token{}, false
	}
	return tok, true
}

func (s *quotaStore) recordIdem(accountID int64, key, tokenID string) {
	s.idemMu.Lock()
	defer s.idemMu.Unlock()
	s.idem[idemKey(accountID, key)] = tokenID
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
	if req.Region == "" {
		req.Region = "cn-north-1"
	}
	if req.BizKey == "" {
		req.BizKey = fmt.Sprintf("acct-%d", acct)
	}
	// Idempotent replay: same key → return the existing reservation, no new
	// Occupying accrual.
	if req.IdempotencyKey != "" {
		if tok, hit := s.lookupIdem(acct, req.IdempotencyKey); hit {
			writeOccupyOK(w, tok)
			return
		}
	}
	mgr := quota.NewManager(s, time.Now, s.nextTokenID)
	tok, err := mgr.CheckAndOccupy(acct, quotaCodeFor(req.ProductCode), req.Region, req.Count, req.BizKey)
	if err != nil {
		switch {
		case errors.Is(err, quota.ErrQuotaExceeded):
			writeErr(w, "Quota.Exceeded", 409, err.Error())
		case errors.Is(err, quota.ErrVersionConflict):
			writeErr(w, "Quota.VersionConflict", 409, err.Error())
		case errors.Is(err, quota.ErrUnknownQuota):
			writeErr(w, "Quota.UnknownQuota", 404, err.Error())
		case errors.Is(err, quota.ErrInvalidAmount):
			writeErr(w, "Common.InvalidParameter", 400, err.Error())
		default:
			slog.Error("quota occupy failed", "account", acct, "err", err)
			writeErr(w, "Quota.OccupyFailed", 500, "配额预占失败")
		}
		return
	}
	if req.IdempotencyKey != "" {
		s.recordIdem(acct, req.IdempotencyKey, tok.TokenID)
	}
	writeOccupyOK(w, tok)
}

func writeOccupyOK(w http.ResponseWriter, tok quota.Token) {
	writeJSON(w, "OK", map[string]any{
		"reservationId": tok.TokenID,
		"quotaCode":     tok.QuotaCode,
		"region":        tok.Region,
		"amount":        tok.Amount,
		"expiresAt":      tok.ExpiresAt.Format(time.RFC3339),
		"bizKey":         tok.BizKey,
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
	if err := mgr.ReleaseOccupy(req.ReservationId); err != nil {
		slog.Error("quota release failed", "account", acct, "reservationId", req.ReservationId, "err", err)
		writeErr(w, "Quota.ReleaseFailed", 500, "配额释放失败")
		return
	}
	writeJSON(w, "OK", map[string]any{"reservationId": req.ReservationId, "released": true})
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
		"accountId":  u.AccountID,
		"quotaCode":  u.QuotaCode,
		"region":     u.Region,
		"used":       u.Used,
		"occupying":  u.Occupying,
		"hardLimit":  u.HardLimit,
		"available":  u.Available(),
		"warn":       quota.ShouldWarn(u),
		"version":    u.Version,
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
		id := r.Header.Get("X-Euler-TraceId")
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
	token := os.Getenv("EULER_INTERNAL_TOKEN")
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Euler-Internal-Token") != token {
			writeErr(w, "Common.Forbidden", 403, "invalid internal token")
			return
		}
		h.ServeHTTP(w, r)
	})
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
	mux.HandleFunc("GET /api/v1/quota/usage", store.handleUsage)

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(internalTokenMiddleware(mux))), ReadHeaderTimeout: 5 * time.Second}
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
}
