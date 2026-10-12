package quota

import (
	"errors"
	"fmt"
	"time"
)

// # Client idempotency keys
//
// A reservation or a free that a caller retries must take effect once. A
// process-local "key → token" map cannot promise that: two replicas each see a
// miss and both reserve, and a restart forgets every key it has seen. So a
// keyed operation records its key in the SAME atomic step that moves the usage
// counter (one transaction in SQLStore, one critical section in a memory
// store): either the change and its key are both there, or neither is, and the
// unique key decides which of two concurrent first attempts applied.
//
// A key names exactly one operation, forever. Replaying it returns the original
// result even after the reservation was committed, released or swept — a
// replay that reserved afresh once the first token was gone would count a
// committed reservation twice. A caller whose reservation ended and that wants
// a new one uses a new key.

// IdempotencyOp is the operation a key was recorded for. Keys are scoped by
// (account, op): an occupy key and a free key never collide.
type IdempotencyOp string

const (
	// OpOccupy is CheckAndOccupyOnce: the key names one reservation.
	OpOccupy IdempotencyOp = "OCCUPY"
	// OpFree is FreeCommitted: the key names one return of committed usage.
	OpFree IdempotencyOp = "FREE"
)

// MaxIdempotencyKeyLen is the width of quota_idempotency.idem_key.
const MaxIdempotencyKeyLen = 128

// IdempotencyRecord is what a key applied. For OpOccupy it is the reservation
// as created (TokenID, ExpiresAt), kept after the token itself is gone.
type IdempotencyRecord struct {
	AccountID int64
	Op        IdempotencyOp
	Key       string
	QuotaCode string
	Region    string
	Amount    int
	TokenID   string // OpOccupy only
	BizKey    string
	ExpiresAt time.Time // OpOccupy only
	CreatedAt time.Time
}

// Token renders an occupy record as the reservation it created.
func (r IdempotencyRecord) Token() Token {
	return Token{
		TokenID: r.TokenID, AccountID: r.AccountID, QuotaCode: r.QuotaCode, Region: r.Region,
		Amount: r.Amount, BizKey: r.BizKey, ExpiresAt: r.ExpiresAt, CreatedAt: r.CreatedAt,
	}
}

// Idempotency errors.
var (
	// ErrIdempotencyKeyTaken: the store already holds a record for the key;
	// nothing was applied. Managers turn it into a replay.
	ErrIdempotencyKeyTaken = errors.New("quota: idempotency key already recorded")
	// ErrIdempotencyMismatch: the key was used for a different request (another
	// quota, region or amount). Replaying the original would answer a question
	// the caller did not ask; applying the new one would break the key.
	ErrIdempotencyMismatch = errors.New("quota: idempotency key reused for a different request")
	// ErrIdempotencyUnsupported: the store cannot record keys atomically.
	ErrIdempotencyUnsupported = errors.New("quota: store does not support idempotency keys")
	// ErrInvalidIdempotencyKey: empty or longer than MaxIdempotencyKeyLen.
	ErrInvalidIdempotencyKey = errors.New("quota: invalid idempotency key")
)

// IdempotentStore is the Store extension behind the keyed operations.
type IdempotentStore interface {
	Store
	// GetIdempotency returns the record for (accountID, op, key), if any.
	GetIdempotency(accountID int64, op IdempotencyOp, key string) (IdempotencyRecord, bool, error)
	// OccupyOnce applies a reservation — the usage change under
	// expectedVersion (UpdateUsage semantics) and the token — and records rec,
	// atomically. ErrIdempotencyKeyTaken (nothing applied) when rec's key is
	// already recorded; ErrVersionConflict when the usage row moved.
	OccupyOnce(u Usage, expectedVersion int, tok Token, rec IdempotencyRecord) error
	// UpdateUsageOnce applies the usage change and records rec, atomically,
	// with the same errors as OccupyOnce.
	UpdateUsageOnce(u Usage, expectedVersion int, rec IdempotencyRecord) error
}

func validIdempotencyKey(key string) error {
	if key == "" || len(key) > MaxIdempotencyKeyLen {
		return fmt.Errorf("%w: length must be 1..%d", ErrInvalidIdempotencyKey, MaxIdempotencyKeyLen)
	}
	return nil
}

func (m *Manager) idempotentStore() (IdempotentStore, error) {
	s, ok := m.store.(IdempotentStore)
	if !ok {
		return nil, ErrIdempotencyUnsupported
	}
	return s, nil
}

// replay checks a recorded key against the request it is replayed for.
func replay(rec IdempotencyRecord, quotaCode, region string, amount int) error {
	if rec.QuotaCode != quotaCode || rec.Region != region || rec.Amount != amount {
		return fmt.Errorf("%w: key %q recorded %s/%s×%d, request is %s/%s×%d", ErrIdempotencyMismatch,
			rec.Key, rec.QuotaCode, rec.Region, rec.Amount, quotaCode, region, amount)
	}
	return nil
}

// CheckAndOccupyOnce is CheckAndOccupy keyed by a client idempotency key: the
// first call reserves, every later call with the key — on any replica, after
// any restart — returns that same reservation (replayed = true) without
// touching the counters, whatever has become of the token since.
//
// The loser of a concurrent first attempt sees ErrVersionConflict (the usage
// row moved under it) and, on its retry, the winner's record. A loser that
// re-read usage after the winner committed may find the quota full — that is
// the winner's reservation it is looking at, so the record is checked again
// before ErrQuotaExceeded is reported.
func (m *Manager) CheckAndOccupyOnce(accountID int64, quotaCode, region string, amount int, bizKey, key string) (Token, bool, error) {
	store, err := m.idempotentStore()
	if err != nil {
		return Token{}, false, err
	}
	if err := validIdempotencyKey(key); err != nil {
		return Token{}, false, err
	}
	if amount <= 0 {
		return Token{}, false, fmt.Errorf("%w: %d", ErrInvalidAmount, amount)
	}
	region, err = m.normalizeRegion(quotaCode, region)
	if err != nil {
		return Token{}, false, err
	}
	recorded := func() (Token, bool, error) {
		rec, found, err := store.GetIdempotency(accountID, OpOccupy, key)
		if err != nil || !found {
			return Token{}, false, err
		}
		if err := replay(rec, quotaCode, region, amount); err != nil {
			return Token{}, true, err
		}
		return rec.Token(), true, nil
	}
	if tok, found, err := recorded(); err != nil || found {
		return tok, found, err
	}

	def, err := m.store.GetDefinition(quotaCode)
	if err != nil {
		return Token{}, false, err
	}
	u, err := m.store.GetUsage(accountID, quotaCode, region)
	if err != nil {
		return Token{}, false, err
	}
	if u.HardLimit == 0 {
		u.HardLimit = def.DefaultValue
		u.AccountID = accountID
		u.QuotaCode = quotaCode
		u.Region = region
	}
	if u.Available() < amount {
		if tok, found, err := recorded(); err != nil || found {
			return tok, found, err
		}
		return Token{}, false, fmt.Errorf("%w: %s available %d, requested %d",
			ErrQuotaExceeded, quotaCode, u.Available(), amount)
	}

	// Whole seconds in UTC: quota_token and quota_idempotency hold DATETIME,
	// and a replay read back from them must render the instant the first
	// answer did.
	now := m.now().UTC().Truncate(time.Second)
	tok := Token{
		TokenID:   m.newToken(),
		AccountID: accountID,
		QuotaCode: quotaCode,
		Region:    region,
		Amount:    amount,
		BizKey:    bizKey,
		ExpiresAt: now.Add(m.tokenTTL),
		CreatedAt: now,
	}
	updated := u
	updated.Occupying += amount
	rec := IdempotencyRecord{
		AccountID: accountID, Op: OpOccupy, Key: key, QuotaCode: quotaCode, Region: region,
		Amount: amount, TokenID: tok.TokenID, BizKey: bizKey, ExpiresAt: tok.ExpiresAt, CreatedAt: now,
	}
	switch err := store.OccupyOnce(updated, u.Version, tok, rec); {
	case err == nil:
		return tok, false, nil
	case errors.Is(err, ErrIdempotencyKeyTaken):
		if tok, found, rerr := recorded(); rerr != nil || found {
			return tok, found, rerr
		}
		return Token{}, false, err
	default:
		return Token{}, false, err
	}
}

// FreeCommitted returns committed capacity when a resource is released
// (Used -= amount, never below zero), exactly once per key: a replay returns
// the original record (replayed = true) and leaves Used alone. This is the
// keyed form of ReleaseCommitted, for callers that retry.
func (m *Manager) FreeCommitted(accountID int64, quotaCode, region string, amount int, bizKey, key string) (IdempotencyRecord, bool, error) {
	store, err := m.idempotentStore()
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	if err := validIdempotencyKey(key); err != nil {
		return IdempotencyRecord{}, false, err
	}
	if amount <= 0 {
		return IdempotencyRecord{}, false, fmt.Errorf("%w: %d", ErrInvalidAmount, amount)
	}
	region, err = m.normalizeRegion(quotaCode, region)
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	recorded := func() (IdempotencyRecord, bool, error) {
		rec, found, err := store.GetIdempotency(accountID, OpFree, key)
		if err != nil || !found {
			return IdempotencyRecord{}, false, err
		}
		if err := replay(rec, quotaCode, region, amount); err != nil {
			return IdempotencyRecord{}, true, err
		}
		return rec, true, nil
	}
	if rec, found, err := recorded(); err != nil || found {
		return rec, found, err
	}
	def, err := m.store.GetDefinition(quotaCode)
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	u, err := m.store.GetUsage(accountID, quotaCode, region)
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	if u.HardLimit == 0 {
		// Never write a row with a zero ceiling: a lazily created row carries
		// the definition's limit, exactly as CheckAndOccupy creates it.
		u.HardLimit = def.DefaultValue
		u.AccountID = accountID
		u.QuotaCode = quotaCode
		u.Region = region
	}
	updated := u
	updated.Used -= amount
	if updated.Used < 0 {
		updated.Used = 0
	}
	rec := IdempotencyRecord{
		AccountID: accountID, Op: OpFree, Key: key, QuotaCode: quotaCode, Region: region,
		Amount: amount, BizKey: bizKey, CreatedAt: m.now().UTC().Truncate(time.Second),
	}
	switch err := store.UpdateUsageOnce(updated, u.Version, rec); {
	case err == nil:
		return rec, false, nil
	case errors.Is(err, ErrIdempotencyKeyTaken):
		if prior, found, rerr := recorded(); rerr != nil || found {
			return prior, found, rerr
		}
		return IdempotencyRecord{}, false, err
	default:
		return IdempotencyRecord{}, false, err
	}
}
