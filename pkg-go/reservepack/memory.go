package reservepack

import (
	"sync"
	"time"

	"github.com/qifalab/euler-platform/billing"
	"github.com/qifalab/euler-platform/pricing"
)

// MemoryStore is an in-memory Store for tests and the runnable demos. It is
// safe for concurrent use. Production replaces it with the Vitess-backed impl.
type MemoryStore struct {
	mu       sync.Mutex
	packs    map[string]Pack
	entries  map[string][]Entry
	byIdem   map[string]Entry // key = packID + "\x00" + idempotencyKey
}

// NewMemoryStore builds an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		packs:   make(map[string]Pack),
		entries: make(map[string][]Entry),
		byIdem:  make(map[string]Entry),
	}
}

func idemKey(packID, key string) string { return packID + "\x00" + key }

func (s *MemoryStore) GetPack(packID string) (Pack, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.packs[packID]
	return p, ok, nil
}

func (s *MemoryStore) ListActiveByAccount(accountID int64, t time.Time) ([]Pack, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pack
	for _, p := range s.packs {
		if p.AccountID != accountID {
			continue
		}
		if p.Status.IsTerminal() {
			continue
		}
		if !p.ExpireAt.IsZero() && !t.Before(p.ExpireAt) {
			// Past deadline but not yet swept — still return it so SweepExpired
			// can process it.
			out = append(out, p)
			continue
		}
		if p.Remaining.IsZero() || p.Remaining.IsNegative() {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *MemoryStore) Apply(entry Entry, newPack Pack, expectedVersion int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.packs[newPack.PackID]
	// New pack: version must start at 0 → 1.
	if !ok && expectedVersion != 0 {
		return ErrPackNotFound
	}
	if ok && existing.Version != expectedVersion {
		return ErrVersionConflict // optimistic-lock mismatch; caller retries
	}
	// Idempotency check at apply time too (defense in depth; the Ledger checks
	// first, but a direct Apply caller could double-apply).
	if _, dup := s.byIdem[idemKey(newPack.PackID, entry.IdempotencyKey)]; dup {
		return ErrDuplicateEntry
	}
	s.packs[newPack.PackID] = newPack
	s.entries[newPack.PackID] = append(s.entries[newPack.PackID], entry)
	s.byIdem[idemKey(newPack.PackID, entry.IdempotencyKey)] = entry
	return nil
}

func (s *MemoryStore) FindByIdempotencyKey(packID, key string) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byIdem[idemKey(packID, key)]
	return e, ok, nil
}

func (s *MemoryStore) ListEntries(packID string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.entries[packID]))
	copy(out, s.entries[packID])
	return out, nil
}

// ToPool adapts a Pack into the billing.Pool the waterfall consumes. The
// waterfall reads Available and ProductCodes; it does not mutate the pool —
// the caller commits the consume intent via Ledger.Consume transactionally
// with the charge.
func ToPool(p Pack, t time.Time) billing.Pool {
	codes := []string{}
	if p.ProductCode != "" {
		codes = []string{p.ProductCode}
	}
	return billing.Pool{
		Source:       billing.SourceResourcePack,
		Ref:          p.PackID,
		Available:    p.Available(t),
		ExpireAt:     p.ExpireAt,
		ProductCodes: codes,
	}
}

// PoolsForAccount builds the waterfall pools for every active pack an account
// holds, ordered as the billing engine expects (the engine sorts by tier then
// expiry internally, so order here does not matter for correctness).
func PoolsForAccount(store Store, accountID int64, t time.Time) ([]billing.Pool, error) {
	active, err := store.ListActiveByAccount(accountID, t)
	if err != nil {
		return nil, err
	}
	pools := make([]billing.Pool, 0, len(active))
	for _, p := range active {
		if p.Available(t).IsZero() {
			continue
		}
		pools = append(pools, ToPool(p, t))
	}
	return pools, nil
}

// Ensure import used. (pricing referenced for completeness of the package's
// relationship; ToPool does not need it directly but the adapter contract is
// fixed-point amounts throughout.)
var _ = pricing.Amount(0)
