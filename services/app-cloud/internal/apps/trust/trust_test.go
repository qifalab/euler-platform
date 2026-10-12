package trust

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"io"
	"mime/multipart"
	_ "modernc.org/sqlite"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	m       *Module
	rt      *appkit.Runtime
	path    string
	enabled bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("EULER_NOTIFY_WECOM_URL", "")
	t.Setenv("EULER_NOTIFY_FEISHU_URL", "")
	f := &fixture{path: filepath.Join(t.TempDir(), "trust.sqlite"), enabled: true}
	key := bytes.Repeat([]byte{42}, 32)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	f.rt = &appkit.Runtime{Encrypt: func(p, a string) ([]byte, error) {
		n := make([]byte, aead.NonceSize())
		if _, e := rand.Read(n); e != nil {
			return nil, e
		}
		return aead.Seal(n, n, []byte(p), []byte(a)), nil
	}, Decrypt: func(b []byte, a string) (string, error) {
		p, e := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], []byte(a))
		return string(p), e
	}, ApplicationEnabled: func(context.Context, string, string, string) (bool, error) { return f.enabled, nil }}
	f.open(t)
	t.Cleanup(func() { f.rt.DB.Close() })
	return f
}
func (f *fixture) open(t *testing.T) {
	t.Helper()
	db, e := sql.Open("sqlite", f.path)
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	f.rt.DB = db
	if _, e = db.Exec(`CREATE TABLE IF NOT EXISTS audit(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,actor_id TEXT,action TEXT,target_id TEXT,summary TEXT,created_at INTEGER)`); e != nil {
		t.Fatal(e)
	}
	f.m = New(f.rt)
	if e = f.m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func person(id string, permissions ...string) appkit.Scope {
	return appkit.Scope{ActorID: id, ActorName: "Person " + id, Email: id + "@example.test", TenantID: "team-a", ProjectID: "project-a", InstallationID: "install", ApplicationID: "trust", Permissions: permissions}
}
func request(t *testing.T, m *Module, s appkit.Scope, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b io.Reader
	if body != nil {
		raw, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		b = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, path, b)
	r = r.WithContext(appkit.WithScope(r.Context(), s))
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, r)
	return w
}
func expect(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("want %d got %d: %s", status, w.Code, w.Body.String())
	}
}
func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func schemeFixture(t *testing.T, f *fixture) Scheme {
	admin := person("admin", "read", "write", "manage", "secrets", "admin")
	v := Scheme{Name: "成员实名", Status: "active", Fields: []Field{{Name: "full_name", Label: "姓名", Type: "text", Required: true}, {Name: "zero", Label: "数字", Type: "number", Required: true}, {Name: "proof", Label: "证明文件", Type: "file"}}}
	w := request(t, f.m, admin, "POST", "/schemes", v)
	expect(t, w, 201)
	return decode[Scheme](t, w)
}
func submitFixture(t *testing.T, f *fixture, scheme Scheme, who appkit.Scope, version int, name string, material string) Submission {
	data := map[string]any{"full_name": name, "zero": 0}
	if material != "" {
		data["proof"] = material
	}
	w := request(t, f.m, who, "POST", "/submissions", map[string]any{"schemeId": scheme.ID, "schemeVersion": scheme.Version, "version": version, "data": data})
	expect(t, w, 201)
	return decode[Submission](t, w)
}
func upload(t *testing.T, f *fixture, s appkit.Scope, scheme Scheme) Material {
	t.Helper()
	var b bytes.Buffer
	form := multipart.NewWriter(&b)
	_ = form.WriteField("schemeId", scheme.ID)
	_ = form.WriteField("fieldName", "proof")
	part, _ := form.CreateFormFile("file", "private-evidence.txt")
	_, _ = part.Write([]byte("secret evidence material"))
	form.Close()
	r := httptest.NewRequest("POST", "/materials", &b)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r = r.WithContext(appkit.WithScope(r.Context(), s))
	w := httptest.NewRecorder()
	f.m.Handler().ServeHTTP(w, r)
	expect(t, w, 201)
	return decode[Material](t, w)
}

func TestPersistentWorkflowAndIsolation(t *testing.T) {
	f := newFixture(t)
	scheme := schemeFixture(t, f)
	user := person("alice", "read", "write")
	reviewer := person("reviewer", "read", "review")
	admin := person("owner", "read", "write", "manage", "secrets", "admin")
	viewer := person("viewer", "read")
	mat := upload(t, f, user, scheme)
	sub := submitFixture(t, f, scheme, user, 0, "Private legal name", mat.ID)
	expect(t, request(t, f.m, admin, "GET", "/review/submissions", nil), 403)
	expect(t, request(t, f.m, reviewer, "POST", "/schemes", scheme), 403)
	expect(t, request(t, f.m, viewer, "POST", "/submissions", map[string]any{}), 403)
	other := person("bob", "read", "write")
	expect(t, request(t, f.m, other, "GET", "/submissions/"+sub.ID, nil), 404)
	expect(t, request(t, f.m, other, "GET", "/materials/"+mat.ID, nil), 404)
	foreign := reviewer
	foreign.ProjectID = "project-b"
	expect(t, request(t, f.m, foreign, "GET", "/review/submissions/"+sub.ID, nil), 404)
	expect(t, request(t, f.m, foreign, "GET", "/review/materials/"+mat.ID, nil), 404)
	w := request(t, f.m, reviewer, "POST", "/review/submissions/"+sub.ID, map[string]any{"version": sub.Version, "status": "rejected", "reason": "请补充资料"})
	expect(t, w, 200)
	rejected := decode[Submission](t, w)
	if len(rejected.History) != 2 || rejected.Reason != "请补充资料" {
		t.Fatal("review history missing")
	}
	resub := submitFixture(t, f, scheme, user, rejected.Version, "Updated private legal name", mat.ID)
	expect(t, request(t, f.m, reviewer, "POST", "/review/submissions/"+sub.ID, map[string]any{"version": rejected.Version, "status": "approved"}), 409)
	w = request(t, f.m, reviewer, "POST", "/review/submissions/"+sub.ID, map[string]any{"version": resub.Version, "status": "approved"})
	expect(t, w, 200)
	approved := decode[Submission](t, w)
	if len(approved.History) != 4 || approved.Reason != "" {
		t.Fatal("missing lifecycle")
	}
	expect(t, request(t, f.m, user, "POST", "/submissions", map[string]any{"schemeId": scheme.ID, "schemeVersion": scheme.Version, "version": approved.Version, "data": map[string]any{"full_name": "next", "zero": 0}}), 409)
	for _, q := range []string{"SELECT payload FROM trust_submissions", "SELECT body FROM trust_materials", "SELECT metadata FROM trust_materials", "SELECT profile FROM trust_actors"} {
		var b []byte
		if e := f.rt.DB.QueryRow(q).Scan(&b); e != nil {
			t.Fatal(e)
		}
		if bytes.Contains(b, []byte("private")) || bytes.Contains(b, []byte("alice@example.test")) || bytes.Contains(b, []byte("legal name")) {
			t.Fatal("PII stored in plaintext")
		}
	}
	f.rt.DB.Close()
	f.open(t)
	expect(t, request(t, f.m, user, "GET", "/submissions/"+sub.ID, nil), 200)
	file := request(t, f.m, user, "GET", "/materials/"+mat.ID, nil)
	expect(t, file, 200)
	if file.Body.String() != "secret evidence material" {
		t.Fatal("material did not survive restart")
	}
	eid := user
	eid.ApplicationID = "eid"
	ok, e := CheckQualification(context.Background(), f.rt, eid, scheme.ID)
	if e != nil || !ok {
		t.Fatalf("qualified=%v err=%v", ok, e)
	}
	eid.ActorID = "bob"
	ok, e = CheckQualification(context.Background(), f.rt, eid, scheme.ID)
	if e != nil || ok {
		t.Fatal("another actor inherited qualification")
	}
	f.enabled = false
	ok, e = CheckQualification(context.Background(), f.rt, user, scheme.ID)
	if e != nil || ok {
		t.Fatal("disabled app qualification accepted")
	}
}
func TestValidationAndAtomicAudit(t *testing.T) {
	f := newFixture(t)
	scheme := schemeFixture(t, f)
	user := person("alice", "read", "write")
	for _, data := range []map[string]any{{"zero": 0}, {"full_name": "Alice", "zero": 0, "actorId": "admin"}, {"full_name": "Alice", "zero": "abc"}, {"full_name": "Alice", "zero": 0, "proof": "foreign-material"}} {
		expect(t, request(t, f.m, user, "POST", "/submissions", map[string]any{"schemeId": scheme.ID, "schemeVersion": scheme.Version, "data": data}), 400)
	}
	_, e := f.rt.DB.Exec("CREATE TRIGGER block_history BEFORE INSERT ON trust_history BEGIN SELECT RAISE(ABORT,'history unavailable'); END;")
	if e != nil {
		t.Fatal(e)
	}
	expect(t, request(t, f.m, user, "POST", "/submissions", map[string]any{"schemeId": scheme.ID, "schemeVersion": scheme.Version, "data": map[string]any{"full_name": "Alice", "zero": 0}}), 500)
	var n int
	_ = f.rt.DB.QueryRow("SELECT count(*) FROM trust_submissions").Scan(&n)
	if n != 0 {
		t.Fatal("submission committed without durable history")
	}
	_, _ = f.rt.DB.Exec("DROP TRIGGER block_history")
	foreign := person("mallory", "read", "write")
	foreign.ProjectID = "foreign"
	expect(t, request(t, f.m, foreign, "POST", "/submissions", map[string]any{"schemeId": scheme.ID, "schemeVersion": scheme.Version, "data": map[string]any{"full_name": "Mallory", "zero": 0}}), 404)
}
func TestScopedAPIKeyRevocationAndEnabled(t *testing.T) {
	f := newFixture(t)
	scheme := schemeFixture(t, f)
	user := person("alice", "read", "write")
	admin := person("admin", "read", "admin", "review", "secrets")
	mat := upload(t, f, user, scheme)
	sub := submitFixture(t, f, scheme, user, 0, "Alice", mat.ID)
	expect(t, request(t, f.m, admin, "POST", "/review/submissions/"+sub.ID, map[string]any{"version": sub.Version, "status": "approved"}), 200)
	keySpec := APIKey{Name: "资格消费", SchemeIDs: []string{scheme.ID}, ActorIDs: []string{user.ActorID}, Details: true, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	operator := person("operator", "read", "admin", "secrets")
	expect(t, request(t, f.m, operator, "POST", "/keys", keySpec), 403)
	noSecrets := person("operator", "read", "admin", "review")
	expect(t, request(t, f.m, noSecrets, "POST", "/keys", keySpec), 403)
	w := request(t, f.m, admin, "POST", "/keys", APIKey{Name: "资格消费", SchemeIDs: []string{scheme.ID}, ActorIDs: []string{user.ActorID}, Details: true, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)})
	expect(t, w, 201)
	created := decode[struct {
		Key   APIKey `json:"key"`
		Token string `json:"token"`
	}](t, w)
	pub := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		f.m.PublicHandler().ServeHTTP(w, r)
		return w
	}
	path := "/verification/status?actorId=alice&schemeId=" + scheme.ID
	keySpec.Details = false
	statusKeyResponse := request(t, f.m, operator, "POST", "/keys", keySpec)
	expect(t, statusKeyResponse, 201)
	statusKey := decode[struct {
		Token string `json:"token"`
	}](t, statusKeyResponse)
	expect(t, pub(path, statusKey.Token), 200)
	expect(t, pub("/verification/details?actorId=alice&schemeId="+scheme.ID, statusKey.Token), 403)
	expect(t, pub("/materials/"+mat.ID, statusKey.Token), 403)
	expect(t, pub(path, created.Token), 200)
	expect(t, pub("/verification/details?actorId=alice&schemeId="+scheme.ID, created.Token), 200)
	expect(t, pub("/materials/"+mat.ID, created.Token), 200)
	expect(t, pub("/verification/status?actorId=bob&schemeId="+scheme.ID, created.Token), 403)
	expect(t, pub(path+"&apiKey="+created.Token, ""), 401)
	expect(t, pub("/verification/status?oauthId=alice&schemeId="+scheme.ID, created.Token), 403)
	f.enabled = false
	expect(t, pub(path, created.Token), 403)
	f.enabled = true
	expect(t, request(t, f.m, noSecrets, "DELETE", "/keys/"+created.Key.ID, nil), 403)
	expect(t, request(t, f.m, admin, "DELETE", "/keys/"+created.Key.ID, nil), 204)
	expect(t, pub(path, created.Token), 401)
}
func TestNotificationsPersistProviderResult(t *testing.T) {
	f := newFixture(t)
	var received int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "private legal name") {
			t.Error("PII in lifecycle notification")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer server.Close()
	t.Setenv("EULER_NOTIFY_WECOM_URL", server.URL)
	scheme := schemeFixture(t, f)
	user := person("alice", "read", "write")
	sub := submitFixture(t, f, scheme, user, 0, "private legal name", "")
	if received != 1 {
		t.Fatalf("received %d", received)
	}
	var status string
	var attempts int
	if e := f.rt.DB.QueryRow("SELECT status,attempts FROM trust_notifications WHERE submission_id=?", sub.ID).Scan(&status, &attempts); e != nil || status != "sent" || attempts != 1 {
		t.Fatalf("status=%s attempts=%d err=%v", status, attempts, e)
	}
}
