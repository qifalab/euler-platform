package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/starcloud/sc-platform/chaos"
	"github.com/starcloud/sc-platform/slo"
)

// TestComparisonOperatorMapping pins the operator enum to proto-hub
// proto/starcloud/monitor/v1/monitor.proto ComparisonOperator (lines 46-52):
// 1=GREATER_THAN, 2=GREATER_THAN_OR_EQUAL, 3=LESS_THAN, 4=LESS_THAN_OR_EQUAL.
func TestComparisonOperatorMapping(t *testing.T) {
	cases := []struct {
		op   int
		want string
	}{
		{cmpGreaterThan, ">"},
		{cmpGreaterThanOrEqual, ">="},
		{cmpLessThan, "<"},
		{cmpLessThanOrEqual, "<="},
		{0, ""},
		{5, ""},
	}
	for _, c := range cases {
		if got := comparisonSymbol(c.op); got != c.want {
			t.Errorf("comparisonSymbol(%d) = %q, want %q", c.op, got, c.want)
		}
	}
	if cmpGreaterThan != 1 || cmpGreaterThanOrEqual != 2 || cmpLessThan != 3 || cmpLessThanOrEqual != 4 {
		t.Fatalf("operator constants drifted from proto ComparisonOperator")
	}
}

func TestValidComparisonOperator(t *testing.T) {
	for op := 1; op <= 4; op++ {
		if !validComparisonOperator(op) {
			t.Errorf("op %d should be valid", op)
		}
	}
	for _, op := range []int{-1, 0, 5, 99} {
		if validComparisonOperator(op) {
			t.Errorf("op %d should be invalid", op)
		}
	}
}

func TestCreateRuleRejectsUnknownOperator(t *testing.T) {
	s := newRuleStore()
	body, _ := json.Marshal(createRuleRequest{
		ProductCode: "scecs", Metric: "disk_usage", Threshold: "95",
		ComparisonOperator: 5, // invalid per proto (no 5==)
	})
	req := httptest.NewRequest("POST", "/api/v1/monitor/rules", bytes.NewReader(body))
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleCreateRule(rec, req)
	if rec.Code != 400 {
		t.Fatalf("expected 400 for operator 5, got %d", rec.Code)
	}
}

func TestCreateRuleDefaultsToGTE(t *testing.T) {
	s := newRuleStore()
	body, _ := json.Marshal(createRuleRequest{
		ProductCode: "scecs", Metric: "disk_usage", Threshold: "95",
	})
	req := httptest.NewRequest("POST", "/api/v1/monitor/rules", bytes.NewReader(body))
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleCreateRule(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data AlertRule `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.ComparisonOperator != cmpGreaterThanOrEqual {
		t.Fatalf("default operator = %d, want %d (GTE)", env.Data.ComparisonOperator, cmpGreaterThanOrEqual)
	}
}

// --- M-9 stability platform: SLO + chaos endpoints ---

func newMonitorTestServer() http.Handler {
	store := newRuleStore()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/monitor/slo", store.handleSLO)
	mux.HandleFunc("GET /api/v1/monitor/chaos", store.handleChaosDrills)
	return recoverMiddleware(requestIDMiddleware(mux))
}

func getMonitorJSON(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set(accountIDHeader, "100123")
	req.Header.Set("X-Sc-TraceId", "t")
	rr := httptest.NewRecorder()
	newMonitorTestServer().ServeHTTP(rr, req)
	var env map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if data, ok := env["Data"].(map[string]any); ok {
		return rr.Code, data
	}
	return rr.Code, env
}

func TestSLOEndpoint(t *testing.T) {
	code, out := getMonitorJSON(t, "/api/v1/monitor/slo")
	if code != 200 {
		t.Fatalf("slo: code %d body %v", code, out)
	}
	objectives, _ := out["objectives"].([]any)
	if len(objectives) != len(slo.PlatformObjectives) {
		t.Fatalf("objectives = %d, want %d", len(objectives), len(slo.PlatformObjectives))
	}
	// metering-no-loss seeds 60s consumed over 1h against a ~4.3s pro-rated
	// budget → burn ≫ 14.4 ⇒ PAGE (08§10.3 fast burn).
	for _, raw := range objectives {
		o, _ := raw.(map[string]any)
		if o["name"] == "metering-no-loss" {
			if o["alert1h"] != "PAGE" {
				t.Errorf("metering alert1h = %v, want PAGE", o["alert1h"])
			}
			if o["budgetPolicy"] != "FREEZE" {
				t.Errorf("metering budgetPolicy = %v, want FREEZE (budget 4m19s, consumed 4m40s)", o["budgetPolicy"])
			}
		}
		if o["name"] == "billing-on-time" {
			if o["budgetPolicy"] != "SLOW_DOWN" {
				t.Errorf("billing budgetPolicy = %v, want SLOW_DOWN (budget 43m12s, consumed 2h10m)", o["budgetPolicy"])
			}
			if o["alert3d"] != "TICKET" {
				t.Errorf("billing alert3d = %v, want TICKET (slow burn ≥1×)", o["alert3d"])
			}
		}
		if o["name"] == "login-auth-success" {
			if o["slaEligible"] != true {
				t.Errorf("login slaEligible = %v, want true (2 consecutive quarters met)", o["slaEligible"])
			}
		}
		if o["name"] == "metering-no-loss" || o["name"] == "billing-on-time" {
			if o["slaEligible"] == true {
				t.Errorf("%v slaEligible = true, want false", o["name"])
			}
		}
	}
}

func TestSLORequiresAccount(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/monitor/slo", nil)
	rr := httptest.NewRecorder()
	newMonitorTestServer().ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("missing account: code %d, want 403", rr.Code)
	}
}

func TestChaosEndpoint(t *testing.T) {
	code, out := getMonitorJSON(t, "/api/v1/monitor/chaos")
	if code != 200 {
		t.Fatalf("chaos: code %d body %v", code, out)
	}
	drills, _ := out["drills"].([]any)
	if len(drills) != len(chaos.MandatoryDrills) {
		t.Fatalf("drills = %d, want %d", len(drills), len(chaos.MandatoryDrills))
	}
	// mysql primary failover: 210s actual vs 120s expected = 175% ⇒ 立项整改.
	for _, raw := range drills {
		d, _ := raw.(map[string]any)
		if d["kind"] == "mysql-primary-failover" {
			if d["needsRemediation"] != true {
				t.Errorf("mysql drill needsRemediation = %v, want true (210s > 150%% of 120s)", d["needsRemediation"])
			}
		}
		// every prod drill must carry a named blast radius (08§9.5).
		if d["stage"] == "PROD" {
			if d["blastRadius"] == "" {
				t.Errorf("prod drill %v has empty blast radius", d["kind"])
			}
			if abort, _ := d["abortDeadlineMs"].(float64); abort > float64(chaos.ProdAbortDeadline.Milliseconds()) {
				t.Errorf("prod drill %v abort %vms exceeds 10min", d["kind"], abort)
			}
		}
	}
}
