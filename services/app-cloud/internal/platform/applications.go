package platform

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) RequireProject(ctx context.Context, user, tenant, project, level string) error {
	return requireProject(ctx, s.db, user, tenant, project, level)
}

func (s *Store) Installations(ctx context.Context, user, tenant, project string) ([]Installation, error) {
	if e := requireProject(ctx, s.db, user, tenant, project, "read"); e != nil {
		return nil, e
	}
	rows, e := s.db.QueryContext(ctx, `SELECT i.id,i.tenant_id,i.project_id,i.application_id,i.status,i.created_at,c.base_url,c.updated_at FROM installations i LEFT JOIN connections c ON c.installation_id=i.id WHERE i.tenant_id=? AND i.project_id=? ORDER BY i.created_at,i.id`, tenant, project)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Installation{}
	for rows.Next() {
		v, e := scanInstallation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanInstallation(row scanner) (Installation, error) {
	var v Installation
	var n int64
	var base sql.NullString
	var updated sql.NullInt64
	e := row.Scan(&v.ID, &v.TenantID, &v.ProjectID, &v.ApplicationID, &v.Status, &n, &base, &updated)
	if errors.Is(e, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	v.CreatedAt = fromStamp(n)
	if base.Valid {
		v.Connection = &ConnectionInfo{BaseURL: base.String, Configured: true, UpdatedAt: fromStamp(updated.Int64)}
	}
	return v, e
}
func installation(ctx context.Context, q queryer, tenant, project, id string) (Installation, error) {
	return scanInstallation(q.QueryRowContext(ctx, `SELECT i.id,i.tenant_id,i.project_id,i.application_id,i.status,i.created_at,c.base_url,c.updated_at FROM installations i LEFT JOIN connections c ON c.installation_id=i.id WHERE i.id=? AND i.tenant_id=? AND i.project_id=?`, id, tenant, project))
}
func (s *Store) Installation(ctx context.Context, user, tenant, project, id, level string) (Installation, error) {
	if e := requireProject(ctx, s.db, user, tenant, project, level); e != nil {
		return Installation{}, e
	}
	return installation(ctx, s.db, tenant, project, id)
}
func (s *Store) EnableApplication(ctx context.Context, user, tenant, project, app string) (Installation, error) {
	v := Installation{ID: newID("app_"), TenantID: tenant, ProjectID: project, ApplicationID: app, Status: "enabled", CreatedAt: time.Now().UTC()}
	e := s.write(ctx, func(tx *sql.Tx) error {
		if e := requireProject(ctx, tx, user, tenant, project, "admin"); e != nil {
			return e
		}
		var existing string
		e := tx.QueryRowContext(ctx, "SELECT id FROM installations WHERE project_id=? AND application_id=?", project, app).Scan(&existing)
		if e == nil {
			return fmt.Errorf("%w: application is already enabled for this project", ErrConflict)
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO installations VALUES(?,?,?,?,?,?)", v.ID, tenant, project, app, v.Status, v.CreatedAt.UnixMilli())
		if e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, project, "application.enabled", v.ID, "Enabled application")
	})
	return v, e
}
func (s *Store) SetInstallationStatus(ctx context.Context, user, tenant, project, id, status string) error {
	if status != "enabled" && status != "disabled" {
		return ErrInvalid
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if e := requireProject(ctx, tx, user, tenant, project, "admin"); e != nil {
			return e
		}
		if _, e := installation(ctx, tx, tenant, project, id); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "UPDATE installations SET status=? WHERE id=?", status, id)
		if e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, project, "application."+status, id, "Updated application availability")
	})
}

// ConnectionFor is only used by authorized server-side connector calls.
func (s *Store) ConnectionFor(ctx context.Context, user, tenant, project, id, level string) (Installation, Connection, error) {
	v, e := s.Installation(ctx, user, tenant, project, id, level)
	if e != nil {
		return v, Connection{}, e
	}
	var c Connection
	var enc []byte
	var external sql.NullString
	e = s.db.QueryRowContext(ctx, "SELECT base_url,credential,external_account_id FROM connections WHERE installation_id=?", id).Scan(&c.BaseURL, &enc, &external)
	if errors.Is(e, sql.ErrNoRows) {
		return v, c, nil
	}
	if e != nil {
		return v, c, e
	}
	c.ExternalAccountID = external.String
	c.Credential, e = s.decrypt(enc, "connection:"+tenant+":"+project+":"+id)
	return v, c, e
}

// SaveConnection accepts the normalized, upstream-verified account identity from
// a connector, never the request's unverified externalAccountId. Both fingerprint
// and verified owner uniqueness prevent two tokens sharing an account across projects.
func (s *Store) SaveConnection(ctx context.Context, user, tenant, project, id string, c Connection) error {
	if c.BaseURL == "" {
		return ErrInvalid
	}
	if len(c.Credential) > 65536 {
		return ErrInvalid
	}
	enc, e := s.encrypt(c.Credential, "connection:"+tenant+":"+project+":"+id)
	if e != nil {
		return e
	}
	var fp, external any
	if c.Credential != "" {
		fp = s.fingerprint(c.Credential)
	}
	if c.ExternalAccountID != "" {
		external = c.ExternalAccountID
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if e := requireProject(ctx, tx, user, tenant, project, "admin"); e != nil {
			return e
		}
		v, e := installation(ctx, tx, tenant, project, id)
		if e != nil {
			return e
		}
		var conflict string
		e = tx.QueryRowContext(ctx, `SELECT installation_id FROM connections WHERE application_id=? AND installation_id<>? AND ((credential_fingerprint IS NOT NULL AND credential_fingerprint=?) OR (external_account_id IS NOT NULL AND external_account_id=? AND base_url=?)) LIMIT 1`, v.ApplicationID, id, fp, external, c.BaseURL).Scan(&conflict)
		if e == nil {
			return fmt.Errorf("%w: external account or credential already belongs to another project", ErrConflict)
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var oldBase string
		var oldOwner sql.NullString
		e = tx.QueryRowContext(ctx, "SELECT base_url,external_account_id FROM connections WHERE installation_id=?", id).Scan(&oldBase, &oldOwner)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e == nil && (oldBase != c.BaseURL || oldOwner.String != c.ExternalAccountID) {
			var n int
			if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM resource_bindings WHERE installation_id=?", id).Scan(&n); e != nil {
				return e
			}
			if n > 0 {
				return fmt.Errorf("%w: disconnect before replacing an account with tracked resources", ErrConflict)
			}
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO connections VALUES(?,?,?,?,?,?,?) ON CONFLICT(installation_id) DO UPDATE SET base_url=excluded.base_url,credential=excluded.credential,credential_fingerprint=excluded.credential_fingerprint,external_account_id=excluded.external_account_id,updated_at=excluded.updated_at`, id, v.ApplicationID, c.BaseURL, enc, fp, external, stamp())
		if e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, project, "connection.updated", id, "Updated encrypted application connection")
	})
}
func (s *Store) ClearConnection(ctx context.Context, user, tenant, project, id string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if e := requireProject(ctx, tx, user, tenant, project, "admin"); e != nil {
			return e
		}
		if _, e := installation(ctx, tx, tenant, project, id); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "DELETE FROM resource_bindings WHERE installation_id=?", id); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "DELETE FROM connections WHERE installation_id=?", id); e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, project, "connection.removed", id, "Disconnected application; remote resources were not deleted")
	})
}
func (s *Store) TrackResources(ctx context.Context, user, tenant, project, id string, resources []Resource) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if e := requireProject(ctx, tx, user, tenant, project, "read"); e != nil {
			return e
		}
		if _, e := installation(ctx, tx, tenant, project, id); e != nil {
			return e
		}
		for _, r := range resources {
			if strings.TrimSpace(r.ID) == "" {
				return ErrInvalid
			}
			_, e := tx.ExecContext(ctx, "INSERT INTO resource_bindings VALUES(?,?,?,?,?,?) ON CONFLICT(installation_id,resource_id) DO UPDATE SET name=excluded.name,type=excluded.type,status=excluded.status,url=excluded.url", id, r.ID, r.Name, r.Type, r.Status, r.URL)
			if e != nil {
				return e
			}
		}
		return nil
	})
}
func (s *Store) RequireResource(ctx context.Context, user, tenant, project, id, resource string) error {
	if _, e := s.Installation(ctx, user, tenant, project, id, "write"); e != nil {
		return e
	}
	var n string
	e := s.db.QueryRowContext(ctx, "SELECT resource_id FROM resource_bindings WHERE installation_id=? AND resource_id=?", id, resource).Scan(&n)
	if errors.Is(e, sql.ErrNoRows) {
		return ErrNotFound
	}
	return e
}
func (s *Store) ResourceCreated(ctx context.Context, user, tenant, project, id string, r Resource) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if e := requireProject(ctx, tx, user, tenant, project, "write"); e != nil {
			return e
		}
		if _, e := installation(ctx, tx, tenant, project, id); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO resource_bindings VALUES(?,?,?,?,?,?)", id, r.ID, r.Name, r.Type, r.Status, r.URL)
		if e != nil {
			return e
		}
		return audit(ctx, tx, user, tenant, project, "resource.created", r.ID, "Created application resource")
	})
}
func (s *Store) ResourceDeleted(ctx context.Context, user, tenant, project, id, resource string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if e := requireProject(ctx, tx, user, tenant, project, "write"); e != nil {
			return e
		}
		if _, e := installation(ctx, tx, tenant, project, id); e != nil {
			return e
		}
		res, e := tx.ExecContext(ctx, "DELETE FROM resource_bindings WHERE installation_id=? AND resource_id=?", id, resource)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, user, tenant, project, "resource.deleted", resource, "Deleted application resource")
	})
}
