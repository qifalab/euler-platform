package main

// Persistence port for web-auth (F1).
//
// Everything web-auth knows about accounts, refresh sessions, RAM users /
// roles / policies, MFA seeds, real-name results and password-reset codes goes
// through authStore. Two implementations satisfy it with the same observable
// behaviour (store_contract_test.go runs one suite against both):
//
//   - memStore: process memory, the default — dev servers and `go test` need
//     no database (pkg-go/storage: persistence is opt-in);
//   - sqlStore: account_db (EULER_DB_DSN), so a restart forgets nothing and
//     every replica, the verify container and /internal/authz/check read the
//     same rows.
//
// AccessKeys are deliberately NOT part of this port: they live in the shared
// pkg-go/accesskey store (memory or account_db.access_key) that cmd/verify
// reads, sealed with the same pkg-go/kms keys, so a key created in the console
// is the key the gateway verifies.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qifalab/euler-platform/authz"
)

// Store errors. Handlers map them onto wire codes; anything else a store
// returns is an infrastructure failure (500, detail in the log).
var (
	errNotFound       = errors.New("store: not found")
	errExists         = errors.New("store: already exists")
	errAlreadyDeleted = errors.New("store: already deleted")
	// errCodeInvalid covers a wrong, expired or never-issued reset code —
	// deliberately one error, so the API cannot tell them apart.
	errCodeInvalid  = errors.New("store: invalid or expired code")
	errCodeAttempts = errors.New("store: too many code attempts")
)

// policyNotFoundError names the policies an attach request referenced that
// are not visible to the account (neither system nor its own custom ones).
type policyNotFoundError struct{ names []string }

func (e *policyNotFoundError) Error() string {
	return "store: policy not found: " + strings.Join(e.names, ", ")
}

// Refresh-session revocation reasons (auth_refresh_session.revoke_reason).
const (
	revokeRotated  = "rotated"  // replaced by the next token of the chain
	revokeLogout   = "logout"   // the user logged out
	revokePassword = "password" // password changed
	revokeReset    = "reset"    // password reset through the forgot flow
	revokeReuse    = "reuse"    // a rotated-away token came back: chain revoked
	revokeAccount  = "account"  // the account is no longer usable
)

// refreshReuseGrace is how long after a rotation the superseded token may
// still be presented without being treated as stolen: two console tabs
// refreshing at the same moment both present the same cookie, and the loser
// must just get a 401, not log the user out everywhere. Outside the grace a
// rotated-away token is a replay and revokes the whole chain (OAuth 2.0
// refresh-token rotation, RFC 9700 §4.14.2).
var refreshReuseGrace = 30 * time.Second

// refreshSession is one refresh token. Only its SHA-256 is stored: a leaked
// session table does not hand out sessions. FamilyID links every token minted
// by rotation from one login (the rotation chain); ParentHash / ReplacedBy are
// the chain's edges. TokenVersion is the account's credential version when the
// token was minted: a password change or reset bumps the account's version,
// which voids every older session atomically with the credential change (the
// explicit revocation that follows is bookkeeping).
type refreshSession struct {
	TokenHash    string
	AccountID    int64
	TokenVersion int
	FamilyID     string
	ParentHash   string
	ReplacedBy   string
	ExpiresAt    time.Time
	RevokedAt    time.Time
	RevokeReason string
	CreatedAt    time.Time
}

// live reports whether the token can still be exchanged.
func (s refreshSession) live(now time.Time) bool {
	return s.RevokedAt.IsZero() && now.Before(s.ExpiresAt)
}

// hashToken is the stored form of an opaque refresh token.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newFamilyID starts a rotation chain.
func newFamilyID() string { return randHex(16) }

// maxResetAttempts bounds the guesses against one password-reset code.
const maxResetAttempts = 5

// resetCode is the pending password-reset code of one account (at most one:
// issuing a new code replaces the old). Only a keyed hash of the code is kept.
type resetCode struct {
	AccountID int64
	CodeHash  string
	Attempts  int
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Email-verification code purposes (email_verification_code.purpose). A code
// is checked only against the purpose it was issued for, and the code hash is
// keyed per purpose as well (emailCodeHash).
const purposeRegister = "register"

// emailCode is the pending verification code mailed to one address for one
// purpose (at most one: issuing a new code replaces the old). The address is
// kept only as a keyed hash (emailSubjectHash), the code likewise.
type emailCode struct {
	Purpose   string
	EmailHash string
	CodeHash  string
	Attempts  int
	ExpiresAt time.Time
	CreatedAt time.Time
}

// authStore is web-auth's persistence port. Every method is safe for
// concurrent use; read-modify-write operations are atomic (one lock in
// memory, one transaction with row locks in MySQL).
type authStore interface {
	// CreateAccount inserts a. AccountID 0 allocates a new id; a set id is
	// used as given (dev seed, tests). errExists on a duplicate id or name.
	CreateAccount(ctx context.Context, a account) (account, error)
	AccountByID(ctx context.Context, id int64) (account, error)
	AccountByName(ctx context.Context, name string) (account, error)
	// UpdateAccount applies fn to the current row atomically; an error from
	// fn aborts the update and is returned unchanged.
	UpdateAccount(ctx context.Context, id int64, fn func(*account) error) (account, error)

	CreateSession(ctx context.Context, s refreshSession) error
	SessionByHash(ctx context.Context, hash string) (refreshSession, error)
	// RotateSession retires oldHash (revokeRotated, ReplacedBy=next) and
	// inserts next in one step; errNotFound when oldHash is not live (lost a
	// concurrent rotation, revoked, expired).
	RotateSession(ctx context.Context, oldHash string, next refreshSession, now time.Time) error
	RevokeSession(ctx context.Context, hash, reason string, now time.Time) error
	RevokeSessionFamily(ctx context.Context, familyID, reason string, now time.Time) error
	RevokeAccountSessions(ctx context.Context, accountID int64, reason string, now time.Time) error

	// PutResetCode replaces the account's pending code (attempts reset).
	PutResetCode(ctx context.Context, rc resetCode) error
	// ResetPassword checks codeHash against the pending code and, when it
	// matches, installs passwordHash, bumps TokenVersion, revokes every
	// refresh session and consumes the code — atomically. A mismatch counts
	// an attempt (errCodeInvalid); a missing or expired code is
	// errCodeInvalid; maxResetAttempts used is errCodeAttempts.
	ResetPassword(ctx context.Context, accountID int64, codeHash, passwordHash string, now time.Time) (account, error)

	// PutEmailCode replaces the pending code of (Purpose, EmailHash)
	// (attempts reset).
	PutEmailCode(ctx context.Context, ec emailCode) error
	// ConsumeEmailCode checks codeHash against the pending code of
	// (purpose, emailHash) and consumes it when it matches — the same
	// outcomes as ResetPassword: a mismatch counts an attempt
	// (errCodeInvalid), a missing or expired code is errCodeInvalid,
	// maxResetAttempts used is errCodeAttempts.
	ConsumeEmailCode(ctx context.Context, purpose, emailHash, codeHash string, now time.Time) error

	// RAM users (soft delete; ListRAMUsers omits deleted ones).
	ListRAMUsers(ctx context.Context, accountID int64) ([]ramUser, error)
	RAMUserByID(ctx context.Context, accountID, id int64) (ramUser, error)
	RAMUserByName(ctx context.Context, accountID int64, username string) (ramUser, error)
	// CreateRAMUser allocates the id; errExists for a live duplicate
	// username, *policyNotFoundError for an unknown attached policy.
	CreateRAMUser(ctx context.Context, u ramUser) (ramUser, error)
	// DeleteRAMUser soft-deletes (status 3) and frees the username.
	DeleteRAMUser(ctx context.Context, accountID, id int64) error

	ListRAMRoles(ctx context.Context, accountID int64) ([]ramRole, error)
	RAMRoleByID(ctx context.Context, accountID, id int64) (ramRole, error)
	RAMRoleByName(ctx context.Context, accountID int64, name string) (ramRole, error)
	CreateRAMRole(ctx context.Context, r ramRole) (ramRole, error)

	// ListPolicies returns the system policies plus the account's own.
	ListPolicies(ctx context.Context, accountID int64) ([]storedPolicy, error)
	// PoliciesByName resolves names visible to the account; unknown names
	// are simply absent from the result.
	PoliciesByName(ctx context.Context, accountID int64, names []string) (map[string]storedPolicy, error)
	// CreatePolicy inserts a custom policy; errExists when the name is taken
	// by the account or by a system policy.
	CreatePolicy(ctx context.Context, p storedPolicy) (storedPolicy, error)
}

// --- system policies ---------------------------------------------------------

// systemPolicyDef is a platform-provided policy, visible to every account
// (ram_policy.account_id = 0, scope = 1). The SQL rows are seeded by
// sql/V3__account_db_web_auth_persistence.sql with exactly these values
// (TestSystemPoliciesMatchMigration keeps the two in step).
type systemPolicyDef struct {
	ID       int64
	Name     string
	Document string
}

var systemPolicyDefs = []systemPolicyDef{
	{1, "EuEcsFullAccess", `{"Version":"1","Statement":[{"Effect":"Allow","Action":"euecs:*","Resource":"*"}]}`},
	{2, "EuReadOnlyAccess", `{"Version":"1","Statement":[{"Effect":"Allow","Action":["euecs:Describe*","euecs:List*"],"Resource":"*"}]}`},
}

// firstCustomPolicyID keeps custom policy ids clear of the system range.
const firstCustomPolicyID = 1000

// systemPolicies returns the parsed system policies.
func systemPolicies() []storedPolicy {
	out := make([]storedPolicy, 0, len(systemPolicyDefs))
	for _, d := range systemPolicyDefs {
		p, err := authz.ParsePolicy([]byte(d.Document))
		if err != nil {
			panic("system policy " + d.Name + ": " + err.Error())
		}
		out = append(out, storedPolicy{ID: d.ID, AccountID: 0, Name: d.Name, Type: "system", Document: d.Document, Parsed: p,
			CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	}
	return out
}

// --- shared validation ------------------------------------------------------

// ramUsernamePattern bounds a RAM user name: it is a login handle that ends up
// in principals ("user/<name>"), ARNs and logs. '#' is reserved for the
// rename a soft delete applies (deletedUsername).
var ramUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// validText bounds a free-text field: at most max runes, valid UTF-8, no
// control characters; required fields must be non-blank.
func validText(s string, max int, required bool) bool {
	if required && strings.TrimSpace(s) == "" {
		return false
	}
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// deletedUsername is the name a soft-deleted RAM user keeps: recognisable
// for audit, and out of the way of the account's unique (account, username)
// key so the name can be reused.
func deletedUsername(name string, id int64) string {
	if len(name) > 38 {
		name = name[:38]
	}
	return name + "#del#" + strconv.FormatInt(id, 10)
}

// dedupeNames drops empty and repeated names, keeping the first occurrence.
func dedupeNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// storeTime normalises a timestamp to what account_db's DATETIME columns keep
// (UTC, whole seconds), so the memory store reports exactly what MySQL would.
func storeTime(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

// createdAt is the creation stamp a store records: the given time, or now.
func createdAt(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now()
	}
	return storeTime(t)
}

// randomDigits returns n uniformly random decimal digits (crypto/rand,
// rejection sampling — no modulo bias).
func randomDigits(n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b >= 250 { // 250 = 25*10: keep the distribution uniform
				continue
			}
			out = append(out, '0'+b%10)
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}
