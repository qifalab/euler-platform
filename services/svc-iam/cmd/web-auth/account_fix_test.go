package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/totp"
)

// Regression (I6): the accounts map was keyed by the raw email while the
// lockout key lowercased it, so "Alice@x" and "alice@x" were two accounts
// (and a duplicate key in account_db, whose unique index ignores case).
func TestEmailIsCaseInsensitive(t *testing.T) {
	code, out := postJSON(t, handleRegister, "/api/auth/register", "", map[string]string{"email": "  Case.Mixed@Euler.Emoera.COM ", "password": "case-pass-123"})
	if code != 201 {
		t.Fatalf("register: %d %v", code, out)
	}
	if got := out["Data"].(map[string]any)["email"]; got != "case.mixed@euler.emoera.com" {
		t.Fatalf("stored email %v, want the normalized form", got)
	}
	if code, _ := postJSON(t, handleRegister, "/api/auth/register", "", map[string]string{"email": "case.mixed@euler.emoera.com", "password": "other-pass-123"}); code != 409 {
		t.Fatalf("re-register with another case: %d, want 409", code)
	}
	for _, login := range []string{"CASE.MIXED@EULER.EMOERA.COM", " case.mixed@euler.emoera.com"} {
		if code, out := postJSON(t, handleLogin, "/api/auth/login", "", map[string]string{"email": login, "password": "case-pass-123"}); code != 200 {
			t.Fatalf("login as %q: %d %v", login, code, out)
		}
	}
}

// Regression (I7): register copied the email into RealName, so a fresh
// account looked verified and the email leaked into the real-name field.
func TestFreshAccountHasNoRealName(t *testing.T) {
	code, out := postJSON(t, handleRegister, "/api/auth/register", "", map[string]string{"email": "fresh-realname@euler.emoera.com", "password": "fresh-pass-123"})
	if code != 201 {
		t.Fatalf("register: %d %v", code, out)
	}
	code, out = postJSON(t, handleLogin, "/api/auth/login", "", map[string]string{"email": "fresh-realname@euler.emoera.com", "password": "fresh-pass-123"})
	if code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	d := out["Data"].(map[string]any)
	if rn := d["user"].(map[string]any)["realName"]; rn != "" {
		t.Fatalf("login user.realName = %v, want empty", rn)
	}
	bearer, _ := d["accessToken"].(string)
	req := httptest.NewRequest("GET", "/api/account/profile", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rr := httptest.NewRecorder()
	handleAccountProfile(rr, req)
	if rr.Code != 200 {
		t.Fatalf("profile: %d %s", rr.Code, rr.Body)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"realName":""`) || !strings.Contains(body, `"realNameStatus":0`) {
		t.Fatalf("profile of a fresh account: %s", body)
	}
	if !strings.Contains(body, `"name":"fresh-realname@euler.emoera.com"`) {
		t.Fatalf("profile lost the display name: %s", body)
	}
}

// Regression (I10): the refresh cookie never carried a Domain, so the session
// minted on the account site did not reach the console (SSO).
func TestRefreshCookieDomain(t *testing.T) {
	t.Setenv("EULER_COOKIE_DOMAIN", " .euler.emoera.com ")
	if got := loadCookieDomain(); got != ".euler.emoera.com" {
		t.Fatalf("loadCookieDomain = %q", got)
	}
	saved := cookieDomain
	defer func() { cookieDomain = saved }()

	cookieDomain = ".euler.emoera.com"
	rr := httptest.NewRecorder()
	setRefreshCookie(rr, "tok", time.Now().Add(time.Hour))
	clearRefreshCookie(rr)
	cookies := rr.Result().Header.Values("Set-Cookie")
	if len(cookies) != 2 {
		t.Fatalf("Set-Cookie headers: %v", cookies)
	}
	for _, c := range cookies {
		if !strings.Contains(c, "Domain=euler.emoera.com") {
			t.Fatalf("cookie without the SSO domain (set and clear must match): %s", c)
		}
	}

	cookieDomain = "" // unset: host-only, as before
	rr = httptest.NewRecorder()
	setRefreshCookie(rr, "tok", time.Now().Add(time.Hour))
	if c := rr.Result().Header.Get("Set-Cookie"); strings.Contains(c, "Domain=") {
		t.Fatalf("host-only cookie carries a Domain: %s", c)
	}
}

// Regression (I11): activation re-read the account and enabled whatever seed
// was stored, so a re-bind between verification and activation armed a seed
// that no code had been verified against.
func TestMFAActivationRequiresVerifiedSeed(t *testing.T) {
	bearer := seedMFAAccount(t, 600001, "mfa-rebind@euler.emoera.com")
	id := accountIDOfBearer(bearer)
	bindMFA(t, bearer)
	first := mustAccount(t, id)
	seedB := bindMFA(t, bearer) // concurrent re-bind replaces the seed

	if err := enableVerifiedMFA(context.Background(), id, first.MFASecretCipher, first.MFAKeyVersion); err == nil || err.Error() != "Auth.MFASeedChanged" {
		t.Fatalf("activation with a superseded seed: %v", err)
	}
	enabled := mustAccount(t, id).MFAEnabled
	if enabled {
		t.Fatal("an unverified seed was activated")
	}
	// The current seed still activates normally.
	code, out, _ := mfaDo(t, "POST", "/api/mfa/verify", bearer, map[string]any{"code": totp.Code(seedB, time.Now())})
	if code != 200 {
		t.Fatalf("activation with the current seed: %d %v", code, out)
	}
}

// Regression (I12): limiter entries were only removed by Reset, so an email
// spray grew the map forever. Expired entries are now swept; live lockouts
// and in-window failures are kept.
func TestAttemptLimiterPrunesExpiredEntries(t *testing.T) {
	l := newAttemptLimiter(5, time.Minute, time.Minute)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5000; i++ {
		l.Fail(fmt.Sprintf("login:spray-%d@x.com", i), t0)
	}
	for i := 0; i < 5; i++ {
		l.Fail("login:locked@x.com", t0)
	}
	if !l.Locked("login:locked@x.com", t0) {
		t.Fatal("five failures did not lock")
	}
	// Inside the window nothing may be forgotten.
	l.Fail("login:trigger@x.com", t0.Add(30*time.Second))
	if len(l.state) < 5000 {
		t.Fatalf("entries pruned inside the window: %d", len(l.state))
	}
	// Past window and lockout every spray entry is dead weight.
	later := t0.Add(2*time.Minute + time.Second)
	l.Fail("login:after@x.com", later)
	l.mu.Lock()
	n := len(l.state)
	l.mu.Unlock()
	if n > 2 {
		t.Fatalf("%d entries left after expiry, want only the fresh ones", n)
	}
	// A lockout that is still running survives a sweep.
	l2 := newAttemptLimiter(5, time.Minute, time.Hour)
	for i := 0; i < 5; i++ {
		l2.Fail("login:long@x.com", t0)
	}
	l2.Fail("login:other@x.com", t0.Add(5*time.Minute))
	if !l2.Locked("login:long@x.com", t0.Add(5*time.Minute)) {
		t.Fatal("an active lockout was pruned")
	}
}
