// External test package: the harness (storage/sqltest) imports storage, so an
// in-package test file would form an import cycle. Everything here goes through
// the exported API, which is the part services use.
package storage_test

import (
	"context"
	"sync"
	"testing"

	"github.com/starcloud/sc-platform/storage"
	"github.com/starcloud/sc-platform/storage/sqltest"
)

// TestSequenceIsUniqueAcrossRestarts is the property the services depend on: a
// process-local counter restarts at 1 and collides with the rows the previous
// run committed, while a segment allocator resumes where the previous one
// stopped — including across segment boundaries, which is where an off-by-one in
// the range bookkeeping would show.
func TestSequenceIsUniqueAcrossRestarts(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-payment"))
	ctx := context.Background()

	seq, err := storage.OpenSequence(ctx, db, "ledger_entry", 2)
	if err != nil {
		t.Fatal(err)
	}

	issued := map[int64]bool{}
	take := func(s *storage.Sequence, n int) []int64 {
		t.Helper()
		out := make([]int64, 0, n)
		for i := 0; i < n; i++ {
			id, err := s.Next(ctx)
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			if id <= 0 {
				t.Fatalf("id = %d, want positive", id)
			}
			if issued[id] {
				t.Fatalf("id %d handed out twice", id)
			}
			issued[id] = true
			out = append(out, id)
		}
		return out
	}

	first := take(seq, 3)
	if first[0]+1 != first[1] || first[1]+1 != first[2] {
		t.Fatalf("ids within a segment are not consecutive: %v", first)
	}
	take(seq, 4) // crosses the 2-id segment boundary twice

	// A second allocator on the same pool stands for a restarted service.
	restarted, err := storage.OpenSequence(ctx, db, "ledger_entry", 2)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart := take(restarted, 4)
	if afterRestart[0] <= first[2] {
		t.Fatalf("restart handed out %v after %v: ids were reused", afterRestart, first)
	}
}

// TestSequenceConcurrentAllocatorsDoNotOverlap proves the FOR UPDATE guard: two
// allocators taking segments at the same moment must not walk away with the same
// range, or the duplicate id surfaces later in whichever request writes second.
func TestSequenceConcurrentAllocatorsDoNotOverlap(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-payment"))
	ctx := context.Background()

	const (
		allocators   = 4
		perAllocator = 25
	)
	seqs := make([]*storage.Sequence, allocators)
	for i := range seqs {
		seq, err := storage.OpenSequence(ctx, db, "resource_pack_journal", 7)
		if err != nil {
			t.Fatal(err)
		}
		seqs[i] = seq
	}

	var (
		mu   sync.Mutex
		seen = map[int64]bool{}
		wg   sync.WaitGroup
	)
	for _, seq := range seqs {
		wg.Add(1)
		go func(s *storage.Sequence) {
			defer wg.Done()
			for i := 0; i < perAllocator; i++ {
				id, err := s.Next(ctx)
				if err != nil {
					t.Errorf("next: %v", err)
					return
				}
				mu.Lock()
				if seen[id] {
					t.Errorf("id %d handed out by two allocators", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}(seq)
	}
	wg.Wait()

	if len(seen) != allocators*perAllocator {
		t.Fatalf("issued %d distinct ids, want %d", len(seen), allocators*perAllocator)
	}
}

// TestSequenceAutoSeedsUnknownName pins the auto-seed contract: a sequence row no
// migration created starts at 1 and hands out sequential ids.
//
// The trade-off this accepts is worth stating: a typo'd sequence name silently
// starts a fresh allocator instead of failing. It is bounded — ids collide only at
// the target table's primary key, which fails loudly on the first INSERT — and it
// spares every service from a migration whose only job is to insert one row.
func TestSequenceAutoSeedsUnknownName(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-payment"))
	ctx := context.Background()

	seq, err := storage.OpenSequence(ctx, db, "brand_new_sequence", 10)
	if err != nil {
		t.Fatalf("auto-seeded sequence = %v, want it created on demand", err)
	}
	first, err := seq.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Fatalf("first id of a new sequence = %d, want 1", first)
	}
	// The row persists: a second allocator over the same name takes the NEXT
	// segment — first + segment size, never the ids the first one holds.
	second, err := storage.OpenSequence(ctx, db, "brand_new_sequence", 10)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := second.Next(ctx); err != nil || id != first+10 {
		t.Fatalf("second allocator id = %d (err %v), want %d", id, err, first+10)
	}
}

func TestSequenceNextFuncAdaptsToDomainClosures(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-payment"))
	ctx := context.Background()

	seq, err := storage.OpenSequence(ctx, db, "ledger_entry", 5)
	if err != nil {
		t.Fatal(err)
	}
	fn := seq.NextFunc()
	if id := fn(); id <= 0 {
		t.Fatalf("NextFunc returned %d", id)
	}
	if id1, id2 := fn(), fn(); id1+1 != id2 {
		t.Fatalf("NextFunc skipped: %d then %d", id1, id2)
	}
}
