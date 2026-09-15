package order

import (
	"errors"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/pricing"
)

// An expired quote must not become an order even when the caller omits At.
// Create resolves the clock itself in that case, and a zero At is always
// "before" any expiry, so the Validate check alone could not catch it — the
// authoritative check has to run against the resolved clock.
func TestCreateRejectsExpiredSnapshotWithZeroAt(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	m := NewMachine(func() time.Time { return now })
	req := CreateRequest{
		AccountID:   100123,
		Type:        TypeNew,
		ProductCode: "euecs",
		ChargeType:  pricing.ChargePrepaid,
		ClientToken: "tok-expired",
		Quote:       pricing.Result{PayableAmount: pricing.MustParseAmount("10")},
		// Expired one hour ago; At is deliberately left zero.
		SnapshotExpiresAt: now.Add(-time.Hour),
	}
	if _, _, err := m.Create(req, 1, "SO1"); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("err = %v, want ErrSnapshotExpired", err)
	}

	// A live snapshot still creates, and the same request pinned to At is also
	// rejected once the expiry passes.
	req.SnapshotExpiresAt = now.Add(time.Hour)
	if _, _, err := m.Create(req, 1, "SO1"); err != nil {
		t.Fatalf("live snapshot must create: %v", err)
	}
	req.At = now.Add(2 * time.Hour)
	if _, _, err := m.Create(req, 1, "SO2"); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("err = %v, want ErrSnapshotExpired for a pinned stale At", err)
	}
}
