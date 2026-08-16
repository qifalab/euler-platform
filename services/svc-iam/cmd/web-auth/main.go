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
//	GET  /api/ak             — list the account's AccessKeys
//	POST /api/ak             — create an AccessKey (secret shown once)
//	POST /api/ak/{akId}/disable — disable an AccessKey
//	POST /api/ak/{akId}/enable  — re-enable a disabled AccessKey
//	POST /api/ak/{akId}/rotate  — mint a replacement, mark old one in grace window
//	DELETE /api/ak/{akId}       — permanently delete an AccessKey
//
// Token scheme (02§5.1):
//   - access_token: HS256-signed JWT, 15 min, returned in the JSON body (memory-only on the client)
//   - refresh_token: opaque crypto/rand string, 7 days, HttpOnly cookie on Domain=.starcloud.cn
//     (in dev the cookie domain is localhost; the frontend reads it via credentials:include)
//
// This is a stdlib-HTTP service (repo convention). The password hash is
// salted SHA-256 here for zero-dep portability; production uses argon2id with a
// 5-failure lockout (07§2.4) — the verify path and table shape are unchanged.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/starcloud/sc-platform/authz"
)

const (
	accessTTL  = 15 * time.Minute
	refreshTTL = 7 * 24 * time.Hour
	// maxBodyBytes caps every JSON request body (defense against oversized
	// payloads); http.MaxBytesReader enforces it per-handler.
	maxBodyBytes = 1 << 20 // 1 MiB
)

// hmacSecret is the HS256 signing key. Loaded from SC_JWT_SECRET; when unset
// (dev) a process-local random key is generated so the service stays usable,
// at the cost of invalidating tokens across restarts. Production must set
// SC_JWT_SECRET (loaded from KMS/secret manager — 07§5.3).
var hmacSecret = loadJWTSecret()

func loadJWTSecret() string {
	if s := os.Getenv("SC_JWT_SECRET"); s != "" {
		return s
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	slog.Warn("SC_JWT_SECRET not set — generated ephemeral JWT signing key (dev only); tokens will not survive restarts")
	return hex.EncodeToString(b)
}

// cookieSecure controls the Secure attribute of the refresh cookie.
// SC_COOKIE_SECURE=true/false overrides; when unset it defaults to true in
// production (SC_ENV=prod/production) and false in dev so plain-HTTP localhost
// keeps working.
var cookieSecure = loadCookieSecure()

func loadCookieSecure() bool {
	if v := os.Getenv("SC_COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
		slog.Warn("invalid SC_COOKIE_SECURE, falling back to env default", "value", v)
	}
	env := strings.ToLower(os.Getenv("SC_ENV"))
	return env == "prod" || env == "production"
}

// dummyPasswordHash is compared against when the account does not exist, so
// login latency does not reveal whether an email is registered.
var dummyPasswordHash = func() string {
	salt := randHex(16)
	return salt + "$" + sha256Hex(salt+randHex(16))
}()

// --- domain ---

type account struct {
	AccountID      int64
	AccountName    string // login (email)
	RealName       string
	PasswordHash   string // salted sha256: salt$hex
	Status         int    // 1 normal, 2 frozen, 3 closed
	RealNameStatus int    // 0 unverified, 1 individual verified, 2 enterprise verified (07§2.3)
	RealNameType   string // "individual" | "enterprise" | ""
	IDCardNo       string // masked after verification (real-name doc number)
	EnterpriseName string
	CreatedAt      time.Time // provisioning time, surfaced in profile (03§9.3)
	// TokenVersion is bumped whenever credentials change (e.g. password
	// change). It is embedded in issued access tokens ("tv" claim) and
	// compared on verification, so older tokens are rejected immediately.
	TokenVersion int
	// phoneBound/mfaEnabled are not yet tracked in phase-1; profile reports false
	// until the binding flows land (07§2.5). Fields kept for forward-compat.
}

type session struct {
	account      account
	refreshToken string
	refreshExp   time.Time
	accessExp    time.Time
}

var (
	mu       sync.RWMutex
	accounts = map[string]account{}  // accountName → account
	byID     = map[int64]account{}   // accountID → account
	sessions = map[string]*session{} // refreshToken → session

	// RAM sub-users keyed by accountID → username → ramUser. Phase-1 is
	// in-memory (02§5.3); prod persists to the iam_user table (03 DDL).
	ramUsers = map[int64]map[string]ramUser{}
	// AccessKeys keyed by accountID → akID. Phase-1 in-memory.
	accessKeys = map[int64][]accessKey{}

	ramSeq int64 // RAM sub-user id counter (prod uses snowflake — 00 附录A)
	akSeq  int64 // AccessKey id counter

	// RAM roles keyed by accountID → roleName → ramRole (07§3.2). Phase-1
	// in-memory; prod persists to iam_role / iam_role_policy.
	ramRoles = map[int64]map[string]ramRole{}
	// RAM policies keyed by accountID → policyName → storedPolicy (07§3.2). A
	// policy document is JSON ({Version,Statement}); the name is the binding
	// key (the doc has no Name field, matching pkg-go/authz.Policy).
	ramPolicies = map[int64]map[string]storedPolicy{}

	roleSeq int64
	polSeq  int64
)

// ramUser is a RAM sub-account (07§2.6). Status mirrors the account status enum.
type ramUser struct {
	ID          int64
	AccountID   int64
	Username    string // unique within the account
	DisplayName string
	Remark      string
	Status      int // 1 enabled, 2 disabled, 3 deleted (soft)
	CreatedAt   time.Time
}

// accessKey is a long-lived credential pair (07§2.7). The secret is shown ONCE
// at creation and never stored in plaintext — only its salted hash is retained
// for later verification (prod never returns the secret again).
type accessKey struct {
	AKID          string // "SC"+14 chars, masked for display after creation
	SecretHash    string // salted sha256 of the plaintext secret
	Status        int    // 1 enabled, 2 disabled
	CreatedAt     time.Time
	LastUsed      time.Time
	RotationGrace bool // true while this AK is the outgoing side of a rotation (07§2.2)
}

func nextRAMID() int64    { ramSeq++; return ramSeq }
func nextAKIDSeq() int64  { akSeq++; return akSeq }
func nextRoleID() int64   { roleSeq++; return roleSeq }
func nextPolicyID() int64 { polSeq++; return polSeq }

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

// seedAccount provisions a real account with a real salted password hash so the
// login flow is genuine (not a mock that always succeeds). Matches the
// console-bff seed account 100123.
func seedAccount(id int64, name, realName, password string) {
	salt := randHex(16)
	h := sha256Hex(salt + password)
	a := account{
		AccountID: id, AccountName: name, RealName: realName,
		PasswordHash: salt + "$" + h, Status: 1,
		CreatedAt: time.Now(),
	}
	accounts[name] = a
	byID[id] = a
}

// seedRAM provisions two demo RAM sub-users for the seed account so the console
// RAM page has real rows on first load (phase-1 dev; prod reads from iam_user).
func seedRAM(accountID int64) {
	now := time.Now()
	ramUsers[accountID] = map[string]ramUser{
		"ops-admin": {ID: nextRAMID(), AccountID: accountID, Username: "ops-admin", DisplayName: "运维管理员", Remark: "生产运维", Status: 1, CreatedAt: now},
		"dev-ro":    {ID: nextRAMID(), AccountID: accountID, Username: "dev-ro", DisplayName: "开发只读", Remark: "只读开发", Status: 1, CreatedAt: now},
	}
}

// seedAK provisions one demo AccessKey for the seed account so the AK page has a
// real row on first load. The secret here is never shown again (only the hash is
// kept), matching prod behavior (07§2.7).
func seedAK(accountID int64) {
	now := time.Now()
	akID := "SC" + randMasked(14)
	accessKeys[accountID] = []accessKey{{
		AKID: akID, SecretHash: randHex(16) + "$" + sha256Hex(randHex(16)+"sc-seed-secret"),
		Status: 1, CreatedAt: now, LastUsed: now.Add(-2 * time.Hour),
	}}
}

// seedRAMRolesPolicies provisions the three canonical roles and two system
// policies for the seed account (07§3.2 — ScEcsFullAccess / ScReadOnlyAccess
// system-policy naming). The policies are real authz.Policy documents the
// simulator evaluates, so the 策略模拟器 has real rows on first load.
func seedRAMRolesPolicies(accountID int64) {
	now := time.Now()
	fullAccess := `{"Version":"1","Statement":[{"Effect":"Allow","Action":"scecs:*","Resource":"*"}]}`
	readOnly := `{"Version":"1","Statement":[{"Effect":"Allow","Action":["scecs:Describe*","scecs:List*"],"Resource":"*"}]}`
	mustParseStored := func(name, typ, doc string) storedPolicy {
		p, err := authz.ParsePolicy([]byte(doc))
		if err != nil {
			panic("seedRAMRolesPolicies: " + err.Error())
		}
		return storedPolicy{ID: nextPolicyID(), AccountID: accountID, Name: name, Type: typ, Document: doc, Parsed: p, CreatedAt: now}
	}
	ramPolicies[accountID] = map[string]storedPolicy{
		"ScEcsFullAccess":  mustParseStored("ScEcsFullAccess", "system", fullAccess),
		"ScReadOnlyAccess": mustParseStored("ScReadOnlyAccess", "system", readOnly),
	}
	ramRoles[accountID] = map[string]ramRole{
		"Admin":    {ID: nextRoleID(), AccountID: accountID, Name: "Admin", Description: "全部权限", Policies: []string{"ScEcsFullAccess"}, Status: 1, CreatedAt: now},
		"Operator": {ID: nextRoleID(), AccountID: accountID, Name: "Operator", Description: "运维操作", Policies: []string{"ScEcsFullAccess"}, Status: 1, CreatedAt: now},
		"ReadOnly": {ID: nextRoleID(), AccountID: accountID, Name: "ReadOnly", Description: "只读", Policies: []string{"ScReadOnlyAccess"}, Status: 1, CreatedAt: now},
	}
}

// randMasked returns a random uppercase alphanumeric string of length n, used to
// build a display akId like "SC"+14 chars. It is NOT the secret.
func randMasked(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	r := make([]byte, n)
	if _, err := rand.Read(r); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(r[i])%len(alphabet)]
	}
	return string(b)
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

// issueAccessToken signs a minimal HS256 JWT carrying sub/name/exp plus the
// account's TokenVersion ("tv") so credential changes invalidate old tokens.
func issueAccessToken(a account, now time.Time) string {
	header := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := fmt.Sprintf(`{"sub":"%d","name":"%s","real_name":"%s","tv":%d,"iat":%d,"exp":%d}`,
		a.AccountID, a.AccountName, a.RealName, a.TokenVersion, now.Unix(), now.Add(accessTTL).Unix())
	payloadB64 := b64url([]byte(payload))
	signingInput := header + "." + payloadB64
	mac := hmac.New(sha256.New, []byte(hmacSecret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + b64url(mac.Sum(nil))
}

// verifyAccessToken returns the account ID and token version embedded in a
// valid JWT, or an error.
func verifyAccessToken(token string, now time.Time) (int64, int, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, 0, errors.New("Auth.MalformedToken")
	}
	mac := hmac.New(sha256.New, []byte(hmacSecret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal([]byte(parts[2]), []byte(b64url(mac.Sum(nil)))) {
		return 0, 0, errors.New("Auth.InvalidSignature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, 0, errors.New("Auth.MalformedToken")
	}
	var p struct {
		Sub string `json:"sub"`
		Tv  int    `json:"tv"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
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
	reqID := w.Header().Get("X-Sc-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{RequestId: reqID, Code: code, Data: data})
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	reqID := w.Header().Get("X-Sc-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{RequestId: reqID, Code: code, Message: msg})
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
// Domain is .starcloud.cn (root, shared across console/billing/ticket); in dev
// the browser scopes it to localhost.
func setRefreshCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: "sc_refresh", Value: token, Expires: exp, HttpOnly: true,
		Secure: cookieSecure, SameSite: http.SameSiteLaxMode, Path: "/api/auth",
	})
}

func clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "sc_refresh", Value: "", Expires: time.Unix(0, 0), HttpOnly: true, Path: "/api/auth"})
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
	mu.RLock()
	a, ok := accounts[req.Email]
	mu.RUnlock()
	// Constant-time-ish: always perform one hash comparison even when the
	// account does not exist, so response timing does not reveal whether an
	// email is registered.
	if !ok {
		_ = verifyPassword(dummyPasswordHash, req.Password)
		writeErr(w, 401, "Auth.InvalidCredentials", "邮箱或密码错误")
		return
	}
	if !verifyPassword(a.PasswordHash, req.Password) || a.Status != 1 {
		writeErr(w, 401, "Auth.InvalidCredentials", "邮箱或密码错误")
		return
	}
	now := time.Now()
	refresh := randHex(32)
	mu.Lock()
	sessions[refresh] = &session{
		account: a, refreshToken: refresh,
		refreshExp: now.Add(refreshTTL), accessExp: now.Add(accessTTL),
	}
	mu.Unlock()
	setRefreshCookie(w, refresh, now.Add(refreshTTL))
	writeJSON(w, 200, "OK", map[string]any{
		"accessToken": issueAccessToken(a, now),
		"user":        map[string]any{"id": a.AccountID, "name": a.AccountName, "realName": a.RealName},
	})
}

func verifyPassword(stored, input string) bool {
	parts := strings.SplitN(stored, "$", 2)
	if len(parts) != 2 {
		return false
	}
	return hmac.Equal([]byte(parts[1]), []byte(sha256Hex(parts[0]+input)))
}

// nextAccountID issues a monotonically increasing account id (prod uses a
// snowflake generator — 00 附录A; phase-1 dev counter is sufficient).
var accountSeq int64 = 100123

func nextAccountID() int64 {
	accountSeq++
	return accountSeq
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
	mu.RLock()
	a := byID[id]
	mu.RUnlock()
	if a.AccountID == 0 {
		return account{}, errors.New("Auth.AccountNotFound")
	}
	// Token issued before a credential change (password change bumps
	// TokenVersion) is no longer accepted.
	if tv != a.TokenVersion {
		return account{}, errors.New("Auth.TokenRevoked")
	}
	return a, nil
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
type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || len(req.Password) < 8 {
		writeErr(w, 400, "Common.InvalidParameter", "邮箱必填,密码至少 8 位")
		return
	}
	mu.Lock()
	if _, exists := accounts[req.Email]; exists {
		mu.Unlock()
		writeErr(w, 409, "Auth.EmailExists", "该邮箱已注册")
		return
	}
	id := nextAccountID()
	salt := randHex(16)
	a := account{
		AccountID: id, AccountName: req.Email, RealName: req.Email,
		PasswordHash: salt + "$" + sha256Hex(salt+req.Password),
		Status:       1, RealNameStatus: 0, CreatedAt: time.Now(),
	}
	accounts[req.Email] = a
	byID[id] = a
	mu.Unlock()
	writeJSON(w, 201, "OK", map[string]any{"id": id, "email": req.Email, "realNameStatus": 0})
}

// handleRealnameStatus returns the current real-name verification state.
func handleRealnameStatus(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
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
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	var req realnameRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Validate before taking the lock (no shared state involved).
	switch req.Type {
	case "individual":
		if req.Name == "" || len(req.IDCardNo) < 15 {
			writeErr(w, 400, "Common.InvalidParameter", "请填写真实姓名与身份证号")
			return
		}
	case "enterprise":
		if req.EnterpriseName == "" || len(req.CreditCode) < 15 {
			writeErr(w, 400, "Common.InvalidParameter", "请填写企业名称与统一社会信用代码")
			return
		}
	default:
		writeErr(w, 400, "Common.InvalidParameter", "实名类型无效")
		return
	}
	// Read-modify-write of the account happens entirely inside one critical
	// section so a concurrent update (e.g. password change) is not lost.
	mu.Lock()
	cur, ok := byID[a.AccountID]
	if !ok {
		mu.Unlock()
		writeErr(w, 401, "Auth.AccountNotFound", "账号不存在")
		return
	}
	status := 0
	switch req.Type {
	case "individual":
		status = 1
		cur.RealName = req.Name
		cur.IDCardNo = req.IDCardNo
	case "enterprise":
		status = 2
		cur.EnterpriseName = req.EnterpriseName
		cur.IDCardNo = req.CreditCode
	}
	cur.RealNameType = req.Type
	cur.RealNameStatus = status
	accounts[cur.AccountName] = cur
	byID[cur.AccountID] = cur
	masked := maskIDCard(cur.IDCardNo)
	mu.Unlock()
	writeJSON(w, 200, "OK", map[string]any{
		"status": status, "type": req.Type, "idCard": masked,
	})
}

// --- account profile & password (03§9.3) ---

// handleAccountProfile returns the authenticated account's profile snapshot.
// Mirrors the shape the console Profile page renders (realName, realNameStatus,
// createdAt, email, phoneBound, mfaEnabled).
func handleAccountProfile(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	writeJSON(w, 200, "OK", map[string]any{
		"id":             a.AccountID,
		"name":           a.AccountName,
		"realName":       a.RealName,
		"realNameStatus": a.RealNameStatus,
		"createdAt":      a.CreatedAt.UTC().Format(time.RFC3339),
		"email":          a.AccountName,
		"phoneBound":     false, // phase-1: no phone-binding flow yet (07§2.5)
		"mfaEnabled":     false, // phase-1: MFA not yet enforced (07§2.4)
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
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	var req changePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.NewPassword) < 8 {
		writeErr(w, 400, "Common.InvalidParameter", "新密码至少 8 位")
		return
	}
	// Verify-old + install-new is one critical section: the old-password check
	// runs against the freshest state and a concurrent update is not lost.
	mu.Lock()
	cur, ok := byID[a.AccountID]
	if !ok {
		mu.Unlock()
		writeErr(w, 401, "Auth.AccountNotFound", "账号不存在")
		return
	}
	if !verifyPassword(cur.PasswordHash, req.OldPassword) {
		mu.Unlock()
		writeErr(w, 400, "Auth.OldPasswordMismatch", "原密码不正确")
		return
	}
	salt := randHex(16)
	cur.PasswordHash = salt + "$" + sha256Hex(salt+req.NewPassword)
	// Invalidate everything issued before the change: bump TokenVersion so
	// outstanding access tokens fail verification, and revoke the account's
	// refresh sessions so they cannot mint new tokens.
	cur.TokenVersion++
	accounts[cur.AccountName] = cur
	byID[cur.AccountID] = cur
	for tok, s := range sessions {
		if s.account.AccountID == cur.AccountID {
			delete(sessions, tok)
		}
	}
	mu.Unlock()
	writeJSON(w, 200, "OK", map[string]any{})
}

// --- RAM sub-users (07§2.6) ---

// handleListRAMUsers returns the (non-deleted) sub-users for the account.
func handleListRAMUsers(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	mu.RLock()
	users := ramUsers[a.AccountID]
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		if u.Status == 3 { // skip soft-deleted
			continue
		}
		out = append(out, map[string]any{
			"id":          u.ID,
			"username":    u.Username,
			"displayName": u.DisplayName,
			"remark":      u.Remark,
			"status":      u.Status,
			"createdAt":   u.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	mu.RUnlock()
	writeJSON(w, 200, "OK", map[string]any{"users": out, "total": len(out)})
}

type createRAMUserRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Remark      string `json:"remark"`
}

// handleCreateRAMUser creates a sub-user. Idempotent on accountID+username:
// a duplicate returns 409 Auth.RAMUserExists (07§2.6).
func handleCreateRAMUser(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
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
	mu.Lock()
	bucket := ramUsers[a.AccountID]
	if bucket != nil {
		if existing, ok := bucket[req.Username]; ok && existing.Status != 3 {
			mu.Unlock()
			writeErr(w, 409, "Auth.RAMUserExists", "该子用户已存在")
			return
		}
	} else {
		bucket = map[string]ramUser{}
		ramUsers[a.AccountID] = bucket
	}
	u := ramUser{
		ID: nextRAMID(), AccountID: a.AccountID, Username: req.Username,
		DisplayName: req.DisplayName, Remark: req.Remark,
		Status: 1, CreatedAt: time.Now(),
	}
	bucket[req.Username] = u
	mu.Unlock()
	writeJSON(w, 201, "OK", map[string]any{
		"id": u.ID, "username": u.Username, "displayName": u.DisplayName,
		"remark": u.Remark, "status": u.Status,
		"createdAt": u.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// handleDeleteRAMUser soft-deletes a sub-user (status=3). A missing user
// returns 404; a deleted one returns 409 to keep DELETE idempotent-ish.
func handleDeleteRAMUser(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, 400, "Common.InvalidParameter", "无效的子用户 ID")
		return
	}
	mu.Lock()
	bucket := ramUsers[a.AccountID]
	var found bool
	for name, u := range bucket {
		if u.ID == id {
			found = true
			if u.Status == 3 {
				mu.Unlock()
				writeErr(w, 409, "Auth.RAMUserDeleted", "该子用户已删除")
				return
			}
			u.Status = 3
			bucket[name] = u
			break
		}
	}
	mu.Unlock()
	if !found {
		writeErr(w, 404, "Auth.RAMUserNotFound", "子用户不存在")
		return
	}
	writeJSON(w, 200, "OK", map[string]any{})
}

// --- AccessKeys (07§2.7) ---

// handleListAccessKeys returns the account's AKs. The secret is never returned.
func handleListAccessKeys(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	mu.RLock()
	aks := accessKeys[a.AccountID]
	out := make([]map[string]any, 0, len(aks))
	for _, k := range aks {
		out = append(out, map[string]any{
			"akId":          k.AKID,
			"status":        k.Status,
			"createdAt":     k.CreatedAt.UTC().Format(time.RFC3339),
			"lastUsed":      k.LastUsed.UTC().Format(time.RFC3339),
			"rotationGrace": k.RotationGrace,
		})
	}
	mu.RUnlock()
	writeJSON(w, 200, "OK", map[string]any{"accessKeys": out, "total": len(out)})
}

// handleCreateAccessKey creates an AK. Max 2 per account: a third returns 409
// Auth.AKLimitExceeded. The plaintext secretKey is returned ONCE here; prod
// never shows it again and stores only the salted hash (07§2.7).
func handleCreateAccessKey(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	mu.Lock()
	enabled := countEnabled(accessKeys[a.AccountID])
	if enabled >= 2 {
		mu.Unlock()
		writeErr(w, 409, "Auth.AKLimitExceeded", "每个账号最多 2 个 AccessKey")
		return
	}
	akID, secretPlain, k := issueAccessKey()
	accessKeys[a.AccountID] = append(accessKeys[a.AccountID], k)
	mu.Unlock()
	// WARNING: secretKey is shown exactly once — prod stores only the hash and
	// never re-displays the plaintext. The client must persist it now.
	writeJSON(w, 201, "OK", map[string]any{
		"akId":      akID,
		"secretKey": secretPlain,
		"createdAt": k.CreatedAt.UTC().Format(time.RFC3339),
		"status":    k.Status,
	})
}

// issueAccessKey mints a fresh AK (id + one-time plaintext secret + stored
// record). Caller holds mu. Mirrors the create seed logic so create and rotate
// produce identical credentials.
func issueAccessKey() (akID, secretPlain string, k accessKey) {
	_ = nextAKIDSeq()
	akID = "SC" + randMasked(14)
	secretPlain = randHex(20)
	salt := randHex(16)
	now := time.Now()
	k = accessKey{
		AKID: akID, SecretHash: salt + "$" + sha256Hex(salt+secretPlain),
		Status: 1, CreatedAt: now, LastUsed: now,
	}
	return akID, secretPlain, k
}

// countEnabled returns how many of the account's AKs are currently enabled.
func countEnabled(aks []accessKey) int {
	n := 0
	for _, k := range aks {
		if k.Status == 1 {
			n++
		}
	}
	return n
}

// findAKIndex returns the index of the AK with the given id for the account,
// or -1. Caller holds the appropriate lock.
func findAKIndex(aks []accessKey, akID string) int {
	for i, k := range aks {
		if k.AKID == akID {
			return i
		}
	}
	return -1
}

// handleDisableAccessKey disables an AK. A disabled AK cannot sign requests.
// Disabling an already-disabled AK is a no-op success (idempotent).
func handleDisableAccessKey(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	akID := r.PathValue("akId")
	if akID == "" {
		writeErr(w, 400, "Common.InvalidParameter", "缺少 akId")
		return
	}
	mu.Lock()
	aks := accessKeys[a.AccountID]
	idx := findAKIndex(aks, akID)
	if idx < 0 {
		mu.Unlock()
		writeErr(w, 404, "Auth.AKNotFound", "AccessKey 不存在")
		return
	}
	aks[idx].Status = 2
	mu.Unlock()
	writeJSON(w, 200, "OK", map[string]any{"akId": akID, "status": 2})
}

// handleEnableAccessKey re-enables a disabled AK. Subject to the 2-enabled limit.
func handleEnableAccessKey(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	akID := r.PathValue("akId")
	if akID == "" {
		writeErr(w, 400, "Common.InvalidParameter", "缺少 akId")
		return
	}
	mu.Lock()
	aks := accessKeys[a.AccountID]
	idx := findAKIndex(aks, akID)
	if idx < 0 {
		mu.Unlock()
		writeErr(w, 404, "Auth.AKNotFound", "AccessKey 不存在")
		return
	}
	if aks[idx].Status == 1 {
		mu.Unlock()
		writeJSON(w, 200, "OK", map[string]any{"akId": akID, "status": 1})
		return
	}
	// Enabling would add a 3rd active AK if two are already enabled.
	otherEnabled := 0
	for i, k := range aks {
		if i != idx && k.Status == 1 {
			otherEnabled++
		}
	}
	if otherEnabled >= 2 {
		mu.Unlock()
		writeErr(w, 409, "Auth.AKLimitExceeded", "每个账号最多 2 个启用的 AccessKey")
		return
	}
	aks[idx].Status = 1
	mu.Unlock()
	writeJSON(w, 200, "OK", map[string]any{"akId": akID, "status": 1})
}

// handleRotateAccessKey mints a replacement AK while keeping the old one
// active through a grace window (07§2.2). The old AK is marked
// RotationGrace so the owner knows to migrate and delete it. The new
// secretKey is returned ONCE. Blocked at 2 total AKs (no slot for the replacement).
func handleRotateAccessKey(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	akID := r.PathValue("akId")
	if akID == "" {
		writeErr(w, 400, "Common.InvalidParameter", "缺少 akId")
		return
	}
	mu.Lock()
	aks := accessKeys[a.AccountID]
	idx := findAKIndex(aks, akID)
	if idx < 0 {
		mu.Unlock()
		writeErr(w, 404, "Auth.AKNotFound", "AccessKey 不存在")
		return
	}
	if countEnabled(aks) >= 2 {
		mu.Unlock()
		writeErr(w, 409, "Auth.AKLimitExceeded", "已达 2 个启用 AK 上限,请先禁用或删除一个再轮换")
		return
	}
	// Mint the replacement and mark the outgoing AK for the grace window.
	newID, secretPlain, k := issueAccessKey()
	aks[idx].RotationGrace = true
	accessKeys[a.AccountID] = append(aks, k)
	mu.Unlock()
	writeJSON(w, 201, "OK", map[string]any{
		"akId":      newID,
		"secretKey": secretPlain,
		"createdAt": k.CreatedAt.UTC().Format(time.RFC3339),
		"status":    k.Status,
		"oldAkId":   akID,
	})
}

// handleDeleteAccessKey permanently removes an AK. Allowed during the rotation
// grace window — deleting the outgoing AK after migration is the intended path.
func handleDeleteAccessKey(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	akID := r.PathValue("akId")
	if akID == "" {
		writeErr(w, 400, "Common.InvalidParameter", "缺少 akId")
		return
	}
	mu.Lock()
	aks := accessKeys[a.AccountID]
	idx := findAKIndex(aks, akID)
	if idx < 0 {
		mu.Unlock()
		writeErr(w, 404, "Auth.AKNotFound", "AccessKey 不存在")
		return
	}
	accessKeys[a.AccountID] = append(aks[:idx], aks[idx+1:]...)
	mu.Unlock()
	writeJSON(w, 200, "OK", map[string]any{"akId": akID})
}

// --- RAM roles & policies (07§3.2) + 策略模拟器 (07§3) ---

// handleListRAMRoles returns the account's roles with their attached policy names.
func handleListRAMRoles(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	mu.RLock()
	roles := ramRoles[a.AccountID]
	out := make([]map[string]any, 0, len(roles))
	for _, role := range roles {
		out = append(out, map[string]any{
			"id":          role.ID,
			"name":        role.Name,
			"description": role.Description,
			"policies":    role.Policies,
			"status":      role.Status,
			"createdAt":   role.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	mu.RUnlock()
	writeJSON(w, 200, "OK", map[string]any{"roles": out, "total": len(out)})
}

type createRoleRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Policies    []string `json:"policies"`
}

// handleCreateRAMRole creates a role. Idempotent on accountID+name: a duplicate
// returns 409 Auth.RAMRoleExists.
func handleCreateRAMRole(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
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
	mu.Lock()
	bucket := ramRoles[a.AccountID]
	if bucket == nil {
		bucket = map[string]ramRole{}
		ramRoles[a.AccountID] = bucket
	}
	if _, exists := bucket[req.Name]; exists {
		mu.Unlock()
		writeErr(w, 409, "Auth.RAMRoleExists", "该角色已存在")
		return
	}
	role := ramRole{
		ID: nextRoleID(), AccountID: a.AccountID, Name: req.Name,
		Description: req.Description, Policies: req.Policies, Status: 1, CreatedAt: time.Now(),
	}
	bucket[req.Name] = role
	mu.Unlock()
	writeJSON(w, 201, "OK", map[string]any{
		"id": role.ID, "name": role.Name, "description": role.Description,
		"policies": role.Policies, "status": role.Status,
		"createdAt": role.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// handleListRAMPolicies returns the account's policies (system + custom).
func handleListRAMPolicies(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
		return
	}
	mu.RLock()
	pols := ramPolicies[a.AccountID]
	out := make([]map[string]any, 0, len(pols))
	for _, p := range pols {
		out = append(out, map[string]any{
			"id":        p.ID,
			"name":      p.Name,
			"type":      p.Type,
			"document":  p.Document,
			"createdAt": p.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	mu.RUnlock()
	writeJSON(w, 200, "OK", map[string]any{"policies": out, "total": len(out)})
}

type createPolicyRequest struct {
	Name     string `json:"name"`
	Document string `json:"document"`
}

// handleCreateRAMPolicy creates a custom policy. The document is validated by
// authz.ParsePolicy before storage, so a malformed policy never persists — the
// same engine the simulator and gateway use decides whether the JSON is valid.
func handleCreateRAMPolicy(w http.ResponseWriter, r *http.Request) {
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
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
	parsed, err := authz.ParsePolicy([]byte(req.Document))
	if err != nil {
		writeErr(w, 400, "Auth.InvalidPolicyDocument", err.Error())
		return
	}
	mu.Lock()
	bucket := ramPolicies[a.AccountID]
	if bucket == nil {
		bucket = map[string]storedPolicy{}
		ramPolicies[a.AccountID] = bucket
	}
	if _, exists := bucket[req.Name]; exists {
		mu.Unlock()
		writeErr(w, 409, "Auth.PolicyExists", "该策略已存在")
		return
	}
	sp := storedPolicy{
		ID: nextPolicyID(), AccountID: a.AccountID, Name: req.Name, Type: "custom",
		Document: req.Document, Parsed: parsed, CreatedAt: time.Now(),
	}
	bucket[req.Name] = sp
	mu.Unlock()
	writeJSON(w, 201, "OK", map[string]any{
		"id": sp.ID, "name": sp.Name, "type": sp.Type, "document": sp.Document,
		"createdAt": sp.CreatedAt.UTC().Format(time.RFC3339),
	})
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
	a, err := requireAuth(r)
	if err != nil {
		writeErr(w, 401, err.Error(), "未认证")
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
	mu.RLock()
	bucket := ramPolicies[a.AccountID]
	named := make([]authz.NamedPolicy, 0, len(req.Policies))
	unknown := make([]string, 0)
	for _, name := range req.Policies {
		sp, ok := bucket[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		named = append(named, authz.NamedPolicy{Name: name, Policy: sp.Parsed})
	}
	mu.RUnlock()
	if len(unknown) > 0 {
		writeErr(w, 404, "Auth.PolicyNotFound", "策略不存在: "+strings.Join(unknown, ", "))
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
	c, err := r.Cookie("sc_refresh")
	if err != nil {
		writeErr(w, 401, "Auth.NoSession", "无活跃会话")
		return
	}
	mu.Lock()
	s, ok := sessions[c.Value]
	if !ok || time.Now().After(s.refreshExp) {
		delete(sessions, c.Value)
		mu.Unlock()
		writeErr(w, 401, "Auth.SessionExpired", "会话已过期,请重新登录")
		return
	}
	// Re-check the live account: a disabled/closed (or deleted) account must
	// not be able to refresh into a fresh access token.
	cur, exists := byID[s.account.AccountID]
	if !exists || cur.Status != 1 {
		delete(sessions, c.Value)
		mu.Unlock()
		writeErr(w, 401, "Auth.AccountUnusable", "账号不可用,请联系管理员")
		return
	}
	// Rotate: revoke the old refresh, issue a new one (02§5.1). The session
	// carries the fresh account snapshot so the new token reflects current state.
	delete(sessions, c.Value)
	newRefresh := randHex(32)
	now := time.Now()
	s.account = cur
	s.refreshToken = newRefresh
	s.refreshExp = now.Add(refreshTTL)
	s.accessExp = now.Add(accessTTL)
	sessions[newRefresh] = s
	mu.Unlock()
	setRefreshCookie(w, newRefresh, s.refreshExp)
	writeJSON(w, 200, "OK", map[string]any{"accessToken": issueAccessToken(cur, now)})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("sc_refresh")
	if err == nil {
		mu.Lock()
		delete(sessions, c.Value)
		mu.Unlock()
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
	mu.RLock()
	a := byID[id]
	mu.RUnlock()
	if a.AccountID == 0 {
		writeErr(w, 401, "Auth.AccountNotFound", "账号不存在")
		return
	}
	if tv != a.TokenVersion {
		writeErr(w, 401, "Auth.TokenRevoked", "token 已失效,请重新登录")
		return
	}
	writeJSON(w, 200, "OK", map[string]any{
		"user":           map[string]any{"id": a.AccountID, "name": a.AccountName, "realName": a.RealName},
		"realNameStatus": a.RealNameStatus,
		"permissions":    map[string]any{"actions": []string{"scecs:Read", "scecs:Start", "scoss:Read", "scrds:Read", "scvpc:Read", "scmon:Read"}},
	})
}

// --- middleware (matches the shared scaffold pattern) ---

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Sc-TraceId")
		if id == "" {
			id = "webauth-" + randHex(8)
		}
		w.Header().Set("X-Sc-TraceId", id)
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

func main() {
	addr := flag.String("http", ":9101", "HTTP listen address")
	flag.Parse()

	slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Real seeded account with a real salted password hash (not a mock that
	// always returns success). Credentials are logged once for dev convenience.
	seedAccount(100123, "admin@starcloud.cn", "种子管理员", "starcloud123")
	slog.Warn("DEV SEED account provisioned", "email", "admin@starcloud.cn",
		"account_id", 100123, "note", "salted SHA-256; prod uses argon2id")
	// Seed demo RAM sub-users + one AccessKey + RAM roles/policies so the
	// console pages render real rows on first load (phase-1 dev only; prod
	// reads from iam_user / iam_ak / iam_role / iam_role_policy).
	seedRAM(100123)
	seedAK(100123)
	seedRAMRolesPolicies(100123)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/auth/login", handleLogin)
	mux.HandleFunc("POST /api/auth/refresh", handleRefresh)
	mux.HandleFunc("POST /api/auth/logout", handleLogout)
	mux.HandleFunc("GET /api/auth/session", handleSession)
	mux.HandleFunc("POST /api/auth/register", handleRegister)
	mux.HandleFunc("GET /api/realname/status", handleRealnameStatus)
	mux.HandleFunc("POST /api/realname/verify", handleRealnameVerify)
	mux.HandleFunc("GET /api/account/profile", handleAccountProfile)
	mux.HandleFunc("PUT /api/account/password", handleChangePassword)
	mux.HandleFunc("GET /api/ram/users", handleListRAMUsers)
	mux.HandleFunc("POST /api/ram/users", handleCreateRAMUser)
	mux.HandleFunc("DELETE /api/ram/users/{id}", handleDeleteRAMUser)
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
