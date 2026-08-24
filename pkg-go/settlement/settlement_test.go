package settlement

import (
	"testing"

	"github.com/starcloud/sc-platform/pricing"
)

func TestSettleSumsExactly(t *testing.T) {
	total := pricing.MustParseAmount("100")
	s, err := Settle(total, 3000) // 30% partner
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if want := pricing.MustParseAmount("30"); s.Partner != want {
		t.Errorf("partner share = %s, want %s", s.Partner, want)
	}
	if want := pricing.MustParseAmount("70"); s.Platform != want {
		t.Errorf("platform share = %s, want %s", s.Platform, want)
	}
	if !s.Valid() {
		t.Error("split must reconcile (Partner + Platform == Total exactly)")
	}
}

func TestSettleReconcilesOddAmounts(t *testing.T) {
	// A total that does not divide evenly must still sum back exactly — the
	// platform share absorbs the rounding so no micro-unit is lost or invented.
	total := pricing.MustParseAmount("0.01") // 10000 micro-units
	s, err := Settle(total, 3333)            // 33.33%
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if !s.Valid() {
		t.Fatalf("split does not reconcile: %s + %s != %s", s.Partner, s.Platform, s.Total)
	}
	if s.Partner.IsNegative() || s.Platform.IsNegative() {
		t.Fatal("no share may be negative")
	}
}

func TestSettleFullAndZero(t *testing.T) {
	total := pricing.MustParseAmount("42")
	full, _ := Settle(total, RateBasisPointsFull)
	if full.Partner != total || !full.Platform.IsZero() {
		t.Fatalf("100%% partner: partner=%s platform=%s", full.Partner, full.Platform)
	}
	zero, _ := Settle(total, 0)
	if !zero.Partner.IsZero() || zero.Platform != total {
		t.Fatalf("0%% partner: partner=%s platform=%s", zero.Partner, zero.Platform)
	}
}

func TestSettleRejectsInvalidRate(t *testing.T) {
	for _, r := range []Rate{-1, RateBasisPointsFull + 1} {
		if _, err := Settle(pricing.MustParseAmount("1"), r); err == nil {
			t.Errorf("rate %d accepted, want rejection", r)
		}
	}
}

func TestSettleRejectsNegativeTotal(t *testing.T) {
	if _, err := Settle(pricing.MustParseAmount("-1"), 1000); err == nil {
		t.Fatal("negative total accepted, want rejection")
	}
}

func TestSplitValidRejectsTampered(t *testing.T) {
	s, _ := Settle(pricing.MustParseAmount("100"), 3000)
	tampered := s
	tampered.Partner = tampered.Partner.Add(pricing.MustParseAmount("0.000001")) // 1 micro-unit off
	if tampered.Valid() {
		t.Fatal("a split off by one micro-unit must not reconcile")
	}
}
