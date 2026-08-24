package workflow

import (
	"errors"
	"testing"
	"time"
)

// Retries must be spaced by Backoff, and the delays must be requestable via an
// injected Sleep so callers (and tests) control the actual waiting.
func TestRetriesUseBackoffViaInjectedSleep(t *testing.T) {
	e := NewEngine(func() time.Time { return testNow })
	var delays []time.Duration
	e.Sleep = func(d time.Duration) { delays = append(delays, d) }

	fail := errors.New("down")
	res := e.Run(1, &Context{}, []Step{{Name: "s1", Do: func(*Context) error { return fail }}})
	if res.Status != FlowCompensated {
		t.Fatalf("status = %s", res.Status)
	}
	// MaxStepAttempts=3 → sleeps before attempts 2 and 3: Backoff(1), Backoff(2).
	want := []time.Duration{Backoff(1), Backoff(2)}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delay[%d] = %v, want %v", i, delays[i], want[i])
		}
	}
}

// A failed compensation must be persisted through OnStep: an unemitted failed
// compensation is invisible to the operator who has to clean it up.
func TestCompensationFailureIsEmitted(t *testing.T) {
	e := newEngine()
	var emitted []StepRecord
	e.OnStep = func(_ int64, rec StepRecord) { emitted = append(emitted, rec) }

	res := e.Run(7, &Context{}, []Step{
		{
			Name: "create",
			Do:   func(*Context) error { return nil },
			Undo: func(*Context) error { return errors.New("undo broken") },
		},
		{
			Name: "boom",
			Do:   func(*Context) error { return errors.New("fail") },
		},
	})
	if res.Status != FlowFailed || !res.NeedsTicket {
		t.Fatalf("result = %+v", res)
	}
	found := false
	for _, rec := range emitted {
		if rec.Name == "create" && rec.Status == StepFailed && rec.Error != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed compensation was not emitted; got %+v", emitted)
	}
}
