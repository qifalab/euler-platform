package trust

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"net/http"
	"regexp"
	"strings"
)

const schemeCols = "id,name,description,status,fields,version,created_at,updated_at"

func scanScheme(row interface{ Scan(...any) error }) (Scheme, error) {
	var v Scheme
	var fields string
	e := row.Scan(&v.ID, &v.Name, &v.Description, &v.Status, &fields, &v.Version, &v.CreatedAt, &v.UpdatedAt)
	if e == nil {
		e = json.Unmarshal([]byte(fields), &v.Fields)
	}
	return v, e
}
func getScheme(ctx context.Context, q queryer, s appkit.Scope, id string) (Scheme, error) {
	v, e := scanScheme(q.QueryRowContext(ctx, "SELECT "+schemeCols+" FROM trust_schemes WHERE tenant_id=? AND project_id=? AND id=?", s.TenantID, s.ProjectID, id))
	return v, notFound(e)
}
func listSchemes(ctx context.Context, q queryer, s appkit.Scope, all bool) ([]Scheme, error) {
	query := "SELECT " + schemeCols + " FROM trust_schemes WHERE tenant_id=? AND project_id=?"
	if !all {
		query += " AND status='active'"
	}
	query += " ORDER BY created_at DESC,id"
	rows, e := q.QueryContext(ctx, query, s.TenantID, s.ProjectID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []Scheme{}
	for rows.Next() {
		v, e := scanScheme(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

// ListAvailableSchemes is deliberately callable from EID's server-side scope.
// A caller cannot select another actor or bypass Trust's enabled state.
func ListAvailableSchemes(ctx context.Context, rt *appkit.Runtime, s appkit.Scope) ([]Scheme, error) {
	if s.ActorID == "" || s.TenantID == "" || s.ProjectID == "" {
		return nil, appkit.Forbidden("缺少已验证的项目身份")
	}
	if rt.ApplicationEnabled == nil {
		return nil, appkit.Unavailable("应用状态检查未配置")
	}
	enabled, e := rt.ApplicationEnabled(ctx, s.TenantID, s.ProjectID, "trust")
	if e != nil {
		return nil, e
	}
	if !enabled {
		return []Scheme{}, nil
	}
	return listSchemes(ctx, rt.DB, s, false)
}
func CheckQualification(ctx context.Context, rt *appkit.Runtime, s appkit.Scope, schemeID string) (bool, error) {
	schemes, e := ListAvailableSchemes(ctx, rt, s)
	if e != nil {
		return false, e
	}
	found := false
	for _, v := range schemes {
		if v.ID == schemeID {
			found = true
			break
		}
	}
	if !found {
		return false, nil
	}
	var status string
	e = rt.DB.QueryRowContext(ctx, "SELECT status FROM trust_submissions WHERE tenant_id=? AND project_id=? AND actor_id=? AND scheme_id=?", s.TenantID, s.ProjectID, s.ActorID, schemeID).Scan(&status)
	if e == sql.ErrNoRows {
		return false, nil
	}
	return status == "approved", e
}
func validateScheme(v *Scheme) error {
	var e error
	v.Name, e = appkit.Name(v.Name, 120)
	if e != nil {
		return e
	}
	if len(v.Description) > 10000 {
		return appkit.Invalid("方案描述过长")
	}
	if v.Status == "" {
		v.Status = "active"
	}
	if v.Status != "active" && v.Status != "inactive" {
		return appkit.Invalid("方案状态无效")
	}
	if len(v.Fields) < 1 || len(v.Fields) > 100 {
		return appkit.Invalid("方案需要 1–100 个字段")
	}
	names := map[string]bool{}
	for i := range v.Fields {
		f := &v.Fields[i]
		if f.ID == "" {
			f.ID = appkit.NewID("fld_")
		}
		if f.Name == "" {
			f.Name = "field_" + strings.ReplaceAll(strings.ReplaceAll(f.ID, "-", "_"), "=", "")
		}
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`).MatchString(f.Name) || names[f.Name] {
			return appkit.Invalid("字段名称必须唯一且仅含字母、数字或下划线")
		}
		names[f.Name] = true
		f.Label, e = appkit.Name(f.Label, 120)
		if e != nil {
			return e
		}
		if len(f.Description) > 2000 {
			return appkit.Invalid("字段说明过长")
		}
		switch f.Type {
		case "text", "email", "phone", "number", "date", "select", "image", "file", "longText":
		default:
			return appkit.Invalid("不支持的字段类型")
		}
		if f.Type == "select" {
			if len(f.Options) < 1 || len(f.Options) > 100 {
				return appkit.Invalid("选择字段需要 1–100 个选项")
			}
			seen := map[string]bool{}
			for _, o := range f.Options {
				if strings.TrimSpace(o) == "" || len(o) > 500 || seen[o] {
					return appkit.Invalid("选项必须非空且唯一")
				}
				seen[o] = true
			}
		}
		r := f.Validations
		if r.Pattern != "" {
			if len(r.Pattern) > 500 {
				return appkit.Invalid("校验规则过长")
			}
			if _, e := regexp.Compile(r.Pattern); e != nil {
				return appkit.Invalid("字段正则规则无效")
			}
		}
		if r.MinLength != nil && (*r.MinLength < 0 || *r.MinLength > 20000) || r.MaxLength != nil && (*r.MaxLength < 1 || *r.MaxLength > 20000) || r.MinLength != nil && r.MaxLength != nil && *r.MinLength > *r.MaxLength || r.Min != nil && r.Max != nil && *r.Min > *r.Max || r.MaxBytes < 0 || r.MaxBytes > 10<<20 {
			return appkit.Invalid("字段校验范围无效")
		}
		for _, mime := range r.Accept {
			switch mime {
			case "image/png", "image/jpeg", "image/gif", "image/webp", "application/pdf", "text/plain":
			default:
				return appkit.Invalid("文件类型只支持常用图片、PDF 或纯文本")
			}
		}
	}
	return nil
}
func (m *Module) schemes(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	all := r.URL.Query().Get("all") == "true"
	if all && !s.Can("admin") && !s.Can("review") {
		return appkit.Forbidden("无方案管理权限")
	}
	v, e := listSchemes(r.Context(), m.rt.DB, s, all)
	if e == nil {
		appkit.JSON(w, 200, map[string]any{"items": v})
	}
	return e
}
func (m *Module) scheme(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	v, e := getScheme(r.Context(), m.rt.DB, s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if v.Status != "active" && !s.Can("admin") && !s.Can("review") {
		return appkit.NotFound()
	}
	appkit.JSON(w, 200, v)
	return nil
}
func (m *Module) createScheme(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var v Scheme
	if e := appkit.Decode(w, r, &v); e != nil {
		return e
	}
	if e := validateScheme(&v); e != nil {
		return e
	}
	v.ID = appkit.NewID("tsc_")
	v.Version = 1
	v.CreatedAt = appkit.Now()
	v.UpdatedAt = v.CreatedAt
	b, _ := json.Marshal(v.Fields)
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(r.Context(), "INSERT INTO trust_schemes VALUES(?,?,?,?,?,?,?,?,?,?)", v.ID, s.TenantID, s.ProjectID, v.Name, v.Description, v.Status, string(b), 1, v.CreatedAt, v.UpdatedAt)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "scheme.create", v.ID, "创建认证方案")
	})
	if e == nil {
		appkit.JSON(w, 201, v)
	}
	return e
}
func (m *Module) updateScheme(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var v Scheme
	if e := appkit.Decode(w, r, &v); e != nil {
		return e
	}
	if e := validateScheme(&v); e != nil {
		return e
	}
	id := r.PathValue("id")
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		old, e := getScheme(r.Context(), tx, s, id)
		if e != nil {
			return e
		}
		if v.Version != old.Version {
			return appkit.Conflict("方案已更新，请刷新后重试")
		}
		v.ID = id
		v.Version++
		v.CreatedAt = old.CreatedAt
		v.UpdatedAt = appkit.Now()
		b, _ := json.Marshal(v.Fields)
		_, e = tx.ExecContext(r.Context(), "UPDATE trust_schemes SET name=?,description=?,status=?,fields=?,version=?,updated_at=? WHERE id=? AND tenant_id=? AND project_id=?", v.Name, v.Description, v.Status, string(b), v.Version, v.UpdatedAt, id, s.TenantID, s.ProjectID)
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "scheme.update", id, "更新认证方案")
	})
	if e == nil {
		appkit.JSON(w, 200, v)
	}
	return e
}
func (m *Module) deleteScheme(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	id := r.PathValue("id")
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		if _, e := getScheme(r.Context(), tx, s, id); e != nil {
			return e
		}
		var count int
		if e := tx.QueryRowContext(r.Context(), "SELECT count(*) FROM trust_submissions WHERE scheme_id=? AND tenant_id=? AND project_id=?", id, s.TenantID, s.ProjectID).Scan(&count); e != nil {
			return e
		}
		if count > 0 {
			return appkit.Conflict("已有申请记录，请停用方案以保留历史")
		}
		if _, e := tx.ExecContext(r.Context(), "DELETE FROM trust_materials WHERE scheme_id=? AND tenant_id=? AND project_id=?", id, s.TenantID, s.ProjectID); e != nil {
			return e
		}
		if _, e := tx.ExecContext(r.Context(), "DELETE FROM trust_schemes WHERE id=? AND tenant_id=? AND project_id=?", id, s.TenantID, s.ProjectID); e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "scheme.delete", id, "删除空认证方案")
	})
	if e == nil {
		appkit.JSON(w, 204, nil)
	}
	return e
}
