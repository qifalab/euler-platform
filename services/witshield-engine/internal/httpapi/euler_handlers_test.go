// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// discardResponse is reused across requests so that only the handler's own
// allocations are counted.
type discardResponse struct {
	header http.Header
	status int
}

func (d *discardResponse) Header() http.Header         { return d.header }
func (d *discardResponse) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardResponse) WriteHeader(status int)      { d.status = status }

// Every Euler management and Agent request goes through these entrypoints. The
// 44-route management mux and the 8-route Agent mux are built once per Server,
// so a request only pays for dispatch. Registering a route costs about 15
// allocations (pattern parsing, routing tree, handler values): with Go 1.27 a
// request allocates 59 (management) and 34 (Agent) objects, against 698 and
// 172 when each request rebuilt its mux. The bounds sit between the two.
func TestEulerHandlersAreBuiltOncePerServer(t *testing.T) {
	x := newTestAPI(t)
	w := &discardResponse{header: http.Header{}}
	// Unregistered paths reach the mux and stop there: no store or body work
	// is measured, only what each request costs before a handler runs.
	management := httptest.NewRequest(http.MethodGet, "/api/v1/dispatch-probe", nil)
	agent := httptest.NewRequest(http.MethodGet, "/agent/v1/dispatch-probe", nil)
	cases := []struct {
		name  string
		limit float64
		serve func()
	}{
		{"management", 200, func() { x.api.EulerManagementHandler("euler_actor").ServeHTTP(w, management) }},
		{"agent", 80, func() {
			x.api.EulerAgentHandler("/public/witshield/t/p/i/agent/v1/dispatch-probe").ServeHTTP(w, agent)
		}},
	}
	for _, c := range cases {
		w.status = 0
		allocs := testing.AllocsPerRun(100, c.serve)
		if w.status != http.StatusNotFound {
			t.Fatalf("%s probe status %d, want the mux's 404", c.name, w.status)
		}
		t.Logf("%s: %.0f allocations per request", c.name, allocs)
		if allocs > c.limit {
			t.Errorf("%s request allocates %.0f objects (limit %.0f): is the mux rebuilt per request?", c.name, allocs, c.limit)
		}
	}
}
