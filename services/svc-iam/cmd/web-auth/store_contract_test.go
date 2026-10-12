package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/authz"
)

// The authStore contract, run against memStore and sqlStore alike (F1
// parity): the handlers cannot tell the two apart.

func TestStoreContractAccounts(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		ctx := context.Background()
		created := time.Date(2026, 10, 1, 8, 30, 15, 700_000_000, time.UTC)
		a, err := st.CreateAccount(ctx, account{AccountName: "contract@euler.emoera.com", PasswordHash: "h1", Status: 1, CreatedAt: created})
		if err != nil {
			t.Fatal(err)
		}
		if a.AccountID == 0 || !a.CreatedAt.Equal(created.Truncate(time.Second)) {
			t.Fatalf("created: id=%d at=%v (want a fresh id, whole seconds)", a.AccountID, a.CreatedAt)
		}
		if _, err := st.CreateAccount(ctx, account{AccountName: "contract@euler.emoera.com", PasswordHash: "x", Status: 1}); !errors.Is(err, errExists) {
			t.Fatalf("duplicate name: %v", err)
		}
		if _, err := st.CreateAccount(ctx, account{AccountID: a.AccountID, AccountName: "other@euler.emoera.com", PasswordHash: "x", Status: 1}); !errors.Is(err, errExists) {
			t.Fatalf("duplicate id: %v", err)
		}
		if got, err := st.AccountByName(ctx, "contract@euler.emoera.com"); err != nil || got.AccountID != a.AccountID {
			t.Fatalf("by name: %+v %v", got, err)
		}
		if _, err := st.AccountByID(ctx, 987654321); !errors.Is(err, errNotFound) {
			t.Fatalf("missing id: %v", err)
		}
		if _, err := st.AccountByName(ctx, "nobody@euler.emoera.com"); !errors.Is(err, errNotFound) {
			t.Fatalf("missing name: %v", err)
		}

		// fn's error aborts the update and comes back unchanged.
		sentinel := errors.New("abort")
		if _, err := st.UpdateAccount(ctx, a.AccountID, func(x *account) error { x.PasswordHash = "nope"; return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("aborted update: %v", err)
		}
		if got := mustAccount(t, a.AccountID); got.PasswordHash != "h1" {
			t.Fatal("an aborted update was applied")
		}

		// Every persisted field round-trips: real name, MFA seed, version.
		cipher := []byte{0, 60, 1, 2, 3, 250}
		upd, err := st.UpdateAccount(ctx, a.AccountID, func(x *account) error {
			x.RealNameStatus, x.RealNameType = 2, "enterprise"
			x.RealName, x.EnterpriseName, x.IDCardNo = "", "欧拉科技有限公司", "9111****3X"
			x.IdentityFingerprint = strings.Repeat("ab", 32)
			x.MFASecretCipher, x.MFAKeyVersion, x.MFAEnabled = cipher, 3, true
			x.TokenVersion = 4
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got := mustAccount(t, a.AccountID)
		if got.RealNameStatus != 2 || got.RealNameType != "enterprise" || got.EnterpriseName != "欧拉科技有限公司" ||
			got.IDCardNo != "9111****3X" || got.IdentityFingerprint != strings.Repeat("ab", 32) ||
			!bytes.Equal(got.MFASecretCipher, cipher) || got.MFAKeyVersion != 3 || !got.MFAEnabled || got.TokenVersion != 4 ||
			!got.CreatedAt.Equal(upd.CreatedAt) {
			t.Fatalf("round trip: %+v", got)
		}
		// Clearing the seed removes it.
		if _, err := st.UpdateAccount(ctx, a.AccountID, func(x *account) error {
			x.MFASecretCipher, x.MFAKeyVersion, x.MFAEnabled = nil, 0, false
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if got := mustAccount(t, a.AccountID); got.MFASecretCipher != nil || got.MFAEnabled {
			t.Fatalf("seed not cleared: %+v", got)
		}
		// Renaming onto a taken name is refused.
		if _, err := st.CreateAccount(ctx, account{AccountName: "taken@euler.emoera.com", PasswordHash: "x", Status: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateAccount(ctx, a.AccountID, func(x *account) error { x.AccountName = "taken@euler.emoera.com"; return nil }); !errors.Is(err, errExists) {
			t.Fatalf("rename onto a taken name: %v", err)
		}
	})
}

// Concurrent read-modify-writes of one account never lose an update.
func TestStoreContractConcurrentUpdates(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		ctx := context.Background()
		a, err := st.CreateAccount(ctx, account{AccountName: "race@euler.emoera.com", PasswordHash: "h", Status: 1})
		if err != nil {
			t.Fatal(err)
		}
		const n = 8
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := st.UpdateAccount(ctx, a.AccountID, func(x *account) error { x.TokenVersion++; return nil })
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if got := mustAccount(t, a.AccountID).TokenVersion; got != n {
			t.Fatalf("token version %d after %d concurrent bumps", got, n)
		}
	})
}

func TestStoreContractSessions(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		ctx, now := context.Background(), time.Now()
		a, err := st.CreateAccount(ctx, account{AccountName: "sessions@euler.emoera.com", PasswordHash: "h", Status: 1})
		if err != nil {
			t.Fatal(err)
		}
		s1 := refreshSession{TokenHash: hashToken("t1"), AccountID: a.AccountID, TokenVersion: 2, FamilyID: "fam-a", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
		if err := st.CreateSession(ctx, s1); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSession(ctx, s1); !errors.Is(err, errExists) {
			t.Fatalf("duplicate token: %v", err)
		}
		got, err := st.SessionByHash(ctx, s1.TokenHash)
		if err != nil || got.AccountID != a.AccountID || got.TokenVersion != 2 || got.FamilyID != "fam-a" || !got.live(now) {
			t.Fatalf("session: %+v %v", got, err)
		}
		// Rotation retires t1 and links t2 into the chain.
		s2 := refreshSession{TokenHash: hashToken("t2"), AccountID: a.AccountID, TokenVersion: 2, FamilyID: "fam-a", ParentHash: s1.TokenHash, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
		if err := st.RotateSession(ctx, s1.TokenHash, s2, now); err != nil {
			t.Fatal(err)
		}
		old, _ := st.SessionByHash(ctx, s1.TokenHash)
		if old.live(now) || old.RevokeReason != revokeRotated || old.ReplacedBy != s2.TokenHash {
			t.Fatalf("rotated token: %+v", old)
		}
		if got, _ := st.SessionByHash(ctx, s2.TokenHash); !got.live(now) || got.ParentHash != s1.TokenHash {
			t.Fatalf("successor: %+v", got)
		}
		// A retired token cannot be rotated again.
		s3 := refreshSession{TokenHash: hashToken("t3"), AccountID: a.AccountID, FamilyID: "fam-a", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
		if err := st.RotateSession(ctx, s1.TokenHash, s3, now); !errors.Is(err, errNotFound) {
			t.Fatalf("rotate a retired token: %v", err)
		}
		// Exactly one of two concurrent rotations of the same token wins.
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i, tok := range []string{"race-a", "race-b"} {
			wg.Add(1)
			go func(i int, tok string) {
				defer wg.Done()
				next := refreshSession{TokenHash: hashToken(tok), AccountID: a.AccountID, FamilyID: "fam-a", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
				if err := st.RotateSession(ctx, s2.TokenHash, next, now); err == nil {
					wins.Add(1)
				} else if !errors.Is(err, errNotFound) {
					t.Errorf("rotation %d: %v", i, err)
				}
			}(i, tok)
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("%d concurrent rotations of one token succeeded, want 1", wins.Load())
		}

		// Family and account revocation.
		other := refreshSession{TokenHash: hashToken("other-family"), AccountID: a.AccountID, FamilyID: "fam-b", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
		if err := st.CreateSession(ctx, other); err != nil {
			t.Fatal(err)
		}
		if err := st.RevokeSessionFamily(ctx, "fam-a", revokeReuse, now); err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{"race-a", "race-b"} {
			if s, err := st.SessionByHash(ctx, hashToken(tok)); err == nil && s.live(now) {
				t.Fatalf("%s survived the family revocation", tok)
			}
		}
		if s, _ := st.SessionByHash(ctx, other.TokenHash); !s.live(now) {
			t.Fatal("another chain was revoked with the family")
		}
		if err := st.RevokeAccountSessions(ctx, a.AccountID, revokePassword, now); err != nil {
			t.Fatal(err)
		}
		if s, _ := st.SessionByHash(ctx, other.TokenHash); s.live(now) || s.RevokeReason != revokePassword {
			t.Fatalf("account revocation: %+v", s)
		}
		// Revoking keeps the first reason.
		if err := st.RevokeSession(ctx, other.TokenHash, revokeLogout, now); err != nil {
			t.Fatal(err)
		}
		if s, _ := st.SessionByHash(ctx, other.TokenHash); s.RevokeReason != revokePassword {
			t.Fatalf("revocation reason overwritten: %q", s.RevokeReason)
		}
		// Expiry.
		exp := refreshSession{TokenHash: hashToken("expired"), AccountID: a.AccountID, FamilyID: "fam-c", ExpiresAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Hour)}
		if err := st.CreateSession(ctx, exp); err != nil {
			t.Fatal(err)
		}
		if err := st.RotateSession(ctx, exp.TokenHash, refreshSession{TokenHash: hashToken("never"), AccountID: a.AccountID, FamilyID: "fam-c", ExpiresAt: now.Add(time.Hour)}, now); !errors.Is(err, errNotFound) {
			t.Fatalf("rotate an expired token: %v", err)
		}
	})
}

func TestStoreContractResetCodes(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		ctx, now := context.Background(), time.Now()
		a, err := st.CreateAccount(ctx, account{AccountName: "reset@euler.emoera.com", PasswordHash: "old", Status: 1, TokenVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.ResetPassword(ctx, a.AccountID, "h-any", "new", now); !errors.Is(err, errCodeInvalid) {
			t.Fatalf("no code issued: %v", err)
		}
		put := func(hash string, ttl time.Duration) {
			t.Helper()
			if err := st.PutResetCode(ctx, resetCode{AccountID: a.AccountID, CodeHash: hash, ExpiresAt: now.Add(ttl), CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
		put("h-good", 10*time.Minute)
		for i := 0; i < maxResetAttempts; i++ {
			if _, err := st.ResetPassword(ctx, a.AccountID, "h-bad", "new", now); !errors.Is(err, errCodeInvalid) {
				t.Fatalf("wrong guess %d: %v", i, err)
			}
		}
		// The budget is spent: even the right code is refused now.
		if _, err := st.ResetPassword(ctx, a.AccountID, "h-good", "new", now); !errors.Is(err, errCodeAttempts) {
			t.Fatalf("right code after %d wrong ones: %v", maxResetAttempts, err)
		}
		// A new code replaces the old one and resets the budget.
		put("h-fresh", 10*time.Minute)
		if _, err := st.ResetPassword(ctx, a.AccountID, "h-good", "new", now); !errors.Is(err, errCodeInvalid) {
			t.Fatalf("superseded code: %v", err)
		}
		plantSession(t, a, "reset-session", time.Hour)
		got, err := st.ResetPassword(ctx, a.AccountID, "h-fresh", "new-hash", now)
		if err != nil {
			t.Fatal(err)
		}
		if got.PasswordHash != "new-hash" || got.TokenVersion != 2 {
			t.Fatalf("after reset: %+v", got)
		}
		if cur := mustAccount(t, a.AccountID); cur.PasswordHash != "new-hash" || cur.TokenVersion != 2 {
			t.Fatalf("stored after reset: %+v", cur)
		}
		if s, _ := st.SessionByHash(ctx, hashToken("reset-session")); s.live(now) || s.RevokeReason != revokeReset {
			t.Fatalf("session after reset: %+v", s)
		}
		// Single use.
		if _, err := st.ResetPassword(ctx, a.AccountID, "h-fresh", "again", now); !errors.Is(err, errCodeInvalid) {
			t.Fatalf("code reused: %v", err)
		}
		// Expired.
		put("h-late", -time.Second)
		if _, err := st.ResetPassword(ctx, a.AccountID, "h-late", "late", now); !errors.Is(err, errCodeInvalid) {
			t.Fatalf("expired code: %v", err)
		}
	})
}

func TestStoreContractRAM(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		ctx := context.Background()
		acct := mustSeed(t, 410001, "ram-contract@euler.emoera.com", "", "pw-12345678").AccountID
		other := mustSeed(t, 410002, "ram-other@euler.emoera.com", "", "pw-12345678").AccountID

		// Policies: the system ones first, byte-exact documents.
		pols, err := st.ListPolicies(ctx, acct)
		if err != nil {
			t.Fatal(err)
		}
		sys := systemPolicies()
		if len(pols) != len(sys) {
			t.Fatalf("fresh account sees %d policies, want the %d system ones", len(pols), len(sys))
		}
		for i, p := range sys {
			if pols[i].ID != p.ID || pols[i].Name != p.Name || pols[i].Type != "system" || pols[i].Document != p.Document {
				t.Fatalf("system policy %d: %+v, want %+v (migration and systemPolicyDefs must agree)", i, pols[i], p)
			}
		}
		doc := "{ \"Version\": \"1\",\n  \"Statement\": [ {\"Effect\":\"Deny\", \"Action\":\"euecs:Delete*\", \"Resource\":\"*\"} ] }"
		parsed, err := authz.ParsePolicy([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		cp, err := st.CreatePolicy(ctx, storedPolicy{AccountID: acct, Name: "NoDelete", Document: doc, Parsed: parsed, CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if cp.ID < firstCustomPolicyID || cp.Type != "custom" {
			t.Fatalf("custom policy: %+v", cp)
		}
		for name, acc := range map[string]int64{"NoDelete": acct, "EuEcsFullAccess": acct} {
			if _, err := st.CreatePolicy(ctx, storedPolicy{AccountID: acc, Name: name, Document: doc, Parsed: parsed}); !errors.Is(err, errExists) {
				t.Fatalf("policy %q again: %v", name, err)
			}
		}
		// Another account may use the same custom name, and does not see ours.
		if _, err := st.CreatePolicy(ctx, storedPolicy{AccountID: other, Name: "NoDelete", Document: doc, Parsed: parsed}); err != nil {
			t.Fatal(err)
		}
		found, err := st.PoliciesByName(ctx, acct, []string{"NoDelete", "EuReadOnlyAccess", "Nope"})
		if err != nil || len(found) != 2 || found["NoDelete"].ID != cp.ID || found["NoDelete"].Document != doc {
			t.Fatalf("by name: %+v %v", found, err)
		}
		if _, ok := found["NoDelete"].Parsed.Statement, true; !ok || len(found["NoDelete"].Parsed.Statement) != 1 {
			t.Fatal("stored document lost its parsed form")
		}

		// Users.
		u, err := st.CreateRAMUser(ctx, ramUser{AccountID: acct, Username: "alice", DisplayName: "爱丽丝", Remark: "r", Status: 1,
			Policies: []string{"EuReadOnlyAccess", "NoDelete", "EuReadOnlyAccess"}, CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if u.ID == 0 || strings.Join(u.Policies, ",") != "EuReadOnlyAccess,NoDelete" {
			t.Fatalf("created user: %+v", u)
		}
		var pnf *policyNotFoundError
		if _, err := st.CreateRAMUser(ctx, ramUser{AccountID: acct, Username: "bob", Status: 1, Policies: []string{"Ghost", "EuEcsFullAccess"}}); !errors.As(err, &pnf) || strings.Join(pnf.names, ",") != "Ghost" {
			t.Fatalf("unknown policy: %v", err)
		}
		if _, err := st.CreateRAMUser(ctx, ramUser{AccountID: acct, Username: "alice", Status: 1}); !errors.Is(err, errExists) {
			t.Fatalf("duplicate username: %v", err)
		}
		bob, err := st.CreateRAMUser(ctx, ramUser{AccountID: acct, Username: "bob", Status: 2})
		if err != nil {
			t.Fatal(err)
		}
		list, err := st.ListRAMUsers(ctx, acct)
		if err != nil || len(list) != 2 || list[0].ID != u.ID || list[1].ID != bob.ID || len(list[1].Policies) != 0 || list[1].Policies == nil {
			t.Fatalf("list: %+v %v", list, err)
		}
		if got, err := st.RAMUserByName(ctx, acct, "alice"); err != nil || got.ID != u.ID || got.DisplayName != "爱丽丝" || got.Remark != "r" || len(got.Policies) != 2 {
			t.Fatalf("by name: %+v %v", got, err)
		}
		if _, err := st.RAMUserByID(ctx, other, u.ID); !errors.Is(err, errNotFound) {
			t.Fatalf("another account's user: %v", err)
		}
		// Soft delete frees the name; the row stays (status 3, renamed).
		if err := st.DeleteRAMUser(ctx, acct, u.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteRAMUser(ctx, acct, u.ID); !errors.Is(err, errAlreadyDeleted) {
			t.Fatalf("delete twice: %v", err)
		}
		if err := st.DeleteRAMUser(ctx, other, bob.ID); !errors.Is(err, errNotFound) {
			t.Fatalf("delete another account's user: %v", err)
		}
		gone, err := st.RAMUserByID(ctx, acct, u.ID)
		if err != nil || gone.Status != 3 || gone.Username != deletedUsername("alice", u.ID) {
			t.Fatalf("deleted user: %+v %v", gone, err)
		}
		if _, err := st.RAMUserByName(ctx, acct, "alice"); !errors.Is(err, errNotFound) {
			t.Fatalf("deleted user by name: %v", err)
		}
		again, err := st.CreateRAMUser(ctx, ramUser{AccountID: acct, Username: "alice", Status: 1})
		if err != nil || again.ID == u.ID {
			t.Fatalf("reuse a deleted name: %+v %v", again, err)
		}

		// Roles.
		r, err := st.CreateRAMRole(ctx, ramRole{AccountID: acct, Name: "Deployer", Description: "d", Policies: []string{"EuEcsFullAccess"}, CreatedAt: time.Now()})
		if err != nil || r.Status != 1 || r.ID == 0 {
			t.Fatalf("role: %+v %v", r, err)
		}
		if _, err := st.CreateRAMRole(ctx, ramRole{AccountID: acct, Name: "Deployer"}); !errors.Is(err, errExists) {
			t.Fatalf("duplicate role: %v", err)
		}
		if _, err := st.CreateRAMRole(ctx, ramRole{AccountID: acct, Name: "Ghosts", Policies: []string{"Ghost"}}); !errors.As(err, &pnf) {
			t.Fatalf("role with an unknown policy: %v", err)
		}
		if got, err := st.RAMRoleByName(ctx, acct, "Deployer"); err != nil || got.ID != r.ID || got.Description != "d" || strings.Join(got.Policies, ",") != "EuEcsFullAccess" {
			t.Fatalf("role by name: %+v %v", got, err)
		}
		if got, err := st.RAMRoleByID(ctx, acct, r.ID); err != nil || got.Name != "Deployer" {
			t.Fatalf("role by id: %+v %v", got, err)
		}
		if _, err := st.RAMRoleByID(ctx, other, r.ID); !errors.Is(err, errNotFound) {
			t.Fatalf("another account's role: %v", err)
		}
		if roles, err := st.ListRAMRoles(ctx, acct); err != nil || len(roles) != 1 {
			t.Fatalf("roles: %+v %v", roles, err)
		}
	})
}
