package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func doFulfill(t *testing.T, s *lifecycleStore, body fulfillRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/v1/orchestrator/fulfill", bytes.NewReader(b))
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleFulfill(rec, req)
	return rec
}

type fulfillData struct {
	Data struct {
		ResourceId string `json:"resourceId"`
		State      string `json:"state"`
		OrderState string `json:"orderState"`
	} `json:"Data"`
}

// TestFulfillIdempotent verifies a retried fulfill for an already-fulfilled
// order returns the existing resourceId with 200 rather than a 409.
func TestFulfillIdempotent(t *testing.T) {
	s := newStore()
	req := fulfillRequest{OrderID: 42, OrderNo: "SO42", ProductCode: "scecs", Region: "cn-north-1", SpecCode: "scecs.s2.large"}

	rec1 := doFulfill(t, s, req)
	if rec1.Code != 200 {
		t.Fatalf("first fulfill: %d %s", rec1.Code, rec1.Body.String())
	}
	var d1 fulfillData
	_ = json.Unmarshal(rec1.Body.Bytes(), &d1)
	if d1.Data.ResourceId == "" || d1.Data.State != "RUNNING" || d1.Data.OrderState != "COMPLETED" {
		t.Fatalf("unexpected first fulfill data: %+v", d1.Data)
	}

	rec2 := doFulfill(t, s, req)
	if rec2.Code != 200 {
		t.Fatalf("retry fulfill should be 200, got %d %s", rec2.Code, rec2.Body.String())
	}
	var d2 fulfillData
	_ = json.Unmarshal(rec2.Body.Bytes(), &d2)
	if d2.Data.ResourceId != d1.Data.ResourceId {
		t.Fatalf("retry returned %q, want existing %q", d2.Data.ResourceId, d1.Data.ResourceId)
	}
}

// TestReleaseWalksStateMachine verifies release ends in RELEASED via the legal
// RELEASING → RELEASED path and calls the driver.
func TestReleaseWalksStateMachine(t *testing.T) {
	s := newStore()
	rec := doFulfill(t, s, fulfillRequest{OrderID: 7, ProductCode: "scecs", Region: "cn-north-1"})
	var d fulfillData
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	resID := d.Data.ResourceId

	relReq := httptest.NewRequest("POST", "/api/v1/orchestrator/resources/"+resID+"/release", nil)
	relReq.SetPathValue("id", resID)
	relReq.Header.Set(accountIDHeader, "100123")
	relRec := httptest.NewRecorder()
	s.handleRelease(relRec, relReq)
	if relRec.Code != 200 {
		t.Fatalf("release: %d %s", relRec.Code, relRec.Body.String())
	}
	s.mu.RLock()
	got := string(s.resources[resID].State)
	s.mu.RUnlock()
	if got != "RELEASED" {
		t.Fatalf("state = %s, want RELEASED", got)
	}
	// Releasing again must fail with a state error, not silently succeed.
	relReq2 := httptest.NewRequest("POST", "/api/v1/orchestrator/resources/"+resID+"/release", nil)
	relReq2.SetPathValue("id", resID)
	relReq2.Header.Set(accountIDHeader, "100123")
	relRec2 := httptest.NewRecorder()
	s.handleRelease(relRec2, relReq2)
	if relRec2.Code != 409 {
		t.Fatalf("double release should 409, got %d", relRec2.Code)
	}
}
