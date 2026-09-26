package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	_ "modernc.org/sqlite"
)

func fixture(t *testing.T) (*Module, appkit.Scope) {
	t.Helper()
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "stats.db"))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, e = db.Exec(`PRAGMA foreign_keys=ON;
CREATE TABLE installations(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,application_id TEXT,status TEXT);
INSERT INTO installations VALUES('install','tenant','project','statistics','enabled');
CREATE TABLE audit(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,actor_id TEXT,action TEXT,target_id TEXT,summary TEXT,created_at INTEGER);`)
	if e != nil {
		t.Fatal(e)
	}
	m := New(&appkit.Runtime{DB: db, PublicURL: "https://euler.example", DeriveKey: func(string) []byte { return []byte("test-only-hmac-key") }})
	if e = m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	return m, appkit.Scope{ActorID: "actor", TenantID: "tenant", ProjectID: "project", InstallationID: "install", ApplicationID: "statistics", Permissions: []string{"read", "manage", "write"}}
}
func call(t *testing.T, h http.Handler, s *appkit.Scope, method, path, body, origin string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if s != nil {
		r = r.WithContext(appkit.WithScope(r.Context(), *s))
	}
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func requireStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
}
func createSite(t *testing.T, m *Module, s appkit.Scope) site {
	t.Helper()
	w := call(t, m.Handler(), &s, "POST", "/sites", `{"name":"Docs","domains":["docs.example"]}`, "")
	requireStatus(t, w, 201)
	var v site
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func TestScopedCollectionAndReport(t *testing.T) {
	m, s := fixture(t)
	v := createSite(t, m, s)
	endpoint := "/sites/" + v.PublicID
	private := "/sites/" + v.ID
	for _, id := range []string{"visitor-0001", "visitor-0001", "visitor-0002"} {
		w := call(t, m.PublicHandler(), nil, "POST", endpoint+"/collect", `{"visitor_id":"`+id+`","url":"https://docs.example/a?token=secret#hash","referrer":"https://ref.example/?secret=x","page_title":"Docs"}`, "https://docs.example")
		requireStatus(t, w, 202)
	}
	w := call(t, m.Handler(), &s, "GET", private+"/report?page=1&pageSize=10", "", "")
	requireStatus(t, w, 200)
	var report struct {
		PageViews, UniqueVisitors int
		TopPages                  []struct{ URL string }
	}
	if e := json.Unmarshal(w.Body.Bytes(), &report); e != nil {
		t.Fatal(e)
	}
	if report.PageViews != 3 || report.UniqueVisitors != 2 || len(report.TopPages) != 1 || report.TopPages[0].URL != "https://docs.example/a" {
		t.Fatalf("unexpected report %s", w.Body.String())
	}
	var savedURL, ref, visitor string
	if e := m.rt.DB.QueryRow("SELECT url,referrer,visitor_hash FROM statistics_views LIMIT 1").Scan(&savedURL, &ref, &visitor); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(savedURL, "secret") || strings.Contains(ref, "secret") || strings.Contains(visitor, "visitor-") {
		t.Fatal("raw identifiers or URL credentials persisted")
	}
	requireStatus(t, call(t, m.PublicHandler(), nil, "GET", endpoint+"/page-views?url=https%3A%2F%2Fdocs.example%2Fa", "", "https://docs.example"), 200)
	for _, file := range []string{"tracker.js", "widget.js", "helper.js"} {
		w := call(t, m.PublicHandler(), nil, "GET", endpoint+"/"+file, "", "")
		requireStatus(t, w, 200)
		if !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
			t.Fatal("wrong script type")
		}
	}
}
func TestAuthorizationAndOriginBoundaries(t *testing.T) {
	m, s := fixture(t)
	v := createSite(t, m, s)
	path := "/sites/" + v.ID + "/report"
	requireStatus(t, call(t, m.Handler(), nil, "GET", path, "", ""), 401)
	other := s
	other.ProjectID = "other"
	requireStatus(t, call(t, m.Handler(), &other, "GET", path, "", ""), 404)
	other = s
	other.TenantID = "other"
	requireStatus(t, call(t, m.Handler(), &other, "PATCH", "/sites/"+v.ID, `{"name":"hijack","domains":["evil.example"]}`, ""), 404)
	viewer := s
	viewer.Permissions = []string{"read"}
	requireStatus(t, call(t, m.Handler(), &viewer, "POST", "/sites", `{"name":"x","domains":["a.example"]}`, ""), 403)
	body := `{"visitor_id":"visitor-001","url":"https://docs.example/a"}`
	for _, origin := range []string{"", "null", "https://evil.example", "https://docs.example.evil"} {
		requireStatus(t, call(t, m.PublicHandler(), nil, "POST", "/sites/"+v.PublicID+"/collect", body, origin), 403)
	}
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", "/sites/"+v.PublicID+"/collect", `{"visitor_id":"visitor-001","url":"https://other.example/a"}`, "https://docs.example"), 403)
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", "/sites/"+v.PublicID+"/collect", body[:len(body)-1]+`,"tenantId":"other"}`, "https://docs.example"), 400)
	if _, e := m.rt.DB.Exec("UPDATE installations SET status='disabled'"); e != nil {
		t.Fatal(e)
	}
	requireStatus(t, call(t, m.PublicHandler(), nil, "POST", "/sites/"+v.PublicID+"/collect", body, "https://docs.example"), 404)
}
func TestSiteWriteRollsBackWithAudit(t *testing.T) {
	m, s := fixture(t)
	if _, e := m.rt.DB.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'unavailable audit'); END;`); e != nil {
		t.Fatal(e)
	}
	requireStatus(t, call(t, m.Handler(), &s, "POST", "/sites", `{"name":"rollback","domains":["docs.example"]}`, ""), 500)
	var n int
	if e := m.rt.DB.QueryRow("SELECT COUNT(*) FROM statistics_sites").Scan(&n); e != nil || n != 0 {
		t.Fatalf("write escaped transaction: %d %v", n, e)
	}
}
func TestInvalidDomainRejected(t *testing.T) {
	for _, d := range []string{"*", "*.example.com", "https://example.com", "example.com/path", "example.com@evil.test", "example.com?x=1"} {
		in := siteInput{Name: "test", Domains: []string{d}}
		if validateSite(&in) == nil {
			t.Errorf("accepted domain %q", d)
		}
	}
}
