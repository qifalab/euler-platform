package workflow

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func newEngine() *Engine {
	e := NewEngine(func() time.Time { return testNow })
	// Tests must not really sleep; record the requested backoff delays instead.
	e.Sleep = func(time.Duration) {}
	return e
}

// tracker records the order in which actions ran, which is what the
// compensation-ordering assertions inspect.
type tracker struct {
	calls []string
}

func (tr *tracker) step(name string, fail bool) Step {
	return Step{
		Name: name,
		Do: func(ctx *Context) error {
			tr.calls = append(tr.calls, "do:"+name)
			if fail {
				return fmt.Errorf("%s failed", name)
			}
			return nil
		},
		Undo: func(ctx *Context) error {
			tr.calls = append(tr.calls, "undo:"+name)
			return nil
		},
	}
}

// --- Happy path ---

func TestAllStepsRunInOrder(t *testing.T) {
	tr := &tracker{}
	e := newEngine()
	res := e.Run(1, &Context{}, []Step{
		tr.step("check_quota", false),
		tr.step("create_resource", false),
		tr.step("start_metering", false),
		tr.step("confirm_order", false),
	})

	if res.Status != FlowCompleted {
		t.Fatalf("status = %s, want COMPLETED (err: %v)", res.Status, res.Err)
	}
	want := []string{"do:check_quota", "do:create_resource", "do:start_metering", "do:confirm_order"}
	if len(tr.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", tr.calls, want)
	}
	for i := range want {
		if tr.calls[i] != want[i] {
			t.Fatalf("call %d = %s, want %s", i, tr.calls[i], want[i])
		}
	}
	for _, rec := range res.Steps {
		if rec.Status != StepDone {
			t.Errorf("step %s = %s, want DONE", rec.Name, rec.Status)
		}
	}
}

// --- Compensation ordering ---

// TestCompensationRunsInReverseOrder is the core saga guarantee. Quota is
// occupied before the resource is created, so it must be released AFTER the
// resource is deleted — otherwise quota briefly reports free capacity that does
// not physically exist, and a concurrent order can oversell it.
func TestCompensationRunsInReverseOrder(t *testing.T) {
	tr := &tracker{}
	e := newEngine()
	res := e.Run(1, &Context{}, []Step{
		tr.step("check_quota", false),
		tr.step("create_resource", false),
		tr.step("start_metering", true), // fails here
		tr.step("confirm_order", false), // never runs
	})

	if res.Status != FlowCompensated {
		t.Fatalf("status = %s, want COMPENSATED", res.Status)
	}
	if res.FailedStep != 2 {
		t.Fatalf("FailedStep = %d, want 2", res.FailedStep)
	}

	// Forward calls stop at the failure; undo runs in reverse over what
	// completed. start_metering failed, so it is not compensated.
	want := []string{
		"do:check_quota",
		"do:create_resource",
		"do:start_metering", "do:start_metering", "do:start_metering", // 3 attempts
		"undo:create_resource",
		"undo:check_quota",
	}
	if len(tr.calls) != len(want) {
		t.Fatalf("calls = %v\nwant  %v", tr.calls, want)
	}
	for i := range want {
		if tr.calls[i] != want[i] {
			t.Fatalf("call %d = %s, want %s\nfull: %v", i, tr.calls[i], want[i], tr.calls)
		}
	}

	// The step that never ran must not be compensated.
	if res.Steps[3].Status != StepPending {
		t.Errorf("unreached step = %s, want PENDING", res.Steps[3].Status)
	}
}

func TestFirstStepFailureCompensatesNothing(t *testing.T) {
	tr := &tracker{}
	e := newEngine()
	res := e.Run(1, &Context{}, []Step{
		tr.step("check_quota", true),
		tr.step("create_resource", false),
	})

	if res.Status != FlowCompensated {
		t.Fatalf("status = %s", res.Status)
	}
	for _, c := range tr.calls {
		if len(c) > 5 && c[:5] == "undo:" {
			t.Fatalf("nothing completed, so nothing should be compensated; got %v", tr.calls)
		}
	}
}

// --- Retries ---

func TestStepRetriedBeforeCompensating(t *testing.T) {
	attempts := 0
	e := newEngine()
	res := e.Run(1, &Context{}, []Step{
		{
			Name: "flaky",
			Do: func(ctx *Context) error {
				attempts++
				if attempts < 3 {
					return errors.New("transient")
				}
				return nil
			},
			Undo: func(ctx *Context) error { return nil },
		},
	})

	if res.Status != FlowCompleted {
		t.Fatalf("a step that succeeds on attempt 3 should complete the flow, got %s", res.Status)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestStepGivesUpAfterMaxAttempts(t *testing.T) {
	attempts := 0
	e := newEngine()
	res := e.Run(1, &Context{}, []Step{
		{
			Name: "always_fails",
			Do: func(ctx *Context) error {
				attempts++
				return errors.New("permanent")
			},
		},
	})

	if attempts != MaxStepAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, MaxStepAttempts)
	}
	if res.Status != FlowCompensated {
		t.Fatalf("status = %s", res.Status)
	}
	if !errors.Is(res.Err, ErrStepFailed) {
		t.Fatalf("Err = %v, want ErrStepFailed", res.Err)
	}
}

// --- Compensation failure escalates ---

// TestCompensationFailureRaisesTicket covers 03§8.4: 补偿三次失败转人工工单.
// An infinite retry against a permanently broken downstream is
// indistinguishable from an outage, so the engine stops and asks for a human.
func TestCompensationFailureRaisesTicket(t *testing.T) {
	undoAttempts := 0
	e := newEngine()
	res := e.Run(1, &Context{}, []Step{
		{
			Name: "create_resource",
			Do:   func(ctx *Context) error { return nil },
			Undo: func(ctx *Context) error {
				undoAttempts++
				return errors.New("controller unreachable")
			},
		},
		{
			Name: "start_metering",
			Do:   func(ctx *Context) error { return errors.New("boom") },
		},
	})

	if undoAttempts != MaxCompensationAttempts {
		t.Fatalf("compensation attempts = %d, want %d", undoAttempts, MaxCompensationAttempts)
	}
	if res.Status != FlowFailed {
		t.Fatalf("status = %s, want FAILED (engine could not restore a clean state)", res.Status)
	}
	if !res.NeedsTicket {
		t.Fatal("a failed compensation must raise an operations ticket")
	}
	if len(res.CompensationErrors) != 1 {
		t.Fatalf("CompensationErrors = %v, want 1", res.CompensationErrors)
	}
}

// TestCompensationContinuesAfterOneFails checks that a broken compensation does
// not abandon the remaining cleanup. Halting at the first problem leaves more
// debris than trying everything and reporting what could not be fixed.
func TestCompensationContinuesAfterOneFails(t *testing.T) {
	var undone []string
	e := newEngine()
	res := e.Run(1, &Context{}, []Step{
		{
			Name: "occupy_quota",
			Do:   func(ctx *Context) error { return nil },
			Undo: func(ctx *Context) error {
				undone = append(undone, "occupy_quota")
				return nil
			},
		},
		{
			Name: "create_resource",
			Do:   func(ctx *Context) error { return nil },
			Undo: func(ctx *Context) error {
				undone = append(undone, "create_resource")
				return errors.New("controller unreachable")
			},
		},
		{
			Name: "start_metering",
			Do:   func(ctx *Context) error { return errors.New("boom") },
		},
	})

	// create_resource compensation fails, but occupy_quota must still be
	// attempted afterwards.
	foundQuota := false
	for _, u := range undone {
		if u == "occupy_quota" {
			foundQuota = true
		}
	}
	if !foundQuota {
		t.Fatalf("quota compensation skipped after an earlier failure: %v", undone)
	}
	if !res.NeedsTicket {
		t.Fatal("the failed compensation should still raise a ticket")
	}
}

// --- Nil Undo ---

func TestNilUndoIsSkippedNotAnError(t *testing.T) {
	// A pure read, or an event emission downstream already handles
	// idempotently, has nothing to reverse.
	e := newEngine()
	res := e.Run(1, &Context{}, []Step{
		{Name: "read_only", Do: func(ctx *Context) error { return nil }}, // no Undo
		{Name: "fails", Do: func(ctx *Context) error { return errors.New("boom") }},
	})

	if res.Status != FlowCompensated {
		t.Fatalf("status = %s, want COMPENSATED", res.Status)
	}
	if res.NeedsTicket {
		t.Fatal("a nil Undo is not a compensation failure")
	}
	if res.Steps[0].Status != StepCompensated {
		t.Errorf("step with nil Undo = %s, want COMPENSATED", res.Steps[0].Status)
	}
}

// --- Context passing ---

func TestContextCarriesDataBetweenSteps(t *testing.T) {
	e := newEngine()
	ctx := &Context{AccountID: 100123, BizKey: "order-9001"}

	res := e.Run(1, ctx, []Step{
		{
			Name: "create_resource",
			Do: func(c *Context) error {
				c.Set("resource_id", "scecs-cn-north-1-01-a1b2c3d4")
				return nil
			},
		},
		{
			Name: "start_metering",
			Do: func(c *Context) error {
				id := c.GetString("resource_id")
				if id != "scecs-cn-north-1-01-a1b2c3d4" {
					return fmt.Errorf("resource_id not carried forward: %q", id)
				}
				return nil
			},
		},
	})
	if res.Status != FlowCompleted {
		t.Fatalf("status = %s, err = %v", res.Status, res.Err)
	}
}

func TestContextAvailableDuringCompensation(t *testing.T) {
	// The compensation needs to know what to undo, which means reading what
	// the forward action produced.
	var deletedID string
	e := newEngine()
	res := e.Run(1, &Context{}, []Step{
		{
			Name: "create_resource",
			Do: func(c *Context) error {
				c.Set("resource_id", "scecs-cn-north-1-01-deadbeef")
				return nil
			},
			Undo: func(c *Context) error {
				deletedID = c.GetString("resource_id")
				return nil
			},
		},
		{Name: "fails", Do: func(c *Context) error { return errors.New("boom") }},
	})

	if res.Status != FlowCompensated {
		t.Fatalf("status = %s", res.Status)
	}
	if deletedID != "scecs-cn-north-1-01-deadbeef" {
		t.Fatalf("compensation could not see the created resource id, got %q", deletedID)
	}
}

// --- Idempotency expectations ---

func TestAtLeastOnceDeliveryMeansActionsRunTwice(t *testing.T) {
	// The engine retries, so an action can legitimately run more than once
	// with the same input. This test documents that contract: an action that
	// is not idempotent will be run twice anyway.
	runs := 0
	e := newEngine()
	e.Run(1, &Context{}, []Step{
		{
			Name: "not_idempotent",
			Do: func(c *Context) error {
				runs++
				if runs == 1 {
					return errors.New("transient")
				}
				return nil
			},
		},
	})
	if runs < 2 {
		t.Fatalf("engine should have retried; runs = %d", runs)
	}
}

// --- Empty flow ---

func TestEmptyFlowRejected(t *testing.T) {
	e := newEngine()
	res := e.Run(1, &Context{}, nil)
	if !errors.Is(res.Err, ErrNoSteps) {
		t.Fatalf("Err = %v, want ErrNoSteps", res.Err)
	}
	if res.Status != FlowFailed {
		t.Fatalf("status = %s", res.Status)
	}
}

// --- Backoff ---

func TestBackoffGrowsAndCaps(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{10, 5 * time.Minute},  // capped
		{100, 5 * time.Minute}, // still capped, no overflow
		{0, 1 * time.Second},   // degenerate input
	}
	for _, c := range cases {
		if got := Backoff(c.attempt); got != c.want {
			t.Errorf("Backoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

// TestBackoffNeverZero guards the overflow that made a long-failing flow retry
// with no delay at all: 1<<99 wraps time.Duration to zero, turning backoff into
// a tight loop against an already-broken downstream.
func TestBackoffNeverZero(t *testing.T) {
	for _, attempt := range []int{1, 5, 31, 32, 33, 63, 64, 100, 1000, 1 << 20} {
		got := Backoff(attempt)
		if got <= 0 {
			t.Errorf("Backoff(%d) = %v — must always be a positive delay", attempt, got)
		}
		if got > 5*time.Minute {
			t.Errorf("Backoff(%d) = %v — exceeds the cap", attempt, got)
		}
	}
}

func TestNextFireAt(t *testing.T) {
	e := newEngine()
	if got := e.NextFireAt(3); !got.Equal(testNow.Add(4 * time.Second)) {
		t.Fatalf("NextFireAt(3) = %v, want %v", got, testNow.Add(4*time.Second))
	}
}

// --- Step observation hook ---

func TestOnStepHookReceivesEveryStep(t *testing.T) {
	// The production engine persists step_instance rows through this hook, so
	// a missing callback means a step that cannot be investigated later.
	var seen []string
	e := newEngine()
	e.OnStep = func(flowID int64, rec StepRecord) {
		seen = append(seen, rec.Name+":"+string(rec.Status))
	}
	e.Run(42, &Context{}, []Step{
		{Name: "a", Do: func(c *Context) error { return nil }},
		{Name: "b", Do: func(c *Context) error { return nil }},
	})
	if len(seen) != 2 {
		t.Fatalf("hook saw %v, want both steps", seen)
	}
	for _, s := range seen {
		if s[len(s)-4:] != "DONE" {
			t.Errorf("unexpected step record: %s", s)
		}
	}
}
