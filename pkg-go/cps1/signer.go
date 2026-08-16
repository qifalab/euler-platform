// Package cps1 implements the CPS1-HMAC-SHA256 OpenAPI signing protocol.
//
// This is the single authoritative implementation of the platform's OpenAPI
// signature algorithm (07-security.md §4.1). The gateway verifier, the SDK
// signer, the OpenAPI Explorer online debugger, and the documentation-site
// example code all share this contract — no parallel implementations are
// permitted ("算法即契约,随 SDK 发布,永不兼容破坏").
//
// Algorithm (SigV4-style, four steps):
//
//	Step 1  CanonicalRequest =
//	          HTTPMethod \n
//	          CanonicalURI \n                # path, URI-encoded (do not encode /)
//	          CanonicalQueryString \n       # by key dict order, k/v both URL-encoded
//	          CanonicalHeaders \n            # host;x-cps-date;x-cps-content-sha256 mandatory,
//	                                       # remaining x-cps-* in dict order, values trimmed
//	          SignedHeaders \n               # header names lowercase, semicolon-separated
//	          Hex(SHA256(Body))
//
//	Step 2  StringToSign =
//	          "CPS1-HMAC-SHA256" \n
//	          x-cps-date \n
//	          {date}/{region}/{service}/cps1_request \n   # scope
//	          Hex(SHA256(CanonicalRequest))
//
//	Step 3  kDate    = HMAC-SHA256("CPS1" + SK, date)
//	        kRegion  = HMAC-SHA256(kDate,    region)
//	        kService = HMAC-SHA256(kRegion,  service)
//	        kSigning = HMAC-SHA256(kService, "cps1_request")
//
//	Step 4  Signature = HexEncode(HMAC-SHA256(kSigning, StringToSign))
//
// The Authorization header form:
//
//	CPS1-HMAC-SHA256 Credential={AK}/{scope}, SignedHeaders={list}, Signature={sig}
//
// where scope = "{date}/{region}/{service}/cps1_request".
//
// Required request headers (取代查询参数式公共参数):
//
//	Authorization            = see above
//	x-cps-date               = ISO8601 UTC, e.g. 20260804T093000Z (server tolerates ±15 min)
//	x-cps-content-sha256     = Hex(SHA256(body)); empty body hashes the empty string
//	x-cps-nonce              = random UUID, anti-replay
//	x-cps-security-token    = required only when using STS temporary credentials
//
// The x-cps- header prefix is signature-protocol-specific and is intentionally
// decoupled from the `sc` brand prefix (adjudication C9 / S4); it is retained
// unchanged.
package cps1

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Algorithm is the identifier string used in StringToSign and Authorization.
const Algorithm = "CPS1-HMAC-SHA256"

// Terminator is the final derivation step constant (SigV4 "request" equivalent).
const Terminator = "cps1_request"

// SchemePrefix is the leading token of the Authorization header value.
const SchemePrefix = Algorithm + " "

// MaxClockSkew is the maximum tolerated difference between x-cps-date and the
// server's wall clock (07-security.md §4.2 anti-replay layer 1).
const MaxClockSkew = 15 * time.Minute

// HeaderPrefix is the signature-protocol-specific header prefix.
const HeaderPrefix = "x-cps-"

// Mandatory signed headers: every CPS1 request MUST sign these three.
const (
	HeaderDate       = "x-cps-date"
	HeaderContentSHA = "x-cps-content-sha256"
	HeaderHost       = "host"
)

// SecurityTokenHeader is present only when signing with STS temp credentials.
const SecurityTokenHeader = "x-cps-security-token"

// NonceHeader is the anti-replay nonce header.
const NonceHeader = "x-cps-nonce"

// HeaderNonce is an alias of NonceHeader for callers that prefer the
// Header* naming used by the other protocol header constants.
const HeaderNonce = NonceHeader

// Errors returned by Verify.
var (
	ErrMissingAuthorization    = errors.New("cps1: missing Authorization header")
	ErrBadAuthorizationFormat  = errors.New("cps1: malformed Authorization header")
	ErrUnsupportedAlgorithm    = errors.New("cps1: unsupported algorithm")
	ErrMissingDate              = errors.New("cps1: missing x-cps-date header")
	ErrMissingContentSHA        = errors.New("cps1: missing x-cps-content-sha256 header")
	ErrMissingHost              = errors.New("cps1: missing host header")
	ErrClockSkew                = errors.New("cps1: request time outside ±15min window")
	ErrSignatureDoesNotMatch    = errors.New("cps1: signature does not match")
	ErrScopeFormat              = errors.New("cps1: invalid credential scope")
	ErrSignedHeadersMismatch    = errors.New("cps1: signed headers do not match canonical headers")
)

// Credentials is the key material used to sign or verify a request.
//
// AK is the public access-key identifier (prefix "SC"). SK is the secret key
// plaintext, required because verification recomputes the HMAC — SK is stored
// at rest as a KMS envelope ciphertext (sk_cipher + sk_key_version) and is
// decrypted only in the gateway verification cache (07-security.md §2.2/§2.5).
// SecurityToken is non-empty only for STS temporary credentials.
type Credentials struct {
	AK            string
	SK            string
	SecurityToken string
}

// Request captures the inputs needed to compute or verify a signature. It is
// transport-agnostic: the gateway parses HTTP headers into this struct; the
// SDK builds it from the outgoing request.
type Request struct {
	Method  string
	Host    string
	Path    string // raw request path, e.g. "/" or "/products"
	Query   url.Values
	Headers map[string]string

	// Body is the raw request body (may be empty). The caller is responsible
	// for providing the exact bytes that were sent; the content-sha256 header
	// MUST match SHA256(Body).
	Body []byte

	// Date is the parsed value of x-cps-date. When signing, callers may set
	// Headers[HeaderDate] instead and leave Date zero; Sign will populate it.
	Date time.Time
}

// SignedRequest is the output of Sign: the headers to attach to the outgoing
// HTTP request.
type SignedRequest struct {
	Authorization string
	Headers       map[string]string // includes x-cps-* headers; caller merges into real request
}

// scope builds the credential scope string: "{date}/{region}/{service}/cps1_request".
func scope(date time.Time, region, service string) string {
	return fmt.Sprintf("%s/%s/%s/%s", date.UTC().Format("20060102"), region, service, Terminator)
}

// hexSHA256 returns the lowercase hex SHA-256 digest of data.
func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hmacSHA256 returns HMAC-SHA256(key, data).
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// deriveSigningKey implements Step 3:
//
//	kDate    = HMAC-SHA256("CPS1" + SK, date)
//	kRegion  = HMAC-SHA256(kDate,    region)
//	kService = HMAC-SHA256(kRegion,  service)
//	kSigning = HMAC-SHA256(kService, "cps1_request")
func deriveSigningKey(sk, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("CPS1"+sk), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(Terminator))
}

// canonicalURI URI-encodes each path segment but preserves "/" separators.
// An empty path is treated as "/".
//
// Encoding is strict RFC 3986: only the unreserved set [A-Za-z0-9-._~] passes
// through. This deliberately does NOT use url.PathEscape, which leaves
// "$ @ : = & +" unescaped — those characters would then be encoded differently
// by SDKs in other languages (whose canonicalisers escape them), producing
// signatures that disagree across the language boundary. The algorithm is the
// wire contract (07§4.1, adjudication D4), so every implementation must encode
// identically; strict RFC 3986 is the interoperable choice.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	if path == "*" {
		return "*" // wildcard (e.g. S3-style bucket operations), do not encode
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = percentEncodeStrict(p)
	}
	return strings.Join(parts, "/")
}

// canonicalQueryString builds the canonical query string: keys sorted, both
// keys and values URL-encoded, joined by "&" as "k=v". Empty values are
// encoded as "k=" (a bare key with no "=" is not permitted).
func canonicalQueryString(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		vals := q[k]
		// Each value is a separate k=v pair, sorted for determinism.
		sort.Strings(vals)
		encKey := percentEncodeQuery(k)
		for j, v := range vals {
			if j > 0 {
				b.WriteByte('&')
			}
			b.WriteString(encKey)
			b.WriteByte('=')
			b.WriteString(percentEncodeQuery(v))
		}
	}
	return b.String()
}

// percentEncodeQuery applies RFC 3986 percent-encoding suitable for query
// strings: the unreserved set [A-Za-z0-9-._~] is left alone, everything else
// (including "/" and "+") is percent-encoded.
func percentEncodeQuery(s string) string {
	return percentEncodeStrict(s)
}

// percentEncodeStrict is the single encoding routine used for both path
// segments and query components. Only the RFC 3986 unreserved set survives;
// everything else becomes %XX with uppercase hex. Keeping one routine for both
// positions is what makes the canonical request reproducible across languages.
func percentEncodeStrict(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalHeaders selects the headers that participate in signing, lowercases
// names, trims and collapses interior whitespace in values, sorts by name, and
// returns the canonical header block and the SignedHeaders list.
//
// host, x-cps-date and x-cps-content-sha256 are mandatory. The remaining
// x-cps-* headers present are included. Non-prefixed headers other than host
// are not signed (they are not part of the protocol).
func canonicalHeaders(headers map[string]string) (canonical, signedList string, err error) {
	// Build a lowercase-keyed copy with trimmed/collapsed values.
	normalized := make(map[string]string, len(headers)+3)
	for k, v := range headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" {
			continue
		}
		normalized[lk] = collapseSpaces(strings.TrimSpace(v))
	}
	// Mandatory headers.
	if _, ok := normalized[HeaderHost]; !ok {
		return "", "", ErrMissingHost
	}
	if _, ok := normalized[HeaderDate]; !ok {
		return "", "", ErrMissingDate
	}
	if _, ok := normalized[HeaderContentSHA]; !ok {
		return "", "", ErrMissingContentSHA
	}
	// Determine which headers participate: host + all x-cps-*.
	var names []string
	for k := range normalized {
		if k == HeaderHost || strings.HasPrefix(k, HeaderPrefix) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var can, signed strings.Builder
	for i, n := range names {
		can.WriteString(n)
		can.WriteByte(':')
		can.WriteString(normalized[n])
		can.WriteByte('\n')
		if i > 0 {
			signed.WriteByte(';')
		}
		signed.WriteString(n)
	}
	return can.String(), signed.String(), nil
}

// collapseSpaces replaces runs of whitespace with a single space.
func collapseSpaces(s string) string {
	var b strings.Builder
	inSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
		} else {
			b.WriteByte(c)
			inSpace = false
		}
	}
	return b.String()
}

// canonicalRequest builds the Step 1 string.
func canonicalRequest(req Request) (string, error) {
	can, _, err := canonicalHeaders(req.Headers)
	if err != nil {
		return "", err
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "GET"
	}
	uri := canonicalURI(req.Path)
	query := canonicalQueryString(req.Query)
	bodyHash := req.Headers[HeaderContentSHA]
	if bodyHash == "" {
		// Fall back to hashing the body directly; this also covers the empty case.
		bodyHash = hexSHA256(req.Body)
	}
	_, signedList, err := canonicalHeaders(req.Headers)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{method, uri, query, can, signedList, bodyHash}, "\n"), nil
}

// stringToSign builds the Step 2 string.
func stringToSign(req Request, region, service string) (string, error) {
	cr, err := canonicalRequest(req)
	if err != nil {
		return "", err
	}
	if req.Date.IsZero() {
		return "", ErrMissingDate
	}
	return strings.Join([]string{
		Algorithm,
		req.Date.UTC().Format("20060102T150405Z"),
		scope(req.Date, region, service),
		hexSHA256([]byte(cr)),
	}, "\n"), nil
}

// Sign computes the signature and produces the Authorization header plus the
// x-cps-* headers the caller must attach to the outgoing request.
//
// The caller supplies region and service (service is the productCode-derived
// service namespace, e.g. "ecs" for scecs; the routing subdomain is
// {productCode}.api.starcloud.cn per 04-middleware §3.2). The Host header must
// already be present in req.Headers. If x-cps-content-sha256 is unset, it is
// computed from Body. If x-cps-date is unset, now is used.
func Sign(req Request, creds Credentials, region, service string, now time.Time) (SignedRequest, error) {
	if creds.AK == "" || creds.SK == "" {
		return SignedRequest{}, errors.New("cps1: credentials required")
	}
	if region == "" || service == "" {
		return SignedRequest{}, errors.New("cps1: region and service required")
	}
	// Materialize a header map we can mutate without touching the caller's.
	hdrs := make(map[string]string, len(req.Headers)+4)
	for k, v := range req.Headers {
		hdrs[k] = v
	}
	if _, ok := hdrs[HeaderHost]; !ok {
		return SignedRequest{}, ErrMissingHost
	}
	date := req.Date
	if date.IsZero() {
		date = now
	}
	hdrs[HeaderDate] = date.UTC().Format("20060102T150405Z")
	if hdrs[HeaderContentSHA] == "" {
		hdrs[HeaderContentSHA] = hexSHA256(req.Body)
	}
	if _, ok := hdrs[HeaderNonce]; !ok {
		// Nonce is part of the protocol but the caller usually supplies it.
		// We do not invent one here to keep Sign deterministic; the SDK layer
		// generates a UUID nonce before calling Sign.
		return SignedRequest{}, errors.New("cps1: x-cps-nonce required")
	}
	if creds.SecurityToken != "" {
		hdrs[SecurityTokenHeader] = creds.SecurityToken
	}

	r := Request{
		Method:  req.Method,
		Host:    req.Host,
		Path:    req.Path,
		Query:   req.Query,
		Headers: hdrs,
		Body:    req.Body,
		Date:    date,
	}
	sts, err := stringToSign(r, region, service)
	if err != nil {
		return SignedRequest{}, err
	}
	kSigning := deriveSigningKey(creds.SK, date.UTC().Format("20060102"), region, service)
	sig := hex.EncodeToString(hmacSHA256(kSigning, []byte(sts)))

	auth := fmt.Sprintf("%sCredential=%s/%s, SignedHeaders=%s, Signature=%s",
		SchemePrefix, creds.AK, scope(date, region, service), signedHeaderList(hdrs), sig)

	out := SignedRequest{Authorization: auth, Headers: hdrs}
	return out, nil
}

// signedHeaderList returns the SignedHeaders value for the Authorization header,
// recomputed from the header set.
func signedHeaderList(headers map[string]string) string {
	list, err := SignedHeaderList(headers)
	if err != nil {
		return ""
	}
	return list
}

// SignedHeaderList returns the SignedHeaders clause (header names lowercase,
// semicolon-separated, sorted) for the given header set. It is exported so SDKs
// and the OpenAPI Explorer can render the same SignedHeaders value the gateway
// verifies, without re-implementing the canonicalization (07§4.1, adjudication
// D4: one definition of the algorithm, consumed by every implementation).
func SignedHeaderList(headers map[string]string) (string, error) {
	_, list, err := canonicalHeaders(headers)
	return list, err
}

// Verify recomputes the signature for an incoming request and compares it in
// constant time. It also enforces the time-window anti-replay check. The nonce
// dedup (Redis SET NX, TTL 16min) is the caller's responsibility — it is not
// part of the signature algorithm and lives in the gateway plugin.
//
// region and service MUST be derived from the routed product subdomain, not
// from any client-supplied value (scecs.api.starcloud.cn → region cn-north-1,
// service "ecs"). The caller resolves these before invoking Verify.
func Verify(req Request, creds Credentials, region, service string, now time.Time) error {
	if req.Headers == nil {
		return ErrMissingAuthorization
	}
	auth := strings.TrimSpace(req.Headers["Authorization"])
	if auth == "" {
		return ErrMissingAuthorization
	}
	if !strings.HasPrefix(auth, SchemePrefix) {
		return ErrUnsupportedAlgorithm
	}
	rest := strings.TrimPrefix(auth, SchemePrefix)

	// Parse "Credential=..., SignedHeaders=..., Signature=..."
	// The credential scope itself contains '/', so split on the named fields.
	credPart, signedPart, sigPart, err := parseAuthorization(rest)
	if err != nil {
		return err
	}

	// Parse credential scope: {AK}/{date}/{region}/{service}/cps1_request
	// credPart.credential is the full "AK/date/region/service/cps1_request"
	// string; credPart.scope is the same with the AK stripped
	// ("date/region/service/cps1_request"). Split the full credential to
	// recover the AK the client claims to be using.
	scopeParts := strings.SplitN(credPart.credential, "/", 2)
	if len(scopeParts) != 2 {
		return ErrScopeFormat
	}
	ak := scopeParts[0]
	if ak != creds.AK {
		// AK mismatch: do not reveal whether the AK exists. Treat as a
		// signature failure (the gateway's AK-existence check happens
		// earlier in the plugin chain per 07§4.1; this is a defense-in-depth).
		return ErrSignatureDoesNotMatch
	}
	sc := strings.Split(scopeParts[1], "/")
	if len(sc) != 4 || sc[3] != Terminator {
		return ErrScopeFormat
	}
	dateStr, sigRegion, sigService := sc[0], sc[1], sc[2]
	if sigRegion != region || sigService != service {
		return ErrSignatureDoesNotMatch
	}

	// Parse date header and check clock skew.
	dateHdr, ok := req.Headers[HeaderDate]
	if !ok {
		// Fall back to a lowercase lookup; gateway normalizes, but be lenient.
		for k, v := range req.Headers {
			if strings.EqualFold(k, HeaderDate) {
				dateHdr = v
				ok = true
				break
			}
		}
	}
	if !ok {
		return ErrMissingDate
	}
	parsed, err := time.Parse("20060102T150405Z", dateHdr)
	if err != nil {
		return ErrClockSkew
	}
	if d := now.Sub(parsed); d < -MaxClockSkew || d > MaxClockSkew {
		return ErrClockSkew
	}
	if parsed.UTC().Format("20060102") != dateStr {
		return ErrScopeFormat
	}

	// Rebuild the header map with a normalized lowercase copy so that the
	// SignedHeaders list from the client lines up with canonicalHeaders.
	norm := make(map[string]string, len(req.Headers))
	for k, v := range req.Headers {
		norm[strings.ToLower(strings.TrimSpace(k))] = collapseSpaces(strings.TrimSpace(v))
	}
	// Verify that the SignedHeaders declared in Authorization exactly match
	// the set we would canonicalize. This prevents a client from claiming to
	// sign headers it did not include, or omitting mandatory ones.
	want := strings.Split(signedPart, ";")
	got := declaredSignedHeaders(norm)
	if !sameSet(want, got) {
		return ErrSignedHeadersMismatch
	}
	// Mandatory presence.
	if _, ok := norm[HeaderHost]; !ok {
		return ErrMissingHost
	}
	if _, ok := norm[HeaderContentSHA]; !ok {
		return ErrMissingContentSHA
	}

	r := Request{
		Method:  req.Method,
		Host:    req.Host,
		Path:    req.Path,
		Query:   req.Query,
		Headers: norm,
		Body:    req.Body,
		Date:    parsed,
	}
	// Body integrity: recompute SHA256 of the actual body and compare it to
	// the x-cps-content-sha256 header that was signed. The signature itself
	// only proves the header value is what was signed, not that it matches
	// the bytes on the wire — this check closes that gap (07§4.2 layer 3).
	signedBodyHash := norm[HeaderContentSHA]
	actualBodyHash := hexSHA256(req.Body)
	if signedBodyHash != actualBodyHash {
		return ErrSignatureDoesNotMatch
	}
	sts, err := stringToSign(r, region, service)
	if err != nil {
		return err
	}
	kSigning := deriveSigningKey(creds.SK, dateStr, region, service)
	expected := hmacSHA256(kSigning, []byte(sts))

	gotSig, err := hex.DecodeString(sigPart)
	if err != nil {
		return ErrSignatureDoesNotMatch
	}
	if !hmac.Equal(expected, gotSig) {
		return ErrSignatureDoesNotMatch
	}
	return nil
}

// declaredSignedHeaders returns the sorted list of headers that participate in
// canonicalization (host + all x-cps-*).
func declaredSignedHeaders(norm map[string]string) []string {
	var out []string
	for k := range norm {
		if k == HeaderHost || strings.HasPrefix(k, HeaderPrefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

// parsedAuth holds the three fields of an Authorization header value.
type parsedAuth struct {
	credential   string
	signedHeaders string
	signature    string
	scope        string // scope portion of credential (everything after AK/)
}

// parseAuthorization splits the Authorization header body into its three fields
// and extracts the credential scope (date/region/service/cps1_request).
func parseAuthorization(rest string) (parsedAuth, string, string, error) {
	var cred, signed, sig string
	// Split on ", " boundaries but each value itself may contain nothing
	// special. Use a simple scanner since the format is well-defined.
	parts := splitAuthFields(rest)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case strings.HasPrefix(p, "Credential="):
			cred = strings.TrimPrefix(p, "Credential=")
		case strings.HasPrefix(p, "SignedHeaders="):
			signed = strings.TrimPrefix(p, "SignedHeaders=")
		case strings.HasPrefix(p, "Signature="):
			sig = strings.TrimPrefix(p, "Signature=")
		}
	}
	if cred == "" || signed == "" || sig == "" {
		return parsedAuth{}, "", "", ErrBadAuthorizationFormat
	}
	scopeParts := strings.SplitN(cred, "/", 2)
	if len(scopeParts) != 2 {
		return parsedAuth{}, "", "", ErrScopeFormat
	}
	return parsedAuth{credential: cred, signedHeaders: signed, signature: sig, scope: scopeParts[1]}, signed, sig, nil
}

// splitAuthFields splits an Authorization body on commas that separate the
// top-level Credential/SignedHeaders/Signature fields. Commas do not appear
// inside any of these values, so a plain split is safe.
func splitAuthFields(s string) []string {
	return strings.Split(s, ",")
}
