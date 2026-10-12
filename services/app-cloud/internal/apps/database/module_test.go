package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	_ "modernc.org/sqlite"
)

type fakeDatabase struct {
	user, password string
	size           int64
	readOnly       bool
}
type fakeEngine struct {
	mu                                     sync.Mutex
	resources                              map[string]fakeDatabase
	creates, rotates, drops, readonlyCalls int
	fail                                   error
	afterCreate                            func()
}

func newFakeEngine() *fakeEngine { return &fakeEngine{resources: map[string]fakeDatabase{}} }
func (e *fakeEngine) Create(ctx context.Context, db, user, password string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.creates++
	e.resources[db] = fakeDatabase{user: user, password: password}
	if e.afterCreate != nil {
		e.afterCreate()
	}
	return e.fail
}
func (e *fakeEngine) Drop(ctx context.Context, db, user string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.drops++
	if e.fail != nil {
		return e.fail
	}
	delete(e.resources, db)
	return nil
}
func (e *fakeEngine) Rotate(ctx context.Context, db, user, password string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rotates++
	v := e.resources[db]
	v.password = password
	e.resources[db] = v
	return e.fail
}
func (e *fakeEngine) Size(ctx context.Context, db string) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resources[db].size, e.fail
}
func (e *fakeEngine) ReadOnly(ctx context.Context, db, user string, ro bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readonlyCalls++
	if e.fail != nil {
		return e.fail
	}
	v := e.resources[db]
	v.readOnly = ro
	e.resources[db] = v
	return nil
}
func (e *fakeEngine) Repair(ctx context.Context, db, user string) error {
	return e.ReadOnly(ctx, db, user, false)
}
func (e *fakeEngine) state(db string) fakeDatabase {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resources[db]
}
func (e *fakeEngine) size(db string, n int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.resources[db]
	v.size = n
	e.resources[db] = v
}

func databaseFixture(t *testing.T) (*Module, appkit.Scope, *fakeEngine, string) {
	t.Helper()
	t.Setenv("EULER_DATABASE_DISABLE_SCHEDULER", "true")
	path := filepath.Join(t.TempDir(), "platform.sqlite")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`CREATE TABLE audit(id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,actor_id TEXT NOT NULL,action TEXT NOT NULL,target_id TEXT NOT NULL,summary TEXT NOT NULL,created_at INTEGER NOT NULL);`)
	if e != nil {
		t.Fatal(e)
	}
	key := make([]byte, 32)
	if _, e = rand.Read(key); e != nil {
		t.Fatal(e)
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		t.Fatal(e)
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		t.Fatal(e)
	}
	rt := &appkit.Runtime{DB: db, ApplicationEnabled: func(context.Context, string, string, string) (bool, error) { return true, nil }, Encrypt: func(p, a string) ([]byte, error) {
		nonce := make([]byte, aead.NonceSize())
		if _, e := rand.Read(nonce); e != nil {
			return nil, e
		}
		return aead.Seal(nonce, nonce, []byte(p), []byte(a)), nil
	}, Decrypt: func(c []byte, a string) (string, error) {
		if len(c) < aead.NonceSize() {
			return "", errors.New("short ciphertext")
		}
		value, e := aead.Open(nil, c[:aead.NonceSize()], c[aead.NonceSize():], []byte(a))
		return string(value), e
	}}
	fake := newFakeEngine()
	m := &Module{rt: rt, engines: map[string]engineConfig{"mysql": {Engine: fake, Host: "database.invalid", Port: 3306}, "postgresql": {Engine: fake, Host: "database.invalid", Port: 5432}}}
	m.ledger = ledger{rt: rt, prefix: "database", base: Quota{MySQLMB: 100, MySQLCount: 1, PostgresMB: 100, PostgresCount: 1}}
	if e = m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = m.Close(); _ = rt.DB.Close() })
	s := appkit.Scope{ActorID: "actor", TenantID: "tenant", ProjectID: "project", InstallationID: "installation", ApplicationID: "database", Permissions: []string{"read", "write", "manage", "secrets", "admin"}}
	return m, s, fake, path
}
func databaseCall(h http.Handler, s *appkit.Scope, method, path string, body any) *httptest.ResponseRecorder {
	var encoded []byte
	if body != nil {
		encoded, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(encoded)))
	r.Header.Set("Content-Type", "application/json")
	if s != nil {
		r = r.WithContext(appkit.WithScope(r.Context(), *s))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func databaseStatus(t *testing.T, w *httptest.ResponseRecorder, expected int) {
	t.Helper()
	if w.Code != expected {
		t.Fatalf("status=%d expected=%d response=%s", w.Code, expected, w.Body.String())
	}
}
func decodeResponse[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func createDatabase(t *testing.T, m *Module, s appkit.Scope, kind string) Resource {
	t.Helper()
	w := databaseCall(m.Handler(), &s, "POST", "/databases", map[string]any{"name": "Test data", "type": kind})
	databaseStatus(t, w, 201)
	return decodeResponse[Resource](t, w)
}
func makeTemplate(t *testing.T, m *Module, s appkit.Scope, q Quota) Template {
	t.Helper()
	w := databaseCall(m.Handler(), &s, "POST", "/admin/templates", Template{Name: "资源包", Description: "测试资源包", Quota: q, Days: 30})
	databaseStatus(t, w, 200)
	return decodeResponse[Template](t, w)
}
func generateCodes(t *testing.T, m *Module, s appkit.Scope, tpl Template, count, uses int) []Code {
	t.Helper()
	w := databaseCall(m.Handler(), &s, "POST", "/admin/codes", map[string]any{"templateId": tpl.ID, "count": count, "maxUses": uses})
	databaseStatus(t, w, 201)
	return decodeResponse[struct {
		Items []Code `json:"items"`
	}](t, w).Items
}

func TestDatabaseResourcePermissionsAndCredentialBoundary(t *testing.T) {
	m, s, fake, _ := databaseFixture(t)
	v := createDatabase(t, m, s, "mysql")
	h := m.Handler()
	path := "/databases/" + v.ID
	databaseStatus(t, databaseCall(h, nil, "GET", path, nil), 401)
	viewer := s
	viewer.Permissions = []string{"read"}
	for _, op := range []struct{ method, path string }{{"POST", "/databases"}, {"GET", path + "/credentials"}, {"POST", path + "/password"}, {"DELETE", path}, {"GET", "/admin/operations"}} {
		databaseStatus(t, databaseCall(h, &viewer, op.method, op.path, map[string]string{"name": "denied", "type": "mysql"}), 403)
	}
	projectAdmin := s
	projectAdmin.Permissions = []string{"read", "write", "manage", "secrets"}
	databaseStatus(t, databaseCall(h, &projectAdmin, "POST", "/admin/check-quota", nil), 403)
	databaseStatus(t, databaseCall(h, &projectAdmin, "POST", "/admin/templates", Template{Name: "no grant", Days: 1}), 403)
	other := s
	other.ProjectID = "another-project"
	for _, op := range []struct{ method, path string }{{"GET", path}, {"GET", path + "/credentials"}, {"POST", path + "/password"}, {"DELETE", path}} {
		databaseStatus(t, databaseCall(h, &other, op.method, op.path, nil), 404)
	}
	other = s
	other.TenantID = "another-tenant"
	databaseStatus(t, databaseCall(h, &other, "GET", path, nil), 404)
	w := databaseCall(h, &s, "GET", path+"/credentials", nil)
	databaseStatus(t, w, 200)
	secret := decodeResponse[struct {
		Password string `json:"password"`
		Username string `json:"username"`
	}](t, w)
	if secret.Password == "" || fake.state(v.Database).password != secret.Password {
		t.Fatal("missing or incorrect real engine credential")
	}
	var cipherBytes []byte
	if e := m.rt.DB.QueryRow("SELECT credential FROM database_instances WHERE id=?", v.ID).Scan(&cipherBytes); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(cipherBytes), secret.Password) {
		t.Fatal("password stored in plaintext")
	}
	for _, path := range []string{"/databases", "/databases/" + v.ID, "/quota", "/admin/operations"} {
		w = databaseCall(h, &s, "GET", path, nil)
		databaseStatus(t, w, 200)
		if strings.Contains(w.Body.String(), secret.Password) || strings.Contains(w.Body.String(), "credential") {
			t.Fatalf("secret projected by %s", path)
		}
	}
	rows, e := m.rt.DB.Query("SELECT summary FROM audit")
	if e != nil {
		t.Fatal(e)
	}
	for rows.Next() {
		var summary string
		if e = rows.Scan(&summary); e != nil {
			t.Fatal(e)
		}
		if strings.Contains(summary, secret.Password) {
			t.Fatal("audit leaked credential")
		}
	}
	rows.Close()
	databaseStatus(t, databaseCall(h, &s, "POST", path+"/password", nil), 200)
	w = databaseCall(h, &s, "GET", path+"/credentials", nil)
	rotated := decodeResponse[struct {
		Password string `json:"password"`
	}](t, w)
	if rotated.Password == secret.Password || fake.state(v.Database).password != rotated.Password {
		t.Fatal("rotation did not synchronize engine and encrypted state")
	}
	databaseStatus(t, databaseCall(h, &s, "DELETE", path, nil), 200)
	databaseStatus(t, databaseCall(h, &s, "GET", path, nil), 404)
	if fake.drops != 1 {
		t.Fatal("engine drop not executed")
	}
}

func TestConcurrentCreationReservesQuota(t *testing.T) {
	m, s, fake, _ := databaseFixture(t)
	h := m.Handler()
	responses := make(chan *httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses <- databaseCall(h, &s, "POST", "/databases", map[string]string{"name": "concurrent", "type": "mysql"})
		}()
	}
	wg.Wait()
	close(responses)
	created := 0
	for w := range responses {
		if w.Code == 201 {
			created++
		} else {
			databaseStatus(t, w, 409)
		}
	}
	if created != 1 || fake.creates != 1 {
		t.Fatalf("quota oversubscribed: created=%d engineCalls=%d", created, fake.creates)
	}
	var n int
	if e := m.rt.DB.QueryRow("SELECT COUNT(*) FROM database_instances WHERE status='active'").Scan(&n); e != nil || n != 1 {
		t.Fatalf("unexpected persisted reservations %d %v", n, e)
	}
}

func TestPackageConcurrentRedemptionScopeAndSnapshot(t *testing.T) {
	m, s, _, _ := databaseFixture(t)
	h := m.Handler()
	tpl := makeTemplate(t, m, s, Quota{MySQLMB: 200, MySQLCount: 3})
	code := generateCodes(t, m, s, tpl, 1, 1)[0]
	outsider := s
	outsider.TenantID = "other"
	databaseStatus(t, databaseCall(h, &outsider, "POST", "/packages/redeem", map[string]string{"code": code.Code}), 400)
	responses := make(chan *httptest.ResponseRecorder, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recipient := s
			recipient.ProjectID = fmt.Sprintf("recipient-%d", i)
			responses <- databaseCall(h, &recipient, "POST", "/packages/redeem", map[string]string{"code": strings.ToLower(code.Code)})
		}(i)
	}
	wg.Wait()
	close(responses)
	wins := 0
	var grant Grant
	for w := range responses {
		if w.Code == 201 {
			wins++
			grant = decodeResponse[Grant](t, w)
		} else {
			databaseStatus(t, w, 409)
		}
	}
	if wins != 1 {
		t.Fatalf("single-use code redeemed %d times", wins)
	}
	var uses, grants int
	m.rt.DB.QueryRow("SELECT uses FROM database_codes WHERE id=?", code.ID).Scan(&uses)
	m.rt.DB.QueryRow("SELECT COUNT(*) FROM database_grants WHERE code_id=?", code.ID).Scan(&grants)
	if uses != 1 || grants != 1 {
		t.Fatal("redemption counters inconsistent")
	}
	tpl.Quota.MySQLMB = 900
	tpl.Active = true
	databaseStatus(t, databaseCall(h, &s, "PUT", "/admin/templates/"+tpl.ID, tpl), 200)
	var raw string
	if e := m.rt.DB.QueryRow("SELECT quota FROM database_grants WHERE id=?", grant.ID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var q Quota
	json.Unmarshal([]byte(raw), &q)
	if q.MySQLMB != 200 {
		t.Fatal("template edit mutated an existing grant")
	}
	other := s
	other.ProjectID = "elsewhere"
	databaseStatus(t, databaseCall(h, &other, "DELETE", "/admin/templates/"+tpl.ID, nil), 404)
	databaseStatus(t, databaseCall(h, &other, "DELETE", "/admin/codes/"+code.ID, nil), 404)
	code2 := generateCodes(t, m, s, tpl, 1, 5)[0]
	databaseStatus(t, databaseCall(h, &s, "POST", "/packages/redeem", map[string]string{"code": code2.Code}), 201)
	databaseStatus(t, databaseCall(h, &s, "POST", "/packages/redeem", map[string]string{"code": code2.Code}), 409)
}

func TestPackageAuditFailureRollsBackAndExpirationEnforcesReadOnly(t *testing.T) {
	m, s, fake, _ := databaseFixture(t)
	h := m.Handler()
	tpl := makeTemplate(t, m, s, Quota{MySQLMB: 200, MySQLCount: 1})
	code := generateCodes(t, m, s, tpl, 1, 1)[0]
	_, e := m.rt.DB.Exec(`CREATE TRIGGER reject_redemption_audit BEFORE INSERT ON audit WHEN NEW.action='database.package.redeemed' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END;`)
	if e != nil {
		t.Fatal(e)
	}
	databaseStatus(t, databaseCall(h, &s, "POST", "/packages/redeem", map[string]string{"code": code.Code}), 500)
	var n, uses int
	m.rt.DB.QueryRow("SELECT COUNT(*) FROM database_grants").Scan(&n)
	m.rt.DB.QueryRow("SELECT uses FROM database_codes WHERE id=?", code.ID).Scan(&uses)
	if n != 0 || uses != 0 {
		t.Fatal("failed audit committed a grant or consumed a redemption")
	}
	m.rt.DB.Exec("DROP TRIGGER reject_redemption_audit")
	databaseStatus(t, databaseCall(h, &s, "POST", "/packages/redeem", map[string]string{"code": code.Code}), 201)
	v := createDatabase(t, m, s, "mysql")
	fake.size(v.Database, 150*1024*1024)
	databaseStatus(t, databaseCall(h, &s, "POST", "/admin/check-quota", nil), 200)
	if fake.state(v.Database).readOnly {
		t.Fatal("active package quota ignored")
	}
	_, e = m.rt.DB.Exec("UPDATE database_grants SET expires_at=?", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano))
	if e != nil {
		t.Fatal(e)
	}
	databaseStatus(t, databaseCall(h, &s, "POST", "/admin/check-quota", nil), 200)
	if !fake.state(v.Database).readOnly {
		t.Fatal("expired capacity package did not revoke writes")
	}
	state, e := m.get(context.Background(), s, v.ID)
	if e != nil || state.Status != "readonly" {
		t.Fatalf("read-only state missing %q %v", state.Status, e)
	}
	w := databaseCall(h, &s, "GET", "/packages", nil)
	databaseStatus(t, w, 200)
	packages := decodeResponse[struct {
		Items []Grant `json:"items"`
		Quota Quota   `json:"quota"`
	}](t, w)
	if len(packages.Items) != 1 || packages.Items[0].Active || packages.Quota.MySQLMB != 100 {
		t.Fatal("expired grant counted in active entitlement")
	}
	databaseStatus(t, databaseCall(h, &s, "POST", "/admin/grants", map[string]string{"templateId": tpl.ID}), 201)
	databaseStatus(t, databaseCall(h, &s, "POST", "/admin/check-quota", nil), 200)
	if fake.state(v.Database).readOnly {
		t.Fatal("new entitlement did not restore writes")
	}
}

func TestExternalFailureIsUncertainAndReservationPersists(t *testing.T) {
	m, s, fake, path := databaseFixture(t)
	fake.fail = errors.New("private-engine-detail-must-not-leak")
	w := databaseCall(m.Handler(), &s, "POST", "/databases", map[string]string{"name": "partial engine outcome", "type": "mysql"})
	databaseStatus(t, w, 503)
	if strings.Contains(w.Body.String(), fake.fail.Error()) {
		t.Fatal("raw engine details exposed")
	}
	var id, status, op string
	if e := m.rt.DB.QueryRow("SELECT id,status FROM database_instances").Scan(&id, &status); e != nil {
		t.Fatal(e)
	}
	m.rt.DB.QueryRow("SELECT state FROM database_operations").Scan(&op)
	if status != "error" || op != "uncertain" {
		t.Fatal("unknown external outcome presented as success")
	}
	databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/databases", map[string]string{"name": "must not oversubscribe", "type": "mysql"}), 409)
	if e := m.rt.DB.Close(); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	m.rt.DB = db
	if e = m.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	v, e := m.get(context.Background(), s, id)
	if e != nil || v.Status != "error" {
		t.Fatal("resource outcome lost after reopen")
	}
	var auditCount int
	m.rt.DB.QueryRow("SELECT COUNT(*) FROM audit WHERE action='database.create.uncertain'").Scan(&auditCount)
	if auditCount != 1 {
		t.Fatal("outcome audit not durable")
	}
}

func TestIntentAuditFailureNeverCallsEngine(t *testing.T) {
	m, s, fake, _ := databaseFixture(t)
	_, e := m.rt.DB.Exec(`CREATE TRIGGER reject_intent_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'audit unavailable'); END;`)
	if e != nil {
		t.Fatal(e)
	}
	databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/databases", map[string]string{"name": "no engine call", "type": "mysql"}), 500)
	var n int
	m.rt.DB.QueryRow("SELECT COUNT(*) FROM database_instances").Scan(&n)
	if n != 0 || fake.creates != 0 {
		t.Fatal("external side effect occurred without durable intent")
	}
}

func TestCompletionSurvivesBrowserCancellation(t *testing.T) {
	m, s, fake, _ := databaseFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fake.afterCreate = cancel
	defer cancel()
	req := httptest.NewRequest("POST", "/databases", strings.NewReader(`{"name":"cancelled browser","type":"mysql"}`))
	req = req.WithContext(appkit.WithScope(ctx, s))
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, req)
	databaseStatus(t, w, 201)
	var status, state string
	if e := m.rt.DB.QueryRow("SELECT status FROM database_instances").Scan(&status); e != nil {
		t.Fatal(e)
	}
	m.rt.DB.QueryRow("SELECT state FROM database_operations").Scan(&state)
	if status != "active" || state != "completed" {
		t.Fatal("browser cancellation lost actual engine completion")
	}
}

func TestCountOnlyPackageExpiryRevokesBothResources(t *testing.T) {
	m, s, fake, _ := databaseFixture(t)
	tpl := makeTemplate(t, m, s, Quota{MySQLCount: 1})
	databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/admin/grants", map[string]string{"templateId": tpl.ID}), 201)
	first := createDatabase(t, m, s, "mysql")
	second := createDatabase(t, m, s, "mysql")
	if _, e := m.rt.DB.Exec("UPDATE database_grants SET expires_at=?", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)); e != nil {
		t.Fatal(e)
	}
	databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/admin/check-quota", nil), 200)
	if !fake.state(first.Database).readOnly || !fake.state(second.Database).readOnly {
		t.Fatal("count-only package expiry did not enforce the reduced database count allowance")
	}
	// A disabled app must not continue background management with a synthetic
	// system actor, even when the same database resources remain persisted.
	before := fake.readonlyCalls
	m.rt.ApplicationEnabled = func(context.Context, string, string, string) (bool, error) { return false, nil }
	m.ledger.base.MySQLCount = 10
	m.maintainAll(context.Background())
	if fake.readonlyCalls != before {
		t.Fatal("disabled application still performed background engine operations")
	}
}

func TestExpiredAndDisabledCodesCannotGrantQuota(t *testing.T) {
	m, s, _, _ := databaseFixture(t)
	tpl := makeTemplate(t, m, s, Quota{PostgresCount: 2})
	codes := generateCodes(t, m, s, tpl, 3, 1)
	databaseStatus(t, databaseCall(m.Handler(), &s, "DELETE", "/admin/codes/"+codes[0].ID, nil), 200)
	databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/packages/redeem", map[string]string{"code": codes[0].Code}), 409)
	if _, e := m.rt.DB.Exec("UPDATE database_codes SET expires_at=? WHERE id=?", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), codes[1].ID); e != nil {
		t.Fatal(e)
	}
	databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/packages/redeem", map[string]string{"code": codes[1].Code}), 409)
	databaseStatus(t, databaseCall(m.Handler(), &s, "DELETE", "/admin/templates/"+tpl.ID, nil), 200)
	databaseStatus(t, databaseCall(m.Handler(), &s, "POST", "/packages/redeem", map[string]string{"code": codes[2].Code}), 409)
	var grants int
	if e := m.rt.DB.QueryRow("SELECT COUNT(*) FROM database_grants").Scan(&grants); e != nil || grants != 0 {
		t.Fatalf("inactive capabilities issued grants: %d %v", grants, e)
	}
}
