package billing

import (
	"errors"
	"testing"
	"time"

	"github.com/starcloud/sc-platform/metering"
	"github.com/starcloud/sc-platform/pricing"
)

var (
	billHour = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	settleAt = time.Date(2026, 8, 8, 13, 15, 0, 0, time.UTC)
)

func newEngine() *Engine {
	return NewEngine(func() time.Time { return settleAt })
}

func usage(qty string, covered int) metering.HourlyUsage {
	return metering.HourlyUsage{
		AggID:           metering.AggID("scecs-cn-north-1-01-a1b2c3d4", "cpu_core_hour", billHour),
		AccountID:       100123,
		Region:          "cn-north-1",
		ResourceType:    "ecs",
		ResourceID:      "scecs-cn-north-1-01-a1b2c3d4",
		MeteringItem:    "cpu_core_hour",
		TotalQuantity:   metering.MustParseQuantity(qty),
		HourStart:       billHour,
		CoveredRatio:    covered,
		WindowsSeen:     covered * 60 / 100,
		WindowsExpected: 60,
	}
}

func amt(s string) pricing.Amount { return pricing.MustParseAmount(s) }

// --- Basic charging ---

func TestSettleComputesPretax(t *testing.T) {
	e := newEngine()
	// 2 core-hours at 0.25/core-hour = 0.50
	s, err := e.Settle(usage("2", 100), amt("0.25"), "scecs", "snap-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Charge.PretaxAmount.String(); got != "0.5" {
		t.Fatalf("pretax = %s, want 0.5", got)
	}
	if s.Charge.BillPeriod != "2026-08" {
		t.Fatalf("period = %s, want 2026-08", s.Charge.BillPeriod)
	}
}

func TestFractionalQuantityCharging(t *testing.T) {
	e := newEngine()
	// 1.99998 core-hours at 0.25 = 0.499995
	s, err := e.Settle(usage("1.99998", 100), amt("0.25"), "scecs", "snap-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Charge.PretaxAmount.String(); got != "0.499995" {
		t.Fatalf("pretax = %s, want 0.499995", got)
	}
}

// TestChargeIDDerivesFromAggID makes re-settlement idempotent: running the
// hour again produces the same id, and the unique index rejects the duplicate.
func TestChargeIDDerivesFromAggID(t *testing.T) {
	e := newEngine()
	u := usage("2", 100)
	a, _ := e.Settle(u, amt("0.25"), "scecs", "snap-1", nil)
	b, _ := e.Settle(u, amt("0.25"), "scecs", "snap-1", nil)
	if a.Charge.ChargeID != b.Charge.ChargeID {
		t.Fatalf("charge ids differ across runs: %s vs %s", a.Charge.ChargeID, b.Charge.ChargeID)
	}
	if a.Charge.ChargeID != ChargeID(u.AggID) {
		t.Fatal("charge id must derive from the aggregate id")
	}
}

// --- Deduction waterfall ---

// TestWaterfallOrder pins 01§12.3: 资源包 → 代金券 → 现金余额.
func TestWaterfallOrder(t *testing.T) {
	e := newEngine()
	pools := []Pool{
		{Source: SourceBalance, Available: amt("100")},
		{Source: SourceCoupon, Ref: "coupon-1", Available: amt("0.2"), ExpireAt: settleAt.AddDate(0, 1, 0)},
		{Source: SourceResourcePack, Ref: "pack-1", Available: amt("0.1")},
	}
	// Charge 0.5: pack 0.1, coupon 0.2, cash 0.2.
	s, err := e.Settle(usage("2", 100), amt("0.25"), "scecs", "snap-1", pools)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Charge.Deductions) != 3 {
		t.Fatalf("deductions = %+v, want 3", s.Charge.Deductions)
	}
	want := []struct {
		src DeductionSource
		amt string
	}{
		{SourceResourcePack, "0.1"},
		{SourceCoupon, "0.2"},
		{SourceBalance, "0.2"},
	}
	for i, w := range want {
		d := s.Charge.Deductions[i]
		if d.Source != w.src || d.Amount.String() != w.amt {
			t.Errorf("deduction %d = %s %s, want %s %s", i, d.Source, d.Amount, w.src, w.amt)
		}
	}
	if s.Charge.PayAmount.String() != "0.2" {
		t.Fatalf("cash paid = %s, want 0.2", s.Charge.PayAmount)
	}
	if !s.Charge.Reconciles() {
		t.Fatal("charge components must add up to pretax")
	}
}

// TestCouponsSpentExpiryAscending is the customer-favourable ordering: credit
// that would otherwise be wasted goes first.
func TestCouponsSpentExpiryAscending(t *testing.T) {
	e := newEngine()
	pools := []Pool{
		{Source: SourceCoupon, Ref: "later", Available: amt("1"), ExpireAt: settleAt.AddDate(0, 2, 0)},
		{Source: SourceCoupon, Ref: "sooner", Available: amt("1"), ExpireAt: settleAt.AddDate(0, 1, 0)},
		{Source: SourceCoupon, Ref: "never", Available: amt("1")},
		{Source: SourceBalance, Available: amt("100")},
	}
	// Charge 2.5 consumes sooner (1), later (1), never (0.5).
	s, _ := e.Settle(usage("10", 100), amt("0.25"), "scecs", "snap-1", pools)

	refs := []string{}
	for _, d := range s.Charge.Deductions {
		if d.Source == SourceCoupon {
			refs = append(refs, d.Ref)
		}
	}
	if len(refs) != 3 || refs[0] != "sooner" || refs[1] != "later" || refs[2] != "never" {
		t.Fatalf("coupon order = %v, want [sooner later never]", refs)
	}
}

func TestCouponScopeRestriction(t *testing.T) {
	e := newEngine()
	pools := []Pool{
		{Source: SourceCoupon, Ref: "storage-only", Available: amt("10"), ProductCodes: []string{"scoss"}},
		{Source: SourceBalance, Available: amt("100")},
	}
	s, _ := e.Settle(usage("2", 100), amt("0.25"), "scecs", "snap-1", pools)

	for _, d := range s.Charge.Deductions {
		if d.Source == SourceCoupon {
			t.Fatal("a storage-scoped coupon must not pay for a compute charge")
		}
	}
	if s.Charge.PayAmount.String() != "0.5" {
		t.Fatalf("cash should cover the whole charge, got %s", s.Charge.PayAmount)
	}
}

// --- Arrears ---

// TestShortfallEntersArrearsNotNegativeBalance guards the rule that overdraft
// is a lifecycle state, never a negative ledger balance.
func TestShortfallEntersArrearsNotNegativeBalance(t *testing.T) {
	e := newEngine()
	pools := []Pool{{Source: SourceBalance, Available: amt("0.3")}}

	// Charge 0.5 against a 0.3 balance.
	s, err := e.Settle(usage("2", 100), amt("0.25"), "scecs", "snap-1", pools)
	if err != nil {
		t.Fatal(err)
	}
	if !s.InArrears {
		t.Fatal("an uncoverable charge must put the account into arrears")
	}
	if got := s.Shortfall.String(); got != "0.2" {
		t.Fatalf("shortfall = %s, want 0.2", got)
	}
	// What could be paid, was paid.
	if s.Charge.PayAmount.String() != "0.3" {
		t.Fatalf("cash paid = %s, want 0.3", s.Charge.PayAmount)
	}
	// The charge DOES reconcile: the unpaid part is explained by the recorded
	// shortfall (deductions + shortfall = pretax), so an arrears line does not
	// keep the monthly bill unreconcilable forever.
	if !s.Charge.Reconciles() {
		t.Fatal("a charge whose shortfall is recorded must reconcile")
	}
	if s.Charge.Shortfall.String() != "0.2" {
		t.Fatalf("charge shortfall = %s, want 0.2", s.Charge.Shortfall)
	}
}

func TestNoPoolsMeansFullShortfall(t *testing.T) {
	e := newEngine()
	s, _ := e.Settle(usage("2", 100), amt("0.25"), "scecs", "snap-1", nil)
	if !s.InArrears || s.Shortfall.String() != "0.5" {
		t.Fatalf("expected full shortfall, got %+v", s)
	}
}

func TestZeroChargeNeedsNoDeduction(t *testing.T) {
	// Free tiers (SCVPC, SCMON basic) settle to nothing without touching any
	// pool — a zero charge that consumed a voucher would be theft of credit.
	e := newEngine()
	pools := []Pool{{Source: SourceCoupon, Ref: "c1", Available: amt("100")}}
	s, err := e.Settle(usage("5", 100), amt("0"), "scvpc", "snap-1", pools)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Charge.PretaxAmount.IsZero() {
		t.Fatalf("pretax = %s, want 0", s.Charge.PretaxAmount)
	}
	if len(s.Charge.Deductions) != 0 {
		t.Fatalf("a zero charge must not consume credit: %+v", s.Charge.Deductions)
	}
	if s.InArrears {
		t.Fatal("a zero charge cannot cause arrears")
	}
}

// --- Incomplete data ---

// TestIncompleteHourIsVisibleOnTheCharge ensures a bill line built on partial
// metering carries that fact, rather than the incompleteness being known only
// inside the pipeline.
func TestIncompleteHourIsVisibleOnTheCharge(t *testing.T) {
	e := newEngine()
	s, err := e.Settle(usage("1.5", 75), amt("0.25"), "scecs", "snap-1",
		[]Pool{{Source: SourceBalance, Available: amt("100")}})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Charge.Incomplete() {
		t.Fatal("an hour covered at 75 percent must be flagged incomplete on the charge")
	}
	if s.Charge.CoveredRatio != 75 {
		t.Fatalf("covered_ratio = %d, want 75", s.Charge.CoveredRatio)
	}
}

// --- Frozen period ---

func TestFrozenPeriodRejectsSettlement(t *testing.T) {
	// August closes on 1 September 06:00. Settling August usage afterwards
	// would silently change a bill the customer already paid.
	late := NewEngine(func() time.Time {
		return time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	})
	_, err := late.Settle(usage("2", 100), amt("0.25"), "scecs", "snap-1", nil)
	if !errors.Is(err, ErrFrozenPeriod) {
		t.Fatalf("expected ErrFrozenPeriod, got %v", err)
	}
}

func TestOpenPeriodAllowsSettlement(t *testing.T) {
	open := NewEngine(func() time.Time {
		return time.Date(2026, 9, 1, 5, 0, 0, 0, time.UTC) // one hour before freeze
	})
	if _, err := open.Settle(usage("2", 100), amt("0.25"), "scecs", "snap-1", nil); err != nil {
		t.Fatalf("period should still be open: %v", err)
	}
}

// --- Monthly summary ---

func TestSummarizeFoldsCharges(t *testing.T) {
	charges := []Charge{
		{AccountID: 100123, BillPeriod: "2026-08", PretaxAmount: amt("0.5"), PayAmount: amt("0.5"),
			CoveredRatio: 100, Deductions: []Deduction{{Source: SourceBalance, Amount: amt("0.5")}}},
		{AccountID: 100123, BillPeriod: "2026-08", PretaxAmount: amt("1.2"), PayAmount: amt("1.2"),
			CoveredRatio: 100, Deductions: []Deduction{{Source: SourceBalance, Amount: amt("1.2")}}},
		// Another account's charge must not leak in.
		{AccountID: 999, BillPeriod: "2026-08", PretaxAmount: amt("99"), CoveredRatio: 100},
		// Another period's charge must not leak in.
		{AccountID: 100123, BillPeriod: "2026-07", PretaxAmount: amt("50"), CoveredRatio: 100},
	}
	bill := Summarize(100123, "2026-08", charges)

	if bill.ChargeCount != 2 {
		t.Fatalf("charge count = %d, want 2", bill.ChargeCount)
	}
	if bill.TotalAmount.String() != "1.7" {
		t.Fatalf("total = %s, want 1.7", bill.TotalAmount)
	}
	if !bill.Final() {
		t.Fatal("a complete, reconciled bill should be final")
	}
}

// TestIncompleteBillIsNotFinal covers the reason CoveredRatio rides all the way
// to the bill: issuing a provisional total as final guarantees a dispute when
// backfill later changes it.
func TestIncompleteBillIsNotFinal(t *testing.T) {
	charges := []Charge{
		{AccountID: 100123, BillPeriod: "2026-08", PretaxAmount: amt("0.5"), CoveredRatio: 100,
			Deductions: []Deduction{{Source: SourceBalance, Amount: amt("0.5")}}},
		{AccountID: 100123, BillPeriod: "2026-08", PretaxAmount: amt("0.3"), CoveredRatio: 80,
			Deductions: []Deduction{{Source: SourceBalance, Amount: amt("0.3")}}},
	}
	bill := Summarize(100123, "2026-08", charges)
	if bill.IncompleteCharges != 1 {
		t.Fatalf("incomplete count = %d, want 1", bill.IncompleteCharges)
	}
	if bill.Final() {
		t.Fatal("a bill containing provisional lines must not be final")
	}
}

// TestUnreconciledLineBlocksFinalBill is the per-bill expression of 09 A2:
// 无未解释差异. A line whose components do not add up is exactly such a
// difference.
func TestUnreconciledLineBlocksFinalBill(t *testing.T) {
	charges := []Charge{
		{AccountID: 100123, BillPeriod: "2026-08", PretaxAmount: amt("1"), CoveredRatio: 100,
			Deductions: []Deduction{{Source: SourceBalance, Amount: amt("0.7")}}}, // 0.3 unaccounted
	}
	bill := Summarize(100123, "2026-08", charges)
	if bill.UnreconciledCount != 1 {
		t.Fatalf("unreconciled = %d, want 1", bill.UnreconciledCount)
	}
	if bill.Final() {
		t.Fatal("an unreconciled line must block a final bill")
	}
}

func TestChargeReconcilesInvariant(t *testing.T) {
	c := Charge{
		PretaxAmount: amt("1.5"),
		Deductions: []Deduction{
			{Source: SourceCoupon, Ref: "c1", Amount: amt("0.5")},
			{Source: SourceBalance, Amount: amt("1")},
		},
	}
	if !c.Reconciles() {
		t.Fatalf("0.5 + 1 should reconcile against 1.5, total = %s", c.TotalDeducted())
	}
	c.Deductions[1].Amount = amt("0.9")
	if c.Reconciles() {
		t.Fatal("0.5 + 0.9 must not reconcile against 1.5")
	}
}

func TestNegativeUnitPriceRejected(t *testing.T) {
	e := newEngine()
	if _, err := e.Settle(usage("2", 100), amt("-0.25"), "scecs", "snap-1", nil); !errors.Is(err, ErrNoUnitPrice) {
		t.Fatalf("expected ErrNoUnitPrice, got %v", err)
	}
}

// --- Cost analysis (phase 2, M-4.3) ---

func TestCostAnalysisBreaksDownByProduct(t *testing.T) {
	charges := []Charge{
		{AccountID: 100123, BillPeriod: "2026-08", ProductCode: "scecs", ResourceID: "r1", PretaxAmount: amt("700"), CoveredRatio: 100},
		{AccountID: 100123, BillPeriod: "2026-08", ProductCode: "scecs", ResourceID: "r2", PretaxAmount: amt("300"), CoveredRatio: 100},
		{AccountID: 100123, BillPeriod: "2026-08", ProductCode: "scoss", ResourceID: "r3", PretaxAmount: amt("50"), CoveredRatio: 100},
		// Other account/period must not leak in.
		{AccountID: 999, BillPeriod: "2026-08", ProductCode: "scecs", ResourceID: "x", PretaxAmount: amt("999"), CoveredRatio: 100},
		{AccountID: 100123, BillPeriod: "2026-07", ProductCode: "scecs", ResourceID: "r1", PretaxAmount: amt("100"), CoveredRatio: 100},
	}
	r := CostAnalysis(100123, "2026-08", charges)
	if r.ByProduct["scecs"] != amt("1000") {
		t.Fatalf("scecs = %s, want 1000", r.ByProduct["scecs"])
	}
	if r.ByProduct["scoss"] != amt("50") {
		t.Fatalf("scoss = %s, want 50", r.ByProduct["scoss"])
	}
	if r.ByResource["r1"] != amt("700") {
		t.Fatalf("r1 = %s, want 700", r.ByResource["r1"])
	}
	if r.Total != amt("1050") {
		t.Fatalf("total = %s, want 1050", r.Total)
	}
}

func TestCostAnalysisAgreesWithSummarize(t *testing.T) {
	// The cost report and the bill must agree on total — a divergence is the
	// exact 无未解释差异 the Gate review rejects (09 A2).
	charges := []Charge{
		{AccountID: 100123, BillPeriod: "2026-08", ProductCode: "scecs", ResourceID: "r1",
			PretaxAmount: amt("700"), PayAmount: amt("700"), CoveredRatio: 100,
			Deductions: []Deduction{{Source: SourceBalance, Amount: amt("700")}}},
		{AccountID: 100123, BillPeriod: "2026-08", ProductCode: "scoss", ResourceID: "r2",
			PretaxAmount: amt("300"), PayAmount: amt("300"), CoveredRatio: 100,
			Deductions: []Deduction{{Source: SourceBalance, Amount: amt("300")}}},
	}
	bill := Summarize(100123, "2026-08", charges)
	report := CostAnalysis(100123, "2026-08", charges)
	if bill.TotalAmount != report.Total {
		t.Fatalf("bill total %s != cost total %s", bill.TotalAmount, report.Total)
	}
}
