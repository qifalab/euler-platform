package reservepack

import (
	"errors"
	"sort"
	"testing"
	"time"
)

// Purchase must not silently re-credit an existing pack: a duplicate packID
// under a DIFFERENT order key would reset a partially consumed pack to full.
// The same order key remains an idempotent replay.
func TestPurchaseRejectsExistingPackID(t *testing.T) {
	l, _ := newTestLedger()
	expire := testNow.Add(30 * 24 * time.Hour)
	if _, _, err := l.Purchase("pk-dup", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
		t.Fatal(err)
	}
	// Draw the pack down so an overwrite would be visible.
	if _, _, _, err := l.Consume("pk-dup", "euecs", yuan("400"), "bill-1", "c-1"); err != nil {
		t.Fatal(err)
	}

	// A different order key against the same pack id must be rejected.
	if _, _, err := l.Purchase("pk-dup", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-2"); !errors.Is(err, ErrPackExists) {
		t.Fatalf("re-purchase err = %v, want ErrPackExists", err)
	}
	p, _, _ := l.store.GetPack("pk-dup")
	if p.Remaining.String() != "600" {
		t.Fatalf("remaining = %s, want 600 (not reset to face value)", p.Remaining)
	}

	// The ORIGINAL order key still replays idempotently.
	pk, entry, err := l.Purchase("pk-dup", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1")
	if err != nil {
		t.Fatalf("idempotent replay must succeed: %v", err)
	}
	if entry.Type != EntryPurchase || pk.Remaining.String() != "600" {
		t.Fatalf("replay returned %+v / remaining %s", entry, pk.Remaining)
	}
}

// afterGetPackStore runs hook once, right after the first GetPack returns — the
// window between Purchase's existence check and its insert.
type afterGetPackStore struct {
	*MemoryStore
	hook func()
}

func (s *afterGetPackStore) GetPack(id string) (Pack, bool, error) {
	p, ok, err := s.MemoryStore.GetPack(id)
	if h := s.hook; h != nil {
		s.hook = nil
		h()
	}
	return p, ok, err
}

// A concurrent Purchase with the SAME order key that loses the insert race is
// an idempotent replay (it used to surface the version conflict); one with a
// DIFFERENT order key is ErrPackExists.
func TestConcurrentPurchaseSameKeyIsReplay(t *testing.T) {
	expire := testNow.Add(30 * 24 * time.Hour)
	for _, tc := range []struct {
		name, loserKey string
		wantErr        error
	}{
		{"same order key", "ord-1", nil},
		{"different order key", "ord-2", ErrPackExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			winner, mem := newTestLedger()
			var winEntry Entry
			racer := &afterGetPackStore{MemoryStore: mem}
			racer.hook = func() {
				var err error
				if _, winEntry, err = winner.Purchase("pk-race", 100123, "euecs", "euecs.pack", yuan("1000"), expire, "ord-1"); err != nil {
					t.Errorf("winner purchase: %v", err)
				}
			}
			var seq int64 = 100
			loser := New(racer, fixedNow, func() int64 { seq++; return seq })
			pk, entry, err := loser.Purchase("pk-race", 100123, "euecs", "euecs.pack", yuan("1000"), expire, tc.loserKey)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("racing replay: err = %v, want the idempotent replay", err)
				}
				if entry.EntryID != winEntry.EntryID || pk.Remaining.String() != "1000" {
					t.Fatalf("replay = %+v / remaining %s, want the winner's entry %d", entry, pk.Remaining, winEntry.EntryID)
				}
			}
			if entries, _ := mem.ListEntries("pk-race"); len(entries) != 1 {
				t.Fatalf("journal has %d entries, want 1 (credited once)", len(entries))
			}
		})
	}
}

// conflictOnPackStore always loses the optimistic-lock race for one pack and
// lists packs in id order (the memory store's map order is random).
type conflictOnPackStore struct {
	*MemoryStore
	hot string
}

func (s *conflictOnPackStore) Apply(e Entry, p Pack, v int) error {
	if p.PackID == s.hot {
		return ErrVersionConflict
	}
	return s.MemoryStore.Apply(e, p, v)
}

func (s *conflictOnPackStore) ListActiveByAccount(acct int64, t time.Time) ([]Pack, error) {
	packs, err := s.MemoryStore.ListActiveByAccount(acct, t)
	sort.Slice(packs, func(i, j int) bool { return packs[i].PackID < packs[j].PackID })
	return packs, err
}

// One pack that keeps losing its version race must not abort the account's
// sweep: the others expire now, the hot one on the next sweep.
func TestSweepExpiredSkipsVersionConflictedPack(t *testing.T) {
	base, mem := newTestLedger()
	past := testNow.Add(-time.Hour)
	for _, id := range []string{"pk-a-hot", "pk-b", "pk-c"} {
		if _, _, err := base.Purchase(id, 100123, "", "sku", yuan("100"), past, "ord-"+id); err != nil {
			t.Fatal(err)
		}
	}
	var seq int64 = 100
	l := New(&conflictOnPackStore{MemoryStore: mem, hot: "pk-a-hot"}, fixedNow, func() int64 { seq++; return seq })
	expired, err := l.SweepExpired(100123)
	if err != nil {
		t.Fatalf("sweep: %v (one conflicted pack aborted the account sweep)", err)
	}
	if len(expired) != 2 || expired[0] != "pk-b" || expired[1] != "pk-c" {
		t.Fatalf("expired = %v, want [pk-b pk-c]", expired)
	}
	if hot, _, _ := mem.GetPack("pk-a-hot"); hot.Status != StatusActive {
		t.Fatalf("hot pack status = %s, want still ACTIVE (retried next sweep)", hot.Status)
	}
	// The next sweep, once the contention is gone, expires it.
	if again, err := base.SweepExpired(100123); err != nil || len(again) != 1 || again[0] != "pk-a-hot" {
		t.Fatalf("second sweep = %v, %v; want [pk-a-hot]", again, err)
	}
}
