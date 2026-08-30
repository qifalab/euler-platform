// Package totp implements RFC 6238 time-based one-time passwords — the second
// factor behind MFA 强制登录 (07-security.md §4.2 M-7.6, 09-roadmap M-2).
//
// # Why a second factor is mandatory on this platform
//
// An account here moves real money: coupons, cash balance, AK/SK that can
// provision billable resources. A password alone is a single point of failure
// reused across sites; a leaked credential database row becomes an immediate
// financial loss. MFA makes credential replay insufficient on its own.
//
// # Parameters
//
// HMAC-SHA1 (the RFC 6238 default every authenticator app speaks), 30-second
// step, 6 digits, ±1 step acceptance window. The window exists because phone
// clocks drift and users type slowly; wider windows trade safety for comfort
// and 2×30s already tolerates a full minute of skew.
//
// # Replay protection
//
// A captured code is valid for up to 90 seconds (its step plus the ±1 window)
// — replayable within that window unless the server refuses time from going
// backwards. Verifier therefore records the timestep each key last consumed
// and rejects any match at or before it: a code, once accepted, never works
// again, and neither does an older sibling still inside the window. The
// classic "watch the victim type, then race them to the login box" attack
// dies at the second attempt.
//
// The seed is stored as a KMS envelope ciphertext (kms.PurposeUser) and
// decrypted only for the duration of a verify — it never appears in logs,
// responses, or the account table in the clear.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Parameters (RFC 6238 defaults). Step and Digits are fixed, not configurable:
// interop with arbitrary authenticator apps is the whole point, and every
// mainstream app assumes exactly these values.
const (
	Step   = 30 * time.Second
	Digits = 6
	// Skew is the acceptance window in steps either side of "now".
	Skew = 1
)

// Errors.
var (
	ErrBadCode       = errors.New("totp: code does not match")
	ErrReplayed      = errors.New("totp: code already used (time may not run backwards)")
	ErrMalformedCode = errors.New("totp: code must be exactly 6 digits")
	ErrBadSecret     = errors.New("totp: secret must be non-empty")
)

// GenerateSecret returns a fresh 160-bit random seed. RFC 4226 §5.3 requires
// at least 128 bits; 160 matches the SHA-1 block size and the de-facto length
// every authenticator expects.
func GenerateSecret() ([]byte, error) {
	secret := make([]byte, 20)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("totp: generate secret: %w", err)
	}
	return secret, nil
}

// SecretBase32 renders a seed the way authenticator apps take it: standard
// base32, padding stripped. This exact string goes into the provisioning URI
// and the QR code.
func SecretBase32(secret []byte) string {
	return strings.TrimRight(base32.StdEncoding.EncodeToString(secret), "=")
}

// ParseSecretBase32 reverses SecretBase32, accepting padding as optional
// (copy-pasted secrets often keep the trailing "=").
func ParseSecretBase32(s string) ([]byte, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimRight(s, "=")
	if s == "" {
		return nil, ErrBadSecret
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
}

// Code computes the 6-digit code for a moment in time.
func Code(secret []byte, at time.Time) string {
	counter := uint64(at.Unix()) / uint64(Step.Seconds())
	return hotp(secret, counter, Digits)
}

// ProvisioningURI builds the otpauth:// URI that turns a seed into a QR code
// (Google Authenticator, Microsoft Authenticator, 1Password, ...). Issuer is
// shown by the app above the account; account is the label the user matches
// against, so it should be the login identity, never the raw seed.
func ProvisioningURI(secret []byte, account, issuer string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", SecretBase32(secret))
	q.Set("issuer", issuer)
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// hotp is the RFC 4226 core: HMAC-SHA1 over the 8-byte big-endian counter,
// dynamic truncation, mod 10^digits, zero-padded.
func hotp(key []byte, counter uint64, digits int) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// Dynamic truncation (RFC 4226 §5.3): the low 4 bits of the last byte
	// select a 4-byte window, whose top bit is masked off.
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off])&0x7f)<<24 |
		uint32(sum[off+1])<<16 |
		uint32(sum[off+2])<<8 |
		uint32(sum[off+3])
	code := bin % uint32(pow10(digits))
	return fmt.Sprintf("%0*d", digits, code)
}

func pow10(n int) uint32 {
	p := uint32(1)
	for i := 0; i < n; i++ {
		p *= 10
	}
	return p
}

// Verifier is the replay-safe state machine behind login and binding. One
// instance per process (per user pool); the last-used timestep per key is
// held in memory, which is the standard trade-off: replay memory resets on
// restart, reopening a ≤90s window, in exchange for zero store round-trips
// per verification.
type Verifier struct {
	mu       sync.Mutex
	lastUsed map[string]uint64 // keyID → last consumed timestep
}

// NewVerifier returns an empty Verifier.
func NewVerifier() *Verifier {
	return &Verifier{lastUsed: make(map[string]uint64)}
}

// Verify checks a submitted code against the secret at time now, within the
// ±Skew window, and refuses any timestep at or before the key's last consumed
// one. keyID scopes the replay memory (the account or seed id), so two users
// sharing nothing cannot interfere.
//
// All mismatch paths — wrong code, malformed code, replay — are deliberately
// indistinguishable in timing structure: compute candidates, compare in
// constant time, only then consult replay state.
func (v *Verifier) Verify(keyID string, secret []byte, code string, now time.Time) error {
	if len(secret) == 0 {
		return ErrBadSecret
	}
	if len(code) != Digits {
		return ErrMalformedCode
	}
	for i := 0; i < Digits; i++ {
		if code[i] < '0' || code[i] > '9' {
			return ErrMalformedCode
		}
	}

	counter := uint64(now.Unix()) / uint64(Step.Seconds())
	var match uint64
	found := false
	for d := int64(-Skew); d <= Skew; d++ {
		c := counter
		if d < 0 && counter < uint64(-d) {
			continue // before the unix epoch; nobody authenticates there
		}
		if d < 0 {
			c -= uint64(-d)
		} else {
			c += uint64(d)
		}
		want := hotp(secret, c, Digits)
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			match, found = c, true
		}
	}
	if !found {
		return ErrBadCode
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if match <= v.lastUsed[keyID] {
		return ErrReplayed
	}
	v.lastUsed[keyID] = match
	return nil
}

// Forget drops a key's replay state (unbind/rebind path: a fresh seed starts
// with a clean slate).
func (v *Verifier) Forget(keyID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.lastUsed, keyID)
}
