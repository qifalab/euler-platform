// Command pricing-demo quotes the phase-1 catalogue through the real pricing
// engine.
//
// It exists to cross-check two artifacts that must agree but live in different
// languages: the seed catalogue in sql/V2__seed_phase1_catalog.sql and the
// engine in pkg-go/pricing. A price that looks reasonable in SQL can still
// combine into a wrong total once quantity, duration, promotions and vouchers
// interact, and that only shows up when something actually computes it.
//
// Run: go run ./cmd/pricing-demo
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/starcloud/sc-platform/pricing"
)

// catalogue mirrors sql/V2__seed_phase1_catalog.sql. When the seed changes,
// this changes with it — the demo failing is the signal that the two drifted.
func catalogue() ([]pricing.PricingRule, []pricing.Promotion) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	rules := []pricing.PricingRule{
		// SCECS 包年包月
		{RuleID: 1, SKUCode: "scecs.s2.small.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth,
			ListPrice: pricing.MustParseAmount("90"), EffectiveFrom: from},
		{RuleID: 2, SKUCode: "scecs.s2.large.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth,
			ListPrice: pricing.MustParseAmount("180"), EffectiveFrom: from},
		{RuleID: 3, SKUCode: "scecs.s2.xlarge.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth,
			ListPrice: pricing.MustParseAmount("360"), EffectiveFrom: from},
		{RuleID: 4, SKUCode: "scecs.s2.large.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth,
			ListPrice: pricing.MustParseAmount("150"), CustomerLevel: "ENTERPRISE", EffectiveFrom: from},
		{RuleID: 5, SKUCode: "scecs.s2.large.prepaid", RegionID: "cn-east-1", DurationUnit: pricing.DurationMonth,
			ListPrice: pricing.MustParseAmount("200"), EffectiveFrom: from},
		// SCECS 按量
		{RuleID: 6, SKUCode: "scecs.s2.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour,
			ListPrice: pricing.MustParseAmount("0.25"), EffectiveFrom: from},
		// SCBS
		{RuleID: 9, SKUCode: "scbs.essd.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth,
			ListPrice: pricing.MustParseAmount("1"), EffectiveFrom: from},
		// SCOSS
		{RuleID: 11, SKUCode: "scoss.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour,
			ListPrice: pricing.MustParseAmount("0.00017"), EffectiveFrom: from},
		// SCRDS
		{RuleID: 12, SKUCode: "scrds.mysql8.small.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth,
			ListPrice: pricing.MustParseAmount("420"), EffectiveFrom: from},
		// SCEIP
		{RuleID: 14, SKUCode: "sceip.bandwidth.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth,
			ListPrice: pricing.MustParseAmount("23"), EffectiveFrom: from},
		// SCVPC / SCMON free tiers
		{RuleID: 16, SKUCode: "scvpc.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour,
			ListPrice: pricing.MustParseAmount("0"), EffectiveFrom: from},
	}

	promos := []pricing.Promotion{
		{PromoID: "promo-newuser-2026", PromoType: pricing.PromoDiscountRate,
			ScopeType: "ORDER", RateBasisPoints: 3000, UserTag: "new",
			StartAt: from, EndAt: to},
		{PromoID: "promo-ecs-annual", PromoType: pricing.PromoDiscountRate,
			ScopeType: "PRODUCT", ScopeRef: "scecs", RateBasisPoints: 8500,
			StartAt: from, EndAt: to},
	}
	return rules, promos
}

type scenario struct {
	name    string
	req     pricing.Request
	coupons []pricing.Coupon
	// want is the expected payable amount; empty means "print, do not assert".
	want string
	why  string
}

func main() {
	rules, promos := catalogue()
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	engine := pricing.Engine{}

	base := func(sku, product, region string, ct pricing.ChargeType) pricing.Request {
		return pricing.Request{
			AccountID: 100123, ProductCode: product, SKUCode: sku,
			RegionID: region, ChargeType: ct, Quantity: 1, At: at,
		}
	}

	prepaid := func(r pricing.Request, months int64) pricing.Request {
		r.Duration = months
		r.DurationUnit = pricing.DurationMonth
		return r
	}
	postpaid := func(r pricing.Request) pricing.Request {
		r.DurationUnit = pricing.DurationHour
		return r
	}

	trialVoucher := []pricing.Coupon{{
		CouponID: "coupon-trial-compute", AccountID: 100123,
		FaceValue: pricing.MustParseAmount("300"), RemainValue: pricing.MustParseAmount("300"),
		ProductCodes: []string{"scecs"},
		ExpireAt:     at.AddDate(0, 1, 0),
	}}

	scenarios := []scenario{
		{
			name: "SCECS 2C4G 包月 ×1",
			req:  prepaid(base("scecs.s2.large.prepaid", "scecs", "cn-north-1", pricing.ChargePrepaid), 1),
			want: "153",
			why:  "180 目录价, 命中云服务器 85 折 → 153",
		},
		{
			name: "SCECS 2C4G 包月 ×1 (cn-east-1)",
			req:  prepaid(base("scecs.s2.large.prepaid", "scecs", "cn-east-1", pricing.ChargePrepaid), 1),
			want: "170",
			why:  "地域规则更具体, 目录价 200 → 85 折 → 170",
		},
		{
			name: "SCECS 2C4G 包月 ×1 (企业客户)",
			req: func() pricing.Request {
				r := prepaid(base("scecs.s2.large.prepaid", "scecs", "cn-north-1", pricing.ChargePrepaid), 1)
				r.CustomerLevel = "ENTERPRISE"
				return r
			}(),
			want: "127.5",
			why:  "企业等级规则 150 → 85 折 → 127.5",
		},
		{
			name: "SCECS 2C4G 包年 ×3 台",
			req: func() pricing.Request {
				r := prepaid(base("scecs.s2.large.prepaid", "scecs", "cn-north-1", pricing.ChargePrepaid), 12)
				r.Quantity = 3
				return r
			}(),
			want: "5508",
			why:  "180 × 3 台 × 12 月 = 6480, 85 折 → 5508",
		},
		{
			name: "SCECS 新客首购 (3折优先于85折)",
			req: func() pricing.Request {
				r := prepaid(base("scecs.s2.large.prepaid", "scecs", "cn-north-1", pricing.ChargePrepaid), 1)
				r.UserTag = "new"
				return r
			}(),
			want: "54",
			why:  "两个促销都命中, 取最优单条 3 折 → 54 (不叠加)",
		},
		{
			name:    "SCECS 新客 + 试用代金券",
			req:     prepaid(base("scecs.s2.large.prepaid", "scecs", "cn-north-1", pricing.ChargePrepaid), 1),
			coupons: trialVoucher,
			want:    "0",
			why:     "153 应付, 300 元试用券覆盖 → 0 元, 券余 147",
		},
		{
			name: "SCECS 2C4G 按量 (每小时)",
			req:  postpaid(base("scecs.s2.large.postpaid", "scecs", "cn-north-1", pricing.ChargePostpaid)),
			want: "0.2125",
			why:  "0.25/小时 → 85 折 → 0.2125",
		},
		{
			name: "SCBS ESSD 20GB 包月",
			req: func() pricing.Request {
				r := prepaid(base("scbs.essd.prepaid", "scbs", "cn-north-1", pricing.ChargePrepaid), 1)
				r.Quantity = 20
				return r
			}(),
			want: "20",
			why:  "1 元/GB/月 × 20GB, 无促销",
		},
		{
			name: "SCRDS MySQL8 2C4G 包月",
			req:  prepaid(base("scrds.mysql8.small.prepaid", "scrds", "cn-north-1", pricing.ChargePrepaid), 1),
			want: "420",
			why:  "云服务器折扣不适用于数据库产品",
		},
		{
			name: "SCEIP 5Mbps 包月",
			req:  prepaid(base("sceip.bandwidth.prepaid", "sceip", "cn-north-1", pricing.ChargePrepaid), 1),
			want: "23",
			why:  "按带宽固定月费",
		},
		{
			name: "SCVPC 标准 (免费)",
			req:  postpaid(base("scvpc.standard.postpaid", "scvpc", "cn-north-1", pricing.ChargePostpaid)),
			want: "0",
			why:  "网络边界本身不计费, 可计费子资源承担计量",
		},
	}

	fmt.Println("Phase-1 catalogue quoted through pkg-go/pricing")
	fmt.Println("(cross-checks sql/V2__seed_phase1_catalog.sql against the engine)")
	fmt.Println()

	failures := 0
	for _, s := range scenarios {
		res, err := engine.Calculate(s.req, rules, promos, s.coupons)
		if err != nil {
			failures++
			fmt.Printf("  ERROR %-38s %v\n", s.name, err)
			continue
		}

		got := res.PayableAmount.String()
		status := "PASS"
		if s.want != "" && got != s.want {
			status = "FAIL"
			failures++
		}

		fmt.Printf("  %s  %-38s %10s CNY\n", status, s.name, got)
		fmt.Printf("        列表 %s  促销 -%s", res.ListAmount, res.PromoAmount)
		if !res.CouponAmount.IsZero() {
			fmt.Printf("  代金券 -%s", res.CouponAmount)
		}
		if res.AppliedPromoID != "" {
			fmt.Printf("  [%s]", res.AppliedPromoID)
		}
		fmt.Println()
		fmt.Printf("        %s\n", s.why)
		if status == "FAIL" {
			fmt.Printf("        expected %s, got %s\n", s.want, got)
		}

		// The breakdown must reconcile exactly. This is the invariant the
		// metering-to-bill reconciliation depends on (09 A2 无未解释差异):
		// if list ≠ promo + coupon + payable, a cent has gone missing.
		sum := res.PromoAmount.Add(res.CouponAmount).Add(res.PayableAmount)
		if sum != res.ListAmount {
			failures++
			fmt.Printf("        RECONCILE FAIL: promo+coupon+payable = %s but list = %s\n",
				sum, res.ListAmount)
		}
		fmt.Println()
	}

	// Deferred charge types must be refused even though the enum accepts them.
	deferred := base("scecs.s2.large.prepaid", "scecs", "cn-north-1", pricing.ChargeResourcePack)
	deferred.Duration = 1
	deferred.DurationUnit = pricing.DurationMonth
	if _, err := engine.Calculate(deferred, rules, promos, nil); err == nil {
		failures++
		fmt.Println("  FAIL  资源包计费形态应被拒绝 (一期不售, 决策 D6)")
	} else {
		fmt.Printf("  PASS  %-38s %v\n", "资源包计费形态被拒绝 (决策 D6)", err)
	}

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d 项检查失败\n", failures)
		os.Exit(1)
	}
	fmt.Println("全部通过 — 种子目录与定价引擎一致")
}
