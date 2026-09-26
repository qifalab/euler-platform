package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureManager(t *testing.T, h http.HandlerFunc) (*Manager, string) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	m, err := New(Config{AllowedOrigins: []string{s.URL}, AllowInsecureHTTP: true, AllowPrivateNetwork: true, Timeout: time.Second, VerificationProvider: "issuer", EIDBaseURL: s.URL, TrustBaseURL: s.URL, TrustSchemeID: "3"})
	if err != nil {
		t.Fatal(err)
	}
	return m, s.URL
}
func respond(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func TestWeAuthContractAndAccountRevalidation(t *testing.T) {
	var deleted, created bool
	var account atomic.Int64
	account.Store(7)
	m, base := fixtureManager(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-token" {
			t.Error("missing upstream bearer")
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/auth/me":
			respond(w, map[string]any{"id": account.Load(), "username": "alice"})
		case "GET /api/admin/sites":
			respond(w, []any{map[string]any{"id": 12, "name": "现有站点", "enabled": true, "secret": "never-return-me"}})
		case "POST /api/admin/sites":
			var input CreateResource
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Name != "Euler" || len(input.Domains) != 1 || input.Domains[0] != "example.com" {
				t.Errorf("wrong creation body: %+v", input)
			}
			created = true
			w.WriteHeader(201)
			respond(w, map[string]any{"id": 13, "name": input.Name, "enabled": true, "secret": "new-secret"})
		case "DELETE /api/admin/sites/13":
			deleted = true
			respond(w, map[string]string{"message": "Site deleted"})
		default:
			t.Errorf("unexpected path %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()
	c, err := m.ValidateConnectionContext(ctx, "weauth", Connection{BaseURL: base, Credential: "user-token", ExternalAccountID: "forged"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ExternalAccountID != base+"#7" {
		t.Fatalf("unverified binding %q", c.ExternalAccountID)
	}
	items, err := m.Resources(ctx, "weauth", c)
	if err != nil || len(items) != 1 || items[0].ID != "12" {
		t.Fatalf("list %v %v", items, err)
	}
	resource, err := m.Create(ctx, "weauth", c, CreateResource{Name: "Euler", Domains: []string{"example.com"}})
	if err != nil || resource.ID != "13" || !created {
		t.Fatalf("create %v %v", resource, err)
	}
	body, _ := json.Marshal(resource)
	if strings.Contains(string(body), "secret") {
		t.Fatal("creation leaked secret")
	}
	if err = m.Delete(ctx, "weauth", c, resource.ID); err != nil || !deleted {
		t.Fatalf("delete %v", err)
	}
	account.Store(9)
	_, err = m.Resources(ctx, "weauth", c)
	if e, ok := err.(*Error); !ok || e.Code != "account_mismatch" {
		t.Fatalf("credential changed owner: %v", err)
	}
}

func TestDatabaseProjectionAndStorageKeyContract(t *testing.T) {
	m, base := fixtureManager(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/me":
			if r.Header.Get("Authorization") != "Bearer db-session" {
				t.Error("db bearer missing")
			}
			respond(w, map[string]any{"id": "db-account"})
		case "/api/databases":
			respond(w, map[string]any{"databases": []any{map[string]any{"id": "db-1", "name": "app", "db_type": "postgresql", "status": "active", "connection_info": map[string]string{"password": "supersecret", "username": "private-user"}}}, "total": 1})
		case "/api/s3/user/quota":
			if r.Header.Get("X-Access-Key") != "account-key" || r.Header.Get("X-Secret-Key") != "secret-key" || r.Header.Get("Authorization") != "" {
				t.Error("wrong key headers")
			}
			respond(w, map[string]any{"user_id": 3, "storage_quota": 4096, "storage_used": 1024, "bucket_count": 1})
		case "/api/s3/buckets":
			respond(w, map[string]any{"buckets": []any{map[string]any{"name": "images", "display_name": "图片", "access_type": "private"}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()
	db, err := m.ValidateConnection("database", Connection{BaseURL: base, Credential: "db-session"})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := m.Resources(ctx, "database", db)
	if err != nil || len(resources) != 1 {
		t.Fatalf("db %v %v", resources, err)
	}
	body, _ := json.Marshal(resources)
	for _, secret := range []string{"supersecret", "private-user", "connection_info"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	storage, err := m.ValidateConnection("storage", Connection{BaseURL: base, Credential: `{"kind":"access_key_pair","accessKey":"account-key","secretKey":"secret-key"}`})
	if err != nil {
		t.Fatal(err)
	}
	resources, err = m.Resources(ctx, "storage", storage)
	if err != nil || len(resources) != 1 || resources[0].ID != "images" {
		t.Fatalf("buckets %v %v", resources, err)
	}
	summary := m.Summary(ctx, "storage", storage, Subject{})
	if summary.State != "ready" || len(summary.Metrics) != 3 || summary.Metrics[1].Value != "1024" {
		t.Fatalf("quota %+v", summary)
	}
	if _, err = m.ValidateConnection("storage", Connection{BaseURL: base, Credential: "administrator-session"}); err == nil {
		t.Fatal("storage accepted admin session")
	}
}

func TestVerificationUsesConfiguredProviderAndVerifiedSubject(t *testing.T) {
	var calls atomic.Int64
	m, base := fixtureManager(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected credential")
		}
		switch r.URL.Path {
		case "/api/latest-verification/":
			if r.URL.Query().Get("oauth_id") != "42" {
				t.Error("wrong EID subject")
			}
			respond(w, map[string]string{"status": "approved", "identity_type": "core"})
		case "/api/verification/status":
			if r.URL.Query().Get("oauthId") != "42" || r.URL.Query().Get("schemeId") != "3" {
				t.Error("wrong trust subject/scheme")
			}
			respond(w, map[string]any{"success": true, "data": map[string]any{"verified": false, "status": "pending", "verificationData": "never-project"}})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	})
	for _, app := range []string{"eid", "trust"} {
		s := m.Summary(context.Background(), app, Connection{BaseURL: "https://attacker.invalid", ExternalAccountID: "someone-else"}, Subject{Provider: "issuer", ID: "42"})
		if s.State != "ready" || s.Verification == nil || s.ConsoleURL != base {
			t.Fatalf("%s %+v", app, s)
		}
		body, _ := json.Marshal(s)
		if strings.Contains(string(body), "never-project") {
			t.Fatal("material leaked")
		}
	}
	before := calls.Load()
	s := m.Summary(context.Background(), "eid", Connection{}, Subject{Provider: "another-issuer", ID: "42"})
	if s.State != "unsupported" || calls.Load() != before {
		t.Fatal("queried a subject from the wrong provider")
	}
}

func TestErrorsNeverBecomeVerifiedOrLeakBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"server-error", 500, `{"password":"secret"}`, "unavailable"},
		{"invalid-json", 200, `<html>secret</html>`, "unavailable"},
		{"missing-status", 200, `{"success":true,"data":{}}`, "unavailable"},
		{"inconsistent-status", 200, `{"success":true,"data":{"verified":true,"status":"rejected"}}`, "unavailable"},
		{"no-record", 404, `{"message":"用户不存在"}`, "ready"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := fixtureManager(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			s := m.Summary(context.Background(), "trust", Connection{}, Subject{Provider: "issuer", ID: "42"})
			if s.State != tc.want {
				t.Fatalf("%+v", s)
			}
			if s.Verification != nil && s.Verification.Status == "approved" {
				t.Fatal("false verification")
			}
			body, _ := json.Marshal(s)
			if strings.Contains(string(body), "secret") {
				t.Fatal("upstream body leaked")
			}
		})
	}
}

func TestIndependentInstanceOnlyProbesWitShieldReadiness(t *testing.T) {
	var calls atomic.Int64
	m, base := fixtureManager(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/readyz" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected privileged call")
		}
		respond(w, map[string]string{"status": "ok"})
	})
	for _, app := range []string{"statistics", "lottery"} {
		c, err := m.ValidateConnection(app, Connection{BaseURL: base})
		if err != nil {
			t.Fatal(err)
		}
		s := m.Summary(context.Background(), app, c, Subject{})
		if s.State != "unsupported" {
			t.Fatalf("fake business summary %+v", s)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("probed unrelated external pages")
	}
	c, err := m.ValidateConnection("witshield", Connection{BaseURL: base})
	if err != nil {
		t.Fatal(err)
	}
	s := m.Summary(context.Background(), "witshield", c, Subject{})
	if s.State != "ready" || calls.Load() != 1 {
		t.Fatalf("readiness %+v", s)
	}
	if _, err = m.ValidateConnection("witshield", Connection{BaseURL: base, Credential: "admin-session"}); err == nil {
		t.Fatal("accepted privileged WitShield credential")
	}
}

func TestOutboundPolicyAndRedirectAreEnforced(t *testing.T) {
	var reached atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1); respond(w, map[string]int{"id": 1}) }))
	defer target.Close()
	m, base := fixtureManager(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) })
	_, err := m.ValidateConnection("weauth", Connection{BaseURL: base, Credential: "secret"})
	if err == nil || reached.Load() != 0 {
		t.Fatal("followed upstream redirect")
	}
	for _, raw := range []string{"https://not-allowed.example", base + "/api", base + "?token=secret", base + "#fragment", "https://user:pass@example.com"} {
		if _, err = m.ValidateConnection("weauth", Connection{BaseURL: raw, Credential: "secret"}); err == nil {
			t.Errorf("allowed origin %s", raw)
		}
	}
	blocked, err := New(Config{AllowedOrigins: []string{target.URL}, AllowInsecureHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = blocked.ValidateConnection("weauth", Connection{BaseURL: target.URL, Credential: "secret"}); err == nil || reached.Load() != 0 {
		t.Fatal("private address accepted without operator configuration")
	}
	for _, ip := range []string{"127.0.0.1", "::1", "169.254.169.254", "10.0.0.1", "100.64.0.1", "::ffff:127.0.0.1", "2001:db8::1"} {
		if publicIP(net.ParseIP(ip)) {
			t.Errorf("unsafe network %s", ip)
		}
	}
}

func TestTimeoutResponseLimitAndUnsupportedMutations(t *testing.T) {
	m, base := fixtureManager(t, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, strings.Repeat("x", (2<<20)+1)) })
	if _, err := m.ValidateConnection("weauth", Connection{BaseURL: base, Credential: "token"}); err == nil {
		t.Fatal("accepted oversized response")
	}
	m2, base2 := fixtureManager(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m2.ValidateConnectionContext(ctx, "weauth", Connection{BaseURL: base2, Credential: "token"}); err == nil {
		t.Fatal("ignored deadline")
	}
	if _, err := m.Create(context.Background(), "database", Connection{}, CreateResource{Name: "fake"}); err == nil {
		t.Fatal("simulated database create")
	}
	if err := m.Delete(context.Background(), "weauth", Connection{}, "../other"); err == nil {
		t.Fatal("path injection accepted")
	}
	if _, err := m.Create(context.Background(), "weauth", Connection{}, CreateResource{Name: "x", Domains: []string{"https://example.com"}}); err == nil {
		t.Fatal("invalid domain accepted")
	}
}

func TestCatalogHasOnlyImplementedCapabilities(t *testing.T) {
	apps := Catalog()
	if len(apps) != 8 {
		t.Fatalf("catalog size %d", len(apps))
	}
	for _, app := range apps {
		if app.Repository == "" {
			t.Errorf("missing source for %s", app.ID)
		}
		for _, capability := range app.Capabilities {
			if capability == "resources:create" && app.ID != "weauth" {
				t.Errorf("unimplemented mutation %s", app.ID)
			}
		}
	}
}
