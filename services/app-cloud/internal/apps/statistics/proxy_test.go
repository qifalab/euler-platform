package statistics

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// Behind the bundled reverse proxy every request arrives from the proxy. With
// the proxy listed as trusted, the per-IP collection limit applies per visitor
// instead of throttling the whole site as one address.
func TestCollectionLimitPerVisitorBehindTrustedProxy(t *testing.T) {
	m, scope := fixture(t)
	m.trusted = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	s := createSite(t, m, scope)
	send := func(peer, forwarded string) int {
		t.Helper()
		r := httptest.NewRequest("POST", "/sites/"+s.PublicID+"/collect", strings.NewReader(`{"visitor_id":"visitor-001","url":"https://docs.example/a"}`))
		r.RemoteAddr = peer
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://docs.example")
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		w := httptest.NewRecorder()
		m.PublicHandler().ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 120; i++ {
		if code := send("10.0.0.2:4000", "198.51.100.7"); code != 202 {
			t.Fatalf("visitor A request %d: status %d", i, code)
		}
	}
	if code := send("10.0.0.2:4000", "198.51.100.7"); code != 429 {
		t.Fatalf("visitor A not limited: %d", code)
	}
	if code := send("10.0.0.2:4000", "198.51.100.8"); code != 202 {
		t.Fatalf("visitor B shares visitor A's limit behind the proxy: %d", code)
	}
	// Without trusted proxies the forwarded header is ignored, as before.
	m.trusted = nil
	if code := send("192.0.2.9:4000", "198.51.100.7"); code != 202 {
		t.Fatalf("untrusted forwarding was believed: %d", code)
	}
}

func TestTrustedProxyDefaultRouteRefused(t *testing.T) {
	t.Setenv(appkit.TrustedProxiesEnv, "0.0.0.0/0")
	if e := New(&appkit.Runtime{}).Migrate(context.Background()); e == nil {
		t.Fatal("default route accepted as trusted proxy")
	}
}
