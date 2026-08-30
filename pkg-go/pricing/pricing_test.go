package pricing

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// --- Amount: the money type ---

func TestParseAmountRoundTrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"180.50", "180.5"},
		{"180", "180"},
		{"0.000001", "0.000001"},
		{"0", "0"},
		{"1234567.891234", "1234567.891234"},
		{"-45.25", "-45.25"},
		{"  99.9  ", "99.9"},
	}
	for _, c := range cases {
		a, err := ParseAmount(c.in)
		if err != nil {
			t.Errorf("ParseAmount(%q): %v", c.in, err)
			continue
		}
		if got := a.String(); got != c.want {
			t.Errorf("ParseAmount(%q).String() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseAmountRejectsMalformed(t *testing.T) {
	bad := []string{"", "abc", "1.2.3", "1,000", "1.2345678", "--5", "+", "1e5"}
	for _, s := range bad {
		if _, err := ParseAmount(s); err == nil {
			t.Errorf("ParseAmount(%q) should have failed", s)
		}
	}
}

// TestNoFloatDrift is the reason this package exists. Summing 0.1 ten thousand
// times in float64 does not give exactly 1000; in Amount it must.
func TestNoFloatDrift(t *testing.T) {
	unit := MustParseAmount("0.1")
	var total Amount
	for i := 0; i < 10000; i++ {
		total = total.Add(unit)
	}
	if got := total.String(); got != "1000" {
		t.Fatalf("10000 × 0.1 = %s, want exactly 1000", got)
	}

	// The float equivalent, for contrast — this is what we are avoiding.
	var f float64
	for i := 0; i < 10000; i++ {
		f += 0.1
	}
	if f == 1000.0 {
		t.Log("note: float64 happened to land on 1000 here, but it is not guaranteed")
	}
}

func TestMulRateRoundsHalfUp(t *testing.T) {
	// 100 × 85% = 85 exactly.
	if got := MustParseAmount("100").MulRate(8500).String(); got != "85" {
		t.Errorf("100 × 8500bp = %s, want 85", got)
	}
	// A value that lands on a half at the last carried place rounds away
	// from zero, matching what appears on a printed invoice.
	if got := MustParseAmount("0.000001").MulRate(5000).String(); got != "0.000001" {
		t.Errorf("0.000001 × 50%% = %s, want 0.000001 (half-up)", got)
	}
}

// TestMulDivAvoidsBasisPointTruncation pins the reason MulDiv exists.
//
// Proration ratios are fractions of whole periods. Routing them through basis
// points truncates: 11/12 → 9166 bp instead of 9166.67, so a 1200 refund pays
// 1099.92. Eight 分 is trivial once, but the error always falls the same way —
// against whoever receives the money — so it is a systematic bias that
// accumulates across every refund and downgrade.
func TestMulDivAvoidsBasisPointTruncation(t *testing.T) {
	amount := MustParseAmount("1200")

	exact := amount.MulDiv(11, 12)
	if got := exact.String(); got != "1100" {
		t.Fatalf("1200 × 11/12 = %s, want exactly 1100", got)
	}

	// Demonstrate the failure mode being avoided.
	viaBasisPoints := amount.MulRate(11 * 10000 / 12)
	if viaBasisPoints == exact {
		t.Log("note: basis-point path happened to agree here")
	} else {
		t.Logf("basis-point path yields %s vs exact %s — the drift MulDiv removes",
			viaBasisPoints, exact)
		if viaBasisPoints > exact {
			t.Errorf("basis-point truncation should under-state, got %s > %s",
				viaBasisPoints, exact)
		}
	}
}

func TestMulDivRounding(t *testing.T) {
	cases := []struct {
		amount   string
		num, den int64
		want     string
	}{
		{"1200", 11, 12, "1100"},
		{"1200", 1, 12, "100"},
		{"1200", 1, 3, "400"},
		{"100", 1, 3, "33.333333"}, // repeating, rounded at 6dp
		{"100", 2, 3, "66.666667"}, // rounds up at the last place
		{"180", 1, 1, "180"},
		{"180", 0, 12, "0"},
		{"100", 1, 0, "0"}, // degenerate denominator returns zero, does not panic
	}
	for _, c := range cases {
		got := MustParseAmount(c.amount).MulDiv(c.num, c.den).String()
		if got != c.want {
			t.Errorf("%s × %d/%d = %s, want %s", c.amount, c.num, c.den, got, c.want)
		}
	}
}

// --- Charge types ---

func TestPhase1ChargeTypeGate(t *testing.T) {
	// 包年包月 + 按量 are Day-1; 资源包 + 抢占式 are model-reserved but unsold
	// until phase 2 (decision D6).
	if !ChargePrepaid.SellableInPhase1() || !ChargePostpaid.SellableInPhase1() {
		t.Fatal("prepaid and postpaid must be sellable in phase 1")
	}
	if ChargeResourcePack.SellableInPhase1() || ChargeSpot.SellableInPhase1() {
		t.Fatal("resource pack and spot must NOT be sellable in phase 1")
	}
}

func TestPhase2SellableGate(t *testing.T) {
	// Phase 2 (09-roadmap M-4) opens all four billing forms for sale: 资源包
	// (M-4.1) and 抢占式 (M-4.2, with the spot price engine + reclaim path).
	// With M-4.2 complete, ChargeSpot is sellable.
	for _, c := range []ChargeType{ChargePrepaid, ChargePostpaid, ChargeResourcePack, ChargeSpot} {
		if !c.Sellable() {
			t.Fatalf("%s must be sellable in phase 2", c)
		}
	}
}

func TestCalculateRejectsGatedChargeType(t *testing.T) {
	// With all four billing forms sellable in phase 2, the gate rejects only
	// an UNKNOWN charge type — one the catalogue never registered. The gate
	// fires before any rule lookup, so no pricing rule is needed.
	e := Engine{}
	_, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargeType("UNKNOWN"),
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), nil, nil)
	if !errors.Is(err, ErrChargeTypeUnsold) {
		t.Fatalf("unknown charge type must be rejected as unsold, got %v", err)
	}
}

// --- Rule selection ---

func basicRules() []PricingRule {
	return []PricingRule{
		{
			RuleID: 1, SKUCode: "s2.large", RegionID: "*",
			DurationUnit: DurationMonth, ListPrice: MustParseAmount("180"),
		},
		{
			RuleID: 2, SKUCode: "s2.large", RegionID: "cn-east-1",
			DurationUnit: DurationMonth, ListPrice: MustParseAmount("200"),
		},
		{
			RuleID: 3, SKUCode: "s2.large", RegionID: "*",
			DurationUnit: DurationMonth, ListPrice: MustParseAmount("150"),
			CustomerLevel: "ENTERPRISE",
		},
	}
}

func TestRegionalRuleBeatsWildcard(t *testing.T) {
	e := Engine{}
	res, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-east-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RuleID != 2 || res.ListAmount.String() != "200" {
		t.Fatalf("expected regional rule 2 at 200, got rule %d at %s", res.RuleID, res.ListAmount)
	}
}

func TestCustomerLevelRuleBeatsUnscoped(t *testing.T) {
	e := Engine{}
	res, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth,
		CustomerLevel: "ENTERPRISE", At: now,
	}, basicRules(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RuleID != 3 || res.ListAmount.String() != "150" {
		t.Fatalf("expected enterprise rule 3 at 150, got rule %d at %s", res.RuleID, res.ListAmount)
	}
}

func TestNoRuleIsAnError(t *testing.T) {
	// A missing price must fail loudly. Falling back to zero would give the
	// product away for free.
	e := Engine{}
	_, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "unknown-sku",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), nil, nil)
	if err != ErrNoPricingRule {
		t.Fatalf("expected ErrNoPricingRule, got %v", err)
	}
}

// TestExpiredRuleIgnored covers the append-only pricing model: superseded rows
// stay in the table with a closed effective range and must not be selected.
func TestExpiredRuleIgnored(t *testing.T) {
	rules := []PricingRule{
		{
			RuleID: 10, SKUCode: "s2.large", RegionID: "*",
			DurationUnit: DurationMonth, ListPrice: MustParseAmount("100"),
			EffectiveFrom: now.AddDate(-1, 0, 0),
			EffectiveTo:   now.AddDate(0, -1, 0), // ended last month
		},
		{
			RuleID: 11, SKUCode: "s2.large", RegionID: "*",
			DurationUnit: DurationMonth, ListPrice: MustParseAmount("180"),
			EffectiveFrom: now.AddDate(0, -1, 0),
		},
	}
	e := Engine{}
	res, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, rules, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RuleID != 11 {
		t.Fatalf("expected current rule 11, got %d (superseded rule leaked)", res.RuleID)
	}
}

// --- Quantity and duration ---

func TestQuantityAndDurationMultiply(t *testing.T) {
	e := Engine{}
	res, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 12, DurationUnit: DurationMonth, Quantity: 3, At: now,
	}, basicRules(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 180 × 3 instances × 12 months
	if got := res.ListAmount.String(); got != "6480" {
		t.Fatalf("list = %s, want 6480", got)
	}
}

func TestPrepaidRequiresDuration(t *testing.T) {
	e := Engine{}
	_, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 0, DurationUnit: DurationMonth, At: now,
	}, basicRules(), nil, nil)
	if err != ErrInvalidDuration {
		t.Fatalf("expected ErrInvalidDuration, got %v", err)
	}
}

func TestPostpaidNeedsNoDuration(t *testing.T) {
	rules := []PricingRule{{
		RuleID: 20, SKUCode: "s2.large", RegionID: "*",
		DurationUnit: DurationHour, ListPrice: MustParseAmount("0.25"),
	}}
	e := Engine{}
	res, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePostpaid,
		DurationUnit: DurationHour, At: now,
	}, rules, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.ListAmount.String(); got != "0.25" {
		t.Fatalf("postpaid hourly list = %s, want 0.25", got)
	}
}

// --- Promotions ---

func TestPromotionsDoNotStack(t *testing.T) {
	// Two applicable promotions: only the better one applies (01§12.3).
	promos := []Promotion{
		{PromoID: "p-9折", PromoType: PromoDiscountRate, ScopeType: "SKU",
			ScopeRef: "s2.large", RateBasisPoints: 9000},
		{PromoID: "p-7折", PromoType: PromoDiscountRate, ScopeType: "PRODUCT",
			ScopeRef: "scecs", RateBasisPoints: 7000},
	}
	e := Engine{}
	res, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), promos, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Best single discount is 7折 → 30% off 180 = 54, not 54+18 stacked.
	if res.AppliedPromoID != "p-7折" {
		t.Errorf("applied promo = %q, want p-7折", res.AppliedPromoID)
	}
	if got := res.PromoAmount.String(); got != "54" {
		t.Errorf("promo discount = %s, want 54 (not stacked)", got)
	}
	if got := res.PayableAmount.String(); got != "126" {
		t.Errorf("payable = %s, want 126", got)
	}
}

func TestPromotionUserTagGate(t *testing.T) {
	promos := []Promotion{{
		PromoID: "p-new-user", PromoType: PromoDiscountRate, ScopeType: "PRODUCT",
		ScopeRef: "scecs", RateBasisPoints: 3000, UserTag: "new",
	}}
	e := Engine{}
	base := Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}

	// Existing user: promotion does not apply.
	res, _ := e.Calculate(base, basicRules(), promos, nil)
	if res.AppliedPromoID != "" {
		t.Errorf("existing user should not get the new-user promo, got %q", res.AppliedPromoID)
	}

	// New user: promotion applies.
	newUser := base
	newUser.UserTag = "new"
	res2, _ := e.Calculate(newUser, basicRules(), promos, nil)
	if res2.AppliedPromoID != "p-new-user" {
		t.Errorf("new user should get the promo, got %q", res2.AppliedPromoID)
	}
	if got := res2.PayableAmount.String(); got != "54" {
		t.Errorf("new-user payable = %s, want 54 (3折)", got)
	}
}

func TestExpiredPromotionIgnored(t *testing.T) {
	promos := []Promotion{{
		PromoID: "p-expired", PromoType: PromoDiscountRate, ScopeType: "SKU",
		ScopeRef: "s2.large", RateBasisPoints: 5000,
		StartAt: now.AddDate(0, -2, 0), EndAt: now.AddDate(0, -1, 0),
	}}
	e := Engine{}
	res, _ := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), promos, nil)
	if res.AppliedPromoID != "" || !res.PromoAmount.IsZero() {
		t.Fatal("expired promotion must not apply")
	}
}

func TestFixedPricePromotionCannotRaisePrice(t *testing.T) {
	// A misconfigured 一口价 above list must not charge more than list.
	promos := []Promotion{{
		PromoID: "p-bad", PromoType: PromoFixedPrice, ScopeType: "SKU",
		ScopeRef: "s2.large", FixedPrice: MustParseAmount("500"),
	}}
	e := Engine{}
	res, _ := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), promos, nil)
	if got := res.PayableAmount.String(); got != "180" {
		t.Fatalf("payable = %s, want 180 (misconfigured promo must not raise price)", got)
	}
}

// --- Coupons (免费试用 carrier, decision D7) ---

func TestCouponsConsumedByExpiryAscending(t *testing.T) {
	coupons := []Coupon{
		{CouponID: "c-later", AccountID: 1, RemainValue: MustParseAmount("100"),
			ExpireAt: now.AddDate(0, 2, 0)},
		{CouponID: "c-sooner", AccountID: 1, RemainValue: MustParseAmount("50"),
			ExpireAt: now.AddDate(0, 1, 0)},
		{CouponID: "c-never", AccountID: 1, RemainValue: MustParseAmount("999")},
	}
	e := Engine{}
	res, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), nil, coupons)
	if err != nil {
		t.Fatal(err)
	}
	// 180 payable: c-sooner (50) then c-later (100) then c-never (30).
	if len(res.AppliedCoupons) != 3 {
		t.Fatalf("expected 3 coupons consumed, got %d: %+v", len(res.AppliedCoupons), res.AppliedCoupons)
	}
	if res.AppliedCoupons[0].CouponID != "c-sooner" {
		t.Errorf("first coupon = %q, want c-sooner (expiry ascending)", res.AppliedCoupons[0].CouponID)
	}
	if res.AppliedCoupons[1].CouponID != "c-later" {
		t.Errorf("second coupon = %q, want c-later", res.AppliedCoupons[1].CouponID)
	}
	if res.AppliedCoupons[2].CouponID != "c-never" {
		t.Errorf("third coupon = %q, want c-never (no expiry sorts last)", res.AppliedCoupons[2].CouponID)
	}
	if got := res.AppliedCoupons[2].Amount.String(); got != "30" {
		t.Errorf("last coupon took %s, want 30 (only the remainder)", got)
	}
	if !res.PayableAmount.IsZero() {
		t.Errorf("payable = %s, want 0 (fully covered by trial credit)", res.PayableAmount)
	}
}

func TestCouponScopeRestriction(t *testing.T) {
	coupons := []Coupon{
		{CouponID: "c-storage-only", AccountID: 1, RemainValue: MustParseAmount("100"),
			ProductCodes: []string{"scoss"}},
	}
	e := Engine{}
	res, _ := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), nil, coupons)
	if !res.CouponAmount.IsZero() {
		t.Fatal("a storage-scoped coupon must not offset a compute order")
	}
}

func TestExpiredCouponNotUsed(t *testing.T) {
	coupons := []Coupon{
		{CouponID: "c-expired", AccountID: 1, RemainValue: MustParseAmount("100"),
			ExpireAt: now.AddDate(0, 0, -1)},
	}
	e := Engine{}
	res, _ := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), nil, coupons)
	if !res.CouponAmount.IsZero() {
		t.Fatal("expired coupon must not be consumed")
	}
}

func TestCouponsOfOtherAccountsIgnored(t *testing.T) {
	// Tenant isolation reaches into pricing too: another account's coupon must
	// never offset this account's order.
	coupons := []Coupon{
		{CouponID: "c-other", AccountID: 999, RemainValue: MustParseAmount("100")},
	}
	e := Engine{}
	res, _ := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), nil, coupons)
	if !res.CouponAmount.IsZero() {
		t.Fatal("another account's coupon must not be usable")
	}
}

// TestQuotingDoesNotSpendCoupons guards the read-only contract of 询价: a user
// browsing prices must not drain their own trial credit.
func TestQuotingDoesNotSpendCoupons(t *testing.T) {
	coupons := []Coupon{
		{CouponID: "c1", AccountID: 1, RemainValue: MustParseAmount("100")},
	}
	before := coupons[0].RemainValue

	e := Engine{}
	for i := 0; i < 5; i++ {
		if _, err := e.Calculate(Request{
			AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
			RegionID: "cn-north-1", ChargeType: ChargePrepaid,
			Duration: 1, DurationUnit: DurationMonth, At: now,
		}, basicRules(), nil, coupons); err != nil {
			t.Fatal(err)
		}
	}
	if coupons[0].RemainValue != before {
		t.Fatalf("quoting mutated the coupon: %s → %s", before, coupons[0].RemainValue)
	}
}

// --- Full pipeline ---

func TestFullPipelineOrdering(t *testing.T) {
	// 目录价 → 促销 → 代金券, in that order. Applying the coupon before the
	// promotion would waste trial credit on a discount the user gets anyway.
	promos := []Promotion{{
		PromoID: "p-8折", PromoType: PromoDiscountRate, ScopeType: "SKU",
		ScopeRef: "s2.large", RateBasisPoints: 8000,
	}}
	coupons := []Coupon{
		{CouponID: "c-50", AccountID: 1, RemainValue: MustParseAmount("50")},
	}
	e := Engine{}
	res, err := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), promos, coupons)
	if err != nil {
		t.Fatal(err)
	}
	// list 180 → promo −36 (20%) → 144 → coupon −50 → payable 94
	if got := res.ListAmount.String(); got != "180" {
		t.Errorf("list = %s, want 180", got)
	}
	if got := res.PromoAmount.String(); got != "36" {
		t.Errorf("promo = %s, want 36", got)
	}
	if got := res.CouponAmount.String(); got != "50" {
		t.Errorf("coupon = %s, want 50", got)
	}
	if got := res.PayableAmount.String(); got != "94" {
		t.Errorf("payable = %s, want 94", got)
	}
	// The breakdown must reconcile exactly — this is the invariant the
	// bill-vs-metering reconciliation depends on (09 A2).
	sum := res.PromoAmount.Add(res.CouponAmount).Add(res.PayableAmount)
	if sum != res.ListAmount {
		t.Errorf("breakdown does not reconcile: promo+coupon+payable = %s, list = %s",
			sum, res.ListAmount)
	}
}

func TestPayableNeverNegative(t *testing.T) {
	// Coupons worth more than the order must not produce a negative payable
	// (which would amount to paying the customer).
	coupons := []Coupon{
		{CouponID: "c-huge", AccountID: 1, RemainValue: MustParseAmount("10000")},
	}
	e := Engine{}
	res, _ := e.Calculate(Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}, basicRules(), nil, coupons)
	if res.PayableAmount.IsNegative() {
		t.Fatalf("payable = %s, must never be negative", res.PayableAmount)
	}
	if got := res.CouponAmount.String(); got != "180" {
		t.Errorf("coupon consumed %s, want exactly 180 (not the full face value)", got)
	}
}

// --- Coupon kinds (phase-2 B7: 满减券 / 折扣券) ---

// baseReq is the standard 180-yuan quote every coupon-kind test builds on.
func baseReq() Request {
	return Request{
		AccountID: 1, ProductCode: "scecs", SKUCode: "s2.large",
		RegionID: "cn-north-1", ChargeType: ChargePrepaid,
		Duration: 1, DurationUnit: DurationMonth, At: now,
	}
}

func TestThresholdCouponTriggersAtThreshold(t *testing.T) {
	// 满 150 减 30 on a 180 order: threshold reached → deduction 30.
	coupons := []Coupon{{
		CouponID: "mj-150-30", AccountID: 1, Kind: KindThreshold,
		FaceValue: MustParseAmount("30"), Threshold: MustParseAmount("150"),
	}}
	e := Engine{}
	res, err := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.CouponAmount.String(); got != "30" {
		t.Errorf("coupon = %s, want 30", got)
	}
	if got := res.PayableAmount.String(); got != "150" {
		t.Errorf("payable = %s, want 150", got)
	}
	if len(res.AppliedCoupons) != 1 || res.AppliedCoupons[0].CouponID != "mj-150-30" {
		t.Errorf("applied = %+v, want the threshold coupon", res.AppliedCoupons)
	}
}

func TestThresholdCouponBelowThresholdInert(t *testing.T) {
	// 满 500 减 100 on a 180 order: threshold not reached → the coupon is
	// simply not applicable, and must not deduct anything.
	coupons := []Coupon{{
		CouponID: "mj-500-100", AccountID: 1, Kind: KindThreshold,
		FaceValue: MustParseAmount("100"), Threshold: MustParseAmount("500"),
	}}
	e := Engine{}
	res, _ := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if !res.CouponAmount.IsZero() {
		t.Fatalf("below-threshold coupon deducted %s", res.CouponAmount)
	}
	if len(res.AppliedCoupons) != 0 {
		t.Errorf("below-threshold coupon recorded as applied: %+v", res.AppliedCoupons)
	}
}

func TestThresholdCouponsDoNotStack(t *testing.T) {
	// Two threshold coupons that both trigger: only the best (higher face)
	// applies, mirroring the promotion no-stacking rule.
	coupons := []Coupon{
		{CouponID: "mj-a", AccountID: 1, Kind: KindThreshold,
			FaceValue: MustParseAmount("30"), Threshold: MustParseAmount("150")},
		{CouponID: "mj-b", AccountID: 1, Kind: KindThreshold,
			FaceValue: MustParseAmount("50"), Threshold: MustParseAmount("150")},
	}
	e := Engine{}
	res, _ := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if got := res.CouponAmount.String(); got != "50" {
		t.Errorf("coupon = %s, want 50 (best single threshold coupon)", got)
	}
	if len(res.AppliedCoupons) != 1 || res.AppliedCoupons[0].CouponID != "mj-b" {
		t.Errorf("applied = %+v, want only mj-b", res.AppliedCoupons)
	}
}

func TestRateCouponDiscount(t *testing.T) {
	// 85折 on 180: retained 153, discount 27.
	coupons := []Coupon{{
		CouponID: "zk-85", AccountID: 1, Kind: KindRate, RateBasisPoints: 8500,
	}}
	e := Engine{}
	res, err := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.CouponAmount.String(); got != "27" {
		t.Errorf("coupon = %s, want 27", got)
	}
	if got := res.PayableAmount.String(); got != "153" {
		t.Errorf("payable = %s, want 153", got)
	}
}

func TestRateCouponCapBoundsDiscount(t *testing.T) {
	// 85折 on 180 would discount 27; a cap of 20 clips it to 20. Without the
	// cap a percentage coupon on a large order is an unbounded liability.
	coupons := []Coupon{{
		CouponID: "zk-85-cap", AccountID: 1, Kind: KindRate,
		RateBasisPoints: 8500, CapAmount: MustParseAmount("20"),
	}}
	e := Engine{}
	res, _ := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if got := res.CouponAmount.String(); got != "20" {
		t.Errorf("coupon = %s, want 20 (capped)", got)
	}
	if got := res.PayableAmount.String(); got != "160" {
		t.Errorf("payable = %s, want 160", got)
	}
}

func TestRateCouponsDoNotStack(t *testing.T) {
	// 9折 (discount 18) vs 85折 (discount 27): only the better one applies.
	coupons := []Coupon{
		{CouponID: "zk-90", AccountID: 1, Kind: KindRate, RateBasisPoints: 9000},
		{CouponID: "zk-85", AccountID: 1, Kind: KindRate, RateBasisPoints: 8500},
	}
	e := Engine{}
	res, _ := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if got := res.CouponAmount.String(); got != "27" {
		t.Errorf("coupon = %s, want 27 (best single rate coupon)", got)
	}
	if len(res.AppliedCoupons) != 1 || res.AppliedCoupons[0].CouponID != "zk-85" {
		t.Errorf("applied = %+v, want only zk-85", res.AppliedCoupons)
	}
}

func TestMalformedCouponKindsInert(t *testing.T) {
	// Configuration errors in coupon rows must make the coupon unusable, not
	// fail the quote: a bad activity row may not take pricing down.
	coupons := []Coupon{
		{CouponID: "mj-face-above-threshold", AccountID: 1, Kind: KindThreshold,
			FaceValue: MustParseAmount("200"), Threshold: MustParseAmount("100")},
		{CouponID: "mj-zero-face", AccountID: 1, Kind: KindThreshold,
			FaceValue: 0, Threshold: MustParseAmount("100")},
		{CouponID: "zk-rate-10000", AccountID: 1, Kind: KindRate, RateBasisPoints: 10000},
		{CouponID: "zk-rate-0", AccountID: 1, Kind: KindRate, RateBasisPoints: 0},
		{CouponID: "unknown-kind", AccountID: 1, Kind: CouponKind("MYSTERY"),
			RemainValue: MustParseAmount("50")},
	}
	e := Engine{}
	res, err := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if err != nil {
		t.Fatal(err)
	}
	if !res.CouponAmount.IsZero() {
		t.Fatalf("malformed coupons deducted %s, want 0", res.CouponAmount)
	}
}

func TestCouponKindsComposeInFixedOrder(t *testing.T) {
	// The full ③ pipeline: 85折 → 满150减30 → voucher 50, in that fixed order.
	//
	// 180 → rate 85折: discount 27, running 153
	//     → threshold 满150减30: 153 ≥ 150 → discount 30, running 123
	//     → voucher −50: running 73
	// Total coupon = 27+30+50 = 107; payable 73.
	coupons := []Coupon{
		{CouponID: "c-voucher", AccountID: 1, RemainValue: MustParseAmount("50")},
		{CouponID: "mj-150-30", AccountID: 1, Kind: KindThreshold,
			FaceValue: MustParseAmount("30"), Threshold: MustParseAmount("150")},
		{CouponID: "zk-85", AccountID: 1, Kind: KindRate, RateBasisPoints: 8500},
	}
	e := Engine{}
	res, err := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.CouponAmount.String(); got != "107" {
		t.Errorf("coupon total = %s, want 107", got)
	}
	if got := res.PayableAmount.String(); got != "73" {
		t.Errorf("payable = %s, want 73", got)
	}
	// Application order is the fixed sub-order, regardless of input order.
	if len(res.AppliedCoupons) != 3 {
		t.Fatalf("applied %d coupons, want 3: %+v", len(res.AppliedCoupons), res.AppliedCoupons)
	}
	wantOrder := []string{"zk-85", "mj-150-30", "c-voucher"}
	for i, want := range wantOrder {
		if res.AppliedCoupons[i].CouponID != want {
			t.Errorf("applied[%d] = %q, want %q (fixed sub-order rate→threshold→voucher)", i, res.AppliedCoupons[i].CouponID, want)
		}
	}
	// The threshold fired at 153 (post-rate), not at 180 (post-promo): the
	// sub-order is observable through the recorded amounts.
	if got := res.AppliedCoupons[1].Amount.String(); got != "30" {
		t.Errorf("threshold coupon took %s, want 30", got)
	}
	// Breakdown reconciles exactly (09 A2).
	sum := res.PromoAmount.Add(res.CouponAmount).Add(res.PayableAmount)
	if sum != res.ListAmount {
		t.Errorf("breakdown does not reconcile: %s != %s", sum, res.ListAmount)
	}
}

func TestThresholdMeasuredAfterRateDiscount(t *testing.T) {
	// 95折 first (discount 9, running 171), then 满 175 减 30: the threshold is
	// measured on the POST-rate amount 171 < 175 → the threshold coupon does
	// NOT fire. Measuring thresholds on the pre-rate amount (180 ≥ 175) would
	// be a different, less defensible rule — this test pins the chosen one.
	coupons := []Coupon{
		{CouponID: "zk-95", AccountID: 1, Kind: KindRate, RateBasisPoints: 9500},
		{CouponID: "mj-175-30", AccountID: 1, Kind: KindThreshold,
			FaceValue: MustParseAmount("30"), Threshold: MustParseAmount("175")},
	}
	e := Engine{}
	res, _ := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if got := res.CouponAmount.String(); got != "9" {
		t.Errorf("coupon = %s, want 9 (rate only; threshold must not fire)", got)
	}
	if len(res.AppliedCoupons) != 1 {
		t.Errorf("applied = %+v, want only the rate coupon", res.AppliedCoupons)
	}
}

func TestLegacyCouponsDefaultToVoucher(t *testing.T) {
	// Phase-1 rows have no Kind set; they must keep working as vouchers
	// without a data migration (usable()/applyCoupons default "" → VOUCHER).
	coupons := []Coupon{{
		CouponID: "c-legacy", AccountID: 1, RemainValue: MustParseAmount("60"),
	}}
	e := Engine{}
	res, err := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.CouponAmount.String(); got != "60" {
		t.Errorf("legacy coupon deducted %s, want 60", got)
	}
	if got := res.PayableAmount.String(); got != "120" {
		t.Errorf("payable = %s, want 120", got)
	}
}

func TestThresholdCouponDeductionCappedAtRemainder(t *testing.T) {
	// threshold fires before vouchers: 180 ≥ 满25 → −20 (running 160), then the
	// voucher −150 → payable 10. The deduction can never exceed the running
	// amount: face ≤ threshold is enforced at usable(), and the threshold check
	// guarantees running ≥ threshold ≥ face at fire time — so payable ≥ 0 by
	// construction, not by clamping.
	coupons := []Coupon{
		{CouponID: "c-150", AccountID: 1, RemainValue: MustParseAmount("150")},
		{CouponID: "mj-25-20", AccountID: 1, Kind: KindThreshold,
			FaceValue: MustParseAmount("20"), Threshold: MustParseAmount("25")},
	}
	e := Engine{}
	res, _ := e.Calculate(baseReq(), basicRules(), nil, coupons)
	if got := res.CouponAmount.String(); got != "170" {
		t.Errorf("coupon total = %s, want 170 (20 threshold + 150 voucher)", got)
	}
	if got := res.PayableAmount.String(); got != "10" {
		t.Errorf("payable = %s, want 10", got)
	}
	if res.PayableAmount.IsNegative() {
		t.Fatalf("payable = %s, must never be negative", res.PayableAmount)
	}
}
