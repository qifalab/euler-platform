// Command metering-demo walks the metering-to-bill loop:
//
//	采集 → 小时聚合(覆盖率) → 迟到数据合并 → 出账 → 抵扣 → 月度账单 → 对账
//
// It composes pkg-go/{metering,billing,pricing,resource} to show that a
// resource's runtime produces exactly the charges it should, that incomplete
// collection is visible all the way to the bill, and that the reconciliation
// catches what the pipeline alone cannot see.
//
// Run: go run ./cmd/metering-demo
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/starcloud/sc-platform/billing"
	"github.com/starcloud/sc-platform/metering"
	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/resource"
)

var (
	hour     = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	settleAt = time.Date(2026, 8, 8, 13, 15, 0, 0, time.UTC)
	failures int
)

const (
	acct       = int64(100123)
	resourceID = "scecs-cn-north-1-01-a1b2c3d4"
)

func main() {
	fmt.Println("Phase-1 计量计费链路: 采集 → 聚合 → 出账 → 抵扣 → 账单 → 对账")
	fmt.Println()

	scenarioFullHour()
	scenarioDuplicateCollection()
	scenarioIncompleteHour()
	scenarioLateData()
	scenarioDeductionWaterfall()
	scenarioArrears()
	scenarioMonthlyBill()
	scenarioReconciliation()
	scenarioFreeze()

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d 项检查失败\n", failures)
		os.Exit(1)
	}
	fmt.Println("全部通过 — 计量计费闭环正确,账单可解释")
}

func check(name string, ok bool, detail string) {
	if ok {
		fmt.Printf("    PASS  %-42s %s\n", name, detail)
		return
	}
	failures++
	fmt.Printf("    FAIL  %-42s %s\n", name, detail)
}

func amt(s string) pricing.Amount { return pricing.MustParseAmount(s) }

// reading builds one minute's usage record.
func reading(minute int, qty string, batch string) metering.UsageRecord {
	ws := hour.Add(time.Duration(minute) * time.Minute)
	return metering.UsageRecord{
		RecordID:      metering.RecordID(resourceID, "cpu_core_hour", ws),
		AccountID:     acct,
		Region:        "cn-north-1",
		ResourceType:  "ecs",
		ResourceID:    resourceID,
		MeteringItem:  "cpu_core_hour",
		Quantity:      metering.MustParseQuantity(qty),
		WindowStart:   ws,
		WindowSeconds: 60,
		CollectTS:     ws.Add(time.Second),
		CollectorID:   "agent-01",
		BatchID:       batch,
	}
}

func hourReadings(n int, qty string) []metering.UsageRecord {
	out := make([]metering.UsageRecord, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, reading(i, qty, metering.BatchRealtime))
	}
	return out
}

// --- 1. A complete hour ------------------------------------------------------

func scenarioFullHour() {
	fmt.Println("  [1] 完整小时: 60 个窗口全部到达")
	agg := metering.NewAggregator(func() time.Time { return settleAt })

	// A 2C instance reports 2 core-minutes per minute = 0.033333 core-hours.
	usage, err := agg.Aggregate(hourReadings(60, "0.033333"), hour)
	if err != nil {
		check("聚合", false, err.Error())
		return
	}
	check("覆盖率 100%", usage.Complete(), fmt.Sprintf("%d/%d 窗口", usage.WindowsSeen, usage.WindowsExpected))
	check("用量精确", usage.TotalQuantity.String() == "1.99998", "60 × 0.033333 = "+usage.TotalQuantity.String())
	check("无需补采", func() bool { _, need := metering.NeedsBackfill(usage); return !need }(), "")

	e := billing.NewEngine(func() time.Time { return settleAt })
	s, err := e.Settle(usage, amt("0.25"), "scecs", "snap-9001",
		[]billing.Pool{{Source: billing.SourceBalance, Available: amt("100")}})
	if err != nil {
		check("出账", false, err.Error())
		return
	}
	check("出账金额", s.Charge.PretaxAmount.String() == "0.499995",
		"1.99998 核时 × 0.25 = "+s.Charge.PretaxAmount.String())
	check("分项平衡", s.Charge.Reconciles(), "抵扣合计 = 原价")
	fmt.Println()
}

// --- 2. Duplicate collection -------------------------------------------------

func scenarioDuplicateCollection() {
	fmt.Println("  [2] 重复采集: 计量宁可重采不可漏采")
	agg := metering.NewAggregator(func() time.Time { return settleAt })

	// The agent retransmits the whole hour after a network failure.
	records := append(hourReadings(60, "0.033333"), hourReadings(60, "0.033333")...)
	usage, _ := agg.Aggregate(records, hour)

	check("重传不翻倍", usage.TotalQuantity.String() == "1.99998",
		"120 条记录去重为 60 个窗口 = "+usage.TotalQuantity.String())
	check("覆盖率仍 100%", usage.CoveredRatio == 100, "")
	fmt.Println()
	fmt.Println("      注:幂等键由 sha1(resource|item|window) 派生而非随机生成,")
	fmt.Println("          同一窗口重传算出同一个键,在 Kafka/聚合/CK/出账 四层同时坍缩。")
	fmt.Println()
}

// --- 3. Incomplete hour ------------------------------------------------------

func scenarioIncompleteHour() {
	fmt.Println("  [3] 采集缺失: 覆盖率如实上报并触发补采")
	agg := metering.NewAggregator(func() time.Time { return settleAt })

	// Only 45 of 60 windows arrived (agent restarted mid-hour).
	usage, _ := agg.Aggregate(hourReadings(45, "0.033333"), hour)

	check("覆盖率 75%", usage.CoveredRatio == 75, fmt.Sprintf("%d/60 窗口", usage.WindowsSeen))
	check("不冒充完整", !usage.Complete(), "不完整的小时与安静的小时必须可区分")

	task, need := metering.NeedsBackfill(usage)
	check("触发补采任务", need && task.MissingCount == 15, fmt.Sprintf("缺 %d 个窗口", task.MissingCount))

	// The incompleteness rides all the way to the bill line.
	e := billing.NewEngine(func() time.Time { return settleAt })
	s, _ := e.Settle(usage, amt("0.25"), "scecs", "snap-9001",
		[]billing.Pool{{Source: billing.SourceBalance, Available: amt("100")}})
	check("账单行标记暂定", s.Charge.Incomplete() && s.Charge.CoveredRatio == 75,
		"否则用户看到的是一个不知道少算了多少的数")
	fmt.Println()
}

// --- 4. Late data ------------------------------------------------------------

func scenarioLateData() {
	fmt.Println("  [4] 迟到数据: 合并入历史窗口,不重复计入")
	agg := metering.NewAggregator(func() time.Time { return settleAt })

	onTime := hourReadings(58, "1")
	first, _ := agg.Aggregate(onTime, hour)
	check("初次覆盖率 96%", first.CoveredRatio == 96, fmt.Sprintf("%d/60", first.WindowsSeen))

	// Two stragglers plus a duplicate of a window already counted.
	late := []metering.UsageRecord{
		reading(58, "1", metering.BatchLate),
		reading(59, "1", metering.BatchLate),
		reading(0, "1", metering.BatchLate), // duplicate
	}
	merged, err := agg.Merge(first, late, onTime)
	if err != nil {
		check("合并", false, err.Error())
		return
	}
	check("合并后 100%", merged.CoveredRatio == 100, "")
	check("重复窗口不叠加", merged.TotalQuantity.String() == "60", "总量 "+merged.TotalQuantity.String())
	check("聚合ID稳定", merged.AggID == first.AggID, "出账幂等键不因重算而变")

	// Publication waits for the watermark so stragglers get their chance.
	early := metering.NewAggregator(func() time.Time { return hour.Add(time.Hour + 5*time.Minute) })
	late10 := metering.NewAggregator(func() time.Time { return hour.Add(time.Hour + 11*time.Minute) })
	check("水位线前不发布", !early.WatermarkPassed(hour), "整点后 5 分钟仍等待")
	check("水位线后发布", late10.WatermarkPassed(hour), "整点后 10 分钟水位线到达")
	fmt.Println()
}

// --- 5. Deduction waterfall --------------------------------------------------

func scenarioDeductionWaterfall() {
	fmt.Println("  [5] 抵扣顺序: 资源包 → 代金券(到期升序) → 现金余额")
	agg := metering.NewAggregator(func() time.Time { return settleAt })
	usage, _ := agg.Aggregate(hourReadings(60, "0.166667"), hour) // 10 core-hours

	e := billing.NewEngine(func() time.Time { return settleAt })
	pools := []billing.Pool{
		{Source: billing.SourceBalance, Available: amt("100")},
		{Source: billing.SourceCoupon, Ref: "券-9月到期", Available: amt("1"),
			ExpireAt: settleAt.AddDate(0, 2, 0)},
		{Source: billing.SourceCoupon, Ref: "券-8月到期", Available: amt("1"),
			ExpireAt: settleAt.AddDate(0, 1, 0)},
	}
	s, err := e.Settle(usage, amt("0.25"), "scecs", "snap-9001", pools)
	if err != nil {
		check("出账", false, err.Error())
		return
	}

	// 10.00002 core-hours × 0.25 = 2.500005
	check("原价", s.Charge.PretaxAmount.String() == "2.500005", s.Charge.PretaxAmount.String())

	var order []string
	for _, d := range s.Charge.Deductions {
		label := string(d.Source)
		if d.Ref != "" {
			label = d.Ref
		}
		order = append(order, fmt.Sprintf("%s:%s", label, d.Amount))
	}
	check("先用早到期的券", len(order) >= 2 && s.Charge.Deductions[0].Ref == "券-8月到期",
		fmt.Sprintf("%v", order))
	check("分项平衡", s.Charge.Reconciles(), "抵扣合计 = 原价")
	fmt.Println()
	fmt.Println("      注:券按到期升序消耗 —— 快过期的先花掉,避免用户的额度")
	fmt.Println("          白白过期而现金反被扣走,这是会变成工单的那种设计。")
	fmt.Println()
}

// --- 6. Arrears --------------------------------------------------------------

func scenarioArrears() {
	fmt.Println("  [6] 余额不足: 转欠费状态,不做负余额")
	agg := metering.NewAggregator(func() time.Time { return settleAt })
	usage, _ := agg.Aggregate(hourReadings(60, "0.166667"), hour)

	e := billing.NewEngine(func() time.Time { return settleAt })
	s, _ := e.Settle(usage, amt("0.25"), "scecs", "snap-9001",
		[]billing.Pool{{Source: billing.SourceBalance, Available: amt("1")}})

	check("能付的先付", s.Charge.PayAmount.String() == "1", "扣完余额 "+s.Charge.PayAmount.String())
	check("差额转欠费", s.InArrears && s.Shortfall.String() == "1.500005",
		"欠 "+s.Shortfall.String())

	// The lifecycle machine takes over from here: grace → lock → retain → release.
	m := resource.NewMachine(func() time.Time { return settleAt })
	deadline := m.ArrearsDeadline(settleAt, false)
	check("交由生命周期接管", deadline.Equal(settleAt.Add(24*time.Hour)),
		"宽限 24h 后停服锁定,数据保留 30 天")
	fmt.Println()
}

// --- 7. Monthly bill ---------------------------------------------------------

func scenarioMonthlyBill() {
	fmt.Println("  [7] 月度账单: 含暂定行则不可出具")
	agg := metering.NewAggregator(func() time.Time { return settleAt })
	e := billing.NewEngine(func() time.Time { return settleAt })
	pools := []billing.Pool{{Source: billing.SourceBalance, Available: amt("1000")}}

	var charges []billing.Charge
	// 23 complete hours plus one that lost part of its collection.
	for h := 0; h < 24; h++ {
		hr := hour.Add(time.Duration(h) * time.Hour)
		n := 60
		if h == 7 {
			n = 40 // agent restart
		}
		recs := make([]metering.UsageRecord, 0, n)
		for i := 0; i < n; i++ {
			ws := hr.Add(time.Duration(i) * time.Minute)
			recs = append(recs, metering.UsageRecord{
				RecordID:     metering.RecordID(resourceID, "cpu_core_hour", ws),
				AccountID:    acct,
				ResourceID:   resourceID,
				MeteringItem: "cpu_core_hour",
				Quantity:     metering.MustParseQuantity("0.033333"),
				WindowStart:  ws,
				BatchID:      metering.BatchRealtime,
			})
		}
		u, err := agg.Aggregate(recs, hr)
		if err != nil {
			continue
		}
		s, err := e.Settle(u, amt("0.25"), "scecs", "snap-9001", pools)
		if err != nil {
			continue
		}
		charges = append(charges, s.Charge)
	}

	bill := billing.Summarize(acct, "2026-08", charges)
	check("账单行数", bill.ChargeCount == 24, fmt.Sprintf("%d 行", bill.ChargeCount))
	check("识别暂定行", bill.IncompleteCharges == 1, fmt.Sprintf("%d 行基于不完整计量", bill.IncompleteCharges))
	check("分项全平", bill.UnreconciledCount == 0, "无未解释差异")
	check("暂定账单不可出具", !bill.Final(), "补采完成后金额会变,提前出具必然引发争议")
	fmt.Printf("      合计 %s CNY,实付 %s CNY\n", bill.TotalAmount, bill.PaidAmount)
	fmt.Println()
}

// --- 8. Reconciliation -------------------------------------------------------

func scenarioReconciliation() {
	fmt.Println("  [8] T+1 对账: 资源台账 vs 计量记录")

	// Perfect month.
	ok := metering.Reconcile(resourceID, 720, 720)
	check("完全一致", !ok.NeedsBackfill && !ok.NeedsAlert, "差异率 0")

	// One hour missing out of a month: 0.14% > 0.1% engineering trigger.
	small := metering.Reconcile(resourceID, 720, 719)
	check("缺 1 小时触发补数", small.NeedsBackfill && !small.NeedsAlert,
		fmt.Sprintf("差异率 %.3f%%", small.DiffRatio*100))

	// Five hours missing: 0.69% > 0.5% alert threshold.
	big := metering.Reconcile(resourceID, 720, 715)
	check("缺 5 小时触发告警", big.NeedsAlert, fmt.Sprintf("差异率 %.3f%%", big.DiffRatio*100))

	// The silent leak: ran all day, metered nothing.
	silent := metering.Reconcile(resourceID, 24, 0)
	check("运行但零计量被捕获", silent.NeedsAlert && silent.DiffRatio == 1.0,
		"管道内部无从发现 —— 它根本没见过这个资源")

	fmt.Println()
	fmt.Println("      注:0.1%/0.5% 是工程触发线,不是验收口径。商业验收要求")
	fmt.Println("          \"无未解释差异\"—— 每一笔差异都要归因闭环,不论大小(裁决 S17)。")
	fmt.Println()
}

// --- 9. Period freeze --------------------------------------------------------

func scenarioFreeze() {
	fmt.Println("  [9] 账期冻结: 已结清月份拒绝补采")
	agg := metering.NewAggregator(func() time.Time { return settleAt })
	usage, _ := agg.Aggregate(hourReadings(60, "0.033333"), hour)

	boundary := metering.FreezeBoundary(hour)
	check("8月冻结于9月1日06:00", boundary.Equal(time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC)),
		boundary.Format("2006-01-02 15:04"))

	open := billing.NewEngine(func() time.Time { return time.Date(2026, 9, 1, 5, 0, 0, 0, time.UTC) })
	if _, err := open.Settle(usage, amt("0.25"), "scecs", "snap-1", nil); err != nil {
		check("冻结前可出账", false, err.Error())
	} else {
		check("冻结前可出账", true, "冻结前 1 小时仍开放")
	}

	frozen := billing.NewEngine(func() time.Time { return time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC) })
	_, err := frozen.Settle(usage, amt("0.25"), "scecs", "snap-1", nil)
	check("冻结后拒绝出账", errors.Is(err, billing.ErrFrozenPeriod),
		"迟到的更正若改动已付账单,比它修复的少算更糟")
}
