// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RFC 9110 5.3: repeated field lines are one comma-separated list. A trusted
// proxy that adds its own X-Forwarded-For line (HAProxy option forwardfor)
// leaves the client's line first, so reading only that line hands a
// client-chosen address to the per-source Agent limits.
func TestRemoteIPReadsEveryForwardedForLine(t *testing.T) {
	x := newTestAPI(t)
	x.api.trustedProxies, _ = parseTrustedProxies([]string{"10.0.0.0/8"})
	remote := func(lines ...string) string {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.2.3:4000"
		for _, line := range lines {
			r.Header.Add("X-Forwarded-For", line)
		}
		return x.api.remoteIP(r).String()
	}
	cases := []struct {
		lines []string
		want  string
	}{
		// A single line behaves as before.
		{[]string{"198.51.100.8, 10.0.0.3"}, "198.51.100.8"},
		// The client forged the first line; the trusted proxy added the real peer.
		{[]string{"198.51.100.66", "203.0.113.7"}, "203.0.113.7"},
		// Trusted hops on a later line are skipped.
		{[]string{"198.51.100.66, 203.0.113.7", "10.9.9.9"}, "203.0.113.7"},
		// A malformed entry on any line ignores the chain in full.
		{[]string{"198.51.100.66", "garbage"}, "10.1.2.3"},
		// The 32-hop bound covers all lines together...
		{[]string{strings.Repeat("198.51.100.1,", 20) + "198.51.100.2", strings.Repeat("198.51.100.3,", 20) + "198.51.100.4"}, "10.1.2.3"},
		{[]string{strings.Repeat("198.51.100.1,", 32) + "198.51.100.2"}, "10.1.2.3"},
		// ...and 32 hops are still walked.
		{[]string{strings.Repeat("10.0.0.9,", 15) + "198.51.100.5", strings.Repeat("10.0.0.8,", 15) + "10.0.0.7"}, "198.51.100.5"},
	}
	for _, c := range cases {
		if got := remote(c.lines...); got != c.want {
			t.Errorf("remoteIP(%q) = %s, want %s", c.lines, got, c.want)
		}
	}
}

// Behind a trusted proxy that adds its own line, rotating a forged first line
// must not hand a client a fresh per-source enrollment budget.
func TestForgedForwardedForLineCannotDodgeAgentSourceLimits(t *testing.T) {
	x := newTestAPI(t)
	x.api.trustedProxies, _ = parseTrustedProxies([]string{"10.0.0.0/8"})
	handler := x.api.Handler()
	challenge := func(i int) (int, string) {
		r := httptest.NewRequest(http.MethodPost, "/agent/v1/enroll/challenge", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "10.0.0.2:41000"
		r.Header.Add("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i)) // written by the client
		r.Header.Add("X-Forwarded-For", "203.0.113.7")                   // added by the proxy
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	for i := 0; i < 120; i++ {
		if code, body := challenge(i); code != http.StatusUnauthorized {
			t.Fatalf("challenge %d: %d %s", i, code, body)
		}
	}
	if code, body := challenge(120); code != http.StatusTooManyRequests || !strings.Contains(body, "enrollment_rate_limited") {
		t.Fatalf("a forged first line escaped the per-source enrollment budget: %d %s", code, body)
	}
}
