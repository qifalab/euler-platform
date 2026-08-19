package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// do performs a request with the account header injected (as the gateway would).
func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Sc-Account-Id", "100123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func publishListing(t *testing.T, h http.Handler) int64 {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/api/v1/marketplace/listings", map[string]any{
		"name": "partner-mysql-image", "category": "IMAGE", "openapiUrl": "https://partner.example/api", "partnerRateBps": 3000,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: status %d body %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct{ ListingID int64 `json:"listingId"` } `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("publish unmarshal: %v", err)
	}
	return env.Data.ListingID
}

func TestPublishStartsPendingApproval(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))
	rec := do(t, h, http.MethodPost, "/api/v1/marketplace/listings", map[string]any{
		"name": "x", "category": "SAAS", "openapiUrl": "https://p/x", "partnerRateBps": 1000,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "PENDING_APPROVAL") {
		t.Fatalf("new listing must be PENDING_APPROVAL, body %s", rec.Body.String())
	}
}

func TestListOnlyShowsApproved(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))
	publishListing(t, h) // pending — must NOT be listed
	rec := do(t, h, http.MethodGet, "/api/v1/marketplace/listings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "partner-mysql-image") {
		t.Fatal("pending listing must not appear in the customer catalogue")
	}
}

func TestApproveMakesSellable(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))
	id := publishListing(t, h)
	rec := do(t, h, http.MethodPost, "/api/v1/marketplace/listings/"+itoa(id)+"/approve", map[string]any{"approved": true})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "APPROVED") {
		t.Fatalf("approve: status %d body %s", rec.Code, rec.Body.String())
	}
	// now listed
	rec = do(t, h, http.MethodGet, "/api/v1/marketplace/listings", nil)
	if !strings.Contains(rec.Body.String(), "partner-mysql-image") {
		t.Fatal("approved listing must appear in the catalogue")
	}
}

func TestApproveRequiresPendingState(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))
	id := publishListing(t, h)
	do(t, h, http.MethodPost, "/api/v1/marketplace/listings/"+itoa(id)+"/approve", map[string]any{"approved": true})
	// second approval of an already-APPROVED listing must conflict
	rec := do(t, h, http.MethodPost, "/api/v1/marketplace/listings/"+itoa(id)+"/approve", map[string]any{"approved": true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-approve: status %d, want 409", rec.Code)
	}
}

func TestSettleSplitsExactly(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))
	id := publishListing(t, h) // partnerRateBps = 3000 (30%)
	do(t, h, http.MethodPost, "/api/v1/marketplace/listings/"+itoa(id)+"/approve", map[string]any{"approved": true})
	rec := do(t, h, http.MethodPost, "/api/v1/marketplace/settlements", map[string]any{
		"orderId": "ord-1", "listingId": id, "grossMicro": 100000000, // 100.000000 yuan
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("settle: status %d body %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			Partner  int64 `json:"partner"`
			Platform int64 `json:"platform"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("settle unmarshal: %v", err)
	}
	if env.Data.Partner != 30000000 || env.Data.Platform != 70000000 {
		t.Fatalf("split = %d/%d, want 30000000/70000000", env.Data.Partner, env.Data.Platform)
	}
}

func TestSettleIdempotent(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))
	id := publishListing(t, h)
	do(t, h, http.MethodPost, "/api/v1/marketplace/listings/"+itoa(id)+"/approve", map[string]any{"approved": true})
	body := map[string]any{"orderId": "ord-1", "listingId": id, "grossMicro": 100000000}
	first := do(t, h, http.MethodPost, "/api/v1/marketplace/settlements", body)
	second := do(t, h, http.MethodPost, "/api/v1/marketplace/settlements", body)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("settle: %d / %d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatal("settling the same order twice must return the same record, not a double payout")
	}
}

func TestSettleRejectsUnapprovedListing(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))
	id := publishListing(t, h) // never approved
	rec := do(t, h, http.MethodPost, "/api/v1/marketplace/settlements", map[string]any{
		"orderId": "ord-1", "listingId": id, "grossMicro": 100000000,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("settle unapproved: status %d, want 409", rec.Code)
	}
}

func TestPublishRejectsInvalidRate(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))
	rec := do(t, h, http.MethodPost, "/api/v1/marketplace/listings", map[string]any{
		"name": "x", "category": "IMAGE", "openapiUrl": "https://p/x", "partnerRateBps": 20000,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rate 20000 bps: status %d, want 400", rec.Code)
	}
}

func TestMissingAccountRejected(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/marketplace/listings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing account: status %d, want 403", rec.Code)
	}
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
