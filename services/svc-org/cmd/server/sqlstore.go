package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/starcloud/sc-platform/storage"
)

// sqlStore is the MySQL-backed orgStore over account_db's org_project,
// org_project_resource and org_tag (03§4.1.2, account_id sharded).
//
// # Ids: the schema decides, not the client
//
// V1 declares org_project.project_id as a 号段-issued BIGINT (04§6.6), while the
// phase-1 handler minted UUIDs and even accepted a client-supplied id. A UUID has
// no representation in that column, so this store issues the id from
// account_db.id_sequence and writes it back through the pointer the port now
// takes. A numeric client-supplied id is honoured (it is a valid key); anything
// else is replaced by a server-issued id rather than silently stored as garbage
// or rejected after the caller already built a response around it.
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
// SC_DB_DSN set, projects, resource membership and tags live in account_db, so a
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

// CreateProject inserts a project. The store owns p.ProjectID: a non-numeric
// value (a phase-1 UUID, or empty) is replaced by a 号段 id.
func (s *sqlStore) CreateProject(p *project) error {
	acct, err := numericAccountID(p.AccountID)
	if err != nil {
		return err
	}
	if _, convErr := strconv.ParseInt(p.ProjectID, 10, 64); convErr != nil {
		p.ProjectID = projectIDString(s.newID())
	}
	var parent any
	if p.ParentID != "" {
		parentID, err := strconv.ParseInt(p.ParentID, 10, 64)
		if err != nil {
			return fmt.Errorf("org: parent project id must be numeric: %q", p.ParentID)
		}
		parent = parentID
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO org_project (project_id, account_id, name, parent_id, status, version)
		 VALUES (?, ?, ?, ?, 1, 0)`,
		p.ProjectID, acct, p.Name, parent); err != nil {
		// The migration comment on uk_acc_name says it plainly: project names are
		// unique per account so a create retry cannot build a second project. The
		// duplicate is reported as "already exists" — the port's own signal for
		// that case — rather than as a driver error.
		if storage.IsDuplicateKey(err) {
			return errProjectExists
		}
		return fmt.Errorf("org: create project %s: %w", p.ProjectID, err)
	}
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
				parent, err := strconv.ParseInt(*parentID, 10, 64)
				if err != nil {
					return fmt.Errorf("org: parent project id must be numeric: %q", *parentID)
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
			`SELECT project_id FROM org_project_resource WHERE resource_id = ? FOR UPDATE`,
			resourceID).Scan(&current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err := tx.ExecContext(ctx,
				`INSERT INTO org_project_resource (resource_id, project_id, account_id)
				 VALUES (?, ?, ?)`, resourceID, to, acct)
			if err != nil {
				return fmt.Errorf("org: register resource %s: %w", resourceID, err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("org: read resource %s: %w", resourceID, err)
		}
		if current != from {
			return fmt.Errorf("org: resource %s is in project %d, not %s", resourceID, current, fromProjectID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE org_project_resource SET project_id = ? WHERE resource_id = ?`,
			to, resourceID); err != nil {
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
