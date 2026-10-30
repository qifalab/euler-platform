package lottery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/weauth"
)

// Deleting a WeAuth site that a room's signup policy still names must not turn
// the room's invitation into a 404 or hide the policy from the managers who
// have to replace it. Signup stays closed until they do.
func TestDeletedWeAuthSiteKeepsPolicyAndSignupPageUsable(t *testing.T) {
	m, s := fixture(t)
	m.rt.ApplicationEnabled = func(context.Context, string, string, string) (bool, error) { return true, nil }
	if _, err := m.rt.DB.Exec("INSERT INTO installations VALUES('weauth','tenant','project','weauth','enabled')"); err != nil {
		t.Fatal(err)
	}
	auth := weauth.New(m.rt)
	if err := auth.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ws := s
	ws.ApplicationID = "weauth"
	ws.InstallationID = "weauth"
	w := call(auth.Handler(), &ws, "POST", "/sites", `{"name":"活动防刷","domains":["euler.example"]}`)
	status(t, w, 201)
	var site struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &site); err != nil {
		t.Fatal(err)
	}
	v := newRoom(t, m, s)
	policyPath := "/rooms/" + v.ID + "/signup-policy"
	status(t, call(m.Handler(), &s, "PUT", policyPath, fmt.Sprintf(`{"weauthSiteId":%q}`, site.ID)), 200)
	inv := call(m.Handler(), &s, "GET", "/rooms/"+v.ID+"/invitation", "")
	status(t, inv, 200)
	var link map[string]string
	if err := json.Unmarshal(inv.Body.Bytes(), &link); err != nil {
		t.Fatal(err)
	}
	join := strings.TrimPrefix(link["signupURL"], "https://euler.example/public/lottery")
	status(t, call(auth.Handler(), &ws, "DELETE", "/sites/"+site.ID, ""), 204)

	w = call(m.Handler(), &s, "GET", policyPath, "")
	status(t, w, 200)
	var p signupPolicy
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.WeAuthSiteID != site.ID || p.Sitekey != "" {
		t.Fatalf("stale WeAuth reference not reported: %+v", p)
	}
	w = call(m.PublicHandler(), nil, http.MethodGet, join, "")
	status(t, w, 200)
	page := w.Body.String()
	if strings.Contains(page, `<form id="signup">`) || !strings.Contains(page, "人机验证暂不可用") {
		t.Fatalf("signup page offers a form that cannot pass verification:\n%s", page)
	}
	if code := call(m.PublicHandler(), nil, http.MethodPost, join+"/register", `{"name":"无验证访客"}`).Code; code < 400 || code >= 500 {
		t.Fatalf("signup without the deleted verification site: status %d", code)
	}
	// Managers can replace the stale reference; signup then works again.
	status(t, call(m.Handler(), &s, "PUT", policyPath, `{"weauthSiteId":""}`), 200)
	status(t, call(m.PublicHandler(), nil, http.MethodPost, join+"/register", `{"name":"恢复后访客"}`), 201)
}
