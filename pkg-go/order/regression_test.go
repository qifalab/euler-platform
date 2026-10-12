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

// A negative UsedPeriods used to make remaining > total, refunding more than
// the customer paid. Malformed proration input must never refund at all.
func TestRefundAmountRejectsNegativeUsedPeriods(t *testing.T) {
	paid := pricing.MustParseAmount("1200")
	for _, used := range []int64{-1, -12, -1 << 40} {
		b := ProrationBasis{SnapshotAmount: paid, TotalPeriods: 12, UsedPeriods: used}
		if got := b.RefundAmount(); !got.IsZero() {
			t.Fatalf("UsedPeriods=%d refunds %s, want 0 (never more than paid)", used, got)
		}
	}
	neg := ProrationBasis{SnapshotAmount: pricing.MustParseAmount("-10"), TotalPeriods: 12, UsedPeriods: 1}
	if got := neg.RefundAmount(); !got.IsZero() {
		t.Fatalf("negative snapshot refunds %s, want 0", got)
	}
	// Valid input is unchanged: 11 of 12 periods unused.
	ok := ProrationBasis{SnapshotAmount: paid, TotalPeriods: 12, UsedPeriods: 1}
	if got := ok.RefundAmount().String(); got != "1100" {
		t.Fatalf("refund = %s, want 1100", got)
	}
}
