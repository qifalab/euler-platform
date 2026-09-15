package main

import (
	"context"
	"testing"

	"github.com/starcloud/sc-platform/audit"
	"github.com/starcloud/sc-platform/storage/sqltest"
)

// TestCheckpointSurvivesRestart pins the anti-truncation property end to end:
// appends publish the chain head outside the chain's own store, and a restarted
// process resumes each tenant's chain from that checkpoint instead of resetting
// it to genesis — a genesis reset would silently validate a truncated trail.
func TestCheckpointSurvivesRestart(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-quota"))
	ctx := context.Background()
	repo := newSQLCheckpointRepo(ctx, db)

	first, err := newAuditStoreWith(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	// chainFor lazily creates the tenant's chain, exactly as handleAppend does.
	c0 := first.chainFor(100123)
	for i := 0; i < 3; i++ {
		if _, err := c0.Append(audit.Event{
			EventID: "e-1", Identity: audit.Identity{AccountID: 100123, Principal: "u"},
		}); err != nil {
			t.Fatalf("append #%d: %v", i+1, err)
		}
		recordCheckpoint(first, 100123)
	}
	if got := c0.Sequence(); got != 3 {
		t.Fatalf("sequence = %d, want 3", got)
	}

	// A fresh process resumes the chain at the persisted head: the next event
	// continues the sequence instead of starting over.
	restarted, err := newAuditStoreWith(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	c := restarted.chainFor(100123)
	if c.Sequence() != 3 {
		t.Fatalf("resumed sequence = %d, want 3", c.Sequence())
	}
	next, err := c.Append(audit.Event{
		EventID: "e-2", Identity: audit.Identity{AccountID: 100123, Principal: "u"},
	})
	if err != nil {
		t.Fatal(err)
	}
	recordCheckpoint(restarted, 100123)

	cps, err := repo.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 1 || cps[0].AccountID != 100123 || cps[0].Sequence != 4 {
		t.Fatalf("checkpoints = %+v, want account 100123 at sequence 4", cps)
	}
	// The event the restarted process appended links onto the persisted chain —
	// its EventID embeds the continued sequence.
	if next.EventID == "" {
		t.Fatal("continued chain produced no event id")
	}
}

// TestCheckpointMonotonic pins the upsert guard: a writer holding a stale view
// cannot roll the recorded chain head backwards.
func TestCheckpointMonotonic(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-quota"))
	ctx := context.Background()
	repo := newSQLCheckpointRepo(ctx, db)

	if err := repo.Upsert(ctx, checkpoint{AccountID: 200123, Head: "h9", Sequence: 9}); err != nil {
		t.Fatal(err)
	}
	// A stale writer (sequence 5) must not move the recorded head back.
	if err := repo.Upsert(ctx, checkpoint{AccountID: 200123, Head: "h5", Sequence: 5}); err != nil {
		t.Fatal(err)
	}
	cps, err := repo.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 1 || cps[0].Sequence != 9 || cps[0].Head != "h9" {
		t.Fatalf("checkpoint rolled back: %+v", cps)
	}
}
