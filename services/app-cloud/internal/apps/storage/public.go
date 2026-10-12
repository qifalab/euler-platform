package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type APIKey struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	AccessKey  string   `json:"accessKey"`
	SecretKey  string   `json:"secretKey,omitempty"`
	Scopes     []string `json:"scopes"`
	Active     bool     `json:"active"`
	ExpiresAt  string   `json:"expiresAt"`
	CreatedAt  string   `json:"createdAt"`
	LastUsedAt string   `json:"lastUsedAt"`
}

func (m *Module) keyHash(access, secret string) []byte {
	h := hmac.New(sha256.New, m.rt.DeriveKey("storage-machine-keys-v1"))
	_, _ = h.Write([]byte(access + ":" + secret))
	return h.Sum(nil)
}
func (m *Module) keys(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT id,name,access_key,scopes,active,expires_at,created_at,last_used_at FROM storage_keys WHERE tenant_id=? AND project_id=? ORDER BY created_at DESC", s.TenantID, s.ProjectID)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var v APIKey
		var raw string
		if e = rows.Scan(&v.ID, &v.Name, &v.AccessKey, &raw, &v.Active, &v.ExpiresAt, &v.CreatedAt, &v.LastUsedAt); e != nil {
			return e
		}
		if e = json.Unmarshal([]byte(raw), &v.Scopes); e != nil {
			return e
		}
		out = append(out, v)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": out, "endpoint": strings.TrimRight(m.rt.PublicURL, "/") + "/public/storage/s3"})
	return nil
}
func (m *Module) createKey(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresAt string   `json:"expiresAt"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	name, e := appkit.Name(input.Name, 64)
	if e != nil {
		return e
	}
	if len(input.Scopes) < 1 || len(input.Scopes) > 3 {
		return appkit.Invalid("请选择密钥权限")
	}
	seen := map[string]bool{}
	for _, v := range input.Scopes {
		if seen[v] || (v != "read" && v != "write" && v != "delete") {
			return appkit.Invalid("密钥权限不符合要求")
		}
		seen[v] = true
	}
	if input.ExpiresAt != "" {
		expiry, e := time.Parse(time.RFC3339, input.ExpiresAt)
		if e != nil || !expiry.After(time.Now()) {
			return appkit.Invalid("密钥有效期应为未来时间")
		}
		input.ExpiresAt = expiry.UTC().Format(time.RFC3339Nano)
	}
	v := APIKey{ID: appkit.NewID("key_"), Name: name, AccessKey: random("EUAK"), SecretKey: random(""), Scopes: input.Scopes, Active: true, ExpiresAt: input.ExpiresAt, CreatedAt: appkit.Now()}
	raw, _ := json.Marshal(v.Scopes)
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var count int
		if e := tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM storage_keys WHERE tenant_id=? AND project_id=?", s.TenantID, s.ProjectID).Scan(&count); e != nil {
			return e
		}
		if count >= 10 {
			return appkit.Conflict("每项目最多保留 10 个密钥，请删除旧密钥后创建")
		}
		_, e := tx.ExecContext(r.Context(), "INSERT INTO storage_keys VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", v.ID, s.TenantID, s.ProjectID, s.InstallationID, v.Name, v.AccessKey, m.keyHash(v.AccessKey, v.SecretKey), string(raw), true, v.ExpiresAt, v.CreatedAt, "")
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "key.created", v.ID, "创建项目程序化访问密钥")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, v)
	return nil
}
func (m *Module) toggleKey(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Active bool `json:"active"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	return m.mutateKey(w, r, s, false, input.Active)
}
func (m *Module) deleteKey(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	return m.mutateKey(w, r, s, true, false)
}
func (m *Module) mutateKey(w http.ResponseWriter, r *http.Request, s appkit.Scope, remove, active bool) error {
	e := m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		var res sql.Result
		var e error
		action := "key.status_changed"
		if remove {
			res, e = tx.ExecContext(r.Context(), "DELETE FROM storage_keys WHERE id=? AND tenant_id=? AND project_id=?", r.PathValue("id"), s.TenantID, s.ProjectID)
			action = "key.deleted"
		} else {
			res, e = tx.ExecContext(r.Context(), "UPDATE storage_keys SET active=? WHERE id=? AND tenant_id=? AND project_id=?", active, r.PathValue("id"), s.TenantID, s.ProjectID)
		}
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return appkit.NotFound()
		}
		return m.rt.Audit(r.Context(), tx, s, action, r.PathValue("id"), "更新项目程序化访问密钥")
	})
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]bool{"active": active, "deleted": remove})
	return nil
}
func (m *Module) enabled(ctx context.Context, s appkit.Scope) error {
	if m.rt.ApplicationEnabled == nil {
		return appkit.Unavailable("公开接口的安装状态检查尚未配置")
	}
	ok, e := m.rt.ApplicationEnabled(ctx, s.TenantID, s.ProjectID, "storage")
	if e != nil {
		return appkit.Unavailable("无法确认应用启用状态")
	}
	if !ok {
		return appkit.NotFound()
	}
	return nil
}
func (m *Module) authenticate(r *http.Request, permission string) (appkit.Scope, error) {
	var s appkit.Scope
	access, secret := r.Header.Get("X-Access-Key"), r.Header.Get("X-Secret-Key")
	if len(r.Header.Values("X-Access-Key")) > 1 || len(r.Header.Values("X-Secret-Key")) > 1 || len(r.Header.Values("Authorization")) > 1 {
		return s, appkit.Forbidden("密钥格式不符合要求")
	}
	if bearer := r.Header.Get("Authorization"); bearer != "" {
		if access != "" || secret != "" || !strings.HasPrefix(bearer, "Bearer ") {
			return s, appkit.Forbidden("密钥格式不符合要求")
		}
		access, secret, _ = strings.Cut(strings.TrimPrefix(bearer, "Bearer "), ".")
	}
	if access == "" || secret == "" || len(access) > 150 || len(secret) > 150 {
		return s, &appkit.Error{Status: 401, Code: "invalid_key", Message: "请提供项目 API 密钥"}
	}
	var id, raw, expiry string
	var hash []byte
	var active bool
	e := m.rt.DB.QueryRowContext(r.Context(), "SELECT id,tenant_id,project_id,installation_id,secret_hash,scopes,active,expires_at FROM storage_keys WHERE access_key=?", access).Scan(&id, &s.TenantID, &s.ProjectID, &s.InstallationID, &hash, &raw, &active, &expiry)
	if e != nil || !active || !hmac.Equal(hash, m.keyHash(access, secret)) {
		return s, &appkit.Error{Status: 401, Code: "invalid_key", Message: "项目 API 密钥无效"}
	}
	if expiry != "" {
		t, e := time.Parse(time.RFC3339Nano, expiry)
		if e != nil || !t.After(time.Now()) {
			return s, &appkit.Error{Status: 401, Code: "invalid_key", Message: "项目 API 密钥已过期"}
		}
	}
	var scopes []string
	if e = json.Unmarshal([]byte(raw), &scopes); e != nil {
		return s, e
	}
	allowed := false
	for _, v := range scopes {
		if v == permission {
			allowed = true
		}
	}
	if !allowed {
		return s, appkit.Forbidden("密钥没有此数据操作权限")
	}
	s.ActorID = "key:" + id
	s.ApplicationID = "storage"
	s.Permissions = scopes
	if e = m.enabled(r.Context(), s); e != nil {
		return s, e
	}
	_, e = m.rt.DB.ExecContext(r.Context(), "UPDATE storage_keys SET last_used_at=? WHERE id=?", appkit.Now(), id)
	return s, e
}

type publicEndpoint func(http.ResponseWriter, *http.Request) error

func publicHandle(mux *http.ServeMux, path string, handler publicEndpoint) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if e := handler(w, r); e != nil {
			appkit.RespondError(w, e)
		}
	})
}
func (m *Module) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	publicHandle(mux, "GET /files/{bucket}/{key...}", m.servePublic)
	publicHandle(mux, "GET /info/{bucket}/{key...}", m.publicInfo)
	publicHandle(mux, "PUT /files/{bucket}/{key...}", m.putPublic)
	publicHandle(mux, "GET /s3/buckets", func(w http.ResponseWriter, r *http.Request) error {
		s, e := m.authenticate(r, "read")
		if e != nil {
			return e
		}
		return m.listBuckets(w, r, s)
	})
	publicHandle(mux, "GET /s3/user/quota", func(w http.ResponseWriter, r *http.Request) error {
		s, e := m.authenticate(r, "read")
		if e != nil {
			return e
		}
		return m.overview(w, r, s)
	})
	publicHandle(mux, "GET /s3/buckets/{bucket}/objects", func(w http.ResponseWriter, r *http.Request) error {
		s, b, e := m.machineBucket(r, "read")
		if e != nil {
			return e
		}
		r.SetPathValue("id", b.ID)
		return m.objects(w, r, s)
	})
	publicHandle(mux, "PUT /s3/buckets/{bucket}/objects/{key...}", func(w http.ResponseWriter, r *http.Request) error {
		s, b, e := m.machineBucket(r, "write")
		if e != nil {
			return e
		}
		return m.directUpload(w, r, s, b)
	})
	publicHandle(mux, "GET /s3/buckets/{bucket}/objects/{key...}", m.machineDownload)
	publicHandle(mux, "HEAD /s3/buckets/{bucket}/objects/{key...}", m.machineDownload)
	publicHandle(mux, "DELETE /s3/buckets/{bucket}/objects/{key...}", func(w http.ResponseWriter, r *http.Request) error {
		s, b, e := m.machineBucket(r, "delete")
		if e != nil {
			return e
		}
		key := r.PathValue("key")
		if e = validKey(key, false); e != nil {
			return e
		}
		unlock := m.lock(s)
		defer unlock()
		if e = m.removeObject(r.Context(), s, b, key); e != nil {
			return e
		}
		appkit.JSON(w, 200, map[string]bool{"deleted": true})
		return nil
	})
	return mux
}
func (m *Module) machineBucket(r *http.Request, permission string) (appkit.Scope, Bucket, error) {
	s, e := m.authenticate(r, permission)
	if e != nil {
		return s, Bucket{}, e
	}
	b, e := scanBucket(m.rt.DB.QueryRowContext(r.Context(), "SELECT "+bucketCols+" FROM storage_buckets WHERE (id=? OR name=?) AND tenant_id=? AND project_id=? AND status='active'", r.PathValue("bucket"), r.PathValue("bucket"), s.TenantID, s.ProjectID))
	if e != nil {
		return s, b, e
	}
	return s, b, m.ready()
}
func (m *Module) anonymousBucket(r *http.Request, write bool) (appkit.Scope, Bucket, error) {
	b, e := scanBucket(m.rt.DB.QueryRowContext(r.Context(), "SELECT "+bucketCols+" FROM storage_buckets WHERE id=? AND status='active'", r.PathValue("bucket")))
	s := appkit.Scope{ActorID: "public", ApplicationID: "storage", TenantID: b.TenantID, ProjectID: b.ProjectID, InstallationID: b.InstallationID}
	if e != nil {
		return s, b, e
	}
	if b.Access == "private" || (write && b.Access != "public-read-write") {
		return s, b, appkit.Forbidden("此存储桶未开放该公开访问能力")
	}
	if e = m.enabled(r.Context(), s); e != nil {
		return s, b, e
	}
	return s, b, m.ready()
}
func (m *Module) servePublic(w http.ResponseWriter, r *http.Request) error {
	_, b, e := m.anonymousBucket(r, false)
	if e != nil {
		return e
	}
	key := r.PathValue("key")
	if e = validKey(key, false); e != nil {
		return e
	}
	if _, e = m.plane.Stat(r.Context(), b.Name, "files/"+key); e != nil {
		return appkit.NotFound()
	}
	signature, e := m.plane.DownloadSignature(r.Context(), b.Name, "files/"+key, 5*time.Minute)
	if e != nil {
		return appkit.Unavailable("无法生成公开文件入口")
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, signature.URL, http.StatusFound)
	return nil
}
func (m *Module) publicInfo(w http.ResponseWriter, r *http.Request) error {
	_, b, e := m.anonymousBucket(r, false)
	if e != nil {
		return e
	}
	key := r.PathValue("key")
	if e = validKey(key, false); e != nil {
		return e
	}
	v, e := m.plane.Stat(r.Context(), b.Name, "files/"+key)
	if e != nil {
		return appkit.NotFound()
	}
	v.Key = key
	appkit.JSON(w, 200, map[string]any{"object": v, "url": m.objectPublicURL(b, key)})
	return nil
}
func (m *Module) putPublic(w http.ResponseWriter, r *http.Request) error {
	s, b, e := m.anonymousBucket(r, true)
	if e != nil {
		return e
	}
	return m.directUpload(w, r, s, b)
}
func (m *Module) directUpload(w http.ResponseWriter, r *http.Request, s appkit.Scope, b Bucket) error {
	key := r.PathValue("key")
	if e := validKey(key, false); e != nil {
		return e
	}
	if strings.HasSuffix(key, "/") {
		return appkit.Invalid("上传文件名不能是目录")
	}
	limit := envInt("EULER_STORAGE_MAX_PROXY_BYTES", 100*1024*1024)
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
	var reader io.Reader = r.Body
	contentType := r.Header.Get("Content-Type")
	if media, _, _ := mime.ParseMediaType(contentType); media == "multipart/form-data" {
		mr, e := r.MultipartReader()
		if e != nil {
			return appkit.Invalid("上传表单不正确")
		}
		found := false
		for {
			part, e := mr.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				return appkit.Invalid("上传表单过大或不正确")
			}
			if part.FormName() == "file" && part.FileName() != "" {
				reader = part
				contentType = part.Header.Get("Content-Type")
				found = true
				break
			}
			_ = part.Close()
		}
		if !found {
			return appkit.Invalid("上传表单缺少 file 字段")
		}
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if len(contentType) > 255 || strings.ContainsAny(contentType, "\r\n") {
		return appkit.Invalid("内容类型不正确")
	}
	tmp, e := os.CreateTemp(m.rt.DataDir, "storage-upload-*")
	if e != nil {
		return e
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, e := io.Copy(tmp, io.LimitReader(reader, limit+1))
	if e != nil || size > limit {
		return appkit.Invalid("代理上传超过大小限制，请使用签名直传")
	}
	if _, e = tmp.Seek(0, 0); e != nil {
		return e
	}
	unlock := m.lock(s)
	defer unlock()
	current, e := m.activeBucket(r.Context(), s, b.ID)
	if e != nil {
		return e
	}
	if s.ActorID == "public" && current.Access != "public-read-write" {
		return appkit.Forbidden("公开写入已关闭")
	}
	if e = m.enabled(r.Context(), s); e != nil {
		return e
	}
	id, e := m.reserve(r.Context(), s, b, key, size, contentType)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 120*time.Second)
	defer cancel()
	if e = m.plane.Put(ctx, b.Name, "staging/"+id, tmp, size, contentType); e != nil {
		return appkit.Unavailable("上传临时对象失败")
	}
	v, e := m.publish(ctx, s, b, id)
	if e != nil {
		return e
	}
	appkit.JSON(w, 201, v)
	return nil
}
func (m *Module) machineDownload(w http.ResponseWriter, r *http.Request) error {
	_, b, e := m.machineBucket(r, "read")
	if e != nil {
		return e
	}
	key := r.PathValue("key")
	if e = validKey(key, false); e != nil {
		return e
	}
	info, e := m.plane.Stat(r.Context(), b.Name, "files/"+key)
	if absent(e) {
		return appkit.NotFound()
	}
	if e != nil {
		return appkit.Unavailable("无法读取文件")
	}
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("ETag", `"`+info.ETag+`"`)
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(key[strings.LastIndex(key, "/")+1:]))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(200)
		return nil
	}
	reader, _, e := m.plane.Get(r.Context(), b.Name, "files/"+key)
	if e != nil {
		return appkit.Unavailable("无法读取文件流")
	}
	defer reader.Close()
	w.WriteHeader(200)
	_, _ = io.Copy(w, reader)
	return nil
}
