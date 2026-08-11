package ledger

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/starcloud/sc-platform/pricing"
)

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// memStore is an in-memory Store. Production writes the entry and the balance
// in one local transaction; this fake preserves that atomicity by mutating
// both under a single lock.
type memStore struct {
	mu       sync.Mutex
	balances map[int64]Balance
	entries  map[int64][]Entry
	byKey    map[string]Entry
	// failNextApply simulates a transaction failure.
	failNextApply bool
}

func newMemStore() *memStore {
	return &memStore{
		balances: make(map[int64]Balance),
		entries:  make(map[int64][]Entry),
		byKey:    make(map[string]Entry),
	}
}

func (m *memStore) GetBalance(accountID int64) (Balance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.balances[accountID]
	if !ok {
		return Balance{AccountID: accountID}, nil
	}
	return b, nil
}

func (m *memStore) Apply(entry Entry, newBalance Balance, expectedVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNextApply {
		m.failNextApply = false
		return errors.New("simulated transaction failure")
	}
	cur, ok := m.balances[entry.AccountID]
	curVersion := 0
	if ok {
		curVersion = cur.Version
	}
	if curVersion != expectedVersion {
		return ErrVersionConflict
	}
	m.balances[entry.AccountID] = newBalance
	m.entries[entry.AccountID] = append(m.entries[entry.AccountID], entry)
	m.byKey[keyFor(entry.AccountID, entry.IdempotencyKey)] = entry
	return nil
}

func (m *memStore) FindByIdempotencyKey(accountID int64, key string) (Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byKey[keyFor(accountID, key)]
	return e, ok, nil
}

func (m *memStore) ListEntries(accountID int64) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, len(m.entries[accountID]))
	copy(out, m.entries[accountID])
	return out, nil
}

func keyFor(accountID int64, key string) string {
	return string(rune(accountID)) + "|" + key
}

func newTestLedger() (*Ledger, *memStore) {
	store := newMemStore()
	var seq int64
	return New(store, func() time.Time { return testNow }, func() int64 {
		seq++
		return seq
	}), store
}

const acct = int64(100123)

func amt(s string) pricing.Amount { return pricing.MustParseAmount(s) }

// --- Basic movements ---

func TestRechargeThenConsume(t *testing.T) {
	l, _ := newTestLedger()

	_, bal, err := l.Recharge(acct, amt("1000"), "recharge-001", "idem-r1", "充值")
	if err != nil {
		t.Fatal(err)
	}
	if bal.Available.String() != "1000" {
		t.Fatalf("after recharge available = %s, want 1000", bal.Available)
	}

	_, bal, err = l.Consume(acct, amt("153"), "order", "9001", "idem-c1", "订单支付")
	if err != nil {
		t.Fatal(err)
	}
	if bal.Available.String() != "847" {
		t.Fatalf("after consume available = %s, want 847", bal.Available)
	}
	if bal.Version != 2 {
		t.Fatalf("version = %d, want 2", bal.Version)
	}
}

func TestInsufficientBalanceRejected(t *testing.T) {
	l, _ := newTestLedger()
	if _, _, err := l.Recharge(acct, amt("100"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	_, bal, err := l.Consume(acct, amt("150"), "order", "9001", "idem-c", "")
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
	// The failed attempt must leave the balance untouched.
	if bal.Available.String() != "100" {
		t.Fatalf("balance changed on failed consume: %s", bal.Available)
	}
}

// TestBalanceNeverGoesNegative guards the rule that overdraft is an arrears
// state on the account, never a negative number in the ledger. A negative
// balance silently extends credit no policy authorised.
func TestBalanceNeverGoesNegative(t *testing.T) {
	l, store := newTestLedger()
	if _, _, err := l.Recharge(acct, amt("10"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	for i, a := range []string{"5", "5", "5", "1"} {
		_, _, _ = l.Consume(acct, amt(a), "bill", "b", "idem-c"+string(rune('a'+i)), "")
	}
	bal, _ := store.GetBalance(acct)
	if bal.Available.IsNegative() {
		t.Fatalf("available balance went negative: %s", bal.Available)
	}
	if bal.Available.String() != "0" {
		t.Fatalf("available = %s, want 0 (two 5s fit, the rest rejected)", bal.Available)
	}
}

func TestNonPositiveAmountRejected(t *testing.T) {
	l, _ := newTestLedger()
	for _, a := range []string{"0", "-5"} {
		if _, _, err := l.Recharge(acct, amt(a), "r", "idem-"+a, ""); !errors.Is(err, ErrNonPositiveAmount) {
			t.Errorf("amount %s: expected ErrNonPositiveAmount, got %v", a, err)
		}
	}
}

func TestIdempotencyKeyRequired(t *testing.T) {
	l, _ := newTestLedger()
	if _, _, err := l.Recharge(acct, amt("100"), "r", "", ""); !errors.Is(err, ErrMissingIdempotency) {
		t.Fatalf("expected ErrMissingIdempotency, got %v", err)
	}
}

// --- Idempotency ---

func TestDuplicateApplicationIsNoOp(t *testing.T) {
	// A repeated charge carrying the same idempotency key must not move money
	// twice — this is what makes a retried channel callback safe.
	l, store := newTestLedger()
	if _, _, err := l.Recharge(acct, amt("1000"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}

	first, _, err := l.Consume(acct, amt("153"), "order", "9001", "idem-dup", "")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		prior, bal, err := l.Consume(acct, amt("153"), "order", "9001", "idem-dup", "")
		if !errors.Is(err, ErrDuplicateEntry) {
			t.Fatalf("repeat %d: expected ErrDuplicateEntry, got %v", i, err)
		}
		if prior.EntryID != first.EntryID {
			t.Fatalf("repeat returned a different entry: %d vs %d", prior.EntryID, first.EntryID)
		}
		if bal.Available.String() != "847" {
			t.Fatalf("repeat moved money: balance now %s", bal.Available)
		}
	}

	entries, _ := store.ListEntries(acct)
	if len(entries) != 2 {
		t.Fatalf("journal has %d entries, want 2 (recharge + one consume)", len(entries))
	}
}

// --- Concurrency ---

func TestConcurrentChargesCannotDoubleSpend(t *testing.T) {
	// Two charges racing from the same starting version: the optimistic lock
	// must let exactly one win. Last-write-wins here would let a customer
	// spend the same money twice.
	l, store := newTestLedger()
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
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of 2 concurrent 80-unit charges succeeded against a 100 balance; want exactly 1", succeeded)
	}
	bal, _ := store.GetBalance(acct)
	if bal.Available.String() != "20" {
		t.Fatalf("balance = %s, want 20 (only one charge applied)", bal.Available)
	}
}

// TestDoubleSpendUnderContention hammers the optimistic lock with many
// concurrent charges that together far exceed the balance.
//
// The single-pair race above can pass by luck if the goroutines happen not to
// interleave. This one makes the window wide: 50 goroutines each charging 10
// against a balance of 100 means at most 10 may succeed, and the surviving
// balance must be exactly 100 minus 10× the number that won. Any double-spend
// shows up as either too many successes or a balance that does not match.
//
// The race detector needs cgo, which is unavailable on this host, so this test
// substitutes volume for instrumentation.
func TestDoubleSpendUnderContention(t *testing.T) {
	const (
		goroutines  = 50
		charge      = "10"
		startBalance = "100"
	)
	l, store := newTestLedger()
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
			// A distinct idempotency key per goroutine, so nothing is
			// suppressed as a duplicate — every attempt is a real charge.
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

	bal, _ := store.GetBalance(acct)
	spent := 10 * okCount
	wantBalance := 100 - spent
	if got := bal.Available.String(); got != itoa(wantBalance) {
		t.Fatalf("balance = %s but %d charges succeeded (expected %d)", got, okCount, wantBalance)
	}
	if bal.Available.IsNegative() {
		t.Fatalf("balance went negative under contention: %s", bal.Available)
	}

	// The journal must still reproduce the balance exactly.
	res, err := l.Reconcile(acct)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Balanced() {
		t.Fatalf("ledger does not reconcile after contention: %+v", res)
	}
	t.Logf("%d/%d charges succeeded, balance %s, journal reconciles",
		okCount, goroutines, bal.Available)
}

// itoa avoids pulling strconv in just for the assertion message.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}

// --- Freeze / unfreeze ---

func TestFreezeMovesFundsWithoutChangingTotal(t *testing.T) {
	l, _ := newTestLedger()
	if _, _, err := l.Recharge(acct, amt("1000"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}

	_, bal, err := l.Freeze(acct, amt("300"), "9001", "idem-f1")
	if err != nil {
		t.Fatal(err)
	}
	if bal.Available.String() != "700" || bal.Frozen.String() != "300" {
		t.Fatalf("after freeze: available=%s frozen=%s, want 700/300", bal.Available, bal.Frozen)
	}
	if bal.Total().String() != "1000" {
		t.Fatalf("freeze changed the total: %s", bal.Total())
	}

	_, bal, err = l.Unfreeze(acct, amt("300"), "9001", "idem-u1")
	if err != nil {
		t.Fatal(err)
	}
	if bal.Available.String() != "1000" || !bal.Frozen.IsZero() {
		t.Fatalf("after unfreeze: available=%s frozen=%s, want 1000/0", bal.Available, bal.Frozen)
	}
}

func TestCannotFreezeMoreThanAvailable(t *testing.T) {
	l, _ := newTestLedger()
	if _, _, err := l.Recharge(acct, amt("100"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Freeze(acct, amt("150"), "9001", "idem-f"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestCannotUnfreezeMoreThanFrozen(t *testing.T) {
	l, _ := newTestLedger()
	if _, _, err := l.Recharge(acct, amt("100"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Freeze(acct, amt("50"), "9001", "idem-f"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Unfreeze(acct, amt("80"), "9001", "idem-u"); !errors.Is(err, ErrInsufficientFrozen) {
		t.Fatalf("expected ErrInsufficientFrozen, got %v", err)
	}
}

// --- Journal integrity ---

func TestJournalIsAppendOnlyAndReconciles(t *testing.T) {
	l, store := newTestLedger()

	_, _, _ = l.Recharge(acct, amt("1000"), "r1", "idem-1", "首充")
	_, _, _ = l.Consume(acct, amt("153"), "order", "9001", "idem-2", "新购")
	_, _, _ = l.Consume(acct, amt("47"), "bill", "b-202608", "idem-3", "小时账单")
	_, _, _ = l.Refund(acct, amt("100"), "9001", "idem-4", "退订退款")

	res, err := l.Reconcile(acct)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Balanced() {
		t.Fatalf("ledger does not reconcile: %+v", res)
	}
	if res.EntryCount != 4 {
		t.Fatalf("entry count = %d, want 4", res.EntryCount)
	}
	// 1000 - 153 - 47 + 100 = 900
	if res.JournalSum.String() != "900" {
		t.Fatalf("journal sum = %s, want 900", res.JournalSum)
	}
	bal, _ := store.GetBalance(acct)
	if bal.Available.String() != "900" {
		t.Fatalf("stored balance = %s, want 900", bal.Available)
	}
}

func TestReconcileDetectsTamperedJournal(t *testing.T) {
	// The BalanceAfter chain is what makes tampering detectable: editing an
	// entry's amount breaks the running total at that point. This is why the
	// journal carries a redundant running balance at all.
	l, store := newTestLedger()
	_, _, _ = l.Recharge(acct, amt("1000"), "r1", "idem-1", "")
	_, _, _ = l.Consume(acct, amt("200"), "order", "9001", "idem-2", "")
	_, _, _ = l.Consume(acct, amt("300"), "order", "9002", "idem-3", "")

	if res, _ := l.Reconcile(acct); !res.Balanced() {
		t.Fatalf("baseline should reconcile: %+v", res)
	}

	// Someone edits the middle entry to hide a charge.
	store.mu.Lock()
	store.entries[acct][1].Amount = amt("50")
	store.mu.Unlock()

	res, err := l.Reconcile(acct)
	if err != nil {
		t.Fatal(err)
	}
	if res.Balanced() {
		t.Fatal("tampered journal must NOT reconcile")
	}
	if res.ChainIntact {
		t.Fatal("BalanceAfter chain should be broken by the edit")
	}
	if res.FirstBreakAt == 0 {
		t.Fatal("reconcile should report where the chain first breaks")
	}
}

func TestReconcileAccountsForFrozenFunds(t *testing.T) {
	// Freezing debits available without the money leaving the account, so the
	// journal sum must reconcile against available + frozen, not available
	// alone. Getting this wrong makes every account with an in-flight order
	// look like it has a reconciliation difference.
	l, _ := newTestLedger()
	_, _, _ = l.Recharge(acct, amt("1000"), "r1", "idem-1", "")
	_, _, _ = l.Freeze(acct, amt("300"), "9001", "idem-2")

	res, err := l.Reconcile(acct)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Balanced() {
		t.Fatalf("frozen funds broke reconciliation: %+v", res)
	}
}

func TestEntryCarriesBizLinkage(t *testing.T) {
	// A statement line the customer cannot trace back to a cause is a support
	// ticket waiting to happen.
	l, store := newTestLedger()
	_, _, _ = l.Recharge(acct, amt("1000"), "recharge-abc", "idem-1", "支付宝充值")
	_, _, _ = l.Consume(acct, amt("153"), "order", "9001", "idem-2", "新购 SCECS")

	entries, _ := store.ListEntries(acct)
	for _, e := range entries {
		if e.BizType == "" || e.BizKey == "" {
			t.Errorf("entry %d has no business linkage: %+v", e.EntryID, e)
		}
		if e.IdempotencyKey == "" {
			t.Errorf("entry %d has no idempotency key", e.EntryID)
		}
		if e.OccurredAt.IsZero() {
			t.Errorf("entry %d has no timestamp", e.EntryID)
		}
	}
}

// --- Corrections ---

func TestAdjustmentIsANewEntryNotAnEdit(t *testing.T) {
	// A correction sits beside the original entry; the original stays as
	// written. An edited journal cannot be distinguished from a fraudulent one.
	l, store := newTestLedger()
	_, _, _ = l.Recharge(acct, amt("1000"), "r1", "idem-1", "")
	original, _, _ := l.Consume(acct, amt("200"), "order", "9001", "idem-2", "误扣")

	_, bal, err := l.Adjust(acct, amt("200"), true, "9001", "idem-3", "冲正误扣")
	if err != nil {
		t.Fatal(err)
	}
	if bal.Available.String() != "1000" {
		t.Fatalf("after correction available = %s, want 1000", bal.Available)
	}

	entries, _ := store.ListEntries(acct)
	if len(entries) != 3 {
		t.Fatalf("want 3 entries (recharge + charge + correction), got %d", len(entries))
	}
	// The original charge is still there, unmodified.
	if entries[1].EntryID != original.EntryID || entries[1].Amount.String() != "200" {
		t.Fatal("the original entry was modified rather than compensated")
	}
	if res, _ := l.Reconcile(acct); !res.Balanced() {
		t.Fatal("ledger should still reconcile after a correction")
	}
}

// --- Failure handling ---

func TestFailedApplyLeavesNoTrace(t *testing.T) {
	// If the transaction fails, neither the balance nor the journal may move —
	// a balance no journal explains is the worst possible state.
	l, store := newTestLedger()
	_, _, _ = l.Recharge(acct, amt("1000"), "r1", "idem-1", "")

	store.mu.Lock()
	store.failNextApply = true
	store.mu.Unlock()

	if _, _, err := l.Consume(acct, amt("153"), "order", "9001", "idem-2", ""); err == nil {
		t.Fatal("expected the simulated transaction failure to surface")
	}

	bal, _ := store.GetBalance(acct)
	if bal.Available.String() != "1000" {
		t.Fatalf("balance moved despite the failure: %s", bal.Available)
	}
	entries, _ := store.ListEntries(acct)
	if len(entries) != 1 {
		t.Fatalf("journal gained an entry despite the failure: %d entries", len(entries))
	}
	if res, _ := l.Reconcile(acct); !res.Balanced() {
		t.Fatal("ledger should still reconcile after a failed apply")
	}
}

func TestEntryTypeDirections(t *testing.T) {
	credits := []EntryType{EntryRecharge, EntryRefund, EntryAdjustCredit}
	debits := []EntryType{EntryConsume, EntryAdjustDebit}
	neutral := []EntryType{EntryFreeze, EntryUnfreeze}

	for _, tp := range credits {
		if tp.Direction() != 1 {
			t.Errorf("%s should credit", tp)
		}
	}
	for _, tp := range debits {
		if tp.Direction() != -1 {
			t.Errorf("%s should debit", tp)
		}
	}
	for _, tp := range neutral {
		if tp.Direction() != 0 {
			t.Errorf("%s should be neutral to the total", tp)
		}
	}
	if EntryType("MYSTERY").Valid() {
		t.Error("unknown entry type must not validate")
	}
}
