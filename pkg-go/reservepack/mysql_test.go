package reservepack

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

// newSQLTestLedger builds the pack ledger on the real MySQL store, with the same
// fixed clock and monotonic entry ids as the in-memory suite.
//
// The DDL applied is services/svc-billing/sql — resource_pack lives there
// (V2__trade_db_resource_pack_schema.sql), not in a table invented for the test.
func newSQLTestLedger(t *testing.T) (*Ledger, *SQLStore) {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-billing"))
	store := NewSQLStore(db)
	var seq int64
	l := New(store, fixedNow, func() int64 {
		seq++
		return seq
	})
	return l, store
}

const sqlAcct = int64(100123)

func TestSQLStorePackLifecycle(t *testing.T) {
	l, store := newSQLTestLedger(t)
	expire := testNow.Add(30 * 24 * time.Hour)

	// Absent pack: (_, false, nil), not an error.
	if _, ok, err := store.GetPack("pk-life"); err != nil || ok {
		t.Fatalf("absent pack: ok=%v err=%v, want false/nil", ok, err)
	}

	pack, _, err := l.Purchase("pk-life", sqlAcct, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1")
	if err != nil {
		t.Fatal(err)
	}
	if pack.Version != 1 || pack.Status != StatusActive {
		t.Fatalf("purchased pack = %+v, want version 1 ACTIVE", pack)
	}

	// Read it back through SQL: this is where a wrong column, a status code
	// mapped to the wrong constant, or a NULL expire_at turns up.
	got, ok, err := store.GetPack("pk-life")
	if err != nil || !ok {
		t.Fatalf("get purchased pack: ok=%v err=%v", ok, err)
	}
	if got.AccountID != sqlAcct || got.ProductCode != "euecs" || got.SKUCode != "euecs.pack" {
		t.Fatalf("pack fields lost in the round trip: %+v", got)
	}
	if got.FaceValue.String() != "1000" || got.Remaining.String() != "1000" || got.Version != 1 {
		t.Fatalf("pack amounts lost: %+v", got)
	}
	if got.Status != StatusActive {
		t.Fatalf("status round-trip = %s, want ACTIVE", got.Status)
	}
	if !got.ExpireAt.Equal(expire) {
		t.Fatalf("expire_at = %s, want %s", got.ExpireAt, expire)
	}
	if !got.PurchasedAt.Equal(testNow) {
		t.Fatalf("purchased_at = %s, want %s", got.PurchasedAt, testNow)
	}

	// A consume that runs the pack dry flips it to the EXHAUSTED terminal state.
	pack, entry, shortfall, err := l.Consume("pk-life", "euecs", yuan("1000"), "chg-1", "consume-1")
	if err != nil {
		t.Fatal(err)
	}
	if !shortfall.IsZero() || !pack.Remaining.IsZero() || pack.Status != StatusExhausted {
		t.Fatalf("after full consume: pack=%+v shortfall=%s", pack, shortfall)
	}
	if entry.Balance.String() != "0" || entry.Type != EntryConsume {
		t.Fatalf("consume entry = %+v", entry)
	}

	entries, err := store.ListEntries("pk-life")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("journal has %d entries, want 2 (purchase + consume)", len(entries))
	}
	for i, e := range entries {
		if e.EntryID != int64(i+1) {
			t.Fatalf("journal out of entry_id order: %+v", entries)
		}
	}
	if entries[0].Type != EntryPurchase || entries[0].Balance.String() != "1000" {
		t.Fatalf("purchase entry = %+v", entries[0])
	}
}

// TestSQLStoreExpireAtNullMeansNoExpiry pins the DDL's nullable expire_at onto
// the domain's zero time. Getting this backwards would make every never-expiring
// pack look long overdue to the sweeper.
func TestSQLStoreExpireAtNullMeansNoExpiry(t *testing.T) {
	l, store := newSQLTestLedger(t)

	if _, _, err := l.Purchase("pk-noexp", sqlAcct, "euecs", "euecs.pack", yuan("10"), time.Time{}, "ord-1"); err != nil {
		t.Fatal(err)
	}
	pack, ok, err := store.GetPack("pk-noexp")
	if err != nil || !ok {
		t.Fatalf("get pack: ok=%v err=%v", ok, err)
	}
	if !pack.ExpireAt.IsZero() {
		t.Fatalf("expire_at = %s, want the zero time (NULL = never expires)", pack.ExpireAt)
	}
	if pack.Available(testNow).String() != "10" {
		t.Fatalf("a pack with no expiry contributed %s, want 10", pack.Available(testNow))
	}
}

func TestSQLStoreRoundTripsMicroPrecision(t *testing.T) {
	l, store := newSQLTestLedger(t)

	// DECIMAL(18,6) matches pricing.Amount's micro-unit scale exactly; a
	// truncated digit here would be invisible until an invoice did not add up.
	if _, _, err := l.Purchase("pk-micro", sqlAcct, "", "euecs.pack", yuan("0.000001"), testNow.Add(time.Hour), "ord-1"); err != nil {
		t.Fatal(err)
	}
	pack, _, err := store.GetPack("pk-micro")
	if err != nil {
		t.Fatal(err)
	}
	if pack.FaceValue.String() != "0.000001" || pack.Remaining.String() != "0.000001" {
		t.Fatalf("face=%s remaining=%s, want 0.000001", pack.FaceValue, pack.Remaining)
	}
}

func TestSQLStoreApplyReportsVersionConflict(t *testing.T) {
	l, store := newSQLTestLedger(t)
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-race", sqlAcct, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatal(err)
	}
	pack, _, err := store.GetPack("pk-race")
	if err != nil {
		t.Fatal(err)
	}

	// A stale writer must be told it lost the race — NOT that the pack is
	// missing. The two were once conflated, and it sent a retryable condition
	// down a 500 path.
	stale := pack
	stale.Remaining = yuan("500")
	stale.Version = pack.Version - 1
	err = store.Apply(Entry{
		EntryID: 99, PackID: "pk-race", Type: EntryConsume, Amount: yuan("500"),
		Balance: yuan("500"), BizKey: "chg-stale", IdempotencyKey: "idem-stale", CreatedAt: testNow,
	}, stale, stale.Version)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale apply = %v, want ErrVersionConflict", err)
	}
	if errors.Is(err, ErrPackNotFound) {
		t.Fatalf("a lost race was reported as a missing pack: %v", err)
	}
	// The shared retry helper must recognise it too, or a benign race becomes a
	// fatal error for any store wrapped in storage.WithVersionRetry.
	if !errors.Is(err, storage.ErrVersionConflict) {
		t.Fatalf("conflict invisible to storage.WithVersionRetry: %v", err)
	}

	after, _, err := store.GetPack("pk-race")
	if err != nil {
		t.Fatal(err)
	}
	if after.Remaining.String() != "1000" || after.Version != pack.Version {
		t.Fatalf("rejected write moved the pack: %+v", after)
	}
}

func TestSQLStoreApplyReportsMissingPack(t *testing.T) {
	_, store := newSQLTestLedger(t)

	// expectedVersion != 0 against a pack that does not exist is genuinely
	// "not found" — the other half of the distinction above.
	err := store.Apply(Entry{
		EntryID: 1, PackID: "pk-ghost", Type: EntryConsume, Amount: yuan("1"),
		Balance: yuan("0"), BizKey: "chg", IdempotencyKey: "idem-ghost", CreatedAt: testNow,
	}, Pack{PackID: "pk-ghost", AccountID: sqlAcct, Status: StatusActive, Version: 2}, 1)
	if !errors.Is(err, ErrPackNotFound) {
		t.Fatalf("apply to absent pack = %v, want ErrPackNotFound", err)
	}
}

func TestSQLStoreDuplicateIdempotencyKeyIsRejected(t *testing.T) {
	l, store := newSQLTestLedger(t)
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-dup", sqlAcct, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.Consume("pk-dup", "euecs", yuan("100"), "chg-1", "consume-x"); err != nil {
		t.Fatal(err)
	}
	pack, _, err := store.GetPack("pk-dup")
	if err != nil {
		t.Fatal(err)
	}

	// Bypass the domain's pre-check: uk_idem(account_id, pack_id, key), not Go
	// code, is what makes a retried settlement a no-op instead of a double debit.
	next := pack
	next.Remaining = pack.Remaining.Sub(yuan("100"))
	next.Version = pack.Version + 1
	err = store.Apply(Entry{
		EntryID: 77, PackID: "pk-dup", Type: EntryConsume, Amount: yuan("100"),
		Balance: next.Remaining, BizKey: "chg-2", IdempotencyKey: "consume-x", CreatedAt: testNow,
	}, next, pack.Version)
	if !errors.Is(err, ErrDuplicateEntry) {
		t.Fatalf("duplicate key = %v, want ErrDuplicateEntry", err)
	}

	after, _, err := store.GetPack("pk-dup")
	if err != nil {
		t.Fatal(err)
	}
	if after.Remaining.String() != "900" || after.Version != pack.Version {
		t.Fatalf("duplicate write moved the pack: %+v", after)
	}
	entries, err := store.ListEntries("pk-dup")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("journal has %d entries, want 2", len(entries))
	}
}

// TestSQLStoreApplyIsAtomic: a failure in either half of the transaction must
// leave neither behind. A pack debited without a journal row is quota that
// vanished with nothing to explain it.
func TestSQLStoreApplyIsAtomic(t *testing.T) {
	l, store := newSQLTestLedger(t)
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-atomic", sqlAcct, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatal(err)
	}
	pack, _, err := store.GetPack("pk-atomic")
	if err != nil {
		t.Fatal(err)
	}

	// entry_id 1 already exists, so the journal insert fails after the pack
	// UPDATE has run inside the same transaction.
	next := pack
	next.Remaining = pack.Remaining.Sub(yuan("100"))
	next.Version = pack.Version + 1
	err = store.Apply(Entry{
		EntryID: 1, PackID: "pk-atomic", Type: EntryConsume, Amount: yuan("100"),
		Balance: next.Remaining, BizKey: "chg", IdempotencyKey: "idem-atomic", CreatedAt: testNow,
	}, next, pack.Version)
	if err == nil {
		t.Fatal("expected the duplicate entry_id to fail the transaction")
	}

	after, _, err := store.GetPack("pk-atomic")
	if err != nil {
		t.Fatal(err)
	}
	if after.Remaining.String() != "1000" || after.Version != pack.Version {
		t.Fatalf("a failed apply moved the pack: %+v", after)
	}
}

// TestSQLStoreListActiveByAccount checks the predicate the billing waterfall
// runs on every cycle, including the case that looks like a bug and is not: a
// past-deadline pack is still listed so the sweeper can settle it.
func TestSQLStoreListActiveByAccount(t *testing.T) {
	l, store := newSQLTestLedger(t)
	future := testNow.Add(30 * 24 * time.Hour)
	past := testNow.Add(-time.Hour)

	// Active with quota, active but past its deadline, exhausted, and another
	// tenant's pack.
	if _, _, err := l.Purchase("pk-live", sqlAcct, "", "sku", yuan("100"), future, "ord-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Purchase("pk-overdue", sqlAcct, "", "sku", yuan("100"), past, "ord-2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Purchase("pk-spent", sqlAcct, "", "sku", yuan("100"), future, "ord-3"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.Consume("pk-spent", "", yuan("100"), "chg", "consume-spent"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Purchase("pk-other", int64(999), "", "sku", yuan("100"), future, "ord-4"); err != nil {
		t.Fatal(err)
	}

	packs, err := store.ListActiveByAccount(sqlAcct, testNow)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range packs {
		got[p.PackID] = true
	}
	if !got["pk-live"] {
		t.Error("an active pack with quota was not listed")
	}
	if !got["pk-overdue"] {
		t.Error("a past-deadline pack must stay listed for the expiry sweeper")
	}
	if got["pk-spent"] {
		t.Error("an exhausted (terminal) pack was listed")
	}
	if got["pk-other"] {
		t.Error("another account's pack leaked into the list")
	}
}

// TestSQLStoreConcurrentConsumeCannotOverspend is the in-memory suite's
// contention test on real MySQL, where the optimistic lock is an actual
// UPDATE ... WHERE version = ? and the row locks are the database's.
func TestSQLStoreConcurrentConsumeCannotOverspend(t *testing.T) {
	l, store := newSQLTestLedger(t)
	expire := testNow.Add(30 * 24 * time.Hour)

	const (
		face       = "1000"
		perConsume = "100"
		goroutines = 20
	)
	if _, _, err := l.Purchase("pk-contend", sqlAcct, "euecs", "euecs.pack", yuan(face), expire, "ord-1"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, err := l.Consume("pk-contend", "euecs", yuan(perConsume), "chg-1", fmt.Sprintf("consume-%d", i))
			switch {
			case err == nil,
				errors.Is(err, ErrInsufficientQuota),
				errors.Is(err, ErrPackExhausted),
				errors.Is(err, ErrPackTerminal),
				// The domain retries a lost race three times; under this much
				// contention a caller can still surface the conflict. That is a
				// retryable condition, not an overspend — the assertions below
				// are about quota, which must not move either way.
				errors.Is(err, ErrVersionConflict):
			default:
				t.Errorf("consume %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	pack, _, err := store.GetPack("pk-contend")
	if err != nil {
		t.Fatal(err)
	}
	if pack.Remaining.IsNegative() {
		t.Fatalf("overspent: remaining = %s", pack.Remaining)
	}
	entries, err := store.ListEntries("pk-contend")
	if err != nil {
		t.Fatal(err)
	}
	var consumed, credited pricing.Amount
	for _, e := range entries {
		switch e.Type {
		case EntryConsume:
			consumed = consumed.Add(e.Amount)
		case EntryPurchase:
			credited = credited.Add(e.Amount)
		}
	}
	if consumed > credited {
		t.Fatalf("consumed %s of a %s pack", consumed, credited)
	}
	// The journal and the pack must agree: remaining is face minus consumed,
	// which is the invariant a customer's invoice depends on.
	if want := credited.Sub(consumed); pack.Remaining != want {
		t.Fatalf("pack remaining %s does not match journal %s", pack.Remaining, want)
	}
	t.Logf("%d/%d consumes of %s succeeded, remaining %s", okConsumes(entries), goroutines, perConsume, pack.Remaining)
}

func okConsumes(entries []Entry) int {
	n := 0
	for _, e := range entries {
		if e.Type == EntryConsume {
			n++
		}
	}
	return n
}

// TestSQLStoreSweepExpired drives the expiry path end to end against the real
// schema: the sweeper has to see the overdue pack through
// ListActiveByAccount, forfeit its residual quota, and write the EXPIRE row.
func TestSQLStoreSweepExpired(t *testing.T) {
	l, store := newSQLTestLedger(t)
	past := testNow.Add(-time.Hour)
	future := testNow.Add(30 * 24 * time.Hour)

	if _, _, err := l.Purchase("pk-old", sqlAcct, "", "sku", yuan("500"), past, "ord-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Purchase("pk-fresh", sqlAcct, "", "sku", yuan("500"), future, "ord-2"); err != nil {
		t.Fatal(err)
	}

	expired, err := l.SweepExpired(sqlAcct)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0] != "pk-old" {
		t.Fatalf("sweep expired %v, want just pk-old", expired)
	}

	pack, _, err := store.GetPack("pk-old")
	if err != nil {
		t.Fatal(err)
	}
	if pack.Status != StatusExpired || !pack.Remaining.IsZero() {
		t.Fatalf("swept pack = %+v, want EXPIRED with no remaining quota", pack)
	}
	entries, err := store.ListEntries("pk-old")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Type != EntryExpire || entries[1].Balance.String() != "0" {
		t.Fatalf("expire journal = %+v", entries)
	}
	// Sweeping twice must not write a second forfeiture row.
	if again, err := l.SweepExpired(sqlAcct); err != nil || len(again) != 0 {
		t.Fatalf("second sweep = %+v (err %v), want none", again, err)
	}
}

// TestSQLStoreParityWithMemoryStore runs one scripted sequence through both
// stores and requires the same outcome: where the tests above check each store
// against the contract, this checks the two implementations against each other.
func TestSQLStoreParityWithMemoryStore(t *testing.T) {
	sqlLedger, sqlStore := newSQLTestLedger(t)
	memLedger, memStore := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)

	type step struct {
		name string
		run  func(l *Ledger) error
	}
	steps := []step{
		{"purchase", func(l *Ledger) error {
			_, _, err := l.Purchase("pk-p", sqlAcct, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1")
			return err
		}},
		{"purchase-replay", func(l *Ledger) error {
			_, _, err := l.Purchase("pk-p", sqlAcct, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1")
			return err
		}},
		{"consume-partial", func(l *Ledger) error {
			_, _, _, err := l.Consume("pk-p", "euecs", yuan("400"), "chg-1", "consume-1")
			return err
		}},
		{"consume-wrong-product", func(l *Ledger) error {
			_, _, _, err := l.Consume("pk-p", "euoss", yuan("100"), "chg-2", "consume-2")
			return err
		}},
		{"refund", func(l *Ledger) error {
			_, _, err := l.Refund("pk-p", yuan("100"), "chg-1", "refund-1")
			return err
		}},
		{"consume-rest", func(l *Ledger) error {
			_, _, _, err := l.Consume("pk-p", "euecs", yuan("700"), "chg-3", "consume-3")
			return err
		}},
	}

	for _, s := range steps {
		sqlErr := s.run(sqlLedger)
		memErr := s.run(memLedger)
		if (sqlErr == nil) != (memErr == nil) {
			t.Fatalf("%s: sql err=%v, memory err=%v", s.name, sqlErr, memErr)
		}
		if sqlErr != nil &&
			!(errors.Is(sqlErr, ErrInsufficientQuota) && errors.Is(memErr, ErrInsufficientQuota)) &&
			!(errors.Is(sqlErr, ErrPackExhausted) && errors.Is(memErr, ErrPackExhausted)) {
			t.Fatalf("%s: sql err=%v, memory err=%v", s.name, sqlErr, memErr)
		}
	}

	sqlPack, _, err := sqlStore.GetPack("pk-p")
	if err != nil {
		t.Fatal(err)
	}
	memPack, _, err := memStore.GetPack("pk-p")
	if err != nil {
		t.Fatal(err)
	}
	if sqlPack.Remaining != memPack.Remaining || sqlPack.Status != memPack.Status ||
		sqlPack.Version != memPack.Version || sqlPack.FaceValue != memPack.FaceValue {
		t.Fatalf("sql pack %+v, memory pack %+v", sqlPack, memPack)
	}

	sqlEntries, err := sqlStore.ListEntries("pk-p")
	if err != nil {
		t.Fatal(err)
	}
	memEntries, err := memStore.ListEntries("pk-p")
	if err != nil {
		t.Fatal(err)
	}
	if len(sqlEntries) != len(memEntries) {
		t.Fatalf("sql journal has %d entries, memory %d", len(sqlEntries), len(memEntries))
	}
	for i := range sqlEntries {
		a, b := sqlEntries[i], memEntries[i]
		if a.EntryID != b.EntryID || a.Type != b.Type || a.Amount != b.Amount ||
			a.Balance != b.Balance || a.BizKey != b.BizKey || a.IdempotencyKey != b.IdempotencyKey {
			t.Fatalf("journal entry %d: sql %+v, memory %+v", i, a, b)
		}
	}
}
