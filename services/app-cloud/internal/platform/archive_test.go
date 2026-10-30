package platform

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

func TestArchivedProjectRejectsNewGrantsAndRename(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s, "owner")
	tenant := mustTenant(t, s, u.ID)
	project := mustProject(t, s, u.ID, tenant.ID)
	if _, err := s.EnableApplication(testContext, u.ID, tenant.ID, project.ID, "trust"); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, actorAuth{u}, managerFor(t), WithApplications([]appkit.Module{scopeModule{}}, []Administrator{{u.Provider, u.Subject}}))
	raw, _ := json.Marshal(map[string]any{"tenantId": tenant.ID, "projectId": project.ID, "applicationId": "trust", "userId": u.ID, "permission": "review"})
	if err := s.SetProjectArchived(testContext, u.ID, tenant.ID, project.ID, project.Name, true); err != nil {
		t.Fatal(err)
	}
	if w := call(h, "POST", "/api/v1/platform/grants", string(raw), true, true); w.Code != 409 {
		t.Fatal("grant created on archived project", w.Code, w.Body.String())
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM app_grants WHERE project_id=?`, project.ID).Scan(&n); err != nil || n != 0 {
		t.Fatal("archived project gained grants", n, err)
	}
	if err := s.RenameProject(testContext, u.ID, tenant.ID, project.ID, "renamed while archived"); !errors.Is(err, ErrConflict) {
		t.Fatal("archived project renamed", err)
	}
	if err := s.SetProjectArchived(testContext, u.ID, tenant.ID, project.ID, project.Name, false); err != nil {
		t.Fatal(err)
	}
	if w := call(h, "POST", "/api/v1/platform/grants", string(raw), true, true); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := s.RenameProject(testContext, u.ID, tenant.ID, project.ID, "renamed after restore"); err != nil {
		t.Fatal(err)
	}
}
