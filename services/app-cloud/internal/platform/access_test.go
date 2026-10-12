package platform

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type machineTestModule struct{}

func (machineTestModule) ID() string                    { return "database" }
func (machineTestModule) Migrate(context.Context) error { return nil }
func (machineTestModule) PublicHandler() http.Handler   { return nil }
func (machineTestModule) Handler() http.Handler {
	m := http.NewServeMux()
	for _, v := range []struct{ pattern, permission string }{{"GET /databases", "read"}, {"POST /databases", "write"}, {"DELETE /databases/{id}", "manage"}, {"GET /databases/{id}/credentials", "secrets"}, {"POST /admin/check-quota", "admin"}} {
		appkit.Handle(m, v.pattern, v.permission, func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error { appkit.JSON(w, 200, s); return nil })
	}
	return m
}
func machineCall(h http.Handler, token, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestServiceAccountScopeExpirationRotationAndRevocation(t *testing.T) {
	s := testStore(t)
	owner := testUser(t, s, "owner")
	outsider := testUser(t, s, "outsider")
	tenant := mustTenant(t, s, owner.ID)
	project := mustProject(t, s, owner.ID, tenant.ID)
	other := mustProject(t, s, owner.ID, tenant.ID)
	if _, err := s.EnableApplication(testContext, owner.ID, tenant.ID, project.ID, "database"); err != nil {
		t.Fatal(err)
	}
	input := ServiceAccountInput{Name: "CI", ExpiresAt: time.Now().Add(time.Hour), Scopes: []ServiceScope{{"database", []string{"read"}}}}
	if _, _, err := s.CreateServiceAccount(testContext, outsider.ID, tenant.ID, project.ID, input); !errors.Is(err, ErrNotFound) {
		t.Fatal("outsider created account", err)
	}
	account, secret, err := s.CreateServiceAccount(testContext, owner.ID, tenant.ID, project.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err = s.db.QueryRow(`SELECT token_hash FROM service_accounts WHERE id=?`, account.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == secret || stored != tokenHash(secret) {
		t.Fatal("service secret not hashed")
	}
	h := NewHandler(s, actorAuth{owner}, managerFor(t), WithApplications([]appkit.Module{machineTestModule{}, scopeModule{}}, nil))
	base := "/api/v1/machine/tenants/" + tenant.ID + "/projects/" + project.ID + "/apps/database"
	if w := machineCall(h, secret, "GET", base+"/databases", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"provider":"euler-service-account"`) || strings.Contains(w.Body.String(), owner.ID) {
		t.Fatal("machine impersonates owner", w.Code, w.Body.String())
	}
	for _, req := range []struct {
		method, path string
		code         int
	}{{"POST", base + "/databases", 403}, {"GET", base + "/databases/1/credentials", 403}, {"POST", base + "/admin/check-quota", 403}, {"GET", strings.Replace(base, project.ID, other.ID, 1) + "/databases", 404}, {"POST", strings.Replace(base, "/database", "/trust", 1) + "/review", 403}} {
		if w := machineCall(h, secret, req.method, req.path, nil); w.Code != req.code {
			t.Fatal(req, w.Code, w.Body.String())
		}
	}
	for _, header := range []map[string]string{{"Cookie": "euler_session=x"}, {"Origin": "https://example.test"}, {"X-CSRF-Token": "test"}} {
		if w := machineCall(h, secret, "GET", base+"/databases", header); w.Code != 403 {
			t.Fatal("mixed credentials accepted", w.Code)
		}
	}
	browser := strings.Replace(base, "/machine", "", 1) + "/databases"
	if w := machineCall(h, secret, "GET", browser, nil); w.Code != 401 {
		t.Fatal("service token accepted on browser API", w.Code)
	}
	if w := call(h, "GET", base+"/databases", "", true, true); w.Code != 403 {
		t.Fatal("browser session accepted on machine API", w.Code)
	}
	next, err := s.RotateServiceAccount(testContext, owner.ID, tenant.ID, project.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := machineCall(h, secret, "GET", base+"/databases", nil); w.Code != 401 {
		t.Fatal("old token survives rotation", w.Code)
	}
	if w := machineCall(h, next, "GET", base+"/databases", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = s.db.Exec(`UPDATE service_accounts SET expires_at=? WHERE id=?`, stamp()-1, account.ID); err != nil {
		t.Fatal(err)
	}
	if w := machineCall(h, next, "GET", base+"/databases", nil); w.Code != 401 {
		t.Fatal("expired token accepted", w.Code)
	}
	if _, err = s.db.Exec(`UPDATE service_accounts SET expires_at=? WHERE id=?`, stamp()+60000, account.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeServiceAccount(testContext, owner.ID, tenant.ID, project.ID, account.ID); err != nil {
		t.Fatal(err)
	}
	if w := machineCall(h, next, "GET", base+"/databases", nil); w.Code != 401 {
		t.Fatal("revoked token accepted", w.Code)
	}
	list, err := s.ServiceAccounts(testContext, owner.ID, tenant.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(list)
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), next) || strings.Contains(string(encoded), stored) {
		t.Fatal("credential leaked in inventory")
	}
}
func TestProjectArchiveRetainsDataDisablesPublicAndRestoresState(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s, "owner")
	tenant := mustTenant(t, s, u.ID)
	project := mustProject(t, s, u.ID, tenant.ID)
	enabled, err := s.EnableApplication(testContext, u.ID, tenant.ID, project.ID, "database")
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := s.EnableApplication(testContext, u.ID, tenant.ID, project.ID, "trust")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetInstallationStatus(testContext, u.ID, tenant.ID, project.ID, disabled.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	_, secret, err := s.CreateServiceAccount(testContext, u.ID, tenant.ID, project.ID, ServiceAccountInput{"job", time.Now().Add(time.Hour), []ServiceScope{{"database", []string{"read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, actorAuth{u}, managerFor(t), WithApplications([]appkit.Module{machineTestModule{}}, nil))
	base := "/api/v1/machine/tenants/" + tenant.ID + "/projects/" + project.ID + "/apps/database/databases"
	if err = s.SetProjectArchived(testContext, u.ID, tenant.ID, project.ID, "wrong", true); !errors.Is(err, ErrInvalid) {
		t.Fatal("archive confirmation ignored", err)
	}
	if err = s.SetProjectArchived(testContext, u.ID, tenant.ID, project.ID, project.Name, true); err != nil {
		t.Fatal(err)
	}
	if yes, err := s.ApplicationEnabled(testContext, tenant.ID, project.ID, "database"); err != nil || yes {
		t.Fatal("runtime remains enabled", yes, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM installations WHERE project_id=? AND status='enabled'`, project.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("direct SQL public/worker check remains enabled", count, err)
	}
	if _, err = s.db.Exec(`UPDATE installations SET status='enabled' WHERE id=?`, enabled.ID); err == nil {
		t.Fatal("archive trigger allowed bypass")
	}
	if err = s.SetInstallationStatus(testContext, u.ID, tenant.ID, project.ID, enabled.ID, "enabled"); !errors.Is(err, ErrConflict) {
		t.Fatal("re-enable accepted in archived project", err)
	}
	if _, err = s.EnableApplication(testContext, u.ID, tenant.ID, project.ID, "lottery"); !errors.Is(err, ErrConflict) {
		t.Fatal("new installation accepted in archived project", err)
	}
	if w := machineCall(h, secret, "GET", base, nil); w.Code != 409 {
		t.Fatal("machine continued in archive", w.Code)
	}
	p, err := s.Project(testContext, u.ID, tenant.ID, project.ID)
	if err != nil || p.Status != "archived" {
		t.Fatal(p, err)
	}
	exported, err := s.ExportProject(testContext, u.ID, tenant.ID, project.ID)
	if err != nil || len(exported.Installations) != 2 {
		t.Fatal(exported, err)
	}
	if err = s.DeleteProject(testContext, u.ID, tenant.ID, project.ID, project.Name); !errors.Is(err, ErrConflict) {
		t.Fatal("resources silently deleted", err)
	}
	if err = s.DeleteTenant(testContext, u.ID, tenant.ID, tenant.Name); !errors.Is(err, ErrConflict) {
		t.Fatal("nonempty team deleted", err)
	}
	if err = s.SetProjectArchived(testContext, u.ID, tenant.ID, project.ID, project.Name, false); err != nil {
		t.Fatal(err)
	}
	if yes, err := s.ApplicationEnabled(testContext, tenant.ID, project.ID, "database"); err != nil || !yes {
		t.Fatal(yes, err)
	}
	if yes, err := s.ApplicationEnabled(testContext, tenant.ID, project.ID, "trust"); err != nil || yes {
		t.Fatal("previously disabled application restored enabled", yes, err)
	}
	if w := machineCall(h, secret, "GET", base, nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestGrantExpiryEvaluatedOnEveryRequest(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s, "owner")
	tenant := mustTenant(t, s, u.ID)
	project := mustProject(t, s, u.ID, tenant.ID)
	_, err := s.EnableApplication(testContext, u.ID, tenant.ID, project.ID, "trust")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, actorAuth{u}, managerFor(t), WithApplications([]appkit.Module{scopeModule{}}, []Administrator{{u.Provider, u.Subject}}))
	b := map[string]any{"tenantId": tenant.ID, "projectId": project.ID, "applicationId": "trust", "userId": u.ID, "permission": "review", "expiresAt": stamp() + 60000}
	raw, _ := json.Marshal(b)
	w := call(h, "POST", "/api/v1/platform/grants", string(raw), true, true)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var grant struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &grant)
	path := "/api/v1/tenants/" + tenant.ID + "/projects/" + project.ID + "/apps/trust/review"
	if w = call(h, "POST", path, "{}", true, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = s.db.Exec(`UPDATE app_grant_expiry SET expires_at=? WHERE grant_id=?`, stamp()-1, grant.ID); err != nil {
		t.Fatal(err)
	}
	if w = call(h, "POST", path, "{}", true, true); w.Code != 403 {
		t.Fatal("expired grant accepted", w.Code)
	}
}
func TestEmptyProjectDeletionPreservesAuditAndConfirmsName(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s, "owner")
	tenant := mustTenant(t, s, u.ID)
	project := mustProject(t, s, u.ID, tenant.ID)
	if err := s.DeleteProject(testContext, u.ID, tenant.ID, project.ID, project.Name); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.SetProjectArchived(testContext, u.ID, tenant.ID, project.ID, project.Name, true); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(testContext, u.ID, tenant.ID, project.ID, "wrong"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := s.DeleteProject(testContext, u.ID, tenant.ID, project.ID, project.Name); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit WHERE project_id=? AND action='project.deleted'`, project.ID).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err := s.DeleteTenant(testContext, u.ID, tenant.ID, tenant.Name); err != nil {
		t.Fatal(err)
	}
}
