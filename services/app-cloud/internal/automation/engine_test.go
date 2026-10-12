package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/database"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/lottery"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/trust"
	_ "modernc.org/sqlite"
)

type fixture struct {
	rt       *appkit.Runtime
	engine   *Engine
	path     string
	trust    *trust.Module
	lottery  *lottery.Module
	database *database.Module
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("EULER_DATABASE_DISABLE_SCHEDULER", "true")
	f := &fixture{path: filepath.Join(t.TempDir(), "workflows.db")}
	f.rt = &appkit.Runtime{PublicURL: "https://euler.test", Encrypt: func(p, a string) ([]byte, error) { return []byte(p), nil }, Decrypt: func(b []byte, a string) (string, error) { return string(b), nil }, DeriveKey: func(string) []byte { return []byte("test-key") }}
	f.open(t)
	_, err := f.rt.DB.Exec(`CREATE TABLE installations(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,application_id TEXT,status TEXT);CREATE TABLE audit(id TEXT PRIMARY KEY,tenant_id TEXT,project_id TEXT,actor_id TEXT,action TEXT,target_id TEXT,summary TEXT,created_at INTEGER);CREATE TABLE permissions(actor TEXT,app TEXT,permission TEXT,PRIMARY KEY(actor,app,permission));CREATE TABLE memberships(actor TEXT PRIMARY KEY);INSERT INTO memberships VALUES('owner'),('alice');`)
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"trust", "database", "lottery"} {
		if _, err = f.rt.DB.Exec("INSERT INTO installations VALUES(?,?,?,?,?)", app, "tenant", "project", app, "enabled"); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"read", "write", "manage", "review", "admin"} {
			if _, err = f.rt.DB.Exec("INSERT INTO permissions VALUES(?,?,?)", "owner", app, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.trust = trust.New(f.rt)
	f.lottery = lottery.New(f.rt)
	f.database = database.New(f.rt)
	for _, m := range []appkit.Module{f.trust, f.lottery, f.database} {
		if err = m.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	f.engine = New(f.rt, f.resolve)
	if err = f.engine.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.rt.DB.Close() })
	return f
}
func (f *fixture) open(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	f.rt.DB = db
	if _, err = db.Exec("PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) resolve(ctx context.Context, tx *sql.Tx, actor, tenant, project, app string) (appkit.Scope, error) {
	s := scope(actor, app)
	var n int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM memberships WHERE actor=?", actor).Scan(&n)
	if err != nil {
		return s, err
	}
	if n == 0 || tenant != "tenant" || project != "project" {
		return s, appkit.Forbidden("项目访问已撤销")
	}
	var enabled bool
	if err = tx.QueryRowContext(ctx, "SELECT status='enabled' FROM installations WHERE application_id=?", app).Scan(&enabled); err != nil {
		return s, err
	}
	if !enabled {
		return s, appkit.Forbidden("应用已停用")
	}
	s.Permissions = []string{"read"}
	rows, err := tx.QueryContext(ctx, "SELECT permission FROM permissions WHERE actor=? AND app=?", actor, app)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			return s, err
		}
		s.Permissions = append(s.Permissions, p)
	}
	return s, rows.Err()
}
func scope(actor, app string) appkit.Scope {
	return appkit.Scope{ActorID: actor, ActorName: actor, Email: actor + "@example.test", TenantID: "tenant", ProjectID: "project", InstallationID: app, ApplicationID: app, Permissions: []string{"read", "write", "manage", "review", "admin"}}
}
func request(t *testing.T, m appkit.Module, actor, method, path, body string, status int) map[string]any {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(appkit.WithScope(r.Context(), scope(actor, m.ID())))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	var out map[string]any
	if status != 204 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}
func (f *fixture) prepare(t *testing.T) (scheme, template string) {
	t.Helper()
	v := request(t, f.trust, "owner", "POST", "/schemes", `{"name":"资格认证","status":"active","fields":[{"name":"name","label":"姓名","type":"text","required":true}]}`, 201)
	scheme = v["id"].(string)
	v = request(t, f.database, "owner", "POST", "/admin/templates", `{"name":"开发者资源","quota":{"mysqlMB":200,"mysqlCount":1},"days":1}`, 200)
	return scheme, v["id"].(string)
}
func (f *fixture) rule(t *testing.T, source, filter, action, template string) Rule {
	t.Helper()
	r, err := f.engine.Create(context.Background(), "owner", "tenant", "project", Rule{Name: "资格权益", Source: source, FilterID: filter, Action: action, TargetProjectID: "project", TargetApp: "database", TemplateID: template})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *fixture) approve(t *testing.T, scheme string) string {
	t.Helper()
	v := request(t, f.trust, "alice", "POST", "/submissions", fmt.Sprintf(`{"schemeId":%q,"schemeVersion":1,"data":{"name":"测试申请人"}}`, scheme), 201)
	id := v["id"].(string)
	request(t, f.trust, "owner", "POST", "/review/submissions/"+id, `{"status":"approved","version":1}`, 200)
	return id
}
func (f *fixture) tick(t *testing.T) {
	t.Helper()
	if err := f.engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := f.rt.DB.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestApprovalOutboxConcurrentRestartAndRevocation(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	f.rule(t, "trust.status", scheme, "package", template)
	id := f.approve(t, scheme)
	// Approval and outbox were committed together; restart before any dispatch.
	if err := f.rt.DB.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	f.engine = New(f.rt, f.resolve)
	if err := f.engine.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.engine.Tick(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 1 {
		t.Fatalf("duplicate or missing grant: %d", n)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM audit WHERE action='database.package.automation_granted'"); n != 1 {
		t.Fatalf("audit not atomic: %d", n)
	}
	request(t, f.trust, "owner", "POST", "/review/submissions/"+id, `{"status":"rejected","reason":"撤销资格","version":2}`, 200)
	f.tick(t)
	if n := f.count(t, "SELECT COUNT(*) FROM automation_entitlements WHERE revoked_at>0"); n != 1 {
		t.Fatalf("revocation missing: %d", n)
	}
	request(t, f.trust, "owner", "POST", "/review/submissions/"+id, `{"status":"approved","version":3}`, 200)
	f.tick(t)
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 1 {
		t.Fatalf("reapproval renewed once-only reward: %d", n)
	}
}
func TestPermissionRemovalDeadLetterThenExplicitRetry(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	f.rule(t, "trust.status", scheme, "package", template)
	f.approve(t, scheme)
	if _, err := f.rt.DB.Exec("DELETE FROM permissions WHERE actor='owner' AND app='database' AND permission='admin'"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 0 {
		t.Fatal("revoked admin executed resource grant")
	}
	xs, err := f.engine.Executions(context.Background(), "tenant", "project")
	if err != nil || len(xs) != 1 || xs[0].State != "failed" {
		t.Fatalf("missing failed history: %+v %v", xs, err)
	}
	if _, err = f.rt.DB.Exec("INSERT INTO permissions VALUES('owner','database','admin')"); err != nil {
		t.Fatal(err)
	}
	if err = f.engine.Retry(context.Background(), "owner", "tenant", "project", xs[0].ID); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 1 {
		t.Fatal("retry did not finish")
	}
}
func TestStaleApprovalNeverGrantsAndTemplateExpiry(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	f.rule(t, "trust.status", scheme, "package", template)
	id := f.approve(t, scheme)
	request(t, f.trust, "owner", "POST", "/review/submissions/"+id, `{"status":"rejected","reason":"撤销","version":2}`, 200)
	f.tick(t)
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 0 {
		t.Fatal("out-of-order approval granted after rejection")
	}
	request(t, f.trust, "owner", "POST", "/review/submissions/"+id, `{"status":"approved","version":3}`, 200)
	f.tick(t)
	var expires string
	if err := f.rt.DB.QueryRow("SELECT expires_at FROM database_grants").Scan(&expires); err != nil {
		t.Fatal(err)
	}
	expiry, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || time.Until(expiry) > 25*time.Hour || time.Until(expiry) < 23*time.Hour {
		t.Fatalf("lost template TTL: %s", expires)
	}
	if _, err = f.rt.DB.Exec("UPDATE database_grants SET expires_at=?", time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	v := request(t, f.database, "owner", "GET", "/packages", "", 200)
	quota := v["quota"].(map[string]any)
	if quota["mysqlMB"].(float64) != 100 {
		t.Fatal("expired grant still counted")
	}
}
func TestExpiredLeaseRecoverAndBusinessRollbackHasNoEvent(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	f.rule(t, "trust.status", scheme, "package", template)
	f.approve(t, scheme)
	if err := f.engine.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, err := f.engine.claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.rt.DB.Exec("UPDATE automation_executions SET lease_until=0 WHERE id=?", j.id); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if err = f.engine.execute(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 1 {
		t.Fatal("expired worker duplicated action")
	}
	tx, err := f.rt.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = appkit.Emit(context.Background(), tx, appkit.Event{TenantID: "tenant", ProjectID: "project", Source: "trust.status", SubjectID: "alice", ResourceID: "rolled-back", FilterID: scheme, Status: "approved", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if n := f.count(t, "SELECT COUNT(*) FROM automation_events WHERE id LIKE '%rolled-back%'"); n != 0 {
		t.Fatal("rolled-back event survived")
	}
}
func TestQualifiedActivityDrawNotificationAndEntitlement(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	f.approve(t, scheme)
	room := request(t, f.lottery, "owner", "POST", "/rooms", `{"name":"开发者活动"}`, 201)["id"].(string)
	request(t, f.lottery, "owner", "PUT", "/rooms/"+room+"/signup-policy", fmt.Sprintf(`{"trustSchemeId":%q,"requireLogin":true}`, scheme), 200)
	f.rule(t, "lottery.won", room, "notify", "")
	f.rule(t, "lottery.won", room, "package", template)
	request(t, f.lottery, "owner", "POST", "/rooms/"+room+"/signup", `{"name":"不满足资格者"}`, 403)
	request(t, f.lottery, "alice", "POST", "/rooms/"+room+"/signup", `{"name":"合格申请者","department":"研发"}`, 201)
	request(t, f.lottery, "alice", "POST", "/rooms/"+room+"/signup", `{"name":"重复报名"}`, 409)
	inv := request(t, f.lottery, "owner", "GET", "/rooms/"+room+"/invitation", "", 200)["signupURL"].(string)
	token := inv[strings.LastIndex(inv, "/")+1:]
	r := httptest.NewRequest("POST", "/join/"+token+"/register", strings.NewReader(`{"name":"匿名绕过者"}`))
	w := httptest.NewRecorder()
	f.lottery.PublicHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("public bypass allowed: %d %s", w.Code, w.Body.String())
	}
	drawBody := `{"count":1,"prizeName":"开发者权益","requestId":"draw-once-123"}`
	request(t, f.lottery, "owner", "POST", "/rooms/"+room+"/draws", drawBody, 200)
	request(t, f.lottery, "owner", "POST", "/rooms/"+room+"/draws", drawBody, 200)
	f.tick(t)
	notes, err := f.engine.Notifications(context.Background(), "alice", "tenant", "project")
	if err != nil || len(notes) != 1 {
		t.Fatalf("missing winner notification: %v %v", notes, err)
	}
	other, err := f.engine.Notifications(context.Background(), "owner", "tenant", "project")
	if err != nil || len(other) != 0 {
		t.Fatal("notification leaked across actor")
	}
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 1 {
		t.Fatal("winner entitlement missing or duplicated")
	}
}
func TestUnauthorizedRuleAndStoppedInstallation(t *testing.T) {
	f := newFixture(t)
	scheme, template := f.prepare(t)
	_, err := f.engine.Create(context.Background(), "alice", "tenant", "project", Rule{Name: "不能越权", Source: "trust.status", FilterID: scheme, Action: "package", TargetProjectID: "project", TargetApp: "database", TemplateID: template})
	if err == nil {
		t.Fatal("plain member created privilege-bearing rule")
	}
	f.rule(t, "trust.status", scheme, "package", template)
	f.approve(t, scheme)
	if _, err = f.rt.DB.Exec("UPDATE installations SET status='disabled' WHERE application_id='trust'"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if n := f.count(t, "SELECT COUNT(*) FROM database_grants"); n != 0 {
		t.Fatal("disabled source granted entitlement")
	}
}
