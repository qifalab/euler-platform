package platform

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/automation"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
)

func (s *Store) NewAutomation() *automation.Engine {
	return automation.New(s.ApplicationRuntime("", ""), func(ctx context.Context, tx *sql.Tx, actor, tenant, project, app string) (appkit.Scope, error) {
		v := appkit.Scope{ActorID: actor, TenantID: tenant, ProjectID: project, ApplicationID: app, Permissions: []string{"read"}}
		if err := requireActiveProject(ctx, tx, tenant, project); err != nil {
			return v, appkit.Forbidden("联动项目已归档或不可访问")
		}
		role, err := projectRole(ctx, tx, actor, tenant, project)
		if err != nil {
			return v, appkit.Forbidden("规则执行人或受益人已失去项目访问权限")
		}
		var status string
		if err = tx.QueryRowContext(ctx, "SELECT id,status FROM installations WHERE tenant_id=? AND project_id=? AND application_id=?", tenant, project, app).Scan(&v.InstallationID, &status); err != nil {
			return v, appkit.Forbidden("联动应用尚未安装")
		}
		if status != "enabled" {
			return v, appkit.Forbidden("联动应用已停用")
		}
		if role != "viewer" {
			v.Permissions = append(v.Permissions, "write")
		}
		if admin(role) {
			v.Permissions = append(v.Permissions, "manage", "secrets")
		}
		rows, err := tx.QueryContext(ctx, "SELECT g.permission FROM app_grants g LEFT JOIN app_grant_expiry e ON e.grant_id=g.id WHERE g.tenant_id=? AND g.project_id=? AND g.application_id=? AND g.user_id=? AND (e.expires_at IS NULL OR e.expires_at>?)", tenant, project, app, actor, stamp())
		if err != nil {
			return v, err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err = rows.Scan(&p); err != nil {
				return v, err
			}
			v.Permissions = append(v.Permissions, p)
		}
		return v, rows.Err()
	})
}
func WithAutomation(engine *automation.Engine) HandlerOption {
	return func(h *Handler) { h.automation = engine }
}
func (h *Handler) automationRoutes() {
	if h.automation == nil {
		return
	}
	base := "/api/v1/tenants/{tenantID}/projects/{projectID}/automation"
	wrap := func(manage bool, fn endpoint) endpoint {
		return func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
			role, err := projectRole(r.Context(), h.store.db, p.ID, r.PathValue("tenantID"), r.PathValue("projectID"))
			if err != nil {
				return err
			}
			if manage && !admin(role) {
				return ErrForbidden
			}
			if r.Method != "GET" && r.Method != "HEAD" {
				if err = requireActiveProject(r.Context(), h.store.db, r.PathValue("tenantID"), r.PathValue("projectID")); err != nil {
					return err
				}
			}
			return fn(w, r, p)
		}
	}
	h.route("GET "+base+"/rules", wrap(true, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		out, err := h.automation.Rules(r.Context(), r.PathValue("tenantID"), r.PathValue("projectID"))
		if err == nil {
			items(w, out)
		}
		return err
	}))
	h.route("POST "+base+"/rules", wrap(true, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b automation.Rule
		if err := decode(w, r, &b); err != nil {
			return err
		}
		out, err := h.automation.Create(r.Context(), p.ID, r.PathValue("tenantID"), r.PathValue("projectID"), b)
		if err == nil {
			writeJSON(w, 201, out)
		}
		return err
	}))
	h.route("PATCH "+base+"/rules/{ruleID}", wrap(true, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			Enabled bool `json:"enabled"`
		}
		if err := decode(w, r, &b); err != nil {
			return err
		}
		err := h.automation.SetEnabled(r.Context(), p.ID, r.PathValue("tenantID"), r.PathValue("projectID"), r.PathValue("ruleID"), b.Enabled)
		if err == nil {
			w.WriteHeader(204)
		}
		return err
	}))
	h.route("GET "+base+"/executions", wrap(true, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		out, err := h.automation.Executions(r.Context(), r.PathValue("tenantID"), r.PathValue("projectID"))
		if err == nil {
			items(w, out)
		}
		return err
	}))
	h.route("POST "+base+"/executions/{executionID}/retry", wrap(true, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		err := h.automation.Retry(r.Context(), p.ID, r.PathValue("tenantID"), r.PathValue("projectID"), r.PathValue("executionID"))
		if err == nil {
			w.WriteHeader(204)
		}
		return err
	}))
	h.route("GET "+base+"/notifications", wrap(false, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		out, err := h.automation.Notifications(r.Context(), p.ID, r.PathValue("tenantID"), r.PathValue("projectID"))
		if err == nil {
			items(w, out)
		}
		return err
	}))
	h.route("POST "+base+"/notifications/{notificationID}/read", wrap(false, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		err := h.automation.ReadNotification(r.Context(), p.ID, r.PathValue("tenantID"), r.PathValue("projectID"), r.PathValue("notificationID"))
		if err == nil {
			w.WriteHeader(204)
		}
		return err
	}))
}
