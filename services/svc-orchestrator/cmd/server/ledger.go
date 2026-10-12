package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/qifalab/euler-platform/provision"
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
	//
	// The options add, in the same atomic step, the resource's attributes, the
	// globally unique names it claims (a *nameTakenError when another resource
	// holds one) and a check against the account's other live resources of the
	// product in the region (its error aborts the create) — see createOption.
	Create(inst *resource.Instance, opts ...createOption) error
	// ByOrder returns the instance an order already produced — the restart-safe
	// fulfilment idempotency check (uk_order, V4, makes it authoritative).
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
	// AppliedSpec returns the spec of the resource's last CREATE dispatch
	// (provision.SpecSource): a restarted driver rebuilds what it applied
	// from it.
	AppliedSpec(resourceID string) (provision.Spec, bool, error)

	// Attrs returns one resource's attributes (resource_instance_attr),
	// system keys included; an empty map when it has none.
	Attrs(accountID int64, resourceID string) (map[string]string, error)
	// AccountAttrs returns the attributes of all the account's resources, by
	// resource id — one read for the console list.
	AccountAttrs(accountID int64) (map[string]map[string]string, error)
	// UpdateAttrs writes attributes under the resource's row lock: u.Check
	// sees the current state and attributes and may veto, and u.Journal (if
	// set) is recorded in resource_state_log in the same transaction.
	// errResourceNotFound when the account has no such resource.
	UpdateAttrs(accountID int64, resourceID string, u attrUpdate) error
	// MarkCompensation claims (on) or revokes (off) the compensation of a
	// CREATING resource: guarded on (CREATING, inst.Version), it bumps the
	// version, sets or clears attrCompensating and journals the step. The
	// claim is what keeps a concurrent settle from delivering a resource
	// whose reclaim is under way (see settleCreating).
	MarkCompensation(inst *resource.Instance, reason string, on bool) error
	// ListByState pages through the resources in a state across all accounts,
	// ordered by resource id, starting after `after` — the provisioning
	// poller's scan.
	ListByState(state resource.State, after string, limit int) ([]*resource.Instance, error)

	// CreateBackup records a snapshot of a resource (resource_backup).
	CreateBackup(b backupRecord) error
	// ListBackups returns the resource's live snapshots, newest first, after
	// deleting the ones whose retention lapsed at now.
	ListBackups(accountID int64, resourceID string, now time.Time) ([]backupRecord, error)
	// DeleteBackup removes one snapshot; false when there was none.
	DeleteBackup(accountID int64, resourceID, backupID string) (bool, error)
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

// memLedger is the phase-1 in-memory resourceLedger. It hands out and stores
// COPIES, never its own pointers: a pointer read after the lock is released
// while Transition writes the same struct is a data race, and a handler
// mutating a stored instance outside any lock is exactly the failure mode this
// interface exists to remove. Callers see what the SQL ledger would give them —
// a snapshot that only Transition can move.
type memLedger struct {
	mu        sync.RWMutex
	resources map[string]*resource.Instance
	// attrs is resource_instance_attr: resource id → key → value.
	attrs map[string]map[string]string
	// claims is resource_name_claim: claimKey → owning resource id.
	claims map[string]string
	// specs holds the last CREATE dispatch spec per resource (provision_task).
	specs map[string]provision.Spec
	// backups is resource_backup, by backup id.
	backups map[string]backupRecord
}

func newMemLedger() *memLedger {
	return &memLedger{
		resources: make(map[string]*resource.Instance),
		attrs:     make(map[string]map[string]string),
		claims:    make(map[string]string),
		specs:     make(map[string]provision.Spec),
		backups:   make(map[string]backupRecord),
	}
}

func cloneInstance(inst *resource.Instance) *resource.Instance {
	c := *inst
	return &c
}

func (s *memLedger) Create(inst *resource.Instance, opts ...createOption) error {
	o := applyCreateOptions(opts)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.resources[inst.ResourceID]; exists {
		return fmt.Errorf("resource: duplicate resource id %s", inst.ResourceID)
	}
	// uk_order parity: one order produces one resource (V4 migration).
	if inst.OrderID != 0 {
		for _, other := range s.resources {
			if other.OrderID == inst.OrderID {
				return fmt.Errorf("resource: duplicate resource for order %d", inst.OrderID)
			}
		}
	}
	if o.peerCheck != nil {
		var peers []peerResource
		for id, other := range s.resources {
			if other.AccountID == inst.AccountID && other.ProductCode == inst.ProductCode &&
				other.Region == inst.Region && occupiesPeerSlot(other.State) {
				peers = append(peers, peerResource{ResourceID: id, State: other.State, Attrs: cloneAttrs(s.attrs[id])})
			}
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].ResourceID < peers[j].ResourceID })
		if err := o.peerCheck(peers); err != nil {
			return err
		}
	}
	// resource_name_claim parity: the primary key (namespace, region, name).
	for _, c := range o.claims {
		if holder, taken := s.claims[claimKey(c, inst.Region)]; taken {
			return &nameTakenError{Claim: c, Region: inst.Region, Holder: holder}
		}
	}
	s.resources[inst.ResourceID] = cloneInstance(inst)
	if len(o.attrs) > 0 {
		s.attrs[inst.ResourceID] = cloneAttrs(o.attrs)
	}
	for _, c := range o.claims {
		s.claims[claimKey(c, inst.Region)] = inst.ResourceID
	}
	return nil
}

func (s *memLedger) ByOrder(accountID, orderID int64) (*resource.Instance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, inst := range s.resources {
		if inst.OrderID == orderID && inst.AccountID == accountID {
			return cloneInstance(inst), nil
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
	return cloneInstance(inst), nil
}

// List returns the account's instances in the SQL ledger's order (created_at
// DESC, resource_id), so the console list does not reshuffle on every refresh
// and both backends agree.
func (s *memLedger) List(accountID int64) ([]*resource.Instance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*resource.Instance, 0)
	for _, inst := range s.resources {
		if inst.AccountID == accountID {
			out = append(out, cloneInstance(inst))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ResourceID < out[j].ResourceID
	})
	return out, nil
}

// Transition applies the machine edge to a copy of the stored instance, guarded
// on the version the caller read (the SQL ledger's WHERE version = ?), carries
// over the caller's staged fields (SpecCode, ExpiredAt — the upgrade and
// prepaid fulfilment flows stage them), and stamps the billing clock on the
// first RUNNING edge. LockedAt/ReleasedAt belong to the state machine: copying
// them from the caller would overwrite the stamp the machine just set with the
// caller's stale (usually zero) value.
func (s *memLedger) Transition(inst *resource.Instance, to resource.State, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.resources[inst.ResourceID]
	if !ok || stored.AccountID != inst.AccountID {
		return fmt.Errorf("resource: %s not found", inst.ResourceID)
	}
	if stored.State != inst.State {
		return fmt.Errorf("%w: %s is %s, caller saw %s",
			resource.ErrVersionConflict, inst.ResourceID, stored.State, inst.State)
	}
	next := cloneInstance(stored)
	next.SpecCode = inst.SpecCode
	next.ExpiredAt = inst.ExpiredAt
	if _, err := resource.NewMachine(time.Now).Transition(next, to, inst.Version, reason); err != nil {
		return err
	}
	if to == resource.StateRunning && next.BillingStart.IsZero() {
		next.BillingStart = time.Now()
	}
	next.UpdatedAt = time.Now()
	s.resources[inst.ResourceID] = next
	if releasesClaims(to) {
		s.dropClaimsLocked(inst.ResourceID)
	}
	*inst = *next // the caller's view follows the ledger
	return nil
}

// RecordProvision keeps the last CREATE spec per resource — the part of
// provision_task a restarted driver reads back (AppliedSpec). Retry scanning
// is a database concern the in-memory ledger has no use for.
func (s *memLedger) RecordProvision(rec provisionRecord) error {
	if rec.OpType != "CREATE" {
		return nil
	}
	var spec provision.Spec
	if err := json.Unmarshal([]byte(rec.Params), &spec); err != nil {
		return fmt.Errorf("resource: record provision for %s: params: %w", rec.ResourceID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs[rec.ResourceID] = spec
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

func (s *sqlLedger) Create(inst *resource.Instance, opts ...createOption) error {
	o := applyCreateOptions(opts)
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

	// One transaction: the row, its attributes and its name claims exist
	// together or not at all, and the peer check sees the peers it vetted
	// until the commit.
	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if o.peerCheck != nil {
			peers, err := lockPeers(ctx, tx, inst)
			if err != nil {
				return err
			}
			if err := o.peerCheck(peers); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO resource_instance
			   (resource_id, account_id, project_id, product_code, resource_type, region, zone, az_id,
			    charge_type, status, spec_code, order_id, billing_start, expired_at, version, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			inst.ResourceID, inst.AccountID, inst.ProjectID, inst.ProductCode, resType,
			// zone is the deprecated V1 column; az_id is its V2 replacement, the one
			// the AZ views (v_resource_by_az, v_resource_az_failover_impact) read.
			inst.Region, nullableString(inst.Zone), nullableString(inst.Zone), chargeC, string(inst.State),
			nullableString(inst.SpecCode), nullableInt(inst.OrderID), nullableTime(inst.BillingStart),
			nullableTime(inst.ExpiredAt), inst.Version, inst.CreatedAt.UTC(), inst.UpdatedAt.UTC()); err != nil {
			if storage.IsDuplicateKey(err) {
				// The primary key (resource id) or uk_order (one resource per order).
				return fmt.Errorf("resource: duplicate resource %s (order %d)", inst.ResourceID, inst.OrderID)
			}
			return fmt.Errorf("resource: create %s: %w", inst.ResourceID, err)
		}
		if err := upsertAttrs(ctx, tx, inst.AccountID, inst.ResourceID, o.attrs); err != nil {
			return err
		}
		for _, c := range o.claims {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO resource_name_claim (claim_ns, region, name, resource_id, account_id)
				 VALUES (?, ?, ?, ?, ?)`,
				c.Namespace, inst.Region, c.Name, inst.ResourceID, inst.AccountID); err != nil {
				if storage.IsDuplicateKey(err) {
					return &nameTakenError{Claim: c, Region: inst.Region}
				}
				return fmt.Errorf("resource: claim %s %q for %s: %w", c.Namespace, c.Name, inst.ResourceID, err)
			}
		}
		return nil
	})
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
//
// The edge is applied to a copy: the caller's instance changes only once the
// transaction committed. Mutating it up front left a caller whose guarded
// UPDATE lost (or whose transaction failed) holding a state and version the
// database never had.
func (s *sqlLedger) Transition(caller *resource.Instance, to resource.State, reason string) error {
	from, prevVersion := caller.State, caller.Version
	next := *caller
	inst := &next
	m := resource.NewMachine(time.Now)
	if _, err := m.Transition(inst, to, prevVersion, reason); err != nil {
		return err
	}
	if to == resource.StateRunning && inst.BillingStart.IsZero() {
		inst.BillingStart = time.Now()
	}
	inst.UpdatedAt = time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	err := storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
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
		if err := journal(ctx, tx, inst.ResourceID, inst.AccountID, from, inst.State, reason); err != nil {
			return err
		}
		if releasesClaims(to) {
			// The resource no longer holds its names (a rolled-back or released
			// bucket): free them in the same transaction as the edge.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM resource_name_claim WHERE resource_id = ?`, inst.ResourceID); err != nil {
				return fmt.Errorf("resource: release name claims of %s: %w", inst.ResourceID, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	*caller = next
	return nil
}

// journal appends one resource_state_log row. reason is cut to the column's
// 256 characters: an over-long driver error must not fail the edge it explains.
func journal(ctx context.Context, tx *sql.Tx, resourceID string, accountID int64, from, to resource.State, reason string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO resource_state_log (resource_id, account_id, from_status, to_status, reason)
		 VALUES (?, ?, ?, ?, ?)`,
		resourceID, accountID, nullableString(string(from)), string(to),
		nullableString(truncateRunes(reason, stateLogReasonMax))); err != nil {
		return fmt.Errorf("resource: state log for %s: %w", resourceID, err)
	}
	return nil
}

// RecordProvision upserts the dispatch record. uk_idem makes (order, operation)
// the identity, so a retried fulfilment updates the row instead of minting a
// second task the retry scanner would double-dispatch. The row's params_json
// is also what AppliedSpec reads back for a restarted driver.
func (s *sqlLedger) RecordProvision(rec provisionRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	var lastErr any
	if rec.LastError != "" {
		// last_error is VARCHAR(512): an over-long driver error must not turn
		// the audit upsert itself into a failure.
		lastErr = truncateUTF8(rec.LastError, lastErrorMaxBytes)
	}
	var finishedAt any
	if rec.Status == "DONE" || rec.Status == "FAILED" {
		finishedAt = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO provision_task
		   (resource_id, account_id, op_type, idempot_key, params_json, status, last_error, dispatched_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE params_json = ?, status = ?, last_error = ?, finished_at = ?`,
		rec.ResourceID, rec.AccountID, rec.OpType, rec.IdempotKey, rec.Params,
		rec.Status, lastErr, time.Now().UTC(), finishedAt,
		// params_json follows the retry: a re-drive dispatches the spec as it
		// is now, and AppliedSpec must hand a restarted driver that one.
		rec.Params, rec.Status, lastErr, finishedAt); err != nil {
		return fmt.Errorf("resource: record provision for %s: %w", rec.ResourceID, err)
	}
	return nil
}

// lastErrorMaxBytes is provision_task.last_error's width. VARCHAR(512) counts
// characters, so 512 bytes always fits whatever the characters are.
const lastErrorMaxBytes = 512

// truncateUTF8 cuts s to at most max bytes without splitting a rune.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
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
