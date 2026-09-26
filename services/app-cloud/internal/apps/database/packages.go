package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// Package quantities are immutable snapshots when redeemed; editing a template
// never silently changes a project's existing entitlement.
type Quota struct {
	MySQLMB       int64 `json:"mysqlMB"`
	MySQLCount    int64 `json:"mysqlCount"`
	PostgresMB    int64 `json:"postgresMB"`
	PostgresCount int64 `json:"postgresCount"`
	StorageBytes  int64 `json:"storageBytes"`
	Buckets       int64 `json:"buckets"`
}

func (q Quota) valid() bool {
	return q.MySQLMB >= 0 && q.MySQLMB <= 1e9 && q.MySQLCount >= 0 && q.MySQLCount <= 100000 && q.PostgresMB >= 0 && q.PostgresMB <= 1e9 && q.PostgresCount >= 0 && q.PostgresCount <= 100000 && q.StorageBytes >= 0 && q.StorageBytes <= 1<<60 && q.Buckets >= 0 && q.Buckets <= 100000
}
func (q *Quota) add(v Quota) {
	capAdd := func(current, extra, limit int64) int64 {
		if current >= limit || extra >= limit-current {
			return limit
		}
		return current + extra
	}
	q.MySQLMB = capAdd(q.MySQLMB, v.MySQLMB, 1e9)
	q.MySQLCount = capAdd(q.MySQLCount, v.MySQLCount, 100000)
	q.PostgresMB = capAdd(q.PostgresMB, v.PostgresMB, 1e9)
	q.PostgresCount = capAdd(q.PostgresCount, v.PostgresCount, 100000)
	q.StorageBytes = capAdd(q.StorageBytes, v.StorageBytes, 1<<60)
	q.Buckets = capAdd(q.Buckets, v.Buckets, 100000)
}

type Template struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Quota       Quota  `json:"quota"`
	Days        int    `json:"days"`
	Active      bool   `json:"active"`
	CreatedAt   string `json:"createdAt"`
}
type Grant struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Quota     Quota  `json:"quota"`
	ExpiresAt string `json:"expiresAt"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"createdAt"`
}
type Code struct {
	ID           string `json:"id"`
	Code         string `json:"code"`
	TemplateID   string `json:"templateId"`
	TemplateName string `json:"templateName"`
	MaxUses      int    `json:"maxUses"`
	Uses         int    `json:"uses"`
	Active       bool   `json:"active"`
	ExpiresAt    string `json:"expiresAt"`
	Note         string `json:"note"`
}
type ledger struct {
	rt     *appkit.Runtime
	prefix string
	base   Quota
}

func (l ledger) migrate(ctx context.Context) error {
	_, e := l.rt.DB.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS `+l.prefix+`_templates(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,name TEXT NOT NULL,description TEXT NOT NULL,quota TEXT NOT NULL,days INTEGER NOT NULL,active INTEGER NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS `+l.prefix+`_codes(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,code TEXT NOT NULL UNIQUE,template_id TEXT NOT NULL,max_uses INTEGER NOT NULL,uses INTEGER NOT NULL DEFAULT 0,active INTEGER NOT NULL,expires_at TEXT NOT NULL,note TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS `+l.prefix+`_grants(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,name TEXT NOT NULL,quota TEXT NOT NULL,expires_at TEXT NOT NULL,created_at TEXT NOT NULL,code_id TEXT,UNIQUE(tenant_id,project_id,code_id));
CREATE INDEX IF NOT EXISTS `+l.prefix+`_grants_scope ON `+l.prefix+`_grants(tenant_id,project_id,expires_at);`)
	return e
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (l ledger) quota(ctx context.Context, q queryer, s appkit.Scope) (Quota, error) {
	out := l.base
	rows, e := q.QueryContext(ctx, "SELECT quota FROM "+l.prefix+"_grants WHERE tenant_id=? AND project_id=? AND expires_at>?", s.TenantID, s.ProjectID, appkit.Now())
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var v Quota
		if e = rows.Scan(&raw); e != nil {
			return out, e
		}
		if e = json.Unmarshal([]byte(raw), &v); e != nil {
			return out, e
		}
		out.add(v)
	}
	return out, rows.Err()
}
func (l ledger) templates(ctx context.Context, s appkit.Scope) ([]Template, error) {
	rows, e := l.rt.DB.QueryContext(ctx, "SELECT id,name,description,quota,days,active,created_at FROM "+l.prefix+"_templates WHERE tenant_id=? AND project_id=? ORDER BY created_at DESC", s.TenantID, s.ProjectID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Template{}
	for rows.Next() {
		var v Template
		var raw string
		if e = rows.Scan(&v.ID, &v.Name, &v.Description, &raw, &v.Days, &v.Active, &v.CreatedAt); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &v.Quota); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (l ledger) template(ctx context.Context, q queryer, s appkit.Scope, id string) (Template, error) {
	var v Template
	var raw string
	e := q.QueryRowContext(ctx, "SELECT id,name,description,quota,days,active,created_at FROM "+l.prefix+"_templates WHERE id=? AND tenant_id=? AND project_id=?", id, s.TenantID, s.ProjectID).Scan(&v.ID, &v.Name, &v.Description, &raw, &v.Days, &v.Active, &v.CreatedAt)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &v.Quota)
	}
	return v, e
}
func (l ledger) register(mux *http.ServeMux) {
	appkit.Handle(mux, "GET /packages", "read", l.listPackages)
	appkit.Handle(mux, "POST /packages/redeem", "manage", l.redeem)
	appkit.Handle(mux, "GET /admin/templates", "admin", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		items, e := l.templates(r.Context(), s)
		if e != nil {
			return e
		}
		appkit.JSON(w, 200, map[string]any{"items": items})
		return nil
	})
	appkit.Handle(mux, "POST /admin/templates", "admin", l.saveTemplate)
	appkit.Handle(mux, "PUT /admin/templates/{id}", "admin", l.saveTemplate)
	appkit.Handle(mux, "DELETE /admin/templates/{id}", "admin", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		return l.activeTemplate(w, r, s, false)
	})
	appkit.Handle(mux, "POST /admin/templates/{id}/enable", "admin", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		return l.activeTemplate(w, r, s, true)
	})
	appkit.Handle(mux, "GET /admin/codes", "admin", l.listCodes)
	appkit.Handle(mux, "POST /admin/codes", "admin", l.createCodes)
	appkit.Handle(mux, "DELETE /admin/codes/{id}", "admin", l.disableCode)
	appkit.Handle(mux, "POST /admin/grants", "admin", l.grant)
}
func (l ledger) listPackages(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	rows, e := l.rt.DB.QueryContext(r.Context(), "SELECT id,name,quota,expires_at,created_at FROM "+l.prefix+"_grants WHERE tenant_id=? AND project_id=? ORDER BY created_at DESC", s.TenantID, s.ProjectID)
	if e != nil {
		return e
	}
	out := []Grant{}
	for rows.Next() {
		var v Grant
		var raw string
		if e = rows.Scan(&v.ID, &v.Name, &raw, &v.ExpiresAt, &v.CreatedAt); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal([]byte(raw), &v.Quota); e != nil {
			rows.Close()
			return e
		}
		expiry, _ := time.Parse(time.RFC3339Nano, v.ExpiresAt)
		v.Active = expiry.After(time.Now())
		out = append(out, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	q, e := l.quota(r.Context(), l.rt.DB, s)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": out, "quota": q, "base": l.base})
	return nil
}
func (l ledger) saveTemplate(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var v Template
	if e := appkit.Decode(w, r, &v); e != nil {
		return e
	}
	name, e := appkit.Name(v.Name, 100)
	if e != nil {
		return e
	}
	v.Name = name
	if !v.Quota.valid() || v.Days < 1 || v.Days > 36500 || len(v.Description) > 2000 {
		return appkit.Invalid("资源包数量、有效期或说明不符合要求")
	}
	raw, _ := json.Marshal(v.Quota)
	v.ID = r.PathValue("id")
	created := v.ID == ""
	if created {
		v.ID = appkit.NewID("tpl_")
		v.Active = true
		v.CreatedAt = appkit.Now()
	}
	e = l.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		if created {
			_, e = tx.ExecContext(r.Context(), "INSERT INTO "+l.prefix+"_templates VALUES(?,?,?,?,?,?,?,?,?)", v.ID, s.TenantID, s.ProjectID, v.Name, v.Description, string(raw), v.Days, v.Active, v.CreatedAt)
		} else {
			var result sql.Result
			result, e = tx.ExecContext(r.Context(), "UPDATE "+l.prefix+"_templates SET name=?,description=?,quota=?,days=?,active=? WHERE id=? AND tenant_id=? AND project_id=?", v.Name, v.Description, string(raw), v.Days, v.Active, v.ID, s.TenantID, s.ProjectID)
			if e == nil {
				n, _ := result.RowsAffected()
				if n == 0 {
					return appkit.NotFound()
				}
			}
		}
		if e != nil {
			return e
		}
		return l.rt.Audit(r.Context(), tx, s, "template.saved", v.ID, "保存资源包模板")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, v)
	return nil
}
func (l ledger) activeTemplate(w http.ResponseWriter, r *http.Request, s appkit.Scope, active bool) error {
	e := l.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), "UPDATE "+l.prefix+"_templates SET active=? WHERE id=? AND tenant_id=? AND project_id=?", active, r.PathValue("id"), s.TenantID, s.ProjectID)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return appkit.NotFound()
		}
		return l.rt.Audit(r.Context(), tx, s, "template.status_changed", r.PathValue("id"), "更改资源包模板状态")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]bool{"active": active})
	return nil
}
func (l ledger) listCodes(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	where := "c.tenant_id=? AND c.project_id=?"
	args := []any{s.TenantID, s.ProjectID}
	if id := r.URL.Query().Get("templateId"); id != "" {
		where += " AND c.template_id=?"
		args = append(args, id)
	}
	if r.URL.Query().Get("includeUsed") == "false" {
		where += " AND c.active=1 AND c.uses<c.max_uses AND (c.expires_at='' OR c.expires_at>?)"
		args = append(args, appkit.Now())
	}
	var total int
	if err := l.rt.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM "+l.prefix+"_codes c WHERE "+where, args...).Scan(&total); err != nil {
		return err
	}
	args = append(args, limit, offset)
	rows, err := l.rt.DB.QueryContext(r.Context(), "SELECT c.id,c.code,c.template_id,t.name,c.max_uses,c.uses,c.active,c.expires_at,c.note FROM "+l.prefix+"_codes c JOIN "+l.prefix+"_templates t ON t.id=c.template_id WHERE "+where+" ORDER BY c.created_at DESC,c.id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Code{}
	for rows.Next() {
		var v Code
		if err = rows.Scan(&v.ID, &v.Code, &v.TemplateID, &v.TemplateName, &v.MaxUses, &v.Uses, &v.Active, &v.ExpiresAt, &v.Note); err != nil {
			return err
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	appkit.JSON(w, 200, map[string]any{"items": out, "total": total, "limit": limit, "offset": offset})
	return nil
}

func (l ledger) createCodes(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		TemplateID string `json:"templateId"`
		Count      int    `json:"count"`
		MaxUses    int    `json:"maxUses"`
		ExpiresAt  string `json:"expiresAt"`
		Note       string `json:"note"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if input.Count < 1 || input.Count > 100 || input.MaxUses < 1 || input.MaxUses > 100000 || len(input.Note) > 1000 {
		return appkit.Invalid("生成数量和使用次数不符合要求")
	}
	if input.ExpiresAt != "" {
		expiry, e := time.Parse(time.RFC3339, input.ExpiresAt)
		if e != nil || !expiry.After(time.Now()) {
			return appkit.Invalid("有效期应为未来时间")
		}
		input.ExpiresAt = expiry.UTC().Format(time.RFC3339Nano)
	}
	out := []Code{}
	e := l.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		tpl, e := l.template(r.Context(), tx, s, input.TemplateID)
		if e != nil {
			return e
		}
		if !tpl.Active {
			return appkit.Conflict("模板已停用")
		}
		for i := 0; i < input.Count; i++ {
			v := Code{ID: appkit.NewID("code_"), Code: strings.ToUpper(appkit.NewID("")), TemplateID: tpl.ID, TemplateName: tpl.Name, MaxUses: input.MaxUses, Active: true, ExpiresAt: input.ExpiresAt, Note: input.Note}
			_, e = tx.ExecContext(r.Context(), "INSERT INTO "+l.prefix+"_codes(id,tenant_id,project_id,code,template_id,max_uses,uses,active,expires_at,note,created_at) VALUES(?,?,?,?,?,?,0,1,?,?,?)", v.ID, s.TenantID, s.ProjectID, v.Code, v.TemplateID, v.MaxUses, v.ExpiresAt, v.Note, appkit.Now())
			if e != nil {
				return e
			}
			out = append(out, v)
		}
		return l.rt.Audit(r.Context(), tx, s, "codes.created", tpl.ID, "批量生成资源包兑换码")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, map[string]any{"items": out})
	return nil
}
func (l ledger) disableCode(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	e := l.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), "UPDATE "+l.prefix+"_codes SET active=0 WHERE id=? AND tenant_id=? AND project_id=?", r.PathValue("id"), s.TenantID, s.ProjectID)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return appkit.NotFound()
		}
		return l.rt.Audit(r.Context(), tx, s, "code.disabled", r.PathValue("id"), "停用资源包兑换码")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]bool{"active": false})
	return nil
}
func (l ledger) insertGrant(ctx context.Context, tx *sql.Tx, s appkit.Scope, t Template, codeID any) (Grant, error) {
	v := Grant{ID: appkit.NewID("pkg_"), Name: t.Name, Quota: t.Quota, ExpiresAt: time.Now().UTC().Add(time.Duration(t.Days) * 24 * time.Hour).Format(time.RFC3339Nano), Active: true, CreatedAt: appkit.Now()}
	raw, _ := json.Marshal(v.Quota)
	_, e := tx.ExecContext(ctx, "INSERT INTO "+l.prefix+"_grants VALUES(?,?,?,?,?,?,?,?)", v.ID, s.TenantID, s.ProjectID, v.Name, string(raw), v.ExpiresAt, v.CreatedAt, codeID)
	return v, e
}
func (l ledger) redeem(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Code string `json:"code"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	input.Code = strings.ToUpper(strings.TrimSpace(input.Code))
	if len(input.Code) > 100 {
		return appkit.Invalid("兑换码不符合要求")
	}
	var out Grant
	e := l.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var id, tid, expiry string
		var active bool
		var uses, max int
		e := tx.QueryRowContext(r.Context(), "SELECT id,template_id,expires_at,active,uses,max_uses FROM "+l.prefix+"_codes WHERE code=? AND tenant_id=?", input.Code, s.TenantID).Scan(&id, &tid, &expiry, &active, &uses, &max)
		if e != nil {
			return appkit.Invalid("兑换码不存在或不可用于当前项目")
		}
		if !active || uses >= max || (expiry != "" && expiry <= appkit.Now()) {
			return appkit.Conflict("兑换码已失效或用尽")
		}
		var count int
		if e = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM "+l.prefix+"_grants WHERE tenant_id=? AND project_id=? AND code_id=?", s.TenantID, s.ProjectID, id).Scan(&count); e != nil {
			return e
		}
		if count > 0 {
			return appkit.Conflict("当前项目已兑换此码")
		}
		var tpl Template
		var raw string
		e = tx.QueryRowContext(r.Context(), "SELECT id,name,description,quota,days,active,created_at FROM "+l.prefix+"_templates WHERE id=? AND tenant_id=?", tid, s.TenantID).Scan(&tpl.ID, &tpl.Name, &tpl.Description, &raw, &tpl.Days, &tpl.Active, &tpl.CreatedAt)
		if e == nil {
			e = json.Unmarshal([]byte(raw), &tpl.Quota)
		}
		if e != nil {
			return e
		}
		if !tpl.Active {
			return appkit.Conflict("资源包模板已停用")
		}
		out, e = l.insertGrant(r.Context(), tx, s, tpl, id)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(r.Context(), "UPDATE "+l.prefix+"_codes SET uses=uses+1 WHERE id=?", id); e != nil {
			return e
		}
		return l.rt.Audit(r.Context(), tx, s, "package.redeemed", out.ID, "兑换资源包")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, out)
	return nil
}
func (l ledger) grant(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		TemplateID string `json:"templateId"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	var out Grant
	e := l.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		tpl, e := l.template(r.Context(), tx, s, input.TemplateID)
		if e != nil {
			return e
		}
		if !tpl.Active {
			return appkit.Conflict("模板已停用")
		}
		out, e = l.insertGrant(r.Context(), tx, s, tpl, nil)
		if e != nil {
			return e
		}
		return l.rt.Audit(r.Context(), tx, s, "package.granted", out.ID, "运营人员赠送项目资源包")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, out)
	return nil
}
