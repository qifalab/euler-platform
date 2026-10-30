package weauth

import "testing"

func TestSiteverifyRemoteIPAcceptsEquivalentSpellings(t *testing.T) {
	m, s, _ := fixture(t)
	site := createTest(t, m, s, false)
	secret := secretFor(t, m, s, site)
	// call() sends every request from 203.0.113.5.
	for _, c := range []struct {
		remote string
		ok     bool
	}{
		{"203.0.113.5", true},
		{"::ffff:203.0.113.5", true},
		{" [::ffff:203.0.113.5] ", true},
		{"198.51.100.7", false},
		{"not-an-ip", false},
	} {
		value := token(t, m, site)
		w := call(t, m.PublicHandler(), nil, "POST", "/pow/siteverify", map[string]string{"secret": secret, "token": value, "remoteip": c.remote})
		expect(t, w, 200)
		if got := decode[map[string]any](t, w)["success"] == true; got != c.ok {
			t.Fatalf("remoteip %q: success=%v, want %v", c.remote, got, c.ok)
		}
	}
}
