package weauth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	_ "modernc.org/sqlite"
)

func fixture(t *testing.T) (*Module, appkit.Scope, *bool) {
	t.Helper()
	db, e := sql.Open("sqlite", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, e = db.Exec("PRAGMA foreign_keys=ON; CREATE TABLE audit(id TEXT,tenant_id TEXT,project_id TEXT,actor_id TEXT,action TEXT,target_id TEXT,summary TEXT,created_at INTEGER)"); e != nil {
		t.Fatal(e)
	}
	key := make([]byte, 32)
	rand.Read(key)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	enabled := true
	runtime := &appkit.Runtime{DB: db, PublicURL: "https://euler.example", ApplicationEnabled: func(context.Context, string, string, string) (bool, error) { return enabled, nil }, Encrypt: func(value, aad string) ([]byte, error) {
		nonce := make([]byte, aead.NonceSize())
		rand.Read(nonce)
		return aead.Seal(nonce, nonce, []byte(value), []byte(aad)), nil
	}, Decrypt: func(value []byte, aad string) (string, error) {
		data, e := aead.Open(nil, value[:aead.NonceSize()], value[aead.NonceSize():], []byte(aad))
		return string(data), e
	}}
	m := New(runtime)
	if e = m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	scope := appkit.Scope{ActorID: "owner", TenantID: "team", ProjectID: "project", InstallationID: "installed", ApplicationID: "weauth", Permissions: []string{"read", "write", "manage", "secrets"}}
	return m, scope, &enabled
}
func call(t *testing.T, h http.Handler, s *appkit.Scope, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var e error
		raw, e = json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.RemoteAddr = "203.0.113.5:12345"
	if s != nil {
		r = r.WithContext(appkit.WithScope(r.Context(), *s))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func expect(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d want %d: %s", w.Code, status, w.Body.String())
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
func createTest(t *testing.T, m *Module, s appkit.Scope, batch bool) Site {
	t.Helper()
	w := call(t, m.Handler(), &s, "POST", "/sites", map[string]any{"name": "User portal", "domains": []string{"example.com", "*.example.net"}, "baseDifficulty": 1, "maxDifficulty": 3, "batchModeEnabled": batch, "batchDifficulty": 1, "minBatchCount": 2, "maxBatchCount": 4})
	expect(t, w, 201)
	return decode[Site](t, w)
}

type proof struct {
	ID         string   `json:"challenge_id"`
	Seed       string   `json:"challenge"`
	Seeds      []string `json:"challenges"`
	Difficulty int      `json:"difficulty"`
	Batch      bool     `json:"batch_mode"`
}

func getProof(t *testing.T, m *Module, site Site) proof {
	t.Helper()
	w := call(t, m.PublicHandler(), nil, "POST", "/pow/challenge", map[string]string{"sitekey": site.Sitekey, "origin": "https://example.com", "action": "login"})
	expect(t, w, 200)
	return decode[proof](t, w)
}
func solve(p proof) map[string]any {
	seeds := p.Seeds
	if !p.Batch {
		seeds = []string{p.Seed}
	}
	nonces := []string{}
	for _, seed := range seeds {
		for n := 0; ; n++ {
			nonce := fmt.Sprint(n)
			if verifySolution(seed, nonce, p.Difficulty) {
				nonces = append(nonces, nonce)
				break
			}
		}
	}
	return map[string]any{"challenge_id": p.ID, "batch_mode": p.Batch, "nonce": nonces[0], "nonces": nonces}
}
func token(t *testing.T, m *Module, site Site) string {
	t.Helper()
	p := getProof(t, m, site)
	w := call(t, m.PublicHandler(), nil, "POST", "/pow/verify", solve(p))
	expect(t, w, 200)
	return decode[struct {
		Token string `json:"token"`
	}](t, w).Token
}
func secretFor(t *testing.T, m *Module, s appkit.Scope, site Site) string {
	t.Helper()
	w := call(t, m.Handler(), &s, "GET", "/sites/"+site.ID+"/secret", nil)
	expect(t, w, 200)
	return decode[map[string]string](t, w)["secret"]
}

func TestProjectPermissionsAndSecretBoundary(t *testing.T) {
	m, s, _ := fixture(t)
	site := createTest(t, m, s, false)
	h := m.Handler()
	secret := secretFor(t, m, s, site)
	viewer := s
	viewer.Permissions = []string{"read"}
	for _, test := range []struct {
		method, path string
		body         any
	}{{"POST", "/sites", map[string]any{"name": "forbidden"}}, {"PUT", "/sites/" + site.ID, map[string]any{"enabled": false}}, {"DELETE", "/sites/" + site.ID, nil}, {"GET", "/sites/" + site.ID + "/secret", nil}, {"GET", "/sites/" + site.ID + "/ip-records", nil}, {"POST", "/sites/" + site.ID + "/regenerate-secret", map[string]any{"confirm": true}}} {
		expect(t, call(t, h, &viewer, test.method, test.path, test.body), 403)
	}
	other := s
	other.ProjectID = "other"
	for _, path := range []string{"", "/secret", "/stats", "/ip-policy", "/ip-records"} {
		expect(t, call(t, h, &other, "GET", "/sites/"+site.ID+path, nil), 404)
	}
	expect(t, call(t, h, &other, "DELETE", "/sites/"+site.ID, nil), 404)
	for _, path := range []string{"/sites", "/sites/" + site.ID} {
		w := call(t, h, &s, "GET", path, nil)
		expect(t, w, 200)
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("secret leaked to normal read")
		}
	}
	var encrypted []byte
	if e := m.rt.DB.QueryRow("SELECT secret FROM weauth_sites WHERE id=?", site.ID).Scan(&encrypted); e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(encrypted, []byte(secret)) {
		t.Fatal("plaintext at rest")
	}
	var audit string
	if e := m.rt.DB.QueryRow("SELECT group_concat(summary) FROM audit").Scan(&audit); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(audit, secret) {
		t.Fatal("secret in audit")
	}
	expect(t, call(t, h, nil, "GET", "/sites", nil), 401)
}
func TestFullManagementWorkflow(t *testing.T) {
	m, s, _ := fixture(t)
	site := createTest(t, m, s, false)
	path := "/sites/" + site.ID
	h := m.Handler()
	settings := site.Settings
	settings.Name = "Updated"
	settings.ChallengeTimeout = 120
	settings.TokenTimeout = 90
	settings.BatchModeEnabled = true
	expect(t, call(t, h, &s, "PUT", path, settings), 200)
	expect(t, call(t, h, &s, "POST", path+"/domains", map[string]string{"domain": "login.example.org"}), 200)
	expect(t, call(t, h, &s, "POST", path+"/domains", map[string]string{"domain": "https://example.org"}), 400)
	expect(t, call(t, h, &s, "DELETE", path+"/domains/login.example.org", nil), 200)
	policy := site.Policy
	policy.RequestThreshold = 2
	expect(t, call(t, h, &s, "PUT", path+"/ip-policy", policy), 200)
	policy.RequestThreshold = 0
	expect(t, call(t, h, &s, "PUT", path+"/ip-policy", policy), 400)
	old := secretFor(t, m, s, site)
	expect(t, call(t, h, &s, "POST", path+"/regenerate-secret", map[string]bool{"confirm": false}), 400)
	w := call(t, h, &s, "POST", path+"/regenerate-secret", map[string]bool{"confirm": true})
	expect(t, w, 200)
	if decode[map[string]string](t, w)["secret"] == old {
		t.Fatal("secret unchanged")
	}
	p := getProof(t, m, site)
	if !p.Batch || len(p.Seeds) != 2 {
		t.Fatalf("updated batch settings missing: %+v", p)
	}
	expect(t, call(t, h, &s, "GET", path+"/stats", nil), 200)
	expect(t, call(t, h, &s, "DELETE", path+"/ip-records/203.0.113.5", nil), 204)
	expect(t, call(t, h, &s, "DELETE", path, nil), 204)
	expect(t, call(t, h, &s, "GET", path, nil), 404)
	expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/verify", solve(p)), 400)
}
func TestRealProofSingleUseAndExpiry(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			m, s, enabled := fixture(t)
			site := createTest(t, m, s, batch)
			p := getProof(t, m, site)
			solution := solve(p)
			bad := solve(p)
			seed := p.Seed
			if batch {
				seed = p.Seeds[0]
			}
			wrong := "wrong"
			for verifySolution(seed, wrong, p.Difficulty) {
				wrong += "x"
			}
			bad["nonce"] = wrong
			if batch {
				nonces := bad["nonces"].([]string)
				nonces[0] = wrong
				bad["nonces"] = nonces
			}
			expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/verify", bad), 400)
			w := call(t, m.PublicHandler(), nil, "POST", "/pow/verify", solution)
			expect(t, w, 200)
			value := decode[map[string]any](t, w)["token"].(string)
			expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/verify", solution), 409)
			secret := secretFor(t, m, s, site)
			var wg sync.WaitGroup
			responses := make(chan bool, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					w := call(t, m.PublicHandler(), nil, "POST", "/pow/siteverify", map[string]string{"secret": secret, "token": value})
					responses <- decode[map[string]any](t, w)["success"] == true
				}()
			}
			wg.Wait()
			close(responses)
			successes := 0
			for ok := range responses {
				if ok {
					successes++
				}
			}
			if successes != 1 {
				t.Fatalf("token consumed %d times", successes)
			}
			expired := getProof(t, m, site)
			if _, e := m.rt.DB.Exec("UPDATE weauth_challenges SET expires_at=? WHERE id=?", time.Now().Add(-time.Second).Unix(), expired.ID); e != nil {
				t.Fatal(e)
			}
			expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/verify", solve(expired)), 400)
			expiredToken := token(t, m, site)
			if _, e := m.rt.DB.Exec("UPDATE weauth_challenges SET solved_at=? WHERE token_hash=?", time.Now().Add(-time.Hour).Unix(), digest(expiredToken)); e != nil {
				t.Fatal(e)
			}
			w = call(t, m.PublicHandler(), nil, "POST", "/pow/siteverify", map[string]string{"secret": secret, "token": expiredToken})
			expect(t, w, 200)
			if decode[map[string]any](t, w)["success"] != false {
				t.Fatal("expired token accepted")
			}
			*enabled = false
			expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/challenge", map[string]string{"sitekey": site.Sitekey, "origin": "https://example.com"}), 403)
			expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/siteverify", map[string]string{"secret": secret, "token": value}), 403)
		})
	}
}
func TestTokenCannotCrossSiteAndRotation(t *testing.T) {
	m, s, _ := fixture(t)
	first := createTest(t, m, s, false)
	second := createTest(t, m, s, false)
	value := token(t, m, first)
	firstSecret := secretFor(t, m, s, first)
	secondSecret := secretFor(t, m, s, second)
	w := call(t, m.PublicHandler(), nil, "POST", "/pow/siteverify", map[string]string{"secret": secondSecret, "token": value})
	expect(t, w, 200)
	if decode[map[string]any](t, w)["success"] != false {
		t.Fatal("cross site token accepted")
	}
	w = call(t, m.Handler(), &s, "POST", "/sites/"+first.ID+"/regenerate-secret", map[string]bool{"confirm": true})
	expect(t, w, 200)
	newSecret := decode[map[string]string](t, w)["secret"]
	w = call(t, m.PublicHandler(), nil, "POST", "/pow/siteverify", map[string]string{"secret": firstSecret, "token": value})
	if decode[map[string]any](t, w)["success"] != false {
		t.Fatal("old secret accepted")
	}
	w = call(t, m.PublicHandler(), nil, "POST", "/pow/siteverify", map[string]string{"secret": newSecret, "token": value})
	if decode[map[string]any](t, w)["success"] != true {
		t.Fatal("new secret rejected")
	}
}
func TestRiskDomainAndTrustedProxies(t *testing.T) {
	m, s, _ := fixture(t)
	site := createTest(t, m, s, false)
	policy := site.Policy
	policy.RequestThreshold = 1
	expect(t, call(t, m.Handler(), &s, "PUT", "/sites/"+site.ID+"/ip-policy", policy), 200)
	one := getProof(t, m, site)
	two := getProof(t, m, site)
	if one.Difficulty != 1 || two.Difficulty != 2 {
		t.Fatalf("risk increment wrong %d %d", one.Difficulty, two.Difficulty)
	}
	for _, origin := range []string{"https://example.com.attacker.org", "https://example.net", "null", "javascript:alert(1)", "https://user@example.com"} {
		expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/challenge", map[string]string{"sitekey": site.Sitekey, "origin": origin}), 403)
	}
	r := httptest.NewRequest("POST", "/pow/challenge", nil)
	r.RemoteAddr = "10.1.2.3:4000"
	r.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.7")
	if m.clientIP(r) != "10.1.2.3" {
		t.Fatal("untrusted forwarding accepted")
	}
	m.trusted = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if m.clientIP(r) != "203.0.113.7" {
		t.Fatal("did not stop at nearest untrusted proxy")
	}
	r.Header.Set("X-Forwarded-For", "garbage")
	if m.clientIP(r) != "10.1.2.3" {
		t.Fatal("invalid forwarding accepted")
	}
}
func TestPublicWidgetAssets(t *testing.T) {
	m, _, _ := fixture(t)
	for _, path := range []string{"/weauth.js", "/worker.js", "/widget.html", "/widget.js"} {
		w := call(t, m.PublicHandler(), nil, "GET", path, nil)
		expect(t, w, 200)
		if w.Body.Len() < 100 {
			t.Fatalf("empty asset %s", path)
		}
	}
}
