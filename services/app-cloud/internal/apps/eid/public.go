package eid

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type QueryKey struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	ActorIDs  []string `json:"actorIds"`
	ExpiresAt string   `json:"expiresAt"`
	CreatedAt string   `json:"createdAt"`
	RevokedAt string   `json:"revokedAt"`
}

func (m *Module) keys(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT id,name,actors,expires_at,created_at,revoked_at FROM eid_query_keys WHERE "+scoped+" ORDER BY created_at DESC", scopeArgs(s)...)
	if e != nil {
		return e
	}
	defer rows.Close()
	items := []QueryKey{}
	for rows.Next() {
		var k QueryKey
		var actors string
		var expiry int64
		if e = rows.Scan(&k.ID, &k.Name, &actors, &expiry, &k.CreatedAt, &k.RevokedAt); e != nil {
			return e
		}
		if e = json.Unmarshal([]byte(actors), &k.ActorIDs); e != nil {
			return e
		}
		k.ExpiresAt = time.Unix(expiry, 0).UTC().Format(time.RFC3339)
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
	var in struct {
		Name      string   `json:"name"`
		ActorIDs  []string `json:"actorIds"`
		ExpiresAt string   `json:"expiresAt"`
	}
	if e := appkit.Decode(w, r, &in); e != nil {
		return e
	}
	name, e := appkit.Name(in.Name, 120)
	if e != nil {
		return e
	}
	expiry, e := time.Parse(time.RFC3339, in.ExpiresAt)
	if e != nil || !expiry.After(time.Now()) || expiry.After(time.Now().Add(366*24*time.Hour)) {
		return appkit.Invalid("查询密钥有效期须在未来一年内")
	}
	if len(in.ActorIDs) == 0 || len(in.ActorIDs) > 100 {
		return appkit.Invalid("请明确选择 1–100 位允许查询的本项目会员")
	}
	seen := map[string]bool{}
	for _, id := range in.ActorIDs {
		if seen[id] {
			return appkit.Invalid("允许查询的会员不能重复")
		}
		seen[id] = true
		if _, e = m.member(r.Context(), s, id); e != nil {
			return appkit.Invalid("只能为本项目已有会员授权查询")
		}
	}
	token := appkit.NewID("eqk_") + appkit.NewID("")
	hash := sha256.Sum256([]byte(token))
	id := appkit.NewID("eqi_")
	actors, _ := json.Marshal(in.ActorIDs)
	now := appkit.Now()
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), "INSERT INTO eid_query_keys(id,installation_id,tenant_id,project_id,name,token_hash,actors,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)", id, s.InstallationID, s.TenantID, s.ProjectID, name, hex.EncodeToString(hash[:]), string(actors), expiry.Unix(), now)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "query_key.create", id, "创建限制会员范围和有效期的资格查询密钥")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, map[string]any{"key": QueryKey{ID: id, Name: name, ActorIDs: in.ActorIDs, ExpiresAt: expiry.UTC().Format(time.RFC3339), CreatedAt: now}, "token": token})
	return nil
}
func (m *Module) revokeKey(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	if e := s.Require("secrets"); e != nil {
		return e
	}
	id := r.PathValue("id")
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), "UPDATE eid_query_keys SET revoked_at=? WHERE "+scoped+" AND id=?", append([]any{appkit.Now()}, append(scopeArgs(s), id)...)...)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return appkit.NotFound()
		}
		return m.rt.Audit(r.Context(), tx, s, "query_key.revoke", id, "撤销资格查询密钥")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 204, nil)
	return nil
}
func (m *Module) qualificationValue(ctx context.Context, s appkit.Scope, actorID string) (map[string]any, error) {
	var id, updated string
	var b []byte
	e := m.rt.DB.QueryRowContext(ctx, "SELECT id,data,updated_at FROM eid_verifications WHERE "+scoped+" AND actor_id=? AND status='approved' ORDER BY updated_at DESC,id DESC LIMIT 1", append(scopeArgs(s), actorID)...).Scan(&id, &b, &updated)
	if e == sql.ErrNoRows {
		return map[string]any{"actorId": actorID, "status": "not_verified"}, nil
	}
	if e != nil {
		return nil, e
	}
	var data VerificationData
	if e = m.open(b, s.InstallationID+":verification:"+id, &data); e != nil {
		return nil, e
	}
	return map[string]any{"actorId": actorID, "status": "approved", "identityType": data.IdentityType, "identityTitle": data.IdentityTitle, "verifiedAt": updated}, nil
}
func (m *Module) qualification(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	out, e := m.qualificationValue(r.Context(), s, s.ActorID)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, out)
	return nil
}
func (m *Module) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /verification/status", func(w http.ResponseWriter, r *http.Request) {
		if e := m.publicQualification(w, r); e != nil {
			appkit.RespondError(w, e)
		}
	})
	return mux
}
func (m *Module) publicQualification(w http.ResponseWriter, r *http.Request) error {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return &appkit.Error{Status: 401, Code: "invalid_key", Message: "需要有效的资格查询密钥"}
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if len(token) < 40 || len(token) > 128 {
		return appkit.Forbidden("查询密钥无效")
	}
	hash := sha256.Sum256([]byte(token))
	var s appkit.Scope
	var keyID, actors string
	var expiry int64
	e := m.rt.DB.QueryRowContext(r.Context(), "SELECT id,tenant_id,project_id,installation_id,actors,expires_at FROM eid_query_keys WHERE token_hash=? AND revoked_at=''", hex.EncodeToString(hash[:])).Scan(&keyID, &s.TenantID, &s.ProjectID, &s.InstallationID, &actors, &expiry)
	if e == sql.ErrNoRows {
		return appkit.Forbidden("查询密钥无效或已撤销")
	}
	if e != nil {
		return e
	}
	if expiry <= time.Now().Unix() {
		return appkit.Forbidden("查询密钥已过期")
	}
	if m.rt.ApplicationEnabled == nil {
		return appkit.Unavailable("应用状态检查不可用")
	}
	enabled, e := m.rt.ApplicationEnabled(r.Context(), s.TenantID, s.ProjectID, "eid")
	if e != nil {
		return e
	}
	if !enabled {
		return appkit.NotFound()
	}
	ids := r.URL.Query()["actorId"]
	if len(ids) != 1 || len(r.URL.Query()) != 1 {
		return appkit.Invalid("仅接受一个 actorId 参数")
	}
	var allowed []string
	if e = json.Unmarshal([]byte(actors), &allowed); e != nil {
		return e
	}
	found := false
	for _, id := range allowed {
		if id == ids[0] {
			found = true
		}
	}
	if !found {
		return appkit.NotFound()
	}
	out, e := m.qualificationValue(r.Context(), s, ids[0])
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, out)
	return nil
}
