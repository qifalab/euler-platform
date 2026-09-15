package reservepack

import (
	"errors"
	"testing"
	"time"
)

// The idempotent replay must report the ORIGINAL shortfall. Returning zero
// tells a retrying settlement that the pack covered the whole charge, so it
// skips the next waterfall tier and the platform under-bills.
func TestConsumeReplayKeepsOriginalShortfall(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(time.Hour)
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("100"), expire, "ord-1"); err != nil {
		t.Fatal(err)
	}
	_, _, shortfall, err := l.Consume("pk-1", "euecs", yuan("250"), "bill-1", "c-1")
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if shortfall.String() != "150" {
		t.Fatalf("shortfall = %s, want 150", shortfall)
	}

	_, _, replayShortfall, err := l.Consume("pk-1", "euecs", yuan("250"), "bill-1", "c-1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayShortfall.String() != "150" {
		t.Fatalf("replay shortfall = %s, want 150 (not zero)", replayShortfall)
	}
}

// A lost optimistic-lock race is a version conflict, not "pack not found":
// reporting it as not-found sent the settlement path down a 500 for a
// condition the caller can simply retry.
func TestStoreApplyReportsVersionConflict(t *testing.T) {
	s := NewMemoryStore()
	pack := Pack{
		PackID: "pk-1", AccountID: 100123, FaceValue: yuan("100"),
		Remaining: yuan("100"), Status: StatusActive, Version: 2,
	}
	first := Entry{
		EntryID: 1, PackID: "pk-1", Type: EntryPurchase,
		Amount: yuan("100"), Balance: yuan("100"), IdempotencyKey: "k-1",
	}
	if err := s.Apply(first, pack, 0); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second := first
	second.EntryID = 2
	second.IdempotencyKey = "k-2"
	if err := s.Apply(second, pack, 1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale version: err = %v, want ErrVersionConflict", err)
	}
	if err := s.Apply(second, pack, 2); err != nil {
		t.Fatalf("current version must apply: %v", err)
	}
}

// A pack past its deadline is dead even before the sweep flips the row: a
// refund into it would report success while the customer gets nothing usable.
func TestRefundAfterDeadlineIsRefused(t *testing.T) {
	l, store := newTestLedger()
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("100"), testNow.Add(time.Minute), "ord-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.Consume("pk-1", "euecs", yuan("40"), "bill-1", "c-1"); err != nil {
		t.Fatal(err)
	}
	later := New(store, func() time.Time { return testNow.Add(time.Hour) }, nil)
	if _, _, err := later.Refund("pk-1", yuan("40"), "bill-1", "r-1"); !errors.Is(err, ErrPackExpired) {
		t.Fatalf("refund after deadline: err = %v, want ErrPackExpired", err)
	}
}

// Movement amounts are magnitudes: a negative "consume" would flow through
// Min() as a negative take and credit quota instead of spending it.
func TestNonPositiveAmountsAreRejected(t *testing.T) {
	l, _ := newTestLedger()
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("100"), testNow.Add(time.Hour), "ord-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.Consume("pk-1", "euecs", yuan("-5"), "bill-1", "c-1"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("negative consume: err = %v, want ErrInvalidAmount", err)
	}
	if _, _, err := l.Refund("pk-1", yuan("-5"), "bill-1", "r-1"); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("negative refund: err = %v, want ErrInvalidAmount", err)
	}
	p, _, err := l.store.GetPack("pk-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Remaining.String() != "100" {
		t.Fatalf("remaining = %s, want 100 (rejected calls must not move quota)", p.Remaining)
	}
}
