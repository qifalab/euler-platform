package trust

import (
	"context"
	"database/sql"
	"encoding/base64"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

type materialPrivate struct {
	ActorID, SchemeID, SubmissionID string
	Body                            []byte
}

func (m *Module) ownMaterials(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT id,metadata FROM trust_materials WHERE tenant_id=? AND project_id=? AND actor_id=? AND submission_id='' ORDER BY created_at DESC", s.TenantID, s.ProjectID, s.ActorID)
	if e != nil {
		return e
	}
	defer rows.Close()
	items := []Material{}
	for rows.Next() {
		var id string
		var b []byte
		if e = rows.Scan(&id, &b); e != nil {
			return e
		}
		var v Material
		if e = m.open(b, "material-meta:"+id, &v); e != nil {
			return e
		}
		items = append(items, v)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": items})
	return nil
}

func (m *Module) getMaterial(ctx context.Context, q queryer, s appkit.Scope, id string, body bool) (Material, materialPrivate, error) {
	var v Material
	var p materialPrivate
	var b []byte
	cols := "metadata,actor_id,scheme_id,submission_id"
	if body {
		cols += ",body"
	}
	row := q.QueryRowContext(ctx, "SELECT "+cols+" FROM trust_materials WHERE tenant_id=? AND project_id=? AND id=?", s.TenantID, s.ProjectID, id)
	var e error
	if body {
		e = row.Scan(&b, &p.ActorID, &p.SchemeID, &p.SubmissionID, &p.Body)
	} else {
		e = row.Scan(&b, &p.ActorID, &p.SchemeID, &p.SubmissionID)
	}
	if e != nil {
		return v, p, notFound(e)
	}
	e = m.open(b, "material-meta:"+id, &v)
	return v, p, e
}
func (m *Module) uploadMaterial(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	r.Body = http.MaxBytesReader(w, r.Body, (10<<20)+(64<<10))
	if e := r.ParseMultipartForm(1 << 20); e != nil {
		return appkit.Invalid("文件请求无效或超过 10 MiB")
	}
	defer r.MultipartForm.RemoveAll()
	schemeID := r.FormValue("schemeId")
	fieldName := r.FormValue("fieldName")
	scheme, e := getScheme(r.Context(), m.rt.DB, s, schemeID)
	if e != nil {
		return e
	}
	if scheme.Status != "active" {
		return appkit.Conflict("方案已停用")
	}
	var field *Field
	for _, f := range scheme.Fields {
		if f.Name == fieldName && (f.Type == "file" || f.Type == "image") {
			copy := f
			field = &copy
		}
	}
	if field == nil {
		return appkit.Invalid("上传字段不存在")
	}
	file, h, e := r.FormFile("file")
	if e != nil {
		return appkit.Invalid("请选择文件")
	}
	defer file.Close()
	data, e := io.ReadAll(io.LimitReader(file, (10<<20)+1))
	if e != nil || len(data) == 0 || len(data) > 10<<20 {
		return appkit.Invalid("文件为空或超过 10 MiB")
	}
	if field.Validations.MaxBytes > 0 && int64(len(data)) > field.Validations.MaxBytes {
		return appkit.Invalid("文件超过本字段允许大小")
	}
	kind := strings.Split(http.DetectContentType(data), ";")[0]
	allowed := map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "application/pdf": true, "text/plain": true}
	if !allowed[kind] || field.Type == "image" && !strings.HasPrefix(kind, "image/") {
		return appkit.Invalid("仅支持图片、PDF 和纯文本材料")
	}
	if len(field.Validations.Accept) > 0 {
		ok := false
		for _, t := range field.Validations.Accept {
			if t == kind {
				ok = true
			}
		}
		if !ok {
			return appkit.Invalid("文件类型不符合字段要求")
		}
	}
	name := strings.ReplaceAll(filepath.Base(strings.ReplaceAll(h.Filename, "\\", "/")), "\x00", "")
	if len(name) > 200 || strings.TrimSpace(name) == "" {
		return appkit.Invalid("文件名无效")
	}
	id := appkit.NewID("tma_")
	v := Material{ID: id, Name: name, Type: kind, Size: int64(len(data)), FieldName: fieldName, CreatedAt: appkit.Now()}
	meta, e := m.seal(v, "material-meta:"+id)
	if e != nil {
		return e
	}
	enc, e := m.rt.Encrypt(base64.StdEncoding.EncodeToString(data), "trust:material-body:"+id)
	if e != nil {
		return e
	}
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		current, err := getScheme(r.Context(), tx, s, schemeID)
		if err != nil {
			return err
		}
		if current.Status != "active" || current.Version != scheme.Version {
			return appkit.Conflict("上传期间方案发生变化，请刷新后重试")
		}
		var count int
		if e := tx.QueryRowContext(r.Context(), "SELECT count(*) FROM trust_materials WHERE tenant_id=? AND project_id=? AND actor_id=? AND submission_id=''", s.TenantID, s.ProjectID, s.ActorID).Scan(&count); e != nil {
			return e
		}
		if count >= 100 {
			return appkit.Conflict("未提交材料已达 100 份，请删除不用的材料")
		}
		if _, e := tx.ExecContext(r.Context(), "INSERT INTO trust_materials VALUES(?,?,?,?,?,?,?,?,?,?)", id, s.TenantID, s.ProjectID, s.ActorID, schemeID, fieldName, "", meta, enc, v.CreatedAt); e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "material.upload", id, "上传认证材料")
	})
	if e == nil {
		appkit.JSON(w, 201, v)
	}
	return e
}
func (m *Module) ownMaterial(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	return m.serveMaterial(w, r, s, false)
}
func (m *Module) reviewMaterial(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	return m.serveMaterial(w, r, s, true)
}
func (m *Module) serveMaterial(w http.ResponseWriter, r *http.Request, s appkit.Scope, review bool) error {
	v, p, e := m.getMaterial(r.Context(), m.rt.DB, s, r.PathValue("id"), true)
	if e != nil {
		return e
	}
	if !review && p.ActorID != s.ActorID {
		return appkit.NotFound()
	}
	if review {
		if p.SubmissionID == "" {
			return appkit.NotFound()
		}
		if _, e = m.loadSubmission(r.Context(), m.rt.DB, s, p.SubmissionID, false); e != nil {
			return e
		}
	}
	return m.writeMaterial(w, r, s, v, p)
}
func (m *Module) writeMaterial(w http.ResponseWriter, r *http.Request, s appkit.Scope, v Material, p materialPrivate) error {
	plain, e := m.rt.Decrypt(p.Body, "trust:material-body:"+v.ID)
	if e != nil {
		return e
	}
	body, e := base64.StdEncoding.DecodeString(plain)
	if e != nil {
		return e
	}
	if e = m.rt.Audit(r.Context(), nil, s, "material.download", v.ID, "访问认证材料"); e != nil {
		return e
	}
	disposition := "attachment"
	if r.URL.Query().Get("preview") == "1" {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", v.Type)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": v.Name}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(200)
	_, e = w.Write(body)
	return e
}
func (m *Module) deleteMaterial(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	id := r.PathValue("id")
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		_, p, e := m.getMaterial(r.Context(), tx, s, id, false)
		if e != nil {
			return e
		}
		if p.ActorID != s.ActorID {
			return appkit.NotFound()
		}
		if p.SubmissionID != "" {
			return appkit.Conflict("已提交的材料随申请保留，不能直接删除")
		}
		if _, e = tx.ExecContext(r.Context(), "DELETE FROM trust_materials WHERE id=? AND tenant_id=? AND project_id=?", id, s.TenantID, s.ProjectID); e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "material.delete", id, "删除未提交材料")
	})
	if e == nil {
		appkit.JSON(w, 204, nil)
	}
	return e
}
