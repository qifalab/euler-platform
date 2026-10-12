package ledger

import (
	"errors"
	"strings"
	"testing"
)

// A reused idempotency key is a replay only when it describes the SAME
// movement. Treating any prior entry under the key as "already paid" let a
// 1-分 entry stand in for a ¥1,000,000 one (svc-billing pack purchases, svc-order
// payments pre-planted through a tenant-callable consume).
func TestReusedKeyWithDifferentMovementIsMismatch(t *testing.T) {
	l, store := newTestLedger()
	if _, _, err := l.Recharge(acct, amt("1000"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	first, _, err := l.Consume(acct, amt("1"), "pack", "ord-1", "idem-shared", "")
	if err != nil {
		t.Fatal(err)
	}

	for name, call := range map[string]func() (Entry, Balance, error){
		"amount": func() (Entry, Balance, error) {
			return l.Consume(acct, amt("999"), "pack", "ord-1", "idem-shared", "")
		},
		"type": func() (Entry, Balance, error) {
			return l.Refund(acct, amt("1"), "ord-1", "idem-shared", "")
		},
		"bizKey": func() (Entry, Balance, error) {
			return l.Consume(acct, amt("1"), "pack", "ord-2", "idem-shared", "")
		},
		"bizType": func() (Entry, Balance, error) {
			return l.Consume(acct, amt("1"), "order", "ord-1", "idem-shared", "")
		},
	} {
		prior, bal, err := call()
		if !errors.Is(err, ErrIdempotencyMismatch) {
			t.Fatalf("%s differs: err = %v, want ErrIdempotencyMismatch", name, err)
		}
		if errors.Is(err, ErrDuplicateEntry) {
			t.Fatalf("%s differs: a mismatch must not read as an idempotent success", name)
		}
		if prior.EntryID != first.EntryID || bal.Available.String() != "999" {
			t.Fatalf("%s differs: prior %d balance %s", name, prior.EntryID, bal.Available)
		}
	}

	// The exact same movement is still the idempotent replay it always was.
	if _, _, err := l.Consume(acct, amt("1"), "pack", "ord-1", "idem-shared", ""); !errors.Is(err, ErrDuplicateEntry) {
		t.Fatalf("exact replay: err = %v, want ErrDuplicateEntry", err)
	}
	if entries, _ := store.ListEntries(acct); len(entries) != 2 {
		t.Fatalf("journal has %d entries, want 2", len(entries))
	}
}

// racingStore hides the key from the pre-check once and then reports the
// unique-index collision from Apply — the window where two requests carrying
// one key race past the domain's lookup.
type racingStore struct {
	*memStore
	hide bool
}

func (s *racingStore) FindByIdempotencyKey(accountID int64, key string) (Entry, bool, error) {
	if s.hide {
		s.hide = false
		return Entry{}, false, nil
	}
	return s.memStore.FindByIdempotencyKey(accountID, key)
}

func (s *racingStore) Apply(entry Entry, newBalance Balance, expectedVersion int) error {
	if _, found, _ := s.memStore.FindByIdempotencyKey(entry.AccountID, entry.IdempotencyKey); found {
		return ErrDuplicateEntry
	}
	return s.memStore.Apply(entry, newBalance, expectedVersion)
}

func TestRacedDuplicateIsCheckedLikeTheReplay(t *testing.T) {
	store := &racingStore{memStore: newMemStore()}
	l := New(store, nil, nil)
	if _, _, err := l.Recharge(acct, amt("100"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	won, _, err := l.Consume(acct, amt("1"), "order", "9001", "idem-race", "")
	if err != nil {
		t.Fatal(err)
	}

	store.hide = true
	if _, _, err := l.Consume(acct, amt("50"), "order", "9001", "idem-race", ""); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("raced different movement: err = %v, want ErrIdempotencyMismatch", err)
	}
	store.hide = true
	prior, bal, err := l.Consume(acct, amt("1"), "order", "9001", "idem-race", "")
	if !errors.Is(err, ErrDuplicateEntry) || prior.EntryID != won.EntryID {
		t.Fatalf("raced same movement: entry %d err %v, want the winner %d + ErrDuplicateEntry", prior.EntryID, err, won.EntryID)
	}
	if bal.Available.String() != "99" {
		t.Fatalf("balance = %s, want 99", bal.Available)
	}
}

// ledger_entry.idempotency_key and biz_key are VARCHAR(64): a longer key must be
// refused before the database truncates or rejects it.
func TestOversizedKeysRejected(t *testing.T) {
	l, store := newTestLedger()
	long := strings.Repeat("k", MaxKeyLen+1)
	if _, _, err := l.Recharge(acct, amt("10"), "r", long, ""); !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("long idempotency key: err = %v, want ErrKeyTooLong", err)
	}
	if _, _, err := l.Consume(acct, amt("10"), "order", long, "idem-ok", ""); !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("long biz key: err = %v, want ErrKeyTooLong", err)
	}
	if _, _, err := l.Recharge(acct, amt("10"), "r", strings.Repeat("k", MaxKeyLen), ""); err != nil {
		t.Fatalf("a key of exactly %d bytes must be accepted: %v", MaxKeyLen, err)
	}
	if entries, _ := store.ListEntries(acct); len(entries) != 1 {
		t.Fatalf("journal has %d entries, want 1", len(entries))
	}
}

func TestFindEntry(t *testing.T) {
	l, _ := newTestLedger()
	applied, _, err := l.Recharge(acct, amt("10"), "r", "idem-find", "")
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := l.FindEntry(acct, "idem-find")
	if err != nil || !ok || got.EntryID != applied.EntryID {
		t.Fatalf("FindEntry = %+v %v %v", got, ok, err)
	}
	if _, ok, _ := l.FindEntry(acct, "idem-missing"); ok {
		t.Fatal("FindEntry found a key that was never applied")
	}
}

// The same rule holds against MySQL, whose unique index is the final arbiter.
func TestSQLStoreReusedKeyWithDifferentMovementIsMismatch(t *testing.T) {
	l, store := newSQLTestLedger(t)
	if _, _, err := l.Recharge(acct, amt("100"), "r", "idem-r", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Consume(acct, amt("0.01"), "order", "9001", "order-pay-9001", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Consume(acct, amt("2160"), "order", "9001", "order-pay-9001", ""); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("err = %v, want ErrIdempotencyMismatch", err)
	}
	if _, _, err := l.Consume(acct, amt("0.01"), "order", "9001", "order-pay-9001", ""); !errors.Is(err, ErrDuplicateEntry) {
		t.Fatalf("exact replay: err = %v, want ErrDuplicateEntry", err)
	}
	bal, err := store.GetBalance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if bal.Available.String() != "99.99" {
		t.Fatalf("balance = %s, want 99.99", bal.Available)
	}
}
