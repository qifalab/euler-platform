package statistics

import (
	"strings"
	"testing"
)

// Collection accepts http:// origins, but browsers expose crypto.randomUUID
// only in secure contexts (HTTPS, localhost). On a plain-HTTP page the old
// tracker threw in both the try and the catch branch, so a new visitor was
// never collected and eulerTrackPage/eulerTrackEvent were never defined.
// No JS runtime is available to Go tests, so this checks the generated script
// keeps a getRandomValues fallback (exposed on plain HTTP) behind a guard.
func TestTrackerVisitorIDDoesNotRequireSecureContext(t *testing.T) {
	m, scope := fixture(t)
	s := createSite(t, m, scope)
	w := call(t, m.PublicHandler(), nil, "GET", "/sites/"+s.PublicID+"/tracker.js", "", "")
	requireStatus(t, w, 200)
	js := w.Body.String()
	if !strings.Contains(js, "crypto.getRandomValues(") || !strings.Contains(js, "typeof crypto.randomUUID==='function'") {
		t.Fatalf("tracker visitor ID needs a non-secure-context fallback: %s", js)
	}
	if strings.Contains(js, "catch(_){visitor=crypto.randomUUID()}") {
		t.Fatal("tracker still calls crypto.randomUUID unguarded in its fallback branch")
	}
}
