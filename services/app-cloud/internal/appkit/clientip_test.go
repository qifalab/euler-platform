package appkit

import (
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	got, e := ParseTrustedProxies(" 10.0.0.0/8, ,fd00::/8,127.0.0.1/32 ")
	if e != nil || len(got) != 3 {
		t.Fatalf("valid list: %v %v", got, e)
	}
	if got, e = ParseTrustedProxies(""); e != nil || got != nil {
		t.Fatalf("empty list: %v %v", got, e)
	}
	for _, bad := range []string{"0.0.0.0/0", "::/0", "10.0.0.0/8,0.0.0.0/0", "10.0.0.1", "not-a-cidr"} {
		if _, e := ParseTrustedProxies(bad); e == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestClientIPOnlyBelievesTrustedProxies(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	request := func(peer, forwarded string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = peer
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		return ClientIP(r, trusted)
	}
	cases := []struct{ peer, forwarded, want string }{
		// Untrusted peer: the forwarded header is ignored.
		{"203.0.113.9:1", "198.51.100.1", "203.0.113.9"},
		// Trusted peer: stop at the nearest untrusted hop.
		{"10.1.2.3:4000", "198.51.100.1, 203.0.113.7", "203.0.113.7"},
		// Walk across further trusted hops.
		{"10.1.2.3:4000", "198.51.100.1, 10.9.9.9", "198.51.100.1"},
		// Malformed or missing forwarding keeps the peer.
		{"10.1.2.3:4000", "garbage", "10.1.2.3"},
		{"10.1.2.3:4000", "", "10.1.2.3"},
		// IPv4-mapped IPv6 is unmapped on both sides.
		{"[::ffff:10.1.2.3]:4000", "::ffff:198.51.100.2", "198.51.100.2"},
		// Overlong chains keep the peer.
		{"10.1.2.3:4000", strings.Repeat("198.51.100.1,", 40) + "198.51.100.2", "10.1.2.3"},
		{"not-an-address", "198.51.100.1", "unknown"},
	}
	for _, c := range cases {
		if got := request(c.peer, c.forwarded); got != c.want {
			t.Errorf("ClientIP(%q, %q) = %q, want %q", c.peer, c.forwarded, got, c.want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.2.3:4000"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := ClientIP(r, nil); got != "10.1.2.3" {
		t.Fatalf("no trusted proxies must ignore forwarding, got %q", got)
	}
}
