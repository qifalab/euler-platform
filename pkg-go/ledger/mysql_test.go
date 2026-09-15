package ledger

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

// newSQLTestLedger builds the domain Ledger on top of the real MySQL store, with
// the test clock and a monotonic entry-id sequence, so the assertions below read
// the same as the in-memory suite's.
//
// The DDL applied is services/svc-payment/sql — the schema of record for
// trade_db's ledger tables. A test that created its own tables would prove
// nothing about the table the service actually writes.
func newSQLTestLedger(t *testing.T) (*Ledger, *SQLStore) {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-payment"))
	store := NewSQLStore(db)
	var seq int64
	l := New(store, func() time.Time { return testNow }, func() int64 {
		seq++
		return seq
	})
	return l, store
}

func TestSQLStoreBalanceLifecycle(t *testing.T) {
	l, store := newSQLTestLedger(t)

	// No row yet: a zero balance, not an error and not version 1 — version 0 is
	// what tells Apply to INSERT rather than UPDATE.
	bal, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if bal.AccountID != acct || !bal.Available.IsZero() || !bal.Frozen.IsZero() || bal.Version != 0 {
		t.Fatalf("absent account balance = %+v, want zero with version 0", bal)
	}

	if _, bal, err = l.Recharge(acct, amt("1000"), "recharge-001", "idem-r1", "充值"); err != nil {
		t.Fatal(err)
	}
	if bal.Available.String() != "1000" || bal.Version != 1 {
		t.Fatalf("after recharge available=%s version=%d, want 1000/1", bal.Available, bal.Version)
	}

	// The row read back through SQL must be the row the domain produced: this
	// is where a wrong column, a lost precision or a mis-parsed DECIMAL shows.
	bal, err = store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if bal.Available.String() != "1000" || bal.Version != 1 {
		t.Fatalf("re-read balance = %s version %d, want 1000/1", bal.Available, bal.Version)
	}
	if bal.UpdatedAt.IsZero() {
		t.Fatal("balance has no updated_at")
	}
}

func TestSQLStoreRoundTripsJournalFields(t *testing.T) {
	l, store := newSQLTestLedger(t)

	// Micro-unit precision is the point of pricing.Amount (1/1_000_000); a
	// DECIMAL(14,6) round trip that quietly dropped it would be invisible until
	// a bill did not add up.
	if _, _, err := l.Recharge(acct, amt("0.000001"), "recharge-micro", "idem-micro", ""); err != nil {
		t.Fatal(err)
	}
	// Empty remark must come back as the empty string the domain uses, not as
	// a scan error on the DDL's NULL default.
	entries, err := store.ListEntries(acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("journal has %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.Amount.String() != "0.000001" {
		t.Fatalf("amount round-trip = %s, want 0.000001", got.Amount)
	}
	if got.BalanceAfter.String() != "0.000001" {
		t.Fatalf("balance_after round-trip = %s, want 0.000001", got.BalanceAfter)
	}
	if got.Type != EntryRecharge || got.BizType != "recharge" || got.BizKey != "recharge-micro" {
		t.Fatalf("entry linkage lost: %+v", got)
	}
	if got.IdempotencyKey != "idem-micro" {
		t.Fatalf("idempotency key = %q", got.IdempotencyKey)
	}
	if got.Remark != "" {
		t.Fatalf("NULL remark became %q, want empty", got.Remark)
	}
	if !got.OccurredAt.Equal(testNow) {
		t.Fatalf("occurred_at = %s, want %s", got.OccurredAt, testNow)
	}
}

func TestSQLStoreJournalIsOrderedByEntryID(t *testing.T) {
	l, store := newSQLTestLedger(t)

	// All four movements share one timestamp; only entry_id orders them. If the
	// store sorted by occurred_at the journal's replay order would be
	// ambiguous, and the BalanceAfter chain could not be checked.
	if _, _, err := l.Recharge(acct, amt("1000"), "r", "idem-1", ""); err != nil {
		t.Fatal(err)
	}
	for i, key := range []string{"idem-2", "idem-3", "idem-4"} {
		if _, _, err := l.Consume(acct, amt("10"), "order", "900"+string(rune('1'+i)), key, ""); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.ListEntries(acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("journal has %d entries, want 4", len(entries))
	}
	for i, e := range entries {
		if e.EntryID != int64(i+1) {
			t.Fatalf("entry %d has id %d: journal is not in entry_id order", i, e.EntryID)
		}
	}
	if res, err := l.Reconcile(acct); err != nil || !res.Balanced() {
		t.Fatalf("journal does not reconcile: %+v (err %v)", res, err)
	}
}

func TestSQLStoreFindByIdempotencyKey(t *testing.T) {
	l, store := newSQLTestLedger(t)

	if _, found, err := store.FindByIdempotencyKey(acct, "idem-absent"); err != nil || found {
		t.Fatalf("absent key: found=%v err=%v, want false/nil", found, err)
	}
	first, _, err := l.Recharge(acct, amt("100"), "r", "idem-r", "")
	if err != nil {
		t.Fatal(err)
	}
	prior, found, err := store.FindByIdempotencyKey(acct, "idem-r")
	if err != nil || !found {
		t.Fatalf("stored key: found=%v err=%v, want true/nil", found, err)
	}
	if prior.EntryID != first.EntryID || prior.Amount.String() != "100" {
		t.Fatalf("replayed entry = %+v, want entry %d amount 100", prior, first.EntryID)
	}
	// A key is scoped to its account: the same string under another account is
	// a different charge, which is what the uk_idempotency(account_id, key)
	// index encodes.
	if _, found, err := store.FindByIdempotencyKey(acct+1, "idem-r"); err != nil || found {
		t.Fatalf("key leaked across accounts: found=%v err=%v", found, err)
	}
}

func TestSQLStoreVersionConflictIsDetected(t *testing.T) {
	l, store := newSQLTestLedger(t)

	if _, _, err := l.Recharge(acct, amt("100"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	bal, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}

	// A writer holding a stale version must not be able to move money: this is
	// the guard that makes the UPDATE the arbiter of a race.
	stale := bal
	stale.Version = bal.Version - 1
	stale.Available = amt("50")
	err = store.Apply(Entry{
		EntryID: 99, AccountID: acct, Type: EntryConsume, Amount: amt("50"),
		BalanceAfter: amt("50"), BizType: "order", BizKey: "9001",
		IdempotencyKey: "idem-stale", OccurredAt: testNow,
	}, stale, stale.Version)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale apply error = %v, want ErrVersionConflict", err)
	}
	// The shared retry helper must also recognise the loss, otherwise services
	// using storage.WithVersionRetry would treat a benign race as fatal.
	if !errors.Is(err, storage.ErrVersionConflict) {
		t.Fatalf("conflict is not visible to storage.WithVersionRetry: %v", err)
	}

	after, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if after.Available.String() != "100" || after.Version != bal.Version {
		t.Fatalf("rejected write moved the balance: %+v", after)
	}
	entries, err := store.ListEntries(acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("rejected write left %d journal entries, want 1", len(entries))
	}
}

func TestSQLStoreDuplicateIdempotencyKeyIsRejected(t *testing.T) {
	l, store := newSQLTestLedger(t)

	if _, _, err := l.Recharge(acct, amt("1000"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	// Bypass the domain's pre-check on purpose: the unique index, not the Go
	// code, is the last line of defence against a double debit.
	bal, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	next := bal
	next.Available = bal.Available.Sub(amt("153"))
	next.Version = bal.Version + 1
	err = store.Apply(Entry{
		EntryID: 42, AccountID: acct, Type: EntryConsume, Amount: amt("153"),
		BalanceAfter: next.Available, BizType: "order", BizKey: "9001",
		IdempotencyKey: "idem-r", OccurredAt: testNow,
	}, next, bal.Version)
	if !errors.Is(err, ErrDuplicateEntry) {
		t.Fatalf("duplicate key error = %v, want ErrDuplicateEntry", err)
	}

	after, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if after.Available.String() != "1000" || after.Version != bal.Version {
		t.Fatalf("duplicate write moved the balance: %+v", after)
	}
}

// TestSQLStoreApplyIsAtomic is the reason the port needs a transaction at all: a
// balance no journal explains cannot be reconciled, so a failure in either half
// must leave neither behind.
func TestSQLStoreApplyIsAtomic(t *testing.T) {
	l, store := newSQLTestLedger(t)

	if _, _, err := l.Recharge(acct, amt("1000"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	bal, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}

	// Force the second statement to fail: entry 1 already exists, so the
	// journal insert violates the primary key *after* the balance UPDATE has
	// run inside the same transaction.
	next := bal
	next.Available = bal.Available.Sub(amt("153"))
	next.Version = bal.Version + 1
	err = store.Apply(Entry{
		EntryID: 1, AccountID: acct, Type: EntryConsume, Amount: amt("153"),
		BalanceAfter: next.Available, BizType: "order", BizKey: "9001",
		IdempotencyKey: "idem-atomic", OccurredAt: testNow,
	}, next, bal.Version)
	if err == nil {
		t.Fatal("expected the duplicate entry_id to fail the transaction")
	}

	after, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if after.Available.String() != "1000" || after.Version != bal.Version {
		t.Fatalf("a failed apply moved the balance: %+v", after)
	}
	entries, err := store.ListEntries(acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("journal has %d entries, want 1", len(entries))
	}
}

// TestSQLStoreConcurrentChargesCannotDoubleSpend is the in-memory suite's race
// test run against real MySQL. It is the test the fake cannot stand in for: the
// optimistic lock here is an UPDATE ... WHERE version = ?, and only a real
// database can show that exactly one of two racing charges matches a row.
func TestSQLStoreConcurrentChargesCannotDoubleSpend(t *testing.T) {
	l, store := newSQLTestLedger(t)

	if _, _, err := l.Recharge(acct, amt("100"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, _, err := l.Consume(acct, amt("80"), "order", "9001", "idem-race-"+string(rune('a'+idx)), "")
			results[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for i, err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("goroutine %d failed with %v, want ErrVersionConflict", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of 2 concurrent 80-unit charges succeeded against a 100 balance; want exactly 1", succeeded)
	}
	bal, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if bal.Available.String() != "20" || bal.Version != 2 {
		t.Fatalf("balance = %s version %d, want 20/2", bal.Available, bal.Version)
	}
	if res, err := l.Reconcile(acct); err != nil || !res.Balanced() {
		t.Fatalf("ledger does not reconcile after the race: %+v (err %v)", res, err)
	}
}

// TestSQLStoreDoubleSpendUnderContention widens the window the pair test can
// miss by luck: many charges that together far exceed the balance. Nothing may
// be created, and the journal must still explain the surviving balance.
func TestSQLStoreDoubleSpendUnderContention(t *testing.T) {
	l, store := newSQLTestLedger(t)

	const (
		goroutines   = 20
		charge       = "10"
		startBalance = "100"
	)
	if _, _, err := l.Recharge(acct, amt(startBalance), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCount int
	)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			key := "idem-contend-" + string(rune('A'+idx/26)) + string(rune('a'+idx%26))
			if _, _, err := l.Consume(acct, amt(charge), "order", "9001", key, ""); err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if okCount > 10 {
		t.Fatalf("%d charges of 10 succeeded against a balance of 100 — money was created", okCount)
	}
	bal, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	want := 100 - 10*okCount
	if got := bal.Available.String(); got != itoa(want) {
		t.Fatalf("balance = %s but %d charges succeeded (expected %d)", got, okCount, want)
	}
	if bal.Available.IsNegative() {
		t.Fatalf("balance went negative under contention: %s", bal.Available)
	}
	res, err := l.Reconcile(acct)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Balanced() {
		t.Fatalf("ledger does not reconcile after contention: %+v", res)
	}
	if res.EntryCount != okCount+1 {
		t.Fatalf("journal has %d entries but %d charges + 1 recharge succeeded", res.EntryCount, okCount)
	}
	t.Logf("%d/%d charges succeeded, balance %s, journal reconciles", okCount, goroutines, bal.Available)
}

// TestSQLStoreParityWithMemoryStore runs one scripted sequence of movements
// through both stores and requires identical results. Where the suites above
// check each store against the contract, this checks the two implementations
// against each other — the failure mode it guards is a SQL path that is
// internally consistent but disagrees with the in-memory one the tests were
// written against.
func TestSQLStoreParityWithMemoryStore(t *testing.T) {
	sqlLedger, sqlStore := newSQLTestLedger(t)
	memLedger, memStore := newTestLedger()

	type step struct {
		name string
		run  func(l *Ledger) (Entry, Balance, error)
	}
	steps := []step{
		{"recharge", func(l *Ledger) (Entry, Balance, error) { return l.Recharge(acct, amt("1000"), "r1", "idem-1", "首充") }},
		{"consume", func(l *Ledger) (Entry, Balance, error) { return l.Consume(acct, amt("153.25"), "order", "9001", "idem-2", "新购") }},
		{"freeze", func(l *Ledger) (Entry, Balance, error) { return l.Freeze(acct, amt("300"), "9002", "idem-3") }},
		{"unfreeze", func(l *Ledger) (Entry, Balance, error) { return l.Unfreeze(acct, amt("300"), "9002", "idem-4") }},
		{"refund", func(l *Ledger) (Entry, Balance, error) { return l.Refund(acct, amt("100"), "9001", "idem-5", "退订") }},
		{"adjust-credit", func(l *Ledger) (Entry, Balance, error) { return l.Adjust(acct, amt("0.5"), true, "ticket-7", "idem-6", "补偿") }},
		{"overdraft-rejected", func(l *Ledger) (Entry, Balance, error) { return l.Consume(acct, amt("100000"), "order", "9003", "idem-7", "") }},
	}

	for _, s := range steps {
		sqlEntry, sqlBal, sqlErr := s.run(sqlLedger)
		memEntry, memBal, memErr := s.run(memLedger)
		if (sqlErr == nil) != (memErr == nil) {
			t.Fatalf("%s: sql err=%v, memory err=%v", s.name, sqlErr, memErr)
		}
		if sqlErr != nil {
			continue
		}
		if sqlEntry.EntryID != memEntry.EntryID || sqlEntry.Amount.String() != memEntry.Amount.String() ||
			sqlEntry.BalanceAfter.String() != memEntry.BalanceAfter.String() ||
			sqlEntry.Type != memEntry.Type || sqlEntry.BizType != memEntry.BizType ||
			sqlEntry.BizKey != memEntry.BizKey || sqlEntry.IdempotencyKey != memEntry.IdempotencyKey {
			t.Fatalf("%s: sql entry %+v, memory entry %+v", s.name, sqlEntry, memEntry)
		}
		if sqlBal.Available.String() != memBal.Available.String() ||
			sqlBal.Frozen.String() != memBal.Frozen.String() ||
			sqlBal.Version != memBal.Version {
			t.Fatalf("%s: sql balance %+v, memory balance %+v", s.name, sqlBal, memBal)
		}
	}

	sqlEntries, err := sqlStore.ListEntries(acct)
	if err != nil {
		t.Fatal(err)
	}
	memEntries, err := memStore.ListEntries(acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(sqlEntries) != len(memEntries) {
		t.Fatalf("sql journal has %d entries, memory %d", len(sqlEntries), len(memEntries))
	}

	// And the final position must reconcile in both: parity of outcomes matters
	// less than both being internally provable.
	for name, l := range map[string]*Ledger{"sql": sqlLedger, "memory": memLedger} {
		res, err := l.Reconcile(acct)
		if err != nil {
			t.Fatalf("%s reconcile: %v", name, err)
		}
		if !res.Balanced() {
			t.Fatalf("%s ledger does not reconcile: %+v", name, res)
		}
	}
}

func TestSQLStoreIsEmptyForUnknownAccount(t *testing.T) {
	_, store := newSQLTestLedger(t)

	entries, err := store.ListEntries(acct)
	if err != nil {
		t.Fatal(err)
	}
	if entries == nil {
		t.Fatal("ListEntries returned nil; callers range over it and compare against empty")
	}
	if len(entries) != 0 {
		t.Fatalf("unknown account has %d entries", len(entries))
	}
	bal, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if !bal.Total().IsZero() {
		t.Fatalf("unknown account total = %s, want 0", bal.Total())
	}
}

// TestSQLStoreCheckConstraintIsTheLastLineOfDefence pins the database's own
// contribution: even if a caller bypasses the domain (as this test does), the
// DDL's CHECK refuses a negative available balance. Overdraft is an arrears
// state on the account, never a negative number in the ledger.
func TestSQLStoreCheckConstraintIsTheLastLineOfDefence(t *testing.T) {
	_, store := newSQLTestLedger(t)

	if _, _, err := New(store, func() time.Time { return testNow },
		func() int64 { return 1 }).Recharge(acct, amt("10"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	bal, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	next := bal
	next.Available = pricing.Amount(-1)
	next.Version = bal.Version + 1
	err = store.Apply(Entry{
		EntryID: 2, AccountID: acct, Type: EntryConsume, Amount: amt("11"),
		BalanceAfter: next.Available, BizType: "order", BizKey: "9001",
		IdempotencyKey: "idem-negative", OccurredAt: testNow,
	}, next, bal.Version)
	if err == nil {
		t.Fatal("the CHECK constraint accepted a negative available balance")
	}
	after, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if after.Available.String() != "10" {
		t.Fatalf("rejected write moved the balance: %s", after.Available)
	}
}
