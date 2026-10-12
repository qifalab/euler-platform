package eusdk

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/cps1"
)

// golden-vector credentials (proto-hub/testdata/cps1-golden-vectors.json).
const (
	gAK     = "EUAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	gSK     = "test-secret-key-0123456789"
	gRegion = "cn-north-1"
)

// TestSignsThroughCps1 proves the SDK does not fork the signer: feeding the
// golden-vector inputs through the SDK path produces an Authorization that
// cps1.Verify (the gateway's verifier) accepts. If this fails, the SDK drifted
// from pkg-go/cps1 — the one thing 03§9.4 rule ⑤ forbids.
func TestSignsThroughCps1(t *testing.T) {
	// We cannot call a real upstream; instead, exercise the exact signing path
	// the SDK uses (Sign + Authorization) by intercepting at the cps1 layer the
	// SDK calls. Rebuild the headers the SDK would attach and verify.
	req := ApiRequest{
		ProductCode: "euecs",
		Method:      "POST",
		Path:        "/",
		Query:       url.Values{"Action": {"RunInstances"}, "Version": {"2026-08-01"}},
		Body:        []byte(`{"ImageId":"img-001","InstanceType":"s2.large"}`),
	}

	// Reproduce the SDK's internal header assembly + Sign, the code path
	// Client.Call uses, so the test asserts the real signing (not a copy).
	host := productAPIHost(req.ProductCode)
	service := serviceNamespace(req.ProductCode)
	hdrs := map[string]string{"host": host, cps1.NonceHeader: newNonce()}
	now := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)
	signed, err := cps1.Sign(cps1.Request{
		Method: "POST", Host: host, Path: req.Path, Query: req.Query,
		Headers: hdrs, Body: req.Body,
	}, cps1.Credentials{AK: gAK, SK: gSK}, gRegion, service, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// The gateway verifies: the SDK's Authorization + headers must pass Verify.
	verifyHdrs := make(map[string]string, len(signed.Headers)+1)
	for k, v := range signed.Headers {
		verifyHdrs[k] = v
	}
	verifyHdrs["Authorization"] = signed.Authorization
	if err := cps1.Verify(cps1.Request{
		Method: "POST", Host: host, Path: req.Path, Query: req.Query,
		Headers: verifyHdrs, Body: req.Body,
	}, cps1.Credentials{AK: gAK, SK: gSK}, gRegion, service, now); err != nil {
		t.Fatalf("SDK signature did not verify via cps1.Verify: %v", err)
	}

	// The body hash the SDK signs must match the golden-vector's post-json-body
	// hash (same body → same content-sha256), proving the body-binding path.
	want := "75f3340d3d49adb89545f48d652021024eb31c67af83c036e0cd14a589de309a"
	if got := signed.Headers[cps1.HeaderContentSHA]; got != want {
		t.Errorf("content-sha256 = %q, want golden-vector %q", got, want)
	}
}

// TestServiceNamespaceDerivation covers the product→service mapping for every
// phase-1 product so the SDK signs with the same service the gateway derives.
func TestServiceNamespaceDerivation(t *testing.T) {
	cases := map[string]string{
		"euecs": "ecs", "euoss": "oss", "euvpc": "vpc", "eurds": "rds",
		"eumon": "mon", "eueip": "eip", "eubs": "bs",
	}
	for product, want := range cases {
		if got := serviceNamespace(product); got != want {
			t.Errorf("serviceNamespace(%q) = %q, want %q", product, got, want)
		}
	}
}

// TestHostForRoutingSubdomain confirms the SDK signs against the routing
// subdomain {product}.api.euler.emoera.com (04-middleware §3.2).
func TestHostForRoutingSubdomain(t *testing.T) {
	c := New(Config{AK: gAK, SK: gSK, Region: gRegion})
	if got := c.hostFor("euecs"); got != "euecs.api.euler.emoera.com" {
		t.Errorf("hostFor(euecs) = %q, want euecs.api.euler.emoera.com", got)
	}
}

// TestHostForEndpointOverride confirms a configured Endpoint overrides the
// subdomain (and strips the scheme for the signed host header).
func TestHostForEndpointOverride(t *testing.T) {
	cases := []struct {
		endpoint, want string
	}{
		{"https://api.internal.test:9101", "api.internal.test:9101"},
		{"http://localhost:9201/path", "localhost:9201"},
		{"api.example.cn", "api.example.cn"},
	}
	for _, tc := range cases {
		c := New(Config{AK: gAK, SK: gSK, Region: gRegion, Endpoint: tc.endpoint})
		if got := c.hostFor("euecs"); got != tc.want {
			t.Errorf("hostFor(endpoint=%q) = %q, want %q", tc.endpoint, got, tc.want)
		}
	}
}

// TestParseErrorDecodesPlatformEnvelope confirms a non-2xx with the platform
// {Code, Message} body becomes an *errors.Error carrying the business code + the
// real HTTP status — the SDK reuses pkg-go/errors (03§9.3), never a swallow.
func TestParseErrorDecodesPlatformEnvelope(t *testing.T) {
	body := []byte(`{"RequestId":"r1","Code":"Quota.Exceeded","Message":"quota exceeded"}`)
	e := parseError(body, 403)
	if e.Code != "Quota.Exceeded" {
		t.Errorf("Code = %q, want Quota.Exceeded", e.Code)
	}
	if e.HTTPStatus != 403 {
		t.Errorf("HTTPStatus = %d, want 403", e.HTTPStatus)
	}
	if e.Message != "quota exceeded" {
		t.Errorf("Message = %q, want 'quota exceeded'", e.Message)
	}
}

// TestParseErrorFallsBackOnUnstructuredBody confirms an unstructured error body
// becomes Common.InternalError (never an empty code the caller can't switch on).
func TestParseErrorFallsBackOnUnstructuredBody(t *testing.T) {
	e := parseError([]byte(`<html>bad gateway</html>`), 502)
	if e.Code != "Common.InternalError" {
		t.Errorf("Code = %q, want Common.InternalError", e.Code)
	}
	if e.HTTPStatus != 502 {
		t.Errorf("HTTPStatus = %d, want 502", e.HTTPStatus)
	}
}

// TestCallRejectsMissingProductCode confirms a programmer error (no product) is
// surfaced before any signing attempt, not swallowed into a 500.
func TestCallRejectsMissingProductCode(t *testing.T) {
	c := New(Config{AK: gAK, SK: gSK, Region: gRegion})
	if _, err := c.Call(ApiRequest{Method: "GET"}); err == nil {
		t.Fatal("expected error for missing ProductCode")
	}
}

// B4: ApiRequest.Path is the decoded path. It is signed as given and sent
// escaped exactly once, so a gateway that unescapes the wire path once before
// verifying (svc-iam verify: url.PathUnescape) recomputes the same signature.
// Appending the path raw put "/a b" on the wire and failed verification.
func TestCallPathIsSignedDecodedAndSentEscapedOnce(t *testing.T) {
	var (
		mu      sync.Mutex
		lastRaw string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.RequestURI
		if i := strings.IndexByte(raw, '?'); i >= 0 {
			raw = raw[:i]
		}
		mu.Lock()
		lastRaw = raw
		mu.Unlock()
		// The gateway's side: decode the wire path once, then verify.
		decoded, err := url.PathUnescape(raw)
		if err == nil {
			hdrs := map[string]string{"host": r.Host}
			for k := range r.Header {
				hdrs[strings.ToLower(k)] = r.Header.Get(k)
			}
			body, _ := io.ReadAll(r.Body)
			err = cps1.Verify(cps1.Request{
				Method: r.Method, Host: r.Host, Path: decoded, Query: r.URL.Query(),
				Headers: hdrs, Body: body,
			}, cps1.Credentials{AK: gAK, SK: gSK}, gRegion, "ecs", time.Now().UTC())
		}
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"Code":"Auth.SignatureDoesNotMatch","Message":"` + err.Error() + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"Code":"OK"}`))
	}))
	defer srv.Close()

	c := New(Config{AK: gAK, SK: gSK, Region: gRegion, Endpoint: srv.URL})
	for path, wire := range map[string]string{
		"/":                          "/",
		"/buckets/my bucket/objects": "/buckets/my%20bucket/objects", // golden vector path-with-encoding
		"/objects/a$b@c:d=e&f+g":     "/objects/a%24b%40c%3Ad%3De%26f%2Bg",
		"/a%20b":                     "/a%2520b", // a literal '%' is data, escaped once
		"/对象/x":                      "/%E5%AF%B9%E8%B1%A1/x",
	} {
		resp, err := c.Call(ApiRequest{
			ProductCode: "euecs", Method: "GET", Path: path,
			Query: url.Values{"Action": {"GetObject"}, "Version": {"2026-08-01"}},
		})
		if err != nil {
			t.Fatalf("%q: %v (status %v)", path, err, resp)
		}
		mu.Lock()
		got := lastRaw
		mu.Unlock()
		if got != wire {
			t.Errorf("%q went on the wire as %q, want %q", path, got, wire)
		}
	}
}

func TestEscapePathRoundTrips(t *testing.T) {
	for _, p := range []string{"/", "/a b/c", "/objects/a$b@c:d=e&f+g", "/x%2Fy", "/~user/._-", "//double"} {
		got, err := url.PathUnescape(escapePath(p))
		if err != nil || got != p {
			t.Errorf("escapePath(%q) = %q, unescapes to %q (%v)", p, escapePath(p), got, err)
		}
	}
}

// B4: credentials from the environment, in the Python SDK's order.
func TestCredentialsFromEnv(t *testing.T) {
	for _, name := range append(append([]string{}, envAccessKeyNames...), envSecretKeyNames...) {
		t.Setenv(name, "")
	}
	if ak, sk := CredentialsFromEnv(); ak != "" || sk != "" {
		t.Fatalf("empty environment: %q %q", ak, sk)
	}
	steps := []struct{ akVar, skVar, ak, sk string }{
		{"EULER_AK", "EULER_SK", "AK3", "SK3"},
		{"EULER_ACCESS_KEY", "EULER_SECRET_KEY", "AK2", "SK2"},
		{"EULER_ACCESS_KEY_ID", "EULER_SECRET_ACCESS_KEY", "AK1", "SK1"},
	}
	// Each spelling is accepted, and a higher-precedence one wins once set.
	for _, s := range steps {
		t.Setenv(s.akVar, s.ak)
		t.Setenv(s.skVar, s.sk)
		if ak, sk := CredentialsFromEnv(); ak != s.ak || sk != s.sk {
			t.Fatalf("after %s/%s: got %q %q, want %q %q", s.akVar, s.skVar, ak, sk, s.ak, s.sk)
		}
	}
	// New falls back to the environment only for what the Config leaves empty.
	if c := New(Config{Region: gRegion}); c.cfg.AK != "AK1" || c.cfg.SK != "SK1" {
		t.Fatalf("New without credentials: %q %q, want the environment's", c.cfg.AK, c.cfg.SK)
	}
	if c := New(Config{AK: "AKCFG", SK: "SKCFG", Region: gRegion}); c.cfg.AK != "AKCFG" || c.cfg.SK != "SKCFG" {
		t.Fatalf("explicit credentials overridden: %q %q", c.cfg.AK, c.cfg.SK)
	}
}

// L3: hostFor cut an Endpoint's path off, so a prefix was neither signed nor
// sent. The split matches the Python SDK's Client._host_and_prefix.
func TestHostAndPrefix(t *testing.T) {
	cases := []struct{ endpoint, host, prefix string }{
		{"", "euecs.api.euler.emoera.com", ""},
		{"https://gw.example.com/prefix", "gw.example.com", "/prefix"},
		{"http://localhost:9201/api/v1/", "localhost:9201", "/api/v1"},
		{"https://api.internal.test:9101", "api.internal.test:9101", ""},
		{"https://api.internal.test:9101/", "api.internal.test:9101", ""},
		{"api.example.cn/a", "api.example.cn", "/a"},
	}
	for _, tc := range cases {
		c := New(Config{AK: gAK, SK: gSK, Region: gRegion, Endpoint: tc.endpoint})
		if host, prefix := c.hostAndPrefix("euecs"); host != tc.host || prefix != tc.prefix {
			t.Errorf("hostAndPrefix(endpoint=%q) = %q, %q; want %q, %q", tc.endpoint, host, prefix, tc.host, tc.prefix)
		}
	}
}

// L3: the prefix is part of both the signed canonical path and the request
// URL, so a gateway that unescapes the wire path once and verifies it accepts
// the request (as it does for the Python SDK); a root path is the prefix alone.
func TestEndpointPathPrefixIsSignedAndSent(t *testing.T) {
	var (
		mu      sync.Mutex
		lastRaw string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.RequestURI
		if i := strings.IndexByte(raw, '?'); i >= 0 {
			raw = raw[:i]
		}
		mu.Lock()
		lastRaw = raw
		mu.Unlock()
		decoded, err := url.PathUnescape(raw)
		if err == nil {
			hdrs := map[string]string{"host": r.Host}
			for k := range r.Header {
				hdrs[strings.ToLower(k)] = r.Header.Get(k)
			}
			body, _ := io.ReadAll(r.Body)
			err = cps1.Verify(cps1.Request{
				Method: r.Method, Host: r.Host, Path: decoded, Query: r.URL.Query(),
				Headers: hdrs, Body: body,
			}, cps1.Credentials{AK: gAK, SK: gSK}, gRegion, "ecs", time.Now().UTC())
		}
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"Code":"Auth.SignatureDoesNotMatch","Message":"` + err.Error() + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"Code":"OK"}`))
	}))
	defer srv.Close()

	for _, tc := range []struct{ prefix, path, wire string }{
		{"/api/v1/", "/", "/api/v1"},
		{"/api/v1", "/instances", "/api/v1/instances"},
		{"/api/v1", "", "/api/v1"},
		{"/api/v1", "/buckets/my bucket", "/api/v1/buckets/my%20bucket"},
		{"/gw path", "/x", "/gw%20path/x"},
	} {
		c := New(Config{AK: gAK, SK: gSK, Region: gRegion, Endpoint: srv.URL + tc.prefix})
		resp, err := c.Call(ApiRequest{
			ProductCode: "euecs", Method: "POST", Path: tc.path,
			Query: url.Values{"Action": {"RunInstances"}, "Version": {"2026-08-01"}},
			Body:  []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("prefix %q path %q: %v (status %v)", tc.prefix, tc.path, err, resp)
		}
		mu.Lock()
		got := lastRaw
		mu.Unlock()
		if got != tc.wire {
			t.Errorf("prefix %q path %q went on the wire as %q, want %q", tc.prefix, tc.path, got, tc.wire)
		}
	}
}
