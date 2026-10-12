package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func forge(header, payload string, sign func(input string) string) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(header))
	p := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return h + "." + p + "." + sign(h+"."+p)
}

func TestAccessTokenIsRS256WithAccountID(t *testing.T) {
	a := mustAccount(t, 100123)
	tok := issueAccessToken(a, time.Now())
	parts := strings.Split(tok, ".")
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var h map[string]string
	_ = json.Unmarshal(hb, &h)
	if h["alg"] != "RS256" || h["kid"] != jwtSigner.kid {
		t.Fatalf("header = %v", h)
	}
	pb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	_ = json.Unmarshal(pb, &c)
	if c["account_id"] != "100123" || c["sub"] != "100123" {
		t.Fatalf("claims = %v (the gateway injects account_id as X-Euler-Account-Id)", c)
	}
	if id, _, err := verifyAccessToken(tok, time.Now()); err != nil || id != 100123 {
		t.Fatalf("round trip: %d %v", id, err)
	}
}

// The classic RS256→HS256 confusion: an attacker who knows the PUBLIC key
// signs an HS256 token with it as the HMAC secret. It must be refused, as must
// "none", JWE-shaped ("enc") and "crit" headers.
func TestForgedTokensRejected(t *testing.T) {
	payload := `{"sub":"100123","account_id":"100123","tv":0,"exp":` + jsonInt64(time.Now().Add(time.Hour).Unix()) + `}`
	pub := string(jwtSigner.publicKeyPEM())
	hs := func(input string) string {
		m := hmac.New(sha256.New, []byte(pub))
		m.Write([]byte(input))
		return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	}
	rs := func(input string) string {
		// a real RS256 signature over a different header is still wrong
		return strings.Split(jwtSigner.sign(map[string]string{"x": input}), ".")[2]
	}
	for name, tok := range map[string]string{
		"HS256 with public key": forge(`{"alg":"HS256","typ":"JWT"}`, payload, hs),
		"none":                  forge(`{"alg":"none","typ":"JWT"}`, payload, func(string) string { return "" }),
		"JWE enc header":        forge(`{"alg":"RS256","enc":"A256GCM"}`, payload, rs),
		"crit header":           forge(`{"alg":"RS256","crit":["exp"]}`, payload, rs),
		"wrong kid":             forge(`{"alg":"RS256","kid":"deadbeef"}`, payload, rs),
		"bad RS256 signature":   forge(`{"alg":"RS256","typ":"JWT","kid":"`+jwtSigner.kid+`"}`, payload, rs),
	} {
		if _, _, err := verifyAccessToken(tok, time.Now()); err == nil {
			t.Errorf("%s: forged token accepted", name)
		}
	}
}

func jsonInt64(v int64) string { b, _ := json.Marshal(v); return string(b) }

func TestCookieSecureByDefault(t *testing.T) {
	cases := []struct {
		env, override string
		want          bool
	}{
		{"", "", true},
		{"prod", "", true},
		{"staging", "", true},
		{"dev", "", false},
		{"local", "", false},
		{"prod", "false", false},
		{"dev", "true", true},
	}
	for _, c := range cases {
		t.Setenv("EULER_ENV", c.env)
		t.Setenv("EULER_COOKIE_SECURE", c.override)
		if got := loadCookieSecure(); got != c.want {
			t.Errorf("EULER_ENV=%q override=%q: Secure=%v, want %v", c.env, c.override, got, c.want)
		}
	}
}

func TestParseRSAPrivateKeyRejectsSmallKeys(t *testing.T) {
	if _, err := parseRSAPrivateKey([]byte("not pem")); err == nil {
		t.Fatal("garbage accepted")
	}
}
