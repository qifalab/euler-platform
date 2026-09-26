package witshield

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	_ "modernc.org/sqlite"
)

func fixture(t *testing.T) (*Module, *sql.DB, appkit.Scope) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE installations(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,application_id TEXT,status TEXT);CREATE TABLE audit(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,actor_id TEXT,action TEXT,target_id TEXT,summary TEXT,created_at INTEGER);INSERT INTO installations VALUES('i_a','t','p_a','witshield','enabled'),('i_b','t','p_b','witshield','enabled')`)
	if err != nil {
		t.Fatal(err)
	}
	rt := &appkit.Runtime{DB: db, DataDir: t.TempDir(), PublicURL: "https://console.example", DeriveKey: func(p string) []byte { h := sha256.Sum256([]byte(p)); return h[:] }, ApplicationEnabled: func(ctx context.Context, tenant, project, app string) (bool, error) {
		var n int
		err := db.QueryRowContext(ctx, `SELECT count(*) FROM installations WHERE tenant_id=? AND project_id=? AND application_id=? AND status='enabled'`, tenant, project, app).Scan(&n)
		return n > 0, err
	}}
	m := New(rt)
	t.Cleanup(func() { m.Close() })
	if err = m.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m, db, appkit.Scope{ActorID: "human", TenantID: "t", ProjectID: "p_a", InstallationID: "i_a", ApplicationID: "witshield", Permissions: []string{"read", "write", "manage"}}
}
func invoke(m *Module, s *appkit.Scope, method, path string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Euler-Actor-ID", "forged-admin")
	r.Header.Set("X-Euler-Permissions", "manage,admin")
	if s != nil {
		r = r.WithContext(appkit.WithScope(r.Context(), *s))
	}
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, r)
	return w
}
func TestScopePermissionsAndPersistentProjectIsolation(t *testing.T) {
	m, db, a := fixture(t)
	if w := invoke(m, nil, "GET", "/devices", nil); w.Code != 401 {
		t.Fatalf("headers forged scope: %d", w.Code)
	}
	viewer := a
	viewer.Permissions = []string{"read"}
	for _, request := range []struct{ method, path string }{{"POST", "/enrollment-tokens"}, {"POST", "/devices/fake/scan"}, {"POST", "/actions/fake/approve"}, {"PUT", "/ai/settings"}, {"PUT", "/devices/fake/policy-grants/network.auth_bruteforce"}} {
		if w := invoke(m, &viewer, request.method, request.path, map[string]any{}); w.Code != 403 {
			t.Fatalf("viewer accepted %s: %d", request.path, w.Code)
		}
	}
	writer := a
	writer.Permissions = []string{"read", "write"}
	for _, path := range []string{"/actions", "/actions/fake/approve", "/devices/fake/emergency-stop"} {
		if w := invoke(m, &writer, "POST", path, map[string]any{}); w.Code != 403 {
			t.Fatalf("writer can manage %s: %d", path, w.Code)
		}
	}
	if w := invoke(m, &viewer, "GET", "/schedules", nil); w.Code != 200 {
		t.Fatalf("viewer cannot read schedules: %d", w.Code)
	}
	w := invoke(m, &a, "POST", "/enrollment-tokens", map[string]string{"name": "private node", "expiresIn": "15m"})
	if w.Code != 201 {
		t.Fatalf("token creation %d %s", w.Code, w.Body)
	}
	var created struct {
		Token           string `json:"token"`
		EnrollmentToken struct {
			ID string `json:"id"`
		} `json:"enrollmentToken"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	// Exercise the actual public mount. Its identity comes only from the
	// enrollment challenge, never from browser-supplied identity headers.
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	challengeBody, _ := json.Marshal(map[string]string{"enrollmentToken": created.Token, "identityPublicKey": base64.RawStdEncoding.EncodeToString(publicKey)})
	challengeRequest := httptest.NewRequest("POST", "/public/witshield/t/p_a/i_a/agent/v1/enroll/challenge", bytes.NewReader(challengeBody))
	challengeRequest.Header.Set("Content-Type", "application/json")
	challengeResponse := httptest.NewRecorder()
	http.StripPrefix("/public/witshield", m.PublicHandler()).ServeHTTP(challengeResponse, challengeRequest)
	if challengeResponse.Code != 201 {
		t.Fatalf("public instance mount: %d %s", challengeResponse.Code, challengeResponse.Body)
	}
	b := a
	b.ProjectID = "p_b"
	b.InstallationID = "i_b"
	w = invoke(m, &b, "GET", "/enrollment-tokens", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private node") {
		t.Fatalf("cross-project list: %d %s", w.Code, w.Body)
	}
	w = invoke(m, &b, "DELETE", "/enrollment-tokens/"+created.EnrollmentToken.ID, nil)
	if w.Code != 404 {
		t.Fatalf("cross-project revoke: %d", w.Code)
	}
	mismatch := a
	mismatch.InstallationID = "i_b"
	if w = invoke(m, &mismatch, "GET", "/devices", nil); w.Code != 404 {
		t.Fatalf("mismatched scope %d", w.Code)
	}
	wrongApp := a
	wrongApp.ApplicationID = "weauth"
	if w = invoke(m, &wrongApp, "GET", "/devices", nil); w.Code != 404 {
		t.Fatalf("mismatched app %d", w.Code)
	}
	for _, path := range []string{"/auth/login", "/auth/me", "/admin/bootstrap"} {
		if w = invoke(m, &a, "POST", path, map[string]any{}); w.Code != 404 {
			t.Fatalf("legacy endpoint %s %d", path, w.Code)
		}
	}
	rows, err := db.Query(`SELECT summary FROM audit`)
	if err != nil {
		t.Fatal(err)
	}
	var summaries []string
	for rows.Next() {
		var summary string
		rows.Scan(&summary)
		summaries = append(summaries, summary)
	}
	rows.Close()
	if len(summaries) < 2 {
		t.Fatal("mutation audit missing")
	}
	for _, summary := range summaries {
		if strings.Contains(summary, created.Token) || strings.Contains(summary, "private node") {
			t.Fatal("audit leaked secret/body")
		}
	}
	if w = invoke(m, &a, "GET", "/instance", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "https://console.example/public/witshield/t/p_a/i_a") {
		t.Fatalf("public URL %d %s", w.Code, w.Body)
	}
	_, err = db.Exec(`UPDATE installations SET status='disabled' WHERE id='i_a'`)
	if err != nil {
		t.Fatal(err)
	}
	if w = invoke(m, &a, "GET", "/devices", nil); w.Code != 404 {
		t.Fatalf("disabled scope accepted %d", w.Code)
	}
	public := http.StripPrefix("/public/witshield", m.PublicHandler())
	r := httptest.NewRequest("POST", "/public/witshield/t/p_a/i_a/agent/v1/enroll/challenge", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	public.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("disabled public accepted %d", w.Code)
	}
}
