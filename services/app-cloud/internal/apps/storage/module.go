// Package storage owns Euler's project storage control plane. Product parity is
// based on ctipscn/ecloud-storage (Apache-2.0); no legacy API or JWT is trusted.
package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type Module struct {
	rt      *appkit.Runtime
	plane   DataPlane
	initErr error
	ledger  ledger
	locks   [64]sync.Mutex
	once    sync.Once
	cancel  context.CancelFunc
	done    chan struct{}
}
type Bucket struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	DisplayName    string   `json:"displayName"`
	Description    string   `json:"description"`
	Access         string   `json:"access"`
	Status         string   `json:"status"`
	UsedBytes      int64    `json:"usedBytes"`
	ObjectCount    int64    `json:"objectCount"`
	Origins        []string `json:"origins"`
	CreatedAt      string   `json:"createdAt"`
	UpdatedAt      string   `json:"updatedAt"`
	TenantID       string   `json:"-"`
	ProjectID      string   `json:"-"`
	InstallationID string   `json:"-"`
}

func envInt(key string, fallback int64) int64 {
	n, e := strconv.ParseInt(os.Getenv(key), 10, 64)
	if e != nil || n < 0 {
		return fallback
	}
	return n
}
func New(rt *appkit.Runtime) *Module {
	p, e := newS3()
	m := &Module{rt: rt, plane: p, initErr: e}
	m.ledger = ledger{rt: rt, prefix: "storage", base: Quota{StorageBytes: envInt("EULER_STORAGE_DEFAULT_BYTES", 1024*1024*1024), Buckets: envInt("EULER_STORAGE_DEFAULT_BUCKETS", 2)}}
	return m
}
func (m *Module) ID() string { return "storage" }
func (m *Module) Migrate(ctx context.Context) error {
	if m.initErr != nil {
		return m.initErr
	}
	_, e := m.rt.DB.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS storage_buckets(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,installation_id TEXT NOT NULL,name TEXT NOT NULL UNIQUE,display_name TEXT NOT NULL,description TEXT NOT NULL,access TEXT NOT NULL,status TEXT NOT NULL,used_bytes INTEGER NOT NULL DEFAULT 0,object_count INTEGER NOT NULL DEFAULT 0,origins TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS storage_buckets_scope ON storage_buckets(tenant_id,project_id,status);
CREATE TABLE IF NOT EXISTS storage_uploads(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,bucket_id TEXT NOT NULL,actor_id TEXT NOT NULL,object_key TEXT NOT NULL,expected_bytes INTEGER NOT NULL,content_type TEXT NOT NULL,state TEXT NOT NULL,expires_at TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS storage_keys(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,installation_id TEXT NOT NULL,name TEXT NOT NULL,access_key TEXT NOT NULL UNIQUE,secret_hash BLOB NOT NULL,scopes TEXT NOT NULL,active INTEGER NOT NULL,expires_at TEXT NOT NULL,created_at TEXT NOT NULL,last_used_at TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS storage_logs(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,bucket_id TEXT NOT NULL,actor_id TEXT NOT NULL,action TEXT NOT NULL,object_key TEXT NOT NULL,size_change INTEGER NOT NULL,state TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS storage_logs_scope ON storage_logs(tenant_id,project_id,created_at);`)
	if e != nil {
		return e
	}
	if e = m.ledger.migrate(ctx); e != nil {
		return e
	}
	if os.Getenv("EULER_STORAGE_DISABLE_SCHEDULER") != "true" {
		m.once.Do(func() {
			ctx, m.cancel = context.WithCancel(ctx)
			m.done = make(chan struct{})
			go func() {
				defer close(m.done)
				ticker := time.NewTicker(5 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						m.cleanup(ctx)
					}
				}
			}()
		})
	}
	return nil
}
func (m *Module) Handler() http.Handler {
	mux := http.NewServeMux()
	appkit.Handle(mux, "GET /", "read", m.overview)
	appkit.Handle(mux, "GET /quota", "read", m.overview)
	appkit.Handle(mux, "GET /buckets", "read", m.listBuckets)
	appkit.Handle(mux, "POST /buckets", "write", m.createBucket)
	appkit.Handle(mux, "GET /buckets/{id}", "read", m.bucketDetail)
	appkit.Handle(mux, "PUT /buckets/{id}", "manage", m.updateBucket)
	appkit.Handle(mux, "DELETE /buckets/{id}", "manage", m.deleteBucket)
	appkit.Handle(mux, "POST /buckets/{id}/sync", "write", m.syncBucket)
	appkit.Handle(mux, "POST /buckets/{id}/cors", "manage", m.cors)
	appkit.Handle(mux, "GET /buckets/{id}/objects", "read", m.objects)
	appkit.Handle(mux, "GET /buckets/{id}/objects/info", "read", m.objectInfo)
	appkit.Handle(mux, "POST /buckets/{id}/objects/folder", "write", m.folder)
	appkit.Handle(mux, "POST /buckets/{id}/objects/presigned-upload", "write", m.presignUpload)
	appkit.Handle(mux, "POST /buckets/{id}/objects/confirm-upload", "write", m.confirmUpload)
	appkit.Handle(mux, "POST /buckets/{id}/objects/presigned-download", "read", m.presignDownload)
	appkit.Handle(mux, "GET /buckets/{id}/objects/public-url", "read", m.publicURL)
	appkit.Handle(mux, "DELETE /buckets/{id}/objects", "write", m.deleteObject)
	appkit.Handle(mux, "POST /buckets/{id}/objects/delete-batch", "write", m.deleteBatch)
	appkit.Handle(mux, "GET /stats/logs", "read", m.logs)
	appkit.Handle(mux, "GET /keys", "manage", m.keys)
	appkit.Handle(mux, "POST /keys", "secrets", m.createKey)
	appkit.Handle(mux, "PUT /keys/{id}", "manage", m.toggleKey)
	appkit.Handle(mux, "DELETE /keys/{id}", "manage", m.deleteKey)
	appkit.Handle(mux, "GET /admin/accounts", "admin", m.overview)
	m.ledger.register(mux)
	return mux
}
func (m *Module) lock(s appkit.Scope) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s.TenantID + "/" + s.ProjectID))
	v := &m.locks[h.Sum32()%uint32(len(m.locks))]
	v.Lock()
	return v.Unlock
}
func (m *Module) ready() error {
	if m.plane == nil {
		return appkit.Unavailable("尚未配置 S3/MinIO 数据面")
	}
	return nil
}
func (m *Module) corsMode() string {
	if p, ok := m.plane.(interface{ CORSMode() string }); ok {
		return p.CORSMode()
	}
	return "bucket"
}
func random(prefix string) string {
	b := make([]byte, 18)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return prefix + hex.EncodeToString(b)
}
func validKey(key string, empty bool) error {
	if (key == "" && !empty) || len(key) > 1024 || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\\\x00\r\n") {
		return appkit.Invalid("对象路径不符合要求")
	}
	for _, part := range strings.Split(key, "/") {
		if part == ".." || part == "." {
			return appkit.Invalid("对象路径不能包含父级目录")
		}
	}
	return nil
}
func (m *Module) origins() []string {
	if v := os.Getenv("EULER_STORAGE_CORS_ORIGINS"); v != "" {
		out := []string{}
		for _, s := range strings.Split(v, ",") {
			out = append(out, strings.TrimSpace(s))
		}
		return out
	}
	u, e := url.Parse(m.rt.PublicURL)
	if e == nil && u.Host != "" {
		return []string{u.Scheme + "://" + u.Host}
	}
	return []string{}
}
func validOrigins(origins []string) bool {
	if len(origins) < 1 || len(origins) > 20 {
		return false
	}
	for _, v := range origins {
		if v == "*" {
			continue
		}
		u, e := url.Parse(v)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && u.Scheme != "http") {
			return false
		}
	}
	return true
}

const bucketCols = "id,tenant_id,project_id,installation_id,name,display_name,description,access,status,used_bytes,object_count,origins,created_at,updated_at"

type scanner interface{ Scan(...any) error }

func scanBucket(row scanner) (Bucket, error) {
	var b Bucket
	var raw string
	e := row.Scan(&b.ID, &b.TenantID, &b.ProjectID, &b.InstallationID, &b.Name, &b.DisplayName, &b.Description, &b.Access, &b.Status, &b.UsedBytes, &b.ObjectCount, &raw, &b.CreatedAt, &b.UpdatedAt)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &b.Origins)
	}
	return b, e
}
func (m *Module) bucket(ctx context.Context, s appkit.Scope, id string) (Bucket, error) {
	return scanBucket(m.rt.DB.QueryRowContext(ctx, "SELECT "+bucketCols+" FROM storage_buckets WHERE id=? AND tenant_id=? AND project_id=? AND status<>'deleted'", id, s.TenantID, s.ProjectID))
}
func (m *Module) buckets(ctx context.Context, s appkit.Scope) ([]Bucket, error) {
	rows, e := m.rt.DB.QueryContext(ctx, "SELECT "+bucketCols+" FROM storage_buckets WHERE tenant_id=? AND project_id=? AND status<>'deleted' ORDER BY created_at DESC", s.TenantID, s.ProjectID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		b, e := scanBucket(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (m *Module) listBuckets(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	items, e := m.buckets(r.Context(), s)
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": items})
	return nil
}
func (m *Module) bucketDetail(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	b, e := m.bucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	appkit.JSON(w, 200, b)
	return nil
}
func (m *Module) overview(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	items, e := m.buckets(r.Context(), s)
	if e != nil {
		return e
	}
	q, e := m.ledger.quota(r.Context(), m.rt.DB, s)
	if e != nil {
		return e
	}
	var used, count, reserved int64
	for _, b := range items {
		used += b.UsedBytes
		count += b.ObjectCount
	}
	if e = m.rt.DB.QueryRowContext(r.Context(), "SELECT COALESCE(SUM(expected_bytes),0) FROM storage_uploads WHERE tenant_id=? AND project_id=? AND state='pending' AND expires_at>?", s.TenantID, s.ProjectID, appkit.Now()).Scan(&reserved); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": items, "quota": q, "usedBytes": used, "objectCount": count, "bucketCount": len(items), "reservedBytes": reserved, "configured": m.plane != nil, "corsMode": m.corsMode()})
	return nil
}
func (m *Module) start(ctx context.Context, s appkit.Scope, bucket, action, key string) (string, error) {
	id := appkit.NewID("st_op_")
	e := m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "INSERT INTO storage_logs VALUES(?,?,?,?,?,?,?,?,?,?)", id, s.TenantID, s.ProjectID, bucket, s.ActorID, action, key, 0, "pending", appkit.Now())
		if e != nil {
			return e
		}
		return m.rt.Audit(ctx, tx, s, action+".started", bucket, "已授权存储操作")
	})
	return id, e
}
func (m *Module) finish(s appkit.Scope, id, bucket, action string, delta int64, externalErr error, fn func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state := "completed"
	if externalErr != nil {
		state = "uncertain"
	}
	return m.rt.Transaction(ctx, func(tx *sql.Tx) error {
		if externalErr == nil && fn != nil {
			if e := fn(ctx, tx); e != nil {
				return e
			}
		}
		if _, e := tx.ExecContext(ctx, "UPDATE storage_logs SET state=?,size_change=? WHERE id=?", state, delta, id); e != nil {
			return e
		}
		return m.rt.Audit(ctx, tx, s, action+"."+state, bucket, "存储操作结果已记录")
	})
}
func (m *Module) createBucket(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	if e := m.ready(); e != nil {
		return e
	}
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Access      string `json:"access"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	name, e := appkit.Name(input.Name, 100)
	if e != nil {
		return e
	}
	if len(input.Description) > 2000 {
		return appkit.Invalid("说明过长")
	}
	if input.Access == "" {
		input.Access = "private"
	}
	if !validAccess(input.Access) {
		return appkit.Invalid("访问策略不符合要求")
	}
	if input.Access != "private" && !s.Can("manage") {
		return appkit.Forbidden("公开访问策略需要项目管理权限")
	}
	origins := m.origins()
	if !validOrigins(origins) {
		return appkit.Unavailable("S3 CORS 来源未正确配置")
	}
	unlock := m.lock(s)
	defer unlock()
	b := Bucket{ID: appkit.NewID("bucket_"), TenantID: s.TenantID, ProjectID: s.ProjectID, InstallationID: s.InstallationID, Name: random("eu-"), DisplayName: name, Description: input.Description, Access: input.Access, Status: "provisioning", Origins: origins, CreatedAt: appkit.Now(), UpdatedAt: appkit.Now()}
	raw, _ := json.Marshal(origins)
	op := appkit.NewID("st_op_")
	e = m.rt.Transaction(r.Context(), func(tx *sql.Tx) error {
		q, e := m.ledger.quota(r.Context(), tx, s)
		if e != nil {
			return e
		}
		var count int64
		if e = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM storage_buckets WHERE tenant_id=? AND project_id=? AND status<>'deleted'", s.TenantID, s.ProjectID).Scan(&count); e != nil {
			return e
		}
		if count >= q.Buckets {
			return appkit.Conflict("存储桶数量已达项目配额")
		}
		_, e = tx.ExecContext(r.Context(), "INSERT INTO storage_buckets VALUES(?,?,?,?,?,?,?,?,?,0,0,?,?,?)", b.ID, s.TenantID, s.ProjectID, s.InstallationID, b.Name, b.DisplayName, b.Description, b.Access, b.Status, string(raw), b.CreatedAt, b.UpdatedAt)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(r.Context(), "INSERT INTO storage_logs VALUES(?,?,?,?,?,?,?,?,?,?)", op, s.TenantID, s.ProjectID, b.ID, s.ActorID, "bucket.create", "", 0, "pending", appkit.Now())
		if e != nil {
			return e
		}
		return m.rt.Audit(r.Context(), tx, s, "bucket.create.started", b.ID, "预留桶配额并开始创建存储桶")
	})
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	externalErr := m.plane.CreateBucket(ctx, b.Name)
	if externalErr == nil {
		externalErr = m.plane.SetAccess(ctx, b.Name, b.Access)
	}
	if externalErr == nil && m.corsMode() == "bucket" {
		externalErr = m.plane.SetCORS(ctx, b.Name, origins)
	}
	e = m.finish(s, op, b.ID, "bucket.create", 0, externalErr, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE storage_buckets SET status='active',updated_at=? WHERE id=?", appkit.Now(), b.ID)
		return e
	})
	if e != nil {
		return e
	}
	if externalErr != nil {
		_, _ = m.rt.DB.ExecContext(context.Background(), "UPDATE storage_buckets SET status='error' WHERE id=?", b.ID)
		return appkit.Unavailable("存储桶创建结果未确认，请查看操作记录")
	}
	b.Status = "active"
	appkit.JSON(w, 201, b)
	return nil
}
func validAccess(v string) bool {
	return v == "private" || v == "public-read" || v == "public-read-write"
}
func (m *Module) updateBucket(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Access      string `json:"access"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	name, e := appkit.Name(input.Name, 100)
	if e != nil {
		return e
	}
	if !validAccess(input.Access) || len(input.Description) > 2000 {
		return appkit.Invalid("访问策略或说明不符合要求")
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.bucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if e = m.ready(); e != nil {
		return e
	}
	op, e := m.start(r.Context(), s, b.ID, "bucket.update", "")
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	externalErr := m.plane.SetAccess(ctx, b.Name, input.Access)
	e = m.finish(s, op, b.ID, "bucket.update", 0, externalErr, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE storage_buckets SET display_name=?,description=?,access=?,status='active',updated_at=? WHERE id=?", name, input.Description, input.Access, appkit.Now(), b.ID)
		return e
	})
	if e != nil {
		return e
	}
	if externalErr != nil {
		return appkit.Unavailable("访问策略更新未确认")
	}
	return m.bucketDetail(w, r, s)
}
func (m *Module) deleteBucket(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	unlock := m.lock(s)
	defer unlock()
	b, e := m.bucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if e = m.ready(); e != nil {
		return e
	}
	force := r.URL.Query().Get("force") == "true"
	if !force {
		items, _, e := m.plane.List(r.Context(), b.Name, "", true, "", 1)
		if e != nil && !absent(e) {
			return appkit.Unavailable("无法确认桶内对象")
		}
		if len(items) > 0 {
			return appkit.Conflict("存储桶不为空；清空后删除或明确选择强制删除")
		}
	}
	op, e := m.start(r.Context(), s, b.ID, "bucket.delete", "")
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 120*time.Second)
	defer cancel()
	externalErr := m.plane.DeleteBucket(ctx, b.Name, force)
	if absent(externalErr) {
		externalErr = nil
	}
	e = m.finish(s, op, b.ID, "bucket.delete", -b.UsedBytes, externalErr, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE storage_buckets SET status='deleted',used_bytes=0,object_count=0,updated_at=? WHERE id=?", appkit.Now(), b.ID)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE storage_uploads SET state='cancelled' WHERE bucket_id=?", b.ID)
		return e
	})
	if e != nil {
		return e
	}
	if externalErr != nil {
		return appkit.Unavailable("删除结果未确认；资源记录和配额仍保留")
	}
	appkit.JSON(w, 200, map[string]bool{"deleted": true})
	return nil
}
func (m *Module) measure(ctx context.Context, b Bucket) (int64, int64, error) {
	var size, count int64
	after := ""
	for {
		items, more, e := m.plane.List(ctx, b.Name, "files/", true, after, 1000)
		if e != nil {
			return 0, 0, e
		}
		for _, v := range items {
			size += v.Size
			if !v.Folder {
				count++
			}
			after = v.Key
		}
		if !more {
			return size, count, nil
		}
		if len(items) == 0 {
			return 0, 0, appkit.Unavailable("S3 分页没有继续前进")
		}
	}
}
func (m *Module) syncProject(ctx context.Context, s appkit.Scope) error {
	items, e := m.buckets(ctx, s)
	if e != nil {
		return e
	}
	for _, b := range items {
		if b.Status != "active" {
			continue
		}
		size, count, e := m.measure(ctx, b)
		if e != nil {
			return appkit.Unavailable("无法读取真实存储用量")
		}
		if _, e = m.rt.DB.ExecContext(ctx, "UPDATE storage_buckets SET used_bytes=?,object_count=?,updated_at=? WHERE id=?", size, count, appkit.Now(), b.ID); e != nil {
			return e
		}
	}
	return nil
}
func (m *Module) syncBucket(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	unlock := m.lock(s)
	defer unlock()
	b, e := m.bucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if e = m.ready(); e != nil {
		return e
	}
	size, count, e := m.measure(r.Context(), b)
	if e != nil {
		return appkit.Unavailable("无法读取实际桶统计")
	}
	if _, e = m.rt.DB.ExecContext(r.Context(), "UPDATE storage_buckets SET used_bytes=?,object_count=?,updated_at=? WHERE id=?", size, count, appkit.Now(), b.ID); e != nil {
		return e
	}
	return m.bucketDetail(w, r, s)
}
func (m *Module) cors(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	var input struct {
		Origins []string `json:"origins"`
	}
	if e := appkit.Decode(w, r, &input); e != nil {
		return e
	}
	if !validOrigins(input.Origins) {
		return appkit.Invalid("请提供有效的 HTTP(S) 来源列表")
	}
	unlock := m.lock(s)
	defer unlock()
	b, e := m.bucket(r.Context(), s, r.PathValue("id"))
	if e != nil {
		return e
	}
	if e = m.ready(); e != nil {
		return e
	}
	if m.corsMode() == "external" {
		return &appkit.Error{Status: 409, Code: "unsupported_feature", Message: "此数据面的 CORS 由部署者统一配置，不支持按桶修改"}
	}
	op, e := m.start(r.Context(), s, b.ID, "bucket.cors", "")
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	externalErr := m.plane.SetCORS(ctx, b.Name, input.Origins)
	raw, _ := json.Marshal(input.Origins)
	e = m.finish(s, op, b.ID, "bucket.cors", 0, externalErr, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE storage_buckets SET origins=?,updated_at=? WHERE id=?", string(raw), appkit.Now(), b.ID)
		return e
	})
	if e != nil {
		return e
	}
	if externalErr != nil {
		return appkit.Unavailable("CORS 更新未确认")
	}
	appkit.JSON(w, 200, map[string]any{"origins": input.Origins})
	return nil
}
func (m *Module) logs(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	rows, e := m.rt.DB.QueryContext(r.Context(), "SELECT id,bucket_id,actor_id,action,object_key,size_change,state,created_at FROM storage_logs WHERE tenant_id=? AND project_id=? ORDER BY created_at DESC LIMIT ? OFFSET ?", s.TenantID, s.ProjectID, limit, offset)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, b, actor, action, key, state, created string
		var delta int64
		if e = rows.Scan(&id, &b, &actor, &action, &key, &delta, &state, &created); e != nil {
			return e
		}
		out = append(out, map[string]any{"id": id, "bucketId": b, "actorId": actor, "action": action, "key": key, "sizeChange": delta, "state": state, "createdAt": created})
	}
	if e = rows.Err(); e != nil {
		return e
	}
	appkit.JSON(w, 200, map[string]any{"items": out, "offset": offset, "limit": limit})
	return nil
}
func (m *Module) cleanup(ctx context.Context) {
	if m.plane == nil {
		return
	}
	rows, e := m.rt.DB.QueryContext(ctx, "SELECT u.id,b.name,b.tenant_id,b.project_id,b.installation_id FROM storage_uploads u JOIN storage_buckets b ON b.id=u.bucket_id WHERE u.expires_at<? AND u.state<>'expired'", appkit.Now())
	if e != nil {
		return
	}
	type pair struct {
		id, b string
		scope appkit.Scope
	}
	var items []pair
	for rows.Next() {
		var v pair
		if rows.Scan(&v.id, &v.b, &v.scope.TenantID, &v.scope.ProjectID, &v.scope.InstallationID) == nil {
			items = append(items, v)
		}
	}
	rows.Close()
	for _, v := range items {
		job, cancel := context.WithTimeout(ctx, 20*time.Second)
		e = m.plane.Delete(job, v.b, "staging/"+v.id)
		cancel()
		if e == nil || absent(e) {
			_, _ = m.rt.DB.ExecContext(ctx, "UPDATE storage_uploads SET state='expired' WHERE id=?", v.id)
		}
	}
}
