package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/qifalab/euler-platform/provision"
	"github.com/qifalab/euler-platform/resource"
	"github.com/qifalab/euler-platform/storage"
)

// Ledger extensions: resource attributes (resource_instance_attr), globally
// unique names (resource_name_claim, V5), the compensation claim, the
// poller's scan, the applied-spec lookup and backup snapshots
// (resource_backup, V5). Each section carries both backends side by side so
// their parity is reviewable in one place.

// errResourceNotFound: the account has no such resource.
var errResourceNotFound = errors.New("resource: not found")

// stateLogReasonMax is resource_state_log.reason's width (VARCHAR(256)).
const stateLogReasonMax = 256

// attrValueMax is resource_instance_attr.attr_value's width (VARCHAR(1024)).
const attrValueMax = 1024

// truncateRunes cuts s to at most max characters (VARCHAR widths count
// characters, not bytes).
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max])
}

// --- create options -----------------------------------------------------------

// createOptions is what Create writes beside the resource row.
type createOptions struct {
	attrs     map[string]string
	claims    []nameClaim
	peerCheck func(peers []peerResource) error
}

// createOption configures one Create.
type createOption func(*createOptions)

func applyCreateOptions(opts []createOption) createOptions {
	var o createOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// withAttrs stores the resource's attributes with the row.
func withAttrs(attrs map[string]string) createOption {
	return func(o *createOptions) { o.attrs = attrs }
}

// withNameClaim takes a name that must be unique within (namespace, region)
// across every account — an euoss bucket. The claim is freed when the resource
// leaves for CREATE_FAILED or RELEASED.
func withNameClaim(c nameClaim) createOption {
	return func(o *createOptions) { o.claims = append(o.claims, c) }
}

// withPeerCheck runs check over the account's other live resources of the same
// product in the same region (with their attributes) inside the create's
// critical section — the memory ledger's lock, or the SQL transaction holding
// those peers' rows (and the index gap after them) locked until commit, so two
// concurrent creates cannot both pass a check that only one of them may pass.
// The check's error aborts the create and is returned as is.
func withPeerCheck(check func(peers []peerResource) error) createOption {
	return func(o *createOptions) { o.peerCheck = check }
}

// nameClaim is one globally unique name.
type nameClaim struct {
	Namespace string // e.g. claimBucket
	Name      string
}

// claimBucket is the euoss bucket namespace: unique per region.
const claimBucket = "euoss.bucket"

// nameTakenError reports a claim another resource already holds.
type nameTakenError struct {
	Claim  nameClaim
	Region string
	Holder string // the holder's resource id when known (memory ledger)
}

func (e *nameTakenError) Error() string {
	return fmt.Sprintf("resource: %s %q is already taken in %s", e.Claim.Namespace, e.Claim.Name, e.Region)
}

// peerResource is one resource a peer check sees.
type peerResource struct {
	ResourceID string
	State      resource.State
	Attrs      map[string]string
}

// occupiesPeerSlot reports whether a resource in state still holds what a
// peer check guards (a VPC's address range): everything but a resource that
// never came up or is gone. A RELEASING VPC still owns its range until it is
// RELEASED.
func occupiesPeerSlot(st resource.State) bool {
	return st != resource.StateReleased && st != resource.StateCreateFailed
}

// releasesClaims reports whether entering `to` frees the resource's names.
func releasesClaims(to resource.State) bool {
	return to == resource.StateCreateFailed || to == resource.StateReleased
}

func claimKey(c nameClaim, region string) string {
	return c.Namespace + "|" + region + "|" + c.Name
}

// dropClaimsLocked frees every name the resource holds. Caller holds s.mu.
func (s *memLedger) dropClaimsLocked(resourceID string) {
	for k, holder := range s.claims {
		if holder == resourceID {
			delete(s.claims, k)
		}
	}
}

func cloneAttrs(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// lockPeers reads the account's live resources of inst's product in its region
// FOR UPDATE (idx_acc_prod: the rows and the gap a concurrent insert of the
// same account+product would land in), with their attributes.
func lockPeers(ctx context.Context, tx *sql.Tx, inst *resource.Instance) ([]peerResource, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT resource_id, status FROM resource_instance
		  WHERE account_id = ? AND product_code = ? AND region = ?
		  ORDER BY resource_id FOR UPDATE`,
		inst.AccountID, inst.ProductCode, inst.Region)
	if err != nil {
		return nil, fmt.Errorf("resource: lock peers of %s: %w", inst.ResourceID, err)
	}
	var peers []peerResource
	for rows.Next() {
		var p peerResource
		if err := rows.Scan(&p.ResourceID, &p.State); err != nil {
			rows.Close()
			return nil, fmt.Errorf("resource: scan peer of %s: %w", inst.ResourceID, err)
		}
		if occupiesPeerSlot(p.State) {
			peers = append(peers, p)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resource: lock peers of %s: %w", inst.ResourceID, err)
	}
	for i := range peers {
		attrs, err := queryAttrs(ctx, tx, inst.AccountID, peers[i].ResourceID)
		if err != nil {
			return nil, err
		}
		peers[i].Attrs = attrs
	}
	return peers, nil
}

// --- attributes ---------------------------------------------------------------

// attrUpdate is one UpdateAttrs call.
type attrUpdate struct {
	Set    map[string]string
	Delete []string
	// Check runs under the row lock with the resource's current state and
	// attributes; its error aborts the update and is returned as is.
	Check func(state resource.State, current map[string]string) error
	// Journal, when set, is recorded as a resource_state_log row from the
	// current state to itself.
	Journal string
}

// queryer is what *sql.DB and *sql.Tx share for reads.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryAttrs(ctx context.Context, q queryer, accountID int64, resourceID string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT attr_key, attr_value FROM resource_instance_attr WHERE resource_id = ? AND account_id = ?`,
		resourceID, accountID)
	if err != nil {
		return nil, fmt.Errorf("resource: attributes of %s: %w", resourceID, err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k string
		var v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("resource: scan attribute of %s: %w", resourceID, err)
		}
		out[k] = v.String
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resource: attributes of %s: %w", resourceID, err)
	}
	return out, nil
}

// upsertAttrs writes attrs, one statement per key in key order (a stable lock
// order). The value is bound twice rather than spelled VALUES() (deprecated in
// 8.0) or with the 8.0.19 row alias (unknown to 5.7) — the svc-org convention.
func upsertAttrs(ctx context.Context, tx *sql.Tx, accountID int64, resourceID string, attrs map[string]string) error {
	for _, k := range sortedKeys(attrs) {
		v := truncateRunes(attrs[k], attrValueMax)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO resource_instance_attr (resource_id, account_id, attr_key, attr_value)
			 VALUES (?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE attr_value = ?`,
			resourceID, accountID, k, v, v); err != nil {
			return fmt.Errorf("resource: write attribute %s of %s: %w", k, resourceID, err)
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s *memLedger) Attrs(accountID int64, resourceID string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	inst, ok := s.resources[resourceID]
	if !ok || inst.AccountID != accountID {
		return map[string]string{}, nil
	}
	return cloneAttrs(s.attrs[resourceID]), nil
}

func (s *memLedger) AccountAttrs(accountID int64) (map[string]map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]map[string]string)
	for id, inst := range s.resources {
		if inst.AccountID == accountID && len(s.attrs[id]) > 0 {
			out[id] = cloneAttrs(s.attrs[id])
		}
	}
	return out, nil
}

func (s *memLedger) UpdateAttrs(accountID int64, resourceID string, u attrUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.resources[resourceID]
	if !ok || inst.AccountID != accountID {
		return errResourceNotFound
	}
	if u.Check != nil {
		if err := u.Check(inst.State, cloneAttrs(s.attrs[resourceID])); err != nil {
			return err
		}
	}
	next := cloneAttrs(s.attrs[resourceID])
	for k, v := range u.Set {
		next[k] = truncateRunes(v, attrValueMax)
	}
	for _, k := range u.Delete {
		delete(next, k)
	}
	s.attrs[resourceID] = next
	return nil
}

func (s *memLedger) MarkCompensation(inst *resource.Instance, reason string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.resources[inst.ResourceID]
	if !ok || stored.AccountID != inst.AccountID {
		return fmt.Errorf("resource: %s not found", inst.ResourceID)
	}
	if stored.State != resource.StateCreating || stored.Version != inst.Version {
		return fmt.Errorf("%w: %s is %s v%d, caller saw %s v%d", resource.ErrVersionConflict,
			inst.ResourceID, stored.State, stored.Version, inst.State, inst.Version)
	}
	attrs := cloneAttrs(s.attrs[inst.ResourceID])
	if on {
		attrs[attrCompensating] = truncateRunes(reason, attrValueMax)
	} else {
		delete(attrs, attrCompensating)
	}
	s.attrs[inst.ResourceID] = attrs
	next := cloneInstance(stored)
	next.Version++
	s.resources[inst.ResourceID] = next
	*inst = *next
	return nil
}

func (s *sqlLedger) Attrs(accountID int64, resourceID string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	return queryAttrs(ctx, s.db, accountID, resourceID)
}

func (s *sqlLedger) AccountAttrs(accountID int64) (map[string]map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT resource_id, attr_key, attr_value FROM resource_instance_attr WHERE account_id = ?`, accountID)
	if err != nil {
		return nil, fmt.Errorf("resource: attributes for account %d: %w", accountID, err)
	}
	defer rows.Close()
	out := make(map[string]map[string]string)
	for rows.Next() {
		var id, k string
		var v sql.NullString
		if err := rows.Scan(&id, &k, &v); err != nil {
			return nil, fmt.Errorf("resource: scan attributes for account %d: %w", accountID, err)
		}
		if out[id] == nil {
			out[id] = make(map[string]string)
		}
		out[id][k] = v.String
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resource: attributes for account %d: %w", accountID, err)
	}
	return out, nil
}

// UpdateAttrs serializes attribute writers on the resource row (SELECT ... FOR
// UPDATE): a check that validates the merged attributes (the euas policy) sees
// the attributes the write will land on, and a concurrent lifecycle edge waits
// for, or is waited on by, the update.
func (s *sqlLedger) UpdateAttrs(accountID int64, resourceID string, u attrUpdate) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var status resource.State
		err := tx.QueryRowContext(ctx,
			`SELECT status FROM resource_instance WHERE resource_id = ? AND account_id = ? FOR UPDATE`,
			resourceID, accountID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return errResourceNotFound
		}
		if err != nil {
			return fmt.Errorf("resource: lock %s: %w", resourceID, err)
		}
		if u.Check != nil {
			cur, err := queryAttrs(ctx, tx, accountID, resourceID)
			if err != nil {
				return err
			}
			if err := u.Check(status, cur); err != nil {
				return err
			}
		}
		if err := upsertAttrs(ctx, tx, accountID, resourceID, u.Set); err != nil {
			return err
		}
		for _, k := range u.Delete {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM resource_instance_attr WHERE resource_id = ? AND account_id = ? AND attr_key = ?`,
				resourceID, accountID, k); err != nil {
				return fmt.Errorf("resource: delete attribute %s of %s: %w", k, resourceID, err)
			}
		}
		if u.Journal != "" {
			return journal(ctx, tx, resourceID, accountID, status, status, u.Journal)
		}
		return nil
	})
}

// MarkCompensation bumps the version without touching updated_at (assigning
// the column to itself suppresses ON UPDATE CURRENT_TIMESTAMP): a claim is not
// progress, and the stuck-intermediate scans key on updated_at.
func (s *sqlLedger) MarkCompensation(inst *resource.Instance, reason string, on bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	verb := "compensation claimed: "
	if !on {
		verb = "compensation revoked: "
	}
	err := storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE resource_instance SET version = version + 1, updated_at = updated_at
			  WHERE resource_id = ? AND account_id = ? AND status = ? AND version = ?`,
			inst.ResourceID, inst.AccountID, string(resource.StateCreating), inst.Version)
		if err := storage.Affected(res, err); err != nil {
			return fmt.Errorf("resource: mark compensation of %s: %w", inst.ResourceID, err)
		}
		if on {
			if err := upsertAttrs(ctx, tx, inst.AccountID, inst.ResourceID,
				map[string]string{attrCompensating: reason}); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx,
			`DELETE FROM resource_instance_attr WHERE resource_id = ? AND account_id = ? AND attr_key = ?`,
			inst.ResourceID, inst.AccountID, attrCompensating); err != nil {
			return fmt.Errorf("resource: clear compensation of %s: %w", inst.ResourceID, err)
		}
		return journal(ctx, tx, inst.ResourceID, inst.AccountID,
			resource.StateCreating, resource.StateCreating, verb+reason)
	})
	if err != nil {
		return err
	}
	inst.Version++
	return nil
}

// --- poller scan and applied specs ----------------------------------------------

func (s *memLedger) ListByState(state resource.State, after string, limit int) ([]*resource.Instance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*resource.Instance
	for id, inst := range s.resources {
		if inst.State == state && id > after {
			out = append(out, cloneInstance(inst))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceID < out[j].ResourceID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memLedger) AppliedSpec(resourceID string) (provision.Spec, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	spec, ok := s.specs[resourceID]
	return spec, ok, nil
}

func (s *sqlLedger) ListByState(state resource.State, after string, limit int) ([]*resource.Instance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+instanceColumns+` FROM resource_instance
		  WHERE status = ? AND resource_id > ?
		  ORDER BY resource_id LIMIT ?`, string(state), after, limit)
	if err != nil {
		return nil, fmt.Errorf("resource: list %s: %w", state, err)
	}
	defer rows.Close()
	var out []*resource.Instance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("resource: scan %s: %w", state, err)
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resource: list %s: %w", state, err)
	}
	return out, nil
}

// AppliedSpec reads the spec of the resource's CREATE dispatch back from
// provision_task.params_json (the marshalled provision.Spec).
func (s *sqlLedger) AppliedSpec(resourceID string) (provision.Spec, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT params_json FROM provision_task
		  WHERE resource_id = ? AND op_type = 'CREATE'
		  ORDER BY task_id DESC LIMIT 1`, resourceID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return provision.Spec{}, false, nil
	}
	if err != nil {
		return provision.Spec{}, false, fmt.Errorf("resource: applied spec of %s: %w", resourceID, err)
	}
	var spec provision.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return provision.Spec{}, false, fmt.Errorf("resource: applied spec of %s: %w", resourceID, err)
	}
	return spec, true, nil
}

// --- backups --------------------------------------------------------------------

// backupRecord is one snapshot of a resource (resource_backup).
type backupRecord struct {
	BackupID   string
	ResourceID string
	AccountID  int64
	Status     string
	SizeGB     int // 0: unknown
	CreatedAt  time.Time
	// ExpiresAt is fixed when the snapshot is taken (backup.RetainUntil); the
	// snapshot is expired once now is after it.
	ExpiresAt time.Time
}

// backupNewestFirst orders snapshots newest first; the id (time-prefixed, see
// newBackupID) breaks ties inside one second.
func backupNewestFirst(list []backupRecord) {
	sort.Slice(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		}
		return list[i].BackupID > list[j].BackupID
	})
}

func (s *memLedger) CreateBackup(b backupRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.backups[b.BackupID]; exists {
		return fmt.Errorf("resource: duplicate backup id %s", b.BackupID)
	}
	s.backups[b.BackupID] = b
	return nil
}

func (s *memLedger) ListBackups(accountID int64, resourceID string, now time.Time) ([]backupRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]backupRecord, 0)
	for id, b := range s.backups {
		if b.AccountID != accountID || b.ResourceID != resourceID {
			continue
		}
		if b.ExpiresAt.Before(now) {
			delete(s.backups, id) // swept: past its retention
			continue
		}
		out = append(out, b)
	}
	backupNewestFirst(out)
	return out, nil
}

func (s *memLedger) DeleteBackup(accountID int64, resourceID, backupID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.backups[backupID]
	if !ok || b.AccountID != accountID || b.ResourceID != resourceID {
		return false, nil
	}
	delete(s.backups, backupID)
	return true, nil
}

func (s *sqlLedger) CreateBackup(b backupRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	var size any
	if b.SizeGB > 0 {
		size = b.SizeGB
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO resource_backup (backup_id, resource_id, account_id, status, size_gb, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.BackupID, b.ResourceID, b.AccountID, b.Status, size, b.CreatedAt.UTC(), b.ExpiresAt.UTC()); err != nil {
		return fmt.Errorf("resource: create backup %s of %s: %w", b.BackupID, b.ResourceID, err)
	}
	return nil
}

// ListBackups sweeps the resource's lapsed snapshots, then reads the rest. The
// sweep is the retention gate (06§4.1: retention is enforced, never
// unbounded); a failed sweep fails the read rather than show lapsed backups.
func (s *sqlLedger) ListBackups(accountID int64, resourceID string, now time.Time) ([]backupRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM resource_backup WHERE resource_id = ? AND account_id = ? AND expires_at < ?`,
		resourceID, accountID, now.UTC()); err != nil {
		return nil, fmt.Errorf("resource: sweep backups of %s: %w", resourceID, err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT backup_id, resource_id, account_id, status, size_gb, created_at, expires_at
		   FROM resource_backup
		  WHERE resource_id = ? AND account_id = ? AND expires_at >= ?
		  ORDER BY created_at DESC, backup_id DESC`, resourceID, accountID, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("resource: list backups of %s: %w", resourceID, err)
	}
	defer rows.Close()
	out := make([]backupRecord, 0)
	for rows.Next() {
		var b backupRecord
		var size sql.NullInt64
		if err := rows.Scan(&b.BackupID, &b.ResourceID, &b.AccountID, &b.Status, &size,
			&b.CreatedAt, &b.ExpiresAt); err != nil {
			return nil, fmt.Errorf("resource: scan backup of %s: %w", resourceID, err)
		}
		b.SizeGB = int(size.Int64)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resource: list backups of %s: %w", resourceID, err)
	}
	return out, nil
}

func (s *sqlLedger) DeleteBackup(accountID int64, resourceID, backupID string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM resource_backup WHERE backup_id = ? AND resource_id = ? AND account_id = ?`,
		backupID, resourceID, accountID)
	if err != nil {
		return false, fmt.Errorf("resource: delete backup %s: %w", backupID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("resource: delete backup %s: %w", backupID, err)
	}
	return n > 0, nil
}
