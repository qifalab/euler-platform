package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/trial"
)

// The claim endpoint is the HTTP surface of pkg-go/trial's admission engine
// (B7). These tests walk the same gates the engine tests walk, but through the
// envelope: denials must surface the deciding rule as Code so the console can
// tell the user WHICH gate fired, and an admitted claim must mint a voucher
// carrying the 01§10 trial parameters (face value, 30-day validity).

func doTrial(t *testing.T, s *trialStore, method, path, acct string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set(accountIDHeader, acct)
	w := httptest.NewRecorder()
	if method == http.MethodPost && path == "/api/v1/trial/claim" {
		s.handleTrialClaim(w, r)
	} else {
		s.handleTrialStatus(w, r)
	}
	return w
}

func trialEnvelope(t *testing.T, w *httptest.ResponseRecorder) (string, map[string]any, string) {
	t.Helper()
	var env struct {
		Code    string         `json:"Code"`
		Data    map[string]any `json:"Data"`
		Message string         `json:"Message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad envelope: %v (%s)", err, w.Body.String())
	}
	return env.Code, env.Data, env.Message
}

func TestTrialClaimRequiresRealName(t *testing.T) {
	s := newTrialStore()
	// Unseeded account = 未实名 (the production default; the registry comes
	// from svc-iam).
	w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100999")
	if w.Code != 409 {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body.String())
	}
	code, _, msg := trialEnvelope(t, w)
	if code != string(trial.RuleRealName) {
		t.Fatalf("want Code %s, got %s", trial.RuleRealName, code)
	}
	if msg == "" {
		t.Error("denial must carry an operator-facing message")
	}
}

func TestTrialClaimIssuesVoucher(t *testing.T) {
	s := newTrialStore()
	w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100123")
	if w.Code != 200 {
		t.Fatalf("claim failed: %d %s", w.Code, w.Body.String())
	}
	code, d, _ := trialEnvelope(t, w)
	if code != "OK" {
		t.Fatalf("want OK, got %s", code)
	}
	if d["couponId"] == "" || d["couponId"] == nil {
		t.Fatalf("no couponId in response: %v", d)
	}
	if got := d["faceValue"]; got != "100" {
		t.Errorf("faceValue = %v, want 100 (t_trial_activity seed)", got)
	}
	if got := d["validDays"]; got != float64(trial.VoucherValidDays) {
		t.Errorf("validDays = %v, want %d", got, trial.VoucherValidDays)
	}
	// The minted voucher is spendable through the pricing engine as a normal
	// VOUCHER coupon — the whole point of decision D7.
	cs := s.coupons[100123]
	if len(cs) != 1 {
		t.Fatalf("store should hold 1 voucher, got %d", len(cs))
	}
	if cs[0].Kind != pricing.KindVoucher || cs[0].RemainValue != pricing.MustParseAmount("100") {
		t.Errorf("minted coupon malformed: %+v", cs[0])
	}
	// 30-day validity: expiry sits in (now+29d, now+31d).
	until := time.Until(cs[0].ExpireAt)
	if until < 29*24*time.Hour || until > 31*24*time.Hour {
		t.Errorf("voucher expires in %v, want ~30d", until)
	}
}

func TestTrialClaimActiveLimit(t *testing.T) {
	s := newTrialStore()
	// First claim succeeds (verified seed account).
	if w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100123"); w.Code != 200 {
		t.Fatalf("first claim failed: %d %s", w.Code, w.Body.String())
	}
	// Second claim while the first voucher is still live → ACTIVE_LIMIT.
	w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100123")
	if w.Code != 409 {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body.String())
	}
	if code, _, _ := trialEnvelope(t, w); code != string(trial.RuleActiveLimit) {
		t.Fatalf("want Code %s, got %s", trial.RuleActiveLimit, code)
	}
}

func TestTrialClaimIdentityDedupAcrossAccounts(t *testing.T) {
	s := newTrialStore()
	// Two different accounts, one 实名 identity: the registry is the only
	// structure that can stop the register-N-accounts attack.
	s.accounts[100200] = trial.AccountState{RealNameVerified: true, IdentityKey: "idn-8f3a1c2d"}
	if w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100123"); w.Code != 200 {
		t.Fatalf("first account claim failed: %d %s", w.Code, w.Body.String())
	}
	w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100200")
	if w.Code != 409 {
		t.Fatalf("want 409 for cross-account reclaim, got %d: %s", w.Code, w.Body.String())
	}
	if code, _, _ := trialEnvelope(t, w); code != string(trial.RuleIdentityLimit) {
		t.Fatalf("want Code %s, got %s", trial.RuleIdentityLimit, code)
	}
}

func TestTrialClaimGlobalBudget(t *testing.T) {
	s := newTrialStore()
	s.globalActive = s.policy.MaxActiveGlobal
	w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100123")
	if w.Code != 409 {
		t.Fatalf("want 409 at exhausted budget, got %d: %s", w.Code, w.Body.String())
	}
	if code, _, _ := trialEnvelope(t, w); code != string(trial.RuleGlobalBudget) {
		t.Fatalf("want Code %s, got %s", trial.RuleGlobalBudget, code)
	}
}

func TestTrialStatusPreviewsEligibility(t *testing.T) {
	s := newTrialStore()
	// Before any claim: eligible, no coupons yet.
	w := doTrial(t, s, http.MethodGet, "/api/v1/trial/status", "100123")
	if w.Code != 200 {
		t.Fatalf("status failed: %d %s", w.Code, w.Body.String())
	}
	_, d, _ := trialEnvelope(t, w)
	if d["eligible"] != true {
		t.Fatalf("seed account should be eligible, got %v", d)
	}

	// After a claim: ineligible with the rule named, coupon listed.
	doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100123")
	w = doTrial(t, s, http.MethodGet, "/api/v1/trial/status", "100123")
	_, d, _ = trialEnvelope(t, w)
	if d["eligible"] != false {
		t.Fatalf("claimed account should be ineligible, got %v", d)
	}
	if d["reason"] != string(trial.RuleActiveLimit) {
		t.Errorf("reason = %v, want %s", d["reason"], trial.RuleActiveLimit)
	}
	coupons, ok := d["coupons"].([]any)
	if !ok || len(coupons) != 1 {
		t.Fatalf("status should list 1 coupon, got %v", d["coupons"])
	}
}

func TestTrialClaimRequiresAccountHeader(t *testing.T) {
	s := newTrialStore()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/trial/claim", nil)
	w := httptest.NewRecorder()
	s.handleTrialClaim(w, r)
	if w.Code != 403 {
		t.Fatalf("want 403 without X-Euler-Account-Id, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Common.MissingAccountId") {
		t.Errorf("body should carry Common.MissingAccountId: %s", w.Body.String())
	}
}
