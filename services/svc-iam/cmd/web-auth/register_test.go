package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/notify"
)

// The email-verification code contract, on both backends (F1 parity).
func TestStoreContractEmailCodes(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		ctx, now := context.Background(), time.Now()
		subject := emailSubjectHash("contract-code@euler.emoera.com")
		put := func(purpose, hash string, ttl time.Duration) {
			t.Helper()
			if err := st.PutEmailCode(ctx, emailCode{Purpose: purpose, EmailHash: subject, CodeHash: hash, ExpiresAt: now.Add(ttl), CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
		consume := func(purpose, hash string) error {
			t.Helper()
			return st.ConsumeEmailCode(ctx, purpose, subject, hash, now)
		}
		if err := consume(purposeRegister, "h-any"); !errors.Is(err, errCodeInvalid) {
			t.Fatalf("no code issued: %v", err)
		}
		put(purposeRegister, "h-good", 10*time.Minute)
		// Purposes do not mix: the same subject and hash under another
		// purpose is not a code.
		if err := consume("email-change", "h-good"); !errors.Is(err, errCodeInvalid) {
			t.Fatalf("other purpose: %v", err)
		}
		if err := st.ConsumeEmailCode(ctx, purposeRegister, emailSubjectHash("other@euler.emoera.com"), "h-good", now); !errors.Is(err, errCodeInvalid) {
			t.Fatalf("other address: %v", err)
		}
		for i := 0; i < maxResetAttempts; i++ {
			if err := consume(purposeRegister, "h-bad"); !errors.Is(err, errCodeInvalid) {
				t.Fatalf("wrong guess %d: %v", i, err)
			}
		}
		if err := consume(purposeRegister, "h-good"); !errors.Is(err, errCodeAttempts) {
			t.Fatalf("right code after %d wrong ones: %v", maxResetAttempts, err)
		}
		// A new code replaces the old one and resets the budget.
		put(purposeRegister, "h-fresh", 10*time.Minute)
		if err := consume(purposeRegister, "h-good"); !errors.Is(err, errCodeInvalid) {
			t.Fatalf("superseded code: %v", err)
		}
		if err := consume(purposeRegister, "h-fresh"); err != nil {
			t.Fatalf("right code: %v", err)
		}
		// Single use.
		if err := consume(purposeRegister, "h-fresh"); !errors.Is(err, errCodeInvalid) {
			t.Fatalf("code reused: %v", err)
		}
		// Expired.
		put(purposeRegister, "h-late", -time.Second)
		if err := consume(purposeRegister, "h-late"); !errors.Is(err, errCodeInvalid) {
			t.Fatalf("expired code: %v", err)
		}
	})
}

func TestEmailCodeHashesAreDomainSeparated(t *testing.T) {
	const email, code = "sep@euler.emoera.com", "123456"
	reg := emailCodeHash(purposeRegister, email, code)
	if reg == emailCodeHash("email-change", email, code) || reg == emailCodeHash(purposeRegister, "other@euler.emoera.com", code) ||
		reg == resetCodeHash(1000001, code) || reg == emailSubjectHash(email) {
		t.Fatal("register code hash collides with another purpose, address or the reset-code hash")
	}
	if first, again := emailSubjectHash(email), emailSubjectHash(email); len(first) != 64 || first != again {
		t.Fatal("subject hash is not a stable 64-hex value")
	}
}

func registerBody(email, password, code string) map[string]string {
	b := map[string]string{"email": email, "password": password}
	if code != "" {
		b["code"] = code
	}
	return b
}

// With EULER_REGISTER_REQUIRE_EMAIL_CODE=1 an account needs a mailed code.
func TestFlowRegisterRequiresEmailCode(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		t.Setenv(registerRequireCodeEnv, "1")
		mail := useRecordingMailer(t)
		c := newFlowClient(t)
		const email = "Reg-Flow@Euler.emoera.com" // normalised to lower case
		const norm = "reg-flow@euler.emoera.com"
		const taken = "reg-taken@euler.emoera.com"
		if _, err := st.CreateAccount(context.Background(), account{AccountName: taken, PasswordHash: hashPassword("password-0"), Status: 1, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}

		// No enumeration: both answer 200; only the free address gets a code.
		for _, e := range []string{email, taken} {
			if code, out := c.do("POST", "/api/auth/register/send-code", map[string]string{"email": e}); code != 200 || out["Code"] != "OK" {
				t.Fatalf("send-code %s: %d %v", e, code, out)
			}
		}
		if n := len(mail.registerCodes(taken)); n != 0 {
			t.Fatalf("%d codes mailed to a registered address", n)
		}
		if len(mail.sent[taken]) != 0 {
			t.Fatal("register send-code mailed a password-reset code")
		}
		codes := mail.registerCodes(norm)
		if len(codes) != 1 || !validResetCode(codes[0]) {
			t.Fatalf("codes mailed to %s: %v", norm, codes)
		}
		code1 := codes[0]
		wrong := "000000"
		if wrong == code1 {
			wrong = "111111"
		}
		if code, out := c.do("POST", "/api/auth/register/send-code", map[string]string{"email": "not-an-email"}); code != 400 || out["Code"] != "Common.InvalidParameter" {
			t.Fatalf("invalid email: %d %v", code, out)
		}

		for name, tc := range map[string]struct {
			body map[string]string
			code string
		}{
			"missing code":  {registerBody(email, "password-1", ""), "Auth.InvalidCode"},
			"wrong code":    {registerBody(email, "password-1", wrong), "Auth.InvalidCode"},
			"malformed":     {registerBody(email, "password-1", "12ab56"), "Auth.InvalidCode"},
			"weak password": {registerBody(email, "short", code1), "Common.InvalidParameter"},
		} {
			if status, out := c.do("POST", "/api/auth/register", tc.body); status != 400 || out["Code"] != tc.code {
				t.Fatalf("%s: %d %v, want 400 %s", name, status, out, tc.code)
			}
		}
		// A new send supersedes the old code.
		c.do("POST", "/api/auth/register/send-code", map[string]string{"email": email})
		code2 := mail.registerCodes(norm)[1]
		if code2 != code1 {
			if status, _ := c.do("POST", "/api/auth/register", registerBody(email, "password-1", code1)); status != 400 {
				t.Fatalf("superseded code accepted: %d", status)
			}
		}
		if status, out := c.do("POST", "/api/auth/register", registerBody(email, "password-1", code2)); status != 201 || out["Code"] != "OK" {
			t.Fatalf("register: %d %v", status, out)
		}
		c.login(norm, "password-1")
		// Single use, and a registered address never has a register code:
		// any code is the same 400 (not the 409 that would reveal it).
		if status, out := c.do("POST", "/api/auth/register", registerBody(email, "password-2", code2)); status != 400 || out["Code"] != "Auth.InvalidCode" {
			t.Fatalf("reused code: %d %v", status, out)
		}
		if status, out := c.do("POST", "/api/auth/register", registerBody(taken, "password-2", "123456")); status != 400 || out["Code"] != "Auth.InvalidCode" {
			t.Fatalf("registered address: %d %v", status, out)
		}

		// A password-reset code cannot register, and a register code cannot
		// reset a password.
		c.do("POST", "/api/auth/forgot/send-code", map[string]string{"email": taken})
		resetCode := mail.last(t, taken)
		if status, out := c.do("POST", "/api/auth/register", registerBody(taken, "password-3", resetCode)); status != 400 || out["Code"] != "Auth.InvalidCode" {
			t.Fatalf("reset code used to register: %d %v", status, out)
		}
		const late = "reg-late@euler.emoera.com"
		c.do("POST", "/api/auth/register/send-code", map[string]string{"email": late})
		regCode := mail.registerCodes(late)[0]
		if _, err := st.CreateAccount(context.Background(), account{AccountName: late, PasswordHash: hashPassword("password-0"), Status: 1, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if status, out := c.do("POST", "/api/auth/forgot/reset", map[string]string{"email": late, "code": regCode, "password": "password-4"}); status != 400 || out["Code"] != "Auth.InvalidCode" {
			t.Fatalf("register code used to reset: %d %v", status, out)
		}

		// Five wrong guesses, then 429 — alike for a free and a taken address.
		for _, e := range []string{"reg-guess@euler.emoera.com", taken} {
			c.do("POST", "/api/auth/register/send-code", map[string]string{"email": e})
			for i := 0; i < maxResetAttempts; i++ {
				if status, _ := c.do("POST", "/api/auth/register", registerBody(e, "password-5", "999999")); status != 400 {
					t.Fatalf("%s guess %d: %d", e, i, status)
				}
			}
			if status, out := c.do("POST", "/api/auth/register", registerBody(e, "password-5", "999999")); status != 429 || out["Code"] != "Auth.TooManyAttempts" {
				t.Fatalf("%s after %d guesses: %d %v, want 429", e, maxResetAttempts, status, out)
			}
		}
		// The stored budget holds without the process limiter (another
		// replica): the right code is refused once five guesses are spent.
		guess := "reg-guess@euler.emoera.com"
		registerVerifyEmailLimiter.Reset("register-verify:" + guess)
		right := mail.registerCodes(guess)[len(mail.registerCodes(guess))-1]
		if status, out := c.do("POST", "/api/auth/register", registerBody(guess, "password-5", right)); status != 429 {
			t.Fatalf("right code after the stored budget: %d %v, want 429", status, out)
		}

		// Expired code.
		const old = "reg-expired@euler.emoera.com"
		if err := st.PutEmailCode(context.Background(), emailCode{Purpose: purposeRegister, EmailHash: emailSubjectHash(old),
			CodeHash: emailCodeHash(purposeRegister, old, "424242"), ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
			t.Fatal(err)
		}
		if status, out := c.do("POST", "/api/auth/register", registerBody(old, "password-6", "424242")); status != 400 || out["Code"] != "Auth.InvalidCode" {
			t.Fatalf("expired code: %d %v", status, out)
		}

		// Sending is rate limited per address (5 per hour).
		c2 := newFlowClient(t)
		c2.ip = "203.0.113.10"
		var last int
		for i := 0; i < 6; i++ {
			last, _ = c2.do("POST", "/api/auth/register/send-code", map[string]string{"email": "reg-bomb@euler.emoera.com"})
		}
		if last != 429 {
			t.Fatalf("6th send in an hour: %d, want 429", last)
		}
	})
}

// Unset, registration stays backward compatible: no code is fine, but a
// code that is given must be right.
func TestFlowRegisterCodeOptionalByDefault(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		t.Setenv(registerRequireCodeEnv, "")
		mail := useRecordingMailer(t)
		c := newFlowClient(t)
		if status, out := c.do("POST", "/api/auth/register", registerBody("reg-legacy@euler.emoera.com", "password-1", "")); status != 201 {
			t.Fatalf("register without code: %d %v", status, out)
		}
		if status, out := c.do("POST", "/api/auth/register", registerBody("reg-legacy@euler.emoera.com", "password-1", "")); status != 409 || out["Code"] != "Auth.EmailExists" {
			t.Fatalf("legacy duplicate: %d %v", status, out)
		}
		const email = "reg-optional@euler.emoera.com"
		c.do("POST", "/api/auth/register/send-code", map[string]string{"email": email})
		code := mail.registerCodes(email)[0]
		wrong := "000000"
		if wrong == code {
			wrong = "111111"
		}
		if status, out := c.do("POST", "/api/auth/register", registerBody(email, "password-1", wrong)); status != 400 || out["Code"] != "Auth.InvalidCode" {
			t.Fatalf("wrong optional code: %d %v", status, out)
		}
		if status, out := c.do("POST", "/api/auth/register", registerBody(email, "password-1", " "+code+" ")); status != 201 {
			t.Fatalf("right optional code: %d %v", status, out)
		}
	})
}

func TestNotifyMailerRegisterCode(t *testing.T) {
	t.Setenv(httpmw.InternalTokenEnv, "svc-token")
	var got struct {
		Class, TemplateId, BizKey string
		Channels                  []string
		Params                    map[string]string
	}
	var acct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acct = r.Header.Get("X-Euler-Account-Id")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"Code":"OK"}`)
	}))
	defer srv.Close()
	m := notifyMailer{base: srv.URL, client: srv.Client()}
	if err := m.SendRegisterCode(context.Background(), "new@example.com", "602113"); err != nil {
		t.Fatal(err)
	}
	if got.Class != string(notify.ClassVerificationCode) || got.TemplateId != registerCodeTemplate ||
		len(got.Channels) != 1 || got.Channels[0] != "EMAIL" ||
		!regexp.MustCompile(`^register:[0-9a-f]{16}$`).MatchString(got.BizKey) ||
		got.Params[notify.ParamRecipient] != "new@example.com" || got.Params[notify.ParamCode] != "602113" || got.Params[notify.ParamTTLMinutes] != "10" {
		t.Fatalf("notification: %+v", got)
	}
	if acct != "1" {
		t.Fatalf("attributed to account %q, want the reserved 1", acct)
	}
}
