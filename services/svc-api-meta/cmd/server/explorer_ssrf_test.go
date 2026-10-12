package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func explorerCall(t *testing.T, s *actionStore, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/v1/apimeta/explorer", bytes.NewReader(raw))
	req.Header.Set("X-Euler-Account-Id", "100123")
	rec := httptest.NewRecorder()
	s.handleExplorer(rec, req)
	return rec
}

// Regression: the user path was string-concatenated onto the upstream base,
// so "@host:port/..." redirected the signed live call to any host — and the
// caller could add X-Euler-Account-Id to impersonate any tenant there.
func TestExplorerCannotBeTurnedIntoSSRF(t *testing.T) {
	var hits []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path+"|"+r.Header.Get("X-Euler-Account-Id"))
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached a host other than the configured target: %s", r.URL)
	}))
	defer victim.Close()
	t.Setenv("EULER_EXPLORER_TARGET", upstream.URL)

	s := newMetaStore()
	victimHost := strings.TrimPrefix(victim.URL, "http://")
	for _, p := range []string{"@" + victimHost + "/x", "//" + victimHost + "/x", "/x\\@" + victimHost, "/x?y=1", "/x#f", "http://" + victimHost} {
		rec := explorerCall(t, s, map[string]any{"product_code": "euecs", "ak": "AK", "sk": "SK", "path": p})
		if rec.Code != 400 {
			t.Errorf("path %q: want 400, got %d %s", p, rec.Code, rec.Body.String())
		}
	}

	rec := explorerCall(t, s, map[string]any{
		"product_code": "euecs", "ak": "AK", "sk": "SK", "path": "/v1/instances",
		"headers": map[string]string{"X-Euler-Account-Id": "999", "X-Euler-Internal-Token": "x", "x-cps-trace": "ok"},
	})
	if rec.Code != 200 {
		t.Fatalf("normal call: %d %s", rec.Code, rec.Body.String())
	}
	if len(hits) != 1 || hits[0] != "/v1/instances|" {
		t.Fatalf("upstream saw %v (caller headers must not be forwarded)", hits)
	}
}

func TestExplorerNonceIsRandom(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	if a, b := explorerNonce(req), explorerNonce(req); a == b {
		t.Fatalf("nonce repeated: %s", a)
	}
}
