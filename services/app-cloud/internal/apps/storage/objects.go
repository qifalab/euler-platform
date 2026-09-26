package storage

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

func (m *Module) activeBucket(ctx context.Context, s appkit.Scope, id string) (Bucket, error) {
	b, e := m.bucket(ctx, s, id)
	if e != nil {
		return b, e
	}
	if b.Status != "active" {
		return b, appkit.Conflict("存储桶尚未就绪")
	}
	return b, m.ready()
}
func (m *Module) objects(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	prefix, after := r.URL.Query().Get("prefix"), r.URL.Query().Get("after")
	if e = validKey(prefix, true); e != nil {
		return e
	}
	if e = validKey(after, true); e != nil {
		return e
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	physicalAfter := ""
	if after != "" {
		physicalAfter = "files/" + after
	}
	items, more, e := m.plane.List(r.Context(), b.Name, "files/"+prefix, r.URL.Query().Get("recursive") == "true", physicalAfter, limit)
	if e != nil {
		return appkit.Unavailable("无法读取对象列表")
	}
	out := []Object{}
	next := ""
	for _, v := range items {
		v.Key = strings.TrimPrefix(v.Key, "files/")
		if v.Key == prefix && v.Folder {
			continue
		}
		out = append(out, v)
		next = v.Key
	}
	appkit.JSON(w, 200, map[string]any{"items": out, "hasMore": more, "next": next, "prefix": prefix})
	return nil
}
func (m *Module) objectInfo(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	key := r.URL.Query().Get("key")
	if e = validKey(key, false); e != nil {
		return e
	}
	v, e := m.plane.Stat(r.Context(), b.Name, "files/"+key)
	if absent(e) {
		return appkit.NotFound()
	}
	if e != nil {
		return appkit.Unavailable("无法读取对象信息")
	}
	v.Key = key
	appkit.JSON(w, 200, v)
	return nil
}
func (m *Module) folder(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Key string `json:"key"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	key := strings.TrimSuffix(input.Key, "/") + "/"
	if e := validKey(key, false); e != nil {
		return e
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	op, e := m.start(r.Context(), s, b.ID, "folder.create", key)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	externalErr := m.plane.Put(ctx, b.Name, "files/"+key, strings.NewReader(""), 0, "application/x-directory")
	if e = m.finish(s, op, b.ID, "folder.create", 0, externalErr, nil); e != nil {
		return e
	}
	if externalErr != nil {
		return appkit.Unavailable("创建目录未确认")
	}
	appkit.JSON(w, 201, map[string]string{"key": key})
	return nil
}
func (m *Module) reserve(ctx context.Context, s appkit.Scope, b Bucket, key string, size int64, contentType string) (string, error) {
	if size < 0 || size > envInt("EULER_STORAGE_MAX_UPLOAD_BYTES", 5*1024*1024*1024) {
		return "", appkit.Invalid("文件大小超出允许范围")
	}
	if e := m.syncProject(ctx, s); e != nil {
		return "", e
	}
	id := appkit.NewID("upload_")
	e := m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		q, e := m.ledger.quota(ctx, tx, s)
		if e != nil {
			return e
		}
		var used, reserved int64
		var pending int
		if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM storage_uploads WHERE tenant_id=? AND project_id=? AND state='pending' AND expires_at>?", s.TenantID, s.ProjectID, appkit.Now()).Scan(&pending); e != nil {
			return e
		}
		if pending >= 100 {
			return appkit.Conflict("当前项目有过多未完成上传，请完成或等待上传许可过期")
		}
		if e = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(used_bytes),0) FROM storage_buckets WHERE tenant_id=? AND project_id=? AND status<>'deleted'", s.TenantID, s.ProjectID).Scan(&used); e != nil {
			return e
		}
		if e = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(expected_bytes),0) FROM storage_uploads WHERE tenant_id=? AND project_id=? AND state='pending' AND expires_at>?", s.TenantID, s.ProjectID, appkit.Now()).Scan(&reserved); e != nil {
			return e
		}
		if size > q.StorageBytes-used-reserved {
			return appkit.Conflict("项目存储配额不足（包含尚未完成上传的预留）")
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO storage_uploads VALUES(?,?,?,?,?,?,?,?,?,?,?)", id, s.TenantID, s.ProjectID, b.ID, s.ActorID, key, size, contentType, "pending", time.Now().UTC().Add(15*time.Minute).Format(time.RFC3339Nano), appkit.Now())
		if e != nil {
			return e
		}
		return m.rt.Audit(ctx, tx, s, "upload.reserved", b.ID, "预留项目上传容量")
	})
	return id, e
}
func (m *Module) presignUpload(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Key         string `json:"key"`
		Size        int64  `json:"size"`
		ContentType string `json:"contentType"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if e := validKey(input.Key, false); e != nil {
		return e
	}
	if strings.HasSuffix(input.Key, "/") || len(input.ContentType) > 255 || strings.ContainsAny(input.ContentType, "\r\n") {
		return appkit.Invalid("文件名或内容类型不符合要求")
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
	id, e := m.reserve(r.Context(), s, b, input.Key, input.Size, input.ContentType)
	if e != nil {
		return e
	}
	signature, e := m.plane.UploadSignature(r.Context(), b.Name, "staging/"+id, input.Size, input.ContentType, 15*time.Minute)
	if e != nil {
		_, _ = m.rt.DB.ExecContext(context.Background(), "UPDATE storage_uploads SET state='cancelled' WHERE id=?", id)
		return appkit.Unavailable("无法签发上传许可")
	}
	appkit.JSON(w, 201, map[string]any{"uploadId": id, "signature": signature, "key": input.Key})
	return nil
}
func (m *Module) publish(ctx context.Context, s appkit.Scope, b Bucket, id string) (Object, error) {
	var key, actor, contentType, state, expiry string
	var expected int64
	e := m.rt.DB.QueryRowContext(ctx, "SELECT object_key,actor_id,expected_bytes,content_type,state,expires_at FROM storage_uploads WHERE id=? AND tenant_id=? AND project_id=? AND bucket_id=?", id, s.TenantID, s.ProjectID, b.ID).Scan(&key, &actor, &expected, &contentType, &state, &expiry)
	if e != nil {
		return Object{}, e
	}
	if actor != s.ActorID {
		return Object{}, appkit.NotFound()
	}
	if state == "completed" {
		v, e := m.plane.Stat(ctx, b.Name, "files/"+key)
		v.Key = key
		return v, e
	}
	if state != "pending" || expiry <= appkit.Now() {
		return Object{}, appkit.Conflict("上传许可已失效")
	}
	source, e := m.plane.Stat(ctx, b.Name, "staging/"+id)
	if e != nil {
		return Object{}, appkit.Conflict("临时文件尚未上传成功")
	}
	if source.Size != expected {
		_ = m.plane.Delete(ctx, b.Name, "staging/"+id)
		_, _ = m.rt.DB.ExecContext(ctx, "UPDATE storage_uploads SET state='rejected' WHERE id=?", id)
		return Object{}, appkit.Conflict("实际文件大小与上传许可不一致，临时文件已拒绝")
	}
	if e = m.syncProject(ctx, s); e != nil {
		return Object{}, e
	}
	oldSize := int64(0)
	oldCount := int64(0)
	old, e := m.plane.Stat(ctx, b.Name, "files/"+key)
	if e == nil {
		oldSize = old.Size
		oldCount = 1
	} else if !absent(e) {
		return Object{}, appkit.Unavailable("无法确认目标对象")
	}
	q, e := m.ledger.quota(ctx, m.rt.DB, s)
	if e != nil {
		return Object{}, e
	}
	var used int64
	if e = m.rt.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(used_bytes),0) FROM storage_buckets WHERE tenant_id=? AND project_id=? AND status<>'deleted'", s.TenantID, s.ProjectID).Scan(&used); e != nil {
		return Object{}, e
	}
	if expected-oldSize > q.StorageBytes-used {
		return Object{}, appkit.Conflict("项目容量不足，文件尚未发布")
	}
	op, e := m.start(ctx, s, b.ID, "object.upload", key)
	if e != nil {
		return Object{}, e
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	externalErr := m.plane.Copy(work, b.Name, "staging/"+id, "files/"+key, source.ETag)
	if externalErr == nil {
		_ = m.plane.Delete(work, b.Name, "staging/"+id)
	}
	e = m.finish(s, op, b.ID, "object.upload", expected-oldSize, externalErr, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE storage_uploads SET state='completed' WHERE id=?", id)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE storage_buckets SET used_bytes=used_bytes+?,object_count=object_count+?,updated_at=? WHERE id=?", expected-oldSize, 1-oldCount, appkit.Now(), b.ID)
		return e
	})
	if e != nil {
		return Object{}, e
	}
	if externalErr != nil {
		return Object{}, appkit.Unavailable("对象发布结果未确认，已保留操作记录")
	}
	source.Key = key
	source.ContentType = contentType
	return source, nil
}
func (m *Module) confirmUpload(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		UploadID string `json:"uploadId"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	v, e := m.publish(r.Context(), s, b, input.UploadID)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, v)
	return nil
}
func (m *Module) presignDownload(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Key     string `json:"key"`
		Expires int    `json:"expires"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if e := validKey(input.Key, false); e != nil {
		return e
	}
	if input.Expires == 0 {
		input.Expires = 3600
	}
	if input.Expires < 60 || input.Expires > 604800 {
		return appkit.Invalid("签名有效期为 60–604800 秒")
	}
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if _, e = m.plane.Stat(r.Context(), b.Name, "files/"+input.Key); absent(e) {
		return appkit.NotFound()
	} else if e != nil {
		return appkit.Unavailable("无法确认文件存在")
	}
	signature, e := m.plane.DownloadSignature(r.Context(), b.Name, "files/"+input.Key, time.Duration(input.Expires)*time.Second)
	if e != nil {
		return appkit.Unavailable("无法签发下载链接")
	}
	if e = m.rt.Audit(r.Context(), nil, s, "download.signed", b.ID, "签发项目对象下载链接"); e != nil {
		return e
	}
	appkit.JSON(w, 200, signature)
	return nil
}
func (m *Module) objectPublicURL(b Bucket, key string) string {
	parts := strings.Split(key, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.TrimRight(m.rt.PublicURL, "/") + "/public/storage/files/" + url.PathEscape(b.ID) + "/" + strings.Join(parts, "/")
}
func (m *Module) publicURL(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	key := r.URL.Query().Get("key")
	if e := validKey(key, false); e != nil {
		return e
	}
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if b.Access == "private" {
		return appkit.Conflict("私有桶没有公开链接，请使用签名下载")
	}
	if _, e = m.plane.Stat(r.Context(), b.Name, "files/"+key); e != nil {
		return appkit.NotFound()
	}
	appkit.JSON(w, 200, map[string]string{"url": m.objectPublicURL(b, key)})
	return nil
}
func (m *Module) removeObject(ctx context.Context, s appkit.Scope, b Bucket, key string) error {
	old, e := m.plane.Stat(ctx, b.Name, "files/"+key)
	if absent(e) {
		return appkit.NotFound()
	}
	if e != nil {
		return appkit.Unavailable("无法确认要删除的文件")
	}
	op, e := m.start(ctx, s, b.ID, "object.delete", key)
	if e != nil {
		return e
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	externalErr := m.plane.Delete(work, b.Name, "files/"+key)
	count := 1
	if old.Folder {
		count = 0
	}
	e = m.finish(s, op, b.ID, "object.delete", -old.Size, externalErr, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE storage_buckets SET used_bytes=MAX(0,used_bytes-?),object_count=MAX(0,object_count-?),updated_at=? WHERE id=?", old.Size, count, appkit.Now(), b.ID)
		return e
	})
	if e != nil {
		return e
	}
	if externalErr != nil {
		return appkit.Unavailable("删除对象结果未确认")
	}
	return nil
}
func (m *Module) deleteObject(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	key := r.URL.Query().Get("key")
	if e := validKey(key, false); e != nil {
		return e
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if e = m.removeObject(r.Context(), s, b, key); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]bool{"deleted": true})
	return nil
}
func (m *Module) deleteBatch(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Keys []string `json:"keys"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if len(input.Keys) < 1 || len(input.Keys) > 1000 {
		return appkit.Invalid("每批应包含 1–1000 个对象")
	}
	for _, key := range input.Keys {
		if e := validKey(key, false); e != nil {
			return e
		}
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.activeBucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	deleted := []string{}
	failed := []string{}
	for _, key := range input.Keys {
		if e = m.removeObject(r.Context(), s, b, key); e != nil {
			failed = append(failed, key)
		} else {
			deleted = append(deleted, key)
		}
	}
	appkit.JSON(w, 200, map[string]any{"deleted": deleted, "failed": failed})
	return nil
}
