package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

const multipartPartSize int64 = 8 * 1024 * 1024

type multipartSession struct {
	ID          string       `json:"id"`
	Key         string       `json:"key"`
	Size        int64        `json:"size"`
	ContentType string       `json:"contentType"`
	State       string       `json:"state"`
	ExpiresAt   string       `json:"expiresAt"`
	PartSize    int64        `json:"partSize"`
	Parts       []UploadPart `json:"parts"`
	RemoteID    string       `json:"-"`
}

func (m *Module) advanced() (AdvancedPlane, error) {
	p, ok := m.plane.(AdvancedPlane)
	if !ok {
		return nil, appkit.Unavailable("当前对象存储适配器不支持高级功能")
	}
	return p, nil
}
func (m *Module) advancedMigrate(ctx context.Context) error {
	_, e := m.rt.DB.ExecContext(ctx, `
 CREATE TABLE IF NOT EXISTS storage_multipart(upload_id TEXT PRIMARY KEY,remote_id TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS storage_multipart_parts(upload_id TEXT NOT NULL,part_number INTEGER NOT NULL,size INTEGER NOT NULL,etag TEXT NOT NULL,sha256 TEXT NOT NULL,PRIMARY KEY(upload_id,part_number));
 CREATE TABLE IF NOT EXISTS storage_upload_cleanup(upload_id TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS storage_upload_guards(upload_id TEXT PRIMARY KEY,expected_etag TEXT NOT NULL);
 `)
	return e
}
func (m *Module) advancedRoutes(mux *http.ServeMux) {
	appkit.Handle(mux, "GET /buckets/{id}/multipart", "write", m.listMultipart)
	appkit.Handle(mux, "POST /buckets/{id}/multipart", "write", m.beginMultipart)
	appkit.Handle(mux, "GET /buckets/{id}/multipart/{upload}", "write", m.getMultipart)
	appkit.Handle(mux, "PUT /buckets/{id}/multipart/{upload}/parts/{part}", "write", m.putPart)
	appkit.Handle(mux, "POST /buckets/{id}/multipart/{upload}/complete", "write", m.completeMultipart)
	appkit.Handle(mux, "DELETE /buckets/{id}/multipart/{upload}", "write", m.abortMultipart)
	appkit.Handle(mux, "GET /buckets/{id}/versioning", "read", m.getVersioning)
	appkit.Handle(mux, "PUT /buckets/{id}/versioning", "manage", m.setVersioning)
	appkit.Handle(mux, "GET /buckets/{id}/versions", "read", m.listVersions)
	appkit.Handle(mux, "POST /buckets/{id}/versions/restore", "write", m.restoreVersion)
	appkit.Handle(mux, "DELETE /buckets/{id}/versions", "manage", m.deleteVersion)
	appkit.Handle(mux, "GET /buckets/{id}/lifecycle", "manage", m.getLifecycle)
	appkit.Handle(mux, "PUT /buckets/{id}/lifecycle", "manage", m.setLifecycle)
	appkit.Handle(mux, "POST /buckets/{id}/objects/transfer", "write", m.transferObject)
}

// Reserve a destination before issuing upload capabilities. Existing targets
// require an explicit ETag, so a second writer cannot silently replace a file.
func (m *Module) guardTarget(ctx context.Context, b Bucket, key, expected string) (string, error) {
	old, e := m.plane.Stat(ctx, b.Name, "files/"+key)
	if absent(e) {
		if expected != "" {
			return "", appkit.Conflict("目标文件已改变，请刷新")
		}
		return "", nil
	}
	if e != nil {
		return "", appkit.Unavailable("无法检查目标文件")
	}
	if expected == "" || old.ETag != expected {
		return "", appkit.Conflict("目标已存在；请明确确认替换并提供当前 ETag")
	}
	return old.ETag, nil
}
func (m *Module) recordGuard(ctx context.Context, id, etag string) error {
	_, e := m.rt.DB.ExecContext(ctx, "INSERT INTO storage_upload_guards VALUES(?,?)", id, etag)
	return e
}
func (m *Module) checkGuard(ctx context.Context, b Bucket, id, key string) error {
	var expected string
	e := m.rt.DB.QueryRowContext(ctx, "SELECT expected_etag FROM storage_upload_guards WHERE upload_id=?", id).Scan(&expected)
	if e == sql.ErrNoRows {
		return nil
	}
	if e != nil {
		return e
	}
	_, e = m.guardTarget(ctx, b, key, expected)
	return e
}
func (m *Module) beginMultipart(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Key          string `json:"key"`
		Size         int64  `json:"size"`
		ContentType  string `json:"contentType"`
		ExpectedETag string `json:"expectedETag"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if e := validKey(input.Key, false); e != nil {
		return e
	}
	if strings.HasSuffix(input.Key, "/") || input.Size <= 0 || input.Size > multipartPartSize*10000 || len(input.ContentType) > 255 || strings.ContainsAny(input.ContentType, "\r\n") {
		return appkit.Invalid("文件大小或类型不符合要求")
	}
	if input.ContentType == "" {
		input.ContentType = "application/octet-stream"
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	etag, e := m.guardTarget(r.Context(), b, input.Key, input.ExpectedETag)
	if e != nil {
		return e
	}
	id, e := m.reserve(r.Context(), s, b, input.Key, input.Size, input.ContentType)
	if e != nil {
		return e
	}
	// Persist the session row before creating S3 state; stale reservations are
	// reclaimed by the same scheduled cleanup even if the process exits here.
	if e = m.recordGuard(r.Context(), id, etag); e != nil {
		return e
	}
	remote, e := p.BeginMultipart(r.Context(), b.Name, "staging/"+id, input.ContentType)
	if e != nil {
		_, _ = m.rt.DB.ExecContext(context.Background(), "UPDATE storage_uploads SET state='cancelled' WHERE id=?", id)
		return appkit.Unavailable("无法创建 S3 分片上传")
	}
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(r.Context(), "INSERT INTO storage_multipart VALUES(?,?)", id, remote); e != nil {
			return e
		}
		_, e := tx.ExecContext(r.Context(), "UPDATE storage_uploads SET expires_at=? WHERE id=?", time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339Nano), id)
		return e
	})
	if e != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = p.AbortMultipart(ctx, b.Name, "staging/"+id, remote)
		return e
	}
	v, e := m.multipart(r.Context(), s, b, id)
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, v)
	return nil
}
func (m *Module) multipart(ctx context.Context, s appkit.Scope, b Bucket, id string) (multipartSession, error) {
	var v multipartSession
	v.PartSize = multipartPartSize
	v.Parts = []UploadPart{}
	e := m.rt.DB.QueryRowContext(ctx, `SELECT u.id,u.object_key,u.expected_bytes,u.content_type,u.state,u.expires_at,m.remote_id FROM storage_uploads u JOIN storage_multipart m ON m.upload_id=u.id WHERE u.id=? AND u.tenant_id=? AND u.project_id=? AND u.bucket_id=? AND u.actor_id=?`, id, s.TenantID, s.ProjectID, b.ID, s.ActorID).Scan(&v.ID, &v.Key, &v.Size, &v.ContentType, &v.State, &v.ExpiresAt, &v.RemoteID)
	if e != nil {
		return v, e
	}
	rows, e := m.rt.DB.QueryContext(ctx, "SELECT part_number,size,etag,sha256 FROM storage_multipart_parts WHERE upload_id=? ORDER BY part_number", id)
	if e != nil {
		return v, e
	}
	defer rows.Close()
	for rows.Next() {
		var part UploadPart
		if e = rows.Scan(&part.Number, &part.Size, &part.ETag, &part.SHA256); e != nil {
			return v, e
		}
		v.Parts = append(v.Parts, part)
	}
	return v, rows.Err()
}
func (m *Module) listMultipart(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	rows, e := m.rt.DB.QueryContext(r.Context(), `SELECT u.id FROM storage_uploads u JOIN storage_multipart m ON m.upload_id=u.id WHERE u.tenant_id=? AND u.project_id=? AND u.bucket_id=? AND u.actor_id=? AND u.state='pending' AND u.expires_at>? ORDER BY u.created_at DESC`, s.TenantID, s.ProjectID, b.ID, s.ActorID, appkit.Now())
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	out := []multipartSession{}
	for _, id := range ids {
		v, e := m.multipart(r.Context(), s, b, id)
		if e != nil {
			return e
		}
		out = append(out, v)
	}
	appkit.JSON(w, 200, map[string]any{"items": out})
	return nil
}
func (m *Module) getMultipart(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	v, e := m.multipart(r.Context(), s, b, r.PathValue("upload"))
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, v)
	return nil
}
func pendingMultipart(v multipartSession) error {
	if v.State != "pending" || v.ExpiresAt <= appkit.Now() {
		return appkit.Conflict("分片会话已结束或过期")
	}
	return nil
}
func (m *Module) putPart(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	n, e := strconv.Atoi(r.PathValue("part"))
	if e != nil || n < 1 || n > 10000 {
		return appkit.Invalid("分片编号不正确")
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	v, e := m.multipart(r.Context(), s, b, r.PathValue("upload"))
	if e != nil {
		return e
	}
	if e = pendingMultipart(v); e != nil {
		return e
	}
	expected := min(multipartPartSize, v.Size-int64(n-1)*multipartPartSize)
	if expected <= 0 {
		return appkit.Invalid("分片超出文件范围")
	}
	// Buffer only one bounded part. This rejects an incorrect byte count before
	// S3 sees data and prevents a client from bypassing the reserved quota.
	r.Body = http.MaxBytesReader(w, r.Body, expected+65536)
	if e = r.ParseMultipartForm(expected + 65536); e != nil {
		return appkit.Invalid("分片请求超限或格式错误")
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, e := r.FormFile("file")
	if e != nil {
		return appkit.Invalid("缺少分片数据")
	}
	defer file.Close()
	if header.Size != expected {
		return appkit.Invalid("分片大小与预留大小不一致")
	}
	data, e := io.ReadAll(io.LimitReader(file, expected+1))
	if e != nil || int64(len(data)) != expected {
		return appkit.Invalid("分片数据不完整")
	}
	digest := sha256.Sum256(data)
	sha := hex.EncodeToString(digest[:])
	if r.FormValue("sha256") != sha {
		return appkit.Invalid("分片 SHA-256 校验失败")
	}
	for _, part := range v.Parts {
		if part.Number == n {
			if part.SHA256 != sha {
				return appkit.Conflict("此分片已经上传了不同内容；请中止后重新上传")
			}
			appkit.JSON(w, 200, part)
			return nil
		}
	}
	etag, e := p.UploadPart(r.Context(), b.Name, "staging/"+v.ID, v.RemoteID, n, bytes.NewReader(data), expected, sha)
	if e != nil {
		return appkit.Unavailable("S3 分片写入未确认，请重试同一分片")
	}
	if _, e = m.rt.DB.ExecContext(r.Context(), "INSERT INTO storage_multipart_parts VALUES(?,?,?,?,?) ON CONFLICT(upload_id,part_number) DO UPDATE SET size=excluded.size,etag=excluded.etag,sha256=excluded.sha256", v.ID, n, expected, etag, sha); e != nil {
		return e
	}
	appkit.JSON(w, 200, UploadPart{Number: n, Size: expected, ETag: etag, SHA256: sha})
	return nil
}
func (m *Module) completeMultipart(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	v, e := m.multipart(r.Context(), s, b, r.PathValue("upload"))
	if e != nil {
		return e
	}
	if v.State != "completed" {
		if e = pendingMultipart(v); e != nil {
			return e
		}
		count := int((v.Size + multipartPartSize - 1) / multipartPartSize)
		if len(v.Parts) != count {
			return appkit.Conflict("尚有分片未上传")
		}
		for i, part := range v.Parts {
			if part.Number != i+1 || part.Size != min(multipartPartSize, v.Size-int64(i)*multipartPartSize) {
				return appkit.Conflict("分片记录不完整")
			}
		}
		// A completed staging object proves S3 already accepted an earlier complete
		// request whose response may have been lost. Do not complete it a second time.
		if check, ok := m.plane.(interface {
			Published(context.Context, string, string, string) (bool, error)
		}); ok {
			published, err := check.Published(r.Context(), b.Name, "files/"+v.Key, v.ID)
			if err != nil {
				return appkit.Unavailable("无法核验发布状态")
			}
			if published {
				obj, err := m.publish(r.Context(), s, b, v.ID)
				if err != nil {
					return err
				}
				appkit.JSON(w, 200, obj)
				return nil
			}
		}
		staged, e := m.plane.Stat(r.Context(), b.Name, "staging/"+v.ID)
		if absent(e) {
			if e = p.CompleteMultipart(r.Context(), b.Name, "staging/"+v.ID, v.RemoteID, v.Parts); e != nil {
				return appkit.Unavailable("分片合并未确认，请重试")
			}
		} else if e != nil {
			return appkit.Unavailable("无法确认临时对象")
		} else if staged.Size != v.Size {
			return appkit.Conflict("临时对象大小不一致")
		}
	}
	obj, e := m.publish(r.Context(), s, b, v.ID)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, obj)
	return nil
}
func (m *Module) abortMultipart(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	v, e := m.multipart(r.Context(), s, b, r.PathValue("upload"))
	if e != nil {
		return e
	}
	if v.State == "completed" {
		return appkit.Conflict("已发布文件不能中止；请在文件列表中删除")
	}
	if e = p.AbortMultipart(r.Context(), b.Name, "staging/"+v.ID, v.RemoteID); e != nil {
		return appkit.Unavailable("S3 分片中止未确认")
	}
	if e = p.PurgeKey(r.Context(), b.Name, "staging/"+v.ID); e != nil {
		return appkit.Unavailable("临时文件清理未确认")
	}
	if _, e = m.rt.DB.ExecContext(r.Context(), "UPDATE storage_uploads SET state='cancelled' WHERE id=?", v.ID); e != nil {
		return e
	}
	if e = m.rt.Audit(r.Context(), nil, s, "upload.aborted", v.ID, "中止分片上传并释放预留容量"); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]bool{"aborted": true})
	return nil
}
func (m *Module) getVersioning(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	status, e := p.Versioning(r.Context(), b.Name)
	if e != nil {
		return appkit.Unavailable("无法读取版本策略")
	}
	appkit.JSON(w, 200, map[string]string{"status": status})
	return nil
}
func (m *Module) setVersioning(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Status string `json:"status"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if input.Status != "Enabled" && input.Status != "Suspended" {
		return appkit.Invalid("版本策略应为 Enabled 或 Suspended")
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	// Issued POST policies remain reusable until their expiry, including after
	// completion. Enabling S3 versions while one exists would let retries grow
	// staging history without a fresh reservation.
	var outstanding int
	if e = m.rt.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM storage_uploads u WHERE u.bucket_id=? AND u.expires_at>? AND NOT EXISTS (SELECT 1 FROM storage_multipart mp WHERE mp.upload_id=u.id)`, b.ID, appkit.Now()).Scan(&outstanding); e != nil {
		return e
	}
	if outstanding > 0 {
		return appkit.Conflict("仍有未过期的普通上传许可，请等待签名到期（最长 15 分钟）后修改版本策略")
	}
	op, e := m.start(r.Context(), s, b.ID, "versioning.update", "")
	if e != nil {
		return e
	}
	ext := p.SetVersioning(r.Context(), b.Name, input.Status)
	if e = m.finish(s, op, b.ID, "versioning.update", 0, ext, nil); e != nil {
		return e
	}
	if ext != nil {
		return appkit.Unavailable("S3 未确认版本策略更新")
	}
	return m.getVersioning(w, r, s)
}
func (m *Module) listVersions(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	prefix := r.URL.Query().Get("prefix")
	if e = validKey(prefix, true); e != nil {
		return e
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 4096 {
		return appkit.Invalid("分页标记过长")
	}
	items, next, e := p.Versions(r.Context(), b.Name, "files/"+prefix, cursor, 100)
	if e != nil {
		return appkit.Unavailable("无法读取对象版本，请刷新重试")
	}
	for i := range items {
		items[i].Key = strings.TrimPrefix(items[i].Key, "files/")
	}
	appkit.JSON(w, 200, map[string]any{"items": items, "next": next})
	return nil
}
func validVersion(id string) error {
	if id == "" || len(id) > 1024 || strings.ContainsAny(id, "\x00\r\n") {
		return appkit.Invalid("请指定对象版本")
	}
	return nil
}
func (m *Module) restoreVersion(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Key          string `json:"key"`
		VersionID    string `json:"versionId"`
		ExpectedETag string `json:"expectedETag"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if e := validKey(input.Key, false); e != nil {
		return e
	}
	if e := validVersion(input.VersionID); e != nil {
		return e
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	if _, e = m.guardTarget(r.Context(), b, input.Key, input.ExpectedETag); e != nil {
		return e
	}
	v, e := p.StatVersion(r.Context(), b.Name, "files/"+input.Key, input.VersionID)
	if e != nil {
		return appkit.NotFound()
	}
	if e = m.ensureCapacity(r.Context(), s, v.Size); e != nil {
		return e
	}
	op, e := m.start(r.Context(), s, b.ID, "version.restore", input.Key)
	if e != nil {
		return e
	}
	ext := p.RestoreVersion(r.Context(), b.Name, "files/"+input.Key, input.VersionID, v.ETag)
	if e = m.finish(s, op, b.ID, "version.restore", v.Size, ext, nil); e != nil {
		return e
	}
	if ext != nil {
		return appkit.Unavailable("版本恢复未确认，请刷新版本列表")
	}
	if e = m.syncProject(r.Context(), s); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]bool{"restored": true})
	return nil
}
func (m *Module) deleteVersion(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	key, id := r.URL.Query().Get("key"), r.URL.Query().Get("versionId")
	if e := validKey(key, false); e != nil {
		return e
	}
	if e := validVersion(id); e != nil {
		return e
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	op, e := m.start(r.Context(), s, b.ID, "version.delete", key)
	if e != nil {
		return e
	}
	ext := p.DeleteVersion(r.Context(), b.Name, "files/"+key, id)
	if e = m.finish(s, op, b.ID, "version.delete", 0, ext, nil); e != nil {
		return e
	}
	if ext != nil {
		return appkit.Unavailable("版本删除未确认")
	}
	if e = m.syncProject(r.Context(), s); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]bool{"deleted": true})
	return nil
}
func (m *Module) getLifecycle(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	rules, e := p.GetLifecycle(r.Context(), b.Name)
	if e != nil {
		return appkit.Unavailable("无法读取生命周期")
	}
	appkit.JSON(w, 200, map[string]any{"items": rules})
	return nil
}
func (m *Module) setLifecycle(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Items []LifecycleRule `json:"items"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if len(input.Items) > 20 {
		return appkit.Invalid("最多配置 20 条规则")
	}
	seen := map[string]bool{}
	for _, v := range input.Items {
		if v.ID == "" || len(v.ID) > 100 || strings.ContainsAny(v.ID, "\x00\r\n") || seen[v.ID] {
			return appkit.Invalid("规则 ID 无效或重复")
		}
		seen[v.ID] = true
		if e := validKey(v.Prefix, true); e != nil {
			return e
		}
		if v.ExpirationDays < 0 || v.NoncurrentDays < 0 || v.ExpirationDays > 36500 || v.NoncurrentDays > 36500 || (v.ExpirationDays == 0 && v.NoncurrentDays == 0) {
			return appkit.Invalid("清理天数范围为 1–36500，至少设置一种清理策略")
		}
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	p, e := m.advanced()
	if e != nil {
		return e
	}
	op, e := m.start(r.Context(), s, b.ID, "lifecycle.update", "")
	if e != nil {
		return e
	}
	ext := p.SetLifecycle(r.Context(), b.Name, input.Items)
	if e = m.finish(s, op, b.ID, "lifecycle.update", 0, ext, nil); e != nil {
		return e
	}
	if ext != nil {
		return appkit.Unavailable("S3 未确认生命周期规则；请检查服务支持情况")
	}
	return m.getLifecycle(w, r, s)
}
func (m *Module) ensureCapacity(ctx context.Context, s appkit.Scope, extra int64) error {
	if e := m.syncProject(ctx, s); e != nil {
		return e
	}
	q, e := m.ledger.quota(ctx, m.rt.DB, s)
	if e != nil {
		return e
	}
	var used, reserved int64
	if e = m.rt.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(used_bytes),0) FROM storage_buckets WHERE tenant_id=? AND project_id=? AND status<>'deleted'", s.TenantID, s.ProjectID).Scan(&used); e != nil {
		return e
	}
	if e = m.rt.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(expected_bytes),0) FROM storage_uploads WHERE tenant_id=? AND project_id=? AND state='pending' AND expires_at>?", s.TenantID, s.ProjectID, appkit.Now()).Scan(&reserved); e != nil {
		return e
	}
	if extra > q.StorageBytes-used-reserved {
		return appkit.Conflict("项目存储配额不足（包含历史版本和上传预留）")
	}
	return nil
}
func (m *Module) transferObject(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Move   bool   `json:"move"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if e := validKey(input.Source, false); e != nil {
		return e
	}
	if e := validKey(input.Target, false); e != nil {
		return e
	}
	if input.Source == input.Target || strings.HasSuffix(input.Source, "/") || strings.HasSuffix(input.Target, "/") {
		return appkit.Invalid("请指定两个不同的文件路径")
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if _, e = m.guardTarget(r.Context(), b, input.Target, ""); e != nil {
		return e
	}
	source, e := m.plane.Stat(r.Context(), b.Name, "files/"+input.Source)
	if e != nil {
		return appkit.NotFound()
	}
	if e = m.ensureCapacity(r.Context(), s, source.Size); e != nil {
		return e
	}
	action := "object.copy"
	if input.Move {
		action = "object.move"
	}
	op, e := m.start(r.Context(), s, b.ID, action, input.Source)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 120*time.Second)
	defer cancel()
	ext := m.plane.Copy(ctx, b.Name, "files/"+input.Source, "files/"+input.Target, source.ETag)
	if ext == nil && input.Move {
		ext = m.plane.Delete(ctx, b.Name, "files/"+input.Source)
	}
	if e = m.finish(s, op, b.ID, action, source.Size, ext, nil); e != nil {
		return e
	}
	if ext != nil {
		return appkit.Unavailable("操作结果未确认；请检查源文件和目标文件，避免重复执行")
	}
	if e = m.syncProject(r.Context(), s); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"key": input.Target, "moved": input.Move})
	return nil
}
