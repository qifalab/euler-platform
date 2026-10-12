package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
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

// The in-memory store hands out copies: a caller mutating (or JSON-encoding)
// what it got must never share memory with the record UpdateListingStatus
// writes under the lock.
func TestMemoryStoreReturnsCopies(t *testing.T) {
	s := newMemoryStore()
	l := &Listing{PartnerID: 1, Name: "x", Category: CategoryImage, OpenAPIURL: "https://p", Status: StatusPendingApproval}
	if err := s.CreateListing(l); err != nil {
		t.Fatal(err)
	}
	l.Status = StatusApproved // the caller's struct is not the stored one
	got, err := s.GetListing(l.ListingID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPendingApproval {
		t.Fatalf("stored status = %s after mutating the created pointer, want PENDING_APPROVAL", got.Status)
	}
	got.Status = StatusOffShelf
	listed, _ := s.ListByStatus(StatusPendingApproval, "")
	if len(listed) != 1 {
		t.Fatalf("pending listings = %d, want 1 (GetListing result aliased the store)", len(listed))
	}
	listed[0].Name = "mutated"
	updated, err := s.UpdateListingStatus(l.ListingID, StatusPendingApproval, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "x" {
		t.Fatalf("name = %q, want x (ListByStatus result aliased the store)", updated.Name)
	}
	updated.Status = StatusRejected
	if again, _ := s.GetListing(l.ListingID); again.Status != StatusApproved {
		t.Fatalf("status = %s, want APPROVED (UpdateListingStatus result aliased the store)", again.Status)
	}

	r := &SettlementRecord{OrderID: "ord-1", ListingID: l.ListingID, Gross: 100}
	if err := s.CreateSettlement(r); err != nil {
		t.Fatal(err)
	}
	r.Gross = 999
	rec, ok, _ := s.GetSettlementByOrder("ord-1")
	if !ok || rec.Gross != 100 {
		t.Fatalf("settlement = %+v, want gross 100", rec)
	}
}

// Approvals racing catalogue reads over HTTP (run with -race: the handlers
// encode store records after the store lock is released).
func TestConcurrentApproveAndListIsRaceFree(t *testing.T) {
	h := newMux(newApp(newMemoryStore()))
	ids := make([]int64, 8)
	for i := range ids {
		ids[i] = publishListing(t, h)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(2)
		go func(id int64) {
			defer wg.Done()
			do(t, h, http.MethodPost, "/api/v1/marketplace/listings/"+itoa(id)+"/approve", map[string]any{"approved": true})
		}(id)
		go func() {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				do(t, h, http.MethodGet, "/api/v1/marketplace/listings?status=PENDING_APPROVAL", nil)
				do(t, h, http.MethodGet, "/api/v1/marketplace/listings", nil)
			}
		}()
	}
	wg.Wait()
	rec := do(t, h, http.MethodGet, "/api/v1/marketplace/listings", nil)
	var env struct {
		Data struct {
			Listings []Listing `json:"listings"`
		} `json:"Data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if len(env.Data.Listings) != len(ids) {
		t.Fatalf("approved listings = %d, want %d", len(env.Data.Listings), len(ids))
	}
}

// lateSettleStore hides an existing settlement from the handler's pre-check
// once — the window in which a concurrent settle of the same order inserts
// between the lookup and this request's insert.
type lateSettleStore struct {
	*memoryStore
	mu     sync.Mutex
	hidden bool
}

func (s *lateSettleStore) GetSettlementByOrder(orderID string) (*SettlementRecord, bool, error) {
	s.mu.Lock()
	hide := !s.hidden
	s.hidden = true
	s.mu.Unlock()
	if hide {
		return nil, false, nil
	}
	return s.memoryStore.GetSettlementByOrder(orderID)
}

// The loser of a concurrent duplicate settle gets the winner's record (200),
// not a 409: settlement is idempotent per order.
func TestConcurrentDuplicateSettleReturnsExistingRecord(t *testing.T) {
	mem := newMemoryStore()
	h := newMux(newApp(mem))
	id := publishListing(t, h)
	do(t, h, http.MethodPost, "/api/v1/marketplace/listings/"+itoa(id)+"/approve", map[string]any{"approved": true})
	body := map[string]any{"orderId": "ord-race", "listingId": id, "grossMicro": 100000000}
	first := do(t, h, http.MethodPost, "/api/v1/marketplace/settlements", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first settle: %d %s", first.Code, first.Body.String())
	}

	late := newMux(newApp(&lateSettleStore{memoryStore: mem}))
	second := do(t, late, http.MethodPost, "/api/v1/marketplace/settlements", body)
	if second.Code != http.StatusOK {
		t.Fatalf("racing duplicate settle: %d %s, want 200 with the existing record", second.Code, second.Body.String())
	}
	var a, b struct {
		Data json.RawMessage `json:"Data"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(second.Body.Bytes(), &b)
	if len(a.Data) == 0 || string(a.Data) != string(b.Data) {
		t.Fatalf("racing settle returned %s, want the winner's %s", b.Data, a.Data)
	}

	// And truly concurrent duplicates all succeed with one record.
	var wg sync.WaitGroup
	codes := make([]int, 8)
	bodies := make([]string, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := do(t, h, http.MethodPost, "/api/v1/marketplace/settlements", map[string]any{"orderId": "ord-burst", "listingId": id, "grossMicro": 5000000})
			codes[i] = rec.Code
			var env struct {
				Data json.RawMessage `json:"Data"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &env)
			bodies[i] = string(env.Data)
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != http.StatusOK || bodies[i] != bodies[0] {
			t.Fatalf("burst settle %d: %d %s (first %s)", i, codes[i], bodies[i], bodies[0])
		}
	}
}

// Both stores report a duplicate settlement as errAlreadySettled, which is what
// handleSettle keys its idempotent replay on.
func TestSQLStoreDuplicateSettlementIsAlreadySettled(t *testing.T) {
	store := newSQLTestStore(t)
	l := newListing(100200, 3000)
	if err := store.CreateListing(l); err != nil {
		t.Fatal(err)
	}
	mk := func() *SettlementRecord {
		return &SettlementRecord{OrderID: "ord-dup", ListingID: l.ListingID, PartnerID: l.PartnerID,
			Gross: 100, PartnerRateBps: 3000, Partner: 30, Platform: 70, SettledAt: testNow}
	}
	if err := store.CreateSettlement(mk()); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSettlement(mk()); !errors.Is(err, errAlreadySettled) {
		t.Fatalf("duplicate settlement: err = %v, want errAlreadySettled", err)
	}
	if err := newMemoryStore().CreateSettlement(mk()); err != nil {
		t.Fatal(err)
	}
}
