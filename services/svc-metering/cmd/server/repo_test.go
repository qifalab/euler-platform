package main

import (
	"context"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/metering"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

func newSQLTestAggRepo(t *testing.T) *sqlAggRepo {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-metering"))
	return newSQLAggRepo(context.Background(), db)
}


var aggHour = time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)

func testUsage(aggID string, qty string) metering.HourlyUsage {
	return metering.HourlyUsage{
		AggID: aggID, AccountID: 100123, Region: "cn-north-1",
		ResourceType: "ecs", ResourceID: "euecs-cn-north-1-01-a1b2c3d4",
		MeteringItem: "cpu_core_hour", TotalQuantity: metering.MustParseQuantity(qty),
		HourStart: aggHour, CoveredRatio: 100, WindowsSeen: 60, WindowsExpected: 60,
		BatchID: metering.BatchRealtime,
	}
}

// TestSQLAggRepoUpsertIdempotent pins the re-run contract: aggregating the same
// hour twice (the same natural key, hence the same AggID) updates the row
// instead of appending a second total — the property that makes aggregation
// infinitely re-runnable.
func TestSQLAggRepoUpsertIdempotent(t *testing.T) {
	repo := newSQLTestAggRepo(t)
	ctx := context.Background()

	if err := repo.UpsertHourly(ctx, testUsage("agg-0001-2f4a", "2")); err != nil {
		t.Fatal(err)
	}
	// Re-run of the same hour with a late-corrected total: replaces, not adds.
	if err := repo.UpsertHourly(ctx, testUsage("agg-0001-2f4a", "2.5")); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM metering_record`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-run produced %d rows, want 1", n)
	}
	loaded, err := repo.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].TotalQuantity.String() != "2.5" {
		t.Fatalf("re-run total = %+v, want the corrected 2.5", loaded)
	}
}

// TestSQLAggRepoSurvivesRestart exercises the full persistence shape: a store
// writes aggregates through its cache, a fresh store rehydrates from the ledger
// and can settle the same hours again.
func TestSQLAggRepoSurvivesRestart(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-metering"))
	ctx := context.Background()

	first := newSQLAggRepo(ctx, db)
	h1 := testUsage("agg-h1-aaaa", "2")
	h2 := testUsage("agg-h2-bbbb", "2")
	h2.HourStart = aggHour.Add(time.Hour)
	h2.AggID = "agg-h2-bbbb"
	if err := first.UpsertHourly(ctx, h1); err != nil {
		t.Fatal(err)
	}
	if err := first.UpsertHourly(ctx, h2); err != nil {
		t.Fatal(err)
	}

	restarted, err := newMeteringStoreWith(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.aggCache) != 2 {
		t.Fatalf("rehydrated cache holds %d aggregates, want 2", len(restarted.aggCache))
	}
	// The ownership metadata the read paths check must be rebuilt too: a
	// restarted process that forgets who owns a resource 403s its own tenants.
	if owner, known := restarted.accounts["euecs-cn-north-1-01-a1b2c3d4"]; !known || owner != 100123 {
		t.Fatalf("rehydrated owner = %d known=%v, want 100123", owner, known)
	}
	if region := restarted.regions["euecs-cn-north-1-01-a1b2c3d4"]; region != "cn-north-1" {
		t.Fatalf("rehydrated region = %q", region)
	}
}

func TestSQLAggRepoInMemoryMode(t *testing.T) {
	// agg == nil is the demo mode: the store seeds and never touches a ledger.
	s, err := newMeteringStoreWith(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.aggCache) == 0 {
		t.Fatal("in-memory store did not seed its demo aggregates")
	}
}
