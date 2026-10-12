package platform

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/lottery"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/automation"
)

func TestAutomationProductGrantExpiryAndArchivedProject(t *testing.T) {
	s := testStore(t)
	owner := testUser(t, s, "owner")
	tenant := mustTenant(t, s, owner.ID)
	project := mustProject(t, s, owner.ID, tenant.ID)
	if _, err := s.EnableApplication(testContext, owner.ID, tenant.ID, project.ID, "trust"); err != nil {
		t.Fatal(err)
	}
	// Minimal source table here isolates the real platform authorization resolver.
	if _, err := s.db.Exec("CREATE TABLE trust_schemes(id TEXT,tenant_id TEXT,project_id TEXT,status TEXT);INSERT INTO trust_schemes VALUES('scheme',?,?,'active')", tenant.ID, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO app_grants(id,tenant_id,project_id,application_id,user_id,permission,granted_by,created_at) VALUES('review',?,?,'trust',?,'review',?,?)", tenant.ID, project.ID, owner.ID, owner.ID, stamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO app_grant_expiry VALUES('review',?)", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	engine := s.NewAutomation()
	if err := engine.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rule, err := engine.Create(testContext, owner.ID, tenant.ID, project.ID, automation.Rule{Name: "认证通知", Source: "trust.status", FilterID: "scheme", Action: "notify"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE app_grant_expiry SET expires_at=?", stamp()-1); err != nil {
		t.Fatal(err)
	}
	if err = engine.SetEnabled(testContext, owner.ID, tenant.ID, project.ID, rule.ID, true); err == nil {
		t.Fatal("expired product permission authorizes automation")
	}
	if _, err = s.db.Exec("UPDATE app_grant_expiry SET expires_at=?", stamp()+60000); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO project_lifecycle(project_id,status,updated_at) VALUES(?,'archived',?)", project.ID, stamp()); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Create(testContext, owner.ID, tenant.ID, project.ID, automation.Rule{Name: "归档后禁止", Source: "trust.status", FilterID: "scheme", Action: "notify"}); err == nil {
		t.Fatal("archived project accepts automation")
	}
}

func TestAutomationRoutesAuthenticateCSRFAndNotificationIsolation(t *testing.T) {
	s := testStore(t)
	owner := testUser(t, s, "owner")
	other := testUser(t, s, "other")
	tenant := mustTenant(t, s, owner.ID)
	project := mustProject(t, s, owner.ID, tenant.ID)
	if _, err := s.EnableApplication(testContext, owner.ID, tenant.ID, project.ID, "lottery"); err != nil {
		t.Fatal(err)
	}
	module := lottery.New(s.ApplicationRuntime("", "https://euler.example"))
	if err := module.Migrate(testContext); err != nil {
		t.Fatal(err)
	}
	engine := s.NewAutomation()
	if err := engine.Migrate(testContext); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, actorAuth{owner}, managerFor(t), WithApplications([]appkit.Module{module}, nil), WithAutomation(engine))
	base := "/api/v1/tenants/" + tenant.ID + "/projects/" + project.ID + "/automation"
	if w := call(h, "GET", base+"/rules", "", false, false); w.Code != 401 {
		t.Fatal("unauthenticated automation", w.Code)
	}
	if w := call(h, "POST", base+"/rules", `{}`, true, false); w.Code != 403 {
		t.Fatal("missing csrf accepted", w.Code)
	}
	if _, err := s.db.Exec("INSERT INTO automation_notifications(id,tenant_id,project_id,actor_id,title,body,created_at) VALUES('n',?,?,?,'private','private',?)", tenant.ID, project.ID, other.ID, stamp()); err != nil {
		t.Fatal(err)
	}
	w := call(h, "GET", base+"/notifications", "", true, false)
	if w.Code != 200 || w.Body.String() != fmt.Sprintln(`{"items":[]}`) {
		t.Fatal("other actor notification exposed", w.Code, w.Body.String())
	}
}
