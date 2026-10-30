package appkit

import (
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// RFC 9110 5.3: repeated field lines are one comma-separated list. Proxies such
// as HAProxy (option forwardfor) add their own X-Forwarded-For line after the
// client's instead of appending to it, so only reading the first line hands the
// client-chosen value to the rate and risk rules.
func TestClientIPReadsEveryForwardedForLine(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	request := func(lines ...string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "10.1.2.3:4000"
		for _, line := range lines {
			r.Header.Add("X-Forwarded-For", line)
		}
		return ClientIP(r, trusted)
	}
	cases := []struct {
		lines []string
		want  string
	}{
		// The client forged the first line; the trusted proxy added the real peer.
		{[]string{"198.51.100.66", "203.0.113.7"}, "203.0.113.7"},
		// Further trusted hops on a later line are skipped as before.
		{[]string{"198.51.100.66, 203.0.113.7", "10.9.9.9"}, "203.0.113.7"},
		// A malformed later line keeps the peer, as a malformed single line does.
		{[]string{"198.51.100.66", "garbage"}, "10.1.2.3"},
		// The 32-hop bound covers all lines together.
		{[]string{strings.Repeat("198.51.100.1,", 20) + "198.51.100.2", strings.Repeat("198.51.100.3,", 20) + "198.51.100.4"}, "10.1.2.3"},
	}
	for _, c := range cases {
		if got := request(c.lines...); got != c.want {
			t.Errorf("ClientIP(%q) = %q, want %q", c.lines, got, c.want)
		}
	}
}
