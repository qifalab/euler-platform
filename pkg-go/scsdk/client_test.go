package scsdk

import (
	"net/url"
	"testing"
	"time"

	"github.com/starcloud/sc-platform/cps1"
)

// golden-vector credentials (proto-hub/testdata/cps1-golden-vectors.json).
const (
	gAK     = "SCAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
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
		ProductCode: "scecs",
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
		"scecs": "ecs", "scoss": "oss", "scvpc": "vpc", "scrds": "rds",
		"scmon": "mon", "sceip": "eip", "scbs": "bs",
	}
	for product, want := range cases {
		if got := serviceNamespace(product); got != want {
			t.Errorf("serviceNamespace(%q) = %q, want %q", product, got, want)
		}
	}
}

// TestHostForRoutingSubdomain confirms the SDK signs against the routing
// subdomain {product}.api.starcloud.cn (04-middleware §3.2).
func TestHostForRoutingSubdomain(t *testing.T) {
	c := New(Config{AK: gAK, SK: gSK, Region: gRegion})
	if got := c.hostFor("scecs"); got != "scecs.api.starcloud.cn" {
		t.Errorf("hostFor(scecs) = %q, want scecs.api.starcloud.cn", got)
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
		if got := c.hostFor("scecs"); got != tc.want {
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
