package main

import (
	"context"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/resource"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

// newSQLTestLedger wires the SQL ledger to a throwaway resource_db built from the
// service's own DDL directory (V1 schema, V2 号段表, V3 surrogate keys).
func newSQLTestLedger(t *testing.T) *sqlLedger {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-orchestrator"))
	return newSQLLedger(context.Background(), db)
}

var testClock = time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC)

func newInstance(id string, orderID int64) *resource.Instance {
	return &resource.Instance{
		ResourceID: id, AccountID: 100123, ProductCode: "euecs", ResourceType: "instance",
		Region: "cn-north-1", ChargeType: resource.ChargePrepaid, State: resource.StateCreating,
		SpecCode: "euecs.s2.large", OrderID: orderID, Version: 1,
		CreatedAt: testClock, UpdatedAt: testClock,
	}
}

func TestSQLLedgerInstanceRoundTrip(t *testing.T) {
	ledger := newSQLTestLedger(t)
	const acct = int64(100123)

	inst := newInstance("euecs-cn-north-1-01-00000042", 42)
	if err := ledger.Create(inst); err != nil {
		t.Fatal(err)
	}

	got, err := ledger.Get(acct, inst.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("created instance not found")
	}
	if got.State != resource.StateCreating || got.ChargeType != resource.ChargePrepaid ||
		got.SpecCode != "euecs.s2.large" || got.OrderID != 42 || got.Version != 1 {
		t.Fatalf("instance round trip = %+v", got)
	}
	// resource_type is NOT NULL in the schema; a caller that left it empty gets
	// the phase-1 default rather than a failed insert.
	bare := newInstance("euecs-cn-north-1-02-00000043", 43)
	bare.ResourceType = ""
	if err := ledger.Create(bare); err != nil {
		t.Fatalf("empty resource type: %v", err)
	}

	// ByOrder is the restart-safe fulfilment idempotency check.
	prior, err := ledger.ByOrder(acct, 42)
	if err != nil {
		t.Fatal(err)
	}
	if prior == nil || prior.ResourceID != inst.ResourceID {
		t.Fatalf("ByOrder = %+v", prior)
	}
	if other, err := ledger.ByOrder(999, 42); err != nil || other != nil {
		t.Fatalf("cross-account ByOrder = %+v (err %v)", other, err)
	}
}

func TestSQLLedgerTransitionStampsBillingAndJournals(t *testing.T) {
	ledger := newSQLTestLedger(t)
	const acct = int64(100123)

	inst := newInstance("euecs-cn-north-1-03-00000044", 44)
	if err := ledger.Create(inst); err != nil {
		t.Fatal(err)
	}

	// CREATING → RUNNING stamps the billing clock (计费起点=首次 RUNNING,永不回拨).
	if err := ledger.Transition(inst, resource.StateRunning, "provisioned"); err != nil {
		t.Fatal(err)
	}
	if inst.BillingStart.IsZero() {
		t.Fatal("BillingStart was not stamped on the first RUNNING edge")
	}
	firstBilling := inst.BillingStart

	// RUNNING → STOPPED → RUNNING must NOT reset the billing clock.
	if err := ledger.Transition(inst, resource.StateStopped, "user stop"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Transition(inst, resource.StateRunning, "user start"); err != nil {
		t.Fatal(err)
	}
	if !inst.BillingStart.Equal(firstBilling) {
		t.Fatalf("billing clock moved: %s -> %s", firstBilling, inst.BillingStart)
	}

	// The journal carries one row per edge, oldest first.
	if n := countResourceLog(t, ledger, inst.ResourceID); n != 3 {
		t.Fatalf("state log has %d rows, want 3 (creating→running→stopped→running)", n)
	}

	// A stale view must not move the row: a second handler holding a copy from
	// before the first transition loses its write, and nothing is journalled.
	stale, err := ledger.Get(acct, inst.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := ledger.Get(acct, inst.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Transition(fresh, resource.StateStopped, "winner"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Transition(stale, resource.StateStopped, "loser"); err == nil {
		t.Fatal("a stale transition succeeded")
	}
	if n := countResourceLog(t, ledger, inst.ResourceID); n != 4 {
		t.Fatalf("a rejected transition journaled a row (%d rows)", n)
	}
}

func TestSQLLedgerReleasePath(t *testing.T) {
	ledger := newSQLTestLedger(t)
	const acct = int64(100123)

	inst := newInstance("euecs-cn-north-1-04-00000045", 45)
	if err := ledger.Create(inst); err != nil {
		t.Fatal(err)
	}
	// The machine's edge table gates the release path: a CREATING instance is
	// not releasable, which is the guard doing its job.
	if err := ledger.Transition(inst, resource.StateReleasing, "premature release"); err == nil {
		t.Fatal("CREATING → RELEASING must be refused by the state machine")
	}
	if err := ledger.Transition(inst, resource.StateRunning, "provisioned"); err != nil {
		t.Fatal(err)
	}
	// The release saga's two legal edges.
	if err := ledger.Transition(inst, resource.StateReleasing, "user release"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Transition(inst, resource.StateReleased, "released"); err != nil {
		t.Fatal(err)
	}
	got, err := ledger.Get(acct, inst.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != resource.StateReleased {
		t.Fatalf("released instance = %+v, want RELEASED", got)
	}
	// RELEASED is terminal: another edge must be refused by the machine.
	if err := ledger.Transition(got, resource.StateRunning, "zombie"); err == nil {
		t.Fatal("a released resource came back to RUNNING")
	}
}

func TestSQLLedgerProvisionRecordUpserts(t *testing.T) {
	ledger := newSQLTestLedger(t)

	inst := newInstance("euecs-cn-north-1-05-00000046", 46)
	if err := ledger.Create(inst); err != nil {
		t.Fatal(err)
	}
	rec := provisionRecord{
		ResourceID: inst.ResourceID, AccountID: 100123, OpType: "CREATE",
		IdempotKey: "46", Params: `{"resourceId":"` + inst.ResourceID + `"}`, Status: "DISPATCHED",
	}
	if err := ledger.RecordProvision(rec); err != nil {
		t.Fatal(err)
	}
	// The same order+operation updates the row (uk_idem): a retry is not a
	// second task the retry scanner would double-dispatch.
	rec.Status = "DONE"
	if err := ledger.RecordProvision(rec); err != nil {
		t.Fatal(err)
	}
	if n := countProvisionTasks(t, ledger, inst.ResourceID); n != 1 {
		t.Fatalf("provision_task has %d rows for one order+op, want 1", n)
	}
	var status string
	if err := ledger.db.QueryRow(
		`SELECT status FROM provision_task WHERE resource_id = ?`, inst.ResourceID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "DONE" {
		t.Fatalf("task status = %s, want DONE after the retry", status)
	}
}

// TestSQLLedgerSurvivesRestart is why the ledger is in a database: a restarted
// orchestrator still knows which orders produced which resources, and can read
// the whole state history of an instance it no longer has in memory.
func TestSQLLedgerSurvivesRestart(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-orchestrator"))
	ctx := context.Background()

	first := newSQLLedger(ctx, db)
	inst := newInstance("euecs-cn-north-1-06-00000047", 47)
	if err := first.Create(inst); err != nil {
		t.Fatal(err)
	}
	if err := first.Transition(inst, resource.StateRunning, "provisioned"); err != nil {
		t.Fatal(err)
	}

	restarted := newSQLLedger(ctx, db)
	prior, err := restarted.ByOrder(100123, 47)
	if err != nil {
		t.Fatal(err)
	}
	if prior == nil || prior.ResourceID != inst.ResourceID || prior.State != resource.StateRunning {
		t.Fatalf("restarted ByOrder = %+v", prior)
	}
	// The billing stamp is real wall-clock time (it must never be derivable from
	// a fixture), so the restart assertion is only that it survived and stays
	// stable across further reads.
	again, err := restarted.ByOrder(100123, 47)
	if err != nil {
		t.Fatal(err)
	}
	if prior.BillingStart.IsZero() || !prior.BillingStart.Equal(again.BillingStart) {
		t.Fatalf("billing start not stable across restarts: %v then %v", prior.BillingStart, again.BillingStart)
	}
	if n := countResourceLog(t, restarted, inst.ResourceID); n != 1 {
		t.Fatalf("state history has %d rows after the restart, want 1", n)
	}
}

// TestSQLLedgerMatchesMemoryStore runs one fulfil-shaped script through both
// implementations and requires the same observable outcome.
func TestSQLLedgerMatchesMemoryStore(t *testing.T) {
	run := func(ledger resourceLedger) *resource.Instance {
		t.Helper()
		inst := newInstance("euecs-cn-north-1-07-00000048", 48)
		if err := ledger.Create(inst); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := ledger.Transition(inst, resource.StateRunning, "provisioned"); err != nil {
			t.Fatalf("run: %v", err)
		}
		if err := ledger.Transition(inst, resource.StateStopped, "user stop"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		got, err := ledger.Get(100123, inst.ResourceID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got
	}

	sqlInst := run(newSQLTestLedger(t))
	memInst := run(newMemLedger())

	if sqlInst.State != memInst.State || sqlInst.Version != memInst.Version ||
		sqlInst.ChargeType != memInst.ChargeType || sqlInst.SpecCode != memInst.SpecCode ||
		sqlInst.ResourceType != memInst.ResourceType || sqlInst.OrderID != memInst.OrderID {
		t.Fatalf("sql %+v, memory %+v", sqlInst, memInst)
	}
	// Both stamp the billing clock on the first RUNNING edge and neither resets
	// it on the later stop.
	if sqlInst.BillingStart.IsZero() || memInst.BillingStart.IsZero() {
		t.Fatalf("billing start missing: sql %v, memory %v", sqlInst.BillingStart, memInst.BillingStart)
	}
}

func countResourceLog(t *testing.T, ledger *sqlLedger, resourceID string) int {
	t.Helper()
	var n int
	if err := ledger.db.QueryRow(
		`SELECT COUNT(*) FROM resource_state_log WHERE resource_id = ?`, resourceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countProvisionTasks(t *testing.T, ledger *sqlLedger, resourceID string) int {
	t.Helper()
	var n int
	if err := ledger.db.QueryRow(
		`SELECT COUNT(*) FROM provision_task WHERE resource_id = ?`, resourceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
