package resource

import (
	"errors"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func newMachineAt(t time.Time) *Machine {
	return NewMachine(func() time.Time { return t })
}

func newInstance(state State) *Instance {
	return &Instance{
		ResourceID:   "scecs-cn-north-1-01-a1b2c3d4",
		AccountID:    100123,
		ProductCode:  "scecs",
		ResourceType: "instance",
		Region:       "cn-north-1",
		Zone:         "cn-north-1-a",
		ChargeType:   ChargePostpaid,
		State:        state,
		OrderID:      9001,
		Version:      0,
		CreatedAt:    base,
		UpdatedAt:    base,
	}
}

// --- Happy path ---

func TestProvisioningPath(t *testing.T) {
	m := newMachineAt(base)
	inst := newInstance(StateInit)

	evt, err := m.Transition(inst, StateCreating, 0, "dispatched to rc-compute")
	if err != nil {
		t.Fatal(err)
	}
	if evt.StartsBilling() {
		t.Error("CREATING must not start billing")
	}

	// The controller reports the resource is up four minutes later.
	m4 := newMachineAt(base.Add(4 * time.Minute))
	evt, err = m4.Transition(inst, StateRunning, 1, "controller callback")
	if err != nil {
		t.Fatal(err)
	}
	if !evt.StartsBilling() {
		t.Fatal("CREATING → RUNNING must start the billing clock")
	}
	// Billing starts when the machine became usable, not when it was ordered.
	if !inst.BillingStart.Equal(base.Add(4 * time.Minute)) {
		t.Fatalf("BillingStart = %v, want the RUNNING moment %v",
			inst.BillingStart, base.Add(4*time.Minute))
	}
}

// TestBillingClockNeverRestarts guards against a customer cycling stop/start
// (or arrears/recharge) to reset their billing origin.
func TestBillingClockNeverRestarts(t *testing.T) {
	m := newMachineAt(base)
	inst := newInstance(StateCreating)
	if _, err := m.Transition(inst, StateRunning, 0, ""); err != nil {
		t.Fatal(err)
	}
	original := inst.BillingStart

	later := newMachineAt(base.Add(48 * time.Hour))
	if _, err := later.Transition(inst, StateStopped, 1, "user stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := later.Transition(inst, StateRunning, 2, "user start"); err != nil {
		t.Fatal(err)
	}
	if !inst.BillingStart.Equal(original) {
		t.Fatalf("BillingStart moved on restart: %v → %v", original, inst.BillingStart)
	}
}

func TestIllegalTransitionsRejected(t *testing.T) {
	m := newMachineAt(base)
	illegal := []struct {
		from, to State
		why      string
	}{
		{StateInit, StateRunning, "cannot skip provisioning"},
		{StateCreating, StateStopped, "not yet running"},
		{StateReleased, StateRunning, "terminal"},
		{StateReleased, StateReleasing, "terminal"},
		{StateRunning, StateInit, "no going back"},
		{StateRunning, StateReleased, "must pass through RELEASING for controller confirmation"},
		{StateLocked, StateStopped, "locked resources unlock to RUNNING or release"},
		{StateCreateFailed, StateRunning, "failed creation cannot become healthy"},
	}
	for _, c := range illegal {
		inst := newInstance(c.from)
		if _, err := m.Transition(inst, c.to, 0, ""); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s → %s (%s): expected ErrInvalidTransition, got %v",
				c.from, c.to, c.why, err)
		}
		if inst.State != c.from || inst.Version != 0 {
			t.Errorf("%s → %s: rejected transition mutated the instance", c.from, c.to)
		}
	}
}

// --- Idempotency: callback + polling double-insurance ---

func TestDuplicateCallbackIsIdempotent(t *testing.T) {
	// The controller callback and the 30s poll can both report the same
	// transition. Whichever arrives second must be a harmless no-op.
	m := newMachineAt(base)
	inst := newInstance(StateCreating)

	if _, err := m.Transition(inst, StateRunning, 0, "callback"); err != nil {
		t.Fatal(err)
	}
	stateAfter, versionAfter := inst.State, inst.Version

	// The poller, unaware, reports the same thing with the stale version.
	_, err := m.Transition(inst, StateRunning, 0, "poll")
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}
	if inst.State != stateAfter || inst.Version != versionAfter {
		t.Fatal("the losing report mutated the instance")
	}
}

// --- Timeouts ---

func TestIntermediateStateTimeouts(t *testing.T) {
	cases := []struct {
		state   State
		within  time.Duration
		beyond  time.Duration
	}{
		{StateCreating, 14 * time.Minute, 16 * time.Minute},
		{StateUpgrading, 29 * time.Minute, 31 * time.Minute},
		{StateReleasing, 29 * time.Minute, 31 * time.Minute},
	}
	for _, c := range cases {
		inst := newInstance(c.state)
		inst.UpdatedAt = base

		within := newMachineAt(base.Add(c.within))
		if out, err := within.TimedOut(*inst); err != nil || out {
			t.Errorf("%s at %v: should not be timed out (err=%v)", c.state, c.within, err)
		}
		beyond := newMachineAt(base.Add(c.beyond))
		if out, err := beyond.TimedOut(*inst); err != nil || !out {
			t.Errorf("%s at %v: should be timed out (err=%v)", c.state, c.beyond, err)
		}
	}
}

func TestStableStatesHaveNoTimeout(t *testing.T) {
	m := newMachineAt(base.Add(365 * 24 * time.Hour))
	for _, s := range []State{StateRunning, StateStopped, StateLocked, StateExpired} {
		inst := newInstance(s)
		if _, err := m.TimedOut(*inst); !errors.Is(err, ErrNotIntermediate) {
			t.Errorf("%s should have no timeout, got %v", s, err)
		}
	}
}

func TestTimeoutDrivesFailure(t *testing.T) {
	// A creation that never called back is forced to CREATE_FAILED so the
	// quota it holds is returned rather than leaked forever.
	inst := newInstance(StateCreating)
	inst.UpdatedAt = base

	m := newMachineAt(base.Add(20 * time.Minute))
	out, _ := m.TimedOut(*inst)
	if !out {
		t.Fatal("should have timed out")
	}
	if _, err := m.Transition(inst, StateCreateFailed, 0, "timeout: no controller callback"); err != nil {
		t.Fatal(err)
	}
	if inst.State != StateCreateFailed {
		t.Fatalf("state = %s, want CREATE_FAILED", inst.State)
	}
}

// --- Arrears lifecycle (01§5.4 D8) ---

func TestArrearsGraceWindows(t *testing.T) {
	m := newMachineAt(base)
	overdue := base

	if got := m.ArrearsDeadline(overdue, false); !got.Equal(base.Add(24 * time.Hour)) {
		t.Errorf("standard grace = %v, want 24h", got.Sub(base))
	}
	if got := m.ArrearsDeadline(overdue, true); !got.Equal(base.Add(72 * time.Hour)) {
		t.Errorf("VIP grace = %v, want 72h", got.Sub(base))
	}
}

func TestArrearsLockThenRecharge(t *testing.T) {
	// Grace expires → LOCKED (data retained) → customer pays → RUNNING.
	// The whole point is that falling behind on a bill does not destroy data.
	lockTime := base.Add(24 * time.Hour)
	m := newMachineAt(lockTime)
	inst := newInstance(StateRunning)
	inst.BillingStart = base

	if _, err := m.Transition(inst, StateLocked, 0, "arrears grace expired"); err != nil {
		t.Fatal(err)
	}
	if !inst.LockedAt.Equal(lockTime) {
		t.Fatalf("LockedAt = %v, want %v", inst.LockedAt, lockTime)
	}

	// Retention runs 30 days from the lock moment.
	wantDeadline := lockTime.Add(30 * 24 * time.Hour)
	if got := m.ReleaseDeadline(*inst); !got.Equal(wantDeadline) {
		t.Fatalf("release deadline = %v, want %v", got, wantDeadline)
	}
	if m.ReleaseEligible(*inst) {
		t.Fatal("must not be releasable the moment it locks")
	}

	// Customer recharges on day 10.
	pay := newMachineAt(lockTime.Add(10 * 24 * time.Hour))
	if _, err := pay.Transition(inst, StateRunning, 1, "recharged"); err != nil {
		t.Fatal(err)
	}
	if inst.State != StateRunning {
		t.Fatalf("state = %s, want RUNNING", inst.State)
	}
	if !inst.LockedAt.IsZero() {
		t.Fatal("LockedAt should clear on recovery")
	}
	// Billing origin is preserved across the arrears episode.
	if !inst.BillingStart.Equal(base) {
		t.Fatalf("BillingStart moved: %v", inst.BillingStart)
	}
}

func TestLockedRetentionExpiryReleases(t *testing.T) {
	lockTime := base
	inst := newInstance(StateLocked)
	inst.LockedAt = lockTime

	// Day 29: still retained.
	d29 := newMachineAt(lockTime.Add(29 * 24 * time.Hour))
	if d29.ReleaseEligible(*inst) {
		t.Fatal("day 29 must still be within the 30-day retention window")
	}
	// Day 31: eligible.
	d31 := newMachineAt(lockTime.Add(31 * 24 * time.Hour))
	if !d31.ReleaseEligible(*inst) {
		t.Fatal("day 31 should be past the retention window")
	}
}

// --- Prepaid expiry ---

func TestPrepaidExpiryRetention(t *testing.T) {
	expiry := base
	inst := newInstance(StateExpired)
	inst.ChargeType = ChargePrepaid
	inst.ExpiredAt = expiry

	// 15-day retention for prepaid (D8), shorter than the 30-day arrears window.
	d14 := newMachineAt(expiry.Add(14 * 24 * time.Hour))
	if d14.ReleaseEligible(*inst) {
		t.Fatal("day 14 must still be retained")
	}
	d16 := newMachineAt(expiry.Add(16 * 24 * time.Hour))
	if !d16.ReleaseEligible(*inst) {
		t.Fatal("day 16 should be past the 15-day retention window")
	}
}

func TestRenewRestoresFromExpired(t *testing.T) {
	expiry := base
	inst := newInstance(StateExpired)
	inst.ChargeType = ChargePrepaid
	inst.ExpiredAt = expiry

	m := newMachineAt(expiry.Add(5 * 24 * time.Hour))
	inst.Extend(30 * 24 * time.Hour)
	if _, err := m.Transition(inst, StateRunning, 0, "renewed within retention"); err != nil {
		t.Fatal(err)
	}
	if inst.State != StateRunning {
		t.Fatalf("state = %s, want RUNNING", inst.State)
	}
	if !inst.ExpiredAt.Equal(expiry.Add(30 * 24 * time.Hour)) {
		t.Fatalf("expiry not extended: %v", inst.ExpiredAt)
	}
}

func TestRenewReminderSchedule(t *testing.T) {
	// D8: remind at 30/15/7/3/1 days before expiry.
	expiry := base.Add(60 * 24 * time.Hour)
	inst := newInstance(StateRunning)
	inst.ChargeType = ChargePrepaid
	inst.ExpiredAt = expiry

	for _, d := range []int{30, 15, 7, 3, 1} {
		m := newMachineAt(expiry.Add(-time.Duration(d)*24*time.Hour - time.Hour))
		got, due := m.RenewRemindDue(*inst)
		if !due || got != d {
			t.Errorf("at %d days before expiry: due=%v day=%d, want due=true day=%d", d, due, got, d)
		}
	}
	// A day with no scheduled reminder stays quiet — reminder fatigue is real.
	m := newMachineAt(expiry.Add(-20*24*time.Hour - time.Hour))
	if _, due := m.RenewRemindDue(*inst); due {
		t.Error("day 20 is not a scheduled reminder day")
	}
	// Postpaid resources never get renewal reminders.
	post := newInstance(StateRunning)
	post.ChargeType = ChargePostpaid
	post.ExpiredAt = expiry
	m2 := newMachineAt(expiry.Add(-30*24*time.Hour - time.Hour))
	if _, due := m2.RenewRemindDue(*post); due {
		t.Error("postpaid resources have no expiry to remind about")
	}
}

// --- Release requires a final notice ---

func TestReleaseRequiresFinalNotice(t *testing.T) {
	// 对标启示 8 / 03§5.4: data is never destroyed without a recorded warning.
	inst := newInstance(StateLocked)
	inst.LockedAt = base
	m := newMachineAt(base.Add(31 * 24 * time.Hour))

	if _, err := m.Release(inst, false, 0, "retention expired"); !errors.Is(err, ErrNoFinalNotice) {
		t.Fatalf("release without a final notice must be refused, got %v", err)
	}
	if inst.State != StateLocked {
		t.Fatal("refused release must not change state")
	}

	if _, err := m.Release(inst, true, 0, "retention expired"); err != nil {
		t.Fatalf("release after notice should succeed: %v", err)
	}
	if inst.State != StateReleasing {
		t.Fatalf("state = %s, want RELEASING", inst.State)
	}
}

func TestFinalNoticeTiming(t *testing.T) {
	inst := newInstance(StateLocked)
	inst.LockedAt = base
	deadline := base.Add(30 * 24 * time.Hour)

	// Two days out: too early.
	early := newMachineAt(deadline.Add(-48 * time.Hour))
	if early.FinalNoticeDue(inst2(inst)) {
		t.Error("final notice should not fire 48h before release")
	}
	// Twelve hours out: within the 24h lead.
	due := newMachineAt(deadline.Add(-12 * time.Hour))
	if !due.FinalNoticeDue(inst2(inst)) {
		t.Error("final notice should fire within 24h of release")
	}
	// Past the deadline: the window has closed.
	late := newMachineAt(deadline.Add(time.Hour))
	if late.FinalNoticeDue(inst2(inst)) {
		t.Error("final notice window closes at the deadline")
	}
}

func inst2(i *Instance) Instance { return *i }

// --- Billing predicates ---

func TestBillableStates(t *testing.T) {
	billable := []State{StateRunning, StateUpgrading}
	for _, s := range billable {
		if !s.Billable() {
			t.Errorf("%s should be billable", s)
		}
	}
	notBillable := []State{
		StateInit, StateCreating, StateStopped, StateLocked,
		StateExpired, StateReleasing, StateReleased, StateCreateFailed,
	}
	for _, s := range notBillable {
		if s.Billable() {
			t.Errorf("%s must NOT be billable", s)
		}
	}
}

func TestBillingStopEvents(t *testing.T) {
	m := newMachineAt(base)
	// RUNNING → LOCKED stops the meter.
	inst := newInstance(StateRunning)
	evt, err := m.Transition(inst, StateLocked, 0, "arrears")
	if err != nil {
		t.Fatal(err)
	}
	if !evt.StopsBilling() {
		t.Error("RUNNING → LOCKED must stop billing")
	}

	// STOPPED → LOCKED does not, since STOPPED was already not billable.
	inst2 := newInstance(StateStopped)
	evt2, err := m.Transition(inst2, StateLocked, 0, "arrears")
	if err != nil {
		t.Fatal(err)
	}
	if evt2.StopsBilling() {
		t.Error("STOPPED → LOCKED cannot stop billing that was not running")
	}
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []State{StateReleased, StateCreateFailed} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []State{StateRunning, StateLocked, StateExpired, StateReleasing} {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestFullArrearsToReleaseJourney(t *testing.T) {
	// The complete trust-sensitive path, as a customer would experience it.
	inst := newInstance(StateRunning)
	inst.BillingStart = base

	// Day 0: arrears begin, service continues through the grace window.
	overdue := base.Add(10 * 24 * time.Hour)
	m := newMachineAt(overdue)
	if m.ArrearsDeadline(overdue, false) != overdue.Add(24*time.Hour) {
		t.Fatal("grace window wrong")
	}

	// Day 1: grace expires, service stops but data is retained.
	lockAt := overdue.Add(24 * time.Hour)
	mLock := newMachineAt(lockAt)
	evt, err := mLock.Transition(inst, StateLocked, 0, "grace expired")
	if err != nil {
		t.Fatal(err)
	}
	if !evt.StopsBilling() {
		t.Error("locking must stop the meter")
	}

	// Day 31: retention expires, final notice due, then release.
	releaseAt := lockAt.Add(30 * 24 * time.Hour)
	mNotice := newMachineAt(releaseAt.Add(-12 * time.Hour))
	if !mNotice.FinalNoticeDue(*inst) {
		t.Error("final notice should be due 12h before release")
	}

	mRelease := newMachineAt(releaseAt.Add(time.Minute))
	if !mRelease.ReleaseEligible(*inst) {
		t.Fatal("should be releasable past the retention window")
	}
	if _, err := mRelease.Release(inst, true, 1, "retention expired"); err != nil {
		t.Fatal(err)
	}
	if _, err := mRelease.Transition(inst, StateReleased, 2, "controller confirmed"); err != nil {
		t.Fatal(err)
	}
	if inst.State != StateReleased || inst.ReleasedAt.IsZero() {
		t.Fatalf("final state = %s, releasedAt = %v", inst.State, inst.ReleasedAt)
	}
}
