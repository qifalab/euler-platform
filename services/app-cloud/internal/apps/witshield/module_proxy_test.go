package witshield

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// The bundled nginx connects from the Compose network and sets
// X-Forwarded-For to the Agent's address (deploy/app-cloud/proxy.conf).
const composeProxy = "10.0.0.2:41000"

// challenge sends one enrollment challenge for installation i_a through the
// platform's public mount. An empty body is refused with 401 after the
// engine's per-source budget (120 per 10 minutes) has been charged.
func challenge(m *Module, peer, forwardedFor string) (int, string) {
	r := httptest.NewRequest(http.MethodPost, "/public/witshield/t/p_a/i_a/agent/v1/enroll/challenge", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = peer
	if forwardedFor != "" {
		r.Header.Set("X-Forwarded-For", forwardedFor)
	}
	w := httptest.NewRecorder()
	http.StripPrefix("/public/witshield", m.PublicHandler()).ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// exhaust spends the challenge budget of whatever source the engine sees for
// these requests and checks that the next one is refused.
func exhaust(t *testing.T, m *Module, peer string, forwardedFor func(int) string) {
	t.Helper()
	for i := 0; i < 120; i++ {
		if code, body := challenge(m, peer, forwardedFor(i)); code != http.StatusUnauthorized {
			t.Fatalf("challenge %d via %s: %d %+q", i, peer, code, body)
		}
	}
	if code, body := challenge(m, peer, forwardedFor(120)); code != http.StatusTooManyRequests || !strings.Contains(body, "enrollment_rate_limited") {
		t.Fatalf("challenge 121 via %s: %d %+q", peer, code, body)
	}
}

func client(address string) func(int) string { return func(int) string { return address } }

// Behind the platform's reverse proxy every Agent reaches the engine from the
// proxy address. With the proxy listed in EULER_WEAUTH_TRUSTED_PROXIES the
// engine's per-source limits key on each Agent host instead of one shared
// bucket for the whole installation.
func TestAgentLimitsKeyOnTheAgentBehindTrustedProxy(t *testing.T) {
	t.Setenv(appkit.TrustedProxiesEnv, "10.0.0.0/8")
	m, _, _ := fixture(t)
	exhaust(t, m, composeProxy, client("203.0.113.10"))
	if code, body := challenge(m, composeProxy, "203.0.113.11"); code != http.StatusUnauthorized {
		t.Fatalf("second Agent behind the proxy shares the first one's budget: %d %+q", code, body)
	}
	// The engine sees host:port, so IPv6 Agents are told apart too (per /64).
	exhaust(t, m, composeProxy, client("2001:db8:a::10"))
	if code, body := challenge(m, composeProxy, "2001:db8:b::10"); code != http.StatusUnauthorized {
		t.Fatalf("IPv6 Agent in another /64 shares a budget: %d %+q", code, body)
	}
	// A client that connects directly cannot choose its bucket with a forged header.
	exhaust(t, m, "198.51.100.7:5000", func(i int) string { return fmt.Sprintf("203.0.113.%d", 20+i) })
}

func TestAgentLimitsWithoutTrustedProxiesKeyOnThePeer(t *testing.T) {
	t.Setenv(appkit.TrustedProxiesEnv, "")
	m, _, _ := fixture(t)
	exhaust(t, m, composeProxy, client("203.0.113.10"))
	if code, body := challenge(m, composeProxy, "203.0.113.11"); code != http.StatusTooManyRequests {
		t.Fatalf("forwarding believed without a trusted proxy list: %d %+q", code, body)
	}
}

// Like WeAuth, Statistics and Lottery, an unusable proxy list stops startup
// instead of silently keying every Agent on the proxy.
func TestInvalidTrustedProxiesFailMigrate(t *testing.T) {
	t.Setenv(appkit.TrustedProxiesEnv, "")
	m, _, _ := fixture(t)
	for _, value := range []string{"proxy.internal", "10.0.0.0/33", "0.0.0.0/0"} {
		t.Setenv(appkit.TrustedProxiesEnv, value)
		rt := *m.rt
		rt.DataDir = t.TempDir()
		other := New(&rt)
		err := other.Migrate(context.Background())
		if closeErr := other.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err == nil {
			t.Errorf("%s=%+q accepted by Migrate", appkit.TrustedProxiesEnv, value)
		}
	}
}
