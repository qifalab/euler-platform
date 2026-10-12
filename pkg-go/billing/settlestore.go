package billing

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
)

// SettleRecord is the durable state of one charge's settlement, keyed by
// (AccountID, ChargeID). ChargeID derives from the aggregate id, so it is the
// settlement's idempotency key: a retried or concurrently duplicated settle of
// the same aggregate finds this record and either replays the final result or
// resumes the plan from the progress recorded here.
//
// # Why the plan is stored, not recomputed
//
// A settlement commits in three stores that cannot share one transaction:
// resource packs (pack ledger), cash (cash ledger) and the charge itself. Each
// commit is idempotent on the ChargeID, but recomputing the waterfall on a
// retry is not safe: a pack the first attempt ran dry no longer lists as
// active, and a recomputation without it bills that share to cash a second
// time. So the waterfall's pack intent is persisted BEFORE any pack moves, and
// what each pack actually covered is pinned once the pack phase completes. A
// retry on any replica — or after a restart — replays the pack consumes under
// the same key (returning what was originally taken) and never recomputes
// them.
type SettleRecord struct {
	AccountID int64
	ChargeID  string
	// PackIntent is what the waterfall asked of each resource pack. It is
	// written before the first pack consume, so a crash mid-way leaves a record
	// naming every pack that may hold a consume under the ChargeID.
	PackIntent map[string]pricing.Amount
	// PackTaken is the quota each pack actually covered under this charge (zero:
	// the pack covered nothing and its share moved down the waterfall).
	PackTaken map[string]pricing.Amount
	// PacksDone pins PackTaken: once set, the pack phase is never re-run.
	PacksDone bool
	// CashPaid is the cash share debited under the ChargeID. The cash ledger's
	// entry under that key is authoritative; this is its copy on the charge.
	CashPaid pricing.Amount
	// Settled marks the record final: Charge is the result every replay
	// returns, and the store refuses to change the record again.
	Settled bool
	Charge  Charge
	// Version is the record's optimistic lock; 0 means "never stored".
	Version int
}

// clone deep-copies the record so a caller never shares maps or slices with a
// store (or with another caller).
func (r SettleRecord) clone() SettleRecord {
	out := r
	out.PackIntent = cloneAmounts(r.PackIntent)
	out.PackTaken = cloneAmounts(r.PackTaken)
	if r.Charge.Deductions != nil {
		out.Charge.Deductions = append([]Deduction(nil), r.Charge.Deductions...)
	}
	return out
}

func cloneAmounts(m map[string]pricing.Amount) map[string]pricing.Amount {
	out := make(map[string]pricing.Amount, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// normalize maps the record's timestamps onto what the SQL columns hold (UTC,
// microsecond precision), so the result returned by the first settle is
// byte-identical to the one a replay reads back from either store.
func (r *SettleRecord) normalize() {
	// The charge carries its own key, so a record read back from either store
	// names the same charge whether or not it is settled yet.
	r.Charge.ChargeID = r.ChargeID
	r.Charge.AccountID = r.AccountID
	if r.PackIntent == nil {
		r.PackIntent = map[string]pricing.Amount{}
	}
	if r.PackTaken == nil {
		r.PackTaken = map[string]pricing.Amount{}
	}
	if !r.Charge.SettledAt.IsZero() {
		r.Charge.SettledAt = r.Charge.SettledAt.UTC().Truncate(time.Microsecond)
	}
	if !r.Charge.BillingCycle.IsZero() {
		r.Charge.BillingCycle = r.Charge.BillingCycle.UTC().Truncate(time.Second)
	}
}

// Settle-store errors.
var (
	// ErrSettleConflict: the record moved since it was read (or is already
	// settled) — the writer lost a race and must re-read. It also matches
	// storage.ErrVersionConflict.
	ErrSettleConflict = fmt.Errorf("billing: settle record changed concurrently: %w", storage.ErrVersionConflict)
	// ErrInvalidSettleRecord: the record cannot be stored (missing key).
	ErrInvalidSettleRecord = errors.New("billing: invalid settle record")
)

// SettleStore persists settlement records and serialises the settlement of
// one account across every process that shares the store.
type SettleStore interface {
	// LockAccount blocks until the caller holds the account's settlement lock
	// (or ctx ends) and returns its release, which must be called exactly once.
	// In the SQL store the lock is a row lock held by a transaction bound to
	// ctx: if ctx ends while the lock is held, the lock is released, so ctx
	// must span the whole critical section.
	LockAccount(ctx context.Context, accountID int64) (release func(), err error)
	// GetSettle returns the record for (accountID, chargeID), if any.
	GetSettle(accountID int64, chargeID string) (SettleRecord, bool, error)
	// SaveSettle stores rec under its optimistic lock: Version 0 creates the
	// record, any other version updates it only if the stored record is still
	// at that version and not yet settled (ErrSettleConflict otherwise). On
	// success rec.Version is advanced and its timestamps normalised to the
	// stored precision.
	SaveSettle(rec *SettleRecord) error
	// ListCharges returns the account's settled charges in a bill period ("" =
	// every period), ordered by settlement time then charge id.
	ListCharges(accountID int64, period string) ([]Charge, error)
}

// ---------------------------------------------------------------------------
// In-process account locks
// ---------------------------------------------------------------------------

// accountLocks is a per-account, context-aware mutex. The memory store uses it
// as THE lock; the SQL store takes it first so one process never parks more
// than one pooled connection per account on the database row lock.
type accountLocks struct {
	mu sync.Mutex
	m  map[int64]*accountLock
}

type accountLock struct {
	ch   chan struct{}
	refs int
}

func (k *accountLocks) lock(ctx context.Context, accountID int64) (func(), error) {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[int64]*accountLock)
	}
	l := k.m[accountID]
	if l == nil {
		l = &accountLock{ch: make(chan struct{}, 1)}
		k.m[accountID] = l
	}
	l.refs++
	k.mu.Unlock()

	select {
	case l.ch <- struct{}{}:
	case <-ctx.Done():
		k.unref(accountID, l)
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.ch
			k.unref(accountID, l)
		})
	}, nil
}

func (k *accountLocks) unref(accountID int64, l *accountLock) {
	k.mu.Lock()
	defer k.mu.Unlock()
	l.refs--
	if l.refs == 0 {
		delete(k.m, accountID)
	}
}

// ---------------------------------------------------------------------------
// Memory store
// ---------------------------------------------------------------------------

// MemorySettleStore is the in-memory SettleStore for the demo and `go test`.
// Its account lock is process-local, which is exactly its scope: the records
// it guards live in this process too.
type MemorySettleStore struct {
	locks   accountLocks
	mu      sync.Mutex
	records map[settleKey]SettleRecord
}

type settleKey struct {
	accountID int64
	chargeID  string
}

// NewMemorySettleStore builds an empty in-memory store.
func NewMemorySettleStore() *MemorySettleStore {
	return &MemorySettleStore{records: make(map[settleKey]SettleRecord)}
}

var _ SettleStore = (*MemorySettleStore)(nil)

// LockAccount takes the account's in-process settlement lock.
func (m *MemorySettleStore) LockAccount(ctx context.Context, accountID int64) (func(), error) {
	return m.locks.lock(ctx, accountID)
}

// GetSettle returns a copy of the stored record.
func (m *MemorySettleStore) GetSettle(accountID int64, chargeID string) (SettleRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[settleKey{accountID, chargeID}]
	if !ok {
		return SettleRecord{}, false, nil
	}
	return rec.clone(), true, nil
}

// SaveSettle stores the record under the same guard as the SQL store.
func (m *MemorySettleStore) SaveSettle(rec *SettleRecord) error {
	if rec.ChargeID == "" {
		return fmt.Errorf("%w: empty charge id", ErrInvalidSettleRecord)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := settleKey{rec.AccountID, rec.ChargeID}
	cur, exists := m.records[k]
	switch {
	case rec.Version == 0 && exists:
		return ErrSettleConflict
	case rec.Version != 0 && (!exists || cur.Version != rec.Version || cur.Settled):
		return ErrSettleConflict
	}
	rec.normalize()
	rec.Version++
	m.records[k] = rec.clone()
	return nil
}

// ListCharges returns the settled charges in (SettledAt, ChargeID) order.
func (m *MemorySettleStore) ListCharges(accountID int64, period string) ([]Charge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Charge, 0)
	for k, rec := range m.records {
		if k.accountID != accountID || !rec.Settled {
			continue
		}
		if period != "" && rec.Charge.BillPeriod != period {
			continue
		}
		out = append(out, rec.clone().Charge)
	}
	sortCharges(out)
	return out, nil
}

// sortCharges is the order both stores list charges in.
func sortCharges(cs []Charge) {
	sort.SliceStable(cs, func(i, j int) bool {
		if !cs[i].SettledAt.Equal(cs[j].SettledAt) {
			return cs[i].SettledAt.Before(cs[j].SettledAt)
		}
		return cs[i].ChargeID < cs[j].ChargeID
	})
}
