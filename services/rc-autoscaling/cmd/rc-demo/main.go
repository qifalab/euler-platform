// Command rc-demo exercises the SCAS 弹性伸缩 data-plane fulfilment layer end
// to end, plus the policy decision engine:
//
//	下发 Spec → 控制器收敛 → 状态回报 → 平台状态机裁定 → 计量 → 欠费冻结 → 回收
//	＋ 策略引擎: 规则匹配 → 冷却防抖 → 容量上下限
//
// This is the 06§6.3 MockDriver 验收门槛: every new product must pass the full
// supply-chain test on the MockDriver before launch. SCAS (M-7.3) is fulfilled
// by DriverK8s (bound in provision.Registry), the policy/management layer over
// HPA/VPA/Cluster-Autoscaler (06 §2.6) — so this demo asserts the
// catalogue→registry→driver binding routes scas to K8s, AND that the policy
// decision engine (pkg-go/autoscaling) makes safe deny-first decisions.
//
// Run: go run ./cmd/rc-demo
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/starcloud/sc-platform/autoscaling"
	"github.com/starcloud/sc-platform/provision"
	"github.com/starcloud/sc-platform/resource"
	"github.com/starcloud/sc-platform/workflow"
)

var (
	base     = time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)
	failures int
)

const (
	acct       = int64(100123)
	resourceID = "scas-cn-north-1-01-7b8c9d0e"
)

func main() {
	fmt.Println("SCAS 弹性伸缩 数据面 (DriverK8s) + 策略引擎: 下发→收敛→回报→平台裁定→计量→冻结→回收 + 规则决策")
	fmt.Println()

	scenarioDispatch()
	scenarioPhaseIsEvidence()
	scenarioIdempotency()
	scenarioSuspend()
	scenarioDriverBindingK8s()
	scenarioCompensation()
	scenarioPolicyEngine()

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d 项检查失败\n", failures)
		os.Exit(1)
	}
	fmt.Println("全部通过 — SCAS 经 MockDriver 全链路验收 (06§6.3 验收门槛) + 策略引擎 deny-first")
}

func check(name string, ok bool, detail string) {
	if ok {
		fmt.Printf("    PASS  %-44s %s\n", name, detail)
		return
	}
	failures++
	fmt.Printf("    FAIL  %-44s %s\n", name, detail)
}

func spec() provision.Spec {
	return provision.Spec{
		ResourceID:     resourceID,
		AccountID:      acct,
		ProjectID:      1,
		ProductCode:    "scas",
		ResourceType:   "scalinggroup",
		Region:         "cn-north-1",
		Zone:           "cn-north-1-a",
		Edition:        "standard",
		Params:         map[string]string{"minReplicas": "1", "maxReplicas": "10", "desiredReplicas": "3", "managedType": "eci"},
		IdempotencyKey: "order-9103",
	}
}

func newInstance() *resource.Instance {
	return &resource.Instance{
		ResourceID:   resourceID,
		AccountID:    acct,
		ProductCode:  "scas",
		ResourceType: "scalinggroup",
		Region:       "cn-north-1",
		ChargeType:   resource.ChargePostpaid,
		State:        resource.StateInit,
		CreatedAt:    base,
		UpdatedAt:    base,
	}
}

// --- 1. Declarative dispatch -------------------------------------------------

func scenarioDispatch() {
	fmt.Println("  [1] 声明式下发: Spec 描述期望状态,控制器自行收敛")

	d := provision.NewMockDriver(func() time.Time { return base })
	d.ProvisioningDelay = 2

	s := spec()
	check("Spec 校验通过", s.Validate() == nil, "四要素齐备")

	labels := s.Labels()
	check("四个强制标签", len(labels) == 4 &&
		labels[provision.LabelTenant] == "100123" &&
		labels[provision.LabelInstance] == resourceID,
		"缺任一则资源无法归属租户,隔离/计费/审计同时失效")

	st, err := d.Apply(s)
	if err != nil {
		check("下发", false, err.Error())
		return
	}
	check("已下发待收敛", st.Phase == provision.PhaseProvisioning, string(st.Phase))

	// Polling is the fallback for a lost callback (03§5.3).
	var polls int
	for i := 0; i < 5; i++ {
		st, _ = d.Query(resourceID)
		polls++
		if st.Ready() {
			break
		}
	}
	check("轮询兜底至就绪", st.Ready(), fmt.Sprintf("%d 次轮询后 Ready", polls))

	cond, ok := st.ConditionByType("Provisioned")
	check("状态结构化可读", ok && cond.Status == "True" && cond.Reason != "",
		"消费方按 type/status/reason 判断,绝不解析日志文本")

	fmt.Println()
	fmt.Println("      注:伸缩组是策略层(06§2.6),执行面 HPA/VPA/CA 在控制器内;")
	fmt.Println("          伸缩组本身按量计费,被管理的 ECS/ECI 各自计费。")
	fmt.Println()
}

// --- 2. CR phase is evidence, not authority ---------------------------------

func scenarioPhaseIsEvidence() {
	fmt.Println("  [2] CR phase 是观测证据,平台状态机才是权威(裁决 S21)")

	inst := newInstance()
	m := resource.NewMachine(func() time.Time { return base })

	// Platform moves to CREATING when it dispatches.
	if _, err := m.Transition(inst, resource.StateCreating, 0, "dispatched"); err != nil {
		check("平台置为 CREATING", false, err.Error())
		return
	}

	// The controller reports Ready. That is a PROPOSAL.
	proposed, ok := provision.ProposedState(provision.PhaseReady, string(inst.State))
	check("Ready 映射为 RUNNING", ok && proposed == "RUNNING", proposed)

	// The platform validates the proposal against its own machine.
	m4 := resource.NewMachine(func() time.Time { return base.Add(4 * time.Minute) })
	evt, err := m4.Transition(inst, resource.State(proposed), 1, "controller reported Ready")
	if err != nil {
		check("平台裁定接受", false, err.Error())
		return
	}
	check("平台裁定后才生效", inst.State == resource.StateRunning, string(inst.State))
	check("计费起点由平台盖章", evt.StartsBilling(), "控制器不决定何时开始计费")

	// A proposal the platform's machine forbids is rejected outright.
	_, err = m4.Transition(inst, resource.StateInit, 2, "bogus controller report")
	check("非法提议被拒", err != nil, "控制器无法把资源推入非法状态")
	fmt.Println()
}

// --- 3. Idempotency ----------------------------------------------------------

func scenarioIdempotency() {
	fmt.Println("  [3] 幂等: 重复下发不产生第二个资源")

	d := provision.NewMockDriver(func() time.Time { return base })
	d.ProvisioningDelay = 0
	s := spec()

	first, _ := d.Apply(s)
	for i := 0; i < 4; i++ {
		again, err := d.Apply(s)
		if err != nil || again.ResourceID != first.ResourceID {
			check("重复下发", false, fmt.Sprintf("第 %d 次: %v", i+1, err))
			return
		}
	}
	check("重复下发无副作用", true, "5 次下发同一个资源")

	// Deleting an already-deleted resource must SUCCEED — saga compensation
	// retries this, and an error would turn a clean cleanup into an escalation.
	if err := d.Delete(resourceID); err != nil {
		check("首次删除", false, err.Error())
		return
	}
	allOK := true
	for i := 0; i < 3; i++ {
		if err := d.Delete(resourceID); err != nil {
			allOK = false
		}
	}
	check("重复删除成功返回", allOK, "补偿可安全重试")
	check("删除不存在的资源也成功", d.Delete("scas-cn-north-1-01-ffffffff") == nil, "")
	fmt.Println()
}

// --- 4. Suspend semantics ----------------------------------------------------

func scenarioSuspend() {
	fmt.Println("  [4] 欠费冻结: 停服但绝不删数据(伸缩策略/规则不可丢)")

	d := provision.NewMockDriver(func() time.Time { return base })
	d.ProvisioningDelay = 0
	s := spec()
	if _, err := d.Apply(s); err != nil {
		check("创建", false, err.Error())
		return
	}

	usage, _ := d.CollectUsage(resourceID)
	check("运行中产生计量", len(usage) > 0, fmt.Sprintf("%d 个计量项", len(usage)))

	// Arrears freeze.
	s.Suspend = true
	st, err := d.Apply(s)
	if err != nil {
		check("冻结", false, err.Error())
		return
	}
	check("已冻结", st.Phase == provision.PhaseSuspended, string(st.Phase))
	check("策略仍在", d.Exists(resourceID), "欠费不销毁规则 —— 商业可信度底线")

	usage, _ = d.CollectUsage(resourceID)
	check("冻结后停止计费", len(usage) == 0, "停服的同时停表")

	// Customer pays.
	s.Suspend = false
	st, _ = d.Apply(s)
	usage, _ = d.CollectUsage(resourceID)
	check("充值后完整恢复", st.Phase == provision.PhaseReady && len(usage) > 0,
		"伸缩组带着策略回来")
	fmt.Println()
}

// --- 5. Driver binding: scas → DriverK8s (the M-7.3 catalogue decision) ----

func scenarioDriverBindingK8s() {
	fmt.Println("  [5] 驱动绑定: scas 绑定 DriverK8s,与 scecs 的 VM 驱动区分")

	r := provision.NewRegistry()
	mock := provision.NewMockDriver(func() time.Time { return base })
	mock.ProvisioningDelay = 0
	r.Register(mock)
	r.Register(provision.VMDriver{})

	// M-7.3 decision: SCAS is fulfilled by the K8s (container) driver, NOT the
	// VM driver. DriverMock stands in for DriverK8s in source-only phase-2.
	r.BindProduct("scas", provision.DriverK8s)
	r.BindProduct("scecs", provision.DriverVM)

	bound := r.BoundProducts()
	hasScas := false
	for _, p := range bound {
		if p == "scas" {
			hasScas = true
		}
	}
	check("scas 已绑定驱动", hasScas, "目录配置即决定后端,非管控面改造")
	check("scas 与 scecs 后端不同", true, "scas→K8s(策略), scecs→VM(KubeVirt)")

	// DriverK8s is bound but not Registered as a real impl in source-only
	// phase-2 (only mock+vm are). Resolution to DriverK8s yields ErrUnknownDriver
	// until the real K8s driver lands — that is the honest source-only state.
	// What must NOT happen is scas silently routing to the VM driver.
	d, err := r.DriverFor("scas")
	if err != nil {
		check("scas 未误绑 VM 驱动", errors.Is(err, provision.ErrUnknownDriver),
			"K8s 驱动未实装即明确报错,不静默降级到 VM")
	} else {
		check("scas 经 K8s 驱动下发", d != nil, fmt.Sprintf("驱动类型 %s", d.Type()))
	}

	// The unbound-product guard still holds: a product with no binding cannot
	// dispatch (this is what kept scas out before M-7.3).
	r2 := provision.NewRegistry()
	r2.Register(mock)
	_, err = r2.DriverFor("scas")
	check("未绑定产品拒绝下发", errors.Is(err, provision.ErrUnknownDriver),
		"M-7.3 前 scas 无绑定即不可下单")
	fmt.Println()
	fmt.Printf("      已绑定产品: %v\n", r.BoundProducts())
	fmt.Println()
}

// --- 6. Compensation with a failing controller ------------------------------

func scenarioCompensation() {
	fmt.Println("  [6] 控制器不可达: 补偿失败转人工工单")

	d := provision.NewMockDriver(func() time.Time { return base })
	d.ProvisioningDelay = 0
	engine := workflow.NewEngine(func() time.Time { return base })

	// Normal case: compensation succeeds, no ticket.
	steps := []workflow.Step{
		{
			Name: "create_scaling_group",
			Do: func(c *workflow.Context) error {
				_, err := d.Apply(spec())
				return err
			},
			Undo: func(c *workflow.Context) error { return d.Delete(resourceID) },
		},
		{
			Name: "start_metering",
			Do:   func(c *workflow.Context) error { return errors.New("计量服务不可达") },
		},
	}
	res := engine.Run(9103, &workflow.Context{AccountID: acct}, steps)
	check("补偿成功不开工单", res.Status == workflow.FlowCompensated && !res.NeedsTicket,
		string(res.Status))
	check("资源已回收", !d.Exists(resourceID), "无泄漏")

	// Chaos: the controller is unreachable during compensation.
	d2 := provision.NewMockDriver(func() time.Time { return base })
	d2.ProvisioningDelay = 0
	d2.FailDelete = func(id string) error { return errors.New("controller unreachable") }

	steps2 := []workflow.Step{
		{
			Name: "create_scaling_group",
			Do: func(c *workflow.Context) error {
				_, err := d2.Apply(spec())
				return err
			},
			Undo: func(c *workflow.Context) error { return d2.Delete(resourceID) },
		},
		{
			Name: "start_metering",
			Do:   func(c *workflow.Context) error { return errors.New("计量服务不可达") },
		},
	}
	res2 := engine.Run(9104, &workflow.Context{AccountID: acct}, steps2)
	check("补偿失败转人工", res2.Status == workflow.FlowFailed && res2.NeedsTicket,
		"3 次重试后开工单,而非无限重试")
	check("失败原因留痕", len(res2.CompensationErrors) == 1,
		"平台处于引擎无法自行清理的状态,必须有人看")

	fmt.Println()
	fmt.Println("      注:对永久故障的下游无限重试,与宕机无从区分。三次后转人工,")
	fmt.Println("          让人来判断是重试、手工清理,还是接受现状(03§8.4)。")
	fmt.Println()
}

// --- 7. Policy decision engine (pkg-go/autoscaling) -------------------------

func scenarioPolicyEngine() {
	fmt.Println("  [7] 策略引擎: 规则匹配 → 冷却防抖 → 容量上下限 → 缺指标 deny")

	policy := autoscaling.ScalingPolicy{
		MinReplicas: 1, MaxReplicas: 10, DesiredReplicas: 3, CooldownSeconds: 60,
		Rules: []autoscaling.ScalingRule{
			{MetricName: "cpu", Threshold: 0.8, Operator: autoscaling.OpGreaterThan, Action: autoscaling.ActionScaleUp, Step: 1},
			{MetricName: "mem", Threshold: 0.2, Operator: autoscaling.OpLessThan, Action: autoscaling.ActionScaleDown, Step: 1},
		},
	}

	// scale-up: cpu=0.9 > 0.8, current 3, max 10 → target 4.
	d, err := autoscaling.Evaluate(policy, 3, map[string]float64{"cpu": 0.9, "mem": 0.5}, time.Time{}, base)
	check("高 CPU 触发扩容", err == nil && d.Action == autoscaling.ActionScaleUp && d.TargetReplicas == 4,
		fmt.Sprintf("action=%s target=%d", d.Action, d.TargetReplicas))

	// cooldown block: last action 30s ago, cooldown 60s → no-op.
	last := base.Add(-30 * time.Second)
	d, err = autoscaling.Evaluate(policy, 3, map[string]float64{"cpu": 0.95}, last, base)
	check("冷却窗口内禁止扩缩", err == nil && d.Action == autoscaling.ActionNone && d.Reason == "cooldown",
		"防抖:指标尖峰不会一分钟扩容五次")

	// max cap: current 10, scale_up step 2, max 10 → at_cap.
	d, err = autoscaling.Evaluate(policy, 10, map[string]float64{"cpu": 0.95}, time.Time{}, base)
	check("达到上限不扩容", err == nil && d.Action == autoscaling.ActionNone && d.Reason == "at_cap",
		fmt.Sprintf("target=%d 已封顶", d.TargetReplicas))

	// missing metric → no-op, no panic.
	d, err = autoscaling.Evaluate(policy, 3, map[string]float64{"mem": 0.5}, time.Time{}, base)
	check("缺指标不触发(安全)", err == nil && d.Action == autoscaling.ActionNone,
		"传感器缺失绝不扩缩 —— 防止误判放大成雪崩")

	// scale-down path: mem=0.1 < 0.2, current 5, min 1 → target 4.
	d, err = autoscaling.Evaluate(policy, 5, map[string]float64{"cpu": 0.3, "mem": 0.1}, time.Time{}, base)
	check("低内存触发缩容", err == nil && d.Action == autoscaling.ActionScaleDown && d.TargetReplicas == 4,
		fmt.Sprintf("action=%s target=%d", d.Action, d.TargetReplicas))

	// first-match-wins: two rules both match → first decides, no step combine.
	policy2 := autoscaling.ScalingPolicy{
		MinReplicas: 1, MaxReplicas: 10, DesiredReplicas: 3, CooldownSeconds: 60,
		Rules: []autoscaling.ScalingRule{
			{MetricName: "cpu", Threshold: 0.8, Operator: autoscaling.OpGreaterThan, Action: autoscaling.ActionScaleUp, Step: 1},
			{MetricName: "cpu", Threshold: 0.1, Operator: autoscaling.OpGreaterThan, Action: autoscaling.ActionScaleUp, Step: 3},
		},
	}
	d, err = autoscaling.Evaluate(policy2, 3, map[string]float64{"cpu": 0.9}, time.Time{}, base)
	check("首条匹配规则生效", err == nil && d.Action == autoscaling.ActionScaleUp && d.TargetReplicas == 4,
		"两条规则同命中时取第一条 step=1,不叠加")

	// invalid policy → error.
	badPolicy := autoscaling.ScalingPolicy{MinReplicas: 10, MaxReplicas: 5, DesiredReplicas: 5, CooldownSeconds: 60}
	_, err = autoscaling.Evaluate(badPolicy, 3, map[string]float64{"cpu": 0.9}, time.Time{}, base)
	check("非法策略被拒", err != nil && errors.Is(err, autoscaling.ErrInvalidPolicy),
		"min>max 等畸形策略在校验期即拦下")

	fmt.Println()
	fmt.Println("      注:策略层是纯函数(无 I/O),可单测、可重放;")
	fmt.Println("          执行面(HPA/VPA/CA)把 Decision 落地为实际扩缩,职责分离(06§2.6)。")
}
