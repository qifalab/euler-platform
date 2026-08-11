// Package resource implements the platform-wide resource lifecycle state
// machine (03-backend-services.md §5.2/§5.4, 01-product-catalog.md §5.4 D8).
//
// # Single writer
//
// svc-orchestrator is the sole owner of this state machine and the sole writer
// of the resource_instance ledger (adjudication S21). The K8s CR `status.phase`
// is an OBSERVATION field, never the source of truth: a controller reporting
// "Running" is evidence, not authority. Letting two systems both claim to own
// the state is how a resource ends up billed as running while its CR says it
// was deleted an hour ago.
//
// # Every transition produces exactly two things
//
//	1. a resource_instance row update, guarded by (status, version)
//	2. a cloud.resource.lifecycle.event, written to the outbox in the SAME
//	   transaction (03§8.2)
//
// Metering, billing, notification, audit, org counters and console search all
// consume that event. If the row moved without the event, billing silently
// stops; if the event fired without the row, billing charges for a resource
// that does not exist. The outbox is what makes the pair atomic.
//
// # Billing starts at RUNNING, not at request
//
// The billing clock starts when the resource actually reaches RUNNING (03§5.3),
// not when the create request arrived. A customer whose provisioning took four
// minutes should not pay for those four minutes — and when they ask why the
// bill starts where it does, "when your machine became usable" is an answer
// that survives the conversation.
//
// # Arrears and expiry are the trust boundary
//
// 对标启示 8: the lifecycle state machine and its data-retention policy are the
// floor of commercial credibility. Every stage notifies, every deletion is
// preceded by a final warning, and no data is destroyed without an audit trail.
// The parameters come from 01§5.4 D8 and are Nacos-configurable, never
// hard-coded here.
package resource

import (
	"errors"
	"fmt"
	"time"
)

// State is the resource lifecycle state (03§5.2).
type State string

const (
	StateInit         State = "INIT"          // 履约单生成,尚未下发
	StateCreating     State = "CREATING"      // 已下发,等待控制器回调
	StateRunning      State = "RUNNING"       // 正常服务;计费起点
	StateStopped      State = "STOPPED"       // 用户主动停止
	StateUpgrading    State = "UPGRADING"     // 变配中
	StateLocked       State = "LOCKED"        // 欠费停服锁定,数据保留
	StateExpired      State = "EXPIRED"       // 包年包月到期
	StateReleasing    State = "RELEASING"     // 回收中
	StateReleased     State = "RELEASED"      // 已释放(终态)
	StateCreateFailed State = "CREATE_FAILED" // 创建失败(终态,已回滚)
)

// Terminal reports whether no further transition is possible. Terminal rows are
// retained for audit and cost traceability, not deleted (03§5.2 rule 3).
func (s State) Terminal() bool {
	return s == StateReleased || s == StateCreateFailed
}

// Billable reports whether the resource accrues charges in this state.
//
// STOPPED still bills for storage in most cloud models, but phase-1 keeps the
// rule simple and defensible: only RUNNING and UPGRADING accrue. A customer who
// stopped their instance and still sees charges will open a ticket, and "your
// disk is still allocated" is a conversation to have deliberately in phase 2
// with a separate storage meter, not to stumble into now.
func (s State) Billable() bool {
	return s == StateRunning || s == StateUpgrading
}

// Intermediate reports whether the state is a transient one that must not be
// left indefinitely. Each has a timeout backstop (§Timeouts).
func (s State) Intermediate() bool {
	return s == StateCreating || s == StateUpgrading || s == StateReleasing
}

// transitions is the complete legal transition table (03§5.2 state diagram).
// Anything absent is rejected: an unlisted transition is a bug, and allowing
// it silently produces states no downstream consumer knows how to handle.
var transitions = map[State][]State{
	StateInit:      {StateCreating, StateCreateFailed},
	StateCreating:  {StateRunning, StateCreateFailed},
	StateRunning:   {StateStopped, StateUpgrading, StateLocked, StateExpired, StateReleasing},
	StateStopped:   {StateRunning, StateLocked, StateExpired, StateReleasing},
	StateUpgrading: {StateRunning, StateReleasing},
	// Recharging unlocks; retention expiry releases.
	StateLocked: {StateRunning, StateReleasing},
	// Renewing within the retention window restores; retention expiry releases.
	StateExpired:      {StateRunning, StateReleasing},
	StateReleasing:    {StateReleased},
	StateCreateFailed: {StateReleasing}, // rollback cleanup
	StateReleased:     {},               // terminal
}

// CanTransition reports whether from → to is legal.
func CanTransition(from, to State) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Timeouts for intermediate states (03§5.2 rule 2). A resource stuck in an
// intermediate state is either a lost callback or a genuinely failed operation;
// either way it must not sit there forever consuming quota.
var Timeouts = map[State]time.Duration{
	StateCreating:  15 * time.Minute,
	StateUpgrading: 30 * time.Minute,
	StateReleasing: 30 * time.Minute,
}

// PollInterval is how often svc-orchestrator polls rc.QueryStatus for
// resources in an intermediate state (03§5.3). Callback and poll are
// double-insurance: whichever arrives first wins, and because transitions are
// guarded by (status, version) the loser is a harmless no-op.
const PollInterval = 30 * time.Second

// ChargeType mirrors the billing form, kept here to avoid importing pricing
// into the resource domain (the resource plane must not depend on the money
// plane — 03§4.3.4: rc-* 控制器不感知"钱").
type ChargeType string

const (
	ChargePrepaid  ChargeType = "PREPAID"
	ChargePostpaid ChargeType = "POSTPAID"
)

// LifecyclePolicy holds the arrears and expiry parameters. The values come from
// 01§5.4 D8 and are Nacos product-level configuration
// (lifecycle.overdue.grace_hours etc.), NOT constants — 03§5.4 is explicit that
// two sets of defaults must not coexist. Defaults here mirror D8 so a test or a
// dev environment behaves like production.
type LifecyclePolicy struct {
	// OverdueGrace is how long a postpaid resource keeps serving after the
	// account falls into arrears. Service continues during this window; the
	// customer is dunned every DunningInterval.
	OverdueGrace time.Duration
	// OverdueGraceVIP is the extended window for contract customers.
	OverdueGraceVIP time.Duration
	// DunningInterval is the gap between arrears reminders during the grace
	// window.
	DunningInterval time.Duration
	// LockedRetention is how long a locked resource keeps its data before
	// release. This is the single most trust-sensitive number on the platform:
	// it is the promise that falling behind on a bill does not destroy data.
	LockedRetention time.Duration
	// PrepaidRetention is how long an expired subscription keeps its data.
	PrepaidRetention time.Duration
	// RenewRemindDays are the days before expiry on which to remind.
	RenewRemindDays []int
	// LockedRemindDays are the days before release on which to warn.
	LockedRemindDays []int
	// FinalNoticeLead is how long before actual deletion the last warning goes
	// out. Nothing is ever deleted without this notice having been sent and
	// recorded (03§5.4).
	FinalNoticeLead time.Duration
}

// DefaultPolicy returns the D8 parameter set (01§5.4).
func DefaultPolicy() LifecyclePolicy {
	return LifecyclePolicy{
		OverdueGrace:     24 * time.Hour,
		OverdueGraceVIP:  72 * time.Hour,
		DunningInterval:  12 * time.Hour,
		LockedRetention:  30 * 24 * time.Hour,
		PrepaidRetention: 15 * 24 * time.Hour,
		RenewRemindDays:  []int{30, 15, 7, 3, 1},
		LockedRemindDays: []int{7, 3, 1},
		FinalNoticeLead:  24 * time.Hour,
	}
}

// Instance is the platform-wide resource record (03§6.2 resource_instance).
// Product-private attributes live in resource_instance_attr or the CR; this
// struct holds only what every product shares, which is what lets the console,
// billing, audit and quota systems work without per-product special cases.
type Instance struct {
	ResourceID  string // {productCode}-{regionId}-{shard2}-{random8}
	AccountID   int64  // shard key
	ProjectID   int64
	ProductCode string
	ResourceType string
	Region      string
	Zone        string
	ChargeType  ChargeType
	State       State
	SpecCode    string
	OrderID     int64

	// BillingStart is the moment the resource reached RUNNING — the billing
	// clock origin (03§5.3).
	BillingStart time.Time
	// ExpiredAt is the prepaid expiry moment.
	ExpiredAt time.Time
	// LockedAt is when arrears locking took effect; retention counts from here.
	LockedAt   time.Time
	ReleasedAt time.Time

	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Event is emitted on every transition, to cloud.resource.lifecycle.event
// (partition key resource_id, 04§5.4). It is the hottest topic on the platform.
type Event struct {
	ResourceID   string
	AccountID    int64
	ProductCode  string
	FromState    State
	ToState      State
	ChargeType   ChargeType
	BillingStart time.Time
	ExpiredAt    time.Time
	LockedAt     time.Time
	ReleasedAt   time.Time
	Reason       string
	OccurredAt   time.Time
}

// StartsBilling reports whether this transition begins the billing clock.
// svc-metering registers the resource for metering on exactly this event; any
// other trigger risks charging for something that never came up.
func (e Event) StartsBilling() bool {
	return e.ToState == StateRunning && e.FromState == StateCreating
}

// StopsBilling reports whether this transition ends chargeable service.
func (e Event) StopsBilling() bool {
	return e.FromState.Billable() && !e.ToState.Billable()
}

// Errors.
var (
	ErrInvalidTransition = errors.New("resource: illegal state transition")
	ErrVersionConflict   = errors.New("resource: version conflict (concurrent update)")
	ErrNotIntermediate   = errors.New("resource: state has no timeout")
	ErrNoFinalNotice     = errors.New("resource: cannot release without the final notice having been sent")
)

// Machine applies lifecycle transitions. It is stateless; the caller loads the
// instance, applies a transition, and persists the result with its event in one
// transaction.
type Machine struct {
	Now    func() time.Time
	Policy LifecyclePolicy
}

// NewMachine builds a Machine with the D8 default policy.
func NewMachine(now func() time.Time) *Machine {
	if now == nil {
		now = time.Now
	}
	return &Machine{Now: now, Policy: DefaultPolicy()}
}

// Transition moves the instance to a new state under the optimistic lock.
//
// expectedVersion mirrors the SQL guard `WHERE status = ? AND version = ?`.
// A duplicate controller callback finds the row already advanced, affects zero
// rows, and is therefore idempotent by construction — which is what makes the
// callback/polling double-insurance safe (03§5.3).
func (m *Machine) Transition(inst *Instance, to State, expectedVersion int, reason string) (Event, error) {
	if inst.Version != expectedVersion {
		return Event{}, fmt.Errorf("%w: %s at version %d, caller expected %d",
			ErrVersionConflict, inst.ResourceID, inst.Version, expectedVersion)
	}
	if !CanTransition(inst.State, to) {
		return Event{}, fmt.Errorf("%w: %s → %s (%s)",
			ErrInvalidTransition, inst.State, to, inst.ResourceID)
	}

	from := inst.State
	now := m.Now()
	inst.State = to
	inst.Version++
	inst.UpdatedAt = now

	// Stamp the moments that later stages depend on.
	switch to {
	case StateRunning:
		// The billing clock starts on the FIRST arrival at RUNNING and is never
		// reset — an unlock or a restart must not restart billing from zero,
		// or a customer could cycle stop/start to avoid charges.
		if inst.BillingStart.IsZero() {
			inst.BillingStart = now
		}
		// Recovering from arrears or expiry clears those markers.
		inst.LockedAt = time.Time{}
	case StateLocked:
		inst.LockedAt = now
	case StateReleased:
		inst.ReleasedAt = now
	}

	return Event{
		ResourceID:   inst.ResourceID,
		AccountID:    inst.AccountID,
		ProductCode:  inst.ProductCode,
		FromState:    from,
		ToState:      to,
		ChargeType:   inst.ChargeType,
		BillingStart: inst.BillingStart,
		ExpiredAt:    inst.ExpiredAt,
		LockedAt:     inst.LockedAt,
		ReleasedAt:   inst.ReleasedAt,
		Reason:       reason,
		OccurredAt:   now,
	}, nil
}

// TimedOut reports whether an instance has exceeded its intermediate-state
// timeout and should be forced to a failure state.
func (m *Machine) TimedOut(inst Instance) (bool, error) {
	timeout, ok := Timeouts[inst.State]
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrNotIntermediate, inst.State)
	}
	return m.Now().Sub(inst.UpdatedAt) > timeout, nil
}

// ArrearsDeadline returns when a resource in arrears must be locked: the moment
// arrears began plus the grace window. vip selects the extended contract window.
func (m *Machine) ArrearsDeadline(overdueSince time.Time, vip bool) time.Time {
	grace := m.Policy.OverdueGrace
	if vip {
		grace = m.Policy.OverdueGraceVIP
	}
	return overdueSince.Add(grace)
}

// ReleaseDeadline returns when a LOCKED or EXPIRED resource becomes eligible
// for release. Zero means the state has no retention clock.
func (m *Machine) ReleaseDeadline(inst Instance) time.Time {
	switch inst.State {
	case StateLocked:
		if inst.LockedAt.IsZero() {
			return time.Time{}
		}
		return inst.LockedAt.Add(m.Policy.LockedRetention)
	case StateExpired:
		if inst.ExpiredAt.IsZero() {
			return time.Time{}
		}
		return inst.ExpiredAt.Add(m.Policy.PrepaidRetention)
	default:
		return time.Time{}
	}
}

// ReleaseEligible reports whether the retention window has elapsed.
func (m *Machine) ReleaseEligible(inst Instance) bool {
	deadline := m.ReleaseDeadline(inst)
	if deadline.IsZero() {
		return false
	}
	return !m.Now().Before(deadline)
}

// FinalNoticeDue reports whether the last-warning notification should be sent
// now: the release deadline is within FinalNoticeLead.
func (m *Machine) FinalNoticeDue(inst Instance) bool {
	deadline := m.ReleaseDeadline(inst)
	if deadline.IsZero() {
		return false
	}
	noticeAt := deadline.Add(-m.Policy.FinalNoticeLead)
	return !m.Now().Before(noticeAt) && m.Now().Before(deadline)
}

// Release moves a resource to RELEASING after verifying the final notice went
// out.
//
// finalNoticeSent is not advisory. 03§5.4 and 对标启示 8 make the notice a
// precondition for deletion: data is never destroyed without a recorded,
// queryable warning having been delivered first. Passing false here is how a
// customer loses data they were never told they were about to lose, so the
// check is enforced rather than documented.
func (m *Machine) Release(inst *Instance, finalNoticeSent bool, expectedVersion int, reason string) (Event, error) {
	if !finalNoticeSent {
		return Event{}, fmt.Errorf("%w: %s", ErrNoFinalNotice, inst.ResourceID)
	}
	return m.Transition(inst, StateReleasing, expectedVersion, reason)
}

// RenewRemindDue reports whether a renewal reminder falls due today for a
// prepaid resource, and which reminder it is (days before expiry).
func (m *Machine) RenewRemindDue(inst Instance) (int, bool) {
	if inst.ChargeType != ChargePrepaid || inst.ExpiredAt.IsZero() {
		return 0, false
	}
	remaining := inst.ExpiredAt.Sub(m.Now())
	if remaining < 0 {
		return 0, false
	}
	days := int(remaining.Hours() / 24)
	for _, d := range m.Policy.RenewRemindDays {
		if days == d {
			return d, true
		}
	}
	return 0, false
}

// Extend pushes out a prepaid expiry after a successful renewal. It does not
// change state: a RUNNING resource stays RUNNING; an EXPIRED one is restored by
// a separate transition, because the restore has to emit its own event for
// billing to resume.
func (inst *Instance) Extend(by time.Duration) {
	base := inst.ExpiredAt
	if base.IsZero() {
		return
	}
	inst.ExpiredAt = base.Add(by)
}
