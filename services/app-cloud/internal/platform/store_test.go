package platform

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
)

var testContext = context.Background()

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "cloud.db"), bytes.Repeat([]byte{42}, 32))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func testUser(t *testing.T, s *Store, id string) identity.Principal {
	t.Helper()
	p, e := s.UpsertIdentity(testContext, identity.ExternalIdentity{Provider: "https://issuer.test", Subject: id, Name: id, Email: id + "@example.test", EmailVerified: true})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func mustTenant(t *testing.T, s *Store, u string) Tenant {
	t.Helper()
	v, e := s.CreateTenant(testContext, u, "Engineering")
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func mustProject(t *testing.T, s *Store, u, tenant string) Project {
	t.Helper()
	v, e := s.CreateProject(testContext, u, tenant, "Project")
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func mustApp(t *testing.T, s *Store, u, tenant, project string) Installation {
	t.Helper()
	v, e := s.EnableApplication(testContext, u, tenant, project, "weauth")
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func invite(t *testing.T, s *Store, owner, tenant, user, role string) {
	t.Helper()
	v, e := s.CreateInvitation(testContext, owner, tenant, role)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.AcceptInvitation(testContext, user, v.Token); e != nil {
		t.Fatal(e)
	}
}

func TestPersistentIdentitySessionsAndEncryptionKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persistent.db")
	key := bytes.Repeat([]byte{1}, 32)
	s, e := Open(path, key)
	if e != nil {
		t.Fatal(e)
	}
	u := testUser(t, s, "alice")
	team := mustTenant(t, s, u.ID)
	other, e := s.UpsertIdentity(testContext, identity.ExternalIdentity{Provider: "https://other.test", Subject: "alice", Name: "alice", Email: u.Email})
	if e != nil || other.ID == u.ID {
		t.Fatal("different providers were merged", e)
	}
	another, e := s.UpsertIdentity(testContext, identity.ExternalIdentity{Provider: u.Provider, Subject: "another-subject", Name: u.Name, Email: u.Email})
	if e != nil || another.ID == u.ID {
		t.Fatal("email linked separate subjects", e)
	}
	refreshed, e := s.UpsertIdentity(testContext, identity.ExternalIdentity{Provider: u.Provider, Subject: u.Subject, Name: "Renamed"})
	if e != nil || refreshed.ID != u.ID {
		t.Fatal("stable identity changed", e)
	}
	if e = s.PutSession(testContext, identity.Session{TokenHash: "hashed-token", UserID: u.ID, CSRFToken: "csrf", ExpiresAt: time.Now().Add(time.Hour)}); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if bad, e := Open(path, bytes.Repeat([]byte{2}, 32)); e == nil {
		bad.Close()
		t.Fatal("wrong encryption key accepted")
	}
	s, e = Open(path, key)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	got, e := s.Tenants(testContext, u.ID)
	if e != nil || len(got) != 1 || got[0].ID != team.ID {
		t.Fatal(got, e)
	}
	session, e := s.GetSession(testContext, "hashed-token")
	if e != nil || session.UserID != u.ID {
		t.Fatal(session, e)
	}
	if e = s.DeleteSession(testContext, "hashed-token"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetSession(testContext, "hashed-token"); !errors.Is(e, identity.ErrNotFound) {
		t.Fatal(e)
	}
	info, e := os.Stat(path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("database permission", e)
	}
}
func TestTenantProjectIsolationAndRevocation(t *testing.T) {
	s := testStore(t)
	alice := testUser(t, s, "alice")
	bob := testUser(t, s, "bob")
	eve := testUser(t, s, "eve")
	ta := mustTenant(t, s, alice.ID)
	tb := mustTenant(t, s, eve.ID)
	pa := mustProject(t, s, alice.ID, ta.ID)
	pb := mustProject(t, s, eve.ID, tb.ID)
	if _, e := s.Members(testContext, bob.ID, ta.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal("nonmember enumerated team", e)
	}
	invite(t, s, alice.ID, ta.ID, bob.ID, "member")
	if ps, e := s.Projects(testContext, bob.ID, ta.ID); e != nil || len(ps) != 0 {
		t.Fatal("membership leaked all projects", ps, e)
	}
	if e := s.SetProjectMember(testContext, alice.ID, ta.ID, pa.ID, bob.ID, "viewer", false); e != nil {
		t.Fatal(e)
	}
	if e := s.RequireProject(testContext, bob.ID, ta.ID, pa.ID, "read"); e != nil {
		t.Fatal(e)
	}
	if e := s.RequireProject(testContext, bob.ID, ta.ID, pa.ID, "write"); !errors.Is(e, ErrForbidden) {
		t.Fatal("viewer could write", e)
	}
	if e := s.RequireProject(testContext, alice.ID, ta.ID, pb.ID, "read"); !errors.Is(e, ErrNotFound) {
		t.Fatal("cross-tenant project accessible", e)
	}
	if e := s.SetProjectMember(testContext, bob.ID, ta.ID, pa.ID, bob.ID, "admin", false); !errors.Is(e, ErrForbidden) {
		t.Fatal("self escalation", e)
	}
	if e := s.SetProjectMember(testContext, alice.ID, ta.ID, pa.ID, eve.ID, "admin", false); !errors.Is(e, ErrNotFound) {
		t.Fatal("nonmember project binding", e)
	}
	if e := s.ChangeMember(testContext, alice.ID, ta.ID, bob.ID, "", true); e != nil {
		t.Fatal(e)
	}
	invite(t, s, alice.ID, ta.ID, bob.ID, "member")
	if ps, e := s.Projects(testContext, bob.ID, ta.ID); e != nil || len(ps) != 0 {
		t.Fatal("old project grants revived after rejoining", ps, e)
	}
}
func TestOwnerProtectionAndInvitationDoesNotEscalate(t *testing.T) {
	s := testStore(t)
	a := testUser(t, s, "a")
	b := testUser(t, s, "b")
	team := mustTenant(t, s, a.ID)
	if e := s.ChangeMember(testContext, a.ID, team.ID, a.ID, "member", false); !errors.Is(e, ErrConflict) {
		t.Fatal("last owner demoted", e)
	}
	invite(t, s, a.ID, team.ID, b.ID, "admin")
	if e := s.ChangeMember(testContext, b.ID, team.ID, a.ID, "", true); !errors.Is(e, ErrForbidden) {
		t.Fatal("admin removed owner", e)
	}
	if e := s.ChangeMember(testContext, b.ID, team.ID, b.ID, "owner", false); !errors.Is(e, ErrForbidden) {
		t.Fatal("admin self promoted", e)
	}
	invite(t, s, a.ID, team.ID, b.ID, "viewer")
	ms, e := s.Members(testContext, a.ID, team.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, m := range ms {
		if m.UserID == b.ID && m.Role != "admin" {
			t.Fatal("invitation changed existing role")
		}
	}
	if e = s.ChangeMember(testContext, a.ID, team.ID, b.ID, "owner", false); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = s.ChangeMember(testContext, a.ID, team.ID, a.ID, "member", false) }()
	go func() { defer wg.Done(); _ = s.ChangeMember(testContext, b.ID, team.ID, b.ID, "member", false) }()
	wg.Wait()
	ms, e = s.Members(testContext, a.ID, team.ID)
	if e != nil {
		t.Fatal(e)
	}
	n := 0
	for _, m := range ms {
		if m.Role == "owner" {
			n++
		}
	}
	if n != 1 {
		t.Fatal("concurrent demotion left invalid owner count", n)
	}
}
func TestInvitationsAtomicSingleUseExpiryAndRevocation(t *testing.T) {
	s := testStore(t)
	a := testUser(t, s, "a")
	b := testUser(t, s, "b")
	c := testUser(t, s, "c")
	team := mustTenant(t, s, a.ID)
	inv, e := s.CreateInvitation(testContext, a.ID, team.ID, "member")
	if e != nil {
		t.Fatal(e)
	}
	list, e := s.Invitations(testContext, a.ID, team.ID)
	if e != nil || len(list) != 1 || list[0].Token != "" {
		t.Fatal("invitation token exposed", list, e)
	}
	var saved string
	if e = s.db.QueryRow("SELECT token_hash FROM invitations WHERE id=?", inv.ID).Scan(&saved); e != nil || saved == inv.Token {
		t.Fatal("invitation token stored raw", e)
	}
	errs := make(chan error, 2)
	for _, u := range []string{b.ID, c.ID} {
		go func(u string) { _, e := s.AcceptInvitation(testContext, u, inv.Token); errs <- e }(u)
	}
	success := 0
	for range 2 {
		if e := <-errs; e == nil {
			success++
		} else if !errors.Is(e, ErrNotFound) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatal("single-use race", success)
	}
	for _, mode := range []string{"expire", "revoke"} {
		v, e := s.CreateInvitation(testContext, a.ID, team.ID, "viewer")
		if e != nil {
			t.Fatal(e)
		}
		if mode == "expire" {
			_, e = s.db.Exec("UPDATE invitations SET expires_at=0 WHERE id=?", v.ID)
		} else {
			e = s.RevokeInvitation(testContext, a.ID, team.ID, v.ID)
		}
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.AcceptInvitation(testContext, c.ID, v.Token); !errors.Is(e, ErrNotFound) {
			t.Fatal(mode, e)
		}
	}
}
func TestCredentialEncryptionOwnerIsolationAndAuditRedaction(t *testing.T) {
	s := testStore(t)
	a := testUser(t, s, "a")
	b := testUser(t, s, "b")
	team := mustTenant(t, s, a.ID)
	p1 := mustProject(t, s, a.ID, team.ID)
	p2 := mustProject(t, s, a.ID, team.ID)
	i1 := mustApp(t, s, a.ID, team.ID, p1.ID)
	i2 := mustApp(t, s, a.ID, team.ID, p2.ID)
	secret := "secret-value-that-must-not-be-stored-plain"
	c := Connection{BaseURL: "https://weauth.example.test", Credential: secret, ExternalAccountID: "verified-owner:7"}
	if e := s.SaveConnection(testContext, a.ID, team.ID, p1.ID, i1.ID, c); e != nil {
		t.Fatal(e)
	}
	for _, other := range []Connection{{BaseURL: c.BaseURL, Credential: "another-token-same-owner", ExternalAccountID: c.ExternalAccountID}, {BaseURL: c.BaseURL, Credential: secret, ExternalAccountID: "verified-owner:8"}} {
		if e := s.SaveConnection(testContext, a.ID, team.ID, p2.ID, i2.ID, other); !errors.Is(e, ErrConflict) {
			t.Fatal("shared account allowed across projects", e)
		}
	}
	if _, _, e := s.ConnectionFor(testContext, b.ID, team.ID, p1.ID, i1.ID, "read"); !errors.Is(e, ErrNotFound) {
		t.Fatal("credential authorized for stranger", e)
	}
	var encrypted []byte
	if e := s.db.QueryRow("SELECT credential FROM connections WHERE installation_id=?", i1.ID).Scan(&encrypted); e != nil || bytes.Contains(encrypted, []byte(secret)) {
		t.Fatal("plaintext credential stored", e)
	}
	_, got, e := s.ConnectionFor(testContext, a.ID, team.ID, p1.ID, i1.ID, "read")
	if e != nil || got.Credential != secret {
		t.Fatal("credential decrypt", e)
	}
	if _, e = s.decrypt(encrypted, "connection:"+team.ID+":"+p2.ID+":"+i2.ID); e == nil {
		t.Fatal("ciphertext moved between projects")
	}
	ev, e := s.Audit(testContext, a.ID, team.ID, "", 100)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range ev {
		if strings.Contains(v.Summary, secret) {
			t.Fatal("audit leaked credential")
		}
	}
	if e = s.ClearConnection(testContext, a.ID, team.ID, p1.ID, i1.ID); e != nil {
		t.Fatal(e)
	}
	_, got, e = s.ConnectionFor(testContext, a.ID, team.ID, p1.ID, i1.ID, "read")
	if e != nil || got.Credential != "" || got.BaseURL != "" {
		t.Fatal("disconnect retained credentials", e)
	}
}
func TestOIDCFlowEncryptedAndAtomicConsumption(t *testing.T) {
	s := testStore(t)
	v := identity.AuthFlow{StateHash: "state-digest", Nonce: "nonce-value", Verifier: "pkce-private-verifier", ReturnTo: "/", ExpiresAt: time.Now().Add(time.Minute)}
	if e := s.PutAuthFlow(testContext, v); e != nil {
		t.Fatal(e)
	}
	var blob []byte
	if e := s.db.QueryRow("SELECT payload FROM auth_flows WHERE state_hash=?", v.StateHash).Scan(&blob); e != nil || bytes.Contains(blob, []byte(v.Verifier)) {
		t.Fatal("PKCE not encrypted", e)
	}
	flow, e := s.ConsumeAuthFlow(testContext, v.StateHash)
	if e != nil || flow.Verifier != v.Verifier {
		t.Fatal(e)
	}
	if _, e = s.ConsumeAuthFlow(testContext, v.StateHash); !errors.Is(e, identity.ErrNotFound) {
		t.Fatal("replayed flow", e)
	}
}
func TestResourceBindingMustBelongToSelectedProject(t *testing.T) {
	s := testStore(t)
	a := testUser(t, s, "a")
	team := mustTenant(t, s, a.ID)
	p1 := mustProject(t, s, a.ID, team.ID)
	p2 := mustProject(t, s, a.ID, team.ID)
	i1 := mustApp(t, s, a.ID, team.ID, p1.ID)
	i2 := mustApp(t, s, a.ID, team.ID, p2.ID)
	r := Resource{ID: "site-7", Name: "App", Type: "site"}
	if e := s.ResourceCreated(testContext, a.ID, team.ID, p1.ID, i1.ID, r); e != nil {
		t.Fatal(e)
	}
	if e := s.RequireResource(testContext, a.ID, team.ID, p2.ID, i2.ID, r.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal("cross-project remote delete allowed", e)
	}
	if e := s.RequireResource(testContext, a.ID, team.ID, p1.ID, i1.ID, r.ID); e != nil {
		t.Fatal(e)
	}
	if e := s.ResourceDeleted(testContext, a.ID, team.ID, p1.ID, i1.ID, r.ID); e != nil {
		t.Fatal(e)
	}
}

func TestAuthFlowConcurrentConsumptionAndAuditPagination(t *testing.T) {
	s := testStore(t)
	v := identity.AuthFlow{StateHash: "single", Nonce: "nonce", Verifier: "private", ReturnTo: "/", ExpiresAt: time.Now().Add(time.Minute)}
	if e := s.PutAuthFlow(testContext, v); e != nil {
		t.Fatal(e)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { _, e := s.ConsumeAuthFlow(testContext, "single"); results <- e }()
	}
	passed := 0
	for range 2 {
		if e := <-results; e == nil {
			passed++
		} else if !errors.Is(e, identity.ErrNotFound) {
			t.Fatal(e)
		}
	}
	if passed != 1 {
		t.Fatal("flow consumed more than once")
	}
	a := testUser(t, s, "a")
	team := mustTenant(t, s, a.ID)
	_ = mustProject(t, s, a.ID, team.ID)
	_ = mustProject(t, s, a.ID, team.ID)
	page, e := s.Audit(testContext, a.ID, team.ID, "", 2)
	if e != nil || len(page) != 2 {
		t.Fatal(page, e)
	}
	next, e := s.Audit(testContext, a.ID, team.ID, "", 2, page[1].ID)
	if e != nil || len(next) != 1 {
		t.Fatal(next, e)
	}
	if next[0].ID == page[0].ID || next[0].ID == page[1].ID {
		t.Fatal("audit pagination repeated row")
	}
	other := mustTenant(t, s, a.ID)
	if _, e = s.Audit(testContext, a.ID, other.ID, "", 2, page[1].ID); !errors.Is(e, ErrNotFound) {
		t.Fatal("foreign audit cursor accepted", e)
	}
}
