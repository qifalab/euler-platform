package appkit

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
)

// TrustedProxiesEnv names the reverse proxies (comma-separated CIDRs) whose
// X-Forwarded-For entries may identify the browser for public risk and rate
// rules. WeAuth introduced the setting; Statistics and Lottery read the same
// list so every public endpoint sees the same client address behind a proxy.
const TrustedProxiesEnv = "EULER_WEAUTH_TRUSTED_PROXIES"

// ParseTrustedProxies parses a comma-separated CIDR list. A default route
// (0.0.0.0/0 or ::/0) would trust every client to choose its own address,
// so it is refused rather than silently accepted.
func ParseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		p, e := netip.ParsePrefix(value)
		if e != nil {
			return nil, e
		}
		if p.Bits() == 0 {
			return nil, errors.New(TrustedProxiesEnv + " must list the actual proxy networks, not 0.0.0.0/0 or ::/0")
		}
		out = append(out, p)
	}
	return out, nil
}

// TrustedProxiesFromEnv reads TrustedProxiesEnv.
func TrustedProxiesFromEnv() ([]netip.Prefix, error) {
	return ParseTrustedProxies(os.Getenv(TrustedProxiesEnv))
}

// ClientIP returns the TCP peer address. Only when the peer is a trusted
// proxy does it walk X-Forwarded-For from the right to the first address that
// is not a trusted proxy. Malformed or overlong forwarding keeps the peer.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	ip, e := netip.ParseAddr(host)
	if e != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	if !trustedProxy(ip, trusted) {
		return ip.String()
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(parts) > 32 {
		return ip.String()
	}
	for i := len(parts) - 1; i >= 0; i-- {
		next, e := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if e != nil {
			return ip.String()
		}
		ip = next.Unmap()
		if !trustedProxy(ip, trusted) {
			break
		}
	}
	return ip.String()
}

func trustedProxy(ip netip.Addr, trusted []netip.Prefix) bool {
	for _, prefix := range trusted {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}
