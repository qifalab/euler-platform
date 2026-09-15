package main

// The actionRepo persistence boundary for OpenAPI Action metadata (api_action,
// 03§4.5.1). The schema/error codes a registered Action declares are the source
// of truth the API gateway and the CI error-code gate validate against (03§9.3)
// — which is why they cannot live only in a process map: every svc-* service
// re-registers its Actions at startup, and the metadata must still be there
// when the console, the doc generator and the SDK build read it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/storage"
)

// actionRepo persists Action metadata.
type actionRepo interface {
	// Upsert registers the Action under its natural key
	// uk_product_action_version. Re-registration is the common path (every
	// service startup), so it must update the row rather than burn an id.
	Upsert(a *Action) error
	// Get returns the current metadata for "{product}.{Action}", or ok=false.
	Get(id string) (*Action, bool, error)
	// List returns Actions, optionally filtered by product, ordered by id.
	List(product string) ([]*Action, error)
}

// --- in-memory implementation ------------------------------------------------

type memActionRepo struct {
	mu      sync.RWMutex
	actions map[string]Action
}

func newMemActionRepo() *memActionRepo {
	return &memActionRepo{actions: make(map[string]Action)}
}

func (s *memActionRepo) Upsert(a *Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.actions[a.ID] = *a
	return nil
}

func (s *memActionRepo) Get(id string) (*Action, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.actions[id]
	if !ok {
		return nil, false, nil
	}
	return &a, true, nil
}

func (s *memActionRepo) List(product string) ([]*Action, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Action
	for _, a := range s.actions {
		if product == "" || a.ProductCode == product {
			copied := a
			out = append(out, &copied)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- SQL implementation ------------------------------------------------------

type sqlActionRepo struct {
	db *sql.DB
}

var _ actionRepo = (*sqlActionRepo)(nil)

func newSQLActionRepo(_ context.Context, db *sql.DB) *sqlActionRepo {
	return &sqlActionRepo{db: db}
}

// Upsert updates the published row first — a re-register is the common path —
// and only inserts when no row answers to the natural key. The schema of record
// keeps one published row per (product, action) in this wiring: the multi-
// revision flow the uk anticipates (version bump + status gating) is a later
// concern and would need a "list versions" API first.
func (s *sqlActionRepo) Upsert(a *Action) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	label := nullableLabel(a.Version)
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_action
		    SET param_schema_json = ?, error_codes_json = ?, api_version_label = ?,
		        updated_by = ?, updated_at = ?
		  WHERE product_code = ? AND action_name = ?`,
		string(a.ParamSchema), marshalErrorCodes(a.ErrorCodes), label, a.UpdatedBy,
		time.Now().UTC(), a.ProductCode, a.ActionName)
	if err != nil {
		return fmt.Errorf("apimeta: update action %s: %w", a.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO api_action
		   (product_code, action_name, api_version_label, param_schema_json, error_codes_json,
		    version, status, updated_by)
		 VALUES (?, ?, ?, ?, ?, 1, 1, ?)`,
		a.ProductCode, a.ActionName, label, string(a.ParamSchema),
		marshalErrorCodes(a.ErrorCodes), a.UpdatedBy); err != nil { // action_id comes from AUTO_INCREMENT (V3)
		if storage.IsDuplicateKey(err) {
			// uk_product_action_version: a concurrent register won the insert.
			// Retry the update once — the row the winner wrote answers to the
			// same natural key.
			return s.updateOnce(ctx, a, label)
		}
		return fmt.Errorf("apimeta: insert action %s: %w", a.ID, err)
	}
	return nil
}

func (s *sqlActionRepo) updateOnce(ctx context.Context, a *Action, label any) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE api_action
		    SET param_schema_json = ?, error_codes_json = ?, api_version_label = ?,
		        updated_by = ?, updated_at = ?
		  WHERE product_code = ? AND action_name = ?`,
		string(a.ParamSchema), marshalErrorCodes(a.ErrorCodes), label, a.UpdatedBy,
		time.Now().UTC(), a.ProductCode, a.ActionName); err != nil {
		return fmt.Errorf("apimeta: update action %s (after insert race): %w", a.ID, err)
	}
	return nil
}

func (s *sqlActionRepo) Get(id string) (*Action, bool, error) {
	product, action, ok := splitActionID(id)
	if !ok {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := scanAction(s.db.QueryRowContext(ctx,
		`SELECT product_code, action_name, api_version_label, param_schema_json,
		        error_codes_json, updated_by, updated_at
		   FROM api_action
		  WHERE product_code = ? AND action_name = ?
		  ORDER BY version DESC LIMIT 1`, product, action))
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("apimeta: get action %s: %w", id, err)
	}
	return a, true, nil
}

func (s *sqlActionRepo) List(product string) ([]*Action, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	query := `SELECT product_code, action_name, api_version_label, param_schema_json,
	         error_codes_json, updated_by, updated_at
	    FROM api_action`
	args := []any{}
	if product != "" {
		query += ` WHERE product_code = ?`
		args = append(args, product)
	}
	query += ` ORDER BY product_code, action_name`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("apimeta: list actions: %w", err)
	}
	defer rows.Close()

	out := make([]*Action, 0, 8)
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, fmt.Errorf("apimeta: scan action: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apimeta: list actions: %w", err)
	}
	return out, nil
}

func scanAction(sc interface{ Scan(...any) error }) (*Action, error) {
	var (
		a         Action
		label     sql.NullString
		schema    []byte
		codes     []byte
		updatedBy sql.NullString
		updatedAt time.Time
	)
	if err := sc.Scan(&a.ProductCode, &a.ActionName, &label, &schema, &codes, &updatedBy, &updatedAt); err != nil {
		return nil, err
	}
	a.ID = actionID(a.ProductCode, a.ActionName)
	a.Version = label.String
	a.ParamSchema = json.RawMessage(schema)
	if err := json.Unmarshal(codes, &a.ErrorCodes); err != nil {
		return nil, fmt.Errorf("apimeta: action %s error codes: %w", a.ID, err)
	}
	a.UpdatedBy = updatedBy.String
	a.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return &a, nil
}

// marshalErrorCodes keeps the JSON column NOT NULL even for an Action that
// declares no codes (an empty array, not SQL NULL — the CI gate reads it).
func marshalErrorCodes(codes []string) string {
	if len(codes) == 0 {
		return "[]"
	}
	b, err := json.Marshal(codes)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func nullableLabel(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// splitActionID splits "{product}.{Action}" — the first dot separates them
// (product codes never contain dots; Action names may).
func splitActionID(id string) (product, action string, ok bool) {
	i := strings.Index(id, ".")
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// newActionRepo picks the backend. Persistence is opt-in (pkg-go/storage doc):
// with EULER_DB_DSN set, Action metadata lives in openapi_meta so the registry
// survives a restart and stays identical for every reader; unset, the in-memory
// repo keeps the demo and `go test` dependency-free.
//
// The console-app registry (registry.go) deliberately stays in process memory:
// it is revisioned runtime configuration served from Nacos in production, not
// openapi_meta rows — no DDL table models it and inventing one would create a
// second source of truth for app routing.
func newActionRepo(ctx context.Context) (actionRepo, error) {
	db, ok, err := storage.MustOpenFor(ctx, "openapi_meta")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newMemActionRepo(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "openapi_meta"); err != nil {
		return nil, err
	}
	return newSQLActionRepo(ctx, db), nil
}

// persistentActionRepo reports whether a repo is backed by MySQL, for the
// startup log.
func persistentActionRepo(r actionRepo) bool {
	_, ok := r.(*sqlActionRepo)
	return ok
}
