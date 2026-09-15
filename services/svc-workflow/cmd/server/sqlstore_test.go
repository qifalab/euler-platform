package main

import (
	"context"
	"errors"
	"strings"
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
// same saga twice — that is double provisioning.
func TestSQLStoreOneFlowPerBusinessKey(t *testing.T) {
	a := newSQLTestApp(t)

	startTestFlow(t, a, "ord-dup")
	_, _, err := a.StartFlow(100123, "test_ok", "ord-dup")
	if err == nil {
		t.Fatal("a second flow over the same business key must be refused")
	}
}
