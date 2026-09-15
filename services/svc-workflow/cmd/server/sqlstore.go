package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/workflow"
)

// newStore picks the backend. Persistence is opt-in (pkg-go/storage doc): with
// EULER_DB_DSN set, flow instances live in resource_db, so a restart keeps the record
// of which sagas ran, which failed, and what was compensated; unset, the in-memory
// store keeps the demo and `go test` dependency-free.
//
// A configured DSN that cannot be reached — or a schema sqlmigrate never touched —
// is a startup failure: a workflow engine that cannot record an instance must not
// accept the request that starts one.
func newStore(ctx context.Context) (flowStore, func() int64, error) {
	db, ok, err := storage.MustOpenFor(ctx, "resource_db")
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		var seq atomic.Int64
		return newMemFlowStore(), func() int64 { return seq.Add(1) }, nil
	}
	if err := storage.EnsureMigrated(ctx, db, "resource_db"); err != nil {
		return nil, nil, err
	}
	store, err := newSQLFlowStore(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	return store, store.nextID, nil
}

// persistentStore reports whether a store is backed by MySQL, for the startup log.
func persistentStore(s flowStore) bool {
	_, ok := s.(*sqlFlowStore)
	return ok
}

// sqlFlowStore is the MySQL-backed flowStore over resource_db's flow_instance and
// step_instance (03§4.3.3).
//
// # Where the record lives, and why it is split in two tables
//
// The in-memory store keeps one flowRecord per instance. The schema normalises it:
// the instance's own facts (status, failed step, error, compensation outcome) go to
// flow_instance — the parts with no column of their own travel in payload_json,
// which is what that column is for — and each step becomes a step_instance row.
// Splitting them is what makes "which step failed, and was it compensated" a query
// rather than a JSON parse, which matters when an operator is chasing a stuck flow.
//
// # Ids come from the 号段 allocator
//
// Both ids were process-local counters in the phase-1 service; a restart would
// re-issue 1 and collide with a committed instance — losing the fulfilment record
// of an order. They now come from resource_db.id_sequence (seeded by resource_db V2).
//
// # One flow per business key
//
// uk_biz(def_key, biz_key) is the schema's answer to "did this order already
// start this flow": the duplicate is refused rather than silently creating a
// second saga over the same order.
type sqlFlowStore struct {
	db         *sql.DB
	flowIDs    func() int64
	stepIDs    func() int64
}

var _ flowStore = (*sqlFlowStore)(nil)

// statementTimeout bounds one store call; the port has no context parameter.
const statementTimeout = 10 * time.Second

// newFlowStore wires the store to its id sequences.
func newSQLFlowStore(ctx context.Context, db *sql.DB) (*sqlFlowStore, error) {
	flows, err := storage.OpenSequence(ctx, db, "flow_instance", 1000)
	if err != nil {
		return nil, err
	}
	steps, err := storage.OpenSequence(ctx, db, "step_instance", 1000)
	if err != nil {
		return nil, err
	}
	return &sqlFlowStore{db: db, flowIDs: flows.NextFunc(), stepIDs: steps.NextFunc()}, nil
}

// nextID mints a flow instance id (the engine needs it before the record is
// written, so it is exposed separately from put).
func (s *sqlFlowStore) nextID() int64 { return s.flowIDs() }

// flowPayload is the part of a flow record with no column of its own. Keeping it
// as a named struct (rather than a map) means a field rename cannot silently
// change the stored JSON shape.
type flowPayload struct {
	FailedStep         int      `json:"failed_step"`
	Err                string   `json:"error,omitempty"`
	CompensationErrors []string `json:"compensation_errors,omitempty"`
	NeedsTicket        bool     `json:"needs_ticket"`
	FinishedAt         string   `json:"finished_at"`
}

// put writes the instance and its steps in one transaction: an instance whose
// steps are missing tells an operator nothing about where the saga stopped.
func (s *sqlFlowStore) put(r flowRecord) error {
	payload, err := json.Marshal(flowPayload{
		FailedStep:         r.FailedStep,
		Err:                r.Err,
		CompensationErrors: r.CompensationErrors,
		NeedsTicket:        r.NeedsTicket,
		FinishedAt:         r.FinishedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("workflow: encode payload for flow %d: %w", r.ID, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO flow_instance
			   (flow_instance_id, def_key, def_version, account_id, biz_key, status,
			    payload_json, current_step, version, created_at, updated_at)
			 VALUES (?, ?, 1, ?, ?, ?, ?, ?, 0, ?, ?)`,
			r.ID, r.DefKey, r.AccountID, r.BizKey, string(r.Status), payload,
			len(r.Steps), r.FinishedAt.UTC(), r.FinishedAt.UTC()); err != nil {
			if storage.IsDuplicateKey(err) {
				// uk_biz: this order's flow already ran (or is running). Starting a
				// second saga over the same order is how double provisioning happens.
				return fmt.Errorf("workflow: flow for %s/%s already exists", r.DefKey, r.BizKey)
			}
			return fmt.Errorf("workflow: insert flow %d: %w", r.ID, err)
		}

		for _, step := range r.Steps {
			var errMsg any
			if step.Error != "" {
				errMsg = step.Error
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO step_instance
				   (step_id, flow_instance_id, account_id, step_index, step_type, status,
				    attempt, error_msg, finished_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				s.stepIDs(), r.ID, r.AccountID, step.Index, step.Name,
				string(step.Status), step.Attempt, errMsg, r.FinishedAt.UTC()); err != nil {
				return fmt.Errorf("workflow: insert step %d of flow %d: %w", step.Index, r.ID, err)
			}
		}
		return nil
	})
}

// get reads one flow instance with its steps, oldest step first.
func (s *sqlFlowStore) get(id int64) (flowRecord, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	var (
		rec     flowRecord
		status  string
		payload []byte
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT flow_instance_id, def_key, account_id, biz_key, status, payload_json, updated_at
		   FROM flow_instance
		  WHERE flow_instance_id = ?`, id).
		Scan(&rec.ID, &rec.DefKey, &rec.AccountID, &rec.BizKey, &status, &payload, &rec.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return flowRecord{}, false, nil
	}
	if err != nil {
		return flowRecord{}, false, fmt.Errorf("workflow: get flow %d: %w", id, err)
	}
	// FlowStatus and StepStatus are string types, and the DDL stores the same
	// vocabulary (RUNNING/COMPLETED/COMPENSATING/COMPENSATED/FAILED), so the cast
	// is the mapping — no lookup table to drift out of sync.
	rec.Status = workflow.FlowStatus(status)

	var decoded flowPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return flowRecord{}, false, fmt.Errorf("workflow: decode payload for flow %d: %w", id, err)
	}
	rec.FailedStep = decoded.FailedStep
	rec.Err = decoded.Err
	rec.CompensationErrors = decoded.CompensationErrors
	rec.NeedsTicket = decoded.NeedsTicket
	if decoded.FinishedAt != "" {
		if ts, err := time.Parse(time.RFC3339Nano, decoded.FinishedAt); err == nil {
			rec.FinishedAt = ts
		}
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT step_index, step_type, status, attempt, error_msg
		   FROM step_instance
		  WHERE flow_instance_id = ?
		  ORDER BY step_index`, id)
	if err != nil {
		return flowRecord{}, false, fmt.Errorf("workflow: list steps for flow %d: %w", id, err)
	}
	defer rows.Close()

	rec.Steps = make([]stepRecord, 0, 8)
	for rows.Next() {
		var (
			step   stepRecord
			st     string
			errMsg sql.NullString
		)
		if err := rows.Scan(&step.Index, &step.Name, &st, &step.Attempt, &errMsg); err != nil {
			return flowRecord{}, false, fmt.Errorf("workflow: scan step for flow %d: %w", id, err)
		}
		step.Status = workflow.StepStatus(st)
		step.Error = errMsg.String
		rec.Steps = append(rec.Steps, step)
	}
	if err := rows.Err(); err != nil {
		return flowRecord{}, false, fmt.Errorf("workflow: list steps for flow %d: %w", id, err)
	}
	return rec, true, nil
}
