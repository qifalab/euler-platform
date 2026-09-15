package main

// Chain-checkpoint persistence (checkpoint.go). The full event stream lives in
// ClickHouse (partitioned by day, ≥180-day hot retention, adjudication S24);
// MySQL keeps only the per-tenant chain head and sequence —
// audit_chain_checkpoint (support_db, 07§6.2).
//
// Why this matters: a hash chain proves internal consistency, but an attacker
// who truncates the tail leaves a shorter chain that still verifies on its
// own. Recording the head here — outside the chain's own store — is what makes
// truncation detectable: the stored head no longer matches the chain. Which is
// also why a checkpoint that stays in the process's memory is not a checkpoint.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/qifalab/euler-platform/storage"
)

// checkpoint is one tenant's persisted chain head.
type checkpoint struct {
	AccountID int64
	Head      string
	Sequence  int64
}

// checkpointRepo persists chain checkpoints.
type checkpointRepo interface {
	// Upsert records the tenant's chain head. The update is monotonic: a
	// sequence that would move backwards never takes (a restarted writer
	// resuming from a stale view cannot roll the recorded chain back).
	Upsert(ctx context.Context, cp checkpoint) error
	// LoadAll returns every tenant's checkpoint, so a restarted process
	// resumes each chain instead of resetting it to genesis.
	LoadAll(ctx context.Context) ([]checkpoint, error)
}

type sqlCheckpointRepo struct {
	db *sql.DB
}

var _ checkpointRepo = (*sqlCheckpointRepo)(nil)

func newSQLCheckpointRepo(_ context.Context, db *sql.DB) *sqlCheckpointRepo {
	return &sqlCheckpointRepo{db: db}
}

func (r *sqlCheckpointRepo) Upsert(ctx context.Context, cp checkpoint) error {
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO audit_chain_checkpoint (account_id, chain_head, sequence)
		 VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   chain_head = IF(VALUES(sequence) > sequence, VALUES(chain_head), chain_head),
		   sequence = IF(VALUES(sequence) > sequence, VALUES(sequence), sequence)`,
		cp.AccountID, cp.Head, cp.Sequence); err != nil {
		return fmt.Errorf("audit: checkpoint for account %d: %w", cp.AccountID, err)
	}
	return nil
}

func (r *sqlCheckpointRepo) LoadAll(ctx context.Context) ([]checkpoint, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT account_id, chain_head, sequence FROM audit_chain_checkpoint ORDER BY account_id`)
	if err != nil {
		return nil, fmt.Errorf("audit: load checkpoints: %w", err)
	}
	defer rows.Close()

	out := make([]checkpoint, 0, 16)
	for rows.Next() {
		var cp checkpoint
		if err := rows.Scan(&cp.AccountID, &cp.Head, &cp.Sequence); err != nil {
			return nil, fmt.Errorf("audit: scan checkpoint: %w", err)
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

// newCheckpointRepo picks the backend. Persistence is opt-in (pkg-go/storage
// doc): with EULER_DB_DSN set, chain heads live in support_db and a restarted
// svc-audit continues every tenant's chain; unset, chains start at genesis in
// memory and `go test` stays dependency-free.
//
// A configured DSN that cannot be reached is a startup failure: a restarted
// audit service that resets chains to genesis would silently validate a
// truncated trail — the exact attack the checkpoint exists to catch.
func newCheckpointRepo(ctx context.Context) (checkpointRepo, error) {
	db, ok, err := storage.MustOpenFor(ctx, "support_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if err := storage.EnsureMigrated(ctx, db, "support_db"); err != nil {
		return nil, err
	}
	return newSQLCheckpointRepo(ctx, db), nil
}

// recordCheckpoint writes the chain head after a successful append. A failure
// is logged, not fatal: the event is already on the chain, and the next append
// carries the newer head, so the checkpoint self-heals on the next write.
func recordCheckpoint(s *auditStore, accountID int64) {
	if s.checkpoints == nil {
		return
	}
	c := s.chains[accountID]
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.checkpoints.Upsert(ctx, checkpoint{
		AccountID: accountID, Head: c.Head(), Sequence: c.Sequence(),
	}); err != nil {
		slog.Error("audit checkpoint failed", "account", accountID, "err", err)
	}
}
