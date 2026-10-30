package lottery

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// Behind the bundled reverse proxy every signup arrives from the proxy. With
// the proxy listed as trusted, the 20-per-minute signup limit applies per
// visitor instead of letting one visitor block the whole room.
func TestSignupLimitPerVisitorBehindTrustedProxy(t *testing.T) {
	m, s := fixture(t)
	m.trusted = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	v := newRoom(t, m, s)
	inv := call(m.Handler(), &s, "GET", "/rooms/"+v.ID+"/invitation", "")
	status(t, inv, 200)
	var invitation map[string]string
	if err := json.Unmarshal(inv.Body.Bytes(), &invitation); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(invitation["signupURL"], "/")
	join := parts[len(parts)-1]
	register := func(name, forwarded string) int {
		t.Helper()
		r := httptest.NewRequest("POST", "/join/"+join+"/register", strings.NewReader(fmt.Sprintf(`{"name":%q}`, name)))
		r.RemoteAddr = "10.0.0.2:4000"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-For", forwarded)
		w := httptest.NewRecorder()
		m.PublicHandler().ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 20; i++ {
		if code := register(fmt.Sprintf("访客甲%02d", i), "198.51.100.7"); code != 201 {
			t.Fatalf("visitor A signup %d: status %d", i, code)
		}
	}
	if code := register("访客甲超额", "198.51.100.7"); code != 429 {
		t.Fatalf("visitor A not limited: %d", code)
	}
	if code := register("访客乙", "198.51.100.8"); code != 201 {
		t.Fatalf("visitor B shares visitor A's limit behind the proxy: %d", code)
	}
}
