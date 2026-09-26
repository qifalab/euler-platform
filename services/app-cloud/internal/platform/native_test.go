package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

type scopeModule struct{}

func (scopeModule) ID() string                    { return "trust" }
func (scopeModule) Migrate(context.Context) error { return nil }
func (scopeModule) PublicHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appkit.JSON(w, 200, map[string]string{"path": r.URL.Path, "requestURI": r.RequestURI})
	})
}
func (scopeModule) Handler() http.Handler {
	m := http.NewServeMux()
	appkit.Handle(m, "GET /records/{recordID}", "read", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		appkit.JSON(w, 200, map[string]any{"scope": s, "record": r.PathValue("recordID")})
		return nil
	})
	appkit.Handle(m, "POST /review", "review", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error { appkit.JSON(w, 200, s); return nil })
	return m
}

func TestNativeScopeAndExplicitProductGrant(t *testing.T) {
	s := testStore(t)
	owner := testUser(t, s, "owner")
	operator := testUser(t, s, "operator")
	other := testUser(t, s, "other")
	team := mustTenant(t, s, owner.ID)
	project := mustProject(t, s, owner.ID, team.ID)
	i, err := s.EnableApplication(testContext, owner.ID, team.ID, project.ID, "trust")
	if err != nil {
		t.Fatal(err)
	}
	opts := WithApplications([]appkit.Module{scopeModule{}}, []Administrator{{Provider: operator.Provider, Subject: operator.Subject}})
	h := NewHandler(s, actorAuth{owner}, managerFor(t), opts)
	base := "/api/v1/tenants/" + team.ID + "/projects/" + project.ID + "/apps/trust"
	w := call(h, "GET", base+"/records/one", "", true, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"record":"one"`) || !strings.Contains(w.Body.String(), owner.ID) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call(h, "GET", base+"/_context", "", false, false); w.Code != 401 {
		t.Fatal("missing session accepted", w.Code)
	}
	if w = call(h, "POST", base+"/review", "{}", true, true); w.Code != 403 {
		t.Fatal("team owner became reviewer", w.Code)
	}
	grantBody := `{"tenantId":"` + team.ID + `","projectId":"` + project.ID + `","applicationId":"trust","userId":"` + owner.ID + `","permission":"review"}`
	if w = call(h, "POST", "/api/v1/platform/grants", grantBody, true, true); w.Code != 403 {
		t.Fatal("owner granted self privileges", w.Code)
	}
	op := NewHandler(s, actorAuth{operator}, managerFor(t), opts)
	if w = call(op, "POST", "/api/v1/platform/grants", grantBody, true, false); w.Code != 403 {
		t.Fatal("operator bypassed CSRF", w.Code)
	}
	w = call(op, "POST", "/api/v1/platform/grants", grantBody, true, true)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var grant struct{ ID string }
	if err = json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if w = call(h, "POST", base+"/review", "{}", true, true); w.Code != 200 {
		t.Fatal("explicit grant not effective", w.Code, w.Body.String())
	}
	if w = call(op, "DELETE", "/api/v1/platform/grants/"+grant.ID, "", true, true); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call(h, "POST", base+"/review", "{}", true, true); w.Code != 403 {
		t.Fatal("revoked grant still effective", w.Code)
	}
	foreign := NewHandler(s, actorAuth{other}, managerFor(t), opts)
	if w = call(foreign, "GET", base+"/_context", "", true, false); w.Code != 404 {
		t.Fatal("foreign project exposed", w.Code)
	}
	if w = call(h, "GET", installPath(team.ID, project.ID, i.ID)+"/summary", "", true, false); w.Code != 410 {
		t.Fatal("legacy upstream path exposed", w.Code)
	}
	if err = s.SetInstallationStatus(testContext, owner.ID, team.ID, project.ID, i.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if w = call(h, "GET", base+"/_context", "", true, false); w.Code != 409 {
		t.Fatal("disabled app exposed", w.Code)
	}
	if enabled, err := s.ApplicationEnabled(testContext, team.ID, project.ID, "trust"); err != nil || enabled {
		t.Fatal(enabled, err)
	}
}

func TestNativePublicRoutingRetainsSignedURI(t *testing.T) {
	h := NewHandler(testStore(t), nil, managerFor(t), WithApplications([]appkit.Module{scopeModule{}}, nil))
	w := call(h, "POST", "/public/trust/agent/v1/events?batch=2", "", false, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"path":"/agent/v1/events"`) || !strings.Contains(w.Body.String(), `"requestURI":"/public/trust/agent/v1/events?batch=2"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
