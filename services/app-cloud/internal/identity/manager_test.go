package identity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This issuer signs actual RS256 tokens and enforces one-use authorization codes
// and S256 PKCE. It only exists in tests, never in the application login routes.
type testIssuer struct {
	server           *httptest.Server
	key              *rsa.PrivateKey
	mu               sync.Mutex
	codes            map[string]url.Values
	claims           func(map[string]any)
	profile          map[string]any
	badSignature     bool
	tokenRedirect    bool
	userinfoRedirect bool
	omitIDToken      bool
	metadataHSOnly   bool
	exchanges        int
	discoveryHits    int
}

func newIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &testIssuer{key: key, codes: map[string]url.Values{}, profile: map[string]any{"sub": "123456", "name": "Alice", "email": "alice@example.test", "email_verified": true}}
	i.server = httptest.NewTLSServer(http.HandlerFunc(i.serveHTTP))
	t.Cleanup(i.server.Close)
	return i
}

func (i *testIssuer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		i.discoveryHits++
		algs := []string{"RS256"}
		if i.metadataHSOnly {
			algs = []string{"HS256"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": i.server.URL, "authorization_endpoint": i.server.URL + "/authorize", "token_endpoint": i.server.URL + "/token", "userinfo_endpoint": i.server.URL + "/userinfo", "jwks_uri": i.server.URL + "/jwks", "id_token_signing_alg_values_supported": algs})
	case "/jwks":
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key", "n": base64.RawURLEncoding.EncodeToString(i.key.N.Bytes()), "e": "AQAB"}}})
	case "/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != "euler-client" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "invalid authorization", 400)
			return
		}
		code := fmt.Sprintf("code-%d", len(i.codes)+i.exchanges+1)
		i.codes[code] = q
		u, _ := url.Parse(q.Get("redirect_uri"))
		v := u.Query()
		v.Set("code", code)
		v.Set("state", q.Get("state"))
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), 302)
	case "/token":
		i.exchanges++
		if i.tokenRedirect {
			http.Redirect(w, r, i.server.URL+"/unexpected", 307)
			return
		}
		_ = r.ParseForm()
		client, secret, ok := r.BasicAuth()
		if !ok {
			client = r.Form.Get("client_id")
			secret = r.Form.Get("client_secret")
		}
		if client != "euler-client" || secret != "test-client-secret-not-real" {
			http.Error(w, "invalid client", 401)
			return
		}
		flow, ok := i.codes[r.Form.Get("code")]
		delete(i.codes, r.Form.Get("code"))
		hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != flow.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(hash[:]) != flow.Get("code_challenge") {
			http.Error(w, "invalid grant", 400)
			return
		}
		now := time.Now().Unix()
		claims := map[string]any{"iss": i.server.URL, "sub": "123456", "aud": "euler-client", "iat": now, "exp": now + 300, "nonce": flow.Get("nonce"), "name": "Alice", "email": "alice@example.test", "email_verified": true}
		if i.claims != nil {
			i.claims(claims)
		}
		body := map[string]any{"access_token": "issuer-access-token", "token_type": "Bearer", "expires_in": 300}
		if !i.omitIDToken {
			body["id_token"] = i.sign(claims)
		}
		_ = json.NewEncoder(w).Encode(body)
	case "/userinfo":
		if i.userinfoRedirect {
			http.Redirect(w, r, i.server.URL+"/unexpected", 302)
			return
		}
		if r.Header.Get("Authorization") != "Bearer issuer-access-token" {
			http.Error(w, "invalid bearer", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(i.profile)
	default:
		http.Error(w, "unexpected endpoint", 404)
	}
}

func (i *testIssuer) sign(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	hash := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, hash[:])
	if err != nil {
		panic(err)
	}
	if i.badSignature {
		sig[0] ^= 1
	}
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type memoryStore struct {
	mu       sync.Mutex
	users    map[string]Principal
	sessions map[string]Session
	flows    map[string]AuthFlow
}

func newStore() *memoryStore {
	return &memoryStore{users: map[string]Principal{}, sessions: map[string]Session{}, flows: map[string]AuthFlow{}}
}
func (s *memoryStore) UpsertIdentity(_ context.Context, e ExternalIdentity) (Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.users {
		if p.Provider == e.Provider && p.Subject == e.Subject {
			p.Email = e.Email
			p.Name = e.Name
			p.EmailVerified = e.EmailVerified
			s.users[id] = p
			return p, nil
		}
	}
	p := Principal{ID: strconv.Itoa(len(s.users) + 1), Provider: e.Provider, Subject: e.Subject, Name: e.Name, Email: e.Email, EmailVerified: e.EmailVerified}
	s.users[p.ID] = p
	return p, nil
}
func (s *memoryStore) GetPrincipal(_ context.Context, id string) (Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.users[id]
	if !ok {
		return p, ErrNotFound
	}
	return p, nil
}
func (s *memoryStore) PutSession(_ context.Context, v Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[v.TokenHash] = v
	return nil
}
func (s *memoryStore) GetSession(_ context.Context, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[id]
	if !ok {
		return v, ErrNotFound
	}
	return v, nil
}
func (s *memoryStore) DeleteSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}
func (s *memoryStore) PutAuthFlow(_ context.Context, v AuthFlow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flows[v.StateHash] = v
	return nil
}
func (s *memoryStore) ConsumeAuthFlow(_ context.Context, id string) (AuthFlow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.flows[id]
	delete(s.flows, id)
	if !ok {
		return v, ErrNotFound
	}
	return v, nil
}

func issuerConfig(i *testIssuer) Config {
	return Config{Issuer: i.server.URL, ClientID: "euler-client", ClientSecret: "test-client-secret-not-real", PublicOrigin: "https://cloud.test", RedirectURL: "https://cloud.test/auth/callback", HTTPClient: i.server.Client()}
}
func newManager(t *testing.T, i *testIssuer, s SessionStore, c Config) *Manager {
	t.Helper()
	m, err := New(context.Background(), c, s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func startLogin(t *testing.T, m *Manager, i *testIssuer, returnTo string) (string, *http.Cookie) {
	t.Helper()
	w := httptest.NewRecorder()
	m.Login(w, httptest.NewRequest("GET", "https://cloud.test/auth/login?returnTo="+url.QueryEscape(returnTo), nil))
	if w.Code != 302 {
		t.Fatalf("login %d: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing flow cookie")
	}
	client := i.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Fatalf("issuer rejected authorization: %d", resp.StatusCode)
	}
	return resp.Header.Get("Location"), cookies[0]
}

func finishLogin(m *Manager, callback string, flow *http.Cookie, old *http.Cookie) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", callback, nil)
	r.AddCookie(flow)
	if old != nil {
		r.AddCookie(old)
	}
	m.Callback(w, r)
	return w
}

func sessionCookie(t *testing.T, m *Manager, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == m.cookieName && c.Value != "" {
			return c
		}
	}
	t.Fatalf("missing session cookie: %d %s", w.Code, w.Body.String())
	return nil
}

func TestOIDCLoginSessionCSRFAndLogout(t *testing.T) {
	i := newIssuer(t)
	s := newStore()
	m := newManager(t, i, s, issuerConfig(i))
	callback, flow := startLogin(t, m, i, "/projects?view=mine")
	if !flow.Secure || !flow.HttpOnly || flow.SameSite != http.SameSiteLaxMode || flow.Path != "/" || flow.Domain != "" {
		t.Fatal("unsafe flow cookie")
	}
	w := finishLogin(m, callback, flow, nil)
	if w.Code != 303 || w.Header().Get("Location") != "/projects?view=mine" {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	cookie := sessionCookie(t, m, w)
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || !strings.HasPrefix(cookie.Name, "__Host-") {
		t.Fatal("unsafe session cookie")
	}
	if _, ok := s.sessions[cookie.Value]; ok {
		t.Fatal("raw bearer persisted")
	}
	r := httptest.NewRequest("GET", "https://cloud.test/api/v1/session", nil)
	r.AddCookie(cookie)
	p, session, err := m.Authenticate(r)
	if err != nil || p.Subject != "123456" || p.Provider != i.server.URL || !p.EmailVerified {
		t.Fatalf("principal=%+v error=%v", p, err)
	}
	for _, tc := range []struct {
		name, origin, csrf string
		want               bool
	}{{"valid", "https://cloud.test", session.CSRFToken, true}, {"cross-origin", "https://evil.test", session.CSRFToken, false}, {"missing-origin", "", session.CSRFToken, false}, {"missing-token", "https://cloud.test", "", false}, {"wrong-token", "https://cloud.test", "wrong", false}} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "https://cloud.test/auth/logout", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("X-CSRF-Token", tc.csrf)
			if (m.ValidateCSRF(req, session) == nil) != tc.want {
				t.Fatal("incorrect CSRF result")
			}
		})
	}
	crossSite := httptest.NewRequest("POST", "https://cloud.test/auth/logout", nil)
	crossSite.Header.Set("Origin", "https://cloud.test")
	crossSite.Header.Set("X-CSRF-Token", session.CSRFToken)
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	if m.ValidateCSRF(crossSite, session) == nil {
		t.Fatal("cross-site fetch metadata accepted")
	}
	req := httptest.NewRequest("POST", "https://cloud.test/auth/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set("Origin", "https://cloud.test")
	req.Header.Set("X-CSRF-Token", session.CSRFToken)
	out := httptest.NewRecorder()
	m.Logout(out, req)
	if out.Code != 204 {
		t.Fatalf("logout %d", out.Code)
	}
	if _, _, err = m.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("logout did not revoke session")
	}
	if out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("session response cached")
	}
}

func TestOIDCRejectsInvalidClaimsAndResponses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testIssuer)
	}{
		{"missing-exp", func(i *testIssuer) { i.claims = func(c map[string]any) { delete(c, "exp") } }},
		{"missing-iat", func(i *testIssuer) { i.claims = func(c map[string]any) { delete(c, "iat") } }},
		{"wrong-nonce", func(i *testIssuer) { i.claims = func(c map[string]any) { c["nonce"] = "forged" } }},
		{"missing-nonce", func(i *testIssuer) { i.claims = func(c map[string]any) { delete(c, "nonce") } }},
		{"wrong-issuer", func(i *testIssuer) { i.claims = func(c map[string]any) { c["iss"] = "https://evil.test" } }},
		{"wrong-audience", func(i *testIssuer) { i.claims = func(c map[string]any) { c["aud"] = "another-client" } }},
		{"multi-aud-no-azp", func(i *testIssuer) {
			i.claims = func(c map[string]any) { c["aud"] = []string{"euler-client", "another-client"} }
		}},
		{"wrong-azp", func(i *testIssuer) { i.claims = func(c map[string]any) { c["azp"] = "another-client" } }},
		{"expired", func(i *testIssuer) { i.claims = func(c map[string]any) { c["exp"] = time.Now().Unix() - 60 } }},
		{"future-iat", func(i *testIssuer) { i.claims = func(c map[string]any) { c["iat"] = time.Now().Unix() + 120 } }},
		{"old-iat", func(i *testIssuer) { i.claims = func(c map[string]any) { c["iat"] = time.Now().Unix() - 1200 } }},
		{"future-nbf", func(i *testIssuer) { i.claims = func(c map[string]any) { c["nbf"] = time.Now().Unix() + 120 } }},
		{"wrong-access-token-hash", func(i *testIssuer) { i.claims = func(c map[string]any) { c["at_hash"] = "incorrect" } }},
		{"empty-sub", func(i *testIssuer) { i.claims = func(c map[string]any) { c["sub"] = "" } }},
		{"userinfo-mismatch", func(i *testIssuer) { i.profile["sub"] = "another-user" }},
		{"bad-signature", func(i *testIssuer) { i.badSignature = true }},
		{"missing-id-token", func(i *testIssuer) { i.omitIDToken = true }},
		{"token-redirect", func(i *testIssuer) { i.tokenRedirect = true }},
		{"userinfo-redirect", func(i *testIssuer) { i.userinfoRedirect = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := newIssuer(t)
			tc.setup(i)
			s := newStore()
			m := newManager(t, i, s, issuerConfig(i))
			callback, flow := startLogin(t, m, i, "/")
			w := finishLogin(m, callback, flow, nil)
			if w.Code < 400 || len(s.sessions) != 0 || len(s.users) != 0 {
				t.Fatalf("invalid identity accepted: %d", w.Code)
			}
			if strings.Contains(w.Body.String(), "test-client-secret") || strings.Contains(w.Body.String(), "issuer-access-token") {
				t.Fatal("credentials reflected")
			}
		})
	}
}

func TestLoginStatePKCEAndCodeAreSingleUse(t *testing.T) {
	i := newIssuer(t)
	s := newStore()
	m := newManager(t, i, s, issuerConfig(i))
	callback, flow := startLogin(t, m, i, "/")
	forged := *flow
	forged.Value, _ = opaqueToken()
	if w := finishLogin(m, callback, &forged, nil); w.Code != 400 || i.exchanges != 0 {
		t.Fatal("cookie binding not checked before token exchange")
	}
	if w := finishLogin(m, callback+"&id_token=forged", flow, nil); w.Code != 400 || i.exchanges != 0 {
		t.Fatal("front-channel ID token accepted")
	}
	if w := finishLogin(m, callback+"&state="+flow.Value, flow, nil); w.Code != 400 || i.exchanges != 0 {
		t.Fatal("duplicate state parameter accepted")
	}
	duplicateCookie := httptest.NewRequest("GET", callback, nil)
	duplicateCookie.AddCookie(flow)
	duplicateCookie.AddCookie(flow)
	duplicateResult := httptest.NewRecorder()
	m.Callback(duplicateResult, duplicateCookie)
	if duplicateResult.Code != 400 || i.exchanges != 0 {
		t.Fatal("duplicate flow cookie accepted")
	}
	w := finishLogin(m, callback, flow, nil)
	if w.Code != 303 {
		t.Fatalf("valid login rejected: %s", w.Body.String())
	}
	if w := finishLogin(m, callback, flow, nil); w.Code != 400 || i.exchanges != 1 {
		t.Fatal("state replay exchanged the code")
	}
	callback2, flow2 := startLogin(t, m, i, "/")
	s.mu.Lock()
	v := s.flows[HashToken(flow2.Value)]
	v.Verifier = "wrong-verifier"
	s.flows[v.StateHash] = v
	s.mu.Unlock()
	if w := finishLogin(m, callback2, flow2, nil); w.Code < 400 {
		t.Fatal("PKCE mismatch accepted")
	}
}

func TestExpiredFlowAndSessionAndSessionRotation(t *testing.T) {
	i := newIssuer(t)
	s := newStore()
	m := newManager(t, i, s, issuerConfig(i))
	callback, flow := startLogin(t, m, i, "/")
	s.mu.Lock()
	v := s.flows[HashToken(flow.Value)]
	v.ExpiresAt = time.Now().Add(-time.Second)
	s.flows[v.StateHash] = v
	s.mu.Unlock()
	if w := finishLogin(m, callback, flow, nil); w.Code != 400 || i.exchanges != 0 {
		t.Fatal("expired flow accepted")
	}
	callback, flow = startLogin(t, m, i, "/")
	first := sessionCookie(t, m, finishLogin(m, callback, flow, nil))
	callback, flow = startLogin(t, m, i, "/")
	second := sessionCookie(t, m, finishLogin(m, callback, flow, first))
	if first.Value == second.Value || len(s.sessions) != 1 {
		t.Fatal("session was not rotated")
	}
	old := httptest.NewRequest("GET", "https://cloud.test/api/v1/session", nil)
	old.AddCookie(first)
	if _, _, err := m.Authenticate(old); err == nil {
		t.Fatal("old cookie remained valid")
	}
	s.mu.Lock()
	session := s.sessions[HashToken(second.Value)]
	session.ExpiresAt = time.Now().Add(-time.Second)
	s.sessions[session.TokenHash] = session
	s.mu.Unlock()
	req := httptest.NewRequest("GET", "https://cloud.test/api/v1/session", nil)
	req.AddCookie(second)
	if _, _, err := m.Authenticate(req); err == nil {
		t.Fatal("expired session accepted")
	}
}

func TestExplicitOAuth2ModeUsesOnlyProtectedUserInfo(t *testing.T) {
	i := newIssuer(t)
	i.badSignature = true
	i.claims = func(c map[string]any) { c["sub"] = "untrusted-id-token-user" }
	c := issuerConfig(i)
	c.Mode = "oauth2_userinfo"
	c.AuthorizationEndpoint = i.server.URL + "/authorize"
	c.TokenEndpoint = i.server.URL + "/token"
	c.UserInfoEndpoint = i.server.URL + "/userinfo"
	c.TokenEndpointAuthMethod = "client_secret_post"
	s := newStore()
	m := newManager(t, i, s, c)
	callback, flow := startLogin(t, m, i, "/")
	w := finishLogin(m, callback, flow, nil)
	if w.Code != 303 || i.discoveryHits != 0 {
		t.Fatalf("explicit OAuth2 login failed or used OIDC discovery: %d", w.Code)
	}
	for _, p := range s.users {
		if p.Subject != "123456" {
			t.Fatal("unverified ID token was trusted")
		}
	}
	t.Run("missing-sub-does-not-fall-back-to-id-token", func(t *testing.T) {
		delete(i.profile, "sub")
		callback, flow := startLogin(t, m, i, "/")
		if w := finishLogin(m, callback, flow, nil); w.Code != 401 {
			t.Fatal("missing userinfo subject accepted")
		}
	})
	t.Run("explicit-legacy-id-envelope", func(t *testing.T) {
		i.profile = map[string]any{"code": 200, "data": map[string]any{"id": json.Number("9007199254740993"), "username": "Legacy", "email": "legacy@example.test"}}
		c.UserInfoSubjectClaim = "id"
		c.UserInfoEnvelope = "data"
		s2 := newStore()
		m2 := newManager(t, i, s2, c)
		callback, flow := startLogin(t, m2, i, "/")
		if w := finishLogin(m2, callback, flow, nil); w.Code != 303 {
			t.Fatalf("legacy mode rejected %s", w.Body.String())
		}
		for _, p := range s2.users {
			if p.Subject != "9007199254740993" || p.EmailVerified {
				t.Fatal("ID precision loss or invented email verification")
			}
		}
	})
}

func TestConfigNeverFallsBackToFakeAuthentication(t *testing.T) {
	i := newIssuer(t)
	base := issuerConfig(i)
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing-secret", func(c *Config) { c.ClientSecret = "" }},
		{"unknown-mode", func(c *Config) { c.Mode = "demo" }},
		{"insecure-issuer", func(c *Config) { c.Issuer = "http://issuer.example.test" }},
		{"loopback-requires-explicit-flag", func(c *Config) { c.Issuer = "http://127.0.0.1:1234" }},
		{"flag-does-not-allow-insecure-remote", func(c *Config) { c.Issuer = "http://issuer.example.test"; c.AllowInsecureLoopback = true }},
		{"foreign-callback", func(c *Config) { c.RedirectURL = "https://evil.test/auth/callback" }},
		{"oauth2-missing-endpoints", func(c *Config) { c.Mode = "oauth2_userinfo" }},
		{"invalid-auth-style", func(c *Config) { c.TokenEndpointAuthMethod = "none" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			if _, err := New(context.Background(), c, newStore()); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	i.metadataHSOnly = true
	if _, err := New(context.Background(), base, newStore()); err == nil {
		t.Fatal("unverifiable OIDC issuer silently accepted")
	}
}

func TestReturnPathAndForgedSessionInputs(t *testing.T) {
	for _, path := range []string{"https://evil.test", "//evil.test", "/%2fevil.test", "/\\evil.test", "/%5cevil.test", "/%0aevil.test"} {
		if got := safeReturnTo(path); got != "/" {
			t.Fatalf("unsafe redirect %q -> %q", path, got)
		}
	}
	if got := safeReturnTo("/projects?p=1"); got != "/projects?p=1" {
		t.Fatal("local return path rejected")
	}
	i := newIssuer(t)
	s := newStore()
	m := newManager(t, i, s, issuerConfig(i))
	callback, flow := startLogin(t, m, i, "//evil.test")
	w := finishLogin(m, callback, flow, nil)
	if w.Header().Get("Location") != "/" {
		t.Fatal("open redirect")
	}
	cookie := sessionCookie(t, m, w)
	r := httptest.NewRequest("GET", "https://cloud.test/api/v1/session", nil)
	r.AddCookie(cookie)
	r.AddCookie(cookie)
	if _, _, err := m.Authenticate(r); err == nil {
		t.Fatal("ambiguous cookies accepted")
	}
	forged := httptest.NewRequest("GET", "https://cloud.test/api/v1/session", nil)
	forged.Header.Set("X-Euler-Account-Id", "1")
	forged.Header.Set("Authorization", "Bearer admin")
	if _, _, err := m.Authenticate(forged); err == nil {
		t.Fatal("client asserted identity accepted")
	}
}
