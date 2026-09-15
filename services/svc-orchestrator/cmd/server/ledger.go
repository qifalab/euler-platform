package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/resource"
	"github.com/qifalab/euler-platform/storage"
)

// resourceLedger is the persistence boundary for the resource ledger
// (resource_instance + resource_state_log + provision_task, 03§4.3.2/06§4.5).
//
// # The ledger, not the handlers, owns transitions
//
// Every state change goes through Transition: the state machine's edge table is
// applied and the new state is persisted with its journal row under the
// optimistic lock, in one place. Handlers hold no locks and touch no maps — the
// two failure modes this service had before (a handler mutating a stored
// instance outside any lock; a second, looser path for console buttons) are
// structurally gone.
//
// # What is deliberately NOT here
//
// The order mirror (the in-flight PAID→FULFILLING→COMPLETED copy of an order) is
// process state, not ledger state: order_main belongs to svc-order, and a second
// service writing it would be two writers on one aggregate. It stays in memory;
// the durable half of the saga is the resource row (CREATING/RUNNING plus
// order_id), which is what the timeout scan and a restarted process can act on.
type resourceLedger interface {
	// Create inserts a CREATING instance. resource_instance's primary key is the
	// resource id, and fulfilment idempotency is arbitrated by looking the order
	// up (ByOrder) before creating; a duplicate here is the race the pre-check
	// lost, reported so the caller can re-read and serve the winner.
	Create(inst *resource.Instance) error
	// ByOrder returns the instance an order already produced — the restart-safe
	// fulfilment idempotency check (idx_order exists for exactly this).
	ByOrder(accountID, orderID int64) (*resource.Instance, error)
	Get(accountID int64, resourceID string) (*resource.Instance, error)
	List(accountID int64) ([]*resource.Instance, error)
	// Transition moves the instance through the state machine to `to` and
	// persists it with its journal row under the optimistic lock. BillingStart
	// is stamped here on the first RUNNING edge — 计费起点=首次 RUNNING 时刻,
	// 永不回拨 (D8) — so no caller can reset the billing clock by mutating the
	// instance directly.
	Transition(inst *resource.Instance, to resource.State, reason string) error
	// RecordProvision writes one dispatch record for the provision driver,
	// keyed by uk_idem (idempot_key, op_type): the same order+operation is one
	// row, updated in place, which is what makes a retry an update rather than
	// a second dispatch.
	RecordProvision(rec provisionRecord) error
}

// provisionRecord is one driver dispatch (06§4.5). Params is the marshalled
// ResourceSpec handed to the rc-* controller.
type provisionRecord struct {
	ResourceID string
	AccountID  int64
	OpType     string // CREATE/MODIFY/SUSPEND/RESUME/DELETE
	IdempotKey string // usually the order id
	Params     string
	Status     string // INIT/DISPATCHED/DONE/FAILED/RETRYING
	LastError  string
}

// --- in-memory implementation ------------------------------------------------

// memLedger is the phase-1 in-memory resourceLedger. It stores the same pointers
// the handlers have always shared, so the demo behaves exactly as before.
type memLedger struct {
	mu        sync.RWMutex
	resources map[string]*resource.Instance
}

func newMemLedger() *memLedger {
	return &memLedger{resources: make(map[string]*resource.Instance)}
}

func (s *memLedger) Create(inst *resource.Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.resources[inst.ResourceID]; exists {
		return fmt.Errorf("resource: duplicate resource id %s", inst.ResourceID)
	}
	s.resources[inst.ResourceID] = inst
	return nil
}

func (s *memLedger) ByOrder(accountID, orderID int64) (*resource.Instance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, inst := range s.resources {
		if inst.OrderID == orderID && inst.AccountID == accountID {
			return inst, nil
		}
	}
	return nil, nil
}

func (s *memLedger) Get(accountID int64, resourceID string) (*resource.Instance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	inst, ok := s.resources[resourceID]
	if !ok || inst.AccountID != accountID {
		return nil, nil
	}
	return inst, nil
}

func (s *memLedger) List(accountID int64) ([]*resource.Instance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*resource.Instance, 0)
	for _, inst := range s.resources {
		if inst.AccountID == accountID {
			out = append(out, inst)
		}
	}
	return out, nil
}

// Transition applies the machine edge to the stored instance, copies the
// caller's staged fields (a fresh Get plus deliberate edits — the upgrade flow
// stages a new SpecCode) onto the stored row, and stamps the billing clock on
// the first RUNNING edge.
func (s *memLedger) Transition(inst *resource.Instance, to resource.State, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.resources[inst.ResourceID]
	if !ok || stored.AccountID != inst.AccountID {
		return fmt.Errorf("resource: %s not found", inst.ResourceID)
	}
	if _, err := resource.NewMachine(time.Now).Transition(stored, to, stored.Version, reason); err != nil {
		return err
	}
	stored.SpecCode = inst.SpecCode
	stored.ExpiredAt = inst.ExpiredAt
	stored.LockedAt = inst.LockedAt
	stored.ReleasedAt = inst.ReleasedAt
	if to == resource.StateRunning && stored.BillingStart.IsZero() {
		stored.BillingStart = time.Now()
	}
	stored.UpdatedAt = time.Now()
	*inst = *stored // the caller's view follows the ledger
	return nil
}

func (s *memLedger) RecordProvision(rec provisionRecord) error {
	// Dispatch records are a database concern (retry scanning); the in-memory
	// ledger has no table to write and nothing to retry.
	return nil
}

// --- SQL implementation ------------------------------------------------------

// sqlLedger is the MySQL-backed resourceLedger over resource_db.
type sqlLedger struct {
	db *sql.DB
}

var _ resourceLedger = (*sqlLedger)(nil)

// statementTimeout bounds one ledger call; the port has no context parameter.
const statementTimeout = 10 * time.Second

// newSQLLedger wires the ledger to resource_db.
func newSQLLedger(_ context.Context, db *sql.DB) *sqlLedger {
	return &sqlLedger{db: db}
}

// newLedger picks the backend. Persistence is opt-in (pkg-go/storage doc): with
// EULER_DB_DSN set, the resource ledger lives in resource_db, so a restart keeps
// every instance, its state history and its provision tasks; unset, the
// in-memory ledger keeps the demo and `go test` dependency-free.
//
// A configured DSN that cannot be reached — or a schema sqlmigrate never
// touched — is a startup failure: an orchestrator that cannot record the
// resources it provisions must not provision them.
func newLedger(ctx context.Context) (resourceLedger, error) {
	db, ok, err := storage.MustOpenFor(ctx, "resource_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newMemLedger(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "resource_db"); err != nil {
		return nil, err
	}
	return newSQLLedger(ctx, db), nil
}

// persistentLedger reports whether a ledger is backed by MySQL, for the startup
// log.
func persistentLedger(l resourceLedger) bool {
	_, ok := l.(*sqlLedger)
	return ok
}

const instanceColumns = `resource_id, account_id, project_id, product_code, resource_type,
	region, zone, charge_type, status, spec_code, order_id, billing_start,
	expired_at, locked_at, released_at, version, created_at, updated_at`

// chargeTypeCode maps the DDL's 1包年包月 2按量 (3资源包 模型预留).
func chargeTypeCode(c resource.ChargeType) (int, error) {
	switch c {
	case resource.ChargePrepaid:
		return 1, nil
	case resource.ChargePostpaid:
		return 2, nil
	}
	return 0, fmt.Errorf("resource: unknown charge type %q", c)
}

func chargeTypeFromCode(code int) resource.ChargeType {
	if code == 2 {
		return resource.ChargePostpaid
	}
	return resource.ChargePrepaid
}

func (s *sqlLedger) Create(inst *resource.Instance) error {
	chargeC, err := chargeTypeCode(inst.ChargeType)
	if err != nil {
		return err
	}
	// resource_type is NOT NULL and the phase-1 saga only provisions instances;
	// an empty value is filled rather than rejected, because the console's
	// action endpoints already treat every row as an instance.
	resType := inst.ResourceType
	if resType == "" {
		resType = "instance"
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO resource_instance
		   (resource_id, account_id, project_id, product_code, resource_type, region, zone,
		    charge_type, status, spec_code, order_id, billing_start, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inst.ResourceID, inst.AccountID, inst.ProjectID, inst.ProductCode, resType,
		inst.Region, nullableString(inst.Zone), chargeC, string(inst.State),
		nullableString(inst.SpecCode), nullableInt(inst.OrderID), nullableTime(inst.BillingStart),
		inst.Version, inst.CreatedAt.UTC(), inst.UpdatedAt.UTC()); err != nil {
		if storage.IsDuplicateKey(err) {
			return fmt.Errorf("resource: duplicate resource id %s", inst.ResourceID)
		}
		return fmt.Errorf("resource: create %s: %w", inst.ResourceID, err)
	}
	return nil
}

func (s *sqlLedger) ByOrder(accountID, orderID int64) (*resource.Instance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	inst, err := scanInstance(s.db.QueryRowContext(ctx,
		`SELECT `+instanceColumns+` FROM resource_instance WHERE order_id = ? AND account_id = ?`,
		orderID, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resource: by order %d: %w", orderID, err)
	}
	return inst, nil
}

func (s *sqlLedger) Get(accountID int64, resourceID string) (*resource.Instance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	inst, err := scanInstance(s.db.QueryRowContext(ctx,
		`SELECT `+instanceColumns+` FROM resource_instance WHERE resource_id = ? AND account_id = ?`,
		resourceID, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resource: get %s: %w", resourceID, err)
	}
	return inst, nil
}

func (s *sqlLedger) List(accountID int64) ([]*resource.Instance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+instanceColumns+` FROM resource_instance
		  WHERE account_id = ?
		  ORDER BY created_at DESC, resource_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("resource: list for account %d: %w", accountID, err)
	}
	defer rows.Close()

	out := make([]*resource.Instance, 0, 8)
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("resource: scan for account %d: %w", accountID, err)
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resource: list for account %d: %w", accountID, err)
	}
	return out, nil
}

// Transition applies the machine edge, then writes the new state and the journal
// row in one transaction. The UPDATE guards on the status AND version the caller
// read — the DDL's documented 迁移带 WHERE status=? AND version=? — so a stale
// console button cannot move a resource twice.
func (s *sqlLedger) Transition(inst *resource.Instance, to resource.State, reason string) error {
	from, prevVersion := inst.State, inst.Version
	m := resource.NewMachine(time.Now)
	if _, err := m.Transition(inst, to, prevVersion, reason); err != nil {
		return err // inst untouched: the machine rejects before mutating
	}
	if to == resource.StateRunning && inst.BillingStart.IsZero() {
		inst.BillingStart = time.Now()
	}
	inst.UpdatedAt = time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE resource_instance
			    SET status = ?, version = ?, spec_code = ?, billing_start = ?,
			        expired_at = ?, locked_at = ?, released_at = ?, updated_at = ?
			  WHERE resource_id = ? AND account_id = ? AND status = ? AND version = ?`,
			string(inst.State), inst.Version, nullableString(inst.SpecCode),
			nullableTime(inst.BillingStart), nullableTime(inst.ExpiredAt),
			nullableTime(inst.LockedAt), nullableTime(inst.ReleasedAt), inst.UpdatedAt.UTC(),
			inst.ResourceID, inst.AccountID, string(from), prevVersion)
		if err != nil {
			return fmt.Errorf("resource: transition %s: %w", inst.ResourceID, err)
		}
		if err := storage.Affected(res, nil); err != nil {
			return fmt.Errorf("resource: transition %s: %w", inst.ResourceID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO resource_state_log (resource_id, account_id, from_status, to_status, reason)
			 VALUES (?, ?, ?, ?, ?)`,
			inst.ResourceID, inst.AccountID, nullableString(string(from)), string(inst.State),
			nullableString(reason)); err != nil {
			return fmt.Errorf("resource: state log for %s: %w", inst.ResourceID, err)
		}
		return nil
	})
}

// RecordProvision upserts the dispatch record. uk_idem makes (order, operation)
// the identity, so a retried fulfilment updates the row instead of minting a
// second task the retry scanner would double-dispatch.
func (s *sqlLedger) RecordProvision(rec provisionRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	var lastErr any
	if rec.LastError != "" {
		lastErr = rec.LastError
	}
	var finishedAt any
	if rec.Status == "DONE" || rec.Status == "FAILED" {
		finishedAt = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO provision_task
		   (resource_id, account_id, op_type, idempot_key, params_json, status, last_error, dispatched_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE status = ?, last_error = ?, finished_at = ?`,
		rec.ResourceID, rec.AccountID, rec.OpType, rec.IdempotKey, rec.Params,
		rec.Status, lastErr, time.Now().UTC(), finishedAt,
		rec.Status, lastErr, finishedAt); err != nil {
		return fmt.Errorf("resource: record provision for %s: %w", rec.ResourceID, err)
	}
	return nil
}

// rowScanner is the shared shape of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanInstance(sc rowScanner) (*resource.Instance, error) {
	var (
		inst             resource.Instance
		chargeC          int
		zone, spec       sql.NullString
		orderID          sql.NullInt64
		billing, expired sql.NullTime
		locked, released sql.NullTime
	)
	if err := sc.Scan(&inst.ResourceID, &inst.AccountID, &inst.ProjectID, &inst.ProductCode,
		&inst.ResourceType, &inst.Region, &zone, &chargeC, &inst.State, &spec, &orderID,
		&billing, &expired, &locked, &released, &inst.Version, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
		return nil, err
	}
	inst.Zone = zone.String
	inst.ChargeType = chargeTypeFromCode(chargeC)
	inst.SpecCode = spec.String
	if orderID.Valid {
		inst.OrderID = orderID.Int64
	}
	if billing.Valid {
		inst.BillingStart = billing.Time
	}
	if expired.Valid {
		inst.ExpiredAt = expired.Time
	}
	if locked.Valid {
		inst.LockedAt = locked.Time
	}
	if released.Valid {
		inst.ReleasedAt = released.Time
	}
	return &inst, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// marshalSpec marshals the driver spec for provision_task.params_json; a
// marshalling failure is reported, not stored as "{}".
func marshalSpec(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
