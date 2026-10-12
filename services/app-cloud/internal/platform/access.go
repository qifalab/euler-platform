package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
)

// Separate tables keep the v1 schema and existing inserts compatible. Archived
// projects also disable installations so public routes and workers fail closed.
func migrateAccess(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS deleted_tenants(id TEXT PRIMARY KEY,name TEXT NOT NULL,deleted_by TEXT NOT NULL,deleted_at INTEGER NOT NULL,summary TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS project_lifecycle(project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,status TEXT NOT NULL CHECK(status IN ('active','archived')),updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS project_archived_installations(installation_id TEXT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,previous_status TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS app_grant_expiry(grant_id TEXT PRIMARY KEY REFERENCES app_grants(id) ON DELETE CASCADE,expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS service_accounts(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,name TEXT NOT NULL,token_hash TEXT NOT NULL UNIQUE,scopes TEXT NOT NULL,created_by TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,revoked_at INTEGER,last_used_at INTEGER,FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS service_accounts_project ON service_accounts(tenant_id,project_id);
CREATE TRIGGER IF NOT EXISTS installation_archive_insert BEFORE INSERT ON installations WHEN NEW.status='enabled' AND EXISTS(SELECT 1 FROM project_lifecycle WHERE project_id=NEW.project_id AND status='archived') BEGIN SELECT RAISE(ABORT,'project is archived'); END;
CREATE TRIGGER IF NOT EXISTS installation_archive_update BEFORE UPDATE OF status ON installations WHEN NEW.status='enabled' AND EXISTS(SELECT 1 FROM project_lifecycle WHERE project_id=NEW.project_id AND status='archived') BEGIN SELECT RAISE(ABORT,'project is archived'); END;
`)
	return err
}

func requireActiveProject(ctx context.Context, q queryer, tenant, project string) error {
	var status string
	err := q.QueryRowContext(ctx, `SELECT COALESCE(l.status,'active') FROM projects p LEFT JOIN project_lifecycle l ON l.project_id=p.id WHERE p.id=? AND p.tenant_id=?`, project, tenant).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "active" {
		return fmt.Errorf("%w: project is archived; restore it before using applications", ErrConflict)
	}
	return nil
}

type ServiceScope struct {
	ApplicationID string   `json:"applicationId"`
	Operations    []string `json:"operations"`
}
type ServiceAccount struct {
	ID         string         `json:"id"`
	TenantID   string         `json:"tenantId"`
	ProjectID  string         `json:"projectId"`
	Name       string         `json:"name"`
	Scopes     []ServiceScope `json:"scopes"`
	CreatedAt  time.Time      `json:"createdAt"`
	ExpiresAt  time.Time      `json:"expiresAt"`
	RevokedAt  *time.Time     `json:"revokedAt,omitempty"`
	LastUsedAt *time.Time     `json:"lastUsedAt,omitempty"`
}
type ServiceAccountInput struct {
	Name      string         `json:"name"`
	ExpiresAt time.Time      `json:"expiresAt"`
	Scopes    []ServiceScope `json:"scopes"`
}

func validateServiceInput(b *ServiceAccountInput) error {
	name, err := cleanName(b.Name)
	if err != nil {
		return err
	}
	b.Name = name
	if !b.ExpiresAt.After(time.Now()) || b.ExpiresAt.After(time.Now().Add(366*24*time.Hour)) {
		return fmt.Errorf("%w: expiry must be within one year", ErrInvalid)
	}
	if len(b.Scopes) == 0 || len(b.Scopes) > 5 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, s := range b.Scopes {
		if s.ApplicationID != "database" && s.ApplicationID != "storage" && s.ApplicationID != "statistics" && s.ApplicationID != "weauth" && s.ApplicationID != "lottery" {
			return ErrInvalid
		}
		if seen[s.ApplicationID] || len(s.Operations) == 0 || len(s.Operations) > 4 {
			return ErrInvalid
		}
		seen[s.ApplicationID] = true
		ops := map[string]bool{}
		for _, op := range s.Operations {
			if !serviceOperationSupported(s.ApplicationID, op) || ops[op] {
				return ErrInvalid
			}
			ops[op] = true
		}
	}
	return nil
}
func (s *Store) CreateServiceAccount(ctx context.Context, actor, tenant, project string, b ServiceAccountInput) (ServiceAccount, string, error) {
	if err := validateServiceInput(&b); err != nil {
		return ServiceAccount{}, "", err
	}
	v := ServiceAccount{ID: newID("sa_"), TenantID: tenant, ProjectID: project, Name: b.Name, Scopes: b.Scopes, CreatedAt: time.Now().UTC(), ExpiresAt: b.ExpiresAt.UTC()}
	secret := "euler_sa_" + newID("") + newID("")
	scopes, _ := json.Marshal(b.Scopes)
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := requireProject(ctx, tx, actor, tenant, project, "admin"); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM service_accounts WHERE tenant_id=? AND project_id=? AND revoked_at IS NULL AND expires_at>?`, tenant, project, stamp()).Scan(&count); err != nil {
			return err
		}
		if count >= 50 {
			return fmt.Errorf("%w: project has 50 active service accounts", ErrConflict)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO service_accounts(id,tenant_id,project_id,name,token_hash,scopes,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?)`, v.ID, tenant, project, v.Name, tokenHash(secret), string(scopes), actor, v.CreatedAt.UnixMilli(), v.ExpiresAt.UnixMilli())
		if err != nil {
			return err
		}
		return audit(ctx, tx, actor, tenant, project, "service_account.created", v.ID, "Created scoped service account; secret shown once")
	})
	if err != nil {
		return ServiceAccount{}, "", err
	}
	return v, secret, nil
}
func scanService(row scanner) (ServiceAccount, error) {
	var v ServiceAccount
	var scopes string
	var created, expires int64
	var revoked, used sql.NullInt64
	err := row.Scan(&v.ID, &v.TenantID, &v.ProjectID, &v.Name, &scopes, &created, &expires, &revoked, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal([]byte(scopes), &v.Scopes); err != nil {
		return v, err
	}
	v.CreatedAt = fromStamp(created)
	v.ExpiresAt = fromStamp(expires)
	if revoked.Valid {
		t := fromStamp(revoked.Int64)
		v.RevokedAt = &t
	}
	if used.Valid {
		t := fromStamp(used.Int64)
		v.LastUsedAt = &t
	}
	return v, nil
}

const serviceColumns = `id,tenant_id,project_id,name,scopes,created_at,expires_at,revoked_at,last_used_at`

func (s *Store) ServiceAccounts(ctx context.Context, actor, tenant, project string) ([]ServiceAccount, error) {
	// Archived projects retain an inspectable credential inventory.
	role, err := projectRole(ctx, s.db, actor, tenant, project)
	if err != nil {
		return nil, err
	}
	if !admin(role) {
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+serviceColumns+` FROM service_accounts WHERE tenant_id=? AND project_id=? ORDER BY created_at DESC,id`, tenant, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceAccount{}
	for rows.Next() {
		v, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) RotateServiceAccount(ctx context.Context, actor, tenant, project, id string) (string, error) {
	secret := "euler_sa_" + newID("") + newID("")
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := requireProject(ctx, tx, actor, tenant, project, "admin"); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE service_accounts SET token_hash=? WHERE id=? AND tenant_id=? AND project_id=? AND revoked_at IS NULL AND expires_at>?`, tokenHash(secret), id, tenant, project, stamp())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, actor, tenant, project, "service_account.rotated", id, "Rotated secret; previous secret revoked immediately")
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}
func (s *Store) RevokeServiceAccount(ctx context.Context, actor, tenant, project, id string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		role, err := projectRole(ctx, tx, actor, tenant, project)
		if err != nil {
			return err
		}
		if !admin(role) {
			return ErrForbidden
		}
		res, err := tx.ExecContext(ctx, `UPDATE service_accounts SET revoked_at=COALESCE(revoked_at,?) WHERE id=? AND tenant_id=? AND project_id=?`, stamp(), id, tenant, project)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, actor, tenant, project, "service_account.revoked", id, "Revoked service account")
	})
}
func (h *Handler) accessRoutes() {
	base := "/api/v1/tenants/{tenantID}/projects/{projectID}/service-accounts"
	h.route("GET "+base, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, pr, _ := scope(r)
		v, e := h.store.ServiceAccounts(r.Context(), p.ID, t, pr)
		if e == nil {
			items(w, v)
		}
		return e
	})
	h.route("POST "+base, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b ServiceAccountInput
		if e := decode(w, r, &b); e != nil {
			return e
		}
		t, pr, _ := scope(r)
		v, secret, e := h.store.CreateServiceAccount(r.Context(), p.ID, t, pr, b)
		if e == nil {
			writeJSON(w, 201, map[string]any{"account": v, "secret": secret})
		}
		return e
	})
	h.route("POST "+base+"/{id}/rotate", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, pr, id := scope(r)
		secret, e := h.store.RotateServiceAccount(r.Context(), p.ID, t, pr, id)
		if e == nil {
			writeJSON(w, 200, map[string]string{"secret": secret})
		}
		return e
	})
	h.route("DELETE "+base+"/{id}", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, pr, id := scope(r)
		e := h.store.RevokeServiceAccount(r.Context(), p.ID, t, pr, id)
		if e == nil {
			w.WriteHeader(204)
		}
		return e
	})
	machine := "/api/v1/machine/tenants/{tenantID}/projects/{projectID}/apps/{applicationID}"
	h.mux.HandleFunc(machine, h.machineRequest)
	h.mux.HandleFunc(machine+"/{rest...}", h.machineRequest)
}

// Machine access never authenticates a person or uses a browser session. Both
// application and operation must match a fixed server-side endpoint allowlist.
func (h *Handler) machineRequest(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" || r.Header.Get("Origin") != "" {
		writeError(w, "mixed_credentials", "Machine API does not accept browser credentials", 403)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(token, "euler_sa_") || len(token) > 200 {
		writeError(w, "unauthenticated", "A service account token is required", 401)
		return
	}
	v, err := scanService(h.store.db.QueryRowContext(r.Context(), `SELECT `+serviceColumns+` FROM service_accounts WHERE token_hash=? AND revoked_at IS NULL AND expires_at>?`, tokenHash(token), stamp()))
	if err != nil {
		writeError(w, "unauthenticated", "The service account is invalid or expired", 401)
		return
	}
	tenant, project, app := r.PathValue("tenantID"), r.PathValue("projectID"), r.PathValue("applicationID")
	if v.TenantID != tenant || v.ProjectID != project {
		respondError(w, ErrNotFound)
		return
	}
	if err = requireActiveProject(r.Context(), h.store.db, tenant, project); err != nil {
		respondError(w, err)
		return
	}
	module, ok := h.modules[app]
	if !ok {
		respondError(w, ErrNotFound)
		return
	}
	rest := "/" + r.PathValue("rest")
	permission := machinePermission(app, r.Method, rest)
	if permission == "" {
		respondError(w, ErrForbidden)
		return
	}
	allowed := false
	for _, sc := range v.Scopes {
		if sc.ApplicationID == app {
			for _, op := range sc.Operations {
				if op == permission {
					allowed = true
				}
			}
		}
	}
	if !allowed {
		respondError(w, ErrForbidden)
		return
	}
	var installation string
	err = h.store.db.QueryRowContext(r.Context(), `SELECT id FROM installations WHERE tenant_id=? AND project_id=? AND application_id=? AND status='enabled'`, tenant, project, app).Scan(&installation)
	if errors.Is(err, sql.ErrNoRows) {
		respondError(w, ErrNotFound)
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	s := appkit.Scope{ActorID: v.ID, ActorName: v.Name, Provider: "euler-service-account", Subject: v.ID, TenantID: tenant, ProjectID: project, InstallationID: installation, ApplicationID: app, Permissions: []string{permission}}
	// Fixed audit text excludes request data, access tokens and object keys.
	err = h.store.write(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `UPDATE service_accounts SET last_used_at=? WHERE id=?`, stamp(), v.ID); err != nil {
			return err
		}
		return audit(r.Context(), tx, v.ID, tenant, project, "service_account.request", v.ID, app+" "+permission+" "+r.Method)
	})
	if err != nil {
		respondError(w, err)
		return
	}
	inner := r.Clone(appkit.WithScope(r.Context(), s))
	u := *r.URL
	inner.URL = &u
	inner.URL.Path = rest
	inner.URL.RawPath = ""
	module.Handler().ServeHTTP(w, inner)
}

type machineRoute struct{ app, method, path, permission string }

var machineRoutes = []machineRoute{
	{"statistics", "GET", "/sites/*/analytics", "read"}, {"statistics", "GET", "/sites/*/export.csv", "read"}, {"statistics", "GET", "/sites/*/settings", "read"}, {"statistics", "PUT", "/sites/*/settings", "manage"}, {"statistics", "POST", "/sites/*/cleanup", "manage"}, {"statistics", "POST", "/sites/*/funnel", "read"},
	{"lottery", "GET", "/rooms/*/invitation", "write"}, {"lottery", "POST", "/rooms/*/invitation/rotate", "manage"}, {"lottery", "POST", "/rooms/*/reset", "manage"},
	{"storage", "GET", "/buckets/*/multipart", "write"}, {"storage", "POST", "/buckets/*/multipart", "write"}, {"storage", "GET", "/buckets/*/multipart/*", "write"}, {"storage", "PUT", "/buckets/*/multipart/*/parts/*", "write"}, {"storage", "POST", "/buckets/*/multipart/*/complete", "write"}, {"storage", "DELETE", "/buckets/*/multipart/*", "write"},
	{"storage", "GET", "/buckets/*/versioning", "read"}, {"storage", "PUT", "/buckets/*/versioning", "manage"}, {"storage", "GET", "/buckets/*/versions", "read"}, {"storage", "POST", "/buckets/*/versions/restore", "write"}, {"storage", "DELETE", "/buckets/*/versions", "manage"}, {"storage", "GET", "/buckets/*/lifecycle", "manage"}, {"storage", "PUT", "/buckets/*/lifecycle", "manage"}, {"storage", "POST", "/buckets/*/objects/transfer", "write"},

	{"database", "GET", "/", "read"}, {"database", "GET", "/quota", "read"}, {"database", "GET", "/databases", "read"}, {"database", "POST", "/databases", "write"}, {"database", "GET", "/databases/*", "read"}, {"database", "DELETE", "/databases/*", "manage"}, {"database", "GET", "/databases/*/credentials", "secrets"}, {"database", "POST", "/databases/*/password", "secrets"}, {"database", "GET", "/packages", "read"},
	{"storage", "GET", "/", "read"}, {"storage", "GET", "/quota", "read"}, {"storage", "GET", "/buckets", "read"}, {"storage", "POST", "/buckets", "write"}, {"storage", "GET", "/buckets/*", "read"}, {"storage", "PUT", "/buckets/*", "manage"}, {"storage", "DELETE", "/buckets/*", "manage"}, {"storage", "POST", "/buckets/*/sync", "write"}, {"storage", "POST", "/buckets/*/cors", "manage"}, {"storage", "GET", "/buckets/*/objects", "read"}, {"storage", "GET", "/buckets/*/objects/info", "read"}, {"storage", "POST", "/buckets/*/objects/folder", "write"}, {"storage", "POST", "/buckets/*/objects/presigned-upload", "write"}, {"storage", "POST", "/buckets/*/objects/confirm-upload", "write"}, {"storage", "POST", "/buckets/*/objects/presigned-download", "read"}, {"storage", "GET", "/buckets/*/objects/public-url", "read"}, {"storage", "DELETE", "/buckets/*/objects", "write"}, {"storage", "POST", "/buckets/*/objects/delete-batch", "write"}, {"storage", "GET", "/stats/logs", "read"}, {"storage", "GET", "/packages", "read"},
	{"statistics", "GET", "/sites", "read"}, {"statistics", "POST", "/sites", "manage"}, {"statistics", "PATCH", "/sites/*", "manage"}, {"statistics", "GET", "/sites/*/report", "read"}, {"statistics", "GET", "/sites/*/integration", "read"},
	{"weauth", "GET", "/sites", "read"}, {"weauth", "POST", "/sites", "write"}, {"weauth", "GET", "/sites/*", "read"}, {"weauth", "PUT", "/sites/*", "write"}, {"weauth", "DELETE", "/sites/*", "manage"}, {"weauth", "GET", "/sites/*/domains", "read"}, {"weauth", "POST", "/sites/*/domains", "write"}, {"weauth", "DELETE", "/sites/*/domains/*", "write"}, {"weauth", "GET", "/sites/*/stats", "read"}, {"weauth", "GET", "/sites/*/secret", "secrets"}, {"weauth", "POST", "/sites/*/regenerate-secret", "secrets"}, {"weauth", "GET", "/sites/*/ip-policy", "read"}, {"weauth", "PUT", "/sites/*/ip-policy", "manage"}, {"weauth", "GET", "/sites/*/ip-records", "manage"}, {"weauth", "DELETE", "/sites/*/ip-records/*", "manage"},
	{"lottery", "GET", "/rooms", "read"}, {"lottery", "POST", "/rooms", "write"}, {"lottery", "GET", "/rooms/*", "read"}, {"lottery", "PATCH", "/rooms/*", "write"}, {"lottery", "GET", "/rooms/*/participants", "read"}, {"lottery", "POST", "/rooms/*/participants", "write"}, {"lottery", "POST", "/rooms/*/participants/batch", "write"}, {"lottery", "DELETE", "/rooms/*/participants/*", "write"}, {"lottery", "GET", "/rooms/*/draws", "read"}, {"lottery", "POST", "/rooms/*/draws", "write"}, {"lottery", "GET", "/history", "read"},
}

func machinePermission(app, method, path string) string {
	for _, route := range machineRoutes {
		if route.app != app || route.method != method {
			continue
		}
		a, b := strings.Split(path, "/"), strings.Split(route.path, "/")
		if len(a) != len(b) {
			continue
		}
		match := true
		for i := range a {
			if (b[i] != "*" && a[i] != b[i]) || (b[i] == "*" && (a[i] == "" || a[i] == "." || a[i] == "..")) {
				match = false
				break
			}
		}
		if match {
			return route.permission
		}
	}
	return ""
}

func serviceOperationSupported(app, operation string) bool {
	for _, route := range machineRoutes {
		if route.app == app && route.permission == operation {
			return true
		}
	}
	return false
}
