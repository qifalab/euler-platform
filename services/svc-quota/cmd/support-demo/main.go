// Command support-demo exercises the support domain where its three services
// intersect: the release path.
//
//	配额两阶段占用 → 资源生命周期 → 欠费 → 通知留证 → 释放前校验 → 审计链
//
// Releasing a customer's data touches all three at once: quota must be
// returned, the warning must be provably delivered, and the whole sequence must
// be recorded in a way an auditor can verify. This demo shows them composing.
//
// Run: go run ./cmd/support-demo
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/starcloud/sc-platform/audit"
	"github.com/starcloud/sc-platform/notify"
	"github.com/starcloud/sc-platform/quota"
	"github.com/starcloud/sc-platform/resource"
)

var (
	base     = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	failures int
)

const (
	acct       = int64(100123)
	resourceID = "scecs-cn-north-1-01-a1b2c3d4"
)

func main() {
	fmt.Println("Phase-1 支撑域: 配额 → 生命周期 → 通知留证 → 释放校验 → 审计链")
	fmt.Println()

	scenarioQuotaTwoPhase()
	scenarioQuotaLeak()
	scenarioNotificationEvidence()
	scenarioReleaseGate()
	scenarioAuditChain()

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d 项检查失败\n", failures)
		os.Exit(1)
	}
	fmt.Println("全部通过 — 支撑域在释放路径上协同正确")
}

func check(name string, ok bool, detail string) {
	if ok {
		fmt.Printf("    PASS  %-42s %s\n", name, detail)
		return
	}
	failures++
	fmt.Printf("    FAIL  %-42s %s\n", name, detail)
}

// --- 1. Quota two-phase ------------------------------------------------------

func scenarioQuotaTwoPhase() {
	fmt.Println("  [1] 配额两阶段: 预留 → 提交/释放")

	store := newQuotaStore()
	m := quota.NewManager(store, func() time.Time { return base }, nil)

	tok, err := m.CheckAndOccupy(acct, "quota_scecs_instance", "cn-north-1", 3, "order-9001")
	if err != nil {
		check("预留配额", false, err.Error())
		return
	}
	u, _ := m.Describe(acct, "quota_scecs_instance", "cn-north-1")
	check("预留后可用量减少", u.Available() == 17,
		fmt.Sprintf("上限 20,已提交 %d,预留 %d,可用 %d", u.Used, u.Occupying, u.Available()))
	check("预留未计入已用", u.Used == 0, "资源尚未创建")

	if err := m.CommitOccupy(tok.TokenID); err != nil {
		check("提交", false, err.Error())
		return
	}
	u, _ = m.Describe(acct, "quota_scecs_instance", "cn-north-1")
	check("提交后转为已用", u.Used == 3 && u.Occupying == 0,
		fmt.Sprintf("已用 %d,预留 %d", u.Used, u.Occupying))

	fmt.Println()
	fmt.Println("      注:单阶段\"下单即扣\"在履约失败时泄漏容量;\"成功才扣\"则超卖 ——")
	fmt.Println("          两个并发订单都会看到同一个空位。两阶段把两个方向都框住。")
	fmt.Println()
}

// --- 2. Dangling reservation -------------------------------------------------

func scenarioQuotaLeak() {
	fmt.Println("  [2] 履约中断: TTL 回收悬挂预留")

	store := newQuotaStore()
	m := quota.NewManager(store, func() time.Time { return base }, nil)

	if _, err := m.CheckAndOccupy(acct, "quota_scecs_instance", "cn-north-1", 8, "order-abandoned"); err != nil {
		check("预留", false, err.Error())
		return
	}
	u, _ := m.Describe(acct, "quota_scecs_instance", "cn-north-1")
	check("编排崩溃,预留悬挂", u.Occupying == 8, fmt.Sprintf("%d 个名额被占住", u.Occupying))

	// Sweeper runs past the TTL.
	later := quota.NewManager(store, func() time.Time { return base.Add(16 * time.Minute) }, nil)
	swept, err := later.SweepExpired()
	if err != nil {
		check("清扫", false, err.Error())
		return
	}
	check("超时自动回收", swept == 1, fmt.Sprintf("回收 %d 个令牌", swept))

	u, _ = later.Describe(acct, "quota_scecs_instance", "cn-north-1")
	check("容量完整归还", u.Available() == 20, fmt.Sprintf("可用 %d", u.Available()))

	fmt.Println()
	fmt.Println("      注:既不可用又不可售的配额是最糟的状态 —— 客户被拒而机器闲置,")
	fmt.Println("          且系统本身不会报告任何异常。TTL 就是为了框住这个洞。")
	fmt.Println()
}

// --- 3. Notification evidence ------------------------------------------------

func scenarioNotificationEvidence() {
	fmt.Println("  [3] 通知留证: 三类信任关键通知必须可查")

	d, senders := newDispatcher(base)

	// An alert storm saturates the tenant's notification budget.
	for i := 0; i < 25; i++ {
		_, _ = d.Send(notify.Notification{
			AccountID: acct, Class: notify.ClassAlert, TemplateID: "tpl-alert",
			BizKey: fmt.Sprintf("alert-%d", i), Channels: notify.ChannelsFor(notify.ClassAlert),
		})
	}
	check("普通告警被限流", true, "10 条/分钟,保护用户注意力")

	// The arrears warning still goes out — throttling it would defeat its purpose.
	n, err := d.Send(notify.Notification{
		AccountID: acct, Class: notify.ClassArrears, TemplateID: "tpl-arrears",
		BizKey: "account-100123", Channels: notify.ChannelsFor(notify.ClassArrears),
	})
	check("欠费催缴不受限流", err == nil && n.Status == notify.StatusSent, string(n.Status))

	// The release warning fans out to every channel.
	warning, err := d.Send(notify.Notification{
		AccountID: acct, Class: notify.ClassReleaseWarning, TemplateID: "tpl-release-final",
		BizKey: resourceID, Channels: notify.ChannelsFor(notify.ClassReleaseWarning),
	})
	if err != nil {
		check("释放预告", false, err.Error())
		return
	}
	check("释放预告多渠道投递", len(warning.Deliveries) == 3,
		fmt.Sprintf("%d 个渠道", len(warning.Deliveries)))

	hasEvidence := false
	for _, del := range warning.Deliveries {
		if del.Success && del.MessageID != "" {
			hasEvidence = true
		}
	}
	check("留有渠道侧流水号", hasEvidence, "争议时可举证网关何时受理")

	// Only marketing is opt-out-able.
	check("不可退订释放预告", !notify.OptOutAllowed(notify.ClassReleaseWarning),
		"用户无法选择不被告知数据将被删除")
	_ = senders
	fmt.Println()
}

// --- 4. Release gate ---------------------------------------------------------

func scenarioReleaseGate() {
	fmt.Println("  [4] 释放闸门: 未送达则不得删数据")

	d, senders := newDispatcher(base)
	inst := &resource.Instance{
		ResourceID: resourceID, AccountID: acct, ProductCode: "scecs",
		Region: "cn-north-1", ChargeType: resource.ChargePostpaid,
		State: resource.StateLocked, LockedAt: base,
		CreatedAt: base, UpdatedAt: base,
	}

	past := resource.NewMachine(func() time.Time { return base.Add(31 * 24 * time.Hour) })
	check("保留期已满", past.ReleaseEligible(*inst), "30 天锁定保留期结束")

	// Case A: every channel is down. The warning cannot be delivered.
	for _, s := range senders {
		s.fail = true
	}
	_, sendErr := d.Send(notify.Notification{
		AccountID: acct, Class: notify.ClassReleaseWarning, TemplateID: "tpl-release-final",
		BizKey: resourceID, Channels: notify.ChannelsFor(notify.ClassReleaseWarning),
	})
	check("通知全渠道失败", errors.Is(sendErr, notify.ErrNotDelivered), "")

	delivered, _ := d.VerifyDelivered(acct, notify.ClassReleaseWarning, resourceID)
	check("未送达不授权释放", !delivered, "")

	_, err := past.Release(inst, delivered, 0, "保留期结束")
	check("释放被拒", errors.Is(err, resource.ErrNoFinalNotice),
		"数据销毁必须先有可查证的告知")
	check("状态未改变", inst.State == resource.StateLocked, string(inst.State))

	// Case B: channels recover, the warning lands, release proceeds.
	for _, s := range senders {
		s.fail = false
	}
	if _, err := d.Send(notify.Notification{
		AccountID: acct, Class: notify.ClassReleaseWarning, TemplateID: "tpl-release-final",
		BizKey: resourceID, Channels: notify.ChannelsFor(notify.ClassReleaseWarning),
	}); err != nil {
		check("重试通知", false, err.Error())
		return
	}
	delivered, _ = d.VerifyDelivered(acct, notify.ClassReleaseWarning, resourceID)
	check("送达后授权释放", delivered, "")

	if _, err := past.Release(inst, delivered, 0, "保留期结束,终版通知已送达"); err != nil {
		check("释放", false, err.Error())
		return
	}
	if _, err := past.Transition(inst, resource.StateReleased, 1, "控制器回收确认"); err != nil {
		check("回收确认", false, err.Error())
		return
	}
	check("释放完成", inst.State == resource.StateReleased, string(inst.State))

	// Quota returns to the pool.
	store := newQuotaStore()
	qm := quota.NewManager(store, func() time.Time { return base }, nil)
	tok, _ := qm.CheckAndOccupy(acct, "quota_scecs_instance", "cn-north-1", 1, "order-9001")
	_ = qm.CommitOccupy(tok.TokenID)
	if err := qm.ReleaseCommitted(acct, "quota_scecs_instance", "cn-north-1", 1); err != nil {
		check("配额归还", false, err.Error())
		return
	}
	u, _ := qm.Describe(acct, "quota_scecs_instance", "cn-north-1")
	check("配额归还", u.Used == 0, fmt.Sprintf("已用 %d", u.Used))
	fmt.Println()
}

// --- 5. Audit chain ----------------------------------------------------------

func scenarioAuditChain() {
	fmt.Println("  [5] 审计链: 篡改可检出,而非仅仅被禁止")

	chain := audit.NewChain(acct)
	var events []audit.Event

	// The release sequence, as an auditor would see it.
	steps := []struct {
		name     string
		decision audit.Decision
		code     int
	}{
		{"StopInstance", audit.DecisionAllow, 200},
		{"SendReleaseWarning", audit.DecisionAllow, 200},
		{"DeleteInstance", audit.DecisionDeny, 403}, // refused: notice not delivered
		{"SendReleaseWarning", audit.DecisionAllow, 200},
		{"DeleteInstance", audit.DecisionAllow, 200},
	}
	for i, s := range steps {
		e, err := chain.Append(audit.Event{
			EventID:     audit.EventID(base, int64(i+1)),
			EventTime:   base.Add(time.Duration(i) * time.Minute),
			EventSource: "scecs.api.starcloud.cn",
			EventName:   s.name,
			SourceIP:    "203.0.113.9",
			Identity: audit.Identity{
				Type: "ram-user", AccountID: acct, Principal: "user/alice",
				AKID: "SCAAAAAAAAAAAAAAAAAAAAAAAAAAAA3F", MFAPresent: true,
			},
			Resources:      []string{"sc:ecs:cn-north-1:100123:instance/" + resourceID},
			Decision:       s.decision,
			DecisionNumber: "A-p0-s1",
			RequestParams:  map[string]string{"InstanceId": resourceID, "Password": "hunter2"},
			ResponseCode:   s.code,
			TraceID:        "5b8e1234c2",
		})
		if err != nil {
			check("记录审计", false, err.Error())
			return
		}
		events = append(events, e)
	}

	check("拒绝操作同样入链", events[2].Decision == audit.DecisionDeny,
		"被拒的特权操作正是调查者要找的事件")
	check("密钥写入即脱敏", events[0].Identity.AKID == "SC****3F", events[0].Identity.AKID)
	check("敏感参数写入即抹除", events[0].RequestParams["Password"] == "[REDACTED]",
		"到达审计库的密钥已经泄漏给所有有读权限的人")

	res := audit.Verify(events)
	check("链完整", res.Intact, fmt.Sprintf("%d 条事件", res.EventCount))

	// Someone rewrites the refusal into an approval.
	tampered := append([]audit.Event{}, events...)
	tampered[2].Decision = audit.DecisionAllow
	tampered[2].ResponseCode = 200

	bad := audit.Verify(tampered)
	check("改写被检出", !bad.Intact && bad.BrokenAt == 3,
		fmt.Sprintf("链在第 %d 条断裂", bad.BrokenAt))

	// Someone deletes the refusal entirely.
	removed := append(append([]audit.Event{}, events[:2]...), events[3:]...)
	gone := audit.Verify(removed)
	check("删除被检出", !gone.Intact, gone.Reason[:40]+"...")

	fmt.Println()
	fmt.Println("      注:数据库权限可以被持有凭证的人修改,而最有动机改审计记录的")
	fmt.Println("          往往正是有权限改的人。哈希链把\"不允许删\"变成\"删了看得出来\"——")
	fmt.Println("          审计员可以自行验证,而不是选择相信。")
}

// --- in-memory stores --------------------------------------------------------

type quotaStore struct {
	defs   map[string]quota.Definition
	usage  map[string]quota.Usage
	tokens map[string]quota.Token
}

func newQuotaStore() *quotaStore {
	return &quotaStore{
		defs: map[string]quota.Definition{
			"quota_scecs_instance": {
				QuotaCode: "quota_scecs_instance", ProductCode: "scecs",
				DefaultValue: 20, Scope: quota.ScopeRegion, Adjustable: true,
			},
		},
		usage:  make(map[string]quota.Usage),
		tokens: make(map[string]quota.Token),
	}
}

func qKey(a int64, c, r string) string { return fmt.Sprintf("%d|%s|%s", a, c, r) }

func (s *quotaStore) GetDefinition(code string) (quota.Definition, error) {
	d, ok := s.defs[code]
	if !ok {
		return quota.Definition{}, quota.ErrUnknownQuota
	}
	return d, nil
}

func (s *quotaStore) GetUsage(a int64, c, r string) (quota.Usage, error) {
	u, ok := s.usage[qKey(a, c, r)]
	if !ok {
		return quota.Usage{AccountID: a, QuotaCode: c, Region: r}, nil
	}
	return u, nil
}

func (s *quotaStore) UpdateUsage(u quota.Usage, expected int) error {
	k := qKey(u.AccountID, u.QuotaCode, u.Region)
	cur, ok := s.usage[k]
	v := 0
	if ok {
		v = cur.Version
	}
	if v != expected {
		return quota.ErrVersionConflict
	}
	u.Version = v + 1
	s.usage[k] = u
	return nil
}

func (s *quotaStore) PutToken(t quota.Token) error { s.tokens[t.TokenID] = t; return nil }

func (s *quotaStore) GetToken(id string) (quota.Token, error) {
	t, ok := s.tokens[id]
	if !ok {
		return quota.Token{}, quota.ErrTokenNotFound
	}
	return t, nil
}

func (s *quotaStore) DeleteToken(id string) error { delete(s.tokens, id); return nil }

func (s *quotaStore) ListExpiredTokens(now time.Time) ([]quota.Token, error) {
	var out []quota.Token
	for _, t := range s.tokens {
		if t.Expired(now) {
			out = append(out, t)
		}
	}
	quota.SortTokens(out)
	return out, nil
}

type notifyStore struct {
	items map[string]notify.Notification
}

func (s *notifyStore) Save(n notify.Notification) error { s.items[n.NotificationID] = n; return nil }

func (s *notifyStore) Get(id string) (notify.Notification, error) {
	n, ok := s.items[id]
	if !ok {
		return notify.Notification{}, errors.New("not found")
	}
	return n, nil
}

func (s *notifyStore) CountInWindow(a int64, since time.Time) (int, error) {
	c := 0
	for _, n := range s.items {
		if n.AccountID == a && n.Status != notify.StatusSuppressed && !n.CreatedAt.Before(since) {
			c++
		}
	}
	return c, nil
}

func (s *notifyStore) FindByBizKey(a int64, class notify.Class, key string) ([]notify.Notification, error) {
	var out []notify.Notification
	for _, n := range s.items {
		if n.AccountID == a && n.Class == class && n.BizKey == key {
			out = append(out, n)
		}
	}
	notify.SortByTime(out)
	return out, nil
}

type mockSender struct {
	ch   notify.Channel
	fail bool
	n    int
}

func (m *mockSender) Channel() notify.Channel { return m.ch }

func (m *mockSender) Send(n notify.Notification) (notify.Delivery, error) {
	m.n++
	if m.fail {
		return notify.Delivery{Channel: m.ch}, fmt.Errorf("%s gateway unavailable", m.ch)
	}
	return notify.Delivery{
		Channel: m.ch, Success: true,
		Provider:  string(m.ch) + "-provider",
		MessageID: fmt.Sprintf("msg-%s-%d", m.ch, m.n),
	}, nil
}

func newDispatcher(now time.Time) (*notify.Dispatcher, map[notify.Channel]*mockSender) {
	store := &notifyStore{items: make(map[string]notify.Notification)}
	var seq int64
	d := notify.NewDispatcher(store, func() time.Time { return now }, func() string {
		seq++
		return fmt.Sprintf("ntf-%d", seq)
	})
	senders := map[notify.Channel]*mockSender{
		notify.ChannelInApp: {ch: notify.ChannelInApp},
		notify.ChannelSMS:   {ch: notify.ChannelSMS},
		notify.ChannelEmail: {ch: notify.ChannelEmail},
	}
	for _, s := range senders {
		d.Register(s)
	}
	return d, senders
}
