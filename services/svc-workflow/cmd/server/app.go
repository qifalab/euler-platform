package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/starcloud/sc-platform/workflow"
)

// app wires the pkg-go/workflow engine to the HTTP handlers through small
// ports, so the production Vitess-backed repositories (03§4.3.3) can be
// swapped in without touching the handlers.
type app struct {
	engine *workflow.Engine
	store  *flowStore
	seq    atomic.Int64
}

func newApp() *app {
	return &app{
		engine: workflow.NewEngine(time.Now),
		store:  newFlowStore(),
	}
}

// StartFlow runs a predefined flow for def_key over an in-memory set of steps
// registered as closures, and records the Result so the caller can inspect it
// later (GET /internal/flows/{id}).
func (a *app) StartFlow(accountID int64, defKey, bizKey string) (int64, workflow.Result, error) {
	steps, ok := registeredFlows[defKey]
	if !ok {
		return 0, workflow.Result{}, fmt.Errorf("unknown flow definition %q", defKey)
	}

	id := a.seq.Add(1)
	ctx := &workflow.Context{AccountID: accountID, BizKey: bizKey}
	result := a.engine.Run(id, ctx, steps)

	a.store.put(toFlowRecord(id, accountID, defKey, bizKey, result))

	return id, result, nil
}

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

// flowStore is the in-memory stand-in for the flow_instance table. It is safe
// for concurrent use so the HTTP handlers can run in parallel.
type flowStore struct {
	mu    sync.RWMutex
	items map[int64]flowRecord
}

func newFlowStore() *flowStore {
	return &flowStore{items: make(map[int64]flowRecord)}
}

func (s *flowStore) put(r flowRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[r.ID] = r
}

func (s *flowStore) get(id int64) (flowRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.items[id]
	return r, ok
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
			c.Set("resource_id", "scecs-cn-north-1-01-"+c.BizKey)
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
