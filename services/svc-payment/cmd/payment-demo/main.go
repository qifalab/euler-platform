// Command payment-demo walks the money side of the commercial loop:
//
//	充值 → 冻结 → 订单支付(抵扣顺序) → 履约 → 退订 → 解冻/退款 → 对账
//
// It composes pkg-go/{pricing,order,ledger} to show that the balance, the
// order state machine, and the pricing breakdown agree at every step, and that
// the journal reproduces the balance at the end.
//
// The scenarios include the failure paths that cost money when handled wrongly:
// insufficient balance, repeated channel callbacks, and a refund arriving
// before the resource is released.
//
// Run: go run ./cmd/payment-demo
package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/starcloud/sc-platform/ledger"
	"github.com/starcloud/sc-platform/order"
	"github.com/starcloud/sc-platform/pricing"
)

var (
	at       = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	failures int
)

const acct = int64(100123)

func main() {
	fmt.Println("Phase-1 资金链路: 充值 → 冻结 → 支付 → 履约 → 退订 → 退款 → 对账")
	fmt.Println()

	scenarioRechargeAndPay()
	scenarioDeductionOrder()
	scenarioInsufficientBalance()
	scenarioChannelCallbackIdempotency()
	scenarioRefundPath()
	scenarioReconciliation()

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d 项检查失败\n", failures)
		os.Exit(1)
	}
	fmt.Println("全部通过 — 资金链路各环节衔接正确,账实相符")
}

func check(name string, ok bool, detail string) {
	if ok {
		fmt.Printf("    PASS  %-44s %s\n", name, detail)
		return
	}
	failures++
	fmt.Printf("    FAIL  %-44s %s\n", name, detail)
}

func amt(s string) pricing.Amount { return pricing.MustParseAmount(s) }

// --- in-memory ledger store (production: Vitess (vtgate) on trade_db) --------

type memStore struct {
	mu       sync.Mutex
	balances map[int64]ledger.Balance
	entries  map[int64][]ledger.Entry
	byKey    map[string]ledger.Entry
}

func newStore() *memStore {
	return &memStore{
		balances: make(map[int64]ledger.Balance),
		entries:  make(map[int64][]ledger.Entry),
		byKey:    make(map[string]ledger.Entry),
	}
}

func (m *memStore) GetBalance(id int64) (ledger.Balance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.balances[id]
	if !ok {
		return ledger.Balance{AccountID: id}, nil
	}
	return b, nil
}

func (m *memStore) Apply(e ledger.Entry, nb ledger.Balance, expectedVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.balances[e.AccountID]
	if cur.Version != expectedVersion {
		return ledger.ErrVersionConflict
	}
	m.balances[e.AccountID] = nb
	m.entries[e.AccountID] = append(m.entries[e.AccountID], e)
	m.byKey[fmt.Sprintf("%d|%s", e.AccountID, e.IdempotencyKey)] = e
	return nil
}

func (m *memStore) FindByIdempotencyKey(id int64, key string) (ledger.Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byKey[fmt.Sprintf("%d|%s", id, key)]
	return e, ok, nil
}

func (m *memStore) ListEntries(id int64) ([]ledger.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ledger.Entry, len(m.entries[id]))
	copy(out, m.entries[id])
	return out, nil
}

func newLedger() (*ledger.Ledger, *memStore) {
	store := newStore()
	var seq int64
	return ledger.New(store, func() time.Time { return at }, func() int64 {
		seq++
		return seq
	}), store
}

// --- 1. Recharge then pay ---------------------------------------------------

func scenarioRechargeAndPay() {
	fmt.Println("  [1] 充值后余额支付订单")
	l, _ := newLedger()

	_, bal, err := l.Recharge(acct, amt("2000"), "recharge-001", "idem-rc-1", "模拟渠道充值")
	if err != nil {
		check("充值", false, err.Error())
		return
	}
	check("充值到账", bal.Available.String() == "2000", "可用余额 "+bal.Available.String())

	// Order payable comes from the frozen price snapshot, not a recomputation.
	payable := amt("1836")
	_, bal, err = l.Consume(acct, payable, "order", "9001", "idem-pay-9001", "新购 SCECS 包年")
	if err != nil {
		check("订单扣款", false, err.Error())
		return
	}
	check("订单扣款", bal.Available.String() == "164", "余额 2000 - 1836 = "+bal.Available.String())
	fmt.Println()
}

// --- 2. Deduction order -----------------------------------------------------

func scenarioDeductionOrder() {
	fmt.Println("  [2] 抵扣顺序: 代金券 → 现金余额 (资源包一期不售)")
	l, _ := newLedger()
	_, _, _ = l.Recharge(acct, amt("500"), "r", "idem-rc-2", "")

	// A 300-yuan trial voucher against a 1836 order: the voucher is consumed
	// first, and only the remainder touches cash.
	rules := []pricing.PricingRule{{
		RuleID: 2, SKUCode: "scecs.s2.large.prepaid", RegionID: "*",
		DurationUnit:  pricing.DurationMonth,
		ListPrice:     amt("180"),
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	coupons := []pricing.Coupon{{
		CouponID: "coupon-trial", AccountID: acct,
		FaceValue: amt("300"), RemainValue: amt("300"),
		ProductCodes: []string{"scecs"},
		ExpireAt:     at.AddDate(0, 1, 0),
	}}

	q, err := pricing.Engine{}.Calculate(pricing.Request{
		AccountID: acct, ProductCode: "scecs", SKUCode: "scecs.s2.large.prepaid",
		RegionID: "cn-north-1", ChargeType: pricing.ChargePrepaid,
		Duration: 12, DurationUnit: pricing.DurationMonth, Quantity: 1, At: at,
	}, rules, nil, coupons)
	if err != nil {
		check("询价", false, err.Error())
		return
	}
	check("代金券先抵扣", q.CouponAmount.String() == "300", "券抵 "+q.CouponAmount.String())
	check("现金付余额部分", q.PayableAmount.String() == "1860", "现金应付 "+q.PayableAmount.String())

	// Cash balance is only 500, so this order cannot complete — which is the
	// correct outcome, not an error to work around.
	_, _, err = l.Consume(acct, q.PayableAmount, "order", "9002", "idem-pay-9002", "")
	check("余额不足被拒", errors.Is(err, ledger.ErrInsufficientBalance),
		"余额 500 < 应付 1860")
	fmt.Println()
}

// --- 3. Insufficient balance ------------------------------------------------

func scenarioInsufficientBalance() {
	fmt.Println("  [3] 余额不足: 拒绝而非透支")
	l, store := newLedger()
	_, _, _ = l.Recharge(acct, amt("100"), "r", "idem-rc-3", "")

	_, _, err := l.Consume(acct, amt("150"), "order", "9003", "idem-pay-9003", "")
	check("扣款被拒", errors.Is(err, ledger.ErrInsufficientBalance), "150 > 100")

	bal, _ := store.GetBalance(acct)
	check("余额未变", bal.Available.String() == "100", "仍为 "+bal.Available.String())
	check("余额非负", !bal.Available.IsNegative(), "欠费走账号状态机,不用负余额表达")

	entries, _ := store.ListEntries(acct)
	check("失败不写流水", len(entries) == 1, fmt.Sprintf("流水 %d 条(仅充值)", len(entries)))
	fmt.Println()
}

// --- 4. Channel callback idempotency ----------------------------------------

func scenarioChannelCallbackIdempotency() {
	fmt.Println("  [4] 渠道重复回调: 幂等键防重复销账")
	l, store := newLedger()

	// A payment channel retries its notification. Each retry carries the same
	// channel transaction number, which becomes the idempotency key.
	const channelTxn = "idem-callback-ALIPAY-2026080812345"
	for i := 0; i < 4; i++ {
		_, _, err := l.Recharge(acct, amt("1000"), "recharge-002", channelTxn, "渠道回调充值")
		switch {
		case i == 0 && err != nil:
			check("首次回调到账", false, err.Error())
		case i > 0 && !errors.Is(err, ledger.ErrDuplicateEntry):
			check("重复回调应幂等", false, fmt.Sprintf("第 %d 次返回 %v", i+1, err))
		}
	}

	bal, _ := store.GetBalance(acct)
	check("4 次回调只到账一次", bal.Available.String() == "1000", "余额 "+bal.Available.String())

	entries, _ := store.ListEntries(acct)
	check("流水只有一条", len(entries) == 1, fmt.Sprintf("%d 条", len(entries)))
	fmt.Println()
	fmt.Println("      注:渠道不保证 exactly-once,重试激进。若无 (channel, channel_txn_no)")
	fmt.Println("          唯一键,一次重试就会重复销账,用户被多充一笔。")
	fmt.Println()
}

// --- 5. Refund path ---------------------------------------------------------

func scenarioRefundPath() {
	fmt.Println("  [5] 退订: 冻结 → 释放确认 → 退款入账")
	l, store := newLedger()
	m := order.NewMachine(func() time.Time { return at })

	_, _, _ = l.Recharge(acct, amt("2000"), "r", "idem-rc-5", "")

	// Freeze while the order is in flight: funds are reserved but have not
	// left the account.
	_, bal, err := l.Freeze(acct, amt("1836"), "9005", "idem-frz-9005")
	if err != nil {
		check("下单冻结", false, err.Error())
		return
	}
	check("冻结不改变总额", bal.Total().String() == "2000",
		fmt.Sprintf("可用 %s + 冻结 %s", bal.Available, bal.Frozen))

	// Order proceeds: unfreeze, then charge for real.
	_, _, _ = l.Unfreeze(acct, amt("1836"), "9005", "idem-unfrz-9005")
	_, bal, _ = l.Consume(acct, amt("1836"), "order", "9005", "idem-pay-9005", "新购")
	check("履约后实扣", bal.Available.String() == "164", "余额 "+bal.Available.String())

	// Build the order in COMPLETED state to exercise the refund ordering rule.
	o, _, _ := m.Create(order.CreateRequest{
		AccountID: acct, Type: order.TypeNew, ProductCode: "scecs",
		ChargeType: pricing.ChargePrepaid, SKUCode: "scecs.s2.large.prepaid",
		RegionID: "cn-north-1", Quantity: 1, Duration: 12,
		DurationUnit: pricing.DurationMonth,
		Quote: pricing.Result{
			ListAmount: amt("2160"), PromoAmount: amt("324"),
			PayableAmount: amt("1836"),
		},
		SnapshotID: "snap-9005", ClientToken: "tok-9005", At: at,
	}, 9005, "SO202608080005")
	_, _ = m.MarkPaid(o, amt("1836"), 0)
	_, _ = m.StartFulfilment(o, 1)
	_, _ = m.CompleteFulfilment(o, "scecs-cn-north-1-01-c3d4e5f6", 2)

	// Refund attempted before release must be refused — otherwise the customer
	// holds both the money and a running resource.
	_, err = m.StartRefund(o, false, 3)
	check("未释放不得退款", errors.Is(err, order.ErrResourceNotReleased), "钱货两清有先后")

	// Orchestrator confirms release; refund proceeds, prorated from the
	// snapshot (1 month used of 12).
	if _, err := m.StartRefund(o, true, 3); err != nil {
		check("释放后退款", false, err.Error())
		return
	}
	refund := order.ProrationBasis{
		SnapshotAmount: amt("1836"), TotalPeriods: 12, UsedPeriods: 1,
	}.RefundAmount()
	check("按剩余时长折算", refund.String() == "1683", "退款 "+refund.String()+" (11/12)")

	_, bal, err = l.Refund(acct, refund, "9005", "idem-rfd-9005", "退订退款")
	if err != nil {
		check("退款入账", false, err.Error())
		return
	}
	_, _ = m.CompleteRefund(o, 4)
	check("退款到账", bal.Available.String() == "1847", "余额 164 + 1683 = "+bal.Available.String())

	res, _ := l.Reconcile(acct)
	check("退款后账实相符", res.Balanced(),
		fmt.Sprintf("流水合计 %s = 余额 %s", res.JournalSum, bal.Total()))
	_ = store
	fmt.Println()
}

// --- 6. Reconciliation ------------------------------------------------------

func scenarioReconciliation() {
	fmt.Println("  [6] T+1 对账: 流水必须复现余额")
	l, store := newLedger()

	_, _, _ = l.Recharge(acct, amt("5000"), "r1", "idem-a", "首充")
	_, _, _ = l.Consume(acct, amt("1836"), "order", "9001", "idem-b", "新购")
	_, _, _ = l.Consume(acct, amt("47.5"), "bill", "b-2026080812", "idem-c", "小时账单")
	_, _, _ = l.Freeze(acct, amt("500"), "9002", "idem-d")
	_, _, _ = l.Refund(acct, amt("200"), "9001", "idem-e", "部分退款")
	_, _, _ = l.Adjust(acct, amt("47.5"), true, "b-2026080812", "idem-f", "计量误差冲正")

	res, err := l.Reconcile(acct)
	if err != nil {
		check("对账", false, err.Error())
		return
	}
	bal, _ := store.GetBalance(acct)

	fmt.Printf("      可用 %s  冻结 %s  流水 %d 条\n", bal.Available, bal.Frozen, res.EntryCount)
	check("账实相符", res.Balanced(),
		fmt.Sprintf("流水合计 %s = 可用+冻结 %s", res.JournalSum, bal.Total()))
	check("链式完整", res.ChainIntact, "balance_after 链未断,未被篡改")
	check("差额为零", res.Difference.IsZero(), "差额 "+res.Difference.String())

	// Simulate tampering to show the chain check earns its keep.
	store.mu.Lock()
	store.entries[acct][1].Amount = amt("100")
	store.mu.Unlock()

	tampered, _ := l.Reconcile(acct)
	check("篡改被检出", !tampered.Balanced() && !tampered.ChainIntact,
		fmt.Sprintf("链在流水 #%d 断裂", tampered.FirstBreakAt))
	fmt.Println()
	fmt.Println("      注:balance_after 是冗余的运行余额,冗余正是其价值 ——")
	fmt.Println("          删行/改行/乱序都会断链,无需从头重算即可发现。")
}
