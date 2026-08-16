package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
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
