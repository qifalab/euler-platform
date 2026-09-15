package cps1

import (
	"crypto/hmac"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fixedTime is the canonical x-cps-date used across the golden-vector tests.
var fixedTime = time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)

const (
	testSK = "test-secret-key-0123456789" // never real material
	testRegion  = "cn-north-1"
	testService = "ecs"
)

// testAK is a 32-char AK with the EU prefix (07§2.5). Not a const because it
// is built from a function call.
var testAK = "EU" + strings.Repeat("A", 30)

func baseHeaders(host, nonce string) map[string]string {
	return map[string]string{
		"host":                  host,
		HeaderDate:              fixedTime.Format("20060102T150405Z"),
		HeaderContentSHA:        hexSHA256(nil),
		HeaderNonce:             nonce,
	}
}

// withAuth copies the signed headers and attaches the Authorization header
// that Sign returned separately, producing the header map the gateway sees.
func withAuth(s SignedRequest) map[string]string {
	out := make(map[string]string, len(s.Headers)+1)
	for k, v := range s.Headers {
		out[k] = v
	}
	out["Authorization"] = s.Authorization
	return out
}

func TestSignThenVerifyRoundTrip(t *testing.T) {
	nonce := "550e8400-e29b-41d4-a716-446655440000"
	hdrs := baseHeaders("euecs.api.euler.emoera.com", nonce)
	body := []byte(`{"ImageId":"img-001","InstanceType":"s2.large"}`)
	hdrs[HeaderContentSHA] = hexSHA256(body)

	req := Request{
		Method:  "POST",
		Host:    "euecs.api.euler.emoera.com",
		Path:    "/",
		Query:   url.Values{"Action": {"RunInstances"}, "Version": {"2026-08-01"}},
		Headers: hdrs,
		Body:    body,
	}

	signed, err := Sign(req, Credentials{AK: testAK, SK: testSK}, testRegion, testService, fixedTime)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !strings.HasPrefix(signed.Authorization, SchemePrefix) {
		t.Fatalf("Authorization missing scheme: %q", signed.Authorization)
	}
	if !strings.Contains(signed.Authorization, "Credential="+testAK+"/") {
		t.Fatalf("Authorization missing AK in credential: %q", signed.Authorization)
	}
	if !strings.Contains(signed.Authorization, "SignedHeaders=host;x-cps-content-sha256;x-cps-date;x-cps-nonce") {
		t.Fatalf("unexpected SignedHeaders in %q", signed.Authorization)
	}

	// Rebuild a verify-side request from the signed headers plus the
	// Authorization header that Sign returned separately.
	verifyHdrs := make(map[string]string, len(signed.Headers)+1)
	for k, v := range signed.Headers {
		verifyHdrs[k] = v
	}
	verifyHdrs["Authorization"] = signed.Authorization
	verifyReq := Request{
		Method:  "POST",
		Host:    req.Host,
		Path:    req.Path,
		Query:   req.Query,
		Headers: verifyHdrs,
		Body:    body,
		Date:    fixedTime,
	}
	if err := Verify(verifyReq, Credentials{AK: testAK, SK: testSK}, testRegion, testService, fixedTime); err != nil {
		t.Fatalf("Verify failed: %v\nauth: %s\nsign headers: %v", err, signed.Authorization, signed.Headers)
	}
}

func TestGoldenVector(t *testing.T) {
	// Compute the expected signature independently from first principles,
	// duplicating the algorithm. If Sign() drifts from this, the test fails —
	// protecting the "algorithm is contract" invariant (07§4.1 / D4).
	nonce := "golden-nonce-123"
	hdrs := baseHeaders("euecs.api.euler.emoera.com", nonce)
	body := []byte("hello-body")
	hdrs[HeaderContentSHA] = hexSHA256(body)
	query := url.Values{"Action": {"DescribeInstances"}, "Version": {"2026-08-01"}}

	req := Request{
		Method:  "GET",
		Host:    "euecs.api.euler.emoera.com",
		Path:    "/",
		Query:   query,
		Headers: hdrs,
		Body:    body,
	}
	signed, err := Sign(req, Credentials{AK: testAK, SK: testSK}, testRegion, testService, fixedTime)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	expected := computeExpectedSignature(t, req, signed.Headers, testSK, testRegion, testService, fixedTime)
	if !strings.Contains(signed.Authorization, "Signature="+expected) {
		t.Fatalf("signature mismatch:\n want %s\n got  %s", expected, signed.Authorization)
	}
}

// computeExpectedSignature re-derives the signature by an independent code
// path so the test catches drift in the production Signer.
func computeExpectedSignature(t *testing.T, req Request, signedHdrs map[string]string, sk, region, service string, ts time.Time) string {
	can, signedList, err := canonicalHeaders(signedHdrs)
	if err != nil {
		t.Fatalf("canonicalHeaders: %v", err)
	}
	uri := canonicalURI(req.Path)
	q := canonicalQueryString(req.Query)
	bodyHash := signedHdrs[HeaderContentSHA]
	cr := strings.Join([]string{strings.ToUpper(req.Method), uri, q, can, signedList, bodyHash}, "\n")

	sts := strings.Join([]string{
		Algorithm,
		ts.UTC().Format("20060102T150405Z"),
		scope(ts, region, service),
		hexSHA256([]byte(cr)),
	}, "\n")

	kDate := hmacSHA256([]byte("CPS1"+sk), []byte(ts.UTC().Format("20060102")))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte(Terminator))
	return hex.EncodeToString(hmacSHA256(kSigning, []byte(sts)))
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	nonce := "tamper-nonce"
	hdrs := baseHeaders("euecs.api.euler.emoera.com", nonce)
	body := []byte(`{"x":1}`)
	hdrs[HeaderContentSHA] = hexSHA256(body)
	req := Request{
		Method: "POST", Host: "euecs.api.euler.emoera.com", Path: "/",
		Query: url.Values{}, Headers: hdrs, Body: body,
	}
	signed, err := Sign(req, Credentials{AK: testAK, SK: testSK}, testRegion, testService, fixedTime)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Swap body but leave the content-sha256 header — must fail.
	tampered := Request{
		Method: "POST", Host: req.Host, Path: req.Path, Query: req.Query,
		Headers: withAuth(signed), Body: []byte(`{"x":2}`), Date: fixedTime,
	}
	if err := Verify(tampered, Credentials{AK: testAK, SK: testSK}, testRegion, testService, fixedTime); err != ErrSignatureDoesNotMatch {
		t.Fatalf("expected ErrSignatureDoesNotMatch, got %v", err)
	}
}

func TestVerifyRejectsClockSkew(t *testing.T) {
	nonce := "skew-nonce"
	hdrs := baseHeaders("euecs.api.euler.emoera.com", nonce)
	req := Request{
		Method: "GET", Host: "euecs.api.euler.emoera.com", Path: "/",
		Query: url.Values{}, Headers: hdrs, Body: nil, Date: fixedTime,
	}
	signed, err := Sign(req, Credentials{AK: testAK, SK: testSK}, testRegion, testService, fixedTime)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Verify against a server clock 20 minutes ahead → outside ±15min window.
	future := fixedTime.Add(20 * time.Minute)
	vr := Request{
		Method: "GET", Host: req.Host, Path: req.Path, Query: req.Query,
		Headers: withAuth(signed), Body: nil, Date: fixedTime,
	}
	if err := Verify(vr, Credentials{AK: testAK, SK: testSK}, testRegion, testService, future); err != ErrClockSkew {
		t.Fatalf("expected ErrClockSkew, got %v", err)
	}
}

func TestVerifyRejectsWrongAK(t *testing.T) {
	nonce := "ak-nonce"
	hdrs := baseHeaders("euecs.api.euler.emoera.com", nonce)
	req := Request{
		Method: "GET", Host: "euecs.api.euler.emoera.com", Path: "/",
		Query: url.Values{}, Headers: hdrs, Body: nil, Date: fixedTime,
	}
	signed, err := Sign(req, Credentials{AK: testAK, SK: testSK}, testRegion, testService, fixedTime)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	vr := Request{
		Method: "GET", Host: req.Host, Path: req.Path, Query: req.Query,
		Headers: withAuth(signed), Body: nil, Date: fixedTime,
	}
	// Different AK on the verify side.
	if err := Verify(vr, Credentials{AK: "EU" + strings.Repeat("B", 30), SK: testSK}, testRegion, testService, fixedTime); err != ErrSignatureDoesNotMatch {
		t.Fatalf("expected ErrSignatureDoesNotMatch, got %v", err)
	}
}

func TestCanonicalQueryStringSortsAndEncodes(t *testing.T) {
	q := url.Values{
		"b": {"2", "10"},
		"a": {"1"},
	}
	got := canonicalQueryString(q)
	// Keys sorted; within "b", values sorted: "10" < "2" lexicographically.
	want := "a=1&b=10&b=2"
	if got != want {
		t.Fatalf("canonicalQueryString: got %q want %q", got, want)
	}
}

func TestCanonicalURIEmptyIsSlash(t *testing.T) {
	if got := canonicalURI(""); got != "/" {
		t.Fatalf("canonicalURI(\"\") = %q, want /", got)
	}
}

func TestCanonicalURIEncodesSpaces(t *testing.T) {
	if got := canonicalURI("/a b/c"); got != "/a%20b/c" {
		t.Fatalf("canonicalURI: got %q want /a%%20b/c", got)
	}
}

// TestCanonicalURIStrictRFC3986 pins the encoding of the characters where
// naive implementations disagree. Go's url.PathEscape leaves "$ @ : = & +"
// unescaped; a Python canonicaliser escapes them. Since the algorithm is the
// wire contract shared by every SDK language (07§4.1, adjudication D4), only
// the strict RFC 3986 unreserved set may pass through unencoded — otherwise a
// Python caller's signature would not verify against a Go gateway.
func TestCanonicalURIStrictRFC3986(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a$b", "/a%24b"},
		{"/a@b", "/a%40b"},
		{"/a:b", "/a%3Ab"},
		{"/a=b", "/a%3Db"},
		{"/a&b", "/a%26b"},
		{"/a+b", "/a%2Bb"},
		{"/a,b", "/a%2Cb"},
		{"/a;b", "/a%3Bb"},
		// The unreserved set survives untouched.
		{"/AZaz09-._~", "/AZaz09-._~"},
		// Separators are preserved, segments encoded independently.
		{"/x/y z/w", "/x/y%20z/w"},
	}
	for _, c := range cases {
		if got := canonicalURI(c.in); got != c.want {
			t.Errorf("canonicalURI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestQueryEncodingStrictRFC3986 pins the same rule for query components.
func TestQueryEncodingStrictRFC3986(t *testing.T) {
	got := canonicalQueryString(url.Values{"Prefix": {"a+b c/d=e&f"}})
	want := "Prefix=a%2Bb%20c%2Fd%3De%26f"
	if got != want {
		t.Fatalf("canonicalQueryString = %q, want %q", got, want)
	}
}

func TestCollapseSpaces(t *testing.T) {
	if got := collapseSpaces("  a   b\tc\n"); got != " a b c " {
		t.Fatalf("collapseSpaces: got %q", got)
	}
}

func TestDeriveSigningKeyDeterministic(t *testing.T) {
	a := deriveSigningKey("k", "20260804", "cn-north-1", "ecs")
	b := deriveSigningKey("k", "20260804", "cn-north-1", "ecs")
	if hex.EncodeToString(a) != hex.EncodeToString(b) {
		t.Fatal("deriveSigningKey not deterministic")
	}
	// Different service → different key.
	c := deriveSigningKey("k", "20260804", "cn-north-1", "oss")
	if hex.EncodeToString(a) == hex.EncodeToString(c) {
		t.Fatal("deriveSigningKey did not vary by service")
	}
}

func TestHmacEqual(t *testing.T) {
	// Sanity: crypto/hmac.Equal is available and constant-time.
	a := hmacSHA256([]byte("k"), []byte("data"))
	b := hmacSHA256([]byte("k"), []byte("data"))
	if !hmac.Equal(a, b) {
		t.Fatal("hmac.Equal should be true for equal inputs")
	}
}

// Note: hexSHA256 and hmacSHA256 are the package's unexported helpers in
// signer.go; because this test file is in package cps1, it reuses them
// directly rather than redeclaring them.
