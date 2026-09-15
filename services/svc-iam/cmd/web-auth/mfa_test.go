package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/totp"
)

// The MFA tests drive the full two-phase bind and the two-step login over
// HTTP, with real TOTP math: bind returns the seed exactly once (as an
// authenticator QR would), the tests parse it, compute real codes, and feed
// them back. Each test seeds a FRESH account id so the process-global TOTP
// replay memory (keyed per account) cannot bleed between tests.

var mfaPass = "mfatest1234"

// mfaMux registers exactly the routes the MFA flow needs, wrapped in the same
// middleware chain as production.
func mfaMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", handleLogin)
	mux.HandleFunc("POST /api/auth/mfa/verify", handleMFALoginVerify)
	mux.HandleFunc("POST /api/mfa/bind", handleMFABind)
	mux.HandleFunc("POST /api/mfa/verify", handleMFABindVerify)
	mux.HandleFunc("POST /api/mfa/unbind", handleMFAUnbind)
	mux.HandleFunc("GET /api/account/profile", handleAccountProfile)
	return requestIDMiddleware(mux)
}

// seedMFAAccount provisions a fresh account and returns its bearer token.
func seedMFAAccount(t *testing.T, id int64, email string) string {
	t.Helper()
	seedAccount(id, email, "MFA测试账号", mfaPass)
	mu.RLock()
	a := byID[id]
	mu.RUnlock()
	if a.AccountID == 0 {
		t.Fatalf("account %d not seeded", id)
	}
	return issueAccessToken(a, time.Now())
}

func mfaDo(t *testing.T, method, path, bearer string, body any) (int, map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("X-Euler-TraceId", "mfa-t")
	rr := httptest.NewRecorder()
	mfaMux().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out, rr
}

// bindMFA drives bind and returns (bearer, parsedSeed).
func bindMFA(t *testing.T, bearer string) []byte {
	t.Helper()
	code, out, _ := mfaDo(t, "POST", "/api/mfa/bind", bearer, nil)
	if code != 200 {
		t.Fatalf("bind failed: %d %v", code, out)
	}
	d, _ := out["Data"].(map[string]any)
	s32, _ := d["secret"].(string)
	seed, err := totp.ParseSecretBase32(s32)
	if err != nil {
		t.Fatalf("bind returned unusable secret: %v", err)
	}
	return seed
}

// activateMFA binds + verifies, leaving the account fully MFA-gated.
//
// The trailing Forget resets the replay memory for the account: activation
// consumes the current 30s timestep, and the tests that follow verify within
// the SAME timestep (they run in milliseconds). Forgetting simulates "the user
// bound the factor earlier" so each test exercises a fresh timestep — the
// replay protection itself is covered by TestMFALoginCodeReplayRejected on a
// clean slate.
func activateMFA(t *testing.T, bearer string) []byte {
	t.Helper()
	seed := bindMFA(t, bearer)
	code, out, _ := mfaDo(t, "POST", "/api/mfa/verify", bearer, map[string]any{"code": totp.Code(seed, time.Now())})
	if code != 200 {
		t.Fatalf("activation failed: %d %v", code, out)
	}
	mu.RLock()
	a := byID[accountIDOfBearer(bearer)]
	mu.RUnlock()
	totpVerifier.Forget(mfaReplayKey(a.AccountID))
	return seed
}

// accountIDOfBearer decodes the sub claim without verifying (test helper —
// the token was minted by seedMFAAccount one line earlier).
func accountIDOfBearer(bearer string) int64 {
	parts := strings.Split(bearer, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var p struct {
		Sub string `json:"sub"`
	}
	_ = json.Unmarshal(payload, &p)
	id, _ := strconv.ParseInt(p.Sub, 10, 64)
	return id
}

func TestMFABindReturnsSecretAndPendingState(t *testing.T) {
	bearer := seedMFAAccount(t, 300001, "bind@euler.emoera.com")
	code, out, _ := mfaDo(t, "POST", "/api/mfa/bind", bearer, nil)
	if code != 200 {
		t.Fatalf("bind failed: %d %v", code, out)
	}
	d, _ := out["Data"].(map[string]any)
	s32, _ := d["secret"].(string)
	if len(s32) != 32 { // 20 bytes → 32 base32 chars
		t.Errorf("secret length = %d, want 32", len(s32))
	}
	if uri, _ := d["provisioningUri"].(string); !strings.HasPrefix(uri, "otpauth://totp/Euler:bind@euler.emoera.com?") {
		t.Errorf("provisioning URI malformed: %s", uri)
	}
	// Pending: bound but not activated — profile must still say false.
	code, out, _ = mfaDo(t, "GET", "/api/account/profile", bearer, nil)
	if code != 200 {
		t.Fatalf("profile failed: %d %v", code, out)
	}
	d, _ = out["Data"].(map[string]any)
	if d["mfaEnabled"] != false {
		t.Errorf("mfaEnabled after bind-only = %v, want false (two-phase activation)", d["mfaEnabled"])
	}
}

func TestMFAVerifyActivatesFactor(t *testing.T) {
	bearer := seedMFAAccount(t, 300002, "activate@euler.emoera.com")
	seed := bindMFA(t, bearer)

	// Wrong code first: rejected, and it must not consume anything.
	if code, out, _ := mfaDo(t, "POST", "/api/mfa/verify", bearer, map[string]any{"code": "000000"}); code != 401 {
		t.Fatalf("wrong code accepted: %d %v", code, out)
	}
	code, out, _ := mfaDo(t, "POST", "/api/mfa/verify", bearer, map[string]any{"code": totp.Code(seed, time.Now())})
	if code != 200 {
		t.Fatalf("activation with real code failed: %d %v", code, out)
	}
	// Profile now reports the factor live.
	_, out, _ = mfaDo(t, "GET", "/api/account/profile", bearer, nil)
	d, _ := out["Data"].(map[string]any)
	if d["mfaEnabled"] != true {
		t.Fatalf("mfaEnabled after activation = %v, want true", d["mfaEnabled"])
	}
}

func TestMFABindRejectedWhileActive(t *testing.T) {
	bearer := seedMFAAccount(t, 300003, "rebind@euler.emoera.com")
	activateMFA(t, bearer)
	code, out, _ := mfaDo(t, "POST", "/api/mfa/bind", bearer, nil)
	if code != 409 || out["Code"] != "Auth.MFAAlreadyBound" {
		t.Fatalf("re-bind while active should 409 Auth.MFAAlreadyBound, got %d %v", code, out)
	}
}

func TestMFALoginBecomesTwoStep(t *testing.T) {
	bearer := seedMFAAccount(t, 300004, "login2step@euler.emoera.com")
	seed := activateMFA(t, bearer)

	// Step 1: correct password alone no longer mints a session.
	code, out, _ := mfaDo(t, "POST", "/api/auth/login", "", map[string]any{"email": "login2step@euler.emoera.com", "password": mfaPass})
	if code != 200 {
		t.Fatalf("login failed: %d %v", code, out)
	}
	d, _ := out["Data"].(map[string]any)
	if d["mfaRequired"] != true {
		t.Fatalf("mfaRequired missing: %v", d)
	}
	if _, has := d["accessToken"]; has {
		t.Fatal("password-only login must not return accessToken when MFA is active")
	}
	mfaToken, _ := d["mfaToken"].(string)
	if mfaToken == "" {
		t.Fatal("no mfaToken in challenge response")
	}

	// Step 2: challenge + real code mints the session.
	code, out, rr := mfaDo(t, "POST", "/api/auth/mfa/verify", "", map[string]any{"mfaToken": mfaToken, "code": totp.Code(seed, time.Now())})
	if code != 200 {
		t.Fatalf("mfa login verify failed: %d %v", code, out)
	}
	d, _ = out["Data"].(map[string]any)
	if _, has := d["accessToken"]; !has {
		t.Fatal("mfa login verify returned no accessToken")
	}
	if !strings.Contains(rr.Header().Get("Set-Cookie"), "eu_refresh=") {
		t.Error("mfa login verify must set the refresh cookie, same as password login")
	}
}

func TestMFALoginWrongPasswordStillRejected(t *testing.T) {
	bearer := seedMFAAccount(t, 300005, "wrongpw@euler.emoera.com")
	activateMFA(t, bearer)
	// MFA never weakens step 1: wrong password → same 401 as before.
	code, out, _ := mfaDo(t, "POST", "/api/auth/login", "", map[string]any{"email": "wrongpw@euler.emoera.com", "password": "not-the-password"})
	if code != 401 || out["Code"] != "Auth.InvalidCredentials" {
		t.Fatalf("want 401 Auth.InvalidCredentials, got %d %v", code, out)
	}
}

func TestMFALoginRejectsWrongThenAcceptsRight(t *testing.T) {
	bearer := seedMFAAccount(t, 300006, "wrongcode@euler.emoera.com")
	seed := activateMFA(t, bearer)
	_, out, _ := mfaDo(t, "POST", "/api/auth/login", "", map[string]any{"email": "wrongcode@euler.emoera.com", "password": mfaPass})
	d, _ := out["Data"].(map[string]any)
	mfaToken, _ := d["mfaToken"].(string)

	if code, out, _ := mfaDo(t, "POST", "/api/auth/mfa/verify", "", map[string]any{"mfaToken": mfaToken, "code": "000000"}); code != 401 {
		t.Fatalf("wrong code accepted: %d %v", code, out)
	}
	// The failed attempt consumed nothing — the real code still works.
	code, out, _ := mfaDo(t, "POST", "/api/auth/mfa/verify", "", map[string]any{"mfaToken": mfaToken, "code": totp.Code(seed, time.Now())})
	if code != 200 {
		t.Fatalf("real code after a wrong attempt rejected: %d %v", code, out)
	}
}

func TestMFALoginCodeReplayRejected(t *testing.T) {
	bearer := seedMFAAccount(t, 300007, "replay@euler.emoera.com")
	seed := activateMFA(t, bearer)
	_, out, _ := mfaDo(t, "POST", "/api/auth/login", "", map[string]any{"email": "replay@euler.emoera.com", "password": mfaPass})
	d, _ := out["Data"].(map[string]any)
	tok1, _ := d["mfaToken"].(string)

	code := totp.Code(seed, time.Now())
	if c, out, _ := mfaDo(t, "POST", "/api/auth/mfa/verify", "", map[string]any{"mfaToken": tok1, "code": code}); c != 200 {
		t.Fatalf("first use failed: %d %v", c, out)
	}

	// Shoulder-surfing attack: replay the same code with a fresh challenge.
	_, out, _ = mfaDo(t, "POST", "/api/auth/login", "", map[string]any{"email": "replay@euler.emoera.com", "password": mfaPass})
	d, _ = out["Data"].(map[string]any)
	tok2, _ := d["mfaToken"].(string)
	if c, out, _ := mfaDo(t, "POST", "/api/auth/mfa/verify", "", map[string]any{"mfaToken": tok2, "code": code}); c != 401 || out["Code"] != "Auth.MFACodeReused" {
		t.Fatalf("replay must 401 Auth.MFACodeReused, got %d %v", c, out)
	}
}

func TestMFAChallengeTokenIsNotAnAccessToken(t *testing.T) {
	bearer := seedMFAAccount(t, 300008, "tokenscope@euler.emoera.com")
	activateMFA(t, bearer)
	_, out, _ := mfaDo(t, "POST", "/api/auth/login", "", map[string]any{"email": "tokenscope@euler.emoera.com", "password": mfaPass})
	d, _ := out["Data"].(map[string]any)
	mfaToken, _ := d["mfaToken"].(string)

	// The 5-minute challenge must NOT authenticate normal endpoints —
	// otherwise it would be a 5-minute full session.
	if code, _, _ := mfaDo(t, "GET", "/api/account/profile", mfaToken, nil); code != 401 {
		t.Fatalf("mfaToken accepted as access token: %d", code)
	}
	// And an access token must NOT complete the MFA step.
	if code, _, _ := mfaDo(t, "POST", "/api/auth/mfa/verify", "", map[string]any{"mfaToken": bearer, "code": "123456"}); code != 401 {
		t.Fatalf("access token accepted as mfaToken: %d", code)
	}
}

func TestMFAUnbindRequiresLiveCode(t *testing.T) {
	bearer := seedMFAAccount(t, 300009, "unbind@euler.emoera.com")
	seed := activateMFA(t, bearer)

	// No code → no unbind: a stolen access token must not strip the factor.
	if code, _, _ := mfaDo(t, "POST", "/api/mfa/unbind", bearer, map[string]any{"code": "000000"}); code != 401 {
		t.Fatal("unbind without a live code accepted")
	}
	if code, out, _ := mfaDo(t, "POST", "/api/mfa/unbind", bearer, map[string]any{"code": totp.Code(seed, time.Now())}); code != 200 {
		t.Fatalf("unbind with live code failed: %d %v", code, out)
	}

	// After unbind: profile false, and login is single-step again.
	_, out, _ := mfaDo(t, "GET", "/api/account/profile", bearer, nil)
	d, _ := out["Data"].(map[string]any)
	if d["mfaEnabled"] != false {
		t.Fatalf("mfaEnabled after unbind = %v, want false", d["mfaEnabled"])
	}
	code, out, _ := mfaDo(t, "POST", "/api/auth/login", "", map[string]any{"email": "unbind@euler.emoera.com", "password": mfaPass})
	if code != 200 {
		t.Fatalf("login after unbind failed: %d %v", code, out)
	}
	d, _ = out["Data"].(map[string]any)
	if _, has := d["accessToken"]; !has {
		t.Fatal("login after unbind should return accessToken directly")
	}
}

func TestPasswordOnlyLoginUnchanged(t *testing.T) {
	// Regression: accounts without MFA keep the exact phase-1 login contract.
	bearer := seedMFAAccount(t, 300010, "plain@euler.emoera.com")
	_ = bearer
	code, out, _ := mfaDo(t, "POST", "/api/auth/login", "", map[string]any{"email": "plain@euler.emoera.com", "password": mfaPass})
	if code != 200 {
		t.Fatalf("plain login failed: %d %v", code, out)
	}
	d, _ := out["Data"].(map[string]any)
	if _, has := d["accessToken"]; !has {
		t.Fatal("plain login must still return accessToken")
	}
	if _, has := d["mfaRequired"]; has {
		t.Error("plain login must not mention mfaRequired")
	}
}
