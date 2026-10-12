package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/qifalab/euler-platform/runmode"
	"github.com/qifalab/euler-platform/trial"
)

// fakeIAM serves svc-iam web-auth's GET /internal/accounts/{id}/realname.
type fakeIAM struct {
	mu       sync.Mutex
	accounts map[string]map[string]any // account id → Data
	fail     bool
	tokens   []string // X-Euler-Internal-Token seen
	paths    []string
}

func (f *fakeIAM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, r.Header.Get(internalTokenHeader))
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if f.fail {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "Common.InternalError", "Message": "boom"})
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/internal/accounts/"), "/realname")
	d, ok := f.accounts[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "IAM.AccountNotFound", "Message": "no such account"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": d})
}

func iamTrialStore(t *testing.T, iam *fakeIAM) *trialStore {
	t.Helper()
	srv := httptest.NewServer(iam)
	t.Cleanup(srv.Close)
	t.Setenv("EULER_INTERNAL_TOKEN", "s3cret")
	t.Setenv(iamURLEnv, srv.URL)
	s, err := newMemTrialStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claimCode(t *testing.T, s *trialStore, acct string) (int, string) {
	t.Helper()
	w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", acct)
	code, _, _ := trialEnvelope(t, w)
	return w.Code, code
}

// Real accounts are admitted on svc-iam's verdict — not only the seeded demo
// account — and the identity key it returns drives the cross-account dedup.
func TestTrialRealNameFromIAM(t *testing.T) {
	iam := &fakeIAM{accounts: map[string]map[string]any{
		"200001": {"verified": true, "type": "personal", "identityKey": "ab12cd"},
		"200002": {"verified": true, "type": "enterprise", "identityKey": "ab12cd"}, // same 实名 identity
		"200003": {"verified": false, "type": "", "identityKey": ""},
		"200004": {"verified": true, "type": "personal", "identityKey": ""}, // a data bug upstream
		"100123": {"verified": false, "type": "", "identityKey": ""},        // the dev seed is not evidence
	}}
	s := iamTrialStore(t, iam)

	if status, code := claimCode(t, s, "200001"); status != 200 || code != "OK" {
		t.Fatalf("verified account: %d %s, want 200 OK", status, code)
	}
	if got := memTrial(s).identityCounts["ab12cd"]; got != 1 {
		t.Fatalf("identity registry = %d for svc-iam's key, want 1", got)
	}
	for acct, want := range map[string]trial.Rule{
		"200002": trial.RuleIdentityLimit, // one ID card, second account
		"200003": trial.RuleRealName,      // unverified
		"200004": trial.RuleRealName,      // verified without a key: fail closed
		"200099": trial.RuleRealName,      // unknown to svc-iam (404)
		"100123": trial.RuleRealName,      // seeded locally, unverified in svc-iam
	} {
		if status, code := claimCode(t, s, acct); status != 409 || code != string(want) {
			t.Fatalf("account %s: %d %s, want 409 %s", acct, status, code, want)
		}
	}
	for _, tok := range iam.tokens {
		if tok != "s3cret" {
			t.Fatalf("svc-iam called without the internal token: %q", tok)
		}
	}
	if iam.paths[0] != "GET /internal/accounts/200001/realname" {
		t.Fatalf("svc-iam path = %q", iam.paths[0])
	}

	// The status page reads the same verdict.
	w := doTrial(t, s, http.MethodGet, "/api/v1/trial/status", "200003")
	if _, d, _ := trialEnvelope(t, w); w.Code != 200 || d["eligible"] != false || d["reason"] != string(trial.RuleRealName) {
		t.Fatalf("status for an unverified account: %d %v", w.Code, d)
	}
}

// svc-iam unreachable: the gate fails closed with a retryable 503 and mints
// nothing.
func TestTrialRealNameUnavailable(t *testing.T) {
	iam := &fakeIAM{fail: true}
	s := iamTrialStore(t, iam)
	if status, code := claimCode(t, s, "200001"); status != 503 || code != "Order.RealNameUnavailable" {
		t.Fatalf("claim with svc-iam down: %d %s, want 503 Order.RealNameUnavailable", status, code)
	}
	if len(trialCoupons(t, s, 200001)) != 0 || memTrial(s).globalActive != 0 {
		t.Fatal("a voucher was minted without a real-name verdict")
	}
	if w := doTrial(t, s, http.MethodGet, "/api/v1/trial/status", "200001"); w.Code != 503 {
		t.Fatalf("status with svc-iam down: %d", w.Code)
	}
}

func TestTrialStoreFromEnv(t *testing.T) {
	t.Setenv(iamURLEnv, "")
	t.Setenv(runmode.EnvVar, "prod")
	if _, err := newMemTrialStoreFromEnv(); err == nil {
		t.Fatal("production without EULER_SVC_IAM_URL must refuse to start")
	}
	t.Setenv(runmode.EnvVar, "dev")
	s, err := newMemTrialStoreFromEnv()
	if err != nil || s.realName != nil {
		t.Fatalf("dev without the URL: realName=%v err=%v, want the seeded registry", s.realName, err)
	}
	if status, code := claimCode(t, s, "100123"); status != 200 || code != "OK" {
		t.Fatalf("seeded dev account: %d %s", status, code)
	}
	t.Setenv(runmode.EnvVar, "prod")
	t.Setenv(iamURLEnv, "http://svc-iam.euler-svc-iam.svc:9101")
	if s, err := newMemTrialStoreFromEnv(); err != nil || s.realName == nil {
		t.Fatalf("production with the URL: realName=%v err=%v", s.realName, err)
	}
}
