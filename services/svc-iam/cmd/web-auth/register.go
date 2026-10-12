package main

// 注册邮箱验证: POST /api/auth/register/send-code and the optional "code" of
// POST /api/auth/register. It reuses the forgot-password machinery (forgot.go):
// keyed-HMAC 6-digit codes, the same TTL and guess limit, per-address and
// per-IP rate limits, svc-notify VERIFICATION_CODE mail off the request path.
//
//   - send-code {email} always answers 200 {"Code":"OK"} (no enumeration).
//     For an address that is NOT registered it stores a keyed hash of a fresh
//     code (purpose "register", TTL 10 min, a previous code is void) and mails
//     it (template auth.register-code). For a registered address — whatever
//     the account's status — nothing is sent: the owner already has an
//     account and signs in or resets the password instead.
//   - register {email, password, code?}: with
//     EULER_REGISTER_REQUIRE_EMAIL_CODE=1 the code is mandatory; unset, a
//     supplied code is still checked and an omitted one is allowed (backward
//     compatible). A missing / wrong / expired / never-sent code is 400
//     Auth.InvalidCode, guesses past the limit 429 Auth.TooManyAttempts. A
//     matching code is consumed before the account is created.
//
// Register codes live in email_verification_code (keyed by purpose and a
// keyed hash of the address — there is no account id yet), reset codes in
// password_reset_code, and the two are hashed under different keys: a reset
// code cannot register an address and a register code cannot reset a password.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// registerRequireCodeEnv makes the registration code mandatory when "1"
// (prod/staging overrides set it).
const registerRequireCodeEnv = "EULER_REGISTER_REQUIRE_EMAIL_CODE"

// registerCodeTemplate is the svc-notify template id of the registration mail.
const registerCodeTemplate = "auth.register-code"

// registerCodeTTL is how long a mailed registration code stays valid.
const registerCodeTTL = resetCodeTTL

// registerNotifyAccountID attributes registration mail in svc-notify, which
// requires an account on every notification: the address has none yet, so
// the platform's reserved id 1 (account ids below 1000000 are never
// allocated, V3) stands in.
const registerNotifyAccountID int64 = 1

var (
	// registerSendEmailLimiter: 5 codes per address per hour.
	registerSendEmailLimiter = newAttemptLimiter(5, time.Hour, time.Hour)
	// registerSendIPLimiter: 20 codes per client IP per hour (mail bombing).
	registerSendIPLimiter = newAttemptLimiter(20, time.Hour, time.Hour)
	// registerVerifyEmailLimiter mirrors the stored guess limit for EVERY
	// submitted address, registered or not, so the 429 says nothing either.
	registerVerifyEmailLimiter = newAttemptLimiter(maxResetAttempts, registerCodeTTL, registerCodeTTL)
	// registerVerifyIPLimiter: wrong codes per client IP (spraying addresses).
	registerVerifyIPLimiter = newAttemptLimiter(20, 15*time.Minute, 15*time.Minute)
)

// registerCodeRequired reports whether registration needs a mailed code.
func registerCodeRequired() bool {
	return strings.TrimSpace(os.Getenv(registerRequireCodeEnv)) == "1"
}

// pepperHMAC is HMAC-SHA256 under a key derived from EULER_IDENTITY_PEPPER
// for one domain (the resetCodeHash construction).
func pepperHMAC(domain, msg string) string {
	k := hmac.New(sha256.New, identityPepper)
	k.Write([]byte(domain))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// emailSubjectHash is the stored key of a normalised address.
func emailSubjectHash(email string) string {
	return pepperHMAC("euler/email-subject/v1", email)
}

// emailCodeHash is the stored form of a code mailed to an address for a
// purpose: keyed per purpose and bound to the address.
func emailCodeHash(purpose, email, code string) string {
	return pepperHMAC("euler/email-code/"+purpose+"/v1", email+":"+code)
}

func handleRegisterSendCode(w http.ResponseWriter, r *http.Request) {
	var req forgotSendRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	email := normalizeEmail(req.Email)
	if !validLoginEmail(email) {
		writeErr(w, 400, "Common.InvalidParameter", "请填写有效邮箱")
		return
	}
	now := time.Now()
	ekey, ikey := "register:"+email, "register-ip:"+clientIP(r)
	if registerSendEmailLimiter.Locked(ekey, now) || registerSendIPLimiter.Locked(ikey, now) {
		writeErr(w, 429, "Auth.TooManyAttempts", "验证码发送过于频繁,请稍后再试")
		return
	}
	registerSendEmailLimiter.Fail(ekey, now)
	registerSendIPLimiter.Fail(ikey, now)
	// A new code starts a new guessing budget — for every address alike.
	registerVerifyEmailLimiter.Reset("register-verify:" + email)

	_, err := st.AccountByName(r.Context(), email)
	switch {
	case err == nil:
		// Registered: nothing to send, same answer.
	case !errors.Is(err, errNotFound):
		internalError(w, r, "register code lookup", err)
		return
	default:
		code, err := randomDigits(6)
		if err != nil {
			internalError(w, r, "register code", err)
			return
		}
		if err := st.PutEmailCode(r.Context(), emailCode{
			Purpose: purposeRegister, EmailHash: emailSubjectHash(email),
			CodeHash:  emailCodeHash(purposeRegister, email, code),
			ExpiresAt: now.Add(registerCodeTTL), CreatedAt: now,
		}); err != nil {
			internalError(w, r, "store register code", err)
			return
		}
		dispatchRegisterCode(email, code)
	}
	writeJSON(w, 200, "OK", map[string]any{})
}

// dispatchRegisterCode hands the code to the mailer, off the request path
// unless deliverAsync is off. A failure is logged; the user asks again.
func dispatchRegisterCode(email, code string) {
	send := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := resetMailer.SendRegisterCode(ctx, email, code); err != nil {
			slog.Error("registration code delivery failed", "err", err)
		}
	}
	if deliverAsync {
		go send()
		return
	}
	send()
}

// checkRegisterCode verifies and consumes the registration code of email,
// answering the request itself when it fails. An empty code is refused
// without counting as a guess.
func checkRegisterCode(w http.ResponseWriter, r *http.Request, email, code string) bool {
	if code == "" {
		writeErr(w, 400, "Auth.InvalidCode", "请填写邮箱验证码")
		return false
	}
	now := time.Now()
	ekey, ikey := "register-verify:"+email, "register-verify-ip:"+clientIP(r)
	if registerVerifyEmailLimiter.Locked(ekey, now) || registerVerifyIPLimiter.Locked(ikey, now) {
		writeErr(w, 429, "Auth.TooManyAttempts", "验证码错误次数过多,请重新获取验证码")
		return false
	}
	invalid := func() bool {
		registerVerifyEmailLimiter.Fail(ekey, now)
		registerVerifyIPLimiter.Fail(ikey, now)
		writeErr(w, 400, "Auth.InvalidCode", "验证码错误或已过期")
		return false
	}
	if !validResetCode(code) {
		return invalid()
	}
	err := st.ConsumeEmailCode(r.Context(), purposeRegister, emailSubjectHash(email),
		emailCodeHash(purposeRegister, email, code), now)
	switch {
	case errors.Is(err, errCodeAttempts):
		writeErr(w, 429, "Auth.TooManyAttempts", "验证码错误次数过多,请重新获取验证码")
		return false
	case errors.Is(err, errCodeInvalid):
		return invalid()
	case err != nil:
		internalError(w, r, "register code check", err)
		return false
	}
	registerVerifyEmailLimiter.Reset(ekey)
	return true
}
