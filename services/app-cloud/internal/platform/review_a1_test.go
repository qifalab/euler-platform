package platform

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/automation"
)

// A team admin reaches every project without a project_members row. Losing the
// admin role removes that reach, so review/admin grants on those projects must
// go with it, exactly as removal from a project deletes them. Otherwise a later
// read-only invitation to the project silently revives the old product grant.
func TestDemotedTeamAdminLosesGrantsOnProjectsNoLongerReachable(t *testing.T) {
	s := testStore(t)
	owner, lead := testUser(t, s, "owner"), testUser(t, s, "lead")
	team := mustTenant(t, s, owner.ID)
	member := mustProject(t, s, owner.ID, team.ID)
	implicit := mustProject(t, s, owner.ID, team.ID)
	invite(t, s, owner.ID, team.ID, lead.ID, "admin")
	if e := s.SetProjectMember(testContext, owner.ID, team.ID, member.ID, lead.ID, "member", false); e != nil {
		t.Fatal(e)
	}
	opts := WithApplications([]appkit.Module{scopeModule{}}, []Administrator{{owner.Provider, owner.Subject}})
	operator := NewHandler(s, actorAuth{owner}, managerFor(t), opts)
	for _, p := range []Project{member, implicit} {
		if _, e := s.EnableApplication(testContext, owner.ID, team.ID, p.ID, "trust"); e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(map[string]any{"tenantId": team.ID, "projectId": p.ID, "applicationId": "trust", "userId": lead.ID, "permission": "review"})
		if w := call(operator, "POST", "/api/v1/platform/grants", string(raw), true, true); w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if e := s.ChangeMember(testContext, owner.ID, team.ID, lead.ID, "member", false); e != nil {
		t.Fatal(e)
	}
	grants := func(project string) int {
		var n int
		if e := s.db.QueryRow(`SELECT count(*) FROM app_grants WHERE project_id=? AND user_id=?`, project, lead.ID).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	if n := grants(implicit.ID); n != 0 {
		t.Fatalf("grant kept on a project the demoted admin can no longer reach: %d", n)
	}
	if n := grants(member.ID); n != 1 {
		t.Fatalf("grant on a project the user still belongs to was removed: %d", n)
	}
	if e := s.SetProjectMember(testContext, owner.ID, team.ID, implicit.ID, lead.ID, "viewer", false); e != nil {
		t.Fatal(e)
	}
	h := NewHandler(s, actorAuth{lead}, managerFor(t), opts)
	review := func(project string) int {
		return call(h, "POST", "/api/v1/tenants/"+team.ID+"/projects/"+project+"/apps/trust/review", "{}", true, true).Code
	}
	if code := review(implicit.ID); code != 403 {
		t.Fatalf("re-added viewer regained the old review grant: %d", code)
	}
	if code := review(member.ID); code != 200 {
		t.Fatalf("grant on a still-reachable project stopped working: %d", code)
	}
}

// The project audit stays readable to the project administrator after archive;
// archive only stops new application work.
func TestProjectAdminReadsAuditOfArchivedProject(t *testing.T) {
	s := testStore(t)
	owner, lead, dev := testUser(t, s, "owner"), testUser(t, s, "lead"), testUser(t, s, "dev")
	team := mustTenant(t, s, owner.ID)
	project := mustProject(t, s, owner.ID, team.ID)
	invite(t, s, owner.ID, team.ID, lead.ID, "member")
	invite(t, s, owner.ID, team.ID, dev.ID, "member")
	if e := s.SetProjectMember(testContext, owner.ID, team.ID, project.ID, lead.ID, "admin", false); e != nil {
		t.Fatal(e)
	}
	if e := s.SetProjectMember(testContext, owner.ID, team.ID, project.ID, dev.ID, "member", false); e != nil {
		t.Fatal(e)
	}
	if e := s.SetProjectArchived(testContext, lead.ID, team.ID, project.ID, project.Name, true); e != nil {
		t.Fatal(e)
	}
	events, e := s.Audit(testContext, lead.ID, team.ID, project.ID, 10)
	if e != nil {
		t.Fatalf("project admin cannot read the audit of an archived project: %v", e)
	}
	if len(events) == 0 || events[0].Action != "project.archived" {
		t.Fatalf("archive event missing: %+v", events)
	}
	if _, e = s.Audit(testContext, dev.ID, team.ID, project.ID, 10); !errors.Is(e, ErrForbidden) {
		t.Fatalf("project member read the project audit: %v", e)
	}
}

// OPERATIONS.md: authenticated and loginAvailable report different things and
// can be true and false at the same time (a valid session while the identity
// provider is down). The console keeps loginAvailable when a session expires.
type loginDownAuth struct{ actorAuth }

func (loginDownAuth) LoginAvailable() bool { return false }

func TestSessionReportsLoginAvailabilityForAuthenticatedUsers(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s, "alice")
	h := NewHandler(s, loginDownAuth{actorAuth{u}}, managerFor(t))
	w := call(h, "GET", "/api/v1/session", "", true, false)
	var body struct {
		Authenticated  bool `json:"authenticated"`
		LoginAvailable bool `json:"loginAvailable"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if !body.Authenticated || body.LoginAvailable {
		t.Fatalf("want authenticated=true loginAvailable=false, got %s", w.Body.String())
	}
	up := NewHandler(s, actorAuth{u}, managerFor(t))
	if w = call(up, "GET", "/api/v1/session", "", true, false); !strings.Contains(w.Body.String(), `"loginAvailable":true`) {
		t.Fatalf("authenticator without a status probe must keep login available: %s", w.Body.String())
	}
}

// An invitation admits one new member. Opening it as somebody who already
// belongs to the team changes nothing, so it must not use up the invitation or
// record a join that did not happen.
func TestExistingMemberDoesNotConsumeInvitation(t *testing.T) {
	s := testStore(t)
	owner, bob, carol := testUser(t, s, "owner"), testUser(t, s, "bob"), testUser(t, s, "carol")
	team := mustTenant(t, s, owner.ID)
	invite(t, s, owner.ID, team.ID, bob.ID, "viewer")
	inv, e := s.CreateInvitation(testContext, owner.ID, team.ID, "member")
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.AcceptInvitation(testContext, bob.ID, inv.Token)
	if e != nil || got.ID != team.ID || got.Role != "viewer" {
		t.Fatalf("existing member: %+v %v", got, e)
	}
	pending, e := s.Invitations(testContext, owner.ID, team.ID)
	if e != nil || len(pending) != 1 || pending[0].ID != inv.ID {
		t.Fatalf("invitation used up by an existing member: %+v %v", pending, e)
	}
	joined, e := s.AcceptInvitation(testContext, carol.ID, inv.Token)
	if e != nil || joined.Role != "member" {
		t.Fatalf("invited newcomer could not join: %+v %v", joined, e)
	}
	var n int
	if e = s.db.QueryRow(`SELECT count(*) FROM audit WHERE action='invitation.accepted' AND actor_id=?`, bob.ID).Scan(&n); e != nil || n != 1 {
		t.Fatalf("false join recorded for an existing member: %d %v", n, e)
	}
}

// AUTOMATION.md: PATCH /rules/{id} takes {"enabled": bool}. A body without the
// field used to decode as enabled=false and paused the rule, which also cancels
// its queued tasks. The field is required. A misspelled field was already
// refused by decode's DisallowUnknownFields; it stays covered here.
func TestAutomationRulePatchRequiresEnabled(t *testing.T) {
	s := testStore(t)
	owner := testUser(t, s, "owner")
	tenant := mustTenant(t, s, owner.ID)
	project := mustProject(t, s, owner.ID, tenant.ID)
	if _, err := s.EnableApplication(testContext, owner.ID, tenant.ID, project.ID, "trust"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TABLE trust_schemes(id TEXT,tenant_id TEXT,project_id TEXT,status TEXT);INSERT INTO trust_schemes VALUES('scheme',?,?,'active')", tenant.ID, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO app_grants(id,tenant_id,project_id,application_id,user_id,permission,granted_by,created_at) VALUES('review',?,?,'trust',?,'review',?,?)", tenant.ID, project.ID, owner.ID, owner.ID, stamp()); err != nil {
		t.Fatal(err)
	}
	engine := s.NewAutomation()
	if err := engine.Migrate(testContext); err != nil {
		t.Fatal(err)
	}
	rule, err := engine.Create(testContext, owner.ID, tenant.ID, project.ID, automation.Rule{Name: "认证通知", Source: "trust.status", FilterID: "scheme", Action: "notify"})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, actorAuth{owner}, managerFor(t), WithApplications([]appkit.Module{scopeModule{}}, nil), WithAutomation(engine))
	path := "/api/v1/tenants/" + tenant.ID + "/projects/" + project.ID + "/automation/rules/" + rule.ID
	enabled := func() bool {
		rules, err := engine.Rules(testContext, tenant.ID, project.ID)
		if err != nil || len(rules) != 1 {
			t.Fatal(rules, err)
		}
		return rules[0].Enabled
	}
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enable":false}`} {
		if w := call(h, "PATCH", path, body, true, true); w.Code != 400 {
			t.Fatalf("%s: status %d %s", body, w.Code, w.Body.String())
		}
		if !enabled() {
			t.Fatalf("%s paused the rule", body)
		}
	}
	if w := call(h, "PATCH", path, `{"enabled":false}`, true, true); w.Code != 204 || enabled() {
		t.Fatalf("explicit pause: %d %s", w.Code, w.Body.String())
	}
}
