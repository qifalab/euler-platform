package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/storage/sqltest"
	"github.com/qifalab/euler-platform/workflow"
)

// newSQLTestStore wires the SQL store to a throwaway resource_db built from the
// service's own DDL directory. The store (not the environment) decides which
// database a test touches, which is what keeps these tests off the developer's
// real resource_db.
func newSQLTestStore(t *testing.T) *sqlFlowStore {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-orchestrator"))
	store, err := newSQLFlowStore(context.Background(), db)
	if err != nil {
		t.Fatalf("newSQLFlowStore: %v", err)
	}
	return store
}

// newSQLTestApp wraps the store in an app, for the tests that drive StartFlow.
func newSQLTestApp(t *testing.T) *app {
	t.Helper()
	store := newSQLTestStore(t)
	return &app{engine: workflow.NewEngine(time.Now), store: store, nextID: store.nextID}
}

var failedStep = workflow.Step{
	Name: "create_resource",
	Do: func(c *workflow.Context) error {
		return errors.New("provisioning failed")
	},
	Undo: func(c *workflow.Context) error { return nil },
}

func startTestFlow(t *testing.T, a *app, bizKey string) (int64, workflow.Result) {
	t.Helper()
	saved := registeredFlows["test_ok"]
	registeredFlows["test_ok"] = []workflow.Step{{
		Name: "occupy_quota",
		Do:   func(c *workflow.Context) error { return nil },
		Undo: func(c *workflow.Context) error { return nil },
	}}
	defer func() { registeredFlows["test_ok"] = saved }()
	id, res, err := a.StartFlow(100123, "test_ok", bizKey)
	if err != nil {
		t.Fatalf("start flow: %v", err)
	}
	return id, res
}

func TestSQLStoreFlowRoundTrip(t *testing.T) {
	a := newSQLTestApp(t)

	id, res := startTestFlow(t, a, "ord-1")
	if res.Status != workflow.FlowCompleted {
		t.Fatalf("flow status = %s, want COMPLETED", res.Status)
	}

	// The record must come back from the database, not from memory: a second app
	// over the same schema stands in for the restarted process.
	restarted := &app{engine: workflow.NewEngine(time.Now), store: a.store, nextID: a.nextID}
	rec, ok, err := restarted.store.get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("flow instance did not survive the restart")
	}
	if rec.Status != workflow.FlowCompleted || rec.DefKey != "test_ok" ||
		rec.BizKey != "ord-1" || rec.AccountID != 100123 {
		t.Fatalf("reloaded record = %+v", rec)
	}
	if len(rec.Steps) != 1 || rec.Steps[0].Status != workflow.StepDone {
		t.Fatalf("steps = %+v", rec.Steps)
	}
}

// TestSQLStoreFailedFlowCarriesCompensationTrail pins what an operator needs when
// a saga fails: which step, the error, and whether the compensation held.
func TestSQLStoreFailedFlowCarriesCompensationTrail(t *testing.T) {
	a := newSQLTestApp(t)

	saved := registeredFlows["test_fail"]
	registeredFlows["test_fail"] = []workflow.Step{
		{Name: "occupy_quota", Do: func(c *workflow.Context) error { return nil },
			Undo: func(c *workflow.Context) error { return nil }},
		failedStep,
	}
	defer func() { registeredFlows["test_fail"] = saved }()

	// StartFlow reports the failure in the Result (the engine's contract), not as
	// a call error. A failed step whose compensation held ends COMPENSATED;
	// FAILED is reserved for "compensation itself failed" (needs a human).
	id, res, err := a.StartFlow(100123, "test_fail", "ord-2")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != workflow.FlowCompensated {
		t.Fatalf("status = %s, want COMPENSATED (compensation held)", res.Status)
	}

	rec, ok, err := a.store.get(id)
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	// The engine wraps the step error with its own context (which step, how many
	// attempts); the assertion is on the cause surviving the round trip, not on
	// the wrapper's exact wording.
	if rec.FailedStep != 1 || !strings.Contains(rec.Err, "provisioning failed") {
		t.Fatalf("failure trail = %+v", rec)
	}
	if len(rec.Steps) != 2 || rec.Steps[1].Status != workflow.StepFailed {
		t.Fatalf("steps = %+v", rec.Steps)
	}
	if rec.Steps[0].Status != workflow.StepCompensated {
		t.Fatalf("the completed step was not compensated: %+v", rec.Steps[0])
	}
}

// TestSQLStoreOneFlowPerBusinessKey pins uk_biz: the same order must not run the
// same saga twice — that is double provisioning. The duplicate must be refused
// with errFlowExists BEFORE any step runs (the check used to happen when the
// finished record was written, after the saga had already provisioned).
func TestSQLStoreOneFlowPerBusinessKey(t *testing.T) {
	a := newSQLTestApp(t)
	runs := registerCountingFlow(t, "test_count")

	if _, _, err := a.StartFlow(100123, "test_count", "ord-dup"); err != nil {
		t.Fatalf("first start: %v", err)
	}
	_, _, err := a.StartFlow(100123, "test_count", "ord-dup")
	if !errors.Is(err, errFlowExists) {
		t.Fatalf("second start over the same business key = %v, want errFlowExists", err)
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("the saga ran %d times, want 1 (a refused start must not run a step)", n)
	}
}

// TestSQLStoreConcurrentStartsRunOnce: the reservation insert is the arbiter, so
// of several simultaneous starts over one key exactly one runs.
func TestSQLStoreConcurrentStartsRunOnce(t *testing.T) {
	a := newSQLTestApp(t)
	runs := registerCountingFlow(t, "test_count")

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = a.StartFlow(100123, "test_count", "ord-race")
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, errFlowExists):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || runs.Load() != 1 {
		t.Fatalf("%d starts succeeded and the saga ran %d times, want 1 and 1", ok, runs.Load())
	}
}

// TestSQLStoreKeylessStartsDoNotCollide: an empty biz_key used to be stored as
// "", so the second keyless start hit uk_biz.
func TestSQLStoreKeylessStartsDoNotCollide(t *testing.T) {
	a := newSQLTestApp(t)
	registerCountingFlow(t, "test_count")

	id1, _, err := a.StartFlow(100123, "test_count", "")
	if err != nil {
		t.Fatal(err)
	}
	id2, _, err := a.StartFlow(100123, "test_count", "")
	if err != nil {
		t.Fatalf("second keyless start: %v", err)
	}
	r1, _, err1 := a.store.get(id1)
	r2, _, err2 := a.store.get(id2)
	if err1 != nil || err2 != nil {
		t.Fatalf("lookup: %v / %v", err1, err2)
	}
	if r1.BizKey == "" || r1.BizKey == r2.BizKey {
		t.Fatalf("keyless starts got biz keys %q and %q, want distinct non-empty keys", r1.BizKey, r2.BizKey)
	}
	if r1.Status != workflow.FlowCompleted {
		t.Fatalf("finished flow status = %s, want COMPLETED", r1.Status)
	}
}
