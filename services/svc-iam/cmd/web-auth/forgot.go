package main

// 找回密码 (F2): POST /api/auth/forgot/send-code and /api/auth/forgot/reset,
// the shapes platform/frontend/apps/web-account/src/views/Forgot.vue sends.
//
//   - send-code {email} answers 200 {"Code":"OK"} whether or not the address
//     is registered (no account enumeration). For a registered, usable
//     account it stores a keyed hash of a fresh 6-digit code (TTL 10 min, the
//     previous code is void) and mails the code through svc-notify
//     (POST /internal/notifications, class VERIFICATION_CODE) off the request
//     path, so the response time does not depend on the address either.
//   - reset {email, code, password} checks the code (5 guesses), applies the
//     register password rule, installs the new password, bumps the token
//     version (every access token and refresh session dies) and clears the
//     login lockout. A wrong / expired / never-sent code is one generic
//     400 Auth.InvalidCode; guesses past the limit are 429.
//
// Both are rate limited per submitted email and per client IP with the
// accountsec limiter (process-local, like the login lockout); the code's own
// attempt counter is in the store, so it holds across replicas.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/notify"
	"github.com/qifalab/euler-platform/runmode"
)

// notifyURLEnv is svc-notify's base URL (chart extraEnv:
// http://svc-notify.euler-svc-notify.svc:8080). Required in production.
const notifyURLEnv = "EULER_SVC_NOTIFY_URL"

// resetCodeTTL is how long a mailed code stays valid.
const resetCodeTTL = 10 * time.Minute

var (
	// forgotSendEmailLimiter: 5 codes per address per hour.
	forgotSendEmailLimiter = newAttemptLimiter(5, time.Hour, time.Hour)
	// forgotSendIPLimiter: 20 codes per client IP per hour (mail bombing).
	forgotSendIPLimiter = newAttemptLimiter(20, time.Hour, time.Hour)
	// forgotResetEmailLimiter mirrors the stored 5-guess limit for EVERY
	// submitted address, registered or not, so "429 after five wrong codes"
	// says nothing about whether the address exists.
	forgotResetEmailLimiter = newAttemptLimiter(maxResetAttempts, resetCodeTTL, resetCodeTTL)
	// forgotResetIPLimiter: wrong codes per client IP (spraying addresses).
	forgotResetIPLimiter = newAttemptLimiter(20, 15*time.Minute, 15*time.Minute)

	resetMailer codeMailer = logCodeMailer{}
	// deliverAsync hands the mail off the request path (tests turn it off).
	deliverAsync = true
)

// codeMailer delivers one-time codes: a password-reset code to the account's
// mailbox (F2), a registration code to an address that is not an account yet
// (register.go).
type codeMailer interface {
	SendResetCode(ctx context.Context, accountID int64, email, code string) error
	SendRegisterCode(ctx context.Context, email, code string) error
}

// logCodeMailer is the development stand-in when svc-notify is not
// configured: the code goes to the log (WARN). Never used in production —
// configureIntegrations refuses to start without EULER_SVC_NOTIFY_URL there.
type logCodeMailer struct{}

func (logCodeMailer) SendResetCode(_ context.Context, accountID int64, email, code string) error {
	slog.Warn("DEV password-reset code (svc-notify not configured — never in production)",
		"account_id", accountID, "email", email, "code", code)
	return nil
}

func (logCodeMailer) SendRegisterCode(_ context.Context, email, code string) error {
	slog.Warn("DEV registration code (svc-notify not configured — never in production)", "email", email, "code", code)
	return nil
}

// notifyMailer sends the code through svc-notify. The notification is
// trust-critical (never throttled) and the code rides a secret parameter, so
// svc-notify's record keeps it redacted.
type notifyMailer struct {
	base   string
	client *http.Client
}

func (m notifyMailer) SendResetCode(ctx context.Context, accountID int64, email, code string) error {
	return m.sendCode(ctx, accountID, notify.TemplatePasswordResetCode,
		"pwreset:"+strconv.FormatInt(accountID, 10), email, code, resetCodeTTL)
}

// SendRegisterCode mails a registration code. There is no account yet, so
// the notification is attributed to registerNotifyAccountID and its BizKey
// names the address only by a prefix of its keyed hash.
func (m notifyMailer) SendRegisterCode(ctx context.Context, email, code string) error {
	return m.sendCode(ctx, registerNotifyAccountID, registerCodeTemplate,
		"register:"+emailSubjectHash(email)[:16], email, code, registerCodeTTL)
}

// sendCode posts one VERIFICATION_CODE email notification to svc-notify.
func (m notifyMailer) sendCode(ctx context.Context, accountID int64, template, bizKey, email, code string, ttl time.Duration) error {
	body, err := json.Marshal(map[string]any{
		"Class":      string(notify.ClassVerificationCode),
		"TemplateId": template,
		"Channels":   []string{string(notify.ChannelEmail)},
		"BizKey":     bizKey,
		"Params": map[string]string{
			notify.ParamRecipient:  email,
			notify.ParamCode:       code,
			notify.ParamTTLMinutes: strconv.Itoa(int(ttl / time.Minute)),
		},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+"/internal/notifications", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Euler-Account-Id", strconv.FormatInt(accountID, 10))
	httpmw.SetInternalToken(req)
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("svc-notify: %w", err)
	}
	defer resp.Body.Close()
	var env struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&env)
	if resp.StatusCode != http.StatusOK || env.Code != "OK" {
		return fmt.Errorf("svc-notify answered %d %s: %s", resp.StatusCode, env.Code, env.Message)
	}
	return nil
}

// configureIntegrations wires the outbound dependencies from the
// environment: svc-notify for reset mail (F2) and the real-name provider (F6).
func configureIntegrations() error {
	if raw := strings.TrimSpace(os.Getenv(notifyURLEnv)); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s must be an http(s) base URL, got %q", notifyURLEnv, raw)
		}
		resetMailer = notifyMailer{base: strings.TrimRight(raw, "/"), client: &http.Client{Timeout: 10 * time.Second}}
		slog.Info("password-reset mail goes through svc-notify", "url", raw)
	} else if runmode.Production() {
		return fmt.Errorf("%s is required in production: password-reset codes are mailed through svc-notify", notifyURLEnv)
	} else {
		resetMailer = logCodeMailer{}
		slog.Warn("svc-notify not configured — password-reset codes are logged instead of mailed (dev only)", "set", notifyURLEnv)
	}
	p, err := realnameProviderFromEnv()
	if err != nil {
		return err
	}
	realnameProvider = p
	return nil
}

// clientIP is the caller's address for per-IP limits: X-Real-IP as set by
// the gateway (APISIX overwrites it with the connecting client), else the
// TCP peer.
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		if ip := net.ParseIP(v); ip != nil {
			return ip.String()
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// resetCodeHash is the stored form of a code: HMAC keyed by a value derived
// from EULER_IDENTITY_PEPPER (a bare hash of six digits is reversed by
// enumeration in milliseconds), bound to the account.
func resetCodeHash(accountID int64, code string) string {
	k := hmac.New(sha256.New, identityPepper)
	k.Write([]byte("euler/forgot-code/v1"))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte(strconv.FormatInt(accountID, 10) + ":" + code))
	return hex.EncodeToString(m.Sum(nil))
}

// validResetCode accepts exactly six ASCII digits.
func validResetCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

type forgotSendRequest struct {
	Email string `json:"email"`
}

func handleForgotSendCode(w http.ResponseWriter, r *http.Request) {
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
	ekey, ikey := "forgot:"+email, "forgot-ip:"+clientIP(r)
	if forgotSendEmailLimiter.Locked(ekey, now) || forgotSendIPLimiter.Locked(ikey, now) {
		writeErr(w, 429, "Auth.TooManyAttempts", "验证码发送过于频繁,请稍后再试")
		return
	}
	forgotSendEmailLimiter.Fail(ekey, now)
	forgotSendIPLimiter.Fail(ikey, now)
	// A new code starts a new guessing budget — for every address alike.
	forgotResetEmailLimiter.Reset("forgot-reset:" + email)

	a, err := st.AccountByName(r.Context(), email)
	switch {
	case errors.Is(err, errNotFound):
		// Unregistered: nothing to send, same answer.
	case err != nil:
		internalError(w, r, "forgot lookup", err)
		return
	case a.Status != 1:
		// Frozen / closed accounts do not self-service a reset; same answer.
	default:
		code, err := randomDigits(6)
		if err != nil {
			internalError(w, r, "forgot code", err)
			return
		}
		if err := st.PutResetCode(r.Context(), resetCode{
			AccountID: a.AccountID, CodeHash: resetCodeHash(a.AccountID, code),
			ExpiresAt: now.Add(resetCodeTTL), CreatedAt: now,
		}); err != nil {
			internalError(w, r, "store reset code", err)
			return
		}
		dispatchResetCode(a.AccountID, a.AccountName, code)
	}
	writeJSON(w, 200, "OK", map[string]any{})
}

// dispatchResetCode hands the code to the mailer, off the request path
// unless deliverAsync is off. A delivery failure is logged; the user simply
// requests another code.
func dispatchResetCode(accountID int64, email, code string) {
	send := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := resetMailer.SendResetCode(ctx, accountID, email, code); err != nil {
			slog.Error("password-reset code delivery failed", "account_id", accountID, "err", err)
		}
	}
	if deliverAsync {
		go send()
		return
	}
	send()
}

type forgotResetRequest struct {
	Email    string `json:"email"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

func handleForgotReset(w http.ResponseWriter, r *http.Request) {
	var req forgotResetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	email := normalizeEmail(req.Email)
	if !validLoginEmail(email) || !validPassword(req.Password) {
		writeErr(w, 400, "Common.InvalidParameter", "请填写有效邮箱,密码 8-128 位")
		return
	}
	now := time.Now()
	ekey, ikey := "forgot-reset:"+email, "forgot-reset-ip:"+clientIP(r)
	if forgotResetEmailLimiter.Locked(ekey, now) || forgotResetIPLimiter.Locked(ikey, now) {
		writeErr(w, 429, "Auth.TooManyAttempts", "验证码错误次数过多,请重新获取验证码")
		return
	}
	invalid := func() {
		forgotResetEmailLimiter.Fail(ekey, now)
		forgotResetIPLimiter.Fail(ikey, now)
		writeErr(w, 400, "Auth.InvalidCode", "验证码错误或已过期")
	}
	if !validResetCode(req.Code) {
		invalid()
		return
	}
	// Hash before the lookup, so an unregistered address costs the same.
	newHash := hashPassword(req.Password)
	a, err := st.AccountByName(r.Context(), email)
	if errors.Is(err, errNotFound) {
		invalid()
		return
	}
	if err != nil {
		internalError(w, r, "forgot reset lookup", err)
		return
	}
	_, err = st.ResetPassword(r.Context(), a.AccountID, resetCodeHash(a.AccountID, req.Code), newHash, now)
	switch {
	case errors.Is(err, errCodeAttempts):
		writeErr(w, 429, "Auth.TooManyAttempts", "验证码错误次数过多,请重新获取验证码")
		return
	case errors.Is(err, errCodeInvalid):
		invalid()
		return
	case err != nil:
		internalError(w, r, "reset password", err)
		return
	}
	// The owner just proved control of the mailbox: lift the login lockout
	// an attacker may have caused.
	loginLimiter.Reset(loginKey(email))
	forgotResetEmailLimiter.Reset(ekey)
	writeJSON(w, 200, "OK", map[string]any{})
}
