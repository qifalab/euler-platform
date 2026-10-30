package platform

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
)

// A removed admin must not re-join with an invitation they created earlier,
// and nobody else may use their unused invitations either.
func TestRemovedAdminCannotRejoinWithOwnInvitation(t *testing.T) {
	s := testStore(t)
	owner, admin, friend := testUser(t, s, "owner"), testUser(t, s, "admin"), testUser(t, s, "friend")
	team := mustTenant(t, s, owner.ID)
	invite(t, s, owner.ID, team.ID, admin.ID, "admin")
	kept, e := s.CreateInvitation(testContext, admin.ID, team.ID, "admin")
	if e != nil {
		t.Fatal(e)
	}
	spare, e := s.CreateInvitation(testContext, admin.ID, team.ID, "member")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ChangeMember(testContext, owner.ID, team.ID, admin.ID, "", true); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AcceptInvitation(testContext, admin.ID, kept.Token); !errors.Is(e, ErrNotFound) {
		t.Fatalf("removed admin re-joined through own invitation: %v", e)
	}
	if _, e = s.AcceptInvitation(testContext, friend.ID, spare.Token); !errors.Is(e, ErrNotFound) {
		t.Fatalf("invitation from removed admin still admits users: %v", e)
	}
	if _, e = s.Members(testContext, admin.ID, team.ID); e == nil {
		t.Fatal("removed admin is a team member again")
	}
	pending, e := s.Invitations(testContext, owner.ID, team.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(pending) != 0 {
		t.Fatalf("invitations from removed admin are still pending: %+v", pending)
	}
}

func TestDemotedAdminInvitationsStopWorking(t *testing.T) {
	s := testStore(t)
	owner, admin, friend, other := testUser(t, s, "owner"), testUser(t, s, "admin"), testUser(t, s, "friend"), testUser(t, s, "other")
	team := mustTenant(t, s, owner.ID)
	invite(t, s, owner.ID, team.ID, admin.ID, "admin")
	demoted, e := s.CreateInvitation(testContext, admin.ID, team.ID, "admin")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ChangeMember(testContext, owner.ID, team.ID, admin.ID, "member", false); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AcceptInvitation(testContext, friend.ID, demoted.Token); !errors.Is(e, ErrNotFound) {
		t.Fatalf("invitation from demoted admin still admits users: %v", e)
	}
	// Rows written before this check existed are refused when used, too.
	if e = s.ChangeMember(testContext, owner.ID, team.ID, admin.ID, "admin", false); e != nil {
		t.Fatal(e)
	}
	legacy, e := s.CreateInvitation(testContext, admin.ID, team.ID, "viewer")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec("UPDATE members SET role='member' WHERE tenant_id=? AND user_id=?", team.ID, admin.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AcceptInvitation(testContext, other.ID, legacy.Token); !errors.Is(e, ErrNotFound) {
		t.Fatalf("invitation whose creator is no longer admin was accepted: %v", e)
	}
	// Invitations from a current owner keep working.
	ok, e := s.CreateInvitation(testContext, owner.ID, team.ID, "member")
	if e != nil {
		t.Fatal(e)
	}
	joined, e := s.AcceptInvitation(testContext, other.ID, ok.Token)
	if e != nil || joined.Role != "member" {
		t.Fatalf("valid invitation failed: %+v %v", joined, e)
	}
}

// A flood of login starts keeps only the newest pending logins.
func TestPendingLoginsAreBounded(t *testing.T) {
	s := testStore(t)
	previous := maxPendingAuthFlows
	maxPendingAuthFlows = 3
	t.Cleanup(func() { maxPendingAuthFlows = previous })
	base := time.Now().Add(5 * time.Minute)
	for i := 0; i < 5; i++ {
		flow := identity.AuthFlow{StateHash: fmt.Sprintf("state-%d", i), Nonce: "nonce", Verifier: "verifier", ReturnTo: "/", ExpiresAt: base.Add(time.Duration(i) * time.Second)}
		if e := s.PutAuthFlow(testContext, flow); e != nil {
			t.Fatal(e)
		}
	}
	var n int
	if e := s.db.QueryRow("SELECT count(*) FROM auth_flows").Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 3 {
		t.Fatalf("pending logins = %d, want 3", n)
	}
	for i := 0; i < 2; i++ {
		if _, e := s.ConsumeAuthFlow(testContext, fmt.Sprintf("state-%d", i)); !errors.Is(e, identity.ErrNotFound) {
			t.Fatalf("oldest pending login %d was kept: %v", i, e)
		}
	}
	for i := 2; i < 5; i++ {
		flow, e := s.ConsumeAuthFlow(testContext, fmt.Sprintf("state-%d", i))
		if e != nil || flow.Verifier != "verifier" {
			t.Fatalf("newest pending login %d was lost: %+v %v", i, flow, e)
		}
	}
}
