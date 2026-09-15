// Package workflow implements the platform's lightweight saga engine
// (03-backend-services.md §4.3.3, §8.4).
//
// # Why self-built
//
// 03§4.3.3 forbids introducing Temporal or Camunda. The platform needs step
// sequencing with compensation, not a BPMN engine, and a heavyweight workflow
// product is one more stateful system for a six-person SRE team to operate.
// What is actually required fits in this file: ordered steps, each with a
// forward action and a compensating action, at-least-once dispatch, and a
// timer driven by scanning next_fire_at.
//
// # Compensation, not rollback
//
// Distributed steps cannot be rolled back — a created VM does not un-exist
// because a later step failed. Each step therefore registers a compensating
// action, and on failure the engine runs the compensations of the completed
// steps in REVERSE order (03§8.4). Reverse order matters: quota was occupied
// before the resource was created, so it must be released after the resource
// is deleted, or the quota briefly shows free capacity that does not exist.
//
// # Compensation must itself be idempotent
//
// Compensations are delivered at-least-once like everything else, and a
// compensation that fails is retried. `rc.DeleteResource` on an
// already-deleted resource must succeed, not error. After three failed
// attempts the flow escalates to a human ticket rather than retrying forever
// (03§8.4) — an infinite retry loop against a permanently broken downstream is
// indistinguishable from an outage.
//
// # Refunds are not compensations
//
// 03§8.4 is explicit: 退款是独立流程而非补偿动作. Releasing a resource and
// refunding the money are two flows in sequence, because refunding as a
// compensation step would return money while the release is still in flight.
package workflow

import (
	"errors"
	"fmt"
	"time"
)

// StepStatus is the state of a single step.
type StepStatus string

const (
	StepPending     StepStatus = "PENDING"
	StepRunning     StepStatus = "RUNNING"
	StepDone        StepStatus = "DONE"
	StepFailed      StepStatus = "FAILED"
	StepCompensated StepStatus = "COMPENSATED"
)

// FlowStatus is the state of a flow instance.
type FlowStatus string

const (
	FlowRunning      FlowStatus = "RUNNING"
	FlowCompleted    FlowStatus = "COMPLETED"
	FlowCompensating FlowStatus = "COMPENSATING"
	FlowCompensated  FlowStatus = "COMPENSATED"
	FlowFailed       FlowStatus = "FAILED" // compensation itself failed; needs a human
)

// MaxCompensationAttempts is how many times a compensation is retried before
// the flow escalates to a ticket (03§8.4).
const MaxCompensationAttempts = 3

// MaxStepAttempts is how many times a forward step is retried before the flow
// starts compensating.
const MaxStepAttempts = 3

// Action performs a step. It must be idempotent: the engine delivers
// at-least-once, so an action may legitimately run twice with the same input.
type Action func(ctx *Context) error

// Step pairs a forward action with its compensation.
type Step struct {
	Name string
	// Do performs the forward operation.
	Do Action
	// Undo reverses it. It runs only if Do previously completed. It must be
	// idempotent and must tolerate the operation having already been undone —
	// deleting an already-deleted resource is a success, not an error.
	//
	// A nil Undo marks a step with nothing to reverse (a pure read, or an
	// event emission that downstream consumers already handle idempotently).
	Undo Action
}

// Context carries data between steps. Steps read what earlier steps produced
// and write what later ones need.
type Context struct {
	FlowInstanceID int64
	AccountID      int64
	BizKey         string
	Values         map[string]any
	// Attempt is the current attempt number for the running step, starting at 1.
	Attempt int
}

// Set stores a value for later steps.
func (c *Context) Set(key string, v any) {
	if c.Values == nil {
		c.Values = make(map[string]any)
	}
	c.Values[key] = v
}

// Get retrieves a value produced by an earlier step.
func (c *Context) Get(key string) (any, bool) {
	v, ok := c.Values[key]
	return v, ok
}

// GetString is a convenience for the common string case.
func (c *Context) GetString(key string) string {
	if v, ok := c.Values[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// StepRecord is the persisted outcome of one step (step_instance row).
type StepRecord struct {
	Index      int
	Name       string
	Status     StepStatus
	Attempt    int
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Result is the outcome of running a flow.
type Result struct {
	FlowInstanceID int64
	Status         FlowStatus
	Steps          []StepRecord
	// FailedStep is the index of the step that failed, -1 if none.
	FailedStep int
	// Err is the failure that triggered compensation.
	Err error
	// CompensationErrors records compensations that could not complete. A
	// non-empty slice means the flow needs human intervention: the platform is
	// in a state the engine could not clean up on its own.
	CompensationErrors []error
	// NeedsTicket reports whether an operations ticket must be raised
	// (03§8.4: 补偿三次失败转人工工单).
	NeedsTicket bool
}

// Errors.
var (
	ErrNoSteps      = errors.New("workflow: flow has no steps")
	ErrStepFailed   = errors.New("workflow: step failed after retries")
	ErrCompensation = errors.New("workflow: compensation failed")
)

// Engine runs flows.
type Engine struct {
	Now func() time.Time
	// OnStep is called after each step attempt, for persistence and tracing.
	// The production engine writes step_instance rows here.
	OnStep func(flowInstanceID int64, rec StepRecord)
	// Sleep is called between retry attempts with the Backoff delay, so a
	// failing downstream is not hammered in a tight loop. Injectable so tests
	// (and callers embedding the engine in a scheduler) do not really sleep;
	// nil defaults to time.Sleep.
	Sleep func(time.Duration)
}

// NewEngine builds an Engine.
func NewEngine(now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{Now: now, Sleep: time.Sleep}
}

// sleep waits the backoff delay before retry attempt `nextAttempt`.
func (e *Engine) sleep(prevAttempt int) {
	s := e.Sleep
	if s == nil {
		s = time.Sleep
	}
	s(Backoff(prevAttempt))
}

// Run executes the steps in order. On failure it compensates the completed
// steps in reverse and returns a Result describing what happened.
//
// The engine deliberately does not return early on a compensation failure: it
// attempts every remaining compensation, because a partial cleanup that stops
// at the first problem leaves more debris than one that tries everything and
// reports what it could not fix.
func (e *Engine) Run(flowInstanceID int64, ctx *Context, steps []Step) Result {
	if len(steps) == 0 {
		return Result{FlowInstanceID: flowInstanceID, Status: FlowFailed, FailedStep: -1, Err: ErrNoSteps}
	}
	if ctx == nil {
		ctx = &Context{}
	}
	ctx.FlowInstanceID = flowInstanceID

	records := make([]StepRecord, len(steps))
	for i, s := range steps {
		records[i] = StepRecord{Index: i, Name: s.Name, Status: StepPending}
	}

	completed := make([]int, 0, len(steps))

	for i, step := range steps {
		rec := &records[i]
		rec.Status = StepRunning
		rec.StartedAt = e.Now()

		var lastErr error
		for attempt := 1; attempt <= MaxStepAttempts; attempt++ {
			if attempt > 1 {
				e.sleep(attempt - 1) // exponential backoff between retries
			}
			ctx.Attempt = attempt
			rec.Attempt = attempt
			lastErr = step.Do(ctx)
			if lastErr == nil {
				break
			}
		}

		rec.FinishedAt = e.Now()
		if lastErr != nil {
			rec.Status = StepFailed
			rec.Error = lastErr.Error()
			e.emit(flowInstanceID, *rec)
			return e.compensate(flowInstanceID, ctx, steps, records, completed, i, lastErr)
		}

		rec.Status = StepDone
		e.emit(flowInstanceID, *rec)
		completed = append(completed, i)
	}

	return Result{
		FlowInstanceID: flowInstanceID,
		Status:         FlowCompleted,
		Steps:          records,
		FailedStep:     -1,
	}
}

// compensate runs the compensations of completed steps in reverse order.
func (e *Engine) compensate(
	flowInstanceID int64,
	ctx *Context,
	steps []Step,
	records []StepRecord,
	completed []int,
	failedStep int,
	cause error,
) Result {
	var compErrors []error

	// Reverse order: undo the most recent completed step first. Quota was
	// occupied before the resource was created, so it must be released after
	// the resource is deleted — otherwise quota briefly reports free capacity
	// that does not physically exist.
	for i := len(completed) - 1; i >= 0; i-- {
		idx := completed[i]
		step := steps[idx]
		if step.Undo == nil {
			// Nothing to reverse for this step, but the record still has to
			// land in step_instance: a compensation result invisible to the
			// operator is indistinguishable from a step still believed to be
			// in effect.
			records[idx].Status = StepCompensated
			e.emit(flowInstanceID, records[idx])
			continue
		}

		var lastErr error
		for attempt := 1; attempt <= MaxCompensationAttempts; attempt++ {
			if attempt > 1 {
				e.sleep(attempt - 1) // exponential backoff between retries
			}
			ctx.Attempt = attempt
			lastErr = step.Undo(ctx)
			if lastErr == nil {
				break
			}
		}

		if lastErr != nil {
			// Do not stop: attempt the remaining compensations anyway. A
			// partial cleanup that halts at the first problem leaves more
			// debris than one that tries everything and reports the rest.
			compErrors = append(compErrors,
				fmt.Errorf("%w: step %q after %d attempts: %v",
					ErrCompensation, step.Name, MaxCompensationAttempts, lastErr))
			records[idx].Status = StepFailed
			records[idx].Error = lastErr.Error()
			// The failure must land in step_instance too: an unemitted failed
			// compensation is invisible to the operator who has to fix it.
			e.emit(flowInstanceID, records[idx])
			continue
		}
		records[idx].Status = StepCompensated
		e.emit(flowInstanceID, records[idx])
	}

	status := FlowCompensated
	needsTicket := false
	if len(compErrors) > 0 {
		// The engine could not restore a clean state on its own.
		status = FlowFailed
		needsTicket = true
	}

	return Result{
		FlowInstanceID:     flowInstanceID,
		Status:             status,
		Steps:              records,
		FailedStep:         failedStep,
		Err:                fmt.Errorf("%w: step %d (%s): %v", ErrStepFailed, failedStep, steps[failedStep].Name, cause),
		CompensationErrors: compErrors,
		NeedsTicket:        needsTicket,
	}
}

func (e *Engine) emit(flowInstanceID int64, rec StepRecord) {
	if e.OnStep != nil {
		e.OnStep(flowInstanceID, rec)
	}
}

// Timer support: the engine's scheduling is "scan next_fire_at + distributed
// lock" rather than a dedicated scheduler component (03§4.3.3). These helpers
// express when a flow should next be examined.

// Backoff returns the delay before retrying an attempt, capped so a persistent
// failure does not push the next attempt beyond any useful horizon.
//
// The shift is bounded before it is applied, not after: 1<<99 overflows
// time.Duration and wraps to zero, which would turn a long-failing flow into a
// tight retry loop against an already-broken downstream — the opposite of what
// backoff is for.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	const maxDelay = 5 * time.Minute
	// 2^32 seconds already exceeds any cap by orders of magnitude, so clamp
	// the exponent before shifting.
	const maxShift = 32
	if attempt-1 >= maxShift {
		return maxDelay
	}
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d > maxDelay || d <= 0 {
		return maxDelay
	}
	return d
}

// NextFireAt returns when a flow awaiting retry should next be picked up.
func (e *Engine) NextFireAt(attempt int) time.Time {
	return e.Now().Add(Backoff(attempt))
}
