package reservepack

import (
	"errors"
	"testing"
	"time"
)

// Purchase must not silently re-credit an existing pack: a duplicate packID
// under a DIFFERENT order key would reset a partially consumed pack to full.
// The same order key remains an idempotent replay.
func TestPurchaseRejectsExistingPackID(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-dup", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatal(err)
	}
	// Draw the pack down so an overwrite would be visible.
	if _, _, _, err := l.Consume("pk-dup", "euecs", yuan("400"), "bill-1", "c-1"); err != nil {
		t.Fatal(err)
	}

	// A different order key against the same pack id must be rejected.
	if _, _, err := l.Purchase("pk-dup", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-2"); !errors.Is(err, ErrPackExists) {
		t.Fatalf("re-purchase err = %v, want ErrPackExists", err)
	}
	p, _, _ := l.store.GetPack("pk-dup")
	if p.Remaining.String() != "600" {
		t.Fatalf("remaining = %s, want 600 (not reset to face value)", p.Remaining)
	}

	// The ORIGINAL order key still replays idempotently.
	pk, entry, err := l.Purchase("pk-dup", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1")
	if err != nil {
		t.Fatalf("idempotent replay must succeed: %v", err)
	}
	if entry.Type != EntryPurchase || pk.Remaining.String() != "600" {
		t.Fatalf("replay returned %+v / remaining %s", entry, pk.Remaining)
	}
}
