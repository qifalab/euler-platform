package quota

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// The test memStore implements the keyed operations the way a memory backend
// must: record and change under the one lock that guards the counters.
type idemKey struct {
	accountID int64
	op        IdempotencyOp
	key       string
}

func (m *memStore) GetIdempotency(accountID int64, op IdempotencyOp, key string) (IdempotencyRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.idem[idemKey{accountID, op, key}]
	return rec, ok, nil
}

func (m *memStore) applyOnceLocked(u Usage, expectedVersion int, rec IdempotencyRecord) error {
	if m.idem == nil {
		m.idem = make(map[idemKey]IdempotencyRecord)
	}
	k := idemKey{rec.AccountID, rec.Op, rec.Key}
	if _, taken := m.idem[k]; taken {
		return ErrIdempotencyKeyTaken
	}
	uk := usageKey(u.AccountID, u.QuotaCode, u.Region)
	if m.usage[uk].Version != expectedVersion {
		return ErrVersionConflict
	}
	u.Version = expectedVersion + 1
	m.usage[uk] = u
	m.idem[k] = rec
	return nil
}

func (m *memStore) OccupyOnce(u Usage, expectedVersion int, tok Token, rec IdempotencyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.applyOnceLocked(u, expectedVersion, rec); err != nil {
		return err
	}
	m.tokens[tok.TokenID] = tok
	return nil
}

func (m *memStore) UpdateUsageOnce(u Usage, expectedVersion int, rec IdempotencyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyOnceLocked(u, expectedVersion, rec)
}

// retryVersion is the caller's half of the optimistic protocol.
func retryVersion(fn func() error) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = fn(); !errors.Is(err, ErrVersionConflict) {
			return err
		}
	}
	return err
}

// runKeyedOccupyContract: a key names one reservation forever.
func runKeyedOccupyContract(t *testing.T, m *Manager, s Store) {
	tok, replayed, err := m.CheckAndOccupyOnce(acct, "quota_euecs_instance", "cn-north-1", 3, "order-1", "k-1")
	if err != nil || replayed {
		t.Fatalf("first occupy: replayed=%v err=%v", replayed, err)
	}
	again, replayed, err := m.CheckAndOccupyOnce(acct, "quota_euecs_instance", "cn-north-1", 3, "order-1", "k-1")
	if err != nil || !replayed || !reflect.DeepEqual(again, tok) {
		t.Fatalf("replay = %+v replayed=%v err=%v, want %+v", again, replayed, err, tok)
	}
	u, _ := s.GetUsage(acct, "quota_euecs_instance", "cn-north-1")
	if u.Occupying != 3 {
		t.Fatalf("Occupying = %d after replay, want 3", u.Occupying)
	}
	// A different request under the same key is refused, not replayed.
	if _, _, err := m.CheckAndOccupyOnce(acct, "quota_euecs_instance", "cn-north-1", 4, "order-1", "k-1"); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("reused key, other amount = %v, want ErrIdempotencyMismatch", err)
	}
	// Keys are per account.
	if _, replayed, err := m.CheckAndOccupyOnce(acct+1, "quota_euecs_instance", "cn-north-1", 3, "order-1", "k-1"); err != nil || replayed {
		t.Fatalf("same key, other account: replayed=%v err=%v", replayed, err)
	}

	// Committed: the replay still names the original reservation and does not
	// reserve again.
	if err := m.CommitOccupy(tok.TokenID); err != nil {
		t.Fatal(err)
	}
	again, replayed, err = m.CheckAndOccupyOnce(acct, "quota_euecs_instance", "cn-north-1", 3, "order-1", "k-1")
	if err != nil || !replayed || again.TokenID != tok.TokenID {
		t.Fatalf("replay after commit = %+v replayed=%v err=%v", again, replayed, err)
	}
	u, _ = s.GetUsage(acct, "quota_euecs_instance", "cn-north-1")
	if u.Used != 3 || u.Occupying != 0 {
		t.Fatalf("after commit + replay: used=%d occupying=%d, want 3/0", u.Used, u.Occupying)
	}

	// Free once: the second call with the key is a replay.
	rec, replayed, err := m.FreeCommitted(acct, "quota_euecs_instance", "cn-north-1", 2, "res-1", "free-1")
	if err != nil || replayed || rec.Amount != 2 {
		t.Fatalf("free: %+v replayed=%v err=%v", rec, replayed, err)
	}
	if _, replayed, err := m.FreeCommitted(acct, "quota_euecs_instance", "cn-north-1", 2, "res-1", "free-1"); err != nil || !replayed {
		t.Fatalf("free replay: replayed=%v err=%v", replayed, err)
	}
	if _, _, err := m.FreeCommitted(acct, "quota_euecs_instance", "cn-north-1", 1, "res-1", "free-1"); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("free key reused for another amount = %v", err)
	}
	u, _ = s.GetUsage(acct, "quota_euecs_instance", "cn-north-1")
	if u.Used != 1 {
		t.Fatalf("Used = %d after one free of 2, want 1", u.Used)
	}
	// Freeing more than is used floors at zero.
	if _, _, err := m.FreeCommitted(acct, "quota_euecs_instance", "cn-north-1", 5, "res-2", "free-2"); err != nil {
		t.Fatal(err)
	}
	u, _ = s.GetUsage(acct, "quota_euecs_instance", "cn-north-1")
	if u.Used != 0 {
		t.Fatalf("Used = %d, want 0 (never negative)", u.Used)
	}
	// An occupy key and a free key never collide; a GLOBAL quota records "*".
	g, _, err := m.CheckAndOccupyOnce(acct, "quota_euoss_bucket", "cn-east-1", 1, "b", "free-1")
	if err != nil || g.Region != "*" {
		t.Fatalf("global occupy under a free key's name: %+v %v", g, err)
	}
	for _, bad := range []string{"", string(make([]byte, MaxIdempotencyKeyLen+1))} {
		if _, _, err := m.CheckAndOccupyOnce(acct, "quota_euecs_instance", "cn-north-1", 1, "b", bad); !errors.Is(err, ErrInvalidIdempotencyKey) {
			t.Fatalf("key of length %d = %v, want ErrInvalidIdempotencyKey", len(bad), err)
		}
	}
}

func TestKeyedOccupyAndFree(t *testing.T) {
	m, s := newManager(base)
	runKeyedOccupyContract(t, m, s)
}

func TestKeyedOperationsNeedIdempotentStore(t *testing.T) {
	m := NewManager(struct{ Store }{newStore()}, nil, nil)
	if _, _, err := m.CheckAndOccupyOnce(acct, "quota_euecs_instance", "cn-north-1", 1, "", "k"); !errors.Is(err, ErrIdempotencyUnsupported) {
		t.Fatalf("plain store = %v, want ErrIdempotencyUnsupported", err)
	}
}

// runKeyedOccupyRace: many concurrent first attempts with one key reserve once
// and all get the same reservation — also when the quota only has room for one.
func runKeyedOccupyRace(t *testing.T, managers []*Manager, s Store, limitRoom int) {
	var wg sync.WaitGroup
	var fresh int32
	ids := make([]string, 16)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := managers[i%len(managers)]
			var tok Token
			var replayed bool
			err := retryVersion(func() error {
				var err error
				tok, replayed, err = m.CheckAndOccupyOnce(acct, "quota_euecs_instance", "cn-north-1", limitRoom, "order-race", "k-race")
				return err
			})
			if err != nil {
				t.Errorf("attempt %d: %v", i, err)
				return
			}
			if !replayed {
				atomic.AddInt32(&fresh, 1)
			}
			ids[i] = tok.TokenID
		}(i)
	}
	wg.Wait()
	if fresh != 1 {
		t.Fatalf("%d attempts reserved, want exactly 1", fresh)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("attempts got different reservations: %v", ids)
		}
	}
	u, _ := s.GetUsage(acct, "quota_euecs_instance", "cn-north-1")
	if u.Occupying != limitRoom {
		t.Fatalf("Occupying = %d, want %d (one reservation)", u.Occupying, limitRoom)
	}
}

func TestKeyedOccupyConcurrentFirstAttempts(t *testing.T) {
	m, s := newManager(base)
	// Room for exactly one: a loser that re-reads usage after the winner sees a
	// full quota and must still get the winner's reservation, not Exceeded.
	runKeyedOccupyRace(t, []*Manager{m}, s, 20)
}

// FreeCommitted under concurrent replays decrements once.
func runKeyedFreeRace(t *testing.T, managers []*Manager, s Store) {
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := managers[i%len(managers)]
			if err := retryVersion(func() error {
				_, _, err := m.FreeCommitted(acct, "quota_euecs_instance", "cn-north-1", 2, "res-race", "free-race")
				return err
			}); err != nil {
				t.Errorf("free %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestKeyedFreeConcurrentReplays(t *testing.T) {
	m, s := newManager(base)
	tok, err := m.CheckAndOccupy(acct, "quota_euecs_instance", "cn-north-1", 5, "o")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CommitOccupy(tok.TokenID); err != nil {
		t.Fatal(err)
	}
	runKeyedFreeRace(t, []*Manager{m}, s)
	if u, _ := s.GetUsage(acct, "quota_euecs_instance", "cn-north-1"); u.Used != 3 {
		t.Fatalf("Used = %d, want 3 (freed once)", u.Used)
	}
}

func seqTokens(prefix string) func() string {
	var n int64
	return func() string { return fmt.Sprintf("%s-%d", prefix, atomic.AddInt64(&n, 1)) }
}

// --- MySQL ---------------------------------------------------------------

func TestSQLKeyedOccupyAndFree(t *testing.T) {
	store, _, m, _ := newSQLQuota(t)
	runKeyedOccupyContract(t, m, store)
}

// Two Managers on one database are two replicas: the unique key, not a
// process lock, makes one first attempt win.
func TestSQLKeyedOccupyConcurrentReplicas(t *testing.T) {
	store, _, _, c := newSQLQuota(t)
	replicas := []*Manager{NewManager(store, c.now, seqTokens("qa")), NewManager(store, c.now, seqTokens("qb"))}
	runKeyedOccupyRace(t, replicas, store, 20)
}

func TestSQLKeyedFreeConcurrentReplicas(t *testing.T) {
	store, _, m, c := newSQLQuota(t)
	tok, err := m.CheckAndOccupy(sqlAcct, "quota_euecs_instance", "cn-north-1", 5, "o")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CommitOccupy(tok.TokenID); err != nil {
		t.Fatal(err)
	}
	replicas := []*Manager{NewManager(store, c.now, seqTokens("qa")), NewManager(store, c.now, seqTokens("qb"))}
	runKeyedFreeRace(t, replicas, store)
	if u, _ := store.GetUsage(sqlAcct, "quota_euecs_instance", "cn-north-1"); u.Used != 3 {
		t.Fatalf("Used = %d, want 3 (freed once)", u.Used)
	}
}

// TestSQLIdempotencyParityWithMemoryStore runs one script through both stores
// and compares the records and counters they end up with.
func TestSQLIdempotencyParityWithMemoryStore(t *testing.T) {
	sqlStore, _, sqlMgr, _ := newSQLQuota(t)
	memMgr, memStore := newManager(base)
	type outcome struct {
		recs  []IdempotencyRecord
		usage []Usage
	}
	run := func(m *Manager, s IdempotentStore) outcome {
		steps := []func() error{
			func() error {
				_, _, err := m.CheckAndOccupyOnce(sqlAcct, "quota_euecs_instance", "cn-north-1", 2, "order-a", "k-a")
				return err
			},
			func() error {
				_, _, err := m.CheckAndOccupyOnce(sqlAcct, "quota_euoss_bucket", "cn-north-1", 1, "order-b", "k-b")
				return err
			},
			func() error {
				_, _, err := m.CheckAndOccupyOnce(sqlAcct, "quota_euecs_instance", "cn-north-1", 2, "order-a", "k-a")
				return err
			},
			func() error {
				_, _, err := m.FreeCommitted(sqlAcct, "quota_euecs_instance", "cn-north-1", 1, "res-a", "f-a")
				return err
			},
		}
		for i, step := range steps {
			if err := step(); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		var out outcome
		for _, k := range []struct {
			op  IdempotencyOp
			key string
		}{{OpOccupy, "k-a"}, {OpOccupy, "k-b"}, {OpFree, "f-a"}} {
			rec, ok, err := s.GetIdempotency(sqlAcct, k.op, k.key)
			if err != nil || !ok {
				t.Fatalf("record %s/%s: ok=%v err=%v", k.op, k.key, ok, err)
			}
			rec.TokenID = "" // minted by different generators
			out.recs = append(out.recs, rec)
		}
		for _, q := range [][2]string{{"quota_euecs_instance", "cn-north-1"}, {"quota_euoss_bucket", "*"}} {
			u, err := s.GetUsage(sqlAcct, q[0], q[1])
			if err != nil {
				t.Fatal(err)
			}
			out.usage = append(out.usage, u)
		}
		return out
	}
	mem, sql := run(memMgr, memStore), run(sqlMgr, sqlStore)
	if !reflect.DeepEqual(mem, sql) {
		t.Fatalf("stores disagree:\nmemory %+v\n   sql %+v", mem, sql)
	}
}
