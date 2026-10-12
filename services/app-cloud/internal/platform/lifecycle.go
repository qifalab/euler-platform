package platform

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
	"net/http"
	"time"
)

func projectAdministrator(ctx context.Context, q queryer, actor, tenant, project string) error {
	role, err := projectRole(ctx, q, actor, tenant, project)
	if err != nil {
		return err
	}
	if !admin(role) {
		return ErrForbidden
	}
	return nil
}
func (s *Store) Project(ctx context.Context, actor, tenant, project string) (Project, error) {
	role, err := projectRole(ctx, s.db, actor, tenant, project)
	if err != nil {
		return Project{}, err
	}
	var p Project
	var created int64
	err = s.db.QueryRowContext(ctx, `SELECT p.id,p.tenant_id,p.name,p.created_at,COALESCE(l.status,'active') FROM projects p LEFT JOIN project_lifecycle l ON l.project_id=p.id WHERE p.id=? AND p.tenant_id=?`, project, tenant).Scan(&p.ID, &p.TenantID, &p.Name, &created, &p.Status)
	p.Role = role
	p.CreatedAt = fromStamp(created)
	return p, err
}
func (s *Store) RenameProject(ctx context.Context, actor, tenant, project, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := projectAdministrator(ctx, tx, actor, tenant, project); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET name=? WHERE id=? AND tenant_id=?`, name, project, tenant); err != nil {
			return err
		}
		return audit(ctx, tx, actor, tenant, project, "project.renamed", project, "Updated project name")
	})
}
func (s *Store) SetProjectArchived(ctx context.Context, actor, tenant, project, confirmation string, archived bool) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := projectAdministrator(ctx, tx, actor, tenant, project); err != nil {
			return err
		}
		var name, status string
		err := tx.QueryRowContext(ctx, `SELECT p.name,COALESCE(l.status,'active') FROM projects p LEFT JOIN project_lifecycle l ON l.project_id=p.id WHERE p.id=? AND p.tenant_id=?`, project, tenant).Scan(&name, &status)
		if err != nil {
			return err
		}
		if confirmation != name {
			return fmt.Errorf("%w: confirm the exact project name", ErrInvalid)
		}
		target := "active"
		if archived {
			target = "archived"
		}
		if status == target {
			return nil
		}
		if archived {
			// Persist installation availability before suspension. Public routes and
			// workers that query installations directly also fail closed.
			if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO project_archived_installations SELECT id,status FROM installations WHERE tenant_id=? AND project_id=?`, tenant, project); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE installations SET status='disabled' WHERE tenant_id=? AND project_id=?`, tenant, project); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_lifecycle VALUES(?,?,?) ON CONFLICT(project_id) DO UPDATE SET status=excluded.status,updated_at=excluded.updated_at`, project, target, stamp()); err != nil {
			return err
		}
		if !archived {
			if _, err = tx.ExecContext(ctx, `UPDATE installations SET status=(SELECT previous_status FROM project_archived_installations WHERE installation_id=installations.id) WHERE tenant_id=? AND project_id=? AND id IN (SELECT installation_id FROM project_archived_installations)`, tenant, project); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM project_archived_installations WHERE installation_id IN (SELECT id FROM installations WHERE project_id=? AND tenant_id=?)`, project, tenant); err != nil {
				return err
			}
		}
		return audit(ctx, tx, actor, tenant, project, "project."+target, project, "Updated project availability; all business resources retained")
	})
}
func (s *Store) DeleteProject(ctx context.Context, actor, tenant, project, confirmation string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := projectAdministrator(ctx, tx, actor, tenant, project); err != nil {
			return err
		}
		var name, status string
		err := tx.QueryRowContext(ctx, `SELECT p.name,COALESCE(l.status,'active') FROM projects p LEFT JOIN project_lifecycle l ON l.project_id=p.id WHERE p.id=? AND p.tenant_id=?`, project, tenant).Scan(&name, &status)
		if err != nil {
			return err
		}
		if name != confirmation {
			return fmt.Errorf("%w: confirm the exact project name", ErrInvalid)
		}
		if status != "archived" {
			return fmt.Errorf("%w: archive the project before deletion", ErrConflict)
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM installations WHERE project_id=?)+(SELECT count(*) FROM service_accounts WHERE project_id=?)`, project, project).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%w: project retains applications or service accounts; keep it archived to preserve resources and history", ErrConflict)
		}
		if err = audit(ctx, tx, actor, tenant, project, "project.deleted", project, "Deleted empty archived project; audit retained"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM projects WHERE id=? AND tenant_id=?`, project, tenant)
		return err
	})
}
func (s *Store) DeleteTenant(ctx context.Context, actor, tenant, confirmation string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		role, err := tenantRole(ctx, tx, actor, tenant)
		if err != nil {
			return err
		}
		if role != "owner" {
			return ErrForbidden
		}
		var name string
		if err = tx.QueryRowContext(ctx, `SELECT name FROM tenants WHERE id=?`, tenant).Scan(&name); err != nil {
			return err
		}
		if name != confirmation {
			return ErrInvalid
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE tenant_id=?`, tenant).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%w: delete empty projects first; projects with resources must be retained", ErrConflict)
		}
		// Empty-team removal has no application data. Save its final audit event in a
		// tombstone table because the tenant's ordinary audit has a cascading FK.
		if _, err = tx.ExecContext(ctx, `INSERT INTO deleted_tenants VALUES(?,?,?,?,?)`, tenant, name, actor, stamp(), "Deleted empty team after explicit confirmation"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM tenants WHERE id=?`, tenant)
		return err
	})
}

// The export is an inventory. Secrets and application contents remain in the
// deployment backup, where encryption and retention policies can be preserved.
type ProjectExport struct {
	SchemaVersion   string           `json:"schemaVersion"`
	ExportedAt      time.Time        `json:"exportedAt"`
	Project         Project          `json:"project"`
	Installations   []Installation   `json:"installations"`
	Members         []Member         `json:"members"`
	ServiceAccounts []ServiceAccount `json:"serviceAccounts"`
	RetentionPolicy string           `json:"retentionPolicy"`
	Excludes        []string         `json:"excludes"`
}

func (s *Store) ExportProject(ctx context.Context, actor, tenant, project string) (ProjectExport, error) {
	v := ProjectExport{SchemaVersion: "euler-project-inventory/v1", ExportedAt: time.Now().UTC(), RetentionPolicy: "Archive suspends Euler application entry points and workers while retaining resources. Existing external database connections, public object URLs and already-issued presigned URLs are not revoked. Deletion requires an empty archived project.", Excludes: []string{"secrets", "personal verification materials", "application business data", "database contents", "object bytes", "device evidence"}}
	if err := projectAdministrator(ctx, s.db, actor, tenant, project); err != nil {
		return v, err
	}
	var err error
	if v.Project, err = s.Project(ctx, actor, tenant, project); err != nil {
		return v, err
	}
	if v.Installations, err = s.Installations(ctx, actor, tenant, project); err != nil {
		return v, err
	}
	if v.Members, err = s.ProjectMembers(ctx, actor, tenant, project); err != nil {
		return v, err
	}
	if v.ServiceAccounts, err = s.ServiceAccounts(ctx, actor, tenant, project); err != nil {
		return v, err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		return audit(ctx, tx, actor, tenant, project, "project.exported", project, "Exported project inventory without secrets or business data")
	})
	return v, err
}
func (h *Handler) lifecycleRoutes() {
	base := "/api/v1/tenants/{tenantID}/projects/{projectID}"
	h.route("GET "+base, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, pr, _ := scope(r)
		v, e := h.store.Project(r.Context(), p.ID, t, pr)
		if e == nil {
			writeJSON(w, 200, v)
		}
		return e
	})
	h.route("PATCH "+base, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			Name string `json:"name"`
		}
		if e := decode(w, r, &b); e != nil {
			return e
		}
		t, pr, _ := scope(r)
		if e := h.store.RenameProject(r.Context(), p.ID, t, pr, b.Name); e != nil {
			return e
		}
		v, e := h.store.Project(r.Context(), p.ID, t, pr)
		if e == nil {
			writeJSON(w, 200, v)
		}
		return e
	})
	for _, action := range []string{"archive", "restore", "delete"} {
		action := action
		pattern := "POST " + base + "/" + action
		if action == "delete" {
			pattern = "DELETE " + base
		}
		h.route(pattern, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
			var b struct {
				Confirmation string `json:"confirmation"`
			}
			if e := decode(w, r, &b); e != nil {
				return e
			}
			t, pr, _ := scope(r)
			var e error
			if action == "delete" {
				e = h.store.DeleteProject(r.Context(), p.ID, t, pr, b.Confirmation)
			} else {
				e = h.store.SetProjectArchived(r.Context(), p.ID, t, pr, b.Confirmation, action == "archive")
			}
			if e == nil {
				w.WriteHeader(204)
			}
			return e
		})
	}
	h.route("GET "+base+"/export", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, pr, _ := scope(r)
		v, e := h.store.ExportProject(r.Context(), p.ID, t, pr)
		if e == nil {
			w.Header().Set("Content-Disposition", `attachment; filename="euler-project-inventory.json"`)
			writeJSON(w, 200, v)
		}
		return e
	})
	h.route("DELETE /api/v1/tenants/{tenantID}", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			Confirmation string `json:"confirmation"`
		}
		if e := decode(w, r, &b); e != nil {
			return e
		}
		e := h.store.DeleteTenant(r.Context(), p.ID, r.PathValue("tenantID"), b.Confirmation)
		if e == nil {
			w.WriteHeader(204)
		}
		return e
	})
}
