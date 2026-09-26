package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/connectors"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
)

// Test-only identity boundary. There is no equivalent identity selector in the server.
type actorAuth struct{ actor identity.Principal }

func (a actorAuth) Authenticate(r *http.Request) (identity.Principal, identity.Session, error) {
	if r.Header.Get("Authorization") != "Bearer test-session" {
		return identity.Principal{}, identity.Session{}, errors.New("no session")
	}
	return a.actor, identity.Session{CSRFToken: "test-csrf"}, nil
}
func (a actorAuth) ValidateCSRF(r *http.Request, s identity.Session) error {
	if r.Header.Get("X-CSRF-Token") != s.CSRFToken {
		return errors.New("csrf")
	}
	return nil
}
func call(h http.Handler, method, path, body string, auth, csrf bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth {
		r.Header.Set("Authorization", "Bearer test-session")
	}
	if csrf {
		r.Header.Set("X-CSRF-Token", "test-csrf")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func managerFor(t *testing.T, origins ...string) *connectors.Manager {
	t.Helper()
	m, e := connectors.New(connectors.Config{AllowedOrigins: origins, AllowInsecureHTTP: true, AllowPrivateNetwork: true})
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func installPath(t, p, id string) string {
	return fmt.Sprintf("/api/v1/tenants/%s/projects/%s/installations/%s", t, p, id)
}

func TestHTTPIdentityCSRFFencesAndStrictInput(t *testing.T) {
	s := testStore(t)
	alice := testUser(t, s, "alice")
	eve := testUser(t, s, "eve")
	team := mustTenant(t, s, eve.ID)
	h := NewHandler(s, actorAuth{alice}, managerFor(t))
	if w := call(h, "POST", "/api/v1/tenants", `{"name":"Team"}`, false, true); w.Code != 401 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call(h, "POST", "/api/v1/tenants", `{"name":"Team"}`, true, false); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call(h, "POST", "/api/v1/tenants", `{"name":"Team","userId":"`+eve.ID+`"}`, true, true); w.Code != 400 {
		t.Fatal("caller supplied actor accepted", w.Code)
	}
	if w := call(h, "POST", "/api/v1/tenants", `{"name":"Team"} {}`, true, true); w.Code != 400 {
		t.Fatal("trailing JSON accepted", w.Code)
	}
	if w := call(h, "GET", "/api/v1/tenants/"+team.ID+"/members", "", true, false); w.Code != 404 {
		t.Fatal("cross tenant enumeration", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/v1/tenants", nil)
	r.Header.Set("X-Euler-Account-Id", eve.ID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("forged legacy identity header accepted")
	}
	if w := call(NewHandler(s, nil, managerFor(t)), "GET", "/api/v1/session", "", false, false); w.Code != 200 || !strings.Contains(w.Body.String(), `"loginAvailable":false`) {
		t.Fatal("unconfigured login not explicit", w.Body.String())
	}
}

func TestUnsupportedResourceActionReturns501WithoutOperation(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s, "u")
	team := mustTenant(t, s, u.ID)
	p := mustProject(t, s, u.ID, team.ID)
	app, e := s.EnableApplication(testContext, u.ID, team.ID, p.ID, "trust")
	if e != nil {
		t.Fatal(e)
	}
	h := NewHandler(s, actorAuth{u}, managerFor(t))
	base := installPath(team.ID, p.ID, app.ID) + "/resources"
	for _, req := range []struct{ method, path, body string }{{"POST", base, `{"name":"Unsupported","domains":[]}`}, {"DELETE", base + "/unknown", ""}} {
		w := call(h, req.method, req.path, req.body, true, true)
		if w.Code != 501 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	var n int
	if e = s.db.QueryRow("SELECT count(*) FROM operations").Scan(&n); e != nil || n != 0 {
		t.Fatal("unsupported action recorded as attempted mutation", n, e)
	}
}
func TestHTTPVerifiedAccountOwnershipAndCredentialOrigin(t *testing.T) {
	var foreignCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/auth/me" {
			_, _ = w.Write([]byte(`{"id":7}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls.Add(1); w.WriteHeader(500) }))
	defer foreign.Close()
	s := testStore(t)
	a := testUser(t, s, "alice")
	team := mustTenant(t, s, a.ID)
	p1 := mustProject(t, s, a.ID, team.ID)
	p2 := mustProject(t, s, a.ID, team.ID)
	i1 := mustApp(t, s, a.ID, team.ID, p1.ID)
	i2 := mustApp(t, s, a.ID, team.ID, p2.ID)
	h := NewHandler(s, actorAuth{a}, managerFor(t, upstream.URL, foreign.URL))
	b, _ := json.Marshal(map[string]string{"baseUrl": upstream.URL, "credential": "private-token-a", "externalAccountId": "arbitrary-client-value"})
	w := call(h, "PUT", installPath(team.ID, p1.ID, i1.ID)+"/connection", string(b), true, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-token-a") {
		t.Fatal("connection response leaked secret")
	}
	_, c, e := s.ConnectionFor(testContext, a.ID, team.ID, p1.ID, i1.ID, "admin")
	if e != nil || !strings.HasSuffix(c.ExternalAccountID, "#7") {
		t.Fatal("unverified account persisted", c.ExternalAccountID, e)
	}
	b, _ = json.Marshal(map[string]string{"baseUrl": upstream.URL, "credential": "private-token-b", "externalAccountId": "different-user-8"})
	w = call(h, "PUT", installPath(team.ID, p2.ID, i2.ID)+"/connection", string(b), true, true)
	if w.Code != 409 {
		t.Fatal("two tokens for same upstream account split projects", w.Code, w.Body.String())
	}
	b, _ = json.Marshal(map[string]string{"baseUrl": foreign.URL})
	w = call(h, "PUT", installPath(team.ID, p1.ID, i1.ID)+"/connection", string(b), true, true)
	if w.Code != 400 || foreignCalls.Load() != 0 {
		t.Fatal("old credential forwarded to different origin", w.Code, foreignCalls.Load())
	}
}
func TestRemoteMutationReceiptSurvivesInFlightRevocation(t *testing.T) {
	s := testStore(t)
	owner := testUser(t, s, "owner")
	member := testUser(t, s, "member")
	team := mustTenant(t, s, owner.ID)
	project := mustProject(t, s, owner.ID, team.ID)
	invite(t, s, owner.ID, team.ID, member.ID, "member")
	if e := s.SetProjectMember(testContext, owner.ID, team.ID, project.ID, member.ID, "member", false); e != nil {
		t.Fatal(e)
	}
	install := mustApp(t, s, owner.ID, team.ID, project.ID)
	mutated := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/auth/me" {
			_, _ = w.Write([]byte(`{"id":7}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/admin/sites" {
			close(mutated)
			<-release
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":99,"name":"Created upstream","enabled":true,"secret":"must-not-reach-browser"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()
	m := managerFor(t, upstream.URL)
	c, e := m.ValidateConnection("weauth", connectors.Connection{BaseURL: upstream.URL, Credential: "test-token"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SaveConnection(testContext, owner.ID, team.ID, project.ID, install.ID, Connection{BaseURL: c.BaseURL, Credential: c.Credential, ExternalAccountID: c.ExternalAccountID}); e != nil {
		t.Fatal(e)
	}
	h := NewHandler(s, actorAuth{member}, m)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", installPath(team.ID, project.ID, install.ID)+"/resources", bytes.NewBufferString(`{"name":"Created upstream","domains":["example.test"]}`))
	r.Header.Set("Authorization", "Bearer test-session")
	r.Header.Set("X-CSRF-Token", "test-csrf")
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, r); close(done) }()
	select {
	case <-mutated:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("no upstream mutation")
	}
	if e = s.ChangeMember(testContext, owner.ID, team.ID, member.ID, "", true); e != nil {
		close(release)
		t.Fatal(e)
	}
	close(release)
	<-done
	if w.Code != 201 || strings.Contains(w.Body.String(), "must-not-reach-browser") {
		t.Fatal(w.Code, w.Body.String())
	}
	if e = s.RequireResource(testContext, owner.ID, team.ID, project.ID, install.ID, "99"); e != nil {
		t.Fatal("lost resource receipt", e)
	}
	events, e := s.Audit(testContext, owner.ID, team.ID, project.ID, 100)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, a := range events {
		if a.Action == "resource.create_completed" {
			found = true
		}
	}
	if !found {
		t.Fatal("lost audit after revocation")
	}
	if w = call(h, "POST", installPath(team.ID, project.ID, install.ID)+"/resources", `{"name":"Denied","domains":["example.test"]}`, true, true); w.Code != 404 {
		t.Fatal("new request survived revocation", w.Code)
	}
}
