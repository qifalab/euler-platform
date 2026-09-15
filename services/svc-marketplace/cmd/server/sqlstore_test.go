package main

import (
	"context"
	"testing"
	"time"

	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/storage/sqltest"
)

// newSQLTestStore wires the SQL store to a throwaway trade_db.
//
// Two DDL directories are applied, in the order the migration manifest uses:
// svc-payment owns trade_db's id_sequence (its V2) and svc-marketplace's V2 seeds
// the two sequence rows this store needs. trade_db is shared, so the dependency is
// real rather than a test artifact.
func newSQLTestStore(t *testing.T) *sqlStore {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-payment"), sqltest.Services("svc-marketplace"))
	store, err := newSQLStore(context.Background(), db)
	if err != nil {
		t.Fatalf("newSQLStore: %v", err)
	}
	return store
}

var testNow = time.Date(2026, 9, 15, 11, 0, 0, 0, time.UTC)

func newListing(partnerID int64, rateBps int64) *Listing {
	return &Listing{
		PartnerID:      partnerID,
		Name:           "第三方镜像",
		Category:       CategoryImage,
		OpenAPIURL:     "https://partner.example.com/fulfill",
		Status:         StatusDraft,
		PartnerRateBps: rateBps,
		CreatedAt:      testNow,
		UpdatedAt:      testNow,
	}
}

func TestSQLStoreListingLifecycle(t *testing.T) {
	store := newSQLTestStore(t)

	l := newListing(20001, 3000)
	if err := store.CreateListing(l); err != nil {
		t.Fatal(err)
	}
	// The id comes from the 号段 allocator: a restart must not reuse 1.
	if l.ListingID == 0 {
		t.Fatal("listing id was not issued")
	}

	got, err := store.GetListing(l.ListingID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PartnerID != 20001 || got.PartnerRateBps != 3000 || got.Category != CategoryImage {
		t.Fatalf("listing round trip = %+v", got)
	}
	if got.OpenAPIURL != "https://partner.example.com/fulfill" {
		t.Fatalf("openapi url lost: %q", got.OpenAPIURL)
	}

	// A DRAFT is invisible to the customer catalogue and visible to the desk.
	approved, err := store.ListApproved(CategoryImage)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved) != 0 {
		t.Fatalf("a draft appeared in the catalogue: %+v", approved)
	}
	pending, err := store.ListByStatus(StatusDraft, CategoryImage)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("draft queue = %d, want 1", len(pending))
	}

	// Review transitions: DRAFT → PENDING_APPROVAL → APPROVED.
	if _, err := store.UpdateListingStatus(l.ListingID, StatusPendingApproval, StatusApproved); err == nil {
		t.Fatal("a transition whose preconditions do not hold must be refused")
	}
	if _, err := store.UpdateListingStatus(l.ListingID, StatusDraft, StatusPendingApproval); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateListingStatus(l.ListingID, StatusPendingApproval, StatusApproved); err != nil {
		t.Fatal(err)
	}
	approved, err = store.ListApproved(CategoryImage)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved) != 1 || approved[0].Status != StatusApproved {
		t.Fatalf("approved catalogue = %+v", approved)
	}
	// Category filter is exclusive.
	if other, err := store.ListApproved(CategorySaaS); err != nil || len(other) != 0 {
		t.Fatalf("category filter leaked: %+v (err %v)", other, err)
	}
	if all, err := store.ListApproved(""); err != nil || len(all) != 1 {
		t.Fatalf("empty category must mean all categories: %+v (err %v)", all, err)
	}

	if _, err := store.UpdateListingStatus(424242, StatusDraft, StatusPendingApproval); err == nil {
		t.Fatal("updating an unknown listing must fail")
	}
}

// TestSQLStoreSettlementIsImmutableAndIdempotent: uk_order is what stops a
// retried settlement from paying a partner twice.
func TestSQLStoreSettlementIsImmutableAndIdempotent(t *testing.T) {
	store := newSQLTestStore(t)

	l := newListing(20001, 3000)
	if err := store.CreateListing(l); err != nil {
		t.Fatal(err)
	}

	r := &SettlementRecord{
		OrderID:        "ord-1",
		ListingID:      l.ListingID,
		PartnerID:      l.PartnerID,
		Gross:          pricing.MustParseAmount("100"),
		PartnerRateBps: 3000,
		Partner:        pricing.MustParseAmount("30"),
		Platform:       pricing.MustParseAmount("70"),
		SettledAt:      testNow,
	}
	if err := store.CreateSettlement(r); err != nil {
		t.Fatal(err)
	}
	if r.SettlementID == 0 {
		t.Fatal("settlement id was not issued")
	}

	// The split must survive the DECIMAL(18,6) round trip exactly, and must
	// still add up — an unexplained difference is what 03§4.2.4 forbids.
	got, ok, err := store.GetSettlementByOrder("ord-1")
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.Gross.String() != "100" || got.Partner.String() != "30" || got.Platform.String() != "70" {
		t.Fatalf("split round trip = %+v", got)
	}
	if got.Partner.Add(got.Platform) != got.Gross {
		t.Fatalf("partner + platform != gross: %s + %s != %s", got.Partner, got.Platform, got.Gross)
	}
	if got.ListingID != l.ListingID || got.PartnerID != l.PartnerID || got.PartnerRateBps != 3000 {
		t.Fatalf("settlement linkage lost: %+v", got)
	}

	// Replay: refused by the unique index, reported as the domain error.
	replay := *r
	replay.SettlementID = 0
	if err := store.CreateSettlement(&replay); err == nil {
		t.Fatal("a second settlement for the same order must be refused")
	}

	// Unknown order: a miss, not an error.
	if _, ok, err := store.GetSettlementByOrder("ord-absent"); err != nil || ok {
		t.Fatalf("absent order: ok=%v err=%v", ok, err)
	}
}

// TestSQLStoreSurvivesRestart is the reason listings and settlements are in a
// database: a restarted process must not forget what partners can sell, and must
// not re-settle an order it already paid.
func TestSQLStoreSurvivesRestart(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-payment"), sqltest.Services("svc-marketplace"))
	ctx := context.Background()

	first, err := newSQLStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	l := newListing(20001, 3000)
	if err := first.CreateListing(l); err != nil {
		t.Fatal(err)
	}
	if err := first.CreateSettlement(&SettlementRecord{
		OrderID: "ord-restart", ListingID: l.ListingID, PartnerID: l.PartnerID,
		Gross: pricing.MustParseAmount("10"), PartnerRateBps: 3000,
		Partner: pricing.MustParseAmount("3"), Platform: pricing.MustParseAmount("7"),
		SettledAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}

	restarted, err := newSQLStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.GetListing(l.ListingID); err != nil {
		t.Fatalf("listing did not survive the restart: %v", err)
	}
	if _, ok, err := restarted.GetSettlementByOrder("ord-restart"); err != nil || !ok {
		t.Fatalf("settlement did not survive the restart: ok=%v err=%v", ok, err)
	}
}

// TestSQLStoreMatchesMemoryStore runs one script through both implementations.
func TestSQLStoreMatchesMemoryStore(t *testing.T) {
	run := func(store Store) (*Listing, *SettlementRecord) {
		t.Helper()
		l := newListing(20001, 3000)
		if err := store.CreateListing(l); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.UpdateListingStatus(l.ListingID, StatusDraft, StatusPendingApproval); err != nil {
			t.Fatalf("submit: %v", err)
		}
		if _, err := store.UpdateListingStatus(l.ListingID, StatusPendingApproval, StatusApproved); err != nil {
			t.Fatalf("approve: %v", err)
		}
		if err := store.CreateSettlement(&SettlementRecord{
			OrderID: "ord-parity", ListingID: l.ListingID, PartnerID: l.PartnerID,
			Gross: pricing.MustParseAmount("100"), PartnerRateBps: 3000,
			Partner: pricing.MustParseAmount("30"), Platform: pricing.MustParseAmount("70"),
			SettledAt: testNow,
		}); err != nil {
			t.Fatalf("settle: %v", err)
		}
		got, err := store.GetListing(l.ListingID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		s, ok, err := store.GetSettlementByOrder("ord-parity")
		if err != nil || !ok {
			t.Fatalf("settlement lookup: ok=%v err=%v", ok, err)
		}
		// The catalogue must agree.
		catalogue, err := store.ListApproved(CategoryImage)
		if err != nil {
			t.Fatalf("catalogue: %v", err)
		}
		if len(catalogue) != 1 {
			t.Fatalf("catalogue has %d entries, want 1", len(catalogue))
		}
		return got, s
	}

	sqlListing, sqlSettlement := run(newSQLTestStore(t))
	memListing, memSettlement := run(newMemoryStore())

	if sqlListing.Status != memListing.Status || sqlListing.Category != memListing.Category ||
		sqlListing.Name != memListing.Name || sqlListing.PartnerRateBps != memListing.PartnerRateBps ||
		sqlListing.OpenAPIURL != memListing.OpenAPIURL || sqlListing.PartnerID != memListing.PartnerID {
		t.Fatalf("listing differs: sql %+v, memory %+v", sqlListing, memListing)
	}
	if sqlSettlement.Gross != memSettlement.Gross || sqlSettlement.Partner != memSettlement.Partner ||
		sqlSettlement.Platform != memSettlement.Platform || sqlSettlement.OrderID != memSettlement.OrderID ||
		sqlSettlement.PartnerRateBps != memSettlement.PartnerRateBps ||
		!sqlSettlement.SettledAt.Equal(memSettlement.SettledAt) {
		t.Fatalf("settlement differs: sql %+v, memory %+v", sqlSettlement, memSettlement)
	}
}
