// Package sts implements temporary-credential issuance (STS) — the phase-3
// productization of 角色/STS (07§10 M1 lists it as 商业化可售; 03§9.2 models
// `x-cps-security-token` as part of the signing algorithm contract). It is
// deferred from phase 1/2 (README: "STS remains phase 3").
//
// AssumeRole issues a temporary AK/SK/SecurityToken triple with an expiry. The
// token rides the CPS1 signature as `x-cps-security-token` (pkg-go/cps1
// Credentials.SecurityToken already carries it), so a caller with temporary
// credentials signs exactly like a long-lived key — the gateway verifier
// validates token presence and expiry on top of the signature (03§9.2).
//
// Two things are non-negotiable and therefore encoded:
//
//   - Temporary credentials ALWAYS expire. A zero or negative TTL is rejected at
//     construction, not discovered when a leaked token never stops working.
//   - An expired credential is invalid regardless of any other field. The
//     boundary is inclusive (expires_at is the first invalid instant).
//
// The AK format delegates to identifier.AKID (single source for the EU prefix),
// exactly as long-lived keys do — the temporary flag is carried by the token's
// expiry, not by a divergent AK format.
package sts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/identifier"
)

// DefaultTTL is the default temporary-credential lifetime (one hour). It is a
// default, not a ceiling: callers may shorten it, but never zero it.
const DefaultTTL = time.Hour

// Credentials is the temporary credential set an AssumeRole returns.
type Credentials struct {
	AccessKey     string
	SecretKey     string
	SecurityToken string
	ExpiresAt     time.Time
}

// Expired reports whether the credentials are past their expiry. The boundary
// is inclusive: at exactly ExpiresAt the credentials are already invalid.
func (c Credentials) Expired(now time.Time) bool { return !now.Before(c.ExpiresAt) }

// Valid reports whether the credentials are complete and unexpired.
func (c Credentials) Valid(now time.Time) bool {
	return c.AccessKey != "" && c.SecretKey != "" && c.SecurityToken != "" && !c.Expired(now)
}

// KeyEnv names the 32-byte (base64) key that seals security tokens. web-auth
// (issuer) and svc-iam verify (validator) must share it.
const KeyEnv = "EULER_STS_KEY"

// tokenPrefix versions the sealed-token format.
const tokenPrefix = "STS2."

// Errors returned by Validate. They are deliberately coarse.
var (
	ErrInvalidToken = errors.New("sts: invalid security token")
	ErrTokenExpired = errors.New("sts: security token expired")
)

// Issuer mints temporary credentials for a fixed TTL, sealing them into the
// security token.
//
// The token used to be the plain string "STS.<role>.<account>.<random>":
// nothing bound it to the issuer, so anyone could write a token naming any
// account and role, and the temporary AK/SK it came with could never be
// verified (they were stored nowhere). Now the token is the credential itself,
// AES-256-GCM sealed: {ak, sk, account, role, exp}. Only holders of the STS
// key can mint one, tampering breaks the GCM tag, and the verifier recovers
// the temporary SK from the token to check the request signature — stateless,
// no credential store.
type Issuer struct {
	ttl  time.Duration
	now  func() time.Time
	aead cipher.AEAD
}

// NewIssuer builds an issuer for the given TTL and 32-byte sealing key. A
// non-positive TTL is rejected — temporary credentials that never expire are
// permanent credentials, which is a different product and a silent security
// hole if conflated.
func NewIssuer(ttl time.Duration, now func() time.Time, key []byte) (*Issuer, error) {
	if ttl <= 0 {
		return nil, errors.New("sts: TTL must be positive — temporary credentials must expire")
	}
	if now == nil {
		now = time.Now
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Issuer{ttl: ttl, now: now, aead: aead}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("sts: sealing key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// KeyFromEnv decodes EULER_STS_KEY. ok is false when the variable is unset.
func KeyFromEnv() (key []byte, ok bool, err error) {
	raw := strings.TrimSpace(os.Getenv(KeyEnv))
	if raw == "" {
		return nil, false, nil
	}
	key, err = base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, true, fmt.Errorf("sts: %s is not base64: %w", KeyEnv, err)
	}
	if len(key) != 32 {
		return nil, true, fmt.Errorf("sts: %s must decode to 32 bytes, got %d", KeyEnv, len(key))
	}
	return key, true, nil
}

// sealed is the token plaintext.
type sealed struct {
	AK      string `json:"ak"`
	SK      string `json:"sk"`
	Account string `json:"acct"`
	Role    string `json:"role"`
	Exp     int64  `json:"exp"` // unix seconds, first invalid instant
}

// Claims is what a valid security token proves.
type Claims struct {
	AccessKey string
	SecretKey string
	AccountID int64
	Role      string
	ExpiresAt time.Time
}

// AssumeRole issues temporary credentials for role in accountID. The caller
// (web-auth) must have checked that the role exists and is enabled on that
// account; the issuer only seals what it is given.
func (i *Issuer) AssumeRole(role, accountID string) (Credentials, error) {
	if role == "" || accountID == "" {
		return Credentials{}, errors.New("sts: role and accountID are required")
	}
	if _, err := strconv.ParseInt(accountID, 10, 64); err != nil {
		return Credentials{}, errors.New("sts: accountID must be numeric")
	}
	akBody, err := randomHex(15) // 30 hex chars → EU-prefixed 32-char AK
	if err != nil {
		return Credentials{}, err
	}
	sk, err := randomHex(20) // 40 hex chars
	if err != nil {
		return Credentials{}, err
	}
	exp := i.now().Add(i.ttl)
	ak := identifier.AKID(akBody)
	plain, err := json.Marshal(sealed{AK: ak, SK: sk, Account: accountID, Role: role, Exp: exp.Unix()})
	if err != nil {
		return Credentials{}, err
	}
	nonce := make([]byte, i.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Credentials{}, fmt.Errorf("sts: random: %w", err)
	}
	box := i.aead.Seal(nonce, nonce, plain, []byte(tokenPrefix))
	return Credentials{
		AccessKey:     ak,
		SecretKey:     sk,
		SecurityToken: tokenPrefix + base64.RawURLEncoding.EncodeToString(box),
		ExpiresAt:     time.Unix(exp.Unix(), 0),
	}, nil
}

// Validate unseals a security token and returns its claims. A token that was
// not sealed with this key, was altered, or is past its expiry is rejected.
func (i *Issuer) Validate(token string, now time.Time) (Claims, error) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return Claims{}, ErrInvalidToken
	}
	box, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, tokenPrefix))
	if err != nil || len(box) < i.aead.NonceSize() {
		return Claims{}, ErrInvalidToken
	}
	plain, err := i.aead.Open(nil, box[:i.aead.NonceSize()], box[i.aead.NonceSize():], []byte(tokenPrefix))
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var s sealed
	if err := json.Unmarshal(plain, &s); err != nil || s.AK == "" || s.SK == "" || s.Role == "" {
		return Claims{}, ErrInvalidToken
	}
	acct, err := strconv.ParseInt(s.Account, 10, 64)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	exp := time.Unix(s.Exp, 0)
	if !now.Before(exp) {
		return Claims{}, ErrTokenExpired
	}
	return Claims{AccessKey: s.AK, SecretKey: s.SK, AccountID: acct, Role: s.Role, ExpiresAt: exp}, nil
}

// randomHex returns n bytes as a lowercase hex string of length 2n, drawn from
// crypto/rand (uniform, not math/rand).
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("sts: random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
