// Command rc-demo exercises the SCLOG data-plane fulfilment layer end to end:
//
//	下发 Spec → 控制器收敛 → 状态回报 → 平台状态机裁定 → 计量 → 欠费冻结 → 回收
//
// This is the 06§6.3 MockDriver 验收门槛: every new product must pass the full
// supply-chain test on the MockDriver before launch. SCLOG (M-7.5) is a managed
// log service (Vector collection + ClickHouse storage), productizing the
// platform's own operational experience (09§4.2). Fulfilled by DriverK8s
// (bound in provision.Registry).
//
// Run: go run ./cmd/rc-demo
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

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
	resourceID = "sclog-cn-north-1-01-7a8b9c0d"
)

func main() {
	fmt.Println("SCLOG 数据面 (DriverK8s): 下发 → 收敛 → 回报 → 平台裁定 → 计量 → 冻结 → 回收")
	fmt.Println()

	scenarioDispatch()
	scenarioPhaseIsEvidence()
	scenarioIdempotency()
	scenarioSuspend()
	scenarioDriverBindingK8s()
	scenarioCompensation()

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d 项检查失败\n", failures)
		os.Exit(1)
	}
	fmt.Println("全部通过 — SCLog 日志服务 经 MockDriver 全链路验收 (06§6.3 验收门槛)")
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
		ProductCode:    "sclog",
		ResourceType:   "scloginstance",
		Region:         "cn-north-1",
		Zone:           "",
		Edition:        "standard",
		Params:         map[string]string{"retentionDays": "30", "storageGb": "200", "crossAz": "false"},
		IdempotencyKey: "order-9301",
	}
}

func newInstance() *resource.Instance {
	return &resource.Instance{
		ResourceID:   resourceID,
		AccountID:    acct,
		ProductCode:  "sclog",
		ResourceType: "scloginstance",
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
	check("Spec 校验通过", s.Validate() == nil, "四要素齐备 (含 REGIONAL region, 无 zone)")

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
	fmt.Println("      注:日志服务是 REGIONAL 产品(跨 AZ 存储是副本标志非放置约束),")
	fmt.Println("          与 SCECI 的 ZONAL 形态同走声明式收敛,后端同为 DriverK8s。")
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
	check("删除不存在的资源也成功", d.Delete("sclog-cn-north-1-01-ffffffff") == nil, "")
	fmt.Println()
}

// --- 4. Suspend semantics ----------------------------------------------------

func scenarioSuspend() {
	fmt.Println("  [4] 欠费冻结: 停止采集但绝不删已有日志(日志可查询是底线)")

	d := provision.NewMockDriver(func() time.Time { return base })
	d.ProvisioningDelay = 0
	s := spec()
	if _, err := d.Apply(s); err != nil {
		check("创建", false, err.Error())
		return
	}

	usage, _ := d.CollectUsage(resourceID)
	check("运行中产生计量", len(usage) > 0, fmt.Sprintf("%d 个计量项", len(usage)))

	// Arrears freeze: stop accepting NEW ingestion, but existing logs stay.
	s.Suspend = true
	st, err := d.Apply(s)
	if err != nil {
		check("冻结", false, err.Error())
		return
	}
	check("已冻结", st.Phase == provision.PhaseSuspended, string(st.Phase))
	check("实例仍在", d.Exists(resourceID), "欠费不销毁已有日志 —— 数据可查询是底线")

	usage, _ = d.CollectUsage(resourceID)
	check("冻结后停止计费", len(usage) == 0, "停服的同时停表(存储+采集计量随之停)")

	// Customer pays.
	s.Suspend = false
	st, _ = d.Apply(s)
	usage, _ = d.CollectUsage(resourceID)
	check("充值后完整恢复", st.Phase == provision.PhaseReady && len(usage) > 0,
		"实例带着历史日志回来")
	fmt.Println()
}

// --- 5. Driver binding: sclog → DriverK8s ----------------------------------

func scenarioDriverBindingK8s() {
	fmt.Println("  [5] 驱动绑定: sclog 绑定 DriverK8s")

	r := provision.NewRegistry()
	mock := provision.NewMockDriver(func() time.Time { return base })
	mock.ProvisioningDelay = 0
	r.Register(mock)
	r.Register(provision.VMDriver{})

	// M-7.5 decision: SCLOG is fulfilled by the K8s driver. DriverMock stands
	// in for DriverK8s in source-only phase-2.
	r.BindProduct("sclog", provision.DriverK8s)
	r.BindProduct("scecs", provision.DriverVM)

	bound := r.BoundProducts()
	hasSclog := false
	for _, p := range bound {
		if p == "sclog" {
			hasSclog = true
		}
	}
	check("sclog 已绑定驱动", hasSclog, "目录配置即决定后端,非管控面改造")

	// DriverK8s is bound but not Registered as a real impl in source-only
	// phase-2 (only mock+vm are). Resolution yields ErrUnknownDriver — the
	// honest source-only state. What must NOT happen is silent routing to VM.
	_, err := r.DriverFor("sclog")
	check("sclog 未误绑 VM 驱动", errors.Is(err, provision.ErrUnknownDriver),
		"K8s 驱动未实装即明确报错,不静默降级")

	// The unbound-product guard: a product with no binding cannot dispatch.
	r2 := provision.NewRegistry()
	r2.Register(mock)
	_, err = r2.DriverFor("sclog")
	check("未绑定产品拒绝下发", errors.Is(err, provision.ErrUnknownDriver),
		"无绑定即不可下单")
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
			Name: "create_log_instance",
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
	res := engine.Run(9301, &workflow.Context{AccountID: acct}, steps)
	check("补偿成功不开工单", res.Status == workflow.FlowCompensated && !res.NeedsTicket,
		string(res.Status))
	check("资源已回收", !d.Exists(resourceID), "无泄漏")

	// Chaos: the controller is unreachable during compensation.
	d2 := provision.NewMockDriver(func() time.Time { return base })
	d2.ProvisioningDelay = 0
	d2.FailDelete = func(id string) error { return errors.New("controller unreachable") }

	steps2 := []workflow.Step{
		{
			Name: "create_log_instance",
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
	res2 := engine.Run(9302, &workflow.Context{AccountID: acct}, steps2)
	check("补偿失败转人工", res2.Status == workflow.FlowFailed && res2.NeedsTicket,
		"3 次重试后开工单,而非无限重试")
	check("失败原因留痕", len(res2.CompensationErrors) == 1,
		"平台处于引擎无法自行清理的状态,必须有人看")

	fmt.Println()
	fmt.Println("      注:对永久故障的下游无限重试,与宕机无从区分。三次后转人工,")
	fmt.Println("          让人来判断是重试、手工清理,还是接受现状(03§8.4)。")
}
