package spot

import (
	"testing"
	"time"

	"github.com/starcloud/sc-platform/pricing"
)

var t0 = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func clock(t *testing.T) *time.Time {
	t.Helper()
	c := t0
	return &c
}

func newEngine(c *time.Time) *Engine {
	return NewEngine(func() time.Time { return *c })
}

func yuan(s string) pricing.Amount { return pricing.MustParseAmount(s) }

func TestPublishOpensRecordAndPriceAtReturnsIt(t *testing.T) {
	c := t0
	eng := newEngine(&c)
	p := eng.Publish("scecs", "cn-north-1", yuan("0.50"))
	if p.PricePerHour != yuan("0.50") {
		t.Fatalf("price = %s, want 0.50", p.PricePerHour)
	}
	if !p.EffectiveTo.IsZero() {
		t.Fatal("new record must be open-ended (EffectiveTo zero)")
	}
	// Price in force at t0.
	got, err := eng.PriceAt("scecs", "cn-north-1", t0)
	if err != nil {
		t.Fatalf("priceAt: %v", err)
	}
	if got.PricePerHour != yuan("0.50") {
		t.Fatalf("priceAt = %s, want 0.50", got.PricePerHour)
	}
}

func TestPublishClosesPreviousRecord(t *testing.T) {
	c := t0
	eng := newEngine(&c)
	eng.Publish("scecs", "cn-north-1", yuan("0.50"))
	c = t0.Add(10 * time.Minute)
	eng.Publish("scecs", "cn-north-1", yuan("0.40"))

	hist, err := eng.History("scecs", "cn-north-1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2 (append-only)", len(hist))
	}
	// First record's open end must now be closed at the second's start.
	if !hist[0].EffectiveTo.Equal(hist[1].EffectiveFrom) {
		t.Fatalf("previous record not closed: EffectiveTo=%s, next EffectiveFrom=%s",
			hist[0].EffectiveTo, hist[1].EffectiveFrom)
	}
	if !hist[1].EffectiveTo.IsZero() {
		t.Fatal("current (last) record must remain open-ended")
	}
}

func TestAppendOnlyDoesNotRewriteHistory(t *testing.T) {
	c := t0
	eng := newEngine(&c)
	eng.Publish("scecs", "cn-north-1", yuan("0.50"))
	c = t0.Add(10 * time.Minute)
	eng.Publish("scecs", "cn-north-1", yuan("0.40"))
	c = t0.Add(20 * time.Minute)
	eng.Publish("scecs", "cn-north-1", yuan("0.30"))

	// A cycle billed at t0+5min must ALWAYS resolve to 0.50, no matter how
	// many later publishes occur. This is the reproducibility invariant.
	p, err := eng.PriceAt("scecs", "cn-north-1", t0.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("priceAt: %v", err)
	}
	if p.PricePerHour != yuan("0.50") {
		t.Fatalf("historical price rewritten: %s, want 0.50", p.PricePerHour)
	}
	// And t0+15min → 0.40.
	p, err = eng.PriceAt("scecs", "cn-north-1", t0.Add(15*time.Minute))
	if err != nil {
		t.Fatalf("priceAt: %v", err)
	}
	if p.PricePerHour != yuan("0.40") {
		t.Fatalf("price = %s, want 0.40", p.PricePerHour)
	}
}

func TestPriceAtRejectsFutureCycle(t *testing.T) {
	c := t0
	eng := newEngine(&c)
	eng.Publish("scecs", "cn-north-1", yuan("0.50"))
	// A cycle one hour in the future must not bill against a price that has
	// not been set yet.
	if _, err := eng.PriceAt("scecs", "cn-north-1", t0.Add(time.Hour)); err == nil {
		t.Fatal("future cycle should be rejected")
	}
}

func TestPriceAtRejectsUnknownProduct(t *testing.T) {
	c := t0
	eng := newEngine(&c)
	if _, err := eng.PriceAt("scecs", "cn-north-1", t0); err == nil {
		t.Fatal("unknown product/region should be rejected")
	}
}

func TestCurrentReturnsLatestPrice(t *testing.T) {
	c := t0
	eng := newEngine(&c)
	eng.Publish("scecs", "cn-north-1", yuan("0.50"))
	c = t0.Add(10 * time.Minute)
	eng.Publish("scecs", "cn-north-1", yuan("0.40"))
	p, err := eng.Current("scecs", "cn-north-1")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if p.PricePerHour != yuan("0.40") {
		t.Fatalf("current = %s, want 0.40 (latest)", p.PricePerHour)
	}
}

func TestReclaimNoticeWindowIsFiveMinutes(t *testing.T) {
	if ReclaimNoticeWindow != 5*time.Minute {
		t.Fatalf("reclaim notice window = %s, want 5m (09 §4.2)", ReclaimNoticeWindow)
	}
}
