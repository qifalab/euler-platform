package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/qifalab/euler-platform/storage"
)

// sqlStore is the MySQL-backed orgStore over account_db's org_project,
// org_project_resource and org_tag (03§4.1.2, account_id sharded).
//
// # Ids: the schema decides, not the client
//
// V1 declares org_project.project_id as a 号段-issued BIGINT (04§6.6), while the
// phase-1 handler minted UUIDs and even accepted a client-supplied id. This store
// always issues the id from account_db.id_sequence and writes it back through the
// pointer the port takes. A client-supplied id is never used, numeric or not:
// project_id is the table's global primary key, so honouring one let a tenant
// take an id the allocator would later hand to another tenant's create (which
// then failed as a "name conflict").
//
// # Account ids are numbers in the schema, strings in the service
//
// The header carries the account as a string and the handlers pass it straight
// through, but every column here is BIGINT UNSIGNED. The conversion is therefore
// validated once, in this store, with an error the handler can surface — a
// non-numeric account id cannot be stored, and pretending otherwise would put the
// row on an unintended shard.
type sqlStore struct {
	db    *sql.DB
	newID func() int64
}

var _ orgStore = (*sqlStore)(nil)

// statementTimeout bounds one store call; the port has no context parameter.
const statementTimeout = 10 * time.Second

// errInvalidAccount reports an account id that cannot be a BIGINT key.
var errInvalidAccount = errors.New("org: account id must be numeric")

// newStore picks the backend. Persistence is opt-in (pkg-go/storage doc): with
// EULER_DB_DSN set, projects, resource membership and tags live in account_db, so a
// restart keeps the org tree; unset, the in-memory store keeps the demo and
// `go test` dependency-free.
//
// A configured DSN that cannot be reached — or a schema sqlmigrate never touched —
// is a startup failure rather than a runtime surprise on the first project create.
func newStore(ctx context.Context) (orgStore, error) {
	db, ok, err := storage.MustOpenFor(ctx, "account_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newMemStore(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "account_db"); err != nil {
		return nil, err
	}
	return newSQLStore(ctx, db)
}

// persistentStore reports whether a store is backed by MySQL, for the startup log.
func persistentStore(s orgStore) bool {
	_, ok := s.(*sqlStore)
	return ok
}

// newSQLStore wires the store to its project-id sequence (seeded by account_db V2).
func newSQLStore(ctx context.Context, db *sql.DB) (*sqlStore, error) {
	ids, err := storage.OpenSequence(ctx, db, "org_project", 1000)
	if err != nil {
		return nil, err
	}
	return &sqlStore{db: db, newID: ids.NextFunc()}, nil
}

// accountID converts the service's string account id into the schema's key.
func numericAccountID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("%w: %q", errInvalidAccount, raw)
	}
	return id, nil
}

// projectIDString converts a stored numeric id into the string the API uses.
func projectIDString(id int64) string { return strconv.FormatInt(id, 10) }

// noProject is the "self" of a project that does not exist yet (a create):
// allocator ids start at 1, so no parent chain can reach it.
const noProject int64 = -1

// parseParent converts a parent id from the API; a non-numeric one cannot name
// any project in this schema.
func parseParent(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: parent project not found", errInvalidParent)
	}
	return id, nil
}

// checkParentTx validates parent as the parent of self within the account: it
// must exist in that account, must not be self, and walking up from it must not
// reach self (that would close a cycle). The walk is bounded by maxProjectDepth.
func checkParentTx(ctx context.Context, tx *sql.Tx, acct, self, parent int64) error {
	if parent == self {
		return fmt.Errorf("%w: a project cannot be its own parent", errInvalidParent)
	}
	cur := parent
	for depth := 0; depth < maxProjectDepth; depth++ {
		var next sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT parent_id FROM org_project WHERE project_id = ? AND account_id = ?`,
			cur, acct).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			if depth == 0 {
				return fmt.Errorf("%w: parent project not found", errInvalidParent)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("org: read ancestor %d: %w", cur, err)
		}
		if !next.Valid {
			return nil
		}
		if next.Int64 == self {
			return fmt.Errorf("%w: the parent is a descendant of this project", errInvalidParent)
		}
		cur = next.Int64
	}
	return fmt.Errorf("%w: project tree is deeper than %d levels", errInvalidParent, maxProjectDepth)
}

// CreateProject inserts a project. The store owns p.ProjectID: whatever the
// caller put there is replaced by a 号段 id.
func (s *sqlStore) CreateProject(p *project) error {
	acct, err := numericAccountID(p.AccountID)
	if err != nil {
		return err
	}
	var parent any
	var parentID int64
	if p.ParentID != "" {
		if parentID, err = parseParent(p.ParentID); err != nil {
			return err
		}
		parent = parentID
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	id := s.newID()
	err = storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if parent != nil {
			if err := checkParentTx(ctx, tx, acct, noProject, parentID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO org_project (project_id, account_id, name, parent_id, status, version)
			 VALUES (?, ?, ?, ?, 1, 0)`,
			id, acct, p.Name, parent); err != nil {
			// The migration comment on uk_acc_name says it plainly: project names
			// are unique per account so a create retry cannot build a second
			// project. The id is the allocator's, so a duplicate here is that
			// name rule — the port's "already exists".
			if storage.IsDuplicateKey(err) {
				return errProjectExists
			}
			return fmt.Errorf("org: create project %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	p.ProjectID = projectIDString(id)
	return nil
}

func (s *sqlStore) GetProject(accountRaw, projectRaw string) (project, error) {
	acct, err := numericAccountID(accountRaw)
	if err != nil {
		return project{}, err
	}
	pid, err := strconv.ParseInt(projectRaw, 10, 64)
	if err != nil {
		// A non-numeric id cannot exist in this schema, so it is a miss, not an
		// error: the handler answers 404, which is what a caller asking for a
		// UUID project in a numeric-id world should see.
		return project{}, errProjectNotFound
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	var (
		name   string
		parent sql.NullInt64
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT name, parent_id FROM org_project WHERE project_id = ? AND account_id = ?`,
		pid, acct).Scan(&name, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return project{}, errProjectNotFound
	}
	if err != nil {
		return project{}, fmt.Errorf("org: get project %s: %w", projectRaw, err)
	}
	p := project{ProjectID: projectRaw, AccountID: accountRaw, Name: name}
	if parent.Valid {
		p.ParentID = projectIDString(parent.Int64)
	}
	return p, nil
}

func (s *sqlStore) ListProjects(accountRaw string) ([]project, error) {
	acct, err := numericAccountID(accountRaw)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id, name, parent_id
		   FROM org_project
		  WHERE account_id = ?
		  ORDER BY project_id`, acct)
	if err != nil {
		return nil, fmt.Errorf("org: list projects for %s: %w", accountRaw, err)
	}
	defer rows.Close()

	out := make([]project, 0, 8)
	for rows.Next() {
		var (
			id     int64
			name   string
			parent sql.NullInt64
		)
		if err := rows.Scan(&id, &name, &parent); err != nil {
			return nil, fmt.Errorf("org: scan project for %s: %w", accountRaw, err)
		}
		p := project{ProjectID: projectIDString(id), AccountID: accountRaw, Name: name}
		if parent.Valid {
			p.ParentID = projectIDString(parent.Int64)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org: list projects for %s: %w", accountRaw, err)
	}
	return out, nil
}

// UpdateProject applies the rename/reparent under the optimistic lock. A partial
// update keeps the untouched column: a nil pointer means "leave it".
func (s *sqlStore) UpdateProject(accountRaw, projectRaw string, name *string, parentID *string) error {
	acct, err := numericAccountID(accountRaw)
	if err != nil {
		return err
	}
	pid, err := strconv.ParseInt(projectRaw, 10, 64)
	if err != nil {
		return errProjectNotFound
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var version int
		err := tx.QueryRowContext(ctx,
			`SELECT version FROM org_project WHERE project_id = ? AND account_id = ? FOR UPDATE`,
			pid, acct).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return errProjectNotFound
		}
		if err != nil {
			return fmt.Errorf("org: lock project %s: %w", projectRaw, err)
		}

		nextName, nextParent := sql.NullString{}, sql.NullInt64{}
		cur := tx.QueryRowContext(ctx,
			`SELECT name, parent_id FROM org_project WHERE project_id = ?`, pid)
		var curParent sql.NullInt64
		if err := cur.Scan(&nextName, &curParent); err != nil {
			return fmt.Errorf("org: read project %s: %w", projectRaw, err)
		}
		if name != nil {
			nextName = sql.NullString{String: *name, Valid: true}
		}
		if parentID != nil {
			if *parentID == "" {
				nextParent = sql.NullInt64{}
			} else {
				parent, err := parseParent(*parentID)
				if err != nil {
					return err
				}
				if err := checkParentTx(ctx, tx, acct, pid, parent); err != nil {
					return err
				}
				nextParent = sql.NullInt64{Int64: parent, Valid: true}
			}
		} else {
			nextParent = curParent
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE org_project SET name = ?, parent_id = ?, version = ?
			  WHERE project_id = ? AND account_id = ? AND version = ?`,
			nextName.String, nullableInt(nextParent), version+1, pid, acct, version)
		if err != nil {
			// uk_acc_name: renaming onto another project's name is the same
			// conflict a duplicate create is, not a server failure.
			if storage.IsDuplicateKey(err) {
				return errProjectExists
			}
			return fmt.Errorf("org: update project %s: %w", projectRaw, err)
		}
		return storage.Affected(res, nil)
	})
}

// MoveResource moves a resource between projects, registering it on first sight.
//
// The uk_resource index — one resource belongs to exactly one project — is what
// the memory store enforces with a map; here the row itself is the arbiter, so two
// concurrent moves cannot both win and leave the resource in two projects.
//
// uk_resource is global (resource_id alone), so every read and write of the
// membership row is scoped by account_id: unscoped, a tenant could move another
// tenant's resource row into its own project, and the mismatch error echoed the
// other tenant's project id. A resource whose row belongs to another account is
// errResourceNotFound — the same answer as for any id the caller cannot see.
func (s *sqlStore) MoveResource(accountRaw, resourceID, fromProjectID, toProjectID string) error {
	acct, err := numericAccountID(accountRaw)
	if err != nil {
		return err
	}
	from, err := strconv.ParseInt(fromProjectID, 10, 64)
	if err != nil {
		return errProjectNotFound
	}
	to, err := strconv.ParseInt(toProjectID, 10, 64)
	if err != nil {
		return errProjectNotFound
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		// Both ends must be this account's projects: a resource must not be moved
		// into (or out of) a project the caller cannot see.
		for _, id := range []int64{from, to} {
			var exists int
			err := tx.QueryRowContext(ctx,
				`SELECT 1 FROM org_project WHERE project_id = ? AND account_id = ?`, id, acct).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				return errProjectNotFound
			}
			if err != nil {
				return fmt.Errorf("org: check project %d: %w", id, err)
			}
		}

		var current int64
		err := tx.QueryRowContext(ctx,
			`SELECT project_id FROM org_project_resource
			  WHERE resource_id = ? AND account_id = ? FOR UPDATE`,
			resourceID, acct).Scan(&current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err := tx.ExecContext(ctx,
				`INSERT INTO org_project_resource (resource_id, project_id, account_id)
				 VALUES (?, ?, ?)`, resourceID, to, acct)
			if storage.IsDuplicateKey(err) {
				// Registered under another account: not this caller's to see.
				return errResourceNotFound
			}
			if err != nil {
				return fmt.Errorf("org: register resource %s: %w", resourceID, err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("org: read resource %s: %w", resourceID, err)
		}
		if current != from {
			return fmt.Errorf("%w: resource %s is in project %d, not %s", errResourceConflict, resourceID, current, fromProjectID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE org_project_resource SET project_id = ? WHERE resource_id = ? AND account_id = ?`,
			to, resourceID, acct); err != nil {
			return fmt.Errorf("org: move resource %s: %w", resourceID, err)
		}
		return nil
	})
}

// PutTag upserts by (account_id, tag_key): uk_acc_key makes a retry a no-op
// update rather than a second tag.
func (s *sqlStore) PutTag(t tag) error {
	acct, err := numericAccountID(t.AccountID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	// ON DUPLICATE KEY UPDATE with the value bound twice keeps this a single
	// statement without the deprecated VALUES() spelling or the row alias, which
	// MySQL versions and the Vitess parser disagree about.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO org_tag (account_id, tag_key, tag_value)
		 VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE tag_value = ?`,
		acct, t.Key, t.Value, t.Value); err != nil {
		return fmt.Errorf("org: put tag %s: %w", t.Key, err)
	}
	return nil
}

func (s *sqlStore) DeleteTag(accountRaw, key string) error {
	acct, err := numericAccountID(accountRaw)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	res, err := s.db.ExecContext(ctx,
		`DELETE FROM org_tag WHERE account_id = ? AND tag_key = ?`, acct, key)
	if err != nil {
		return fmt.Errorf("org: delete tag %s: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("org: delete tag %s: %w", key, err)
	}
	if n == 0 {
		return errTagNotFound
	}
	return nil
}

func (s *sqlStore) ListTags(accountRaw string) ([]tag, error) {
	acct, err := numericAccountID(accountRaw)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT tag_key, tag_value FROM org_tag WHERE account_id = ? ORDER BY tag_key`, acct)
	if err != nil {
		return nil, fmt.Errorf("org: list tags for %s: %w", accountRaw, err)
	}
	defer rows.Close()

	out := make([]tag, 0, 8)
	for rows.Next() {
		t := tag{AccountID: accountRaw}
		if err := rows.Scan(&t.Key, &t.Value); err != nil {
			return nil, fmt.Errorf("org: scan tag for %s: %w", accountRaw, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org: list tags for %s: %w", accountRaw, err)
	}
	return out, nil
}

// nullableInt writes a NULL for an unset parent_id.
func nullableInt(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}
