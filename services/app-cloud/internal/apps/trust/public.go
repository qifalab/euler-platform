package trust

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"net/http"
	"strings"
	"time"
)

type APIKey struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	SchemeIDs []string `json:"schemeIds"`
	ActorIDs  []string `json:"actorIds"`
	Details   bool     `json:"details"`
	ExpiresAt string   `json:"expiresAt"`
	RevokedAt string   `json:"revokedAt"`
	CreatedAt string   `json:"createdAt"`
}

func tokenHash(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func contains(v []string, item string) bool {
	for _, x := range v {
		if x == item {
			return true
		}
	}
	return false
}
func scanKey(row interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	var schemes, actors string
	e := row.Scan(&k.ID, &k.Name, &schemes, &actors, &k.Details, &k.ExpiresAt, &k.RevokedAt, &k.CreatedAt)
	if e == nil {
		e = json.Unmarshal([]byte(schemes), &k.SchemeIDs)
	}
	if e == nil {
		e = json.Unmarshal([]byte(actors), &k.ActorIDs)
	}
	return k, e
}
func (m *Module) keys(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT id,name,schemes,actors,details,expires_at,revoked_at,created_at FROM trust_keys WHERE tenant_id=? AND project_id=? ORDER BY created_at DESC", s.TenantID, s.ProjectID)
	if e != nil {
		return e
	}
	defer rows.Close()
	items := []APIKey{}
	for rows.Next() {
		k, e := scanKey(rows)
		if e != nil {
			return e
		}
		items = append(items, k)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": items})
	return nil
}
func (m *Module) createKey(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	if e := s.Require("secrets"); e != nil {
		return e
	}
	var k APIKey
	if e := appkit.Decode(w, r, &k); e != nil {
		return e
	}
	var e error
	k.Name, e = appkit.Name(k.Name, 100)
	if e != nil {
		return e
	}
	exp, e := time.Parse(time.RFC3339, k.ExpiresAt)
	if e != nil || !exp.After(time.Now()) || exp.After(time.Now().Add(90*24*time.Hour)) {
		return appkit.Invalid("密钥有效期必须在未来 90 天内")
	}
	if len(k.SchemeIDs) < 1 || len(k.SchemeIDs) > 100 || len(k.ActorIDs) < 1 || len(k.ActorIDs) > 100 {
		return appkit.Invalid("必须指定 1–100 个方案和申请人，不能使用全局通配")
	}
	if k.Details && !s.Can("review") {
		return appkit.Forbidden("签发材料读取密钥还需要独立审核权限")
	}
	token := appkit.NewID("trust_") + "." + appkit.NewID("")
	k.ID = appkit.NewID("tkey_")
	k.CreatedAt = appkit.Now()
	k.RevokedAt = ""
	k.ExpiresAt = exp.UTC().Format(time.RFC3339Nano)
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		for _, id := range k.SchemeIDs {
			v, e := getScheme(r.Context(), tx, s, id)
			if e != nil {
				return e
			}
			if v.Status != "active" {
				return appkit.Invalid("只能为可用方案签发密钥")
			}
		}
		for _, id := range k.ActorIDs {
			if _, e := m.actor(r.Context(), tx, s, id); e != nil {
				return appkit.Invalid("申请人不在当前项目 Trust 用户中")
			}
		}
		a, _ := json.Marshal(k.ActorIDs)
		c, _ := json.Marshal(k.SchemeIDs)
		_, e := tx.ExecContext(r.Context(), "INSERT INTO trust_keys VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", k.ID, s.TenantID, s.ProjectID, k.Name, tokenHash(token), string(c), string(a), k.Details, k.ExpiresAt, "", k.CreatedAt, s.ActorID)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "key.create", k.ID, "签发限定方案和申请人的查询密钥")
	})
	if e == nil {
		appkit.JSON(w, 201, map[string]any{"key": k, "token": token})
	}
	return e
}
func (m *Module) revokeKey(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	if e := s.Require("secrets"); e != nil {
		return e
	}
	id := r.PathValue("id")
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		result, e := tx.ExecContext(r.Context(), "UPDATE trust_keys SET revoked_at=? WHERE id=? AND tenant_id=? AND project_id=? AND revoked_at=''", appkit.Now(), id, s.TenantID, s.ProjectID)
		if e != nil {
			return e
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return appkit.NotFound()
		}
		return m.rt.Audit(r.Context(), tx, s, "key.revoke", id, "撤销认证查询密钥")
	})
	if e == nil {
		appkit.JSON(w, 204, nil)
	}
	return e
}
func (m *Module) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	for _, path := range []string{"GET /verification/status", "GET /verification/details", "GET /materials/{id}"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if e := m.public(w, r); e != nil {
				appkit.RespondError(w, e)
			}
		})
	}
	return mux
}
func (m *Module) public(w http.ResponseWriter, r *http.Request) error {
	// Only a dedicated, scoped machine credential is accepted. Query API keys,
	// OAuth IDs, old app JWTs, and browser-provided identity headers are ignored.
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer trust_") || len(auth) > 200 {
		return &appkit.Error{Status: 401, Code: "invalid_key", Message: "需要有效的应用查询密钥"}
	}
	raw := strings.TrimPrefix(auth, "Bearer ")
	var s appkit.Scope
	var kid, schemes, actors, expires, revoked, creator string
	var details bool
	e := m.rt.DB.QueryRowContext(r.Context(), "SELECT id,tenant_id,project_id,schemes,actors,details,expires_at,revoked_at,created_by FROM trust_keys WHERE token_hash=?", tokenHash(raw)).Scan(&kid, &s.TenantID, &s.ProjectID, &schemes, &actors, &details, &expires, &revoked, &creator)
	if e != nil {
		return &appkit.Error{Status: 401, Code: "invalid_key", Message: "查询密钥无效"}
	}
	expiry, e := time.Parse(time.RFC3339Nano, expires)
	if e != nil || !expiry.After(time.Now()) || revoked != "" {
		return &appkit.Error{Status: 401, Code: "invalid_key", Message: "查询密钥已过期或撤销"}
	}
	if m.rt.ApplicationEnabled == nil {
		return appkit.Unavailable("应用状态检查未配置")
	}
	enabled, e := m.rt.ApplicationEnabled(r.Context(), s.TenantID, s.ProjectID, "trust")
	if e != nil {
		return e
	}
	if !enabled {
		return appkit.Forbidden("项目未启用 Trust")
	}
	var allowedSchemes, allowedActors []string
	if json.Unmarshal([]byte(schemes), &allowedSchemes) != nil || json.Unmarshal([]byte(actors), &allowedActors) != nil {
		return appkit.Forbidden("密钥范围无效")
	}
	s.ActorID = creator
	s.ApplicationID = "trust"
	s.InstallationID = "machine:" + kid
	if r.PathValue("id") != "" {
		if !details {
			return appkit.Forbidden("密钥不允许读取材料")
		}
		v, p, e := m.getMaterial(r.Context(), m.rt.DB, s, r.PathValue("id"), true)
		if e != nil {
			return e
		}
		if p.SubmissionID == "" || !contains(allowedActors, p.ActorID) || !contains(allowedSchemes, p.SchemeID) {
			return appkit.NotFound()
		}
		scheme, e := getScheme(r.Context(), m.rt.DB, s, p.SchemeID)
		if e != nil || scheme.Status != "active" {
			return appkit.NotFound()
		}
		sub, e := m.loadSubmission(r.Context(), m.rt.DB, s, p.SubmissionID, true)
		if e != nil {
			return e
		}
		if sub.Status != "approved" {
			return appkit.Forbidden("申请尚未通过")
		}
		current := false
		for _, mat := range sub.Materials {
			if mat.ID == v.ID {
				current = true
			}
		}
		if !current {
			return appkit.NotFound()
		}
		return m.writeMaterial(w, r, s, v, p)
	}
	actorID, schemeID := r.URL.Query().Get("actorId"), r.URL.Query().Get("schemeId")
	if !contains(allowedActors, actorID) || !contains(allowedSchemes, schemeID) {
		return appkit.Forbidden("查询超出密钥范围")
	}
	scheme, e := getScheme(r.Context(), m.rt.DB, s, schemeID)
	if e != nil || scheme.Status != "active" {
		return appkit.NotFound()
	}
	var id, status string
	e = m.rt.DB.QueryRowContext(r.Context(), "SELECT id,status FROM trust_submissions WHERE tenant_id=? AND project_id=? AND actor_id=? AND scheme_id=?", s.TenantID, s.ProjectID, actorID, schemeID).Scan(&id, &status)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if status == "" {
		status = "not_submitted"
	}
	if strings.HasSuffix(r.URL.Path, "/details") {
		if !details {
			return appkit.Forbidden("密钥不允许读取认证详情")
		}
		if status != "approved" {
			return appkit.Forbidden("申请尚未通过")
		}
		v, e := m.loadSubmission(r.Context(), m.rt.DB, s, id, true)
		if e != nil {
			return e
		}
		if e = m.rt.Audit(r.Context(), nil, s, "key.details", kid, "使用限定密钥查询认证详情"); e != nil {
			return e
		}
		v.History = nil
		appkit.JSON(w, 200, map[string]any{"success": true, "data": v})
		return nil
	}
	appkit.JSON(w, 200, map[string]any{"success": true, "data": map[string]any{"verified": status == "approved", "status": status}})
	return nil
}
