// Package main: tests for the OpenAPI Explorer endpoint (M-5.1).
//
// The Explorer's central guarantee (03§9.4 rule ⑤) is that it does NOT
// reimplement signing — it calls pkg-go/cps1, the same implementation the SDK
// ships and the gateway verifies. These tests pin that: feeding the golden-vector
// inputs through the Explorer handler produces a Signature the gateway's own
// cps1.Verify accepts. If that ever breaks, someone has forked the signer.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/cps1"
)

// golden-vector credentials (proto-hub/testdata/cps1-golden-vectors.json).
const (
	gAK      = "EUAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	gSK      = "test-secret-key-0123456789"
	gRegion  = "cn-north-1"
	gProduct = "euecs" // service namespace "ecs"
)

// TestExplorerSignsAndVerifiesGoldenInputs feeds the Explorer a request with
// the golden-vector credentials and asserts the returned Authorization
// verifies through cps1.Verify — proving the Explorer signs with pkg-go/cps1,
// not a fork.
func TestExplorerSignsAndVerifiesGoldenInputs(t *testing.T) {
	store := newMetaStore()
	h := http.HandlerFunc(store.handleExplorer)

	body := explorerRequest{
		ProductCode: gProduct,
		ActionName:  "RunInstances",
		Method:      "POST",
		Path:        "/",
		Query:       map[string]string{"Action": "RunInstances", "Version": "2026-08-01"},
		Body:        `{"ImageId":"img-001","InstanceType":"s2.large"}`,
		AK:          gAK,
		SK:          gSK,
		Region:      gRegion,
	}
	raw, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apimeta/explorer", bytes.NewReader(raw))
	req.Header.Set("X-Euler-Account-Id", "100123")
	req.Header.Set("X-Euler-TraceId", "test-trace")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Code string           `json:"Code"`
		Data explorerResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != "OK" {
		t.Fatalf("envelope Code = %q, want OK", resp.Code)
	}

	// Must not have executed a live call (no EULER_EXPLORER_TARGET configured).
	if resp.Data.Executed {
		t.Fatal("Executed=true; Explorer made a live call with no target configured")
	}

	sig := resp.Data.Signature
	if !strings.HasPrefix(sig.Authorization, cps1.SchemePrefix) {
		t.Fatalf("Authorization %q missing %q prefix", sig.Authorization, cps1.SchemePrefix)
	}
	// SignedHeaders must include the three mandatory headers + nonce.
	for _, want := range []string{"host", "x-cps-content-sha256", "x-cps-date", "x-cps-nonce"} {
		if !strings.Contains(sig.SignedHeaders, want) {
			t.Errorf("SignedHeaders %q missing %q", sig.SignedHeaders, want)
		}
	}
	if sig.Signature == "" {
		t.Fatal("Signature is empty")
	}
	// service namespace derived from euecs → ecs (golden-vector service).
	if sig.Service != "ecs" {
		t.Errorf("Service = %q, want ecs", sig.Service)
	}
	if sig.Host != "euecs.api.euler.emoera.com" {
		t.Errorf("Host = %q, want euecs.api.euler.emoera.com", sig.Host)
	}

	// The proof: a live call through the Explorer must verify, at the receiving
	// end, through cps1.Verify — using only what arrived on the wire (Host,
	// headers including Authorization, path, query, body). Nothing is added by
	// the test: a request that left without Authorization, or with the dial
	// target as Host, fails here.
	var verifyErr error
	var gotHost, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdrs := map[string]string{"host": r.Host}
		for k := range r.Header {
			hdrs[strings.ToLower(k)] = r.Header.Get(k)
		}
		raw, _ := io.ReadAll(r.Body)
		gotHost, gotPath = r.Host, r.URL.Path
		verifyErr = cps1.Verify(cps1.Request{
			Method:  r.Method,
			Host:    r.Host,
			Path:    r.URL.Path, // the decoded path, as the gateway verifies it
			Query:   r.URL.Query(),
			Headers: hdrs,
			Body:    raw,
		}, cps1.Credentials{AK: gAK, SK: gSK}, gRegion, "ecs", time.Now())
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	t.Setenv("EULER_EXPLORER_TARGET", upstream.URL)

	body.Path = "/v1/a%20b"
	raw, _ = json.Marshal(body)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/apimeta/explorer", bytes.NewReader(raw))
	req.Header.Set("X-Euler-Account-Id", "100123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("live call: status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var live struct {
		Data explorerResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !live.Data.Executed || live.Data.Upstream == nil || live.Data.Upstream.Error != "" {
		t.Fatalf("live call not executed cleanly: %+v", live.Data.Upstream)
	}
	if gotHost != "euecs.api.euler.emoera.com" {
		t.Errorf("upstream Host = %q, want the signed routing host", gotHost)
	}
	if gotPath != "/v1/a b" {
		t.Errorf("upstream path = %q, want %q (escape sent once, not double-encoded)", gotPath, "/v1/a b")
	}
	if verifyErr != nil {
		t.Fatalf("the live request did not verify via cps1.Verify: %v", verifyErr)
	}
}

func TestValidExplorerPathPercentEscapes(t *testing.T) {
	for p, want := range map[string]bool{
		"/v1/a%20b": true,
		"/v1/%2Fx":  true,
		"/v1/%":     false,
		"/v1/%2":    false,
		"/v1/%zz":   false,
	} {
		if got := validExplorerPath(p); got != want {
			t.Errorf("validExplorerPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestExplorerRejectsMissingCredentials asserts the Explorer does not sign with
// absent credentials — it returns 400 rather than producing a garbage signature.
func TestExplorerRejectsMissingCredentials(t *testing.T) {
	store := newMetaStore()
	h := http.HandlerFunc(store.handleExplorer)

	body, _ := json.Marshal(explorerRequest{ProductCode: gProduct, Method: "GET", Path: "/"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apimeta/explorer", bytes.NewReader(body))
	req.Header.Set("X-Euler-Account-Id", "100123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestExplorerRejectsMissingAccountHeader asserts the gateway-injected account
// header is enforced (403), consistent with every other svc-api-meta handler.
func TestExplorerRejectsMissingAccountHeader(t *testing.T) {
	store := newMetaStore()
	h := http.HandlerFunc(store.handleExplorer)

	body, _ := json.Marshal(explorerRequest{ProductCode: gProduct, AK: gAK, SK: gSK})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apimeta/explorer", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// TestServiceNamespace covers the product→service derivation for phase-1 products.
func TestServiceNamespace(t *testing.T) {
	cases := map[string]string{
		"euecs": "ecs",
		"euoss": "oss",
		"euvpc": "vpc",
		"eurds": "rds",
		"eumon": "mon",
		"eueip": "eip",
	}
	for product, want := range cases {
		if got := serviceNamespace(product); got != want {
			t.Errorf("serviceNamespace(%q) = %q, want %q", product, got, want)
		}
	}
}
