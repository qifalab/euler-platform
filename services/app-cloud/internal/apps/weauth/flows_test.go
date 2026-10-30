package weauth

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// A proof submitted from another address is refused without spending the
// challenge; the original visitor can still finish it.
func TestVerifyFromAnotherIPIsRefusedAndChallengeSurvives(t *testing.T) {
	m, s, _ := fixture(t)
	site := createTest(t, m, s, false)
	p := getProof(t, m, site)
	raw, _ := json.Marshal(solve(p))
	r := httptest.NewRequest("POST", "/pow/verify", bytes.NewReader(raw))
	r.RemoteAddr = "198.51.100.44:1000"
	w := httptest.NewRecorder()
	m.PublicHandler().ServeHTTP(w, r)
	expect(t, w, 403)
	expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/verify", solve(p)), 200)
}

// floor((requests + failures × weight) / threshold): a wrong proof raises the
// next challenge's difficulty by the configured failure weight.
func TestFailureWeightRaisesNextDifficulty(t *testing.T) {
	m, s, _ := fixture(t)
	site := createTest(t, m, s, false) // base 1, max 3, increment 1
	policy := site.Policy
	policy.RequestThreshold = 2
	policy.FailureWeight = 3
	expect(t, call(t, m.Handler(), &s, "PUT", "/sites/"+site.ID+"/ip-policy", policy), 200)
	first := getProof(t, m, site) // requests 0 → +0
	bad := solve(first)
	wrong := "x"
	for verifySolution(first.Seed, wrong, first.Difficulty) {
		wrong += "x"
	}
	bad["nonce"] = wrong
	expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/verify", bad), 400)
	next := getProof(t, m, site) // floor((1 + 1×3) / 2) = 2 → 1 + 2
	if first.Difficulty != 1 || next.Difficulty != 3 {
		t.Fatalf("difficulty %d then %d, want 1 then 3", first.Difficulty, next.Difficulty)
	}
	w := call(t, m.Handler(), &s, "GET", "/sites/"+site.ID+"/stats", nil)
	expect(t, w, 200)
	stats := decode[map[string]int](t, w)
	if stats["totalChallenges"] != 2 || stats["totalVerifications"] != 1 || stats["successVerifications"] != 0 || stats["last24hChallenges"] != 2 || stats["last24hSolved"] != 0 {
		t.Fatalf("stats after a failed proof: %v", stats)
	}
	expect(t, call(t, m.PublicHandler(), nil, "POST", "/pow/verify", solve(next)), 200)
	stats = decode[map[string]int](t, call(t, m.Handler(), &s, "GET", "/sites/"+site.ID+"/stats", nil))
	if stats["solvedChallenges"] != 1 || stats["totalVerifications"] != 2 || stats["successVerifications"] != 1 || stats["last24hSolved"] != 1 {
		t.Fatalf("stats after a valid proof: %v", stats)
	}
}

// The console test consumes a token of the selected site exactly once, needs
// the secrets permission and records an audit event.
func TestConsoleTokenTestConsumesOnce(t *testing.T) {
	m, s, _ := fixture(t)
	site := createTest(t, m, s, false)
	other := createTest(t, m, s, false)
	h := m.Handler()
	value := token(t, m, site)
	writer := s
	writer.Permissions = []string{"read", "write", "manage"}
	expect(t, call(t, h, &writer, "POST", "/sites/"+site.ID+"/test-token", map[string]string{"token": value}), 403)
	foreign := s
	foreign.ProjectID = "other"
	expect(t, call(t, h, &foreign, "POST", "/sites/"+site.ID+"/test-token", map[string]string{"token": value}), 404)
	w := call(t, h, &s, "POST", "/sites/"+other.ID+"/test-token", map[string]string{"token": value})
	expect(t, w, 200)
	if decode[map[string]any](t, w)["success"] != false {
		t.Fatal("token accepted by another site")
	}
	w = call(t, h, &s, "POST", "/sites/"+site.ID+"/test-token", map[string]string{"token": value})
	expect(t, w, 200)
	result := decode[map[string]any](t, w)
	if result["success"] != true || result["hostname"] != "example.com" || result["action"] != "login" {
		t.Fatalf("console token test: %v", result)
	}
	w = call(t, h, &s, "POST", "/sites/"+site.ID+"/test-token", map[string]string{"token": value})
	if result = decode[map[string]any](t, w); result["success"] != false {
		t.Fatalf("token consumed twice: %v", result)
	}
	var audits int
	if e := m.rt.DB.QueryRow("SELECT count(*) FROM audit WHERE action='weauth.token.tested' AND target_id=?", site.ID).Scan(&audits); e != nil {
		t.Fatal(e)
	}
	if audits != 1 {
		t.Fatalf("audit events %d, want 1", audits)
	}
}
