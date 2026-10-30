package platform

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/database"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/lottery"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/statistics"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/storage"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/weauth"
)

// A service-account call carries exactly the permission its machine route
// names, and operations do not include one another. If a module route needs a
// different permission, every service account is refused on that route even
// with the documented scope. A scope holding every permission except the
// route's own must therefore stop at the module's permission check; that check
// runs before the endpoint, so no business code or infrastructure is reached.
func TestMachineRoutesMatchModulePermissions(t *testing.T) {
	s := testStore(t)
	rt := s.ApplicationRuntime(t.TempDir(), "http://127.0.0.1")
	modules := map[string]appkit.Module{}
	for _, m := range []appkit.Module{database.New(rt), storage.New(rt), statistics.New(rt), weauth.New(rt), lottery.New(rt)} {
		modules[m.ID()] = m
	}
	all := []string{"read", "write", "manage", "secrets", "review", "admin"}
	for _, route := range machineRoutes {
		module, ok := modules[route.app]
		if !ok {
			t.Errorf("machine route for unknown application %s", route.app)
			continue
		}
		others := []string{}
		for _, p := range all {
			if p != route.permission {
				others = append(others, p)
			}
		}
		path := strings.ReplaceAll(route.path, "*", "x1")
		code, body := serveRecovered(module.Handler(), route.method, path, appkit.Scope{ActorID: "sa_test", TenantID: "t", ProjectID: "p", InstallationID: "i", ApplicationID: route.app, Permissions: others})
		if code != 403 || !strings.Contains(body, `"code":"forbidden"`) {
			t.Errorf("%s %s %s: module does not require %q (status %d)", route.app, route.method, route.path, route.permission, code)
		}
	}
}

func serveRecovered(h http.Handler, method, path string, s appkit.Scope) (code int, body string) {
	defer func() {
		if p := recover(); p != nil {
			code, body = 0, fmt.Sprint(p)
		}
	}()
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r = r.WithContext(appkit.WithScope(r.Context(), s))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}
