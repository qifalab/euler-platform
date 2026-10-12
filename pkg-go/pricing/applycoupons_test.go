package pricing

import (
	"testing"
	"time"
)

// ApplyCoupons is Calculate's ③ stage exported for the order plane, which
// prices the coupons a customer selects at checkout on top of the catalogue's
// quote. It must agree with Calculate exactly (same coupons, same outstanding →
// same breakdown) and, with a quantum, express every deduction in that unit.

func TestApplyCouponsMatchesCalculate(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	req := Request{AccountID: 7, ProductCode: "euecs", SKUCode: "euecs.c1", RegionID: "cn-north-1",
		ChargeType: ChargePrepaid, Duration: 1, DurationUnit: DurationMonth, Quantity: 1, At: at}
	rules := []PricingRule{{RuleID: 1, SKUCode: "euecs.c1", RegionID: "*", DurationUnit: DurationMonth, ListPrice: MustParseAmount("500")}}
	coupons := []Coupon{
		{CouponID: "v2", AccountID: 7, Kind: KindVoucher, FaceValue: MustParseAmount("50"), RemainValue: MustParseAmount("50"), ExpireAt: at.Add(48 * time.Hour)},
		{CouponID: "v1", AccountID: 7, Kind: KindVoucher, FaceValue: MustParseAmount("30"), RemainValue: MustParseAmount("30"), ExpireAt: at.Add(24 * time.Hour)},
		{CouponID: "r1", AccountID: 7, Kind: KindRate, RateBasisPoints: 9000, CapAmount: MustParseAmount("100"), ExpireAt: at.Add(time.Hour)},
		{CouponID: "t1", AccountID: 7, Kind: KindThreshold, Threshold: MustParseAmount("400"), FaceValue: MustParseAmount("40"), ExpireAt: at.Add(time.Hour)},
		{CouponID: "foreign", AccountID: 8, Kind: KindVoucher, FaceValue: MustParseAmount("99"), RemainValue: MustParseAmount("99"), ExpireAt: at.Add(time.Hour)},
	}
	res, err := Engine{}.Calculate(req, rules, nil, coupons)
	if err != nil {
		t.Fatal(err)
	}
	uses, total := ApplyCoupons(coupons, req, res.ListAmount, 0)
	if total != res.CouponAmount || len(uses) != len(res.AppliedCoupons) {
		t.Fatalf("ApplyCoupons = %v / %s, Calculate = %v / %s", uses, total, res.AppliedCoupons, res.CouponAmount)
	}
	for i := range uses {
		if uses[i] != res.AppliedCoupons[i] {
			t.Fatalf("use %d = %+v, Calculate %+v", i, uses[i], res.AppliedCoupons[i])
		}
	}
	// ③a 50 off 500, ③b 40 (450 ≥ 400), ③c vouchers by expiry: v1 then v2.
	want := []CouponUse{{"r1", MustParseAmount("50")}, {"t1", MustParseAmount("40")}, {"v1", MustParseAmount("30")}, {"v2", MustParseAmount("50")}}
	for i, w := range want {
		if uses[i] != w {
			t.Fatalf("use %d = %+v, want %+v (all %v)", i, uses[i], w, uses)
		}
	}
}

func TestApplyCouponsQuantumRoundsRateShare(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	req := Request{AccountID: 7, ProductCode: "euecs", ChargeType: ChargePrepaid, At: at}
	rate := Coupon{CouponID: "r", AccountID: 7, Kind: KindRate, RateBasisPoints: 8888, ExpireAt: at.Add(time.Hour)}
	// 12.34 × 11.12% = 1.372208 → 1.37 at 分 granularity.
	uses, total := ApplyCoupons([]Coupon{rate}, req, MustParseAmount("12.34"), 10_000)
	if total != MustParseAmount("1.37") || len(uses) != 1 || uses[0].Amount != MustParseAmount("1.37") {
		t.Fatalf("rate share = %v / %s, want 1.37", uses, total)
	}
	// Without a quantum the share keeps its micro precision (Calculate).
	if _, raw := ApplyCoupons([]Coupon{rate}, req, MustParseAmount("12.34"), 0); raw != MustParseAmount("1.372208") {
		t.Fatalf("unrounded share = %s", raw)
	}
	// A cap that is not cent-aligned is honoured by rounding it down.
	capped := rate
	capped.CapAmount = MustParseAmount("1.005")
	if _, got := ApplyCoupons([]Coupon{capped}, req, MustParseAmount("12.34"), 10_000); got != MustParseAmount("1") {
		t.Fatalf("capped share = %s, want 1 (cap 1.005 rounded down)", got)
	}
}

func TestCouponSKUScope(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := Coupon{CouponID: "v", AccountID: 7, Kind: KindVoucher, FaceValue: MustParseAmount("10"),
		RemainValue: MustParseAmount("10"), SKUCodes: []string{"euecs.c1"}, ExpireAt: at.Add(time.Hour)}
	if !c.Usable(at, Request{AccountID: 7, SKUCode: "euecs.c1"}) {
		t.Fatal("in-scope SKU must be usable")
	}
	if c.Usable(at, Request{AccountID: 7, SKUCode: "euecs.c2"}) {
		t.Fatal("out-of-scope SKU must not be usable")
	}
	if _, total := ApplyCoupons([]Coupon{c}, Request{AccountID: 7, SKUCode: "euecs.c2", At: at}, MustParseAmount("100"), 10_000); total != 0 {
		t.Fatalf("out-of-scope voucher deducted %s", total)
	}
	if c.EffectiveKind() != KindVoucher || (Coupon{}).EffectiveKind() != KindVoucher {
		t.Fatal("empty kind must resolve to VOUCHER")
	}
}
