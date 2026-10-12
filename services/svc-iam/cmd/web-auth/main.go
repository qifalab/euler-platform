// Command svc-iam-web-auth runs the web-facing authentication endpoints the
// frontend SSO uses (02-frontend-architecture.md §5.1/§5.2).
//
// Routes (POST unless noted):
//
//	POST /api/auth/login    — account + password → access_token + refresh cookie
//	POST /api/auth/refresh  — rotate refresh, return new access_token
//	POST /api/auth/logout   — revoke refresh
//	GET  /api/auth/session   — validate access_token, return user + permission snapshot
//
//	MFA 强制登录 (07§2.4, 09-roadmap M-2): when an account has a bound TOTP
//	seed, password alone is NOT enough — login returns a 5-minute mfaToken,
//	and only POST /api/auth/mfa/verify (token + 6-digit code) mints the
//	session. Binding lifecycle:
//
//	POST /api/mfa/bind     — mint a TOTP seed (KMS envelope), return QR provisioning URI
//	POST /api/mfa/verify   — confirm a code from the app, ACTIVATE the factor
//	POST /api/mfa/unbind   — remove the factor (requires a live code)
//
//	GET  /api/ak             — list the account's AccessKeys
//	POST /api/ak             — create an AccessKey (secret shown once)
//	POST /api/ak/{akId}/disable — disable an AccessKey
//	POST /api/ak/{akId}/enable  — re-enable a disabled AccessKey
//	POST /api/ak/{akId}/rotate  — mint a replacement, mark old one in grace window
//	DELETE /api/ak/{akId}       — permanently delete an AccessKey
//	  (create/list take an optional ramUserId — body / query — to manage a
//	  RAM user's keys; such keys verify as "user/id:<n>")
//
//	POST /api/auth/forgot/send-code — email a 6-digit reset code (always 200)
//	POST /api/auth/forgot/reset     — code + new password; revokes every session
//
//	POST /internal/authz/check  — gateway CheckAccess (eu-authorize.lua);
//	                              internal-token only, never routed publicly
//	GET  /internal/accounts/{id}/realname — real-name status for other
//	                              services (svc-order trial); internal-token only
//
// Persistence (F1): with EULER_DB_DSN every account, refresh session, RAM
// user/role/policy, MFA seed, real-name result and reset code lives in
// account_db (store_sql.go); AccessKeys go through the pkg-go/accesskey store
// and pkg-go/kms keys cmd/verify reads, so a console-created key verifies at
// the gateway. Unset, the in-memory store keeps dev and tests dependency-free
// (store_mem.go); production refuses to start without it.
//
// Token scheme (02§5.1):
//   - access_token: RS256-signed JWT (jwt.go), 15 min, returned in the JSON body (memory-only on the client)
//   - refresh_token: opaque crypto/rand string, 7 days, HttpOnly cookie. Its Domain comes from
//     EULER_COOKIE_DOMAIN (prod/staging: .euler.emoera.com, shared by account→console SSO);
//     unset means a host-only cookie (dev: localhost; the frontend reads it via credentials:include)
//
// This is a stdlib-HTTP service (repo convention). The password hash is
// salted SHA-256 here for zero-dep portability; production uses argon2id with a
// 5-failure lockout (07§2.4) — the verify path and table shape are unchanged.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qifalab/euler-platform/accesskey"
	"github.com/qifalab/euler-platform/authz"
	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/kms"
	"github.com/qifalab/euler-platform/runmode"
	"github.com/qifalab/euler-platform/sts"
	"github.com/qifalab/euler-platform/totp"
)

const (
	accessTTL  = 15 * time.Minute
	refreshTTL = 7 * 24 * time.Hour
	// mfaTTL bounds the second-factor challenge issued after a successful
	// password check. Five minutes: enough for a human to open the
	// authenticator, short enough that an intercepted challenge is not a
	// lingering door.
	mfaTTL = 5 * time.Minute
	// maxBodyBytes caps every JSON request body (defense against oversized
	// payloads); http.MaxBytesReader enforces it per-handler.
	maxBodyBytes = 1 << 20 // 1 MiB
)

// cookieSecure controls the Secure attribute of the refresh cookie.
// EULER_COOKIE_SECURE=true/false overrides. Otherwise the cookie is Secure
// unless EULER_ENV explicitly says dev/local/test: secure by default, so a
// deployment that forgets EULER_ENV does not ship a refresh token that may
// travel over plain HTTP. (Browsers accept Secure cookies on
// http://localhost, so local development keeps working.)
var cookieSecure = loadCookieSecure()

func loadCookieSecure() bool {
	if v := os.Getenv("EULER_COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
		slog.Warn("invalid EULER_COOKIE_SECURE, falling back to env default", "value", v)
	}
	switch runmode.Env() {
	case "dev", "local", "test":
		return false
	}
	return true
}

// cookieDomain is the Domain attribute of the refresh cookie
// (EULER_COOKIE_DOMAIN). Production sets ".euler.emoera.com" so the session
// minted on the account site is sent to the console (02§5.1 SSO); empty keeps
// the cookie host-only, which is what local development needs.
var cookieDomain = loadCookieDomain()

func loadCookieDomain() string { return strings.TrimSpace(os.Getenv("EULER_COOKIE_DOMAIN")) }

// dummyPasswordHash is compared against when the account does not exist, so
// login latency does not reveal whether an email is registered.
var dummyPasswordHash = hashPassword(randHex(16))

// --- domain ---

type account struct {
	AccountID      int64
	AccountName    string // login (email)
	RealName       string
	PasswordHash   string // PBKDF2-SHA256 (accountsec.go); legacy "salt$hex" still verifies
	Status         int    // 1 normal, 2 frozen, 3 closed
	RealNameStatus int    // 0 unverified, 1 individual verified, 2 enterprise verified (07§2.3)
	RealNameType   string // "individual" | "enterprise" | ""
	IDCardNo       string // MASKED document number (the plaintext is never kept)
	// IdentityFingerprint is HMAC(pepper, kind:number) — the dedup key for
	// "one person, many accounts" (trial anti-abuse) without storing the number.
	IdentityFingerprint string
	EnterpriseName      string
	CreatedAt           time.Time // provisioning time, surfaced in profile (03§9.3)
	// version is account_db's optimistic-lock column (SQL store bookkeeping).
	version int
	// TokenVersion is bumped whenever credentials change (e.g. password
	// change). It is embedded in issued access tokens ("tv" claim) and
	// compared on verification, so older tokens are rejected immediately.
	TokenVersion int
	// MFA (07§2.4): the TOTP seed is stored ONLY as a KMS envelope ciphertext
	// (kms.PurposeUser, 07§5.3) and decrypted per verification. MFAEnabled
	// flips on when the user proves the authenticator works (bind → verify);
	// until then a bound seed does not gate login.
	MFASecretCipher []byte
	MFAKeyVersion   int
	MFAEnabled      bool
}

// mfaReplayKey scopes the TOTP replay memory per account.
func mfaReplayKey(accountID int64) string { return fmt.Sprintf("mfa:%d", accountID) }

// mfaSecret decrypts the account's TOTP seed for the duration of one
// verification. Callers must not retain or log the return value.
func mfaSecret(a account) ([]byte, error) {
	if len(a.MFASecretCipher) == 0 {
		return nil, errors.New("Auth.MFANotBound")
	}
	return keyring.Decrypt(kms.PurposeUser, a.MFASecretCipher, a.MFAKeyVersion)
}

// ramUser is a RAM sub-account (07§2.6). Status mirrors the account status enum.
type ramUser struct {
	ID          int64
	AccountID   int64
	Username    string // unique within the account
	DisplayName string
	Remark      string
	Status      int // 1 enabled, 2 disabled, 3 deleted (soft)
	// Policies are the attached policy names (ram_user_policy); they are the
	// user's effective policy set at /internal/authz/check.
	Policies  []string
	CreatedAt time.Time
}

// ramRole is a named role that groups policies (07§3.2 RBAC skeleton:
// user → group/role → system policy). A role carries a set of attached policy
// names; the effective policy set is the union of those documents.
type ramRole struct {
	ID          int64
	AccountID   int64
	Name        string // unique within the account
	Description string
	Policies    []string // attached policy names
	Status      int      // 1 enabled, 2 disabled
	CreatedAt   time.Time
}

// storedPolicy pairs a RAM policy name with its document JSON and parsed form.
// The document shape matches pkg-go/authz.Policy ({Version,Statement}); the
// parsed form is what the simulator evaluates.
type storedPolicy struct {
	ID        int64
	AccountID int64
	Name      string // unique within the account (07§3.2 system policy naming)
	Type      string // "system" | "custom"
	Document  string // raw policy JSON (audit + round-trip)
	Parsed    authz.Policy
	CreatedAt time.Time
}

// seedAccount provisions an account with a real salted password hash so the
// login flow is genuine (not a mock that always succeeds). created is false
// when the id already holds a real account (a persistent store keeps the seed
// across restarts). A placeholder row the verify container's dev seed wrote
// for the same id (password "!dev-seed-no-login") is taken over.
func seedAccount(ctx context.Context, id int64, name, realName, password string) (created bool, err error) {
	name = normalizeEmail(name)
	_, err = st.CreateAccount(ctx, account{
		AccountID: id, AccountName: name, RealName: realName,
		PasswordHash: hashPassword(password), Status: 1, CreatedAt: time.Now(),
	})
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, errExists) {
		return false, err
	}
	existing, gerr := st.AccountByID(ctx, id)
	if gerr != nil || existing.PasswordHash != devSeedPlaceholderHash {
		return false, nil // already seeded (or the name is taken): leave it alone
	}
	_, err = st.UpdateAccount(ctx, id, func(a *account) error {
		a.AccountName, a.RealName, a.PasswordHash, a.Status = name, realName, hashPassword(password), 1
		return nil
	})
	return err == nil, err
}

// devSeedPlaceholderHash marks the account row cmd/verify inserts for its dev
// seed AK (devSeedAccountRow); it can never match a password.
const devSeedPlaceholderHash = "!dev-seed-no-login"

// seedRAM provisions two demo RAM sub-users for the seed account so the console
// RAM page has real rows on first load (dev only).
func seedRAM(ctx context.Context, accountID int64) error {
	for _, u := range []ramUser{
		{AccountID: accountID, Username: "ops-admin", DisplayName: "运维管理员", Remark: "生产运维", Status: 1, Policies: []string{"EuEcsFullAccess"}},
		{AccountID: accountID, Username: "dev-ro", DisplayName: "开发只读", Remark: "只读开发", Status: 1, Policies: []string{"EuReadOnlyAccess"}},
	} {
		u.CreatedAt = time.Now()
		if _, err := st.CreateRAMUser(ctx, u); err != nil && !errors.Is(err, errExists) {
			return err
		}
	}
	return nil
}

// seedAK provisions one demo AccessKey for the seed account so the AK page has a
// real row on first load. The secret is discarded — never shown, never logged
// (07§2.7: the SK exists in plaintext exactly once, at creation).
func seedAK(accountID int64) error {
	_, err := akMgr.Create(accountID, accesskey.OwnerMaster, 0)
	return err
}

// seedRAMRolesPolicies provisions the three canonical roles for the seed
// account. They attach the platform system policies (EuEcsFullAccess /
// EuReadOnlyAccess, 07§3.2 naming — systemPolicyDefs), which every account
// sees, so the 策略模拟器 has real rows on first load.
func seedRAMRolesPolicies(ctx context.Context, accountID int64) error {
	for _, r := range []ramRole{
		{AccountID: accountID, Name: "Admin", Description: "全部权限", Policies: []string{"EuEcsFullAccess"}, Status: 1},
		{AccountID: accountID, Name: "Operator", Description: "运维操作", Policies: []string{"EuEcsFullAccess"}, Status: 1},
		{AccountID: accountID, Name: "ReadOnly", Description: "只读", Policies: []string{"EuReadOnlyAccess"}, Status: 1},
	} {
		r.CreatedAt = time.Now()
		if _, err := st.CreateRAMRole(ctx, r); err != nil && !errors.Is(err, errExists) {
			return err
		}
	}
	return nil
}

// --- token primitives ---

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // rand.Read failing is unrecoverable
	}
	return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// accessClaims is the access-token payload. It is ALWAYS serialised with
// encoding/json: user-controlled strings (login email, real name) used to be
// spliced into a fmt.Sprintf template, so an email such as
// `x@evil.com","sub":"100123","a":"` injected a second "sub" claim — and
// encoding/json keeps the LAST duplicate key, which turned that token into a
// token for account 100123 (account takeover, MFA bypassed).
type accessClaims struct {
	Sub       string `json:"sub"`
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	RealName  string `json:"real_name"`
	Tv        int    `json:"tv"`
	Iat       int64  `json:"iat"`
	Exp       int64  `json:"exp"`
}

// mfaClaims is the MFA-challenge payload ("typ":"mfa"), serialised the same way.
type mfaClaims struct {
	Sub string `json:"sub"`
	Typ string `json:"typ"`
	Tv  int    `json:"tv"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// signJWT serialises claims with encoding/json (never string templating)
// and returns the compact RS256 JWT (jwt.go).
func signJWT(claims any) string { return jwtSigner.sign(claims) }

// issueAccessToken signs a minimal RS256 JWT carrying sub/account_id/name/exp
// plus the account's TokenVersion ("tv") so credential changes invalidate old
// tokens. account_id is the claim the gateway injects as X-Euler-Account-Id.
func issueAccessToken(a account, now time.Time) string {
	return signJWT(accessClaims{
		Sub:       strconv.FormatInt(a.AccountID, 10),
		AccountID: strconv.FormatInt(a.AccountID, 10),
		Name:      a.AccountName,
		RealName:  a.RealName,
		Tv:        a.TokenVersion,
		Iat:       now.Unix(),
		Exp:       now.Add(accessTTL).Unix(),
	})
}

// verifyAccessToken returns the account ID and token version embedded in a
// valid JWT, or an error.
func verifyAccessToken(token string, now time.Time) (int64, int, error) {
	payload, err := jwtSigner.verify(token)
	if err != nil {
		return 0, 0, err
	}
	var p struct {
		Sub string `json:"sub"`
		Typ string `json:"typ"`
		Tv  int    `json:"tv"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0, 0, errors.New("Auth.MalformedToken")
	}
	// Token-class separation: an MFA challenge token ("typ":"mfa") carries
	// sub/tv/exp like an access token, but authorizes exactly one call
	// (/api/auth/mfa/verify). Letting it through requireAuth would make the
	// 5-minute login challenge a 5-minute full session.
	if p.Typ != "" {
		return 0, 0, errors.New("Auth.MalformedToken")
	}
	if now.Unix() >= p.Exp {
		return 0, 0, errors.New("Auth.TokenExpired")
	}
	id, err := strconv.ParseInt(p.Sub, 10, 64)
	if err != nil {
		return 0, 0, errors.New("Auth.MalformedToken")
	}
	return id, p.Tv, nil
}

// issueMFAToken signs a short-lived challenge binding one account to one
// pending second-factor verification ("typ":"mfa"). It is deliberately NOT an
// access token: it authorizes exactly the /api/auth/mfa/verify call and
// nothing else — every other endpoint ignores it.
func issueMFAToken(a account, now time.Time) string {
	return signJWT(mfaClaims{
		Sub: strconv.FormatInt(a.AccountID, 10),
		Typ: "mfa",
		Tv:  a.TokenVersion,
		Iat: now.Unix(),
		Exp: now.Add(mfaTTL).Unix(),
	})
}

// verifyMFAToken validates an MFA challenge token. It re-checks the account's
// TokenVersion so a challenge issued before a password change is void.
func verifyMFAToken(token string, now time.Time) (int64, int, error) {
	payload, err := jwtSigner.verify(token)
	if err != nil {
		return 0, 0, err
	}
	var p struct {
		Sub string `json:"sub"`
		Typ string `json:"typ"`
		Tv  int    `json:"tv"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0, 0, errors.New("Auth.MalformedToken")
	}
	if p.Typ != "mfa" {
		return 0, 0, errors.New("Auth.MalformedToken")
	}
	if now.Unix() >= p.Exp {
		return 0, 0, errors.New("Auth.TokenExpired")
	}
	id, err := strconv.ParseInt(p.Sub, 10, 64)
	if err != nil {
		return 0, 0, errors.New("Auth.MalformedToken")
	}
	return id, p.Tv, nil
}

// --- HTTP ---

type envelope struct {
	RequestId string `json:"RequestId"`
	Code      string `json:"Code"`
	Message   string `json:"Message,omitempty"`
	Data      any    `json:"Data,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, code string, data any) {
	reqID := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{RequestId: reqID, Code: code, Data: data})
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	reqID := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{RequestId: reqID, Code: code, Message: msg})
}

// internalError answers a store / infrastructure failure: 500 with a stable
// code, the cause in the log only (SQL text and hosts never reach a client).
func internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	slog.Error("web-auth: "+what+" failed", "path", r.URL.Path, "err", err)
	writeErr(w, http.StatusInternalServerError, "Common.InternalError", "internal error")
}

// decodeJSON caps the request body at maxBodyBytes and decodes it into v.
// On failure it responds with a stable error (no err.Error() detail leaks to
// the client — the detail goes to the log) and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		slog.Info("request body decode failed", "path", r.URL.Path, "err", err)
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "Common.RequestTooLarge", "request body too large")
			return false
		}
		writeErr(w, 400, "Common.InvalidParameter", "malformed body")
		return false
	}
	return true
}

// setRefreshCookie writes the HttpOnly refresh_token cookie. In production the
// Domain is .euler.emoera.com (root, shared across console/billing/ticket) via
// EULER_COOKIE_DOMAIN; in dev it is host-only (localhost).
func setRefreshCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: "eu_refresh", Value: token, Expires: exp, HttpOnly: true, Domain: cookieDomain,
		Secure: cookieSecure, SameSite: http.SameSiteLaxMode, Path: "/api/auth",
	})
}

// clearRefreshCookie must name the same Domain as setRefreshCookie: a
// host-only clear does not delete a domain cookie, so logout would leave the
// shared session behind.
func clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: "eu_refresh", Value: "", Expires: time.Unix(0, 0), MaxAge: -1, HttpOnly: true, Domain: cookieDomain,
		Secure: cookieSecure, SameSite: http.SameSiteLaxMode, Path: "/api/auth",
	})
}

// handlePublicKey serves the RS256 public key (PEM) and its kid so operators
// can provision the gateway's eu-jwt-public-key Secret. Public by nature.
func handlePublicKey(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("X-Euler-Jwt-Kid", jwtSigner.kid)
	_, _ = w.Write(jwtSigner.publicKeyPEM())
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	now := time.Now()
	// 5 failures in 15 minutes lock the email for 15 minutes (07§2.4), keyed
	// by what was submitted so a lockout says nothing about whether the
	// account exists. The gateway's per-IP limit alone did not stop a
	// distributed guess.
	lkey := loginKey(req.Email)
	if loginLimiter.Locked(lkey, now) {
		writeErr(w, 429, "Auth.TooManyAttempts", "尝试次数过多,请 15 分钟后再试")
		return
	}
	a, err := st.AccountByName(r.Context(), normalizeEmail(req.Email))
	if err != nil && !errors.Is(err, errNotFound) {
		internalError(w, r, "login lookup", err)
		return
	}
	// Constant-time-ish: always perform one hash comparison even when the
	// account does not exist, so response timing does not reveal whether an
	// email is registered.
	if err != nil {
		_ = verifyPassword(dummyPasswordHash, req.Password)
		loginLimiter.Fail(lkey, now)
		writeErr(w, 401, "Auth.InvalidCredentials", "邮箱或密码错误")
		return
	}
	if !verifyPassword(a.PasswordHash, req.Password) || a.Status != 1 {
		loginLimiter.Fail(lkey, now)
		writeErr(w, 401, "Auth.InvalidCredentials", "邮箱或密码错误")
		return
	}
	loginLimiter.Reset(lkey)

	// MFA 强制登录 (07§2.4): a bound-and-activated TOTP seed makes the
	// password merely step one. No session, no tokens — just a 5-minute
	// challenge. The response still says who is logging in so the console can
	// render "请输入动态验证码" without a second round-trip.
	if a.MFAEnabled {
		writeJSON(w, 200, "OK", map[string]any{
			"mfaRequired": true,
			"mfaToken":    issueMFAToken(a, now),
			"user":        map[string]any{"id": a.AccountID, "name": a.AccountName, "realName": a.RealName},
		})
		return
	}
	if !startSession(w, r, a, now) {
		return
	}
	writeJSON(w, 200, "OK", map[string]any{
		"accessToken": issueAccessToken(a, now),
		"user":        map[string]any{"id": a.AccountID, "name": a.AccountName, "realName": a.RealName},
	})
}

// startSession mints a refresh token (the first link of a new rotation
// chain), stores only its hash and sets the cookie. It answers the request
// itself on failure and reports false.
func startSession(w http.ResponseWriter, r *http.Request, a account, now time.Time) bool {
	token := randHex(32)
	s := refreshSession{
		TokenHash: hashToken(token), AccountID: a.AccountID, TokenVersion: a.TokenVersion,
		FamilyID: newFamilyID(), ExpiresAt: now.Add(refreshTTL), CreatedAt: now,
	}
	if err := st.CreateSession(r.Context(), s); err != nil {
		internalError(w, r, "create session", err)
		return false
	}
	setRefreshCookie(w, token, s.ExpiresAt)
	return true
}

// mfaLoginRequest completes a second-factor login: the mfaToken from the
// password step plus the 6-digit code from the authenticator.
type mfaLoginRequest struct {
	MFAToken string `json:"mfaToken"`
	Code     string `json:"code"`
}

// handleMFALoginVerify implements POST /api/auth/mfa/verify — the second half
// of login for MFA-enabled accounts. On success it mints the same session the
// password-only path would have (access_token + refresh cookie), so the client
// code after login is identical either way.
func handleMFALoginVerify(w http.ResponseWriter, r *http.Request) {
	var req mfaLoginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, tv, err := verifyMFAToken(req.MFAToken, time.Now())
	if err != nil {
		writeErr(w, 401, err.Error(), "MFA 校验凭证无效,请重新登录")
		return
	}
	a, err := st.AccountByID(r.Context(), id)
	if err != nil && !errors.Is(err, errNotFound) {
		internalError(w, r, "mfa login lookup", err)
		return
	}
	if err != nil || a.Status != 1 {
		writeErr(w, 401, "Auth.AccountNotFound", "账号不存在或不可用")
		return
	}
	if tv != a.TokenVersion {
		writeErr(w, 401, "Auth.TokenRevoked", "登录凭证已失效,请重新登录")
		return
	}
	secret, err := mfaSecret(a)
	if err != nil {
		writeErr(w, 401, "Auth.MFANotBound", "MFA 未绑定")
		return
	}
	mkey := mfaReplayKey(a.AccountID)
	if mfaLimiter.Locked(mkey, time.Now()) {
		writeErr(w, 429, "Auth.TooManyAttempts", "动态验证码错误次数过多,请 15 分钟后再试")
		return
	}
	if err := totpVerifier.Verify(mkey, secret, req.Code, time.Now()); err != nil {
		// 6 digits are 10^6 guesses; without a limit the 5-minute MFA
		// challenge (or a stolen session on bind/unbind) could be brute-forced.
		mfaLimiter.Fail(mkey, time.Now())
		writeMFACodeErr(w, err)
		return
	}
	mfaLimiter.Reset(mkey)

	now := time.Now()
	if !startSession(w, r, a, now) {
		return
	}
	writeJSON(w, 200, "OK", map[string]any{
		"accessToken": issueAccessToken(a, now),
		"user":        map[string]any{"id": a.AccountID, "name": a.AccountName, "realName": a.RealName},
	})
}

// writeMFACodeErr maps the verifier's failure to a wire code. The distinction
// matters operationally: ErrReplayed means the code WAS valid — someone just
// used it — which is a security signal worth surfacing, not a typo.
func writeMFACodeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, totp.ErrReplayed):
		writeErr(w, 401, "Auth.MFACodeReused", "验证码已被使用,请等待下一枚")
	case errors.Is(err, totp.ErrMalformedCode), errors.Is(err, totp.ErrBadCode):
		writeErr(w, 401, "Auth.MFAInvalidCode", "动态验证码错误")
	default:
		writeErr(w, 401, "Auth.MFAInvalidCode", "动态验证码错误")
	}
}

// requireAuth extracts and validates the Bearer access_token, returning the
// authenticated account. Used by authenticated endpoints (实名, profile, ...).
func requireAuth(r *http.Request) (account, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return account{}, errors.New("Auth.NoToken")
	}
	id, tv, err := verifyAccessToken(strings.TrimPrefix(auth, "Bearer "), time.Now())
	if err != nil {
		return account{}, err
	}
	a, err := st.AccountByID(r.Context(), id)
	if errors.Is(err, errNotFound) {
		return account{}, errors.New("Auth.AccountNotFound")
	}
	if err != nil {
		return account{}, &storeFailure{err}
	}
	// Token issued before a credential change (password change bumps
	// TokenVersion) is no longer accepted.
	if tv != a.TokenVersion {
		return account{}, errors.New("Auth.TokenRevoked")
	}
	return a, nil
}

// storeFailure marks an authentication that failed on infrastructure, not
// on the caller's credentials (500, not 401).
type storeFailure struct{ err error }

func (e *storeFailure) Error() string { return "Common.InternalError" }
func (e *storeFailure) Unwrap() error { return e.err }

// authed authenticates the request, answering it on failure.
func authed(w http.ResponseWriter, r *http.Request) (account, bool) {
	a, err := requireAuth(r)
	if err != nil {
		var sf *storeFailure
		if errors.As(err, &sf) {
			internalError(w, r, "authenticate", sf.err)
			return account{}, false
		}
		writeErr(w, 401, err.Error(), "未认证")
		return account{}, false
	}
	return a, true
}

// maskIDCard returns a masked ID number for display (keeps first 4 + last 2).
func maskIDCard(id string) string {
	r := []rune(id)
	if len(r) <= 6 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:4]) + strings.Repeat("*", len(r)-6) + string(r[len(r)-2:])
}

// handleRegister creates a new account with a real salted password hash.
// A duplicate email returns 409. The new account starts unverified (实名=0).
// Code is the mailed registration code (register.go): mandatory with
// EULER_REGISTER_REQUIRE_EMAIL_CODE=1, otherwise checked only when given.
type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

// maxEmailLen is the RFC 5321 forward-path limit.
const maxEmailLen = 254

// maxPasswordLen bounds the input to the (deliberately slow) password hash.
const maxPasswordLen = 128

// validLoginEmail accepts only a bare addr-spec (no display name, no quoted
// local part, no control characters). The login name ends up in tokens,
// logs and UI, so anything that is not a plain address is refused at the door.
func validLoginEmail(s string) bool {
	if s == "" || len(s) > maxEmailLen {
		return false
	}
	for _, c := range s {
		if c <= 0x20 || c == 0x7f || c == '"' || c == '\\' || c == '<' || c == '>' {
			return false
		}
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Name != "" || addr.Address != s {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	return at > 0 && at < len(s)-1 && strings.Contains(s[at+1:], ".")
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// One account per address regardless of case: the store key, the login
	// lockout key and account_db's (case-insensitive) unique key must all
	// agree, or "Alice@x.com" and "alice@x.com" become two accounts.
	email := normalizeEmail(req.Email)
	if !validLoginEmail(email) || !validPassword(req.Password) {
		writeErr(w, 400, "Common.InvalidParameter", "请填写有效邮箱,密码 8-128 位")
		return
	}
	// The mailed code proves control of the address. It is checked (and
	// consumed) before the duplicate check, so without a code for the
	// address a caller cannot learn whether it is registered.
	if code := strings.TrimSpace(req.Code); code != "" || registerCodeRequired() {
		if !checkRegisterCode(w, r, email, code) {
			return
		}
	}
	// RealName stays empty until real-name verification sets it: copying the
	// email in made a fresh account look verified to the UI and leaked the
	// address into the real-name field. Display code uses AccountName.
	a, err := st.CreateAccount(r.Context(), account{
		AccountName: email, PasswordHash: hashPassword(req.Password),
		Status: 1, RealNameStatus: 0, CreatedAt: time.Now(),
	})
	if errors.Is(err, errExists) {
		writeErr(w, 409, "Auth.EmailExists", "该邮箱已注册")
		return
	}
	if err != nil {
		internalError(w, r, "register", err)
		return
	}
	writeJSON(w, 201, "OK", map[string]any{"id": a.AccountID, "email": email, "realNameStatus": 0})
}

// validPassword is the password rule shared by register, change and reset:
// 8-128 bytes (the upper bound caps the deliberately slow hash's input).
func validPassword(pw string) bool { return len(pw) >= 8 && len(pw) <= maxPasswordLen }

// handleRealnameStatus returns the current real-name verification state.
func handleRealnameStatus(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, "OK", map[string]any{
		"status": a.RealNameStatus,
		"type":   a.RealNameType,
		"name":   a.RealName,
		"idCard": maskIDCard(a.IDCardNo),
	})
}

// handleRealnameVerify verifies a real-name submission. In production this
// hits a third-party ID-verification service; phase-1 does format validation +
// stores the verified status. Individual: 姓名+身份证号; Enterprise: 企业名+信用代码.
type realnameRequest struct {
	Type           string `json:"type"` // "individual" | "enterprise"
	Name           string `json:"name"`
	IDCardNo       string `json:"idCardNo"`       // individual
	EnterpriseName string `json:"enterpriseName"` // enterprise
	CreditCode     string `json:"creditCode"`     // enterprise
	LegalPerson    string `json:"legalPerson"`    // enterprise
}

func handleRealnameVerify(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	var req realnameRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Format first (no shared state involved): the check digits reject
	// made-up numbers before a provider is ever asked.
	check := realnameCheck{Type: req.Type}
	switch req.Type {
	case "individual":
		if !validPersonName(req.Name) || !validResidentID(req.IDCardNo, time.Now()) {
			writeErr(w, 400, "Common.InvalidParameter", "请填写真实姓名与有效的 18 位身份证号")
			return
		}
		check.Name, check.IDNumber = strings.TrimSpace(req.Name), strings.ToUpper(req.IDCardNo)
	case "enterprise":
		if !validPersonName(req.EnterpriseName) || !validCreditCode(req.CreditCode) {
			writeErr(w, 400, "Common.InvalidParameter", "请填写企业名称与有效的统一社会信用代码")
			return
		}
		check.Name, check.IDNumber = strings.TrimSpace(req.EnterpriseName), strings.ToUpper(req.CreditCode)
	default:
		writeErr(w, 400, "Common.InvalidParameter", "实名类型无效")
		return
	}
	// A verified identity is final: re-submitting used to overwrite it, so one
	// account could cycle identities (and the trial dedup that keys on them).
	// Changing a verified identity is an operator workflow, not a self-service
	// form. Checked before the provider call (it costs money) and again inside
	// the atomic update below.
	if a.RealNameStatus != 0 {
		writeErr(w, 409, "Auth.RealNameAlreadyVerified", "已完成实名认证,如需变更请提交工单")
		return
	}
	if !verifyWithProvider(w, r, a.AccountID, check) {
		return
	}
	cur, err := st.UpdateAccount(r.Context(), a.AccountID, func(cur *account) error {
		if cur.RealNameStatus != 0 {
			return errRealNameFinal
		}
		switch req.Type {
		case "individual":
			cur.RealNameStatus = 1
			cur.RealName = check.Name
			cur.IDCardNo = maskIDCard(check.IDNumber)
			cur.IdentityFingerprint = identityFingerprint("id", req.IDCardNo)
		case "enterprise":
			cur.RealNameStatus = 2
			cur.EnterpriseName = check.Name
			cur.IDCardNo = maskIDCard(check.IDNumber)
			cur.IdentityFingerprint = identityFingerprint("uscc", req.CreditCode)
		}
		cur.RealNameType = req.Type
		return nil
	})
	switch {
	case errors.Is(err, errRealNameFinal):
		writeErr(w, 409, "Auth.RealNameAlreadyVerified", "已完成实名认证,如需变更请提交工单")
		return
	case errors.Is(err, errNotFound):
		writeErr(w, 401, "Auth.AccountNotFound", "账号不存在")
		return
	case err != nil:
		internalError(w, r, "realname update", err)
		return
	}
	writeJSON(w, 200, "OK", map[string]any{
		"status": cur.RealNameStatus, "type": req.Type, "idCard": maskIDCard(cur.IDCardNo),
	})
}

var errRealNameFinal = errors.New("Auth.RealNameAlreadyVerified")

// --- MFA management (07§2.4) -----------------------------------------------
//
// Binding is two-phase on purpose: bind mints the seed and shows the QR, but
// MFAEnabled only flips when the user proves the authenticator works (verify).
// A seed that never got confirmed therefore never locks anyone out — the
// classic "bound MFA, lost phone, locked out on first login" failure cannot
// happen.

type mfaCodeRequest struct {
	Code string `json:"code"`
}

// handleMFABind implements POST /api/mfa/bind — mint a fresh TOTP seed, store
// it as a KMS envelope, and return the base32 secret + provisioning URI for
// the QR code. The secret is shown here exactly once (same convention as AK
// secrets); after this call it can only be proven, never read again.
func handleMFABind(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		writeErr(w, 500, "Common.InternalError", "生成 MFA 种子失败")
		return
	}
	cipher, version, err := keyring.Encrypt(kms.PurposeUser, secret)
	if err != nil {
		writeErr(w, 500, "Common.InternalError", "加密 MFA 种子失败")
		return
	}
	cur, err := st.UpdateAccount(r.Context(), a.AccountID, func(cur *account) error {
		if cur.MFAEnabled {
			return errMFAAlreadyBound
		}
		cur.MFASecretCipher = cipher
		cur.MFAKeyVersion = version
		cur.MFAEnabled = false
		return nil
	})
	switch {
	case errors.Is(err, errNotFound):
		writeErr(w, 401, "Auth.AccountNotFound", "账号不存在")
		return
	case errors.Is(err, errMFAAlreadyBound):
		writeErr(w, 409, "Auth.MFAAlreadyBound", "已开启 MFA,请先解绑后再重新绑定")
		return
	case err != nil:
		internalError(w, r, "mfa bind", err)
		return
	}
	writeJSON(w, 200, "OK", map[string]any{
		"secret":             totp.SecretBase32(secret),
		"provisioningUri":    totp.ProvisioningURI(secret, cur.AccountName, "Euler"),
		"activationRequired": true,
	})
}

var errMFAAlreadyBound = errors.New("Auth.MFAAlreadyBound")

// handleMFABindVerify implements POST /api/mfa/verify — confirm a code from
// the freshly bound authenticator and ACTIVATE the factor. From this moment
// password-only login is refused for the account.
func handleMFABindVerify(w http.ResponseWriter, r *http.Request) {
	cur, ok := authed(w, r)
	if !ok {
		return
	}
	var req mfaCodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(cur.MFASecretCipher) == 0 {
		writeErr(w, 409, "Auth.MFANotBound", "请先绑定 MFA")
		return
	}
	// The seed the code is checked against. Activation below must enable
	// exactly this seed.
	verifiedCipher, verifiedVersion := cur.MFASecretCipher, cur.MFAKeyVersion
	secret, err := mfaSecret(cur)
	if err != nil {
		writeErr(w, 500, "Common.InternalError", "读取 MFA 种子失败")
		return
	}
	mkey := mfaReplayKey(cur.AccountID)
	if mfaLimiter.Locked(mkey, time.Now()) {
		writeErr(w, 429, "Auth.TooManyAttempts", "动态验证码错误次数过多,请 15 分钟后再试")
		return
	}
	if err := totpVerifier.Verify(mkey, secret, req.Code, time.Now()); err != nil {
		// 6 digits are 10^6 guesses; without a limit the 5-minute MFA
		// challenge (or a stolen session on bind/unbind) could be brute-forced.
		mfaLimiter.Fail(mkey, time.Now())
		writeMFACodeErr(w, err)
		return
	}
	mfaLimiter.Reset(mkey)

	if err := enableVerifiedMFA(r.Context(), cur.AccountID, verifiedCipher, verifiedVersion); err != nil {
		if errors.Is(err, errMFASeedChanged) || errors.Is(err, errNotFound) {
			writeErr(w, 409, "Auth.MFASeedChanged", "MFA 种子已变更,请使用最新绑定的验证器重新验证")
			return
		}
		internalError(w, r, "mfa activate", err)
		return
	}
	writeJSON(w, 200, "OK", map[string]any{"mfaEnabled": true})
}

// enableVerifiedMFA turns the factor on — but only if the stored seed is still the
// one the code was verified against. The verification runs outside the lock,
// so a concurrent re-bind (another session calling /api/mfa/bind) could
// replace the seed in between; enabling whatever is stored then would
// activate a seed nobody has proven, locking the user out (or arming an
// attacker's seed).
func enableVerifiedMFA(ctx context.Context, accountID int64, verifiedCipher []byte, verifiedVersion int) error {
	_, err := st.UpdateAccount(ctx, accountID, func(cur *account) error {
		if !bytes.Equal(cur.MFASecretCipher, verifiedCipher) || cur.MFAKeyVersion != verifiedVersion {
			return errMFASeedChanged
		}
		cur.MFAEnabled = true
		return nil
	})
	return err
}

var errMFASeedChanged = errors.New("Auth.MFASeedChanged")

// handleMFAUnbind implements POST /api/mfa/unbind — remove the factor. A live
// code is required, not just the session: a stolen access token (XSS shoulder
// surfing) must not be enough to strip the account's second factor, which is
// exactly the moment an attacker would otherwise strike.
func handleMFAUnbind(w http.ResponseWriter, r *http.Request) {
	cur, ok := authed(w, r)
	if !ok {
		return
	}
	var req mfaCodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(cur.MFASecretCipher) == 0 {
		writeErr(w, 409, "Auth.MFANotBound", "未绑定 MFA")
		return
	}
	secret, err := mfaSecret(cur)
	if err != nil {
		writeErr(w, 500, "Common.InternalError", "读取 MFA 种子失败")
		return
	}
	mkey := mfaReplayKey(cur.AccountID)
	if mfaLimiter.Locked(mkey, time.Now()) {
		writeErr(w, 429, "Auth.TooManyAttempts", "动态验证码错误次数过多,请 15 分钟后再试")
		return
	}
	if err := totpVerifier.Verify(mkey, secret, req.Code, time.Now()); err != nil {
		// 6 digits are 10^6 guesses; without a limit the 5-minute MFA
		// challenge (or a stolen session on bind/unbind) could be brute-forced.
		mfaLimiter.Fail(mkey, time.Now())
		writeMFACodeErr(w, err)
		return
	}
	mfaLimiter.Reset(mkey)

	if _, err := st.UpdateAccount(r.Context(), cur.AccountID, func(a *account) error {
		a.MFASecretCipher = nil
		a.MFAKeyVersion = 0
		a.MFAEnabled = false
		return nil
	}); err != nil {
		internalError(w, r, "mfa unbind", err)
		return
	}
	totpVerifier.Forget(mfaReplayKey(cur.AccountID))
	writeJSON(w, 200, "OK", map[string]any{"mfaEnabled": false})
}

// --- account profile & password (03§9.3) ---

// handleAccountProfile returns the authenticated account's profile snapshot.
// Mirrors the shape the console Profile page renders (realName, realNameStatus,
// createdAt, email, phoneBound, mfaEnabled).
func handleAccountProfile(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, "OK", map[string]any{
		"id":             a.AccountID,
		"name":           a.AccountName,
		"realName":       a.RealName,
		"realNameStatus": a.RealNameStatus,
		"createdAt":      a.CreatedAt.UTC().Format(time.RFC3339),
		"email":          a.AccountName,
		"phoneBound":     false,        // phase-1: no phone-binding flow yet (07§2.5)
		"mfaEnabled":     a.MFAEnabled, // real state since M-2: KMS-sealed TOTP seed (07§2.4)
		// Password status for the security settings card: the account always
		// authenticates by password in phase-1, so the hash presence IS the truth.
		"passwordSet": a.PasswordHash != "",
	})
}

// handleChangePassword verifies the old password and installs a fresh salted
// hash for the new one, atomically. It bumps the account TokenVersion and
// revokes the account's refresh sessions, so all previously issued tokens and
// sessions stop working (the client must log in again).
type changePasswordRequest struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

func handleChangePassword(w http.ResponseWriter, r *http.Request) {
	snap, ok := authed(w, r)
	if !ok {
		return
	}
	var req changePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validPassword(req.NewPassword) {
		writeErr(w, 400, "Common.InvalidParameter", "新密码 8-128 位")
		return
	}
	pkey := "pwchange:" + strconv.FormatInt(snap.AccountID, 10)
	if passwordChangeLimiter.Locked(pkey, time.Now()) {
		writeErr(w, 429, "Auth.TooManyAttempts", "尝试次数过多,请 15 分钟后再试")
		return
	}
	// The (slow) hash work runs outside the atomic update; the install step
	// re-checks that the stored hash is still the one the old password was
	// verified against, so a concurrent change is not lost.
	if !verifyPassword(snap.PasswordHash, req.OldPassword) {
		passwordChangeLimiter.Fail(pkey, time.Now())
		writeErr(w, 400, "Auth.OldPasswordMismatch", "原密码不正确")
		return
	}
	passwordChangeLimiter.Reset(pkey)
	newHash := hashPassword(req.NewPassword)
	_, err := st.UpdateAccount(r.Context(), snap.AccountID, func(cur *account) error {
		if cur.PasswordHash != snap.PasswordHash {
			return errConcurrentPassword
		}
		cur.PasswordHash = newHash
		// Invalidate everything issued before the change: the bumped
		// TokenVersion fails outstanding access tokens and every refresh
		// session minted under the old version.
		cur.TokenVersion++
		return nil
	})
	switch {
	case errors.Is(err, errConcurrentPassword), errors.Is(err, errNotFound):
		writeErr(w, 409, "Auth.ConcurrentUpdate", "密码已被修改,请重试")
		return
	case err != nil:
		internalError(w, r, "change password", err)
		return
	}
	if err := st.RevokeAccountSessions(r.Context(), snap.AccountID, revokePassword, time.Now()); err != nil {
		// Not fatal: the TokenVersion bump already voids those sessions.
		slog.Error("revoke sessions after password change failed", "account", snap.AccountID, "err", err)
	}
	writeJSON(w, 200, "OK", map[string]any{})
}

var errConcurrentPassword = errors.New("Auth.ConcurrentUpdate")

// --- RAM sub-users (07§2.6) ---

// handleListRAMUsers returns the (non-deleted) sub-users for the account.
func handleListRAMUsers(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	users, err := st.ListRAMUsers(r.Context(), a.AccountID)
	if err != nil {
		internalError(w, r, "list ram users", err)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, ramUserView(u))
	}
	writeJSON(w, 200, "OK", map[string]any{"users": out, "total": len(out)})
}

func ramUserView(u ramUser) map[string]any {
	return map[string]any{
		"id":          u.ID,
		"username":    u.Username,
		"displayName": u.DisplayName,
		"remark":      u.Remark,
		"status":      u.Status,
		"policies":    nonNilNames(u.Policies),
		"createdAt":   u.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func nonNilNames(n []string) []string {
	if n == nil {
		return []string{}
	}
	return n
}

// writePolicyNotFound answers an attach / simulate request naming policies
// the account cannot see.
func writePolicyNotFound(w http.ResponseWriter, names []string) {
	writeErr(w, 404, "Auth.PolicyNotFound", "策略不存在: "+strings.Join(names, ", "))
}

type createRAMUserRequest struct {
	Username    string   `json:"username"`
	DisplayName string   `json:"displayName"`
	Remark      string   `json:"remark"`
	Policies    []string `json:"policies"` // attached policy names (optional)
}

// handleCreateRAMUser creates a sub-user. Idempotent on accountID+username:
// a duplicate returns 409 Auth.RAMUserExists (07§2.6).
func handleCreateRAMUser(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	var req createRAMUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Username == "" {
		writeErr(w, 400, "Common.InvalidParameter", "用户名必填")
		return
	}
	if !ramUsernamePattern.MatchString(req.Username) || !validText(req.DisplayName, 64, false) || !validText(req.Remark, 256, false) {
		writeErr(w, 400, "Common.InvalidParameter", "用户名 1-64 位字母/数字/._@-,显示名 ≤64 字,备注 ≤256 字")
		return
	}
	u, err := st.CreateRAMUser(r.Context(), ramUser{
		AccountID: a.AccountID, Username: req.Username,
		DisplayName: req.DisplayName, Remark: req.Remark,
		Status: 1, Policies: dedupeNames(req.Policies), CreatedAt: time.Now(),
	})
	var pnf *policyNotFoundError
	switch {
	case errors.Is(err, errExists):
		writeErr(w, 409, "Auth.RAMUserExists", "该子用户已存在")
		return
	case errors.As(err, &pnf):
		writePolicyNotFound(w, pnf.names)
		return
	case err != nil:
		internalError(w, r, "create ram user", err)
		return
	}
	writeJSON(w, 201, "OK", ramUserView(u))
}

// handleDeleteRAMUser soft-deletes a sub-user (status=3). A missing user
// returns 404; a deleted one returns 409 to keep DELETE idempotent-ish.
func handleDeleteRAMUser(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "Common.InvalidParameter", "无效的子用户 ID")
		return
	}
	switch err := st.DeleteRAMUser(r.Context(), a.AccountID, id); {
	case errors.Is(err, errNotFound):
		writeErr(w, 404, "Auth.RAMUserNotFound", "子用户不存在")
		return
	case errors.Is(err, errAlreadyDeleted):
		writeErr(w, 409, "Auth.RAMUserDeleted", "该子用户已删除")
		return
	case err != nil:
		internalError(w, r, "delete ram user", err)
		return
	}
	// A deleted user's keys must not outlive it. CheckAccess already denies a
	// deleted principal; deleting the keys also stops them verifying at all.
	deleteOwnerKeys(r.Context(), a.AccountID, accesskey.OwnerRAMUser, id)
	writeJSON(w, 200, "OK", map[string]any{})
}

// --- AccessKeys (07§2.7) ---

// stsIssuer mints temporary credentials (STS, phase-3 D-3; 03§9.2
// x-cps-security-token). Initialized in main with the default TTL.
var stsIssuer *sts.Issuer

// handleAssumeRole implements POST /api/sts/assume-role — the phase-3 STS
// productization (07§10 M1 角色/STS; deferred from phase 2). It issues a
// temporary AK/SK/SecurityToken triple for the authenticated account; the
// token rides x-cps-security-token so the gateway verifier can validate it on
// top of the CPS1 signature (03§9.2). The plaintext secret is returned once.
func handleAssumeRole(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "Auth.InvalidParameter", "malformed body")
		return
	}
	roleName := strings.TrimSpace(req.Role)
	if roleName == "" {
		writeErr(w, 400, "Auth.InvalidParameter", "role is required")
		return
	}
	// A role may only be assumed if it exists on THIS account and is enabled.
	// Without this lookup the role name is a free-form string that ends up in
	// the security token's principal — any signed-in tenant could mint itself
	// a token claiming an arbitrary (e.g. platform-admin) role.
	role, err := st.RAMRoleByName(r.Context(), a.AccountID, roleName)
	if errors.Is(err, errNotFound) {
		writeErr(w, 403, "Auth.RoleNotAssumable", "该角色不存在")
		return
	}
	if err != nil {
		internalError(w, r, "assume role lookup", err)
		return
	}
	if role.Status != 1 {
		writeErr(w, 403, "Auth.RoleNotAssumable", "该角色已被禁用")
		return
	}
	if stsIssuer == nil {
		writeErr(w, 503, "Common.InternalError", "STS not initialised")
		return
	}
	cred, err := stsIssuer.AssumeRole(roleName, strconv.FormatInt(a.AccountID, 10))
	if err != nil {
		writeErr(w, 500, "Common.InternalError", err.Error())
		return
	}
	writeJSON(w, 200, "OK", map[string]any{
		"accessKey":     cred.AccessKey,
		"secretKey":     cred.SecretKey, // shown once
		"securityToken": cred.SecurityToken,
		"expiresAt":     cred.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleListAccessKeys returns the AKs of the account root, or of one RAM
// user (?ramUserId=). The secret is never returned.
func handleListAccessKeys(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	var ramUserID int64
	if v := r.URL.Query().Get("ramUserId"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, 400, "Common.InvalidParameter", "无效的子用户 ID")
			return
		}
		ramUserID = id
	}
	ot, oid, ok := akOwner(w, r, a, ramUserID, false)
	if !ok {
		return
	}
	recs, err := akStore.ListByOwner(a.AccountID, ot, oid)
	if err != nil {
		internalError(w, r, "list access keys", err)
		return
	}
	now := time.Now()
	out := make([]map[string]any, 0, len(recs))
	for _, k := range recs {
		if k.Status == accesskey.StatusDeleted {
			continue
		}
		out = append(out, akView(k, now))
	}
	writeJSON(w, 200, "OK", map[string]any{"accessKeys": out, "total": len(out)})
}

// akView is the console projection of an AK row. The secret never appears.
// A superseded key whose rotation grace has closed reports status 2: the
// verifier already refuses it (07§2.2), whatever the row's flag says.
func akView(k accesskey.Record, now time.Time) map[string]any {
	inGrace := !k.GraceUntil.IsZero() && now.Before(k.GraceUntil)
	status := int(k.Status)
	if k.Status == accesskey.StatusEnabled && !k.GraceUntil.IsZero() && !inGrace {
		status = int(accesskey.StatusDisabled)
	}
	var lastUsed any // null until the key has been used
	if !k.LastUsedAt.IsZero() {
		lastUsed = k.LastUsedAt.UTC().Format(time.RFC3339)
	}
	v := map[string]any{
		"akId":          k.AK,
		"status":        status,
		"createdAt":     k.CreatedAt.UTC().Format(time.RFC3339),
		"lastUsed":      lastUsed,
		"rotationGrace": inGrace,
	}
	if inGrace {
		v["graceUntil"] = k.GraceUntil.UTC().Format(time.RFC3339)
	}
	if k.OwnerType == accesskey.OwnerRAMUser {
		v["ramUserId"] = k.OwnerID
	}
	return v
}

// akOwner resolves which identity's keys a request manages: the account root
// (ramUserID 0) or one of the account's RAM users. Creating a key requires an
// enabled user; listing also works for a disabled one.
func akOwner(w http.ResponseWriter, r *http.Request, a account, ramUserID int64, forCreate bool) (accesskey.OwnerType, int64, bool) {
	if ramUserID == 0 {
		return accesskey.OwnerMaster, 0, true
	}
	u, err := st.RAMUserByID(r.Context(), a.AccountID, ramUserID)
	if errors.Is(err, errNotFound) || (err == nil && u.Status == 3) {
		writeErr(w, 404, "Auth.RAMUserNotFound", "子用户不存在")
		return 0, 0, false
	}
	if err != nil {
		internalError(w, r, "access key owner lookup", err)
		return 0, 0, false
	}
	if forCreate && u.Status != 1 {
		writeErr(w, 409, "Auth.RAMUserDisabled", "子用户已禁用,不能创建 AccessKey")
		return 0, 0, false
	}
	return accesskey.OwnerRAMUser, u.ID, true
}

// lockAKs takes the per-account AK lock (the 2-key cap is check-then-insert),
// answering the request itself on failure.
func lockAKs(w http.ResponseWriter, r *http.Request, accountID int64) (func(), bool) {
	unlock, err := akLocks.Lock(r.Context(), akLockKey(accountID))
	if err != nil {
		internalError(w, r, "access key lock", err)
		return nil, false
	}
	return unlock, true
}

// ownedKey loads an AK of the caller's account; another account's key, a
// deleted key and a missing key are all 404.
func ownedKey(w http.ResponseWriter, r *http.Request, a account) (accesskey.Record, bool) {
	akID := r.PathValue("akId")
	if akID == "" {
		writeErr(w, 400, "Common.InvalidParameter", "缺少 akId")
		return accesskey.Record{}, false
	}
	rec, err := akStore.Get(akID)
	if errors.Is(err, accesskey.ErrNotFound) || (err == nil && (rec.AccountID != a.AccountID || rec.Status == accesskey.StatusDeleted)) {
		writeErr(w, 404, "Auth.AKNotFound", "AccessKey 不存在")
		return accesskey.Record{}, false
	}
	if err != nil {
		internalError(w, r, "access key lookup", err)
		return accesskey.Record{}, false
	}
	return rec, true
}

// writeAKErr maps the pkg-go/accesskey lifecycle errors onto wire codes.
func writeAKErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, accesskey.ErrTooManyKeys):
		writeErr(w, 409, "Auth.AKLimitExceeded", "每个身份最多 2 个 AccessKey(含已禁用),请先删除一个")
	case errors.Is(err, accesskey.ErrNotFound), errors.Is(err, accesskey.ErrAlreadyDeleted):
		writeErr(w, 404, "Auth.AKNotFound", "AccessKey 不存在")
	case errors.Is(err, accesskey.ErrDisabled):
		writeErr(w, 409, "Auth.AKDisabled", "已禁用(或宽限期已过)的 AccessKey 不能轮换,请先启用")
	case errors.Is(err, accesskey.ErrRotationInProgress):
		writeErr(w, 409, "Auth.AKRotationInProgress", "该 AccessKey 已在轮换宽限期内")
	default:
		internalError(w, r, "access key lifecycle", err)
	}
}

type createAccessKeyRequest struct {
	// RAMUserID creates the key for one of the account's RAM users; absent
	// or 0 creates a key for the account root.
	RAMUserID int64 `json:"ramUserId"`
}

// handleCreateAccessKey creates an AK through pkg-go/accesskey: the SK is
// sealed with the shared KMS key (mk-ak) into the store cmd/verify reads, and
// returned in plaintext exactly ONCE here (07§2.2). Max 2 keys per identity
// (enabled or disabled): a third is 409 Auth.AKLimitExceeded.
func handleCreateAccessKey(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	var req createAccessKeyRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, 400, "Common.InvalidParameter", "malformed body")
		return
	}
	if req.RAMUserID < 0 {
		writeErr(w, 400, "Common.InvalidParameter", "无效的子用户 ID")
		return
	}
	ot, oid, ok := akOwner(w, r, a, req.RAMUserID, true)
	if !ok {
		return
	}
	unlock, ok := lockAKs(w, r, a.AccountID)
	if !ok {
		return
	}
	created, err := akMgr.Create(a.AccountID, ot, oid)
	unlock()
	if err != nil {
		writeAKErr(w, r, err)
		return
	}
	// WARNING: secretKey is shown exactly once — the store keeps only the KMS
	// envelope. The client must persist it now.
	out := map[string]any{
		"akId":      created.Record.AK,
		"secretKey": created.SKPlaintext,
		"createdAt": created.Record.CreatedAt.UTC().Format(time.RFC3339),
		"status":    int(created.Record.Status),
	}
	if ot == accesskey.OwnerRAMUser {
		out["ramUserId"] = oid
	}
	writeJSON(w, 201, "OK", out)
}

// handleDisableAccessKey disables an AK. A disabled AK cannot sign requests.
// Disabling an already-disabled AK is a no-op success (idempotent).
func handleDisableAccessKey(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	unlock, ok := lockAKs(w, r, a.AccountID)
	if !ok {
		return
	}
	defer unlock()
	rec, ok := ownedKey(w, r, a)
	if !ok {
		return
	}
	if err := akMgr.SetStatus(rec.AK, accesskey.StatusDisabled); err != nil {
		writeAKErr(w, r, err)
		return
	}
	writeJSON(w, 200, "OK", map[string]any{"akId": rec.AK, "status": 2})
}

// handleEnableAccessKey re-enables a disabled AK (and ends a rotation grace:
// an explicit enable keeps the key). Subject to the per-identity cap.
func handleEnableAccessKey(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	unlock, ok := lockAKs(w, r, a.AccountID)
	if !ok {
		return
	}
	defer unlock()
	rec, ok := ownedKey(w, r, a)
	if !ok {
		return
	}
	if err := akMgr.SetStatus(rec.AK, accesskey.StatusEnabled); err != nil {
		if errors.Is(err, accesskey.ErrTooManyKeys) {
			writeErr(w, 409, "Auth.AKLimitExceeded", "每个身份最多同时启用 2 个 AccessKey(轮换宽限期内的旧密钥也计入)")
			return
		}
		writeAKErr(w, r, err)
		return
	}
	writeJSON(w, 200, "OK", map[string]any{"akId": rec.AK, "status": 1})
}

// handleRotateAccessKey mints a replacement AK while the old one keeps working
// through the rotation grace window (≤72h, 07§2.2), after which the verifier
// refuses it. The new secretKey is returned ONCE.
func handleRotateAccessKey(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	unlock, ok := lockAKs(w, r, a.AccountID)
	if !ok {
		return
	}
	defer unlock()
	rec, ok := ownedKey(w, r, a)
	if !ok {
		return
	}
	created, err := akMgr.Rotate(rec.AK, accesskey.MaxRotationGrace)
	if err != nil {
		writeAKErr(w, r, err)
		return
	}
	writeJSON(w, 201, "OK", map[string]any{
		"akId":      created.Record.AK,
		"secretKey": created.SKPlaintext,
		"createdAt": created.Record.CreatedAt.UTC().Format(time.RFC3339),
		"status":    int(created.Record.Status),
		"oldAkId":   rec.AK,
	})
}

// handleDeleteAccessKey deletes an AK (soft delete in the shared store: the
// verifier refuses it at once, the row stays 7 days for audit). Allowed during
// the rotation grace window — deleting the outgoing AK after migration is the
// intended path.
func handleDeleteAccessKey(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	unlock, ok := lockAKs(w, r, a.AccountID)
	if !ok {
		return
	}
	defer unlock()
	rec, ok := ownedKey(w, r, a)
	if !ok {
		return
	}
	if err := akMgr.Delete(rec.AK); err != nil {
		writeAKErr(w, r, err)
		return
	}
	writeJSON(w, 200, "OK", map[string]any{"akId": rec.AK})
}

// deleteOwnerKeys soft-deletes every key of one identity (RAM user removal).
// Best effort: a failure is logged — CheckAccess denies the deleted principal
// regardless.
func deleteOwnerKeys(ctx context.Context, accountID int64, ot accesskey.OwnerType, ownerID int64) {
	unlock, err := akLocks.Lock(ctx, akLockKey(accountID))
	if err != nil {
		slog.Error("delete keys of removed identity: lock failed", "account", accountID, "owner", ownerID, "err", err)
		return
	}
	defer unlock()
	recs, err := akStore.ListByOwner(accountID, ot, ownerID)
	if err != nil {
		slog.Error("delete keys of removed identity: list failed", "account", accountID, "owner", ownerID, "err", err)
		return
	}
	for _, k := range recs {
		if k.Status == accesskey.StatusDeleted {
			continue
		}
		if err := akMgr.Delete(k.AK); err != nil && !errors.Is(err, accesskey.ErrAlreadyDeleted) {
			slog.Error("delete keys of removed identity failed", "account", accountID, "ak", k.AK, "err", err)
		}
	}
}

// --- RAM roles & policies (07§3.2) + 策略模拟器 (07§3) ---

// handleListRAMRoles returns the account's roles with their attached policy names.
func handleListRAMRoles(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	roles, err := st.ListRAMRoles(r.Context(), a.AccountID)
	if err != nil {
		internalError(w, r, "list ram roles", err)
		return
	}
	out := make([]map[string]any, 0, len(roles))
	for _, role := range roles {
		out = append(out, ramRoleView(role))
	}
	writeJSON(w, 200, "OK", map[string]any{"roles": out, "total": len(out)})
}

func ramRoleView(role ramRole) map[string]any {
	return map[string]any{
		"id":          role.ID,
		"name":        role.Name,
		"description": role.Description,
		"policies":    nonNilNames(role.Policies),
		"status":      role.Status,
		"createdAt":   role.CreatedAt.UTC().Format(time.RFC3339),
	}
}

type createRoleRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Policies    []string `json:"policies"`
}

// handleCreateRAMRole creates a role. Idempotent on accountID+name: a duplicate
// returns 409 Auth.RAMRoleExists.
func handleCreateRAMRole(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	var req createRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeErr(w, 400, "Common.InvalidParameter", "角色名必填")
		return
	}
	if !validText(req.Name, 64, true) || strings.Contains(req.Name, "/") || !validText(req.Description, 256, false) {
		writeErr(w, 400, "Common.InvalidParameter", "角色名 ≤64 字且不含 /,描述 ≤256 字")
		return
	}
	role, err := st.CreateRAMRole(r.Context(), ramRole{
		AccountID: a.AccountID, Name: req.Name, Description: req.Description,
		Policies: dedupeNames(req.Policies), Status: 1, CreatedAt: time.Now(),
	})
	var pnf *policyNotFoundError
	switch {
	case errors.Is(err, errExists):
		writeErr(w, 409, "Auth.RAMRoleExists", "该角色已存在")
		return
	case errors.As(err, &pnf):
		writePolicyNotFound(w, pnf.names)
		return
	case err != nil:
		internalError(w, r, "create ram role", err)
		return
	}
	writeJSON(w, 201, "OK", ramRoleView(role))
}

// handleListRAMPolicies returns the account's policies (system + custom).
func handleListRAMPolicies(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	pols, err := st.ListPolicies(r.Context(), a.AccountID)
	if err != nil {
		internalError(w, r, "list ram policies", err)
		return
	}
	out := make([]map[string]any, 0, len(pols))
	for _, p := range pols {
		out = append(out, policyView(p))
	}
	writeJSON(w, 200, "OK", map[string]any{"policies": out, "total": len(out)})
}

func policyView(p storedPolicy) map[string]any {
	return map[string]any{
		"id":        p.ID,
		"name":      p.Name,
		"type":      p.Type,
		"document":  p.Document,
		"createdAt": p.CreatedAt.UTC().Format(time.RFC3339),
	}
}

type createPolicyRequest struct {
	Name     string `json:"name"`
	Document string `json:"document"`
}

// handleCreateRAMPolicy creates a custom policy. The document is validated by
// authz.ParsePolicy before storage, so a malformed policy never persists — the
// same engine the simulator and gateway use decides whether the JSON is valid.
func handleCreateRAMPolicy(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	var req createPolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.Document == "" {
		writeErr(w, 400, "Common.InvalidParameter", "策略名与文档必填")
		return
	}
	if !validText(req.Name, 64, true) {
		writeErr(w, 400, "Common.InvalidParameter", "策略名 ≤64 字")
		return
	}
	parsed, err := authz.ParsePolicy([]byte(req.Document))
	if err != nil {
		writeErr(w, 400, "Auth.InvalidPolicyDocument", err.Error())
		return
	}
	sp, err := st.CreatePolicy(r.Context(), storedPolicy{
		AccountID: a.AccountID, Name: req.Name, Type: "custom",
		Document: req.Document, Parsed: parsed, CreatedAt: time.Now(),
	})
	if errors.Is(err, errExists) {
		writeErr(w, 409, "Auth.PolicyExists", "该策略已存在")
		return
	}
	if err != nil {
		internalError(w, r, "create ram policy", err)
		return
	}
	writeJSON(w, 201, "OK", policyView(sp))
}

// simulateRequest is the 策略模拟器 body. Principal is an audit/display label
// (the policy document has no Principal field — platform-internal policies are
// always bound to an identity, so the binding carries the principal).
type simulateRequest struct {
	Principal string   `json:"principal"`
	Policies  []string `json:"policies"` // policy names to evaluate, resolved from the account's policies
	Action    string   `json:"action"`
	Resource  string   `json:"resource"`
}

// handleSimulate resolves the named policies against the account's stored set,
// runs authz.Simulate (Deny-first, 07§3.3), and returns the verdict naming the
// deciding statement. This is the backend for the console 策略模拟器 — an
// operator tests "would principal X be allowed to do Y on Z?" before committing.
func handleSimulate(w http.ResponseWriter, r *http.Request) {
	a, ok := authed(w, r)
	if !ok {
		return
	}
	var req simulateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Action == "" || req.Resource == "" {
		writeErr(w, 400, "Common.InvalidParameter", "action 与 resource 必填")
		return
	}
	found, err := st.PoliciesByName(r.Context(), a.AccountID, req.Policies)
	if err != nil {
		internalError(w, r, "simulate policy lookup", err)
		return
	}
	named := make([]authz.NamedPolicy, 0, len(req.Policies))
	unknown := make([]string, 0)
	for _, name := range req.Policies {
		sp, ok := found[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		named = append(named, authz.NamedPolicy{Name: name, Policy: sp.Parsed})
	}
	if len(unknown) > 0 {
		writePolicyNotFound(w, unknown)
		return
	}
	v := authz.Simulate(named, req.Principal, req.Action, req.Resource)
	writeJSON(w, 200, "OK", map[string]any{
		"allowed":           v.Allowed,
		"decidingStatement": v.DecidingStatement,
		"decidingPolicy":    v.DecidingPolicy,
		"reason":            v.Reason,
		"principal":         v.Principal,
	})
}

func handleRefresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("eu_refresh")
	if err != nil {
		writeErr(w, 401, "Auth.NoSession", "无活跃会话")
		return
	}
	ctx, now := r.Context(), time.Now()
	oldHash := hashToken(c.Value)
	s, err := st.SessionByHash(ctx, oldHash)
	if err != nil && !errors.Is(err, errNotFound) {
		internalError(w, r, "refresh lookup", err)
		return
	}
	if err != nil || !s.live(now) {
		// A token that was rotated away, presented again after the
		// concurrent-refresh grace, is a replay of a stolen cookie: revoke
		// the whole chain so the thief's copy dies with the victim's.
		if err == nil && s.RevokeReason == revokeRotated && now.Sub(s.RevokedAt) > refreshReuseGrace {
			slog.Warn("refresh token reuse detected — revoking the session chain", "account", s.AccountID)
			if rerr := st.RevokeSessionFamily(ctx, s.FamilyID, revokeReuse, now); rerr != nil {
				slog.Error("revoke session chain failed", "account", s.AccountID, "err", rerr)
			}
		}
		writeErr(w, 401, "Auth.SessionExpired", "会话已过期,请重新登录")
		return
	}
	// Re-check the live account: a disabled/closed (or deleted) account must
	// not be able to refresh into a fresh access token.
	cur, err := st.AccountByID(ctx, s.AccountID)
	if err != nil && !errors.Is(err, errNotFound) {
		internalError(w, r, "refresh account lookup", err)
		return
	}
	if err != nil || cur.Status != 1 {
		_ = st.RevokeSession(ctx, oldHash, revokeAccount, now)
		writeErr(w, 401, "Auth.AccountUnusable", "账号不可用,请联系管理员")
		return
	}
	// A session minted before a password change / reset is void.
	if s.TokenVersion != cur.TokenVersion {
		_ = st.RevokeSession(ctx, oldHash, revokePassword, now)
		writeErr(w, 401, "Auth.SessionExpired", "会话已过期,请重新登录")
		return
	}
	// Rotate: retire the presented token and mint its successor in the same
	// chain, atomically (02§5.1).
	newRefresh := randHex(32)
	next := refreshSession{
		TokenHash: hashToken(newRefresh), AccountID: cur.AccountID, TokenVersion: cur.TokenVersion,
		FamilyID: s.FamilyID, ParentHash: oldHash, ExpiresAt: now.Add(refreshTTL), CreatedAt: now,
	}
	if err := st.RotateSession(ctx, oldHash, next, now); err != nil {
		if errors.Is(err, errNotFound) { // a concurrent refresh won the rotation
			writeErr(w, 401, "Auth.SessionExpired", "会话已过期,请重新登录")
			return
		}
		internalError(w, r, "rotate session", err)
		return
	}
	setRefreshCookie(w, newRefresh, next.ExpiresAt)
	writeJSON(w, 200, "OK", map[string]any{"accessToken": issueAccessToken(cur, now)})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("eu_refresh"); err == nil {
		if err := st.RevokeSession(r.Context(), hashToken(c.Value), revokeLogout, time.Now()); err != nil {
			slog.Error("logout: revoke session failed", "err", err)
		}
	}
	clearRefreshCookie(w)
	writeJSON(w, 200, "OK", map[string]any{})
}

func handleSession(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeErr(w, 401, "Auth.NoToken", "缺少 access_token")
		return
	}
	id, tv, err := verifyAccessToken(strings.TrimPrefix(auth, "Bearer "), time.Now())
	if err != nil {
		writeErr(w, 401, err.Error(), "token 无效")
		return
	}
	a, err := st.AccountByID(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, 401, "Auth.AccountNotFound", "账号不存在")
		return
	}
	if err != nil {
		internalError(w, r, "session lookup", err)
		return
	}
	if tv != a.TokenVersion {
		writeErr(w, 401, "Auth.TokenRevoked", "token 已失效,请重新登录")
		return
	}
	writeJSON(w, 200, "OK", map[string]any{
		"user":           map[string]any{"id": a.AccountID, "name": a.AccountName, "realName": a.RealName},
		"realNameStatus": a.RealNameStatus,
		"permissions":    map[string]any{"actions": []string{"euecs:Read", "euecs:Start", "euoss:Read", "eurds:Read", "euvpc:Read", "eumon:Read"}},
	})
}

// --- middleware (matches the shared scaffold pattern) ---

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := httpmw.SanitizeRequestID(r.Header.Get("X-Euler-TraceId"))
		if id == "" {
			id = "webauth-" + randHex(8)
		}
		w.Header().Set("X-Euler-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

func recoverMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "rec", rec, "path", r.URL.Path)
				writeErr(w, 500, "Common.InternalError", "internal error")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// seedDemoData provisions the local-development account (100123, password
// euler123 — published in the README) plus demo RAM users, an AK and RAM
// roles/policies, so the console pages render real rows on first load. Only
// reachable with EULER_DEV_SEED=1 outside production.
func seedDemoData(ctx context.Context) error {
	created, err := seedAccount(ctx, 100123, "admin@euler.emoera.com", "种子管理员", "euler123")
	if err != nil {
		return err
	}
	if !created {
		slog.Info("DEV SEED account already present", "account_id", 100123)
		return nil
	}
	slog.Warn("DEV SEED account provisioned (EULER_DEV_SEED=1)", "email", "admin@euler.emoera.com", "account_id", 100123)
	if err := seedRAM(ctx, 100123); err != nil {
		return err
	}
	if err := seedAK(100123); err != nil {
		return err
	}
	return seedRAMRolesPolicies(ctx, 100123)
}

func main() {
	addr := flag.String("http", ":9101", "HTTP listen address")
	flag.Parse()

	slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// A production process must have a configured signing key: an ephemeral
	// one would differ per replica and vanish on restart, and "dev mode" must
	// never be what a forgotten variable silently selects.
	if jwtSigner.ephemeral && runmode.Production() {
		slog.Error("refusing to start: no JWT signing key configured", "set", jwtKeyFileEnv, "env", runmode.Env())
		os.Exit(1)
	}
	// Production also needs: the database (a restart must not forget every
	// account), the KMS root that seals MFA seeds and AK secrets (shared with
	// the verify container), the identity pepper, the internal token, and
	// svc-notify for password-reset mail.
	runmode.MustRequireSecrets(productionSettings()...)

	ctx := context.Background()
	b, err := backendFromEnv(ctx, runmode.Production())
	if err != nil {
		slog.Error("refusing to start: storage", "err", err)
		os.Exit(1)
	}
	installBackend(b)
	if err := configureIntegrations(); err != nil {
		slog.Error("refusing to start", "err", err)
		os.Exit(1)
	}

	// Demo data with a PUBLISHED password is opt-in (EULER_DEV_SEED=1) and never
	// created in production — it used to be seeded unconditionally.
	if !runmode.DevSeedEnabled() {
		slog.Info("no demo account seeded (set EULER_DEV_SEED=1 for local development)")
	} else if err := seedDemoData(ctx); err != nil {
		slog.Error("dev seed failed", "err", err)
		os.Exit(1)
	}
	// STS temporary-credential issuer (phase-3 D-3). Default TTL; a nil issuer
	// here would leave the endpoint 503ing, which is better than issuing
	// permanent credentials by accident.
	//
	// Tokens are sealed with EULER_STS_KEY, which svc-iam verify shares to
	// validate them. Production without a key leaves STS disabled (503)
	// rather than minting tokens nobody can check.
	if key, ok, err := sts.KeyFromEnv(); err != nil {
		slog.Error("refusing to start: invalid STS key", "err", err)
		os.Exit(1)
	} else if ok {
		stsIssuer, _ = sts.NewIssuer(sts.DefaultTTL, nil, key)
	} else if !runmode.Production() {
		devKey := make([]byte, 32)
		if _, err := rand.Read(devKey); err != nil {
			panic(err)
		}
		stsIssuer, _ = sts.NewIssuer(sts.DefaultTTL, nil, devKey)
		slog.Warn("no STS key configured — ephemeral key (dev only; tokens will not validate at the gateway)", "set", sts.KeyEnv)
	} else {
		slog.Warn("STS disabled: no key configured", "set", sts.KeyEnv)
	}

	mux := newMux()

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(mux)), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("svc-iam web-auth listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutdown signal received, draining")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
}

// newMux registers every web-auth route (main and the HTTP tests share it).
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("GET /api/auth/public-key", handlePublicKey)
	mux.HandleFunc("POST /api/auth/login", handleLogin)
	mux.HandleFunc("POST /api/auth/refresh", handleRefresh)
	mux.HandleFunc("POST /api/auth/logout", handleLogout)
	mux.HandleFunc("GET /api/auth/session", handleSession)
	mux.HandleFunc("POST /api/auth/register", handleRegister)
	// 注册邮箱验证码: no enumeration, rate-limited per email and per IP.
	mux.HandleFunc("POST /api/auth/register/send-code", handleRegisterSendCode)
	// 找回密码 (F2): no enumeration, rate-limited per email and per IP.
	mux.HandleFunc("POST /api/auth/forgot/send-code", handleForgotSendCode)
	mux.HandleFunc("POST /api/auth/forgot/reset", handleForgotReset)
	// MFA 强制登录 (M-2): step two of login + factor lifecycle.
	mux.HandleFunc("POST /api/auth/mfa/verify", handleMFALoginVerify)
	mux.HandleFunc("POST /api/mfa/bind", handleMFABind)
	mux.HandleFunc("POST /api/mfa/verify", handleMFABindVerify)
	mux.HandleFunc("POST /api/mfa/unbind", handleMFAUnbind)
	mux.HandleFunc("GET /api/realname/status", handleRealnameStatus)
	mux.HandleFunc("POST /api/realname/verify", handleRealnameVerify)
	mux.HandleFunc("GET /api/account/profile", handleAccountProfile)
	mux.HandleFunc("PUT /api/account/password", handleChangePassword)
	mux.HandleFunc("GET /api/ram/users", handleListRAMUsers)
	mux.HandleFunc("POST /api/ram/users", handleCreateRAMUser)
	mux.HandleFunc("DELETE /api/ram/users/{id}", handleDeleteRAMUser)
	mux.HandleFunc("POST /api/sts/assume-role", handleAssumeRole)
	mux.HandleFunc("GET /api/ak", handleListAccessKeys)
	mux.HandleFunc("POST /api/ak", handleCreateAccessKey)
	mux.HandleFunc("POST /api/ak/{akId}/disable", handleDisableAccessKey)
	mux.HandleFunc("POST /api/ak/{akId}/enable", handleEnableAccessKey)
	mux.HandleFunc("POST /api/ak/{akId}/rotate", handleRotateAccessKey)
	mux.HandleFunc("DELETE /api/ak/{akId}", handleDeleteAccessKey)
	// RAM roles, policies, and the 策略模拟器 (07§3.2/§3, M-7.6).
	mux.HandleFunc("GET /api/ram/roles", handleListRAMRoles)
	mux.HandleFunc("POST /api/ram/roles", handleCreateRAMRole)
	mux.HandleFunc("GET /api/ram/policies", handleListRAMPolicies)
	mux.HandleFunc("POST /api/ram/policies", handleCreateRAMPolicy)
	mux.HandleFunc("POST /api/ram/simulate", handleSimulate)
	// Gateway CheckAccess for eu-authorize (07§3.4). Internal only: guarded
	// by the shared internal token, and no public route maps /internal/*.
	mux.Handle("POST /internal/authz/check", internalOnly(http.HandlerFunc(handleAuthzCheck)))
	// Real-name status for other services (svc-order trial dedup, F4).
	mux.Handle("GET /internal/accounts/{id}/realname", internalOnly(http.HandlerFunc(handleInternalRealname)))
	return mux
}
