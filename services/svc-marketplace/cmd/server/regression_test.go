package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Review and settlement are platform operations. Before the operator gate any
// tenant could approve its own listing (with partnerRateBps=10000) and then
// fabricate a settlement for an arbitrary order id — the whole commercial loop
// of the marketplace was self-service.
func TestReviewAndSettlementRequireAPlatformOperator(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))

	doAs := func(acct, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("X-Euler-Account-Id", acct)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	dataOf := func(rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var env struct {
			Data map[string]any `json:"Data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("envelope: %v (%s)", err, rec.Body.String())
		}
		return env.Data
	}

	// A partner (account 100200) publishes a listing with a 100% partner rate.
	rec := doAs("100200", http.MethodPost, "/api/v1/marketplace/listings", map[string]any{
		"name": "partner-thing", "category": "SERVICE",
		"openapiUrl": "https://partner.example/api", "partnerRateBps": 10000,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	listingID := int64(dataOf(rec)["listingId"].(float64))
	approvePath := "/api/v1/marketplace/listings/" + itoa(listingID) + "/approve"

	// The review queue is not a partner's business.
	if rec := doAs("100200", http.MethodGet, "/api/v1/marketplace/listings?status=PENDING_APPROVAL", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("partner reading the review queue: %d, want 403", rec.Code)
	}
	// Nor is approving its own listing.
	if rec := doAs("100200", http.MethodPost, approvePath, map[string]any{"approved": true}); rec.Code != http.StatusForbidden {
		t.Fatalf("self-approval: %d %s, want 403", rec.Code, rec.Body.String())
	}
	// And it must not be able to fabricate a settlement first.
	if rec := doAs("100200", http.MethodPost, "/api/v1/marketplace/settlements", map[string]any{
		"orderId": "ord-victim", "listingId": listingID, "grossMicro": 999999999,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("self-settlement: %d %s, want 403", rec.Code, rec.Body.String())
	}

	// The platform operator (the dev seed account by default) can do both.
	if rec := doAs("100123", http.MethodPost, approvePath, map[string]any{"approved": true}); rec.Code != http.StatusOK {
		t.Fatalf("operator approval: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doAs("100123", http.MethodPost, "/api/v1/marketplace/settlements", map[string]any{
		"orderId": "ord-1", "listingId": listingID, "grossMicro": 100000000,
	}); rec.Code != http.StatusOK {
		t.Fatalf("operator settlement: %d %s", rec.Code, rec.Body.String())
	}
}
