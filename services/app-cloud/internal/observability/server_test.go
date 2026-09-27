package observability

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func testMonitor(c Config) (*Monitor, *http.ServeMux) {
	m := New(c)
	mux := http.NewServeMux()
	m.Register(mux)
	return m, mux
}
func TestReadinessIgnoresOptionalDependenciesAndFollowsCoreRecovery(t *testing.T) {
	var failed atomic.Bool
	m, mux := testMonitor(Config{CoreCheck: func(context.Context) error {
		if failed.Load() {
			return errors.New("private path secret")
		}
		return nil
	}, Checks: []Check{{Name: "s3", Probe: func(context.Context) string { return "not_configured" }}, {Name: "mysql", Probe: func(context.Context) string { return "down" }}}, Authorize: func(*http.Request) int { return 0 }})
	m.Refresh(context.Background())
	for _, state := range []bool{false, true, false} {
		failed.Store(state)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		want := 200
		if state {
			want = 503
		}
		if w.Code != want || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("readiness: %d %s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/ops/diagnostics", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"not_configured"`) || !strings.Contains(w.Body.String(), `"ready":true`) {
		t.Fatal(w.Body.String())
	}
	m.Stop()
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 503 {
		t.Fatal("shutdown still ready")
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Fatal("liveness incorrectly depends on readiness")
	}
}
func TestProtectedSignalsAndSafeRequestLogging(t *testing.T) {
	var logs bytes.Buffer
	m, mux := testMonitor(Config{CoreCheck: func(context.Context) error { return nil }, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Authorize: func(r *http.Request) int {
		if r.Header.Get("Authorization") == "Bearer private-ops-token" {
			return 0
		}
		return 401
	}, Checks: []Check{{Name: "bad_probe", Probe: func(context.Context) string { return "secret-driver-error" }}}})
	m.Refresh(context.Background())
	for _, path := range []string{"/ops/metrics", "/ops/diagnostics"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 || strings.Contains(w.Body.String(), "dependencies") {
			t.Fatalf("unprotected signals: %s", w.Body.String())
		}
	}
	ids := map[string]bool{}
	mux.HandleFunc("GET /public/{secret}", func(w http.ResponseWriter, r *http.Request) {
		if RequestID(r.Context()) != w.Header().Get("X-Request-ID") {
			t.Error("context request ID missing")
		}
		w.WriteHeader(204)
	})
	h := m.Middleware(mux)
	for _, path := range []string{"/public/invite-secret?code=oauth-secret&password=hidden", "/auth/callback?state=hidden&code=oauth-secret", "/api/private-record"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer business-secret")
		r.Header.Set("X-Request-ID", "attacker-secret")
		r.Header.Set("Cookie", "session=private-cookie")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		id := w.Header().Get("X-Request-ID")
		if len(id) != 32 || ids[id] {
			t.Fatal("invalid or reused correlation ID")
		}
		ids[id] = true
	}
	for _, secret := range []string{"invite-secret", "oauth-secret", "business-secret", "private-cookie", "attacker-secret", "private-record", "password", "hidden"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaked %s: %s", secret, logs.String())
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ops/metrics", nil)
	r.Header.Set("Authorization", "Bearer private-ops-token")
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `route="public"`) || strings.Contains(w.Body.String(), "private-record") || strings.Contains(w.Body.String(), "secret-driver-error") {
		t.Fatal(w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `name="bad_probe",state="down"`) {
		t.Fatal("probe state not canonicalized")
	}
}
func TestConcurrentMetricsAndPanicRedaction(t *testing.T) {
	var logs bytes.Buffer
	m, mux := testMonitor(Config{CoreCheck: func(context.Context) error { return nil }, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Authorize: func(*http.Request) int { return 0 }})
	mux.HandleFunc("GET /api/panic", func(http.ResponseWriter, *http.Request) { panic("private-database-password") })
	h := m.Middleware(mux)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				path := "/readyz"
				if i%2 == 0 {
					path = "/ops/metrics"
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				if w.Code != 200 {
					t.Error("request failed")
				}
			}
		}(i)
	}
	wg.Wait()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/panic", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "private-database-password") || strings.Contains(logs.String(), "private-database-password") {
		t.Fatal("unsafe panic response")
	}
	if m.inFlight.Load() != 0 {
		t.Fatal("inflight counter leaked")
	}
}
