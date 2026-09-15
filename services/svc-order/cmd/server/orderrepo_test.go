package main

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

// newSQLTestRepo wires the SQL repo to a throwaway trade_db. Two DDL directories
// are applied: svc-payment owns trade_db's id_sequence (its V2), and svc-order's
// V2 adds the surrogate-key fixes and the auto-renew table. Any sequence row the
// store needs that no migration seeded is created by the allocator itself.
func newSQLTestRepo(t *testing.T) *sqlOrderRepo {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-payment"), sqltest.Services("svc-order"))
	repo, err := newSQLRepo(context.Background(), db)
	if err != nil {
		t.Fatalf("newSQLRepo: %v", err)
	}
	return repo
}

var testClock = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func testOrder(id int64, token string) (*order.Order, order.Event) {
	o := &order.Order{
		OrderID: id, OrderNo: "SO20260915000001", AccountID: 100123,
		Type: order.TypeNew, State: order.StatePendingPayment,
		ProductCode: "euecs", ChargeType: pricing.ChargePrepaid,
		SnapshotID: "", OriginalAmount: pricing.MustParseAmount("100"),
		DiscountAmount: pricing.MustParseAmount("0"), PayableAmount: pricing.MustParseAmount("100"),
		ClientToken: token, Version: 0,
		CreatedAt: testClock, UpdatedAt: testClock,
	}
	evt := order.Event{
		OrderID: id, AccountID: 100123, OrderNo: o.OrderNo, Type: o.Type,
		ToState: o.State, ProductCode: o.ProductCode, ChargeType: o.ChargeType,
		Amount: o.PayableAmount, OccurredAt: testClock,
	}
	return o, evt
}

func TestSQLRepoOrderLifecycle(t *testing.T) {
	repo := newSQLTestRepo(t)
	const acct = int64(100123)

	// Ids are minted, not counted: the DDL's key is 号段-issued (04§6.6).
	if repo.NewOrderID() <= 0 {
		t.Fatal("NewOrderID returned a non-positive id")
	}

	o, evt := testOrder(repo.NewOrderID(), "tok-1")
	if err := repo.Create(o, evt); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(acct, o.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("created order not found")
	}
	if got.State != order.StatePendingPayment || got.Version != 0 {
		t.Fatalf("reloaded order = %+v", got)
	}
	if got.PayableAmount.String() != "100" || got.OriginalAmount.String() != "100" {
		t.Fatalf("amounts lost precision: %+v", got)
	}

	// A retried create with the same token finds the winner's order.
	byToken, err := repo.ByClientToken(acct, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	if byToken == nil || byToken.OrderID != o.OrderID {
		t.Fatalf("ByClientToken = %+v", byToken)
	}

	// Another account asking for the id is a miss, never a leak.
	if other, err := repo.Get(999, o.OrderID); err != nil || other != nil {
		t.Fatalf("cross-account get = %+v (err %v)", other, err)
	}

	// The transition moves state, version and journal — and the payment proof
	// rides the same transaction.
	from, prevVersion := got.State, got.Version
	next := *got
	next.State = order.StatePaid
	next.Version = prevVersion + 1
	next.PaidAt = testClock.Add(time.Minute)
	paidEvt := order.Event{
		OrderID: next.OrderID, AccountID: next.AccountID, OrderNo: next.OrderNo,
		Type: next.Type, FromState: from, ToState: next.State,
		ProductCode: next.ProductCode, ChargeType: next.ChargeType,
		Amount: next.PayableAmount, OccurredAt: next.PaidAt,
	}
	if err := repo.SaveTransition(&next, from, prevVersion, "payment pay-1", paidEvt, "pay-1"); err != nil {
		t.Fatal(err)
	}

	paid, err := repo.Get(acct, o.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if paid.State != order.StatePaid || paid.Version != 1 || paid.PaidAt.IsZero() {
		t.Fatalf("after pay = %+v", paid)
	}
	ref, has, err := repo.PaymentRef(acct, o.OrderID)
	if err != nil || !has || ref != "pay-1" {
		t.Fatalf("payment ref = %q has=%v err=%v", ref, has, err)
	}

	// The journal must carry both rows: creation (from NULL) and the payment.
	if count := countJournal(t, repo, o.OrderID); count != 2 {
		t.Fatalf("state log has %d rows, want 2 (created + paid)", count)
	}
	if count := countOutbox(t, repo, o.OrderID); count != 2 {
		t.Fatalf("outbox has %d events, want 2 (created + paid)", count)
	}
}

// TestSQLRepoStaleVersionCannotMoveAnOrder is the optimistic lock doing its job:
// a writer holding a stale version must not be able to mark an order paid.
func TestSQLRepoStaleVersionCannotMoveAnOrder(t *testing.T) {
	repo := newSQLTestRepo(t)
	const acct = int64(100123)

	o, evt := testOrder(repo.NewOrderID(), "tok-stale")
	if err := repo.Create(o, evt); err != nil {
		t.Fatal(err)
	}

	stale := *o
	stale.State = order.StatePaid
	stale.Version = 0 // o was created at version 0 and is still there; pretend we read 1
	paidEvt := order.Event{OrderID: o.OrderID, AccountID: o.AccountID, ToState: order.StatePaid, OccurredAt: testClock}
	err := repo.SaveTransition(&stale, o.State, 1, "stale payment", paidEvt, "pay-stale")
	if !errors.Is(err, order.ErrVersionConflict) && !errors.Is(err, storage.ErrVersionConflict) {
		t.Fatalf("stale transition = %v, want a version conflict", err)
	}

	got, err := repo.Get(acct, o.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != order.StatePendingPayment || got.Version != 0 {
		t.Fatalf("rejected write moved the order: %+v", got)
	}
	if countJournal(t, repo, o.OrderID) != 1 {
		t.Fatal("a rejected transition wrote a journal row")
	}
}

// TestSQLRepoAutoRenewSurvivesRestart covers the state that has no other home: a
// customer's renewal switch must outlive the process that set it.
func TestSQLRepoAutoRenewSurvivesRestart(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-payment"), sqltest.Services("svc-order"))
	ctx := context.Background()

	first, err := newSQLRepo(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetAutoRenew(100123, autoRenewSetting{ResourceID: "euecs-01", ProductCode: "euecs", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	restarted, err := newSQLRepo(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	list, err := restarted.ListAutoRenew(100123)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ResourceID != "euecs-01" || !list[0].Enabled {
		t.Fatalf("auto-renew did not survive the restart: %+v", list)
	}

	// Turning it off stores the off state rather than forgetting the row: the
	// console renders the customer's actual choice.
	if err := restarted.SetAutoRenew(100123, autoRenewSetting{ResourceID: "euecs-01", ProductCode: "euecs", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	list, err = restarted.ListAutoRenew(100123)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Enabled {
		t.Fatalf("off switch = %+v, want a stored disabled row", list)
	}
}

// TestSQLRepoMatchesMemoryStore runs one script through both implementations and
// requires the same observable outcome.
func TestSQLRepoMatchesMemoryStore(t *testing.T) {
	run := func(repo orderRepo) (*order.Order, *order.Order) {
		t.Helper()
		o, evt := testOrder(repo.NewOrderID(), "tok-parity")
		if err := repo.Create(o, evt); err != nil {
			t.Fatalf("create: %v", err)
		}
		from, prevVersion := o.State, o.Version
		next := *o
		next.State = order.StateCancelled
		next.Version = prevVersion + 1
		next.UpdatedAt = testClock.Add(time.Minute)
		cancelEvt := order.Event{
			OrderID: next.OrderID, AccountID: next.AccountID, OrderNo: next.OrderNo,
			Type: next.Type, FromState: from, ToState: next.State,
			ProductCode: next.ProductCode, ChargeType: next.ChargeType,
			Amount: next.PayableAmount, OccurredAt: next.UpdatedAt,
		}
		if err := repo.SaveTransition(&next, from, prevVersion, "customer cancelled", cancelEvt, ""); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		got, err := repo.Get(100123, o.OrderID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return o, got
	}

	// The memory repo hands back the shared pointer, so compare field-wise.
	_, memCancelled := run(newMemOrderRepo())
	_, sqlCancelled := run(newSQLTestRepo(t))

	if sqlCancelled.State != memCancelled.State || sqlCancelled.Version != memCancelled.Version ||
		sqlCancelled.OrderNo != memCancelled.OrderNo || sqlCancelled.Type != memCancelled.Type ||
		sqlCancelled.PayableAmount != memCancelled.PayableAmount ||
		sqlCancelled.OriginalAmount != memCancelled.OriginalAmount ||
		sqlCancelled.DiscountAmount != memCancelled.DiscountAmount ||
		sqlCancelled.ClientToken != memCancelled.ClientToken {
		t.Fatalf("sql %+v, memory %+v", sqlCancelled, memCancelled)
	}
}

func countJournal(t *testing.T, repo *sqlOrderRepo, orderID int64) int {
	t.Helper()
	var n int
	if err := repo.db.QueryRow(
		`SELECT COUNT(*) FROM order_state_log WHERE order_id = ?`, orderID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countOutbox(t *testing.T, repo *sqlOrderRepo, orderID int64) int {
	t.Helper()
	var n int
	if err := repo.db.QueryRow(
		`SELECT COUNT(*) FROM outbox_message WHERE biz_type = 'order' AND biz_key = ?`,
		strconv.FormatInt(orderID, 10)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
