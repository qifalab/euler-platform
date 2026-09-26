package platform

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/connectors"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
)

// Administrator is a deployment-level bootstrap grant, bound to a verified
// provider and subject. Email, team ownership and request headers never qualify.
type Administrator struct {
	Provider string `json:"provider"`
	Subject  string `json:"subject"`
}

type HandlerOption func(*Handler)

func WithApplications(modules []appkit.Module, administrators []Administrator) HandlerOption {
	return func(h *Handler) {
		h.modules = make(map[string]appkit.Module, len(modules))
		for _, m := range modules {
			h.modules[m.ID()] = m
		}
		h.administrators = append([]Administrator(nil), administrators...)
	}
}

func (s *Store) ApplicationRuntime(dataDir, publicURL string) *appkit.Runtime {
	return &appkit.Runtime{
		DB: s.db, DataDir: dataDir, PublicURL: strings.TrimRight(publicURL, "/"),
		Encrypt: s.encrypt, Decrypt: s.decrypt,
		DeriveKey: func(purpose string) []byte {
			h := hmac.New(sha256.New, s.key)
			_, _ = h.Write([]byte("euler-native-app-v1\x00" + purpose))
			return h.Sum(nil)
		},
		ApplicationEnabled: s.ApplicationEnabled,
	}
}

func (s *Store) ApplicationEnabled(ctx context.Context, tenant, project, app string) (bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx, `SELECT status='enabled' FROM installations WHERE tenant_id=? AND project_id=? AND application_id=?`, tenant, project, app).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return enabled, err
}

func (s *Store) nativeScope(ctx context.Context, p identity.Principal, tenant, project, app string) (appkit.Scope, error) {
	v := appkit.Scope{ActorID: p.ID, ActorName: p.Name, Email: p.Email, EmailVerified: p.EmailVerified, Provider: p.Provider, Subject: p.Subject, TenantID: tenant, ProjectID: project, ApplicationID: app, Permissions: []string{"read"}}
	role, err := projectRole(ctx, s.db, p.ID, tenant, project)
	if err != nil {
		return v, err
	}
	var status string
	err = s.db.QueryRowContext(ctx, `SELECT id,status FROM installations WHERE tenant_id=? AND project_id=? AND application_id=?`, tenant, project, app).Scan(&v.InstallationID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if status != "enabled" {
		return v, appkit.Conflict("当前项目已停用此应用")
	}
	if role != "viewer" {
		v.Permissions = append(v.Permissions, "write")
	}
	if admin(role) {
		v.Permissions = append(v.Permissions, "manage", "secrets")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT permission FROM app_grants WHERE tenant_id=? AND project_id=? AND application_id=? AND user_id=? ORDER BY permission`, tenant, project, app, p.ID)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var permission string
		if err = rows.Scan(&permission); err != nil {
			return v, err
		}
		v.Permissions = append(v.Permissions, permission)
	}
	return v, rows.Err()
}

func (h *Handler) isPlatformAdmin(p identity.Principal) bool {
	for _, a := range h.administrators {
		if a.Provider != "" && a.Subject != "" && p.Provider == a.Provider && p.Subject == a.Subject {
			return true
		}
	}
	return false
}

func (h *Handler) catalog() []connectors.Application {
	apps := h.connectors.Catalog()
	descriptions := map[string]string{
		"eid":        "身份申请、成员资格、社团报名与录取，在欧拉内完成。",
		"trust":      "管理认证方案、提交材料和审核申请，联动成员资格。",
		"weauth":     "为网站配置人机验证、域名策略与访问风控。",
		"database":   "创建和管理 MySQL、PostgreSQL 数据库及项目配额。",
		"storage":    "管理存储桶、文件、访问密钥和资源包。",
		"statistics": "采集网站访问，查看 PV、UV 与页面排行。",
		"lottery":    "创建活动、收集报名、现场抽奖并保留完整记录。",
		"witshield":  "接入设备，扫描风险、调查事件并审批修复。",
	}
	for i := range apps {
		if _, ok := h.modules[apps[i].ID]; ok {
			apps[i].ConnectionMode = "native"
			apps[i].Description = descriptions[apps[i].ID]
			apps[i].Capabilities = []string{"native:workspace"}
			apps[i].Limitations = []string{}
		}
	}
	return apps
}

func (h *Handler) nativeRoutes() {
	if len(h.modules) == 0 {
		return
	}
	base := "/api/v1/tenants/{tenantID}/projects/{projectID}/apps/{applicationID}"
	serve := func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		app := r.PathValue("applicationID")
		m, ok := h.modules[app]
		if !ok {
			return ErrNotFound
		}
		s, err := h.store.nativeScope(r.Context(), p, r.PathValue("tenantID"), r.PathValue("projectID"), app)
		if err != nil {
			return err
		}
		rest := r.PathValue("rest")
		if rest == "_context" {
			if r.Method != "GET" {
				return ErrNotFound
			}
			writeJSON(w, 200, s)
			return nil
		}
		inner := r.Clone(appkit.WithScope(r.Context(), s))
		u := *r.URL
		inner.URL = &u
		inner.URL.Path = "/" + rest
		inner.URL.RawPath = ""
		m.Handler().ServeHTTP(w, inner)
		return nil
	}
	h.route(base, serve)
	h.route(base+"/{rest...}", serve)
	for id, m := range h.modules {
		if public := m.PublicHandler(); public != nil {
			h.mux.Handle("/public/"+id+"/", http.StripPrefix("/public/"+id, public))
		}
	}
	h.grantRoutes()
}

type AppGrant struct {
	ID            string `json:"id"`
	TenantID      string `json:"tenantId"`
	ProjectID     string `json:"projectId"`
	ApplicationID string `json:"applicationId"`
	UserID        string `json:"userId"`
	DisplayName   string `json:"displayName"`
	Permission    string `json:"permission"`
	GrantedBy     string `json:"grantedBy"`
	CreatedAt     int64  `json:"createdAt"`
}

func (h *Handler) grantRoutes() {
	h.route("GET /api/v1/platform/grants", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		if !h.isPlatformAdmin(p) {
			return ErrForbidden
		}
		tenant, project := r.URL.Query().Get("tenantId"), r.URL.Query().Get("projectId")
		if tenant == "" || project == "" {
			return ErrInvalid
		}
		rows, err := h.store.db.QueryContext(r.Context(), `SELECT g.id,g.tenant_id,g.project_id,g.application_id,g.user_id,u.display_name,g.permission,g.granted_by,g.created_at FROM app_grants g JOIN users u ON u.id=g.user_id WHERE g.tenant_id=? AND g.project_id=? ORDER BY g.created_at DESC,g.id`, tenant, project)
		if err != nil {
			return err
		}
		defer rows.Close()
		out := []AppGrant{}
		for rows.Next() {
			var g AppGrant
			if err = rows.Scan(&g.ID, &g.TenantID, &g.ProjectID, &g.ApplicationID, &g.UserID, &g.DisplayName, &g.Permission, &g.GrantedBy, &g.CreatedAt); err != nil {
				return err
			}
			out = append(out, g)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		items(w, out)
		return nil
	})
	h.route("POST /api/v1/platform/grants", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		if !h.isPlatformAdmin(p) {
			return ErrForbidden
		}
		var b struct {
			TenantID      string `json:"tenantId"`
			ProjectID     string `json:"projectId"`
			ApplicationID string `json:"applicationId"`
			UserID        string `json:"userId"`
			Permission    string `json:"permission"`
		}
		if err := decode(w, r, &b); err != nil {
			return err
		}
		if _, ok := h.modules[b.ApplicationID]; !ok {
			return ErrInvalid
		}
		if b.Permission != "review" && b.Permission != "admin" {
			return ErrInvalid
		}
		id := newID("grant_")
		err := h.store.write(r.Context(), func(tx *sql.Tx) error {
			if _, err := projectRole(r.Context(), tx, b.UserID, b.TenantID, b.ProjectID); err != nil {
				return err
			}
			var existing string
			err := tx.QueryRowContext(r.Context(), `SELECT id FROM app_grants WHERE tenant_id=? AND project_id=? AND application_id=? AND user_id=? AND permission=?`, b.TenantID, b.ProjectID, b.ApplicationID, b.UserID, b.Permission).Scan(&existing)
			if err == nil {
				return fmt.Errorf("%w: permission already granted", ErrConflict)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			_, err = tx.ExecContext(r.Context(), `INSERT INTO app_grants(id,tenant_id,project_id,application_id,user_id,permission,granted_by,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, b.TenantID, b.ProjectID, b.ApplicationID, b.UserID, b.Permission, p.ID, stamp())
			if err != nil {
				return err
			}
			return audit(r.Context(), tx, p.ID, b.TenantID, b.ProjectID, "application.permission_granted", id, b.ApplicationID+" "+b.Permission+" granted to "+b.UserID)
		})
		if err != nil {
			return err
		}
		writeJSON(w, 201, map[string]string{"id": id})
		return nil
	})
	h.route("DELETE /api/v1/platform/grants/{grantID}", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		if !h.isPlatformAdmin(p) {
			return ErrForbidden
		}
		err := h.store.write(r.Context(), func(tx *sql.Tx) error {
			var tenant, project, app, user, permission string
			err := tx.QueryRowContext(r.Context(), `SELECT tenant_id,project_id,application_id,user_id,permission FROM app_grants WHERE id=?`, r.PathValue("grantID")).Scan(&tenant, &project, &app, &user, &permission)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(r.Context(), `DELETE FROM app_grants WHERE id=?`, r.PathValue("grantID")); err != nil {
				return err
			}
			return audit(r.Context(), tx, p.ID, tenant, project, "application.permission_revoked", r.PathValue("grantID"), app+" "+permission+" revoked from "+user)
		})
		if err == nil {
			w.WriteHeader(204)
		}
		return err
	})
}
