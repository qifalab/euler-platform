package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/starcloud/sc-platform/storage/sqltest"
)

func newSQLTestActionRepo(t *testing.T) *sqlActionRepo {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-api-meta"))
	return newSQLActionRepo(context.Background(), db)
}

func testAction(product, name string) *Action {
	return &Action{
		ID: actionID(product, name), ProductCode: product, ActionName: name,
		Version: "2026-08-01",
		ParamSchema: json.RawMessage(`{"type":"object","required":["InstanceId"],"properties":{"InstanceId":{"type":"string"}}}`),
		ErrorCodes:  []string{product + ".NotFound", product + ".QuotaExceeded"},
		UpdatedBy:   "100123",
	}
}

func TestSQLActionRepoRoundTrip(t *testing.T) {
	repo := newSQLTestActionRepo(t)

	in := testAction("scecs", "DescribeInstances")
	if err := repo.Upsert(in); err != nil {
		t.Fatal(err)
	}

	got, ok, err := repo.Get(in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("registered action not found")
	}
	if got.Version != "2026-08-01" || got.UpdatedBy != "100123" || len(got.ErrorCodes) != 2 {
		t.Fatalf("action round trip = %+v", got)
	}
	if got.UpdatedAt == "" {
		t.Fatal("UpdatedAt not stamped")
	}
	var schema struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(got.ParamSchema, &schema); err != nil || schema.Type != "object" {
		t.Fatalf("param schema did not survive the JSON column: %s (%v)", got.ParamSchema, err)
	}

	// A malformed id is a miss, not a crash.
	if _, ok, err := repo.Get("no-dot-here"); err != nil || ok {
		t.Fatalf("malformed id = ok=%v err=%v, want miss", ok, err)
	}
	if _, ok, err := repo.Get("scoss.DescribeInstances"); err != nil || ok {
		t.Fatalf("unknown action = ok=%v err=%v, want miss", ok, err)
	}
}

// TestSQLActionRepoReregisterUpdatesInPlace pins the re-registration contract:
// every svc-* service re-registers its Actions at startup, which must update
// the row the gateway reads — never mint a second one.
func TestSQLActionRepoReregisterUpdatesInPlace(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-api-meta"))
	ctx := context.Background()
	repo := newSQLActionRepo(ctx, db)

	first := testAction("scoss", "CreateBucket")
	if err := repo.Upsert(first); err != nil {
		t.Fatal(err)
	}

	again := testAction("scoss", "CreateBucket")
	again.ErrorCodes = []string{"scoss.BucketAlreadyExists", "scoss.InvalidBucketName", "scoss.AccessDenied"}
	if err := repo.Upsert(again); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM api_action WHERE product_code = 'scoss'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-register produced %d rows, want 1", n)
	}
	got, ok, err := repo.Get(first.ID)
	if err != nil || !ok {
		t.Fatalf("get after re-register: ok=%v err=%v", ok, err)
	}
	if len(got.ErrorCodes) != 3 {
		t.Fatalf("re-register did not update error codes: %v", got.ErrorCodes)
	}
}

func TestSQLActionRepoListFiltersByProduct(t *testing.T) {
	repo := newSQLTestActionRepo(t)

	for _, a := range []*Action{
		testAction("scecs", "RunInstances"),
		testAction("scecs", "StartInstance"),
		testAction("scvpc", "CreateVpc"),
	} {
		if err := repo.Upsert(a); err != nil {
			t.Fatal(err)
		}
	}

	all, err := repo.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID > all[1].ID {
		t.Fatalf("list all = %d entries, unordered", len(all))
	}
	ecs, err := repo.List("scecs")
	if err != nil {
		t.Fatal(err)
	}
	if len(ecs) != 2 || ecs[0].ProductCode != "scecs" {
		t.Fatalf("list scecs = %+v", ecs)
	}
}

// TestSQLActionRepoSurvivesRestart and the parity check below close the loop:
// the gateway's validation surface must not depend on one process having
// booted, and both backends must agree on what a register→read cycle yields.
func TestSQLActionRepoSurvivesRestart(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-api-meta"))
	ctx := context.Background()

	first := newSQLActionRepo(ctx, db)
	in := testAction("scvpc", "CreateVpc")
	if err := first.Upsert(in); err != nil {
		t.Fatal(err)
	}

	restarted := newSQLActionRepo(ctx, db)
	got, ok, err := restarted.Get(in.ID)
	if err != nil || !ok {
		t.Fatalf("restarted Get: ok=%v err=%v", ok, err)
	}
	if got.UpdatedBy != "100123" || got.Version != "2026-08-01" {
		t.Fatalf("restarted action = %+v", got)
	}
}

func TestSQLActionRepoMatchesMemoryStore(t *testing.T) {
	run := func(repo actionRepo) *Action {
		t.Helper()
		in := testAction("scecs", "StartInstance")
		if err := repo.Upsert(in); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		got, ok, err := repo.Get(in.ID)
		if err != nil || !ok {
			t.Fatalf("get: ok=%v err=%v", ok, err)
		}
		return got
	}

	sqlGot := run(newSQLTestActionRepo(t))
	memGot := run(newMemActionRepo())

	if sqlGot.ID != memGot.ID || sqlGot.Version != memGot.Version ||
		sqlGot.UpdatedBy != memGot.UpdatedBy || len(sqlGot.ErrorCodes) != len(memGot.ErrorCodes) {
		t.Fatalf("sql %+v, memory %+v", sqlGot, memGot)
	}
}
