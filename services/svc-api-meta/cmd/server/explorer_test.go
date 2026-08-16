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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/starcloud/sc-platform/cps1"
)

// golden-vector credentials (proto-hub/testdata/cps1-golden-vectors.json).
const (
	gAK      = "SCAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	gSK      = "test-secret-key-0123456789"
	gRegion  = "cn-north-1"
	gProduct = "scecs" // service namespace "ecs"
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
	req.Header.Set("X-Sc-Account-Id", "100123")
	req.Header.Set("X-Sc-TraceId", "test-trace")
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

	// Must not have executed a live call (no SC_EXPLORER_TARGET configured).
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
	// service namespace derived from scecs → ecs (golden-vector service).
	if sig.Service != "ecs" {
		t.Errorf("Service = %q, want ecs", sig.Service)
	}
	if sig.Host != "scecs.api.starcloud.cn" {
		t.Errorf("Host = %q, want scecs.api.starcloud.cn", sig.Host)
	}

	// The proof: the Authorization the Explorer produced must verify through
	// cps1.Verify. We reconstruct a cps1.Request from the signature material at
	// the signed date and confirm Verify accepts it.
	date, err := time.Parse("20060102T150405Z", sig.Date)
	if err != nil {
		t.Fatalf("parse date %q: %v", sig.Date, err)
	}
	hdrs := make(map[string]string, len(sig.Headers))
	for k, v := range sig.Headers {
		hdrs[k] = v
	}
	hdrs["Authorization"] = sig.Authorization
	if err := cps1.Verify(cps1.Request{
		Method:  sig.Method,
		Host:    sig.Host,
		Path:    sig.Path,
		Query:   toValues(sig.Query),
		Headers: hdrs,
		Body:    []byte(sig.Body),
	}, cps1.Credentials{AK: gAK, SK: gSK}, gRegion, "ecs", date); err != nil {
		t.Fatalf("Explorer signature did not verify via cps1.Verify: %v", err)
	}
}

// TestExplorerRejectsMissingCredentials asserts the Explorer does not sign with
// absent credentials — it returns 400 rather than producing a garbage signature.
func TestExplorerRejectsMissingCredentials(t *testing.T) {
	store := newMetaStore()
	h := http.HandlerFunc(store.handleExplorer)

	body, _ := json.Marshal(explorerRequest{ProductCode: gProduct, Method: "GET", Path: "/"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apimeta/explorer", bytes.NewReader(body))
	req.Header.Set("X-Sc-Account-Id", "100123")
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
		"scecs": "ecs",
		"scoss": "oss",
		"scvpc": "vpc",
		"scrds": "rds",
		"scmon": "mon",
		"sceip": "eip",
	}
	for product, want := range cases {
		if got := serviceNamespace(product); got != want {
			t.Errorf("serviceNamespace(%q) = %q, want %q", product, got, want)
		}
	}
}

func toValues(m map[string]string) (q map[string][]string) {
	q = make(map[string][]string, len(m))
	for k, v := range m {
		q[k] = []string{v}
	}
	return q
}
