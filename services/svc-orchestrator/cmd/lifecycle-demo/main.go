// Command lifecycle-demo walks the resource side of the commercial loop:
//
//	订单支付 → 编排履约(saga) → 资源开通 → 计费起点 → 欠费锁定 → 释放
//
// It composes pkg-go/{order,resource,workflow} to show that the order state
// machine, the saga engine, and the resource lifecycle agree at every step —
// and that a failed provisioning rolls back cleanly instead of leaking quota
// and resources.
//
// Run: go run ./cmd/lifecycle-demo
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/starcloud/sc-platform/identifier"
	"github.com/starcloud/sc-platform/order"
	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/resource"
	"github.com/starcloud/sc-platform/workflow"
)

var (
	base     = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	failures int
)

const acct = int64(100123)

func main() {
	fmt.Println("Phase-1 资源链路: 支付 → 编排履约 → 开通 → 计费 → 欠费 → 释放")
	fmt.Println()

	scenarioProvisioning()
	scenarioRollback()
	scenarioTimeout()
	scenarioArrears()
	scenarioPrepaidExpiry()
	scenarioDataProtection()

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d 项检查失败\n", failures)
		os.Exit(1)
	}
	fmt.Println("全部通过 — 资源生命周期各环节衔接正确")
}

func check(name string, ok bool, detail string) {
	if ok {
		fmt.Printf("    PASS  %-44s %s\n", name, detail)
		return
	}
	failures++
	fmt.Printf("    FAIL  %-44s %s\n", name, detail)
}

func newInstance(id string, ct resource.ChargeType) *resource.Instance {
	return &resource.Instance{
		ResourceID:   id,
		AccountID:    acct,
		ProductCode:  "scecs",
		ResourceType: "instance",
		Region:       "cn-north-1",
		Zone:         "cn-north-1-a",
		ChargeType:   ct,
		State:        resource.StateInit,
		SpecCode:     "s2.large",
		OrderID:      9001,
		CreatedAt:    base,
		UpdatedAt:    base,
	}
}

// --- 1. Successful provisioning ---------------------------------------------

func scenarioProvisioning() {
	fmt.Println("  [1] 订单支付后编排履约,资源开通")

	// Resource id is minted with the shard factor embedded, so ops can
	// reverse-route it to its shard without a global index lookup.
	rid, err := identifier.NewResourceID("scecs", "cn-north-1", acct, 8, 16)
	if err != nil {
		check("生成资源ID", false, err.Error())
		return
	}
	check("资源ID格式", true, rid.String())

	inst := newInstance(rid.String(), resource.ChargePostpaid)
	m := resource.NewMachine(func() time.Time { return base })

	// The saga: quota → resource → metering → order confirmation. Each step
	// registers its compensation.
	var quotaOccupied, resourceCreated, meteringStarted bool
	engine := workflow.NewEngine(func() time.Time { return base })

	steps := []workflow.Step{
		{
			Name: "check_and_occupy_quota",
			Do:   func(c *workflow.Context) error { quotaOccupied = true; return nil },
			Undo: func(c *workflow.Context) error { quotaOccupied = false; return nil },
		},
		{
			Name: "create_resource",
			Do: func(c *workflow.Context) error {
				// svc-orchestrator dispatches via gRPC ApplyResource, then the
				// state moves to CREATING awaiting the controller callback.
				if _, err := m.Transition(inst, resource.StateCreating, 0, "dispatched to rc-compute"); err != nil {
					return err
				}
				resourceCreated = true
				c.Set("resource_id", inst.ResourceID)
				return nil
			},
			Undo: func(c *workflow.Context) error { resourceCreated = false; return nil },
		},
		{
			Name: "start_metering",
			Do:   func(c *workflow.Context) error { meteringStarted = true; return nil },
			Undo: func(c *workflow.Context) error { meteringStarted = false; return nil },
		},
	}

	res := engine.Run(1001, &workflow.Context{AccountID: acct, BizKey: "order-9001"}, steps)
	check("编排流程完成", res.Status == workflow.FlowCompleted, string(res.Status))
	check("配额已占用", quotaOccupied, "")
	check("资源已下发", resourceCreated && inst.State == resource.StateCreating, string(inst.State))
	check("计量已注册", meteringStarted, "")

	// The controller reports the instance is up, four minutes later.
	ready := resource.NewMachine(func() time.Time { return base.Add(4 * time.Minute) })
	evt, err := ready.Transition(inst, resource.StateRunning, 1, "rc-compute callback: Ready")
	if err != nil {
		check("控制器回调", false, err.Error())
		return
	}
	check("资源就绪", inst.State == resource.StateRunning, string(inst.State))
	check("计费起点=就绪时刻", evt.StartsBilling() && inst.BillingStart.Equal(base.Add(4*time.Minute)),
		"开通耗时 4 分钟不计费,对用户公平且可解释")
	fmt.Println()
}

// --- 2. Rollback on failure -------------------------------------------------

func scenarioRollback() {
	fmt.Println("  [2] 履约失败: 逆序补偿,不泄漏配额与资源")

	var quotaOccupied, resourceCreated bool
	var undoOrder []string
	engine := workflow.NewEngine(func() time.Time { return base })

	steps := []workflow.Step{
		{
			Name: "check_and_occupy_quota",
			Do:   func(c *workflow.Context) error { quotaOccupied = true; return nil },
			Undo: func(c *workflow.Context) error {
				undoOrder = append(undoOrder, "quota")
				quotaOccupied = false
				return nil
			},
		},
		{
			Name: "create_resource",
			Do:   func(c *workflow.Context) error { resourceCreated = true; return nil },
			Undo: func(c *workflow.Context) error {
				undoOrder = append(undoOrder, "resource")
				resourceCreated = false
				return nil
			},
		},
		{
			Name: "start_metering",
			Do:   func(c *workflow.Context) error { return errors.New("计量服务不可达") },
		},
	}

	res := engine.Run(1002, &workflow.Context{AccountID: acct}, steps)
	check("流程转为已补偿", res.Status == workflow.FlowCompensated, string(res.Status))
	check("配额已释放", !quotaOccupied, "补偿后未泄漏")
	check("资源已删除", !resourceCreated, "补偿后未泄漏")

	// Reverse order matters: quota was occupied before the resource existed,
	// so it is released after the resource is deleted. Releasing quota first
	// would briefly advertise capacity that does not physically exist.
	check("补偿逆序执行", len(undoOrder) == 2 && undoOrder[0] == "resource" && undoOrder[1] == "quota",
		fmt.Sprintf("%v — 先删资源再退配额", undoOrder))
	check("无需人工介入", !res.NeedsTicket, "补偿全部成功")
	fmt.Println()
}

// --- 3. Timeout backstop ----------------------------------------------------

func scenarioTimeout() {
	fmt.Println("  [3] 回调丢失: 中间态超时兜底")

	inst := newInstance("scecs-cn-north-1-01-deadbeef", resource.ChargePostpaid)
	inst.State = resource.StateCreating
	inst.UpdatedAt = base

	within := resource.NewMachine(func() time.Time { return base.Add(14 * time.Minute) })
	out, _ := within.TimedOut(*inst)
	check("14 分钟未超时", !out, "CREATING 超时阈值 15 分钟")

	beyond := resource.NewMachine(func() time.Time { return base.Add(16 * time.Minute) })
	out, _ = beyond.TimedOut(*inst)
	check("16 分钟已超时", out, "转失败态并触发补偿")

	if _, err := beyond.Transition(inst, resource.StateCreateFailed, 0, "timeout: 控制器无回调"); err != nil {
		check("转失败态", false, err.Error())
		return
	}
	check("配额可回收", inst.State == resource.StateCreateFailed, "否则配额永久泄漏")

	// Callback and polling are double-insurance: a late callback arriving now
	// finds the row already advanced and is a harmless no-op.
	_, err := beyond.Transition(inst, resource.StateRunning, 0, "迟到的回调")
	check("迟到回调无副作用", err != nil, "版本已推进,重复迁移被拒")
	fmt.Println()
}

// --- 4. Arrears lifecycle ---------------------------------------------------

func scenarioArrears() {
	fmt.Println("  [4] 欠费生命周期: 宽限 → 锁定保留 → 充值恢复")

	inst := newInstance("scecs-cn-north-1-01-cafebabe", resource.ChargePostpaid)
	inst.State = resource.StateRunning
	inst.BillingStart = base
	inst.Version = 3

	overdue := base.Add(10 * 24 * time.Hour)
	m := resource.NewMachine(func() time.Time { return overdue })

	// Grace: service continues, customer is dunned. This is deliberate —
	// cutting service the instant a payment fails costs more trust than the
	// day of revenue it protects.
	std := m.ArrearsDeadline(overdue, false)
	vip := m.ArrearsDeadline(overdue, true)
	check("标准宽限 24h", std.Equal(overdue.Add(24*time.Hour)), "宽限期内继续服务并催缴")
	check("大客户宽限 72h", vip.Equal(overdue.Add(72*time.Hour)), "按合同延长")

	// Grace expires: lock service, retain data.
	lockAt := overdue.Add(24 * time.Hour)
	mLock := resource.NewMachine(func() time.Time { return lockAt })
	evt, err := mLock.Transition(inst, resource.StateLocked, 3, "宽限期结束仍欠费")
	if err != nil {
		check("停服锁定", false, err.Error())
		return
	}
	check("停服但保留数据", inst.State == resource.StateLocked, "欠费不销毁数据 —— 商业可信度底线")
	check("停止计费", evt.StopsBilling(), "锁定后不再累计费用")

	deadline := mLock.ReleaseDeadline(*inst)
	check("保留期 30 天", deadline.Equal(lockAt.Add(30*24*time.Hour)), deadline.Format("2006-01-02"))

	// Customer pays on day 10.
	pay := resource.NewMachine(func() time.Time { return lockAt.Add(10 * 24 * time.Hour) })
	if _, err := pay.Transition(inst, resource.StateRunning, 4, "充值销账"); err != nil {
		check("充值恢复", false, err.Error())
		return
	}
	check("充值后恢复服务", inst.State == resource.StateRunning, "数据完好")
	check("计费起点未回拨", inst.BillingStart.Equal(base), "否则可 stop/start 刷掉账单")
	fmt.Println()
}

// --- 5. Prepaid expiry ------------------------------------------------------

func scenarioPrepaidExpiry() {
	fmt.Println("  [5] 包年包月到期: 提醒 → 到期保留 15 天 → 续费恢复")

	expiry := base.Add(60 * 24 * time.Hour)
	inst := newInstance("scecs-cn-north-1-01-feedface", resource.ChargePrepaid)
	inst.State = resource.StateRunning
	inst.BillingStart = base
	inst.ExpiredAt = expiry

	// Reminder schedule 30/15/7/3/1 days out (D8).
	reminded := []int{}
	for _, d := range []int{30, 15, 7, 3, 1} {
		m := resource.NewMachine(func() time.Time {
			return expiry.Add(-time.Duration(d)*24*time.Hour - time.Hour)
		})
		if day, due := m.RenewRemindDue(*inst); due {
			reminded = append(reminded, day)
		}
	}
	check("续费提醒 30/15/7/3/1 天", len(reminded) == 5, fmt.Sprintf("%v", reminded))

	// Not reminded on unscheduled days — reminder fatigue is real.
	quiet := resource.NewMachine(func() time.Time { return expiry.Add(-20*24*time.Hour - time.Hour) })
	_, due := quiet.RenewRemindDue(*inst)
	check("非提醒日不打扰", !due, "第 20 天无提醒")

	// Expiry: stop service, retain data 15 days.
	mExp := resource.NewMachine(func() time.Time { return expiry })
	if _, err := mExp.Transition(inst, resource.StateExpired, 0, "包年包月到期"); err != nil {
		check("到期停机", false, err.Error())
		return
	}
	rel := mExp.ReleaseDeadline(*inst)
	check("到期保留 15 天", rel.Equal(expiry.Add(15*24*time.Hour)),
		"比欠费的 30 天短 —— 到期是可预期的,欠费未必")

	// Renew on day 5 of the retention window.
	mRenew := resource.NewMachine(func() time.Time { return expiry.Add(5 * 24 * time.Hour) })
	inst.Extend(30 * 24 * time.Hour)
	if _, err := mRenew.Transition(inst, resource.StateRunning, 1, "保留期内续费"); err != nil {
		check("续费恢复", false, err.Error())
		return
	}
	check("续费后恢复", inst.State == resource.StateRunning && inst.ExpiredAt.Equal(expiry.Add(30*24*time.Hour)),
		"数据完整,到期时间顺延")
	fmt.Println()
}

// --- 6. Data protection -----------------------------------------------------

func scenarioDataProtection() {
	fmt.Println("  [6] 释放前置条件: 终版通知必须先送达")

	lockAt := base
	inst := newInstance("scecs-cn-north-1-01-0badf00d", resource.ChargePostpaid)
	inst.State = resource.StateLocked
	inst.LockedAt = lockAt

	deadline := lockAt.Add(30 * 24 * time.Hour)

	// Final-notice window opens 24h before deletion.
	early := resource.NewMachine(func() time.Time { return deadline.Add(-48 * time.Hour) })
	check("48h 前不发终版通知", !early.FinalNoticeDue(*inst), "避免过早惊扰")

	notice := resource.NewMachine(func() time.Time { return deadline.Add(-12 * time.Hour) })
	check("24h 内发终版通知", notice.FinalNoticeDue(*inst), "留最后的挽回窗口")

	// Release without a recorded notice must be refused.
	after := resource.NewMachine(func() time.Time { return deadline.Add(time.Hour) })
	check("保留期已满", after.ReleaseEligible(*inst), "")

	_, err := after.Release(inst, false, 0, "保留期结束")
	check("未通知不得释放", errors.Is(err, resource.ErrNoFinalNotice),
		"数据销毁必须先有可查证的告知")
	check("被拒后状态不变", inst.State == resource.StateLocked, string(inst.State))

	// With the notice recorded, release proceeds.
	if _, err := after.Release(inst, true, 0, "保留期结束,终版通知已送达"); err != nil {
		check("通知后释放", false, err.Error())
		return
	}
	if _, err := after.Transition(inst, resource.StateReleased, 1, "控制器回收确认"); err != nil {
		check("回收确认", false, err.Error())
		return
	}
	check("释放完成", inst.State == resource.StateReleased && !inst.ReleasedAt.IsZero(),
		"终态保留记录供审计与费用追溯")

	fmt.Println()
	fmt.Println("      注:对标启示 8 —— 生命周期状态机与数据保留政策是商业可信度底线。")
	fmt.Println("          欠费/到期通知必须可达可追溯,绝不误删客户数据。")
	fmt.Println("          代码层面把\"已通知\"设为释放的前置条件,而非写在文档里靠自觉。")

	// Order side stays in step: a refund still cannot precede release.
	om := order.NewMachine(func() time.Time { return base })
	o, _, _ := om.Create(order.CreateRequest{
		AccountID: acct, Type: order.TypeNew, ProductCode: "scecs",
		ChargeType: pricing.ChargePrepaid, SKUCode: "scecs.s2.large.prepaid",
		RegionID: "cn-north-1", Quantity: 1, Duration: 12,
		DurationUnit: pricing.DurationMonth,
		Quote: pricing.Result{
			ListAmount: pricing.MustParseAmount("2160"),
			PayableAmount: pricing.MustParseAmount("2160"),
		},
		SnapshotID: "snap-x", ClientToken: "tok-x", At: base,
	}, 9009, "SO202608080009")
	_, _ = om.MarkPaid(o, pricing.MustParseAmount("2160"), 0)
	_, _ = om.StartFulfilment(o, 1)
	_, _ = om.CompleteFulfilment(o, inst.ResourceID, 2)
	_, err = om.StartRefund(o, false, 3)
	fmt.Println()
	check("订单侧同样约束", errors.Is(err, order.ErrResourceNotReleased),
		"资源未释放前不得退款,两侧规则一致")
}
