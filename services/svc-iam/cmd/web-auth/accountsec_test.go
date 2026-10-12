package main

import (
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/totp"
)

func TestPasswordHashIsPBKDF2AndLegacyStillVerifies(t *testing.T) {
	h := hashPassword("correct horse")
	if !strings.HasPrefix(h, "pbkdf2-sha256$") {
		t.Fatalf("hash format %q", h)
	}
	if !verifyPassword(h, "correct horse") || verifyPassword(h, "wrong") {
		t.Fatal("PBKDF2 verify")
	}
	legacy := "abcd$" + sha256Hex("abcd"+"old-password")
	if !verifyPassword(legacy, "old-password") || verifyPassword(legacy, "x") {
		t.Fatal("legacy salted-SHA256 hashes must still verify")
	}
	for _, bad := range []string{"", "pbkdf2-sha256$x$y$z", "pbkdf2-sha256$0$AA$AA", "a$b$c"} {
		if verifyPassword(bad, "anything") {
			t.Errorf("malformed hash %q verified", bad)
		}
	}
}

// 07§2.4: 5 failures lock the email for 15 minutes — for unknown emails too,
// so the lockout reveals nothing about account existence.
func TestLoginLockoutAfterFiveFailures(t *testing.T) {
	for _, email := range []string{"lockme@euler.emoera.com", "nobody-here@euler.emoera.com"} {
		if email == "lockme@euler.emoera.com" {
			mustSeed(t, 500001, email, "锁定测试", "right-password-1")
		}
		for i := 0; i < 5; i++ {
			if code, _ := postJSON(t, handleLogin, "/api/auth/login", "", map[string]string{"email": email, "password": "wrong"}); code != 401 {
				t.Fatalf("%s attempt %d: %d", email, i, code)
			}
		}
		code, _ := postJSON(t, handleLogin, "/api/auth/login", "", map[string]string{"email": email, "password": "right-password-1"})
		if code != 429 {
			t.Fatalf("%s after 5 failures: %d, want 429 even with the right password", email, code)
		}
	}
}

func TestMFACodeGuessingIsLimited(t *testing.T) {
	bearer := seedMFAAccount(t, 500002, "mfa-brute@euler.emoera.com")
	seed := activateMFA(t, bearer)
	_, out, _ := mfaDo(t, "POST", "/api/auth/login", "", map[string]any{"email": "mfa-brute@euler.emoera.com", "password": mfaPass})
	mfaToken, _ := out["Data"].(map[string]any)["mfaToken"].(string)
	for i := 0; i < 5; i++ {
		if code, _, _ := mfaDo(t, "POST", "/api/auth/mfa/verify", "", map[string]any{"mfaToken": mfaToken, "code": "000000"}); code != 401 {
			t.Fatalf("wrong code %d: %d", i, code)
		}
	}
	code, _, _ := mfaDo(t, "POST", "/api/auth/mfa/verify", "", map[string]any{"mfaToken": mfaToken, "code": totp.Code(seed, time.Now())})
	if code != 429 {
		t.Fatalf("after 5 wrong codes: %d, want 429 even with the right code", code)
	}
}

func TestRealnameValidationAndFinality(t *testing.T) {
	tok := issueAccessToken(mustSeed(t, 500003, "realname@euler.emoera.com", "", "pw-12345678"), time.Now())
	for name, body := range map[string]map[string]string{
		"bad checksum":    {"type": "individual", "name": "张三", "idCardNo": "110101199001011234"},
		"future birth":    {"type": "individual", "name": "张三", "idCardNo": "110101299001011234"},
		"15 chars":        {"type": "individual", "name": "张三", "idCardNo": "123456789012345"},
		"empty name":      {"type": "individual", "name": " ", "idCardNo": "110101199001011237"},
		"bad credit code": {"type": "enterprise", "enterpriseName": "某公司", "creditCode": "91350100M000100Y40"},
	} {
		if code, _ := postJSON(t, handleRealnameVerify, "/api/realname/verify", tok, body); code != 400 {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	ok := map[string]string{"type": "individual", "name": "张三", "idCardNo": "110101199001011237"}
	if code, out := postJSON(t, handleRealnameVerify, "/api/realname/verify", tok, ok); code != 200 {
		t.Fatalf("valid ID: %d %v", code, out)
	}
	a := mustAccount(t, 500003)
	if strings.Contains(a.IDCardNo, "19900101") || a.IdentityFingerprint == "" {
		t.Fatalf("plaintext ID kept or no fingerprint: %q %q", a.IDCardNo, a.IdentityFingerprint)
	}
	other := map[string]string{"type": "individual", "name": "李四", "idCardNo": "440306198512310014"}
	if code, _ := postJSON(t, handleRealnameVerify, "/api/realname/verify", tok, other); code != 409 {
		t.Fatalf("re-verifying a verified account: %d, want 409", code)
	}
}

func TestDocumentChecksums(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for id, want := range map[string]bool{
		"110101199001011237": true, "440306198512310014": true, "11010119900101123x": false,
		"110101199002301230": false, "110101199001011234": false, "abc": false,
	} {
		if got := validResidentID(id, now); got != want {
			t.Errorf("validResidentID(%s) = %v", id, got)
		}
	}
	for code, want := range map[string]bool{"91350100M000100Y43": true, "91350100M000100Y40": false, "91350100I000100Y43": false} {
		if got := validCreditCode(code); got != want {
			t.Errorf("validCreditCode(%s) = %v", code, got)
		}
	}
	if identityFingerprint("id", "110101199001011237") == identityFingerprint("id", "440306198512310014") {
		t.Fatal("fingerprints collide")
	}
}
