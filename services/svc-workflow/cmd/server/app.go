package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qifalab/euler-platform/workflow"
)

// flowStore is the persistence boundary for flow instances. The SQL
// implementation writes resource_db's flow_instance + step_instance; the
// in-memory one keeps the demo and `go test` dependency-free.
//
// Every method carries an error because a SQL backend has one: refusing to start
// a flow it cannot record is better than running an untraceable saga.
type flowStore interface {
	// reserve records a RUNNING instance before any step runs. It returns
	// errFlowExists when (def_key, biz_key) already has an instance — the
	// uk_biz rule, enforced by both implementations.
	reserve(r flowRecord) error
	// finish writes the outcome (status, failure trail, steps) of an instance
	// reserve recorded.
	finish(r flowRecord) error
	// release drops a reservation that never got an outcome (still RUNNING), so
	// the business key can be started again. An instance that already has an
	// outcome is left alone (nil).
	release(id int64) error
	get(id int64) (flowRecord, bool, error)
}

// Errors StartFlow reports; the handler maps them to status codes.
var (
	errUnknownDef = errors.New("workflow: unknown flow definition")
	errFlowExists = errors.New("workflow: a flow for this business key was already started")
)

// app wires the pkg-go/workflow engine to the HTTP handlers through small
// ports, so the production Vitess-backed repositories (03§4.3.3) can be
// swapped in without touching the handlers.
type app struct {
	engine *workflow.Engine
	store  flowStore
	// nextID mints flow instance ids. The in-memory app uses an atomic counter;
	// the persistent app takes them from the 号段 allocator, because a restart
	// must not re-issue ids it already committed.
	nextID func() int64
}

func newApp(ctx context.Context) (*app, error) {
	store, nextID, err := newStore(ctx)
	if err != nil {
		return nil, err
	}
	slog.Info("svc-workflow store ready", "persistent", persistentStore(store))
	return &app{
		engine: workflow.NewEngine(time.Now),
		store:  store,
		nextID: nextID,
	}, nil
}

// newInMemoryApp builds the demo app. Handler tests construct it directly, so
// they never depend on whether the developer's shell has EULER_DB_DSN set.
func newInMemoryApp() *app {
	var seq atomic.Int64
	return &app{
		engine: workflow.NewEngine(time.Now),
		store:  newMemFlowStore(),
		nextID: func() int64 { return seq.Add(1) },
	}
}

// StartFlow runs a predefined flow for def_key over an in-memory set of steps
// registered as closures, and records the Result so the caller can inspect it
// later (GET /internal/flows/{id}).
//
// The instance is reserved (RUNNING) before the first step runs. uk_biz is the
// answer to "did this order already start this flow", and it used to be
// consulted only when the finished record was written — after the saga had
// already provisioned — so a retried start ran every step a second time and
// only then failed. A duplicate now returns errFlowExists without running.
func (a *app) StartFlow(accountID int64, defKey, bizKey string) (int64, workflow.Result, error) {
	steps, ok := registeredFlows[defKey]
	if !ok {
		return 0, workflow.Result{}, fmt.Errorf("%w %q", errUnknownDef, defKey)
	}

	id := a.nextID()
	if bizKey == "" {
		bizKey = autoBizKey(id)
	}
	if err := a.store.reserve(flowRecord{
		ID: id, AccountID: accountID, DefKey: defKey, BizKey: bizKey,
		Status: workflow.FlowRunning, Steps: []stepRecord{},
	}); err != nil {
		return 0, workflow.Result{}, err
	}

	ctx := &workflow.Context{AccountID: accountID, BizKey: bizKey}
	result := a.engine.Run(id, ctx, steps)

	// The record is the saga's trail: writing it is part of starting the flow,
	// and a failure here is surfaced rather than logged-and-forgotten — an
	// instance nobody can see is an unexplainable resource (03§8.5).
	if err := a.store.finish(toFlowRecord(id, accountID, defKey, bizKey, result)); err != nil {
		// A reservation left RUNNING refused every retry with 409 forever. It
		// is released (best effort) so a retry can run the flow again — its
		// steps are idempotent per business key — and the failure is surfaced.
		if rerr := a.store.release(id); rerr != nil {
			slog.Error("flow outcome not recorded and its reservation could not be released; retries will be refused",
				"flowId", id, "defKey", defKey, "bizKey", bizKey, "status", result.Status, "err", err, "releaseErr", rerr)
		} else {
			slog.Warn("flow outcome not recorded; reservation released so a retry can start it again",
				"flowId", id, "defKey", defKey, "bizKey", bizKey, "status", result.Status, "err", err)
		}
		return id, result, err
	}

	return id, result, nil
}

// autoBizKey names a flow started without a business key. Such a start has
// nothing to deduplicate against, so it gets a key of its own: storing "" would
// make every keyless start after the first collide on uk_biz.
func autoBizKey(id int64) string { return "auto-" + strconv.FormatInt(id, 10) }

// flowRecord is the persisted representation of a flow instance as served by
// GET /internal/flows/{id}. It mirrors workflow.Result with fields that decode
// to JSON cleanly.
type flowRecord struct {
	ID                 int64               `json:"id"`
	AccountID          int64               `json:"account_id"`
	DefKey             string              `json:"def_key"`
	BizKey             string              `json:"biz_key"`
	Status             workflow.FlowStatus `json:"status"`
	FailedStep         int                 `json:"failed_step"`
	Err                string              `json:"error,omitempty"`
	CompensationErrors []string            `json:"compensation_errors,omitempty"`
	NeedsTicket        bool                `json:"needs_ticket"`
	Steps              []stepRecord        `json:"steps"`
	FinishedAt         time.Time           `json:"finished_at"`
}

type stepRecord struct {
	Index   int                 `json:"index"`
	Name    string              `json:"name"`
	Status  workflow.StepStatus `json:"status"`
	Attempt int                 `json:"attempt"`
	Error   string              `json:"error,omitempty"`
}

func toFlowRecord(id, accountID int64, defKey, bizKey string, res workflow.Result) flowRecord {
	steps := make([]stepRecord, 0, len(res.Steps))
	for _, s := range res.Steps {
		steps = append(steps, stepRecord{
			Index:   s.Index,
			Name:    s.Name,
			Status:  s.Status,
			Attempt: s.Attempt,
			Error:   s.Error,
		})
	}
	comp := make([]string, 0, len(res.CompensationErrors))
	for _, e := range res.CompensationErrors {
		comp = append(comp, e.Error())
	}
	errMsg := ""
	if res.Err != nil {
		errMsg = res.Err.Error()
	}
	rec := flowRecord{
		ID:                 id,
		AccountID:          accountID,
		DefKey:             defKey,
		BizKey:             bizKey,
		Status:             res.Status,
		FailedStep:         res.FailedStep,
		Err:                errMsg,
		CompensationErrors: comp,
		NeedsTicket:        res.NeedsTicket,
		Steps:              steps,
		FinishedAt:         time.Now(),
	}
	return rec
}

// memFlowStore is the in-memory stand-in for the flow_instance table. It is safe
// for concurrent use so the HTTP handlers can run in parallel.
type memFlowStore struct {
	mu    sync.RWMutex
	items map[int64]flowRecord
	byBiz map[string]int64 // def_key + "\x00" + biz_key → id: the in-memory uk_biz
}

func newMemFlowStore() *memFlowStore {
	return &memFlowStore{items: make(map[int64]flowRecord), byBiz: make(map[string]int64)}
}

func bizIndexKey(defKey, bizKey string) string { return defKey + "\x00" + bizKey }

func (s *memFlowStore) reserve(r flowRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := bizIndexKey(r.DefKey, r.BizKey)
	if _, dup := s.byBiz[k]; dup {
		return errFlowExists
	}
	if _, dup := s.items[r.ID]; dup {
		return fmt.Errorf("workflow: flow id %d already exists", r.ID)
	}
	s.byBiz[k] = r.ID
	s.items[r.ID] = r
	return nil
}

func (s *memFlowStore) finish(r flowRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.items[r.ID]
	if !ok || cur.Status != workflow.FlowRunning {
		return fmt.Errorf("workflow: flow %d has no running reservation", r.ID)
	}
	s.items[r.ID] = r
	return nil
}

func (s *memFlowStore) release(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.items[id]
	if !ok || cur.Status != workflow.FlowRunning {
		return nil
	}
	delete(s.items, id)
	delete(s.byBiz, bizIndexKey(cur.DefKey, cur.BizKey))
	return nil
}

func (s *memFlowStore) get(id int64) (flowRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.items[id]
	return r, ok, nil
}

// --- Predefined flows (in-memory step closures) ------------------------------
//
// These stand in for the flow definitions that in production live in
// Vitess and are fetched by def_key. The steps exercise the engine's core
// guarantee: on failure, completed steps are compensated in REVERSE order
// (03§8.4), and quota occupied before a resource is created must be released
// after the resource is deleted.

// resourceCreateFlow mirrors the canonical provisioning saga: occupy quota →
// create the resource → start metering. Each forward action produces a value
// its compensation needs, and the compensation is idempotent (deleting an
// already-deleted resource is a success).
var resourceCreateFlow = []workflow.Step{
	{
		Name: "occupy_quota",
		Do: func(c *workflow.Context) error {
			c.Set("quota_occupied", "true")
			return nil
		},
		Undo: func(c *workflow.Context) error {
			// Releasing quota is idempotent: releasing an already-released
			// reservation is a success, not an error (03§8.4).
			if c.GetString("quota_occupied") == "" {
				return nil
			}
			c.Set("quota_occupied", "")
			return nil
		},
	},
	{
		Name: "create_resource",
		Do: func(c *workflow.Context) error {
			c.Set("resource_id", "euecs-cn-north-1-01-"+c.BizKey)
			return nil
		},
		Undo: func(c *workflow.Context) error {
			// Deleting an already-deleted resource must succeed.
			_ = c.GetString("resource_id")
			return nil
		},
	},
	{
		Name: "start_metering",
		Do: func(c *workflow.Context) error {
			c.Set("metering_started", "true")
			return nil
		},
		Undo: func(c *workflow.Context) error {
			c.Set("metering_started", "")
			return nil
		},
	},
}

// registeredFlows maps a def_key to its step list. A predefined flow is a
// fixed, named sequence — the server exposes StartFlow(def_key, biz_key),
// which runs the steps for that definition.
var registeredFlows = map[string][]workflow.Step{
	"resource_create": resourceCreateFlow,
}
