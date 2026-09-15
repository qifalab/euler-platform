package quota

import (
	"errors"
	"testing"
	"time"
)

var fixNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// GLOBAL-scoped quotas normalize region to "*" on EVERY path. Before the fix,
// ReleaseCommitted/Reconcile/Correct read the literal region and operated on a
// phantom row that never matched the one CheckAndOccupy wrote.
func TestGlobalScopeRegionNormalizedOnAllPaths(t *testing.T) {
	m, store := newManager(fixNow)
	const q = "quota_euoss_bucket" // GLOBAL in the fixture

	tok, err := m.CheckAndOccupy(1, q, "cn-north-1", 5, "order-1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Region != "*" {
		t.Fatalf("occupy region = %q, want *", tok.Region)
	}
	if err := m.CommitOccupy(tok.TokenID); err != nil {
		t.Fatal(err)
	}

	// ReleaseCommitted with the literal region must hit the "*" row.
	if err := m.ReleaseCommitted(1, q, "cn-north-1", 2); err != nil {
		t.Fatal(err)
	}
	u, _ := store.GetUsage(1, q, "*")
	if u.Used != 3 {
		t.Fatalf("used after release = %d, want 3", u.Used)
	}

	// Reconcile must read the "*" row, not an empty per-region row.
	r, err := m.Reconcile(1, q, "cn-north-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.RecordedUsed != 3 || r.Drifted() {
		t.Fatalf("reconcile = %+v, want recorded 3, no drift", r)
	}

	// Correct must write the "*" row.
	if err := m.Correct(1, q, "cn-north-1", 7); err != nil {
		t.Fatal(err)
	}
	u, _ = store.GetUsage(1, q, "*")
	if u.Used != 7 {
		t.Fatalf("used after correct = %d, want 7", u.Used)
	}
}

// The commit path claims (deletes) the token before touching usage, so a
// reservation the sweeper already reclaimed cannot ALSO be committed — the
// double-count that oversells capacity.
func TestCommitAfterSweepCannotDoubleCount(t *testing.T) {
	m, store := newManager(fixNow)
	m.SetTokenTTL(time.Minute)
	tok, err := m.CheckAndOccupy(1, "quota_euecs_instance", "cn-north-1", 5, "order-1")
	if err != nil {
		t.Fatal(err)
	}

	// Advance past the TTL and sweep: the reservation returns to the pool.
	m2 := NewManager(store, func() time.Time { return fixNow.Add(2 * time.Minute) }, nil)
	n, err := m2.SweepExpired()
	if err != nil || n != 1 {
		t.Fatalf("swept = %d, %v", n, err)
	}

	// A late commit must fail — the token is gone — and Used must stay 0.
	err = m2.CommitOccupy(tok.TokenID)
	if !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("commit after sweep: err = %v, want ErrTokenNotFound", err)
	}
	u, _ := store.GetUsage(1, "quota_euecs_instance", "cn-north-1")
	if u.Used != 0 || u.Occupying != 0 {
		t.Fatalf("usage after sweep+commit = %+v, want 0/0", u)
	}
}

// failingUpdateStore wraps a Store and fails UpdateUsage on demand.
type failingUpdateStore struct {
	Store
	failUpdate bool
}

func (f *failingUpdateStore) UpdateUsage(u Usage, v int) error {
	if f.failUpdate {
		return ErrVersionConflict
	}
	return f.Store.UpdateUsage(u, v)
}

// A commit whose usage update fails must restore the token so the reservation
// is not silently lost (delete-first needs the rollback half).
func TestCommitRestoresTokenOnUsageFailure(t *testing.T) {
	m, store := newManager(fixNow)
	tok, err := m.CheckAndOccupy(1, "quota_euecs_instance", "cn-north-1", 2, "order-1")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &failingUpdateStore{Store: store, failUpdate: true}
	m2 := NewManager(wrapped, func() time.Time { return fixNow }, nil)
	if err := m2.CommitOccupy(tok.TokenID); err == nil {
		t.Fatal("commit must surface the usage-update failure")
	}
	// The token must have been put back — the reservation still stands.
	if _, err := store.GetToken(tok.TokenID); err != nil {
		t.Fatalf("token must be restored after failed commit: %v", err)
	}
	// And a later, healthy commit still works exactly once.
	wrapped.failUpdate = false
	if err := m2.CommitOccupy(tok.TokenID); err != nil {
		t.Fatalf("healthy retry should commit: %v", err)
	}
	final, _ := store.GetUsage(1, "quota_euecs_instance", "cn-north-1")
	if final.Used != 2 || final.Occupying != 0 {
		t.Fatalf("usage = %+v, want Used 2 / Occupying 0", final)
	}
}

// A sweep whose usage update fails must put the token back for the next round
// instead of leaving the counter decremented with the token gone (or vice
// versa, the token present with the counter already decremented — the shape
// that produced a second decrement on the next sweep).
func TestSweepDeletesTokenBeforeUsage(t *testing.T) {
	m, store := newManager(fixNow)
	m.SetTokenTTL(time.Minute)
	if _, err := m.CheckAndOccupy(1, "quota_euecs_instance", "cn-north-1", 5, "o1"); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(store, func() time.Time { return fixNow.Add(2 * time.Minute) }, nil)
	if n, _ := m2.SweepExpired(); n != 1 {
		t.Fatalf("first sweep should reclaim 1")
	}
	// A second sweep finds nothing: the token cannot be decremented twice.
	if n, _ := m2.SweepExpired(); n != 0 {
		t.Fatalf("second sweep must find nothing")
	}
	u, _ := store.GetUsage(1, "quota_euecs_instance", "cn-north-1")
	if u.Occupying != 0 {
		t.Fatalf("occupying = %d, want 0 (not negative-clamped twice)", u.Occupying)
	}
}
