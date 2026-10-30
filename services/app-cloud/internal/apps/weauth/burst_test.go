package weauth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// The per-IP burst limit used to apply only during the first minute of the
// risk window, after which one address could use the whole site budget.
func TestPerIPBurstLimitAppliesForWholeRiskWindow(t *testing.T) {
	m, s, _ := fixture(t)
	site := createTest(t, m, s, false)
	body := map[string]string{"sitekey": site.Sitekey, "origin": "https://example.com"}
	challenge := func() int {
		t.Helper()
		return call(t, m.PublicHandler(), nil, "POST", "/pow/challenge", body).Code
	}
	expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/challenge", body), 200)
	// The risk window started 90 seconds ago, so its first minute is over.
	if _, e := m.rt.DB.Exec("UPDATE weauth_ip SET window_start=window_start-90"); e != nil {
		t.Fatal(e)
	}
	if _, e := m.rt.DB.Exec("UPDATE weauth_challenges SET created_at=created_at-90"); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 120; i++ {
		if code := challenge(); code != 200 {
			t.Fatalf("challenge %d: status %d", i, code)
		}
	}
	if code := challenge(); code != 429 {
		t.Fatalf("121st challenge from one IP within a minute: status %d", code)
	}
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/pow/challenge", bytes.NewReader(raw))
	r.RemoteAddr = "198.51.100.20:5555"
	w := httptest.NewRecorder()
	m.PublicHandler().ServeHTTP(w, r)
	expect(t, w, 200)
}

func TestTrustedProxyDefaultRouteRefused(t *testing.T) {
	for _, value := range []string{"0.0.0.0/0", "::/0", "127.0.0.1/32,0.0.0.0/0"} {
		t.Setenv(appkit.TrustedProxiesEnv, value)
		if e := New(&appkit.Runtime{}).Migrate(context.Background()); e == nil {
			t.Fatalf("%q accepted as trusted proxies", value)
		}
	}
	t.Setenv(appkit.TrustedProxiesEnv, "127.0.0.1/32,::1/128")
	if m := New(&appkit.Runtime{}); m.configErr != nil || len(m.trusted) != 2 {
		t.Fatalf("valid proxies refused: %v %v", m.trusted, m.configErr)
	}
}
