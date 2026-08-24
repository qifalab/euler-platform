// Package verify implements svc-iam's internal OpenAPI verification endpoint —
// the target of the APISIX forward-auth bypass (04§3.4).
//
// APISIX has no built-in cloud-vendor signature plugin, so the gateway's
// sc-auth plugin forwards the request metadata here; this endpoint owns the
// CPS1-HMAC-SHA256 contract and the AK metadata cache. Endpoint budget is
// P99 < 10ms (local signature computation + cached AK metadata), which is why
// nothing on this path may touch the database synchronously.
//
// Verification order is fixed by 07§4.1 and must not be reordered — each step
// is cheaper than the next, so failing fast also sheds load under attack:
//
//	① time window (x-cps-date within ±15 min)
//	② AK existence & status (enabled / not deleted / owner not frozen)
//	③ STS token validity & expiry (when x-cps-security-token present)
//	④ recompute signature, constant-time compare
//	⑤ nonce dedup (Redis SET NX, TTL 16 min)
//	⑥ inject identity context downstream
//
// On success the gateway injects X-Sc-Account-Id / X-Sc-Identity / X-Sc-TraceId
// plus X-Sc-Ak-Id and X-Sc-Quota-Qps for per-AK rate limiting. Upstream
// services trust these headers only from gateway mTLS / internal CIDR;
// anything else carrying them is rejected 403 (07§4.3).
package verify

import (
	"errors"
	"net/url"
	"time"

	"github.com/starcloud/sc-platform/accesskey"
	"github.com/starcloud/sc-platform/cps1"
)

// NonceStore deduplicates replay nonces. The production implementation is
// Redis SET NX with a 16-minute TTL (one minute beyond the 15-minute clock
// window, so a nonce cannot outlive its own validity period).
//
// Capacity at phase-1 peak: ~10k QPS × 960s ≈ 9.6M keys × ~120B ≈ 1.2GB,
// which fits a single Redis Cluster shard (07§4.2).
type NonceStore interface {
	// SetNX records the nonce and reports whether it was new. A false return
	// means the nonce was already used → replay.
	SetNX(key string, ttl time.Duration) (bool, error)
}

// AKResolver returns the plaintext SK and metadata for an access key. svc-iam
// backs this with the two-level cache described in 07§2.5: local L1 plus Redis
// L2 keyed by (ciphertext, sk_key_version), so a key rotation invalidates it.
type AKResolver interface {
	ResolveSK(ak string, now time.Time) (sk string, rec accesskey.Record, err error)
}

// AccountStatusChecker reports whether the owning account is usable. A frozen
// or cancelled account must fail verification even when the signature is
// valid (07§4.1 step ②).
type AccountStatusChecker interface {
	IsAccountUsable(accountID int64) (bool, error)
}

// STSValidator validates a temporary security token (x-cps-security-token).
// STS credentials are never persisted, so validation is signature/expiry based.
type STSValidator interface {
	ValidateToken(token string, now time.Time) (accountID int64, principal string, err error)
}

// NonceTTL is one minute beyond the clock-skew window so a nonce is retained
// for the whole period a request bearing it could still be considered fresh.
const NonceTTL = cps1.MaxClockSkew + time.Minute

// Failure reasons map to the 403 error codes in 03§9.3 / 07§4.1.
var (
	ErrRequestTimeTooSkewed  = errors.New("RequestTimeTooSkewed")
	ErrInvalidAccessKeyID    = errors.New("InvalidAccessKeyId")
	ErrSecurityTokenExpired  = errors.New("SecurityTokenExpired")
	ErrSignatureDoesNotMatch = errors.New("SignatureDoesNotMatch")
	ErrSignatureNonceUsed    = errors.New("SignatureNonceUsed")
	ErrSignatureNonceMissing = errors.New("SignatureNonceMissing")
	ErrAccountUnusable       = errors.New("AccountUnusable")
)

// Request is what the gateway forwards for verification.
type Request struct {
	Method  string
	Host    string
	Path    string
	Query   url.Values
	Headers map[string]string
	Body    []byte

	// Region and Service are resolved by the gateway from the routed product
	// subdomain ({productCode}.api.starcloud.cn), never from client input —
	// otherwise a caller could sign for one scope and be verified in another.
	Region  string
	Service string
}

// Identity is the verified caller, injected downstream as request headers.
type Identity struct {
	AccountID  int64
	AKID       string
	Principal  string // e.g. "user/alice"; empty for master-account keys
	OwnerType  accesskey.OwnerType
	MFAPresent bool
}

// Verifier runs the six-step verification chain.
type Verifier struct {
	AK       AKResolver
	Nonces   NonceStore
	Accounts AccountStatusChecker
	STS      STSValidator
}

// Verify executes the chain in the fixed order. The returned error is one of
// the sentinel values above, which the gateway maps to a 403 error code.
func (v *Verifier) Verify(req Request, now time.Time) (Identity, error) {
	// The Authorization header carries the AK inside the credential scope;
	// parse it out before anything else so steps ② and ④ can use it.
	ak, err := extractAK(req.Headers)
	if err != nil {
		return Identity{}, ErrInvalidAccessKeyID
	}

	// ① Time window. Cheapest check, and the one that sheds replayed traffic
	// before any key material is touched.
	if err := checkTimeWindow(req.Headers, now); err != nil {
		return Identity{}, err
	}

	// ② AK existence & status. ResolveSK enforces enabled/not-deleted and the
	// rotation grace deadline.
	sk, rec, err := v.AK.ResolveSK(ak, now)
	if err != nil {
		return Identity{}, ErrInvalidAccessKeyID
	}
	// Owner account must be usable (not frozen / cancelled).
	if v.Accounts != nil {
		usable, err := v.Accounts.IsAccountUsable(rec.AccountID)
		if err != nil || !usable {
			return Identity{}, ErrAccountUnusable
		}
	}

	// ③ STS token validity, when the request uses temporary credentials.
	identity := Identity{
		AccountID: rec.AccountID,
		AKID:      ak,
		OwnerType: rec.OwnerType,
	}
	if token := header(req.Headers, cps1.SecurityTokenHeader); token != "" {
		if v.STS == nil {
			return Identity{}, ErrSecurityTokenExpired
		}
		stsAccount, principal, err := v.STS.ValidateToken(token, now)
		if err != nil {
			return Identity{}, ErrSecurityTokenExpired
		}
		// The token must belong to the same account as the AK.
		if stsAccount != rec.AccountID {
			return Identity{}, ErrSecurityTokenExpired
		}
		identity.Principal = principal
	}

	// ④ Recompute the signature and compare in constant time. cps1.Verify
	// also re-derives the body hash, so a tampered body fails here.
	sigReq := cps1.Request{
		Method:  req.Method,
		Host:    req.Host,
		Path:    req.Path,
		Query:   req.Query,
		Headers: req.Headers,
		Body:    req.Body,
	}
	creds := cps1.Credentials{AK: ak, SK: sk}
	if err := cps1.Verify(sigReq, creds, req.Region, req.Service, now); err != nil {
		switch {
		case errors.Is(err, cps1.ErrClockSkew):
			return Identity{}, ErrRequestTimeTooSkewed
		default:
			return Identity{}, ErrSignatureDoesNotMatch
		}
	}

	// ⑤ Nonce dedup. Deliberately after signature verification: an attacker
	// must forge a valid signature before they can burn nonce-store capacity.
	// When a nonce store is configured, the nonce header is MANDATORY: letting
	// a request omit it would let any replayer bypass the dedup entirely by
	// stripping the header, which makes the whole store decorative.
	if v.Nonces != nil {
		nonce := header(req.Headers, cps1.NonceHeader)
		if nonce == "" {
			return Identity{}, ErrSignatureNonceMissing
		}
		fresh, err := v.Nonces.SetNX("sec:nonce:"+ak+":"+nonce, NonceTTL)
		if err != nil {
			// Fail closed: if the replay store is unavailable we cannot prove
			// this request is not a replay.
			return Identity{}, ErrSignatureNonceUsed
		}
		if !fresh {
			return Identity{}, ErrSignatureNonceUsed
		}
	}

	// ⑥ Identity context returned to the gateway for header injection.
	return identity, nil
}

// checkTimeWindow enforces |now - x-cps-date| ≤ 15 min (07§4.2 layer 1).
func checkTimeWindow(headers map[string]string, now time.Time) error {
	raw := header(headers, cps1.HeaderDate)
	if raw == "" {
		return ErrRequestTimeTooSkewed
	}
	ts, err := time.Parse("20060102T150405Z", raw)
	if err != nil {
		return ErrRequestTimeTooSkewed
	}
	d := now.Sub(ts)
	if d < -cps1.MaxClockSkew || d > cps1.MaxClockSkew {
		return ErrRequestTimeTooSkewed
	}
	return nil
}

// extractAK pulls the access key id out of the Authorization credential scope:
// "CPS1-HMAC-SHA256 Credential={AK}/{date}/{region}/{service}/cps1_request, ..."
func extractAK(headers map[string]string) (string, error) {
	auth := header(headers, "authorization")
	if auth == "" {
		return "", ErrInvalidAccessKeyID
	}
	const marker = "Credential="
	i := indexOf(auth, marker)
	if i < 0 {
		return "", ErrInvalidAccessKeyID
	}
	rest := auth[i+len(marker):]
	// AK runs up to the first '/'.
	for j := 0; j < len(rest); j++ {
		if rest[j] == '/' {
			if j == 0 {
				return "", ErrInvalidAccessKeyID
			}
			return rest[:j], nil
		}
		if rest[j] == ',' || rest[j] == ' ' {
			return "", ErrInvalidAccessKeyID
		}
	}
	return "", ErrInvalidAccessKeyID
}

// header does a case-insensitive header lookup, since the gateway may forward
// headers with their original casing.
func header(headers map[string]string, name string) string {
	if v, ok := headers[name]; ok {
		return v
	}
	for k, v := range headers {
		if equalFold(k, name) {
			return v
		}
	}
	return ""
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
