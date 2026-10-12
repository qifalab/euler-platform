package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/accesskey"
	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/totp"
)

// End-to-end HTTP flows over the full route table, run on both backends
// (forEachBackend): the parity tests for F1 and the acceptance tests for F2,
// F4 and F6.

const flowInternalToken = "flow-internal-token"

// flowClient is a browser-ish client: it keeps the refresh cookie and the
// bearer token between calls.
type flowClient struct {
	t      *testing.T
	h      http.Handler
	bearer string
	cookie string
	ip     string
}

func newFlowClient(t *testing.T) *flowClient {
	t.Helper()
	t.Setenv(httpmw.InternalTokenEnv, flowInternalToken)
	return &flowClient{t: t, h: requestIDMiddleware(newMux()), ip: "198.51.100.7"}
}

func (c *flowClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "eu_refresh", Value: c.cookie})
	}
	if strings.HasPrefix(path, "/internal/") {
		req.Header.Set(httpmw.InternalTokenHeader, flowInternalToken)
	}
	req.Header.Set("X-Real-IP", c.ip)
	rr := httptest.NewRecorder()
	c.h.ServeHTTP(rr, req)
	for _, ck := range rr.Result().Cookies() {
		if ck.Name == "eu_refresh" {
			c.cookie = ck.Value
			if ck.MaxAge < 0 {
				c.cookie = ""
			}
		}
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func dataOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	d, ok := out["Data"].(map[string]any)
	if !ok {
		t.Fatalf("no Data in %v", out)
	}
	return d
}

// signup registers and logs in, leaving the client holding a session.
func (c *flowClient) signup(email, password string) int64 {
	c.t.Helper()
	code, out := c.do("POST", "/api/auth/register", map[string]string{"email": email, "password": password})
	if code != 201 {
		c.t.Fatalf("register %s: %d %v", email, code, out)
	}
	c.login(email, password)
	return int64(dataOf(c.t, out)["id"].(float64))
}

func (c *flowClient) login(email, password string) {
	c.t.Helper()
	code, out := c.do("POST", "/api/auth/login", map[string]string{"email": email, "password": password})
	if code != 200 {
		c.t.Fatalf("login %s: %d %v", email, code, out)
	}
	c.bearer = dataOf(c.t, out)["accessToken"].(string)
}

func TestFlowSessionLifecycle(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		c := newFlowClient(t)
		c.signup("session-flow@euler.emoera.com", "first-pass-1")
		if code, out := c.do("GET", "/api/auth/session", nil); code != 200 {
			t.Fatalf("session: %d %v", code, out)
		}
		// Every refresh rotates the cookie.
		first := c.cookie
		if code, out := c.do("POST", "/api/auth/refresh", nil); code != 200 || c.cookie == first {
			t.Fatalf("refresh: %d %v (cookie rotated: %v)", code, out, c.cookie != first)
		}
		second := c.cookie
		// The superseded cookie inside the concurrent-refresh grace: just 401.
		c.cookie = first
		if code, _ := c.do("POST", "/api/auth/refresh", nil); code != 401 {
			t.Fatalf("superseded cookie: %d", code)
		}
		c.cookie = second
		if code, _ := c.do("POST", "/api/auth/refresh", nil); code != 200 {
			t.Fatalf("current cookie after a benign race: %d", code)
		}
		third := c.cookie
		// Outside the grace a superseded cookie is a replay: the whole chain
		// dies, including the legitimate holder's current cookie.
		saved := refreshReuseGrace
		refreshReuseGrace = 0
		t.Cleanup(func() { refreshReuseGrace = saved })
		time.Sleep(5 * time.Millisecond)
		c.cookie = second
		if code, _ := c.do("POST", "/api/auth/refresh", nil); code != 401 {
			t.Fatalf("replayed cookie: %d", code)
		}
		c.cookie = third
		if code, _ := c.do("POST", "/api/auth/refresh", nil); code != 401 {
			t.Fatalf("chain survived a replay: %d", code)
		}

		// Logout revokes.
		c.login("session-flow@euler.emoera.com", "first-pass-1")
		if code, _ := c.do("POST", "/api/auth/logout", nil); code != 200 || c.cookie != "" {
			t.Fatalf("logout: %d cookie=%q", code, c.cookie)
		}
		// Password change: old access token and every session die.
		c.login("session-flow@euler.emoera.com", "first-pass-1")
		oldBearer, oldCookie := c.bearer, c.cookie
		if code, out := c.do("PUT", "/api/account/password", map[string]string{"oldPassword": "first-pass-1", "newPassword": "second-pass-2"}); code != 200 {
			t.Fatalf("change password: %d %v", code, out)
		}
		c.bearer, c.cookie = oldBearer, oldCookie
		if code, _ := c.do("GET", "/api/account/profile", nil); code != 401 {
			t.Fatalf("old access token after password change: %d", code)
		}
		if code, _ := c.do("POST", "/api/auth/refresh", nil); code != 401 {
			t.Fatalf("old session after password change: %d", code)
		}
		c.login("session-flow@euler.emoera.com", "second-pass-2")
	})
}

func TestFlowRAMAccessKeysAndCheckAccess(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		c := newFlowClient(t)
		acct := c.signup("ram-flow@euler.emoera.com", "ram-flow-pass")
		other := newFlowClient(t)
		other.signup("ram-other@euler.emoera.com", "ram-other-pass")

		// A fresh account sees the system policies.
		code, out := c.do("GET", "/api/ram/policies", nil)
		if code != 200 || dataOf(t, out)["total"].(float64) != float64(len(systemPolicyDefs)) {
			t.Fatalf("policies of a fresh account: %d %v", code, out)
		}
		deny := `{"Version":"1","Statement":[{"Effect":"Deny","Action":"euecs:DeleteInstance","Resource":"*"}]}`
		if code, out := c.do("POST", "/api/ram/policies", map[string]string{"name": "DenyDelete", "document": deny}); code != 201 || dataOf(t, out)["document"] != deny {
			t.Fatalf("create policy: %d %v", code, out)
		}
		if code, out := c.do("POST", "/api/ram/users", map[string]any{"username": "ghost-user", "policies": []string{"Nope"}}); code != 404 || out["Code"] != "Auth.PolicyNotFound" {
			t.Fatalf("user with an unknown policy: %d %v", code, out)
		}
		if code, _ := c.do("POST", "/api/ram/users", map[string]any{"username": "bad name!"}); code != 400 {
			t.Fatalf("invalid username: %d", code)
		}
		code, out = c.do("POST", "/api/ram/users", map[string]any{"username": "deployer", "displayName": "发布机器人", "policies": []string{"EuEcsFullAccess", "DenyDelete"}})
		if code != 201 {
			t.Fatalf("create user: %d %v", code, out)
		}
		uid := int64(dataOf(t, out)["id"].(float64))
		if code, _ := c.do("POST", "/api/ram/users", map[string]any{"username": "deployer"}); code != 409 {
			t.Fatalf("duplicate user: %d", code)
		}
		code, out = c.do("GET", "/api/ram/users", nil)
		if code != 200 || dataOf(t, out)["total"].(float64) != 1 {
			t.Fatalf("list users: %d %v", code, out)
		}
		if code, out := c.do("POST", "/api/ram/roles", map[string]any{"name": "Deployer", "policies": []string{"EuEcsFullAccess"}}); code != 201 || len(dataOf(t, out)["policies"].([]any)) != 1 {
			t.Fatalf("create role: %d %v", code, out)
		}
		if code, out := c.do("POST", "/api/ram/simulate", map[string]any{"policies": []string{"EuEcsFullAccess", "DenyDelete"}, "action": "euecs:DeleteInstance", "resource": "*"}); code != 200 || dataOf(t, out)["allowed"] != false {
			t.Fatalf("simulate: %d %v", code, out)
		}

		// Master-account keys: the shared lifecycle, 2 keys per identity.
		var keys []string
		for i := 0; i < 2; i++ {
			code, out := c.do("POST", "/api/ak", map[string]any{})
			d := dataOf(t, out)
			if code != 201 || len(d["secretKey"].(string)) != 40 || !strings.HasPrefix(d["akId"].(string), accesskey.Prefix) || d["status"].(float64) != 1 {
				t.Fatalf("create AK: %d %v", code, out)
			}
			keys = append(keys, d["akId"].(string))
		}
		if code, out := c.do("POST", "/api/ak", nil); code != 409 || out["Code"] != "Auth.AKLimitExceeded" {
			t.Fatalf("third AK: %d %v", code, out)
		}
		code, out = c.do("GET", "/api/ak", nil)
		list := dataOf(t, out)["accessKeys"].([]any)
		if code != 200 || len(list) != 2 {
			t.Fatalf("list AKs: %d %v", code, out)
		}
		row := list[0].(map[string]any)
		if row["status"].(float64) != 1 || row["rotationGrace"] != false || row["lastUsed"] != nil || row["createdAt"] == "" {
			t.Fatalf("AK row: %v", row)
		}
		if code, out := c.do("POST", "/api/ak/"+keys[0]+"/disable", nil); code != 200 || dataOf(t, out)["status"].(float64) != 2 {
			t.Fatalf("disable: %d %v", code, out)
		}
		if code, out := c.do("POST", "/api/ak/"+keys[0]+"/rotate", nil); code != 409 || out["Code"] != "Auth.AKDisabled" {
			t.Fatalf("rotate a disabled key: %d %v", code, out)
		}
		if code, _ := c.do("POST", "/api/ak/"+keys[0]+"/enable", nil); code != 200 {
			t.Fatalf("enable: %d", code)
		}
		code, out = c.do("POST", "/api/ak/"+keys[0]+"/rotate", nil)
		if code != 201 || dataOf(t, out)["oldAkId"] != keys[0] || dataOf(t, out)["secretKey"] == "" {
			t.Fatalf("rotate: %d %v", code, out)
		}
		_, out = c.do("GET", "/api/ak", nil)
		inGrace := 0
		for _, r := range dataOf(t, out)["accessKeys"].([]any) {
			if m := r.(map[string]any); m["rotationGrace"] == true && m["akId"] == keys[0] && m["graceUntil"] != nil {
				inGrace++
			}
		}
		if inGrace != 1 {
			t.Fatalf("superseded key not in grace: %v", out)
		}
		if code, _ := other.do("POST", "/api/ak/"+keys[1]+"/disable", nil); code != 404 {
			t.Fatalf("another account disabled our key: %d", code)
		}
		if code, _ := c.do("DELETE", "/api/ak/"+keys[0], nil); code != 200 {
			t.Fatalf("delete: %d", code)
		}
		if code, _ := c.do("DELETE", "/api/ak/"+keys[0], nil); code != 404 {
			t.Fatalf("delete twice: %d", code)
		}
		if _, out := c.do("GET", "/api/ak", nil); dataOf(t, out)["total"].(float64) != 2 {
			t.Fatalf("after delete: %v", out)
		}

		// RAM-user key: carries the owner, verifies as user/id:<n>, and is
		// judged by the user's attached policies.
		code, out = c.do("POST", "/api/ak", map[string]any{"ramUserId": uid})
		if code != 201 || dataOf(t, out)["ramUserId"].(float64) != float64(uid) {
			t.Fatalf("RAM-user AK: %d %v", code, out)
		}
		userAK, userSK := dataOf(t, out)["akId"].(string), dataOf(t, out)["secretKey"].(string)
		if _, out := c.do("GET", "/api/ak?ramUserId="+strconv.FormatInt(uid, 10), nil); dataOf(t, out)["total"].(float64) != 1 {
			t.Fatalf("RAM-user key list: %v", out)
		}
		if code, _ := other.do("POST", "/api/ak", map[string]any{"ramUserId": uid}); code != 404 {
			t.Fatalf("key for another account's user: %d", code)
		}
		verifier := accesskey.NewManager(akStore, keyring, nil) // what cmd/verify runs over the same store
		sk, rec, err := verifier.ResolveSK(userAK, time.Now())
		if err != nil || sk != userSK || rec.OwnerType != accesskey.OwnerRAMUser || rec.OwnerID != uid || rec.AccountID != acct {
			t.Fatalf("verifier view of the console key: %+v %v", rec, err)
		}
		identity := "user/id:" + strconv.FormatInt(uid, 10)
		for action, want := range map[string]bool{"euecs:RunInstances": true, "euecs:DeleteInstance": false, "euoss:GetObject": false} {
			code, out := c.do("POST", "/internal/authz/check", map[string]any{"account_id": strconv.FormatInt(acct, 10), "identity": identity, "action": action})
			if code != 200 || out["allow"] != want {
				t.Fatalf("check %s: %d %v, want allow=%v", action, code, out, want)
			}
		}
		// Deleting the user removes its keys and its permissions.
		if code, _ := c.do("DELETE", "/api/ram/users/"+strconv.FormatInt(uid, 10), nil); code != 200 {
			t.Fatalf("delete user: %d", code)
		}
		if _, _, err := verifier.ResolveSK(userAK, time.Now()); !errors.Is(err, accesskey.ErrAlreadyDeleted) {
			t.Fatalf("deleted user's key still resolves: %v", err)
		}
		if _, out := c.do("POST", "/internal/authz/check", map[string]any{"account_id": acct, "identity": identity, "action": "euecs:RunInstances"}); out["allow"] != false {
			t.Fatalf("deleted user still allowed: %v", out)
		}
	})
}

func TestFlowMFA(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		c := newFlowClient(t)
		id := c.signup("mfa-flow@euler.emoera.com", "mfa-flow-pass")
		code, out := c.do("POST", "/api/mfa/bind", nil)
		if code != 200 {
			t.Fatalf("bind: %d %v", code, out)
		}
		seed, err := totp.ParseSecretBase32(dataOf(t, out)["secret"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if code, out := c.do("POST", "/api/mfa/verify", map[string]string{"code": totp.Code(seed, time.Now())}); code != 200 {
			t.Fatalf("activate: %d %v", code, out)
		}
		totpVerifier.Forget(mfaReplayKey(id))
		code, out = c.do("POST", "/api/auth/login", map[string]string{"email": "mfa-flow@euler.emoera.com", "password": "mfa-flow-pass"})
		if code != 200 || dataOf(t, out)["mfaRequired"] != true {
			t.Fatalf("login with MFA: %d %v", code, out)
		}
		code, out = c.do("POST", "/api/auth/mfa/verify", map[string]string{"mfaToken": dataOf(t, out)["mfaToken"].(string), "code": totp.Code(seed, time.Now())})
		if code != 200 || dataOf(t, out)["accessToken"] == nil {
			t.Fatalf("second factor: %d %v", code, out)
		}
		c.bearer = dataOf(t, out)["accessToken"].(string)
		totpVerifier.Forget(mfaReplayKey(id))
		if code, out := c.do("POST", "/api/mfa/unbind", map[string]string{"code": totp.Code(seed, time.Now())}); code != 200 {
			t.Fatalf("unbind: %d %v", code, out)
		}
		if _, out := c.do("GET", "/api/account/profile", nil); dataOf(t, out)["mfaEnabled"] != false {
			t.Fatalf("profile after unbind: %v", out)
		}
	})
}

// F4: GET /internal/accounts/{id}/realname.
func TestFlowInternalRealname(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		c := newFlowClient(t)
		id := c.signup("realname-flow@euler.emoera.com", "realname-pass")
		path := "/internal/accounts/" + strconv.FormatInt(id, 10) + "/realname"
		code, out := c.do("GET", path, nil)
		if code != 200 || out["Code"] != "OK" {
			t.Fatalf("unverified: %d %v", code, out)
		}
		if d := dataOf(t, out); d["verified"] != false || d["type"] != "" || d["identityKey"] != "" {
			t.Fatalf("unverified payload: %v", d)
		}
		if code, out := c.do("POST", "/api/realname/verify", map[string]string{"type": "individual", "name": "张三", "idCardNo": "110101199001011237"}); code != 200 {
			t.Fatalf("verify: %d %v", code, out)
		}
		_, out = c.do("GET", path, nil)
		if d := dataOf(t, out); d["verified"] != true || d["type"] != "personal" || d["identityKey"] != identityFingerprint("id", "110101199001011237") {
			t.Fatalf("verified payload: %v", d)
		}
		ent := newFlowClient(t)
		eid := ent.signup("realname-ent@euler.emoera.com", "realname-pass")
		if code, out := ent.do("POST", "/api/realname/verify", map[string]string{"type": "enterprise", "enterpriseName": "欧拉科技", "creditCode": "91350100M000100Y43"}); code != 200 {
			t.Fatalf("enterprise verify: %d %v", code, out)
		}
		_, out = ent.do("GET", "/internal/accounts/"+strconv.FormatInt(eid, 10)+"/realname", nil)
		if d := dataOf(t, out); d["type"] != "enterprise" || d["identityKey"] != identityFingerprint("uscc", "91350100M000100Y43") {
			t.Fatalf("enterprise payload: %v", d)
		}
		if code, out := c.do("GET", "/internal/accounts/987654321/realname", nil); code != 404 || out["Code"] != "Auth.AccountNotFound" {
			t.Fatalf("unknown account: %d %v", code, out)
		}
		if code, _ := c.do("GET", "/internal/accounts/abc/realname", nil); code != 400 {
			t.Fatalf("malformed id: %d", code)
		}
		// Internal token only.
		req := httptest.NewRequest("GET", path, nil)
		rr := httptest.NewRecorder()
		c.h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("without the internal token: %d", rr.Code)
		}
	})
}

// recordingMailer captures reset and registration codes instead of mailing
// them.
type recordingMailer struct {
	mu       sync.Mutex
	sent     map[string][]string // email → reset codes
	register map[string][]string // email → registration codes
}

func (m *recordingMailer) SendRegisterCode(_ context.Context, email, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.register[email] = append(m.register[email], code)
	return nil
}

// registerCodes returns the registration codes mailed to email.
func (m *recordingMailer) registerCodes(email string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.register[email]...)
}

func (m *recordingMailer) SendResetCode(_ context.Context, _ int64, email, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent[email] = append(m.sent[email], code)
	return nil
}

func (m *recordingMailer) last(t *testing.T, email string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	codes := m.sent[email]
	if len(codes) == 0 {
		t.Fatalf("no code mailed to %s", email)
	}
	return codes[len(codes)-1]
}

func useRecordingMailer(t *testing.T) *recordingMailer {
	t.Helper()
	m := &recordingMailer{sent: map[string][]string{}, register: map[string][]string{}}
	saved := resetMailer
	resetMailer = m
	t.Cleanup(func() { resetMailer = saved })
	return m
}

// F2: forgot password.
func TestFlowForgotPassword(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		mail := useRecordingMailer(t)
		c := newFlowClient(t)
		const email = "forgot-flow@euler.emoera.com"
		c.signup(email, "old-password-1")
		oldBearer, oldCookie := c.bearer, c.cookie

		// No enumeration: an unknown address gets the same 200, and nothing is sent.
		for _, e := range []string{"nobody-at-all@euler.emoera.com", email} {
			if code, out := c.do("POST", "/api/auth/forgot/send-code", map[string]string{"email": e}); code != 200 || out["Code"] != "OK" {
				t.Fatalf("send-code %s: %d %v", e, code, out)
			}
		}
		if len(mail.sent["nobody-at-all@euler.emoera.com"]) != 0 {
			t.Fatal("a code was mailed to an unregistered address")
		}
		code1 := mail.last(t, email)
		if !validResetCode(code1) {
			t.Fatalf("mailed code %q is not 6 digits", code1)
		}
		// Meanwhile someone hammers the login: locked.
		for i := 0; i < 5; i++ {
			c.do("POST", "/api/auth/login", map[string]string{"email": email, "password": "wrong-guess"})
		}
		if code, _ := c.do("POST", "/api/auth/login", map[string]string{"email": email, "password": "old-password-1"}); code != 429 {
			t.Fatalf("lockout not engaged: %d", code)
		}
		wrong := "000000"
		if wrong == code1 {
			wrong = "111111"
		}
		if code, out := c.do("POST", "/api/auth/forgot/reset", map[string]string{"email": email, "code": wrong, "password": "new-password-2"}); code != 400 || out["Code"] != "Auth.InvalidCode" {
			t.Fatalf("wrong code: %d %v", code, out)
		}
		if code, out := c.do("POST", "/api/auth/forgot/reset", map[string]string{"email": email, "code": code1, "password": "short"}); code != 400 || out["Code"] != "Common.InvalidParameter" {
			t.Fatalf("weak password: %d %v", code, out)
		}
		// A new send supersedes the old code.
		c.do("POST", "/api/auth/forgot/send-code", map[string]string{"email": email})
		code2 := mail.last(t, email)
		if code2 != code1 {
			if code, _ := c.do("POST", "/api/auth/forgot/reset", map[string]string{"email": email, "code": code1, "password": "new-password-2"}); code != 400 {
				t.Fatalf("superseded code accepted: %d", code)
			}
		}
		if code, out := c.do("POST", "/api/auth/forgot/reset", map[string]string{"email": email, "code": code2, "password": "new-password-2"}); code != 200 || out["Code"] != "OK" {
			t.Fatalf("reset: %d %v", code, out)
		}
		// Single use; old credentials, tokens and sessions are dead; the
		// lockout is lifted.
		if code, _ := c.do("POST", "/api/auth/forgot/reset", map[string]string{"email": email, "code": code2, "password": "new-password-3"}); code != 400 {
			t.Fatalf("code reused: %d", code)
		}
		c.bearer, c.cookie = oldBearer, oldCookie
		if code, _ := c.do("GET", "/api/account/profile", nil); code != 401 {
			t.Fatalf("access token minted before the reset: %d", code)
		}
		if code, _ := c.do("POST", "/api/auth/refresh", nil); code != 401 {
			t.Fatalf("session minted before the reset: %d", code)
		}
		if code, _ := c.do("POST", "/api/auth/login", map[string]string{"email": email, "password": "old-password-1"}); code != 401 {
			t.Fatalf("old password after reset: %d", code)
		}
		c.login(email, "new-password-2")

		// Five wrong guesses, then 429 — identically for a registered and an
		// unregistered address (no enumeration through the limit either).
		for _, e := range []string{email, "nobody-at-all@euler.emoera.com"} {
			c.do("POST", "/api/auth/forgot/send-code", map[string]string{"email": e})
			for i := 0; i < maxResetAttempts; i++ {
				if code, _ := c.do("POST", "/api/auth/forgot/reset", map[string]string{"email": e, "code": "999999", "password": "new-password-9"}); code != 400 {
					t.Fatalf("%s guess %d: %d", e, i, code)
				}
			}
			if code, out := c.do("POST", "/api/auth/forgot/reset", map[string]string{"email": e, "code": "999999", "password": "new-password-9"}); code != 429 {
				t.Fatalf("%s after %d guesses: %d %v, want 429", e, maxResetAttempts, code, out)
			}
		}
		// Expired code.
		a := mustAccount(t, mustAccountByName(t, email).AccountID)
		if err := st.PutResetCode(context.Background(), resetCode{AccountID: a.AccountID, CodeHash: resetCodeHash(a.AccountID, "424242"), ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
			t.Fatal(err)
		}
		forgotResetEmailLimiter.Reset("forgot-reset:" + email)
		if code, _ := c.do("POST", "/api/auth/forgot/reset", map[string]string{"email": email, "code": "424242", "password": "new-password-9"}); code != 400 {
			t.Fatalf("expired code: %d", code)
		}
		// Sending is rate limited per address (5 per hour).
		c2 := newFlowClient(t)
		c2.ip = "203.0.113.9"
		var last int
		for i := 0; i < 6; i++ {
			last, _ = c2.do("POST", "/api/auth/forgot/send-code", map[string]string{"email": "bomb@euler.emoera.com"})
		}
		if last != 429 {
			t.Fatalf("6th send in an hour: %d, want 429", last)
		}
	})
}

func mustAccountByName(t *testing.T, email string) account {
	t.Helper()
	a, err := st.AccountByName(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
