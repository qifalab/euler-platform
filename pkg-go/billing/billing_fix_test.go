package billing

import (
	"testing"
	"time"
)

// BillPeriod must be derived in UTC, matching metering.FreezeBoundary/Frozen.
// An hour near the month boundary expressed in a non-UTC zone used to land in
// the neighbouring month's bill while freeze logic treated it as this month's.
func TestBillPeriodDerivedInUTC(t *testing.T) {
	e := newEngine()
	// 2026-08-31 23:00 UTC expressed as 2026-09-01 07:00 in UTC+8: the local
	// Format would say "2026-09" while the hour belongs to August in UTC.
	cst := time.FixedZone("CST", 8*3600)
	u := usage("2", 100)
	u.HourStart = time.Date(2026, 9, 1, 7, 0, 0, 0, cst) // == 2026-08-31 23:00 UTC
	s, err := e.Settle(u, amt("0.25"), "euecs", "snap-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Charge.BillPeriod != "2026-08" {
		t.Fatalf("BillPeriod = %s, want 2026-08 (UTC month)", s.Charge.BillPeriod)
	}
	if !s.Charge.BillingCycle.Equal(time.Date(2026, 8, 31, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("BillingCycle = %v, want the UTC instant", s.Charge.BillingCycle)
	}
}

// A monthly bill whose only imbalance is recorded arrears must reach Final()
// once backfill completes — the shortfall is explained, not unexplained.
func TestShortfallChargesStillReconcileInSummary(t *testing.T) {
	e := newEngine()
	s, err := e.Settle(usage("2", 100), amt("0.25"), "euecs", "snap-1", nil) // no pools → full shortfall
	if err != nil {
		t.Fatal(err)
	}
	bill := Summarize(100123, s.Charge.BillPeriod, []Charge{s.Charge})
	if bill.UnreconciledCount != 0 {
		t.Fatalf("UnreconciledCount = %d, want 0 (shortfall is explained)", bill.UnreconciledCount)
	}
	if !bill.Final() {
		t.Fatal("a complete bill with explained arrears must be issuable")
	}
	// A genuinely corrupted charge (components do not add up) still fails.
	bad := s.Charge
	bad.Shortfall = 0
	badBill := Summarize(100123, bad.BillPeriod, []Charge{bad})
	if badBill.UnreconciledCount != 1 || badBill.Final() {
		t.Fatalf("corrupted charge must stay unreconciled: %+v", badBill)
	}
}
