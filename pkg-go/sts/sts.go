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
// The AK format delegates to identifier.AKID (single source for the SC prefix),
// exactly as long-lived keys do — the temporary flag is carried by the token's
// expiry, not by a divergent AK format.
package sts

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/starcloud/sc-platform/identifier"
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

// Issuer mints temporary credentials for a fixed TTL.
type Issuer struct {
	ttl time.Duration
	now func() time.Time
}

// NewIssuer builds an issuer for the given TTL. A non-positive TTL is rejected
// — temporary credentials that never expire are permanent credentials, which is
// a different product and a silent security hole if conflated.
func NewIssuer(ttl time.Duration, now func() time.Time) (*Issuer, error) {
	if ttl <= 0 {
		return nil, errors.New("sts: TTL must be positive — temporary credentials must expire")
	}
	if now == nil {
		now = time.Now
	}
	return &Issuer{ttl: ttl, now: now}, nil
}

// AssumeRole issues temporary credentials for role. The role string is carried
// into the security token (a scoped, revocable grant) but the AK/SK format is
// identical to long-lived keys — expiry is what makes them temporary.
func (i *Issuer) AssumeRole(role, accountID string) (Credentials, error) {
	if role == "" || accountID == "" {
		return Credentials{}, errors.New("sts: role and accountID are required")
	}
	akBody, err := randomHex(15) // 30 hex chars → SC-prefixed 32-char AK
	if err != nil {
		return Credentials{}, err
	}
	sk, err := randomHex(20) // 40 hex chars
	if err != nil {
		return Credentials{}, err
	}
	token, err := randomHex(16) // 32 hex chars
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		AccessKey:     identifier.AKID(akBody),
		SecretKey:     sk,
		SecurityToken: fmt.Sprintf("STS.%s.%s.%s", role, accountID, token),
		ExpiresAt:     i.now().Add(i.ttl),
	}, nil
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
