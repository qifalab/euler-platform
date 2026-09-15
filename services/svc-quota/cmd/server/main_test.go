package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

type occupyData struct {
	Data struct {
		ReservationId string `json:"reservationId"`
		Amount        int    `json:"amount"`
	} `json:"Data"`
}

func doOccupy(t *testing.T, s *quotaStore, body occupyRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/v1/quota/occupy", bytes.NewReader(b))
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleOccupy(rec, req)
	return rec
}

// TestOccupyIdempotencyKey verifies a retried occupy with the same
// idempotencyKey returns the original reservation and does not accumulate
// Occupying twice.
func TestOccupyIdempotencyKey(t *testing.T) {
	s := newInMemoryQuotaStore()
	req := occupyRequest{ProductCode: "scecs", Region: "cn-north-1", Count: 3, BizKey: "order-1", IdempotencyKey: "idem-1"}

	rec1 := doOccupy(t, s, req)
	if rec1.Code != 200 {
		t.Fatalf("first occupy: %d %s", rec1.Code, rec1.Body.String())
	}
	var d1 occupyData
	_ = json.Unmarshal(rec1.Body.Bytes(), &d1)

	rec2 := doOccupy(t, s, req)
	if rec2.Code != 200 {
		t.Fatalf("retry occupy: %d %s", rec2.Code, rec2.Body.String())
	}
	var d2 occupyData
	_ = json.Unmarshal(rec2.Body.Bytes(), &d2)
	if d2.Data.ReservationId != d1.Data.ReservationId {
		t.Fatalf("retry returned %q, want %q", d2.Data.ReservationId, d1.Data.ReservationId)
	}

	// Read the counter through the store API rather than the in-memory map: the
	// assertion then holds for whichever backend the service is wired to.
	u, err := s.GetUsage(100123, "quota_scecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Occupying != 3 {
		t.Fatalf("Occupying = %d, want 3 (no double count)", u.Occupying)
	}
}

// TestOccupyDistinctKeysAccumulate verifies different keys still reserve
// separately.
func TestOccupyDistinctKeysAccumulate(t *testing.T) {
	s := newInMemoryQuotaStore()
	if rec := doOccupy(t, s, occupyRequest{ProductCode: "scecs", Count: 2, IdempotencyKey: "a"}); rec.Code != 200 {
		t.Fatalf("occupy a: %d", rec.Code)
	}
	if rec := doOccupy(t, s, occupyRequest{ProductCode: "scecs", Count: 2, IdempotencyKey: "b"}); rec.Code != 200 {
		t.Fatalf("occupy b: %d", rec.Code)
	}
	u, err := s.GetUsage(100123, "quota_scecs_instance", "cn-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Occupying != 4 {
		t.Fatalf("Occupying = %d, want 4", u.Occupying)
	}
}
