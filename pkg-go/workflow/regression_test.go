package workflow

import (
	"errors"
	"testing"
	"time"
)

// Every compensation result must reach OnStep (step_instance). The nil-Undo
// branch used to flip the in-memory record to COMPENSATED without emitting,
// so an operator reading step_instance saw a step as DONE while the flow had
// already rolled it back.
func TestNilUndoCompensationIsEmitted(t *testing.T) {
	e := NewEngine(func() time.Time { return time.Unix(0, 0) })
	var emitted []StepRecord
	e.OnStep = func(_ int64, rec StepRecord) { emitted = append(emitted, rec) }

	steps := []Step{
		// A pure read: nothing to reverse, so Undo is nil by design.
		{Name: "read", Do: func(*Context) error { return nil }},
		{Name: "create", Do: func(*Context) error { return nil }, Undo: func(*Context) error { return nil }},
		{Name: "fail", Do: func(*Context) error { return errors.New("boom") }},
	}
	res := e.Run(7, &Context{FlowInstanceID: 7, AccountID: 100123, Values: map[string]any{}}, steps)
	if res.Status != FlowCompensated {
		t.Fatalf("status = %s, want COMPENSATED", res.Status)
	}

	found := false
	for _, rec := range emitted {
		if rec.Name == "read" && rec.Status == StepCompensated {
			found = true
		}
	}
	if !found {
		t.Fatal("the nil-Undo step's compensation was never emitted to step_instance")
	}
}
