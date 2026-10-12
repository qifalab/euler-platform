package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Regression (I1): the e2e client posted to :9101 (web-auth, which has no
// verify route) and never sent the internal token, so against a production-
// configured verifier every check failed.
func TestEndpointDefaultsToVerifyPort(t *testing.T) {
	t.Setenv("EULER_VERIFY_ENDPOINT", "")
	if got := endpoint(); got != "http://127.0.0.1:9100/internal/openapi/verify" {
		t.Fatalf("default endpoint %q", got)
	}
	t.Setenv("EULER_VERIFY_ENDPOINT", "http://svc-iam.euler-svc-iam.svc:9100/internal/openapi/verify")
	if got := endpoint(); got != "http://svc-iam.euler-svc-iam.svc:9100/internal/openapi/verify" {
		t.Fatalf("override ignored: %q", got)
	}
}

func TestPostSendsInternalTokenAndBodyB64(t *testing.T) {
	var gotToken string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Euler-Internal-Token")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("EULER_VERIFY_ENDPOINT", srv.URL+"/internal/openapi/verify")
	t.Setenv("EULER_INTERNAL_TOKEN", "s3cret")

	body := []byte{0x00, 0xff, '{'}
	if st, _ := post(sign("EUAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "sk", "n-1", body, time.Now())); st != http.StatusOK {
		t.Fatalf("status %d", st)
	}
	if gotToken != "s3cret" {
		t.Fatalf("X-Euler-Internal-Token %q", gotToken)
	}
	if got["body_b64"] != base64.StdEncoding.EncodeToString(body) {
		t.Fatalf("body_b64 = %v", got["body_b64"])
	}
}
