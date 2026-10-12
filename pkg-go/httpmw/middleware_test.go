package httpmw

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInternalTokenEnforcedExceptProbes(t *testing.T) {
	t.Setenv(InternalTokenEnv, "s3cret")
	h := InternalToken(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for path, hdr := range map[string]string{"/api/x": "", "/api/y": "wrong"} {
		r := httptest.NewRequest("GET", path, nil)
		if hdr != "" {
			r.Header.Set(InternalTokenHeader, hdr)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("%s with %q: want 403, got %d", path, hdr, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/api/x", nil)
	r.Header.Set(InternalTokenHeader, "s3cret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("valid token: got %d", w.Code)
	}
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 204 {
			t.Errorf("probe %s must not need the token, got %d", p, w.Code)
		}
	}
}

func TestSanitizeRequestID(t *testing.T) {
	for in, want := range map[string]string{
		"abc-123":                 "abc-123",
		"7f3c:aa.bb_cc":           "7f3c:aa.bb_cc",
		"":                        "",
		"x\r\nSet-Cookie: a=b":    "",
		`","Code":"OK`:            "",
		string(make([]byte, 200)): "",
	} {
		if got := SanitizeRequestID(in); got != want {
			t.Errorf("SanitizeRequestID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestIDReplacesMalformedInbound(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = RequestIDFromContext(r.Context()) }))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", `evil","Code":"OK`)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen == "" || seen == `evil","Code":"OK` {
		t.Fatalf("malformed inbound id must be replaced, got %q", seen)
	}
}
