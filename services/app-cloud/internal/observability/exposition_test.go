package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Prometheus text format requires all samples of one metric family to form
// a single group directly after its TYPE line, and only one TYPE line per name.
func TestMetricsExpositionGroupsEachFamily(t *testing.T) {
	m, mux := testMonitor(Config{CoreCheck: func(context.Context) error { return nil }, Authorize: func(*http.Request) int { return 0 }, Checks: []Check{{Name: "s3", Probe: func(context.Context) string { return "up" }}}})
	m.Refresh(context.Background())
	mux.HandleFunc("GET /api/item", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := m.Middleware(mux)
	for _, path := range []string{"/readyz", "/api/item", "/api/missing"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/ops/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("metrics: %d", w.Code)
	}
	typed := map[string]bool{}
	samples := map[string]int{}
	current := ""
	for _, line := range strings.Split(strings.TrimSpace(w.Body.String()), "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			fields := strings.Fields(line)
			if len(fields) != 4 {
				t.Fatalf("malformed TYPE line %q", line)
			}
			if typed[fields[2]] {
				t.Fatalf("second TYPE line for %s", fields[2])
			}
			typed[fields[2]] = true
			current = fields[2]
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		if name != current {
			t.Fatalf("sample %q is outside its family group (current family %q)", line, current)
		}
		samples[name]++
	}
	for _, name := range []string{"euler_http_requests_total", "euler_http_duration_seconds_sum"} {
		if samples[name] != 3 {
			t.Fatalf("%s has %d samples, want one per method/route/status (3)", name, samples[name])
		}
	}
	for _, want := range []string{`euler_http_requests_total{method="GET",route="api",status="404"} 1`, `euler_http_requests_total{method="GET",route="probe",status="200"} 1`, `euler_dependency_state{name="s3",state="up"} 1`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s in\n%s", want, w.Body.String())
		}
	}
}
