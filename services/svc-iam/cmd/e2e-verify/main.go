// Command e2e-verify exercises the svc-iam verification endpoint over real
// HTTP: it provisions an AK/SK, signs a request with the shared CPS1 library,
// posts it to the forward-auth endpoint, and asserts the outcomes.
//
// This is the wire-level proof that the chain holds together —
// sign → HTTP → AK resolve → KMS decrypt → HMAC recompute → nonce dedup —
// rather than only the in-process unit tests.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/starcloud/sc-platform/cps1"
)

const (
	endpoint = "http://127.0.0.1:9101/internal/openapi/verify"
	region   = "cn-north-1"
	service  = "ecs"
	host     = "scecs.api.starcloud.cn"
)

type verifyRequest struct {
	Method  string            `json:"method"`
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Region  string            `json:"region"`
	Service string            `json:"service"`
}

func main() {
	ak := os.Getenv("SC_AK")
	sk := os.Getenv("SC_SK")
	if ak == "" || sk == "" {
		fmt.Println("SC_AK and SC_SK must be set (printed by the server on boot)")
		os.Exit(1)
	}

	failures := 0
	check := func(name string, wantStatus int, gotStatus int, body string) {
		if gotStatus == wantStatus {
			fmt.Printf("  PASS  %-38s → %d\n", name, gotStatus)
			return
		}
		failures++
		fmt.Printf("  FAIL  %-38s → got %d want %d  %s\n", name, gotStatus, wantStatus, body)
	}

	now := time.Now().UTC()

	// 1. Valid signed request → 200.
	st, body := post(sign(ak, sk, "e2e-nonce-1", []byte(`{"InstanceType":"s2.large"}`), now))
	check("valid signature", http.StatusOK, st, body)

	// 2. Replay the same nonce → 403 SignatureNonceUsed.
	st, body = post(sign(ak, sk, "e2e-nonce-1", []byte(`{"InstanceType":"s2.large"}`), now))
	check("replayed nonce rejected", http.StatusForbidden, st, body)

	// 3. Tampered body → 403 SignatureDoesNotMatch.
	req := sign(ak, sk, "e2e-nonce-2", []byte(`{"amount":1}`), now)
	req.Body = `{"amount":99999}`
	st, body = post(req)
	check("tampered body rejected", http.StatusForbidden, st, body)

	// 4. Clock skew beyond ±15 min → 403 RequestTimeTooSkewed.
	st, body = post(sign(ak, sk, "e2e-nonce-3", nil, now.Add(-30*time.Minute)))
	check("skewed clock rejected", http.StatusForbidden, st, body)

	// 5. Wrong SK → 403 SignatureDoesNotMatch.
	st, body = post(sign(ak, "wrong-secret-key", "e2e-nonce-4", nil, now))
	check("wrong SK rejected", http.StatusForbidden, st, body)

	// 6. Scope mismatch: signed for ecs, presented on the oss route.
	r := sign(ak, sk, "e2e-nonce-5", nil, now)
	r.Service = "oss"
	st, body = post(r)
	check("cross-service scope rejected", http.StatusForbidden, st, body)

	// 7. A fresh nonce still works (the store is not wedged by failures).
	st, body = post(sign(ak, sk, "e2e-nonce-6", nil, now))
	check("fresh nonce accepted", http.StatusOK, st, body)

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d check(s) FAILED\n", failures)
		os.Exit(1)
	}
	fmt.Println("all end-to-end checks passed")
}

// sign builds a fully signed forward-auth payload.
func sign(ak, sk, nonce string, body []byte, at time.Time) verifyRequest {
	hdrs := map[string]string{
		"host":                host,
		cps1.HeaderDate:       at.UTC().Format("20060102T150405Z"),
		cps1.HeaderContentSHA: "",
		cps1.NonceHeader:      nonce,
	}
	query := url.Values{"Action": {"RunInstances"}, "Version": {"2026-08-01"}}

	signed, err := cps1.Sign(cps1.Request{
		Method:  "POST",
		Host:    host,
		Path:    "/",
		Query:   query,
		Headers: hdrs,
		Body:    body,
	}, cps1.Credentials{AK: ak, SK: sk}, region, service, at)
	if err != nil {
		fmt.Println("sign error:", err)
		os.Exit(1)
	}

	out := make(map[string]string, len(signed.Headers)+1)
	for k, v := range signed.Headers {
		out[k] = v
	}
	out["Authorization"] = signed.Authorization

	return verifyRequest{
		Method:  "POST",
		Host:    host,
		Path:    "/",
		Query:   map[string]string{"Action": "RunInstances", "Version": "2026-08-01"},
		Headers: out,
		Body:    string(body),
		Region:  region,
		Service: service,
	}
}

func post(vr verifyRequest) (int, string) {
	payload, _ := json.Marshal(vr)
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Println("post error:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
