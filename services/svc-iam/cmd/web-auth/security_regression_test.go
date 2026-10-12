package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression: access-token claims used to be built with fmt.Sprintf, so a
// crafted login email or real name could inject a second "sub" claim. Since
// encoding/json keeps the last duplicate key, the token resolved to whatever
// account the attacker named (e.g. the seed admin 100123).

const injectedSub = `x@evil.com","sub":"100123","a":"`

func postJSON(t *testing.T, h func(w http.ResponseWriter, r *http.Request), path, bearer string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func tokenSub(t *testing.T, tok string) string {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed token %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"sub"`) != 1 {
		t.Fatalf("payload carries more than one sub claim: %s", raw)
	}
	var c struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c.Sub
}

func TestRegisterRejectsMalformedEmail(t *testing.T) {
	for _, email := range []string{
		injectedSub,
		`"quoted"@euler.emoera.com`,
		`Admin <admin@euler.emoera.com>`,
		"no-at-sign",
		"a@nodot",
		"tab\t@euler.emoera.com",
		strings.Repeat("a", 250) + "@x.com",
	} {
		code, _ := postJSON(t, handleRegister, "/api/auth/register", "", map[string]string{"email": email, "password": "attacker-pass"})
		if code != 400 {
			t.Errorf("register(%q) = %d, want 400", email, code)
		}
	}
}

func TestRegisterAcceptsOrdinaryEmail(t *testing.T) {
	code, out := postJSON(t, handleRegister, "/api/auth/register", "", map[string]string{"email": "dev.ops+ci@euler.emoera.com", "password": "ordinary-pass"})
	if code != 201 {
		t.Fatalf("register ordinary email = %d %v", code, out)
	}
}

func TestRealNameCannotInjectTokenClaims(t *testing.T) {
	const email = "realname-inject@euler.emoera.com"
	if code, out := postJSON(t, handleRegister, "/api/auth/register", "", map[string]string{"email": email, "password": "attacker-pass"}); code != 201 {
		t.Fatalf("register = %d %v", code, out)
	}
	_, out := postJSON(t, handleLogin, "/api/auth/login", "", map[string]string{"email": email, "password": "attacker-pass"})
	tok, _ := testData(t, out)["accessToken"].(string)
	ownSub := tokenSub(t, tok)
	if ownSub == "100123" {
		t.Fatal("fresh account was issued the seed account's id")
	}

	// Real name is attacker-controlled and is copied into the next token.
	code, out := postJSON(t, handleRealnameVerify, "/api/realname/verify", tok, map[string]string{
		"type": "individual", "name": injectedSub, "idCardNo": "110101199001011237",
	})
	if code != 200 {
		t.Fatalf("realname verify = %d %v", code, out)
	}
	_, out = postJSON(t, handleLogin, "/api/auth/login", "", map[string]string{"email": email, "password": "attacker-pass"})
	tok, _ = testData(t, out)["accessToken"].(string)
	if got := tokenSub(t, tok); got != ownSub {
		t.Fatalf("token sub = %s, want the caller's own id %s", got, ownSub)
	}

	req := httptest.NewRequest("GET", "/api/account/profile", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	handleAccountProfile(rr, req)
	if strings.Contains(rr.Body.String(), `"id":100123`) {
		t.Fatalf("token resolved to the seed admin account: %s", rr.Body)
	}
}

func TestMFATokenClaimsAreWellFormed(t *testing.T) {
	a := mustAccount(t, 100123)
	tok := issueMFAToken(a, time.Now())
	id, _, err := verifyMFAToken(tok, time.Now())
	if err != nil || id != 100123 {
		t.Fatalf("verifyMFAToken = %d, %v", id, err)
	}
	if _, _, err := verifyAccessToken(tok, time.Now()); err == nil {
		t.Fatal("an MFA challenge token must not pass as an access token")
	}
}
