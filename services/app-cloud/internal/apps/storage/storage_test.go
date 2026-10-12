package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	_ "modernc.org/sqlite"
)

type storedFile struct {
	data        []byte
	contentType string
}
type fakeS3 struct {
	mu       sync.Mutex
	buckets  map[string]map[string]storedFile
	access   map[string]string
	copyHook func()
	copyErr  error
}

func newFake() *fakeS3 {
	return &fakeS3{buckets: map[string]map[string]storedFile{}, access: map[string]string{}}
}
func (f *fakeS3) CreateBucket(_ context.Context, b string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buckets[b] != nil {
		return errors.New("exists")
	}
	f.buckets[b] = map[string]storedFile{}
	return nil
}
func (f *fakeS3) DeleteBucket(_ context.Context, b string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !force && len(f.buckets[b]) > 0 {
		return errors.New("not empty")
	}
	delete(f.buckets, b)
	return nil
}
func (f *fakeS3) SetAccess(_ context.Context, b, a string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.access[b] = a
	return nil
}
func (f *fakeS3) SetCORS(context.Context, string, []string) error { return nil }
func info(k string, v storedFile) Object {
	hash := sha256.Sum256(v.data)
	return Object{Key: k, Size: int64(len(v.data)), ContentType: v.contentType, ETag: hex.EncodeToString(hash[:]), ModifiedAt: time.Now().UTC(), Folder: strings.HasSuffix(k, "/")}
}
func (f *fakeS3) List(_ context.Context, b, prefix string, recursive bool, after string, limit int) ([]Object, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buckets[b] == nil {
		return nil, false, os.ErrNotExist
	}
	data := map[string]Object{}
	for key, v := range f.buckets[b] {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if !recursive {
			if at := strings.Index(strings.TrimPrefix(key, prefix), "/"); at >= 0 {
				key = prefix + strings.TrimPrefix(key, prefix)[:at+1]
				data[key] = Object{Key: key, Folder: true}
				continue
			}
		}
		data[key] = info(key, v)
	}
	var keys []string
	for key := range data {
		if key > after {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	more := len(keys) > limit
	if more {
		keys = keys[:limit]
	}
	out := []Object{}
	for _, key := range keys {
		out = append(out, data[key])
	}
	return out, more, nil
}
func (f *fakeS3) Stat(_ context.Context, b, k string) (Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.buckets[b][k]
	if !ok {
		return Object{}, os.ErrNotExist
	}
	return info(k, v), nil
}
func (f *fakeS3) Put(_ context.Context, b, k string, r io.Reader, size int64, ct string) error {
	data, e := io.ReadAll(r)
	if e != nil {
		return e
	}
	if int64(len(data)) != size {
		return errors.New("size mismatch")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buckets[b] == nil {
		return os.ErrNotExist
	}
	f.buckets[b][k] = storedFile{data, ct}
	return nil
}
func (f *fakeS3) Get(ctx context.Context, b, k string) (io.ReadCloser, Object, error) {
	v, e := f.Stat(ctx, b, k)
	if e != nil {
		return nil, v, e
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return io.NopCloser(bytes.NewReader(f.buckets[b][k].data)), v, nil
}
func (f *fakeS3) Delete(_ context.Context, b, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.buckets[b], k)
	return nil
}
func (f *fakeS3) Copy(_ context.Context, b, source, target, etag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.buckets[b][source]
	if !ok {
		return os.ErrNotExist
	}
	if info(source, v).ETag != etag {
		return errors.New("source changed")
	}
	f.buckets[b][target] = v
	if f.copyHook != nil {
		f.copyHook()
	}
	return f.copyErr
}
func (f *fakeS3) UploadSignature(_ context.Context, b, k string, size int64, ct string, ttl time.Duration) (Signature, error) {
	return Signature{URL: "https://s3.example.test/" + b, Method: "POST", Fields: map[string]string{"key": k}, ExpiresAt: time.Now().Add(ttl).Format(time.RFC3339Nano)}, nil
}
func (f *fakeS3) DownloadSignature(_ context.Context, b, k string, ttl time.Duration) (Signature, error) {
	return Signature{URL: "https://s3.example.test/" + b + "/" + k, Method: "GET"}, nil
}
func testModule(t *testing.T) (*Module, *fakeS3, *atomic.Bool) {
	t.Helper()
	t.Setenv("EULER_STORAGE_DISABLE_SCHEDULER", "true")
	t.Setenv("EULER_STORAGE_S3_ENDPOINT", "")
	db, e := sql.Open("sqlite", t.TempDir()+"/metadata.sqlite")
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, e = db.Exec("CREATE TABLE audit(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,actor_id TEXT,action TEXT,target_id TEXT,summary TEXT,created_at INTEGER)")
	if e != nil {
		t.Fatal(e)
	}
	enabled := &atomic.Bool{}
	enabled.Store(true)
	rt := &appkit.Runtime{DB: db, DataDir: t.TempDir(), PublicURL: "https://euler.example.test", DeriveKey: func(string) []byte { return bytes.Repeat([]byte{7}, 32) }, ApplicationEnabled: func(context.Context, string, string, string) (bool, error) { return enabled.Load(), nil }}
	m := New(rt)
	fake := newFake()
	m.plane = fake
	if e = m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { m.Close() })
	return m, fake, enabled
}
func scope() appkit.Scope {
	return appkit.Scope{ActorID: "alice", TenantID: "tenant1", ProjectID: "project1", InstallationID: "install1", ApplicationID: "storage", Permissions: []string{"read", "write", "manage", "secrets", "admin"}}
}
func request(t *testing.T, m *Module, s appkit.Scope, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var e error
		encoded, e = json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	r.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(appkit.WithScope(r.Context(), s), 5*time.Second)
	defer cancel()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, r.WithContext(ctx))
	return w
}
func decode[T any](t *testing.T, w *httptest.ResponseRecorder, want int) T {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
	}
	var v T
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func makeBucket(t *testing.T, m *Module, s appkit.Scope, acl string) Bucket {
	return decode[Bucket](t, request(t, m, s, "POST", "/buckets", map[string]any{"name": "Project files", "access": acl}), 201)
}
func reserveFile(t *testing.T, m *Module, s appkit.Scope, b Bucket, key string, size int64) string {
	v := decode[struct {
		UploadID  string    `json:"uploadId"`
		Signature Signature `json:"signature"`
	}](t, request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/presigned-upload", map[string]any{"key": key, "size": size, "contentType": "text/plain"}), 201)
	if v.Signature.Method != "POST" {
		t.Fatal("upload must use size-constrained POST policy")
	}
	return v.UploadID
}
func TestStorageIsolationPermissionsAndSecrets(t *testing.T) {
	m, _, enabled := testModule(t)
	s := scope()
	b := makeBucket(t, m, s, "private")
	other := s
	other.ProjectID = "project2"
	other.InstallationID = "install2"
	for _, path := range []string{"/buckets/" + b.ID, "/buckets/" + b.ID + "/objects", "/buckets/" + b.ID + "/objects/info?key=a"} {
		if got := request(t, m, other, "GET", path, nil); got.Code != 404 {
			t.Fatalf("cross project %s: %d", path, got.Code)
		}
	}
	if got := request(t, m, other, "DELETE", "/buckets/"+b.ID+"?force=true", nil); got.Code != 404 {
		t.Fatal(got.Body.String())
	}
	viewer := s
	viewer.Permissions = []string{"read"}
	if got := request(t, m, viewer, "POST", "/buckets", map[string]string{"name": "forbidden"}); got.Code != 403 {
		t.Fatal(got.Body.String())
	}
	manager := s
	manager.Permissions = []string{"read", "write", "manage", "secrets"}
	if got := request(t, m, manager, "GET", "/admin/templates", nil); got.Code != 403 {
		t.Fatal("project manager gained operations role")
	}
	k := decode[APIKey](t, request(t, m, s, "POST", "/keys", map[string]any{"name": "read key", "scopes": []string{"read"}}), 201)
	if k.SecretKey == "" {
		t.Fatal("missing one-time secret")
	}
	list := request(t, m, s, "GET", "/keys", nil)
	if strings.Contains(list.Body.String(), k.SecretKey) || strings.Contains(list.Body.String(), "secretHash") {
		t.Fatal("secret leaked in list")
	}
	call := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("X-Access-Key", k.AccessKey)
		r.Header.Set("X-Secret-Key", k.SecretKey)
		w := httptest.NewRecorder()
		m.PublicHandler().ServeHTTP(w, r)
		return w
	}
	if got := call("GET", "/s3/buckets"); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if got := call("PUT", "/s3/buckets/"+b.ID+"/objects/evil"); got.Code != 403 {
		t.Fatal("read key wrote object")
	}
	b2 := makeBucket(t, m, other, "private")
	if got := call("GET", "/s3/buckets/"+b2.ID+"/objects"); got.Code != 404 {
		t.Fatal("key escaped project")
	}
	enabled.Store(false)
	if got := call("GET", "/s3/buckets"); got.Code != 404 {
		t.Fatal("disabled installation accepted key")
	}
	enabled.Store(true)
	decode[map[string]any](t, request(t, m, s, "PUT", "/keys/"+k.ID, map[string]bool{"active": false}), 200)
	if got := call("GET", "/s3/buckets"); got.Code != 401 {
		t.Fatal("revoked key still active")
	}
}
func TestUploadReservationsCommitAndTamper(t *testing.T) {
	m, f, _ := testModule(t)
	m.ledger.base.StorageBytes = 8
	s := scope()
	b := makeBucket(t, m, s, "private")
	id := reserveFile(t, m, s, b, "a.txt", 4)
	if got := request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/presigned-upload", map[string]any{"key": "too-large", "size": 5}); got.Code != 409 {
		t.Fatalf("reservation oversubscribed: %s", got.Body.String())
	}
	if e := f.Put(context.Background(), b.Name, "staging/"+id, strings.NewReader("test"), 4, "text/plain"); e != nil {
		t.Fatal(e)
	}
	wrongActor := s
	wrongActor.ActorID = "bob"
	if got := request(t, m, wrongActor, "POST", "/buckets/"+b.ID+"/objects/confirm-upload", map[string]string{"uploadId": id}); got.Code != 404 {
		t.Fatal("foreign actor confirmed upload")
	}
	for i := 0; i < 2; i++ {
		decode[Object](t, request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/confirm-upload", map[string]string{"uploadId": id}), 200)
	}
	var uploaded int
	if e := m.rt.DB.QueryRow("SELECT COUNT(*) FROM storage_logs WHERE action='object.upload' AND state='completed'").Scan(&uploaded); e != nil || uploaded != 1 {
		t.Fatalf("upload completion not idempotent: %d %v", uploaded, e)
	}
	var used int64
	if e := m.rt.DB.QueryRow("SELECT used_bytes FROM storage_buckets WHERE id=?", b.ID).Scan(&used); e != nil || used != 4 {
		t.Fatalf("quota mismatch: %d %v", used, e)
	}
	id2 := reserveFile(t, m, s, b, "b.txt", 2)
	_ = f.Put(context.Background(), b.Name, "staging/"+id2, strings.NewReader("bad!"), 4, "text/plain")
	if got := request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/confirm-upload", map[string]string{"uploadId": id2}); got.Code != 409 {
		t.Fatal("oversize staging object published")
	}
	if _, e := f.Stat(context.Background(), b.Name, "files/b.txt"); !absent(e) {
		t.Fatal("tampered upload appeared in project")
	}
	if got := request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/presigned-upload", map[string]any{"key": "../escape", "size": 1}); got.Code != 400 {
		t.Fatal("path traversal accepted")
	}
}
func TestUploadCompletionSurvivesCancellation(t *testing.T) {
	m, f, _ := testModule(t)
	s := scope()
	b := makeBucket(t, m, s, "private")
	id := reserveFile(t, m, s, b, "cancel.txt", 3)
	_ = f.Put(context.Background(), b.Name, "staging/"+id, strings.NewReader("abc"), 3, "text/plain")
	ctx, cancel := context.WithCancel(context.Background())
	f.copyHook = cancel
	r := httptest.NewRequest("POST", "/buckets/"+b.ID+"/objects/confirm-upload", strings.NewReader(`{"uploadId":"`+id+`"}`))
	r = r.WithContext(appkit.WithScope(ctx, s))
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var count int
	if e := m.rt.DB.QueryRow("SELECT COUNT(*) FROM audit WHERE action='storage.object.upload.completed'").Scan(&count); e != nil || count != 1 {
		t.Fatalf("lost actual mutation audit: %d %v", count, e)
	}
}
func TestPublicWriteAndDisable(t *testing.T) {
	m, f, enabled := testModule(t)
	s := scope()
	b := makeBucket(t, m, s, "public-read-write")
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		m.PublicHandler().ServeHTTP(w, r)
		return w
	}
	if got := call("PUT", "/files/"+b.ID+"/open.txt", "hello"); got.Code != 201 {
		t.Fatal(got.Body.String())
	}
	if _, e := f.Stat(context.Background(), b.Name, "files/open.txt"); e != nil {
		t.Fatal(e)
	}
	if got := call("GET", "/files/"+b.ID+"/open.txt", ""); got.Code != 302 {
		t.Fatal(got.Body.String())
	}
	enabled.Store(false)
	if got := call("GET", "/files/"+b.ID+"/open.txt", ""); got.Code != 404 {
		t.Fatal("disabled app still serves public links")
	}
	enabled.Store(true)
	decode[Bucket](t, request(t, m, s, "PUT", "/buckets/"+b.ID, map[string]string{"name": "private now", "access": "private"}), 200)
	if got := call("PUT", "/files/"+b.ID+"/closed.txt", "no"); got.Code != 403 {
		t.Fatal("private bucket accepted anonymous write")
	}
}
func TestQuotaReservationsConcurrent(t *testing.T) {
	m, _, _ := testModule(t)
	m.ledger.base.StorageBytes = 5
	s := scope()
	b := makeBucket(t, m, s, "private")
	var success atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/presigned-upload", map[string]any{"key": "file", "size": 3})
			if w.Code == 201 {
				success.Add(1)
			} else if w.Code != 409 {
				t.Errorf("unexpected %d: %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatalf("overbooked quota: %d reservations", success.Load())
	}
}

type externalCORS struct{ *fakeS3 }

func (externalCORS) CORSMode() string { return "external" }
func (externalCORS) SetCORS(context.Context, string, []string) error {
	return errors.New("per-bucket CORS must never be called in external mode")
}
func TestExternalCORSCapability(t *testing.T) {
	m, f, _ := testModule(t)
	m.plane = externalCORS{f}
	s := scope()
	b := makeBucket(t, m, s, "private")
	v := decode[struct {
		CORSMode string `json:"corsMode"`
	}](t, request(t, m, s, "GET", "/", nil), 200)
	if v.CORSMode != "external" {
		t.Fatal("capability not disclosed")
	}
	w := request(t, m, s, "POST", "/buckets/"+b.ID+"/cors", map[string]any{"origins": []string{"https://new.example.test"}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "unsupported_feature") {
		t.Fatalf("must not claim per-bucket CORS changed: %d %s", w.Code, w.Body.String())
	}
}
