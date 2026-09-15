package reservepack

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/billing"
	"github.com/qifalab/euler-platform/metering"
	"github.com/qifalab/euler-platform/pricing"
)

var testNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testNow }

func newTestLedger() (*Ledger, *MemoryStore) {
	var seq int64
	store := NewMemoryStore()
	l := New(store, fixedNow, func() int64 { seq++; return seq })
	return l, store
}

// yuan parses a decimal-yuan string into the fixed-point micro-unit Amount.
// pricing.Amount is micro-units (1/1_000_000 yuan); using MustParseAmount keeps
// the tests honest about the scale (yuan(1000) as a raw Amount would be 0.001
// yuan — the silent-scale trap that makes bills unreconcilable).
func yuan(s string) pricing.Amount { return pricing.MustParseAmount(s) }

func TestPurchaseCreditsQuota(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	pack, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1")
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if pack.Remaining != yuan("1000") {
		t.Fatalf("remaining = %s, want 1000", pack.Remaining)
	}
	if pack.Status != StatusActive {
		t.Fatalf("status = %s, want ACTIVE", pack.Status)
	}
	if pack.FaceValue != yuan("1000") {
		t.Fatalf("face = %s, want 1000", pack.FaceValue)
	}
	if pack.Version != 1 {
		t.Fatalf("version = %d, want 1", pack.Version)
	}
}

func TestPurchaseIsIdempotent(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase 1: %v", err)
	}
	// Replay with same idempotency key: no-op, not a double-credit.
	pack, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1")
	if err != nil {
		t.Fatalf("purchase 2: %v", err)
	}
	if pack.Remaining != yuan("1000") {
		t.Fatalf("double-credit: remaining = %s, want 1000", pack.Remaining)
	}
}

func TestConsumeDrawsQuotaAndReturnsShortfall(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("300"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	// Request 500; pack only has 300 → takes 300, shortfall 200.
	pack, _, shortfall, err := l.Consume("pk-1", "euecs", yuan("500"), "chg-1", "consume-1")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if pack.Remaining != 0 {
		t.Fatalf("remaining = %s, want 0 (exhausted)", pack.Remaining)
	}
	if pack.Status != StatusExhausted {
		t.Fatalf("status = %s, want EXHAUSTED", pack.Status)
	}
	if shortfall != yuan("200") {
		t.Fatalf("shortfall = %s, want 200", shortfall)
	}
}

func TestConsumeIsIdempotent(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if _, _, _, err := l.Consume("pk-1", "euecs", yuan("400"), "chg-1", "consume-1"); err != nil {
		t.Fatalf("consume 1: %v", err)
	}
	// Replay: must not double-spend.
	pack, _, shortfall, err := l.Consume("pk-1", "euecs", yuan("400"), "chg-1", "consume-1")
	if err != nil {
		t.Fatalf("consume 2: %v", err)
	}
	if pack.Remaining != yuan("600") {
		t.Fatalf("double-spend: remaining = %s, want 600", pack.Remaining)
	}
	if shortfall != 0 {
		t.Fatalf("replay shortfall = %s, want 0", shortfall)
	}
}

func TestConsumeRejectsWrongProduct(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	// Pack scoped to euecs; a euoss charge may not draw on it.
	_, _, shortfall, err := l.Consume("pk-1", "euoss", yuan("100"), "chg-1", "consume-1")
	if err != ErrInsufficientQuota {
		t.Fatalf("err = %v, want ErrInsufficientQuota (product scope)", err)
	}
	if shortfall != yuan("100") {
		t.Fatalf("shortfall = %s, want 100 (nothing consumed)", shortfall)
	}
}

func TestRefundRevivesExhaustedPack(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("300"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if _, _, _, err := l.Consume("pk-1", "euecs", yuan("300"), "chg-1", "consume-1"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	// Refund reverses 100; pack revives to ACTIVE with 100.
	pack, _, err := l.Refund("pk-1", yuan("100"), "chg-1", "refund-1")
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if pack.Remaining != yuan("100") {
		t.Fatalf("remaining = %s, want 100", pack.Remaining)
	}
	if pack.Status != StatusActive {
		t.Fatalf("status = %s, want ACTIVE (revived)", pack.Status)
	}
}

func TestRefundCannotExceedFaceValue(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("300"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	// Refunding 200 on a full 300 pack would give 500 > 300 face.
	_, _, err := l.Refund("pk-1", yuan("200"), "chg-1", "refund-1")
	if !errors.Is(err, ErrRefundExceedsFace) {
		t.Fatalf("err = %v, want ErrRefundExceedsFace", err)
	}
}

func TestExpireForfeitsResidual(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(-1 * time.Hour) // already past deadline
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	pack, _, err := l.Expire("pk-1", "expire-1")
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if pack.Remaining != 0 {
		t.Fatalf("remaining = %s, want 0 (forfeited)", pack.Remaining)
	}
	if pack.Status != StatusExpired {
		t.Fatalf("status = %s, want EXPIRED", pack.Status)
	}
}

func TestExpireRefusesFutureDeadline(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour) // not yet reached
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	if _, _, err := l.Expire("pk-1", "expire-1"); err == nil {
		t.Fatal("expire of future-deadline pack should fail")
	}
}

func TestExpiredPackCannotBeConsumed(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(-1 * time.Hour)
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	_, _, shortfall, err := l.Consume("pk-1", "euecs", yuan("100"), "chg-1", "consume-1")
	if err == nil {
		t.Fatal("consume of expired pack should fail")
	}
	if shortfall != yuan("100") {
		t.Fatalf("shortfall = %s, want 100 (nothing taken)", shortfall)
	}
}

func TestSweepExpiredExpiresPastDeadlinePacks(t *testing.T) {
	l, _ := newTestLedger()
	past := testNow.Add(-1 * time.Hour)
	future := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-past", 100123, "euecs", "euecs.pack", yuan("1000"), past, "ord-1"); err != nil {
		t.Fatalf("purchase past: %v", err)
	}
	if _, _, err := l.Purchase("pk-future", 100123, "euecs", "euecs.pack", yuan("1000"), future, "ord-2"); err != nil {
		t.Fatalf("purchase future: %v", err)
	}
	expired, err := l.SweepExpired(100123)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(expired) != 1 || expired[0] != "pk-past" {
		t.Fatalf("expired = %v, want [pk-past]", expired)
	}
	// Sweep is idempotent.
	expired2, _ := l.SweepExpired(100123)
	if len(expired2) != 0 {
		t.Fatalf("second sweep expired = %v, want [] (idempotent)", expired2)
	}
}

func TestConcurrentConsumeCannotOverspend(t *testing.T) {
	l, store := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	// A pack with 1000 yuan; 50 concurrent consumes of 100 each.
	// Capacity admits 10 consumers (10×100=1000); the other 40 must take 0.
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, err := l.Consume("pk-1", "euecs", yuan("100"), "chg-1", fmt.Sprintf("consume-%d", i))
			// A consumer that lost the race gets a terminal/insufficient error;
			// that is expected, not a test failure.
			if err != nil && err != ErrInsufficientQuota && err != ErrPackExhausted && err != ErrPackTerminal {
				t.Errorf("consume %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// Re-read final state.
	pack, ok, _ := store.GetPack("pk-1")
	if !ok {
		t.Fatal("pack missing")
	}
	// Remaining must be exactly 0 (1000 fully consumed) and never negative.
	if pack.Remaining < 0 {
		t.Fatalf("overspent: remaining = %s (negative)", pack.Remaining)
	}
	if pack.Remaining != 0 {
		t.Fatalf("remaining = %s, want 0 (exact exhaustion)", pack.Remaining)
	}
	// Sum of all taken must equal exactly face value 1000.
	entries, _ := store.ListEntries("pk-1")
	var consumed pricing.Amount
	for _, e := range entries {
		if e.Type == EntryConsume {
			consumed = consumed.Add(e.Amount)
		}
	}
	if consumed != yuan("1000") {
		t.Fatalf("total consumed = %s, want 1000 (no more, no less)", consumed)
	}
}

// TestWaterfallIntegration proves the resource-pack tier feeds the billing
// waterfall at rank 0: a pack covers the charge first, cash only pays the
// shortfall. This is the M-4.1 invariant — B1 "资源包抵扣准确率 100%".
func TestWaterfallIntegration(t *testing.T) {
	l, store := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-1", 100123, "euecs", "euecs.pack", yuan("700"), expire, "ord-1"); err != nil {
		t.Fatalf("purchase: %v", err)
	}
	// Build the waterfall pools: resource pack + a cash pool.
	pools := []billing.Pool{
		{Source: billing.SourceBalance, Ref: "cash", Available: yuan("10000")},
	}
	packPools, err := PoolsForAccount(store, 100123, testNow)
	if err != nil {
		t.Fatalf("pools: %v", err)
	}
	pools = append(packPools, pools...)

	// A 1000 yuan charge: pack covers 700, cash covers 300.
	// unitPrice = 1 yuan/hour, quantity = 1000 hours → pretax = 1000 yuan.
	// metering.Quantity is micro-units, so 1000 hours = 1000 * 1e6.
	eng := billing.NewEngine(fixedNow)
	usage := metering.HourlyUsage{
		AggID:         "agg-1",
		AccountID:     100123,
		ResourceID:    "euecs-x",
		MeteringItem:  "cpu_core_hour",
		HourStart:     testNow,
		TotalQuantity: metering.Quantity(1000) * metering.Quantity(1_000_000),
		CoveredRatio:  100,
	}
	settle, err := eng.Settle(usage, yuan("1"), "euecs", "snap-1", pools)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// The waterfall must have spent the pack first (rank 0), then cash.
	var packDeduct, cashDeduct pricing.Amount
	for _, d := range settle.Charge.Deductions {
		switch d.Source {
		case billing.SourceResourcePack:
			packDeduct = packDeduct.Add(d.Amount)
		case billing.SourceBalance:
			cashDeduct = cashDeduct.Add(d.Amount)
		}
	}
	if packDeduct != yuan("700") {
		t.Fatalf("pack deducted = %s, want 700", packDeduct)
	}
	if cashDeduct != yuan("300") {
		t.Fatalf("cash deducted = %s, want 300", cashDeduct)
	}
	if !settle.Charge.Reconciles() {
		t.Fatal("charge does not reconcile (deductions != pretax)")
	}
	if settle.Shortfall != 0 {
		t.Fatalf("shortfall = %s, want 0 (fully covered)", settle.Shortfall)
	}
}
