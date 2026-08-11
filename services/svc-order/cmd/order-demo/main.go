// Command order-demo walks the phase-1 commercial loop end to end:
//
//	询价 (svc-catalog) → 下单 → 支付 → 履约 → 完成 → 退订 → 释放 → 退款
//
// It exercises pkg-go/pricing and pkg-go/order together, including the outbox
// events each transition emits, so the two packages are shown to compose
// rather than merely to pass their own tests.
//
// The scenarios deliberately include the failure paths that cost money when
// they are wrong: duplicate payment callbacks, refunds attempted before the
// resource is released, and provisioning triggered from the wrong state.
//
// Run: go run ./cmd/order-demo
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/starcloud/sc-platform/order"
	"github.com/starcloud/sc-platform/pricing"
)

var (
	at      = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	failures int
)

func main() {
	fmt.Println("Phase-1 commercial loop: 询价 → 下单 → 支付 → 履约 → 退订 → 退款")
	fmt.Println()

	scenarioHappyPath()
	scenarioDuplicateCallback()
	scenarioRefundOrdering()
	scenarioFailedFulfilment()
	scenarioProration()

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d 项检查失败\n", failures)
		os.Exit(1)
	}
	fmt.Println("全部通过 — 商业闭环各环节衔接正确")
}

func check(name string, ok bool, detail string) {
	if ok {
		fmt.Printf("    PASS  %-46s %s\n", name, detail)
		return
	}
	failures++
	fmt.Printf("    FAIL  %-46s %s\n", name, detail)
}

// emit prints an outbox event the way the relay would publish it.
func emit(evt order.Event) {
	arrow := ""
	if evt.FromState != "" {
		arrow = string(evt.FromState) + " → "
	}
	tail := ""
	if evt.TriggersProvisioning() {
		tail = "   ← 触发资源开通"
	}
	fmt.Printf("      outbox → %-34s %s%s%s\n",
		evt.EventType(), arrow, evt.ToState, tail)
}

// quote runs the catalogue pricing for a standard SCECS monthly order.
func quote(userTag string, coupons []pricing.Coupon) pricing.Result {
	rules := []pricing.PricingRule{{
		RuleID: 2, SKUCode: "scecs.s2.large.prepaid", RegionID: "*",
		DurationUnit: pricing.DurationMonth,
		ListPrice:    pricing.MustParseAmount("180"),
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	promos := []pricing.Promotion{{
		PromoID: "promo-ecs-annual", PromoType: pricing.PromoDiscountRate,
		ScopeType: "PRODUCT", ScopeRef: "scecs", RateBasisPoints: 8500,
		StartAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EndAt:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	res, err := pricing.Engine{}.Calculate(pricing.Request{
		AccountID: 100123, ProductCode: "scecs", SKUCode: "scecs.s2.large.prepaid",
		RegionID: "cn-north-1", ChargeType: pricing.ChargePrepaid,
		Duration: 12, DurationUnit: pricing.DurationMonth, Quantity: 1,
		UserTag: userTag, At: at,
	}, rules, promos, coupons)
	if err != nil {
		fmt.Println("quote error:", err)
		os.Exit(1)
	}
	return res
}

func newOrderReq(q pricing.Result, token string) order.CreateRequest {
	return order.CreateRequest{
		AccountID: 100123, Type: order.TypeNew,
		ProductCode: "scecs", ChargeType: pricing.ChargePrepaid,
		SKUCode: "scecs.s2.large.prepaid", RegionID: "cn-north-1",
		Quantity: 1, Duration: 12, DurationUnit: pricing.DurationMonth,
		Quote: q, SnapshotID: "snap-" + token,
		SnapshotExpiresAt: at.Add(15 * time.Minute),
		ClientToken:       token, At: at,
	}
}

// --- 1. Happy path -----------------------------------------------------------

func scenarioHappyPath() {
	fmt.Println("  [1] 新购包年 SCECS 2C4G,全流程")
	m := order.NewMachine(func() time.Time { return at })

	q := quote("", nil)
	fmt.Printf("      询价: 目录 %s  促销 -%s  应付 %s CNY\n",
		q.ListAmount, q.PromoAmount, q.PayableAmount)
	check("询价金额", q.PayableAmount.String() == "1836", "180×12 = 2160, 85折 → 1836")

	o, createEvt, err := m.Create(newOrderReq(q, "tok-happy-001"), 9001, "SO202608080001")
	if err != nil {
		check("下单", false, err.Error())
		return
	}
	emit(createEvt)
	check("订单初始状态", o.State == order.StatePendingPayment, string(o.State))

	paidEvt, err := m.MarkPaid(o, q.PayableAmount, 0)
	if err != nil {
		check("支付", false, err.Error())
		return
	}
	emit(paidEvt)

	fulfilEvt, err := m.StartFulfilment(o, 1)
	if err != nil {
		check("开始履约", false, err.Error())
		return
	}
	emit(fulfilEvt)
	check("唯一开通触发点", fulfilEvt.TriggersProvisioning(), "paid → fulfilling")

	resourceID := "scecs-cn-north-1-01-a1b2c3d4"
	doneEvt, err := m.CompleteFulfilment(o, resourceID, 2)
	if err != nil {
		check("履约完成", false, err.Error())
		return
	}
	emit(doneEvt)
	check("终态与资源号", o.State == order.StateCompleted && o.ResourceID == resourceID,
		string(o.State)+" "+o.ResourceID)
	fmt.Println()
}

// --- 2. Duplicate payment callback -------------------------------------------

func scenarioDuplicateCallback() {
	fmt.Println("  [2] 支付渠道重复回调(幂等)")
	m := order.NewMachine(func() time.Time { return at })

	q := quote("", nil)
	o, _, _ := m.Create(newOrderReq(q, "tok-dup-002"), 9002, "SO202608080002")

	if _, err := m.MarkPaid(o, q.PayableAmount, 0); err != nil {
		check("首次回调", false, err.Error())
		return
	}
	check("首次回调", o.State == order.StatePaid && o.Version == 1,
		fmt.Sprintf("%s v%d", o.State, o.Version))

	// Channel retries three times with the same (stale) version.
	for i := 0; i < 3; i++ {
		_, _ = m.MarkPaid(o, q.PayableAmount, 0)
	}
	check("重复回调无副作用", o.State == order.StatePaid && o.Version == 1,
		fmt.Sprintf("仍为 %s v%d", o.State, o.Version))

	// A channel reporting a different amount is a reconciliation problem.
	_, err := m.MarkPaid(o, pricing.MustParseAmount("1000"), 1)
	check("金额不符被拒", errors.Is(err, order.ErrAmountMismatch), "渠道金额与订单不符不得静默接受")
	fmt.Println()
}

// --- 3. Refund ordering ------------------------------------------------------

func scenarioRefundOrdering() {
	fmt.Println("  [3] 退订:资源释放先于退款(钱货两清有先后)")
	m := order.NewMachine(func() time.Time { return at })

	q := quote("", nil)
	o, _, _ := m.Create(newOrderReq(q, "tok-refund-003"), 9003, "SO202608080003")
	_, _ = m.MarkPaid(o, q.PayableAmount, 0)
	_, _ = m.StartFulfilment(o, 1)
	_, _ = m.CompleteFulfilment(o, "scecs-cn-north-1-01-b2c3d4e5", 2)

	// Refund attempted while the resource is still running.
	_, err := m.StartRefund(o, false, 3)
	check("资源未释放时拒绝退款", errors.Is(err, order.ErrResourceNotReleased),
		"否则用户同时持有资金与资源")
	check("被拒后状态不变", o.State == order.StateCompleted, string(o.State))

	// Orchestrator confirms release, then the refund proceeds.
	refundEvt, err := m.StartRefund(o, true, 3)
	if err != nil {
		check("释放后退款", false, err.Error())
		return
	}
	emit(refundEvt)

	doneEvt, err := m.CompleteRefund(o, 4)
	if err != nil {
		check("退款完成", false, err.Error())
		return
	}
	emit(doneEvt)
	check("退款终态", o.State == order.StateRefunded, string(o.State))
	fmt.Println()
}

// --- 4. Failed fulfilment ----------------------------------------------------

func scenarioFailedFulfilment() {
	fmt.Println("  [4] 履约失败:已付款订单走退款而非取消")
	m := order.NewMachine(func() time.Time { return at })

	q := quote("", nil)
	o, _, _ := m.Create(newOrderReq(q, "tok-fail-004"), 9004, "SO202608080004")
	_, _ = m.MarkPaid(o, q.PayableAmount, 0)
	_, _ = m.StartFulfilment(o, 1)

	_, err := m.Transition(o, order.StateCancelled, 2)
	check("已付款订单不可取消", errors.Is(err, order.ErrInvalidTransition),
		"钱已收,必须退回而非让订单消失")

	// Nothing was provisioned, so no release is required.
	evt, err := m.StartRefund(o, false, 2)
	if err != nil {
		check("失败后转退款", false, err.Error())
		return
	}
	emit(evt)
	check("失败后转退款", o.State == order.StateRefunding, string(o.State))
	fmt.Println()
}

// --- 5. Proration ------------------------------------------------------------

func scenarioProration() {
	fmt.Println("  [5] 退订折算:按剩余时长,基准取自价格快照")

	cases := []struct {
		name              string
		amount            string
		total, used       int64
		want              string
	}{
		{"包年用满1月退11月", "2160", 12, 1, "1980"},
		{"包年用满6月退6月", "2160", 12, 6, "1080"},
		{"包年用满12月无退款", "2160", 12, 12, "0"},
		{"超期使用不倒扣", "2160", 12, 15, "0"},
	}
	for _, c := range cases {
		b := order.ProrationBasis{
			SnapshotAmount: pricing.MustParseAmount(c.amount),
			TotalPeriods:   c.total,
			UsedPeriods:    c.used,
		}
		got := b.RefundAmount()
		check(c.name, got.String() == c.want,
			fmt.Sprintf("退款 %s CNY", got))
		if got.IsNegative() {
			check(c.name+" 非负", false, "退款绝不为负")
		}
	}
	fmt.Println()
	fmt.Println("      注:折算用 MulDiv 而非基点转换 —— 11/12 折算成 9166bp 会")
	fmt.Println("          少退 8 分,误差恒定偏向平台一侧,笔笔累积。")
}
