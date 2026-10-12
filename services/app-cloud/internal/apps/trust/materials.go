package trust

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	// Store only ciphertext in the dedicated private S3 bucket. The encrypted
	// database reference carries no capability to bypass Trust authorization.
	enc, e = m.storeMaterialBody(r.Context(), id, enc)
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
	if e != nil {
		m.cleanupMaterialAfterFailure(id)
	}
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
	plain, e := m.readMaterialBody(r.Context(), v.ID, p.Body)
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
		// Logical deletion is committed first. A failed physical deletion is
		// retained in the object ledger and retried by hourly maintenance.
		_ = m.cleanupMaterialObject(r.Context(), id)
		appkit.JSON(w, 204, nil)
	}
	return e
}

// Material storage deliberately uses a bucket outside storage_buckets. A
// project Storage administrator never gains Trust material access through it.
const materialObjectPrefix = "euler-private-material-v1:"

type materialObjectRef struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (m *Module) materialObjectTables(ctx context.Context) error {
	_, e := m.rt.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS trust_material_objects(material_id TEXT PRIMARY KEY,bucket TEXT NOT NULL,object_key TEXT NOT NULL,created_at TEXT NOT NULL)`)
	return e
}
func materialEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return os.Getenv(fallback)
}
func (m *Module) materialS3(ctx context.Context, bucket string) (*minio.Client, error) {
	dedicatedEndpoint := os.Getenv("EULER_TRUST_MATERIAL_S3_ENDPOINT")
	dedicatedAccess, dedicatedSecret := os.Getenv("EULER_TRUST_MATERIAL_S3_ACCESS_KEY"), os.Getenv("EULER_TRUST_MATERIAL_S3_SECRET_KEY")
	if (dedicatedAccess == "") != (dedicatedSecret == "") {
		return nil, appkit.Unavailable("认证材料对象存储凭据必须成对配置")
	}
	if dedicatedEndpoint != "" && dedicatedEndpoint != os.Getenv("EULER_STORAGE_S3_ENDPOINT") && (dedicatedAccess == "" || dedicatedSecret == "") {
		return nil, appkit.Unavailable("独立认证材料存储端点必须配置独立凭据")
	}
	endpoint := materialEnv("EULER_TRUST_MATERIAL_S3_ENDPOINT", "EULER_STORAGE_S3_ENDPOINT")
	access := materialEnv("EULER_TRUST_MATERIAL_S3_ACCESS_KEY", "EULER_STORAGE_S3_ACCESS_KEY")
	secret := materialEnv("EULER_TRUST_MATERIAL_S3_SECRET_KEY", "EULER_STORAGE_S3_SECRET_KEY")
	if endpoint == "" || access == "" || secret == "" || bucket == "" {
		return nil, appkit.Unavailable("认证材料私有对象存储尚未配置")
	}
	p, e := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: materialEnv("EULER_TRUST_MATERIAL_S3_SECURE", "EULER_STORAGE_S3_SECURE") != "false", Region: materialEnv("EULER_TRUST_MATERIAL_S3_REGION", "EULER_STORAGE_S3_REGION")})
	if e != nil {
		return nil, appkit.Unavailable("认证材料对象存储配置无效")
	}
	// Refuse accidental reuse of a project-visible bucket even if it currently
	// has private ACL: project administrators can change that ACL later.
	var hasTable int
	if e = m.rt.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='storage_buckets'").Scan(&hasTable); e != nil {
		return nil, e
	}
	if hasTable > 0 {
		var shared int
		if e = m.rt.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM storage_buckets WHERE name=?", bucket).Scan(&shared); e != nil {
			return nil, e
		}
		if shared > 0 {
			return nil, appkit.Unavailable("认证材料必须使用独立私有桶")
		}
	}
	policy, e := p.GetBucketPolicy(ctx, bucket)
	if e != nil && minio.ToErrorResponse(e).Code != "NoSuchBucketPolicy" {
		return nil, appkit.Unavailable("无法确认认证材料桶的私有策略")
	}
	if strings.TrimSpace(policy) != "" {
		return nil, appkit.Unavailable("认证材料桶不能配置公开访问策略")
	}
	return p, nil
}
func (m *Module) storeMaterialBody(ctx context.Context, id string, cipher []byte) ([]byte, error) {
	bucket := os.Getenv("EULER_TRUST_MATERIAL_S3_BUCKET")
	if bucket == "" {
		return cipher, nil
	}
	if e := m.materialObjectTables(ctx); e != nil {
		return nil, e
	}
	p, e := m.materialS3(ctx, bucket)
	if e != nil {
		return nil, e
	}
	digest := sha256.Sum256(cipher)
	ref := materialObjectRef{Bucket: bucket, Key: "materials/" + id + "/" + appkit.NewID("blob_"), SHA256: hex.EncodeToString(digest[:]), Size: int64(len(cipher))}
	// A durable ledger precedes the external write. Orphans remain discoverable
	// after a crash or a later scheme/transaction deletion.
	if _, e = m.rt.DB.ExecContext(ctx, "INSERT INTO trust_material_objects VALUES(?,?,?,?)", id, ref.Bucket, ref.Key, appkit.Now()); e != nil {
		return nil, e
	}
	if _, e = p.PutObject(ctx, bucket, ref.Key, bytes.NewReader(cipher), int64(len(cipher)), minio.PutObjectOptions{ContentType: "application/octet-stream"}); e != nil {
		m.cleanupMaterialAfterFailure(id)
		return nil, appkit.Unavailable("认证材料密文写入未确认")
	}
	raw, e := json.Marshal(ref)
	if e != nil {
		return nil, e
	}
	return m.rt.Encrypt(materialObjectPrefix+string(raw), "trust:material-body:"+id)
}
func (m *Module) readMaterialBody(ctx context.Context, id string, cipher []byte) (string, error) {
	plain, e := m.rt.Decrypt(cipher, "trust:material-body:"+id)
	if e != nil {
		return "", e
	}
	if !strings.HasPrefix(plain, materialObjectPrefix) {
		return plain, nil
	}
	var ref materialObjectRef
	if e = json.Unmarshal([]byte(strings.TrimPrefix(plain, materialObjectPrefix)), &ref); e != nil || ref.Size < 1 || ref.Size > 16<<20 || !strings.HasPrefix(ref.Key, "materials/"+id+"/") {
		return "", appkit.Unavailable("认证材料引用无效")
	}
	p, e := m.materialS3(ctx, ref.Bucket)
	if e != nil {
		return "", e
	}
	object, e := p.GetObject(ctx, ref.Bucket, ref.Key, minio.GetObjectOptions{})
	if e != nil {
		return "", appkit.Unavailable("无法读取认证材料密文")
	}
	defer object.Close()
	data, e := io.ReadAll(io.LimitReader(object, ref.Size+1))
	if e != nil || int64(len(data)) != ref.Size {
		return "", appkit.Unavailable("认证材料密文不完整")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != ref.SHA256 {
		return "", appkit.Unavailable("认证材料完整性校验失败")
	}
	return m.rt.Decrypt(data, "trust:material-body:"+id)
}
func (m *Module) cleanupMaterialObject(ctx context.Context, id string) error {
	var ref materialObjectRef
	e := m.rt.DB.QueryRowContext(ctx, "SELECT bucket,object_key FROM trust_material_objects WHERE material_id=?", id).Scan(&ref.Bucket, &ref.Key)
	if e == sql.ErrNoRows {
		return nil
	}
	if e != nil {
		return e
	}
	p, e := m.materialS3(ctx, ref.Bucket)
	if e != nil {
		return e
	}
	// Remove every version. Normal S3 deletion could retain a hidden ciphertext
	// version indefinitely when the operator enables versioning on the bucket.
	for v := range p.ListObjects(ctx, ref.Bucket, minio.ListObjectsOptions{Prefix: ref.Key, Recursive: true, WithVersions: true}) {
		if v.Err != nil {
			return appkit.Unavailable("认证材料清理未确认")
		}
		if v.Key != ref.Key {
			continue
		}
		if e = p.RemoveObject(ctx, ref.Bucket, ref.Key, minio.RemoveObjectOptions{VersionID: v.VersionID}); e != nil {
			return appkit.Unavailable("认证材料清理未确认")
		}
	}
	_, e = m.rt.DB.ExecContext(ctx, "DELETE FROM trust_material_objects WHERE material_id=?", id)
	return e
}

// Maintain is called immediately after startup and hourly by the app-cloud
// maintenance loop. Its grace period avoids deleting an in-flight upload.
func (m *Module) Maintain(ctx context.Context) error { return m.CleanupMaterialObjects(ctx) }
func (m *Module) CleanupMaterialObjects(ctx context.Context) error {
	if e := m.materialObjectTables(ctx); e != nil {
		return e
	}
	rows, e := m.rt.DB.QueryContext(ctx, `SELECT o.material_id FROM trust_material_objects o LEFT JOIN trust_materials m ON m.id=o.material_id WHERE m.id IS NULL AND o.created_at<? LIMIT 100`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano))
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
	var failures []error
	for _, id := range ids {
		if e = m.cleanupMaterialObject(ctx, id); e != nil {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}

// Compensation must not outlive a failed request indefinitely. The durable
// ledger remains for hourly retries if the data plane is unavailable.
func (m *Module) cleanupMaterialAfterFailure(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = m.cleanupMaterialObject(ctx, id)
}
