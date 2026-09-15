package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// Sequence is the platform's 号段 (segment) allocator for business ids
// (04§6.6). Ledger entry ids, journal ids and order ids are allocated, not
// counted by the database: the id embeds a shard factor, and a table without
// AUTO_INCREMENT has no other way to produce one.
//
// # Why a process-local counter is not enough
//
// It is, exactly as long as the store is in memory. Point the same code at a
// real database and the counter restarts at 1 on every process start, so the
// first write after a restart collides with the row the previous run committed
// — a duplicate primary key, not a slow degradation. Ids must be unique across
// restarts, restarts of a *different* replica, and every replicas' concurrent
// writes.
//
// # Segments, not round trips
//
// A caller takes a whole range (default 1000 ids) under one row lock and hands
// them out from memory, returning to the database only when the range is spent.
// The range is taken with SELECT ... FOR UPDATE inside a transaction, so two
// callers can never walk away with overlapping ranges — the failure mode that
// makes an id allocator worse than useless, since a duplicate id is discovered
// at INSERT time in an unrelated request.
//
// A range that is never fully used is simply lost: ids must be unique, not
// gapless, and a process that dies mid-range must not hand its remainder to
// anyone else.
type Sequence struct {
	db          *sql.DB
	name        string
	segmentSize int64

	mu    sync.Mutex
	next  int64 // next id to hand out
	limit int64 // exclusive upper bound of the taken segment
}

// ErrSequenceUnknown is returned when the named sequence cannot be read even
// after the automatic seed: in practice this means the id_sequence TABLE does not
// exist, i.e. the schema has not been migrated.
var ErrSequenceUnknown = errors.New("storage: unknown id sequence")

// OpenSequence loads (and takes the first segment of) a named sequence.
func OpenSequence(ctx context.Context, db *sql.DB, name string, segmentSize int64) (*Sequence, error) {
	if segmentSize < 1 {
		segmentSize = 1000
	}
	s := &Sequence{db: db, name: name, segmentSize: segmentSize}
	if err := s.refill(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Next returns the next id.
func (s *Sequence) Next(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= s.limit {
		if err := s.refill(ctx); err != nil {
			return 0, err
		}
	}
	id := s.next
	s.next++
	return id, nil
}

// NextFunc adapts Next to the shape the domain packages take (a plain
// func() int64 with no error channel — ledger.New, reservepack.New), so a
// service can hand the allocator straight to them.
//
// A database failure while refilling cannot be reported through that shape, so
// it panics with the cause attached. That is deliberate: the alternative is a
// silent fallback to a local counter, which is the duplicate-id bug this type
// exists to prevent. Service HTTP handlers run under recover middleware, so the
// request fails with a 500 and a logged stack rather than with corrupt ids.
func (s *Sequence) NextFunc() func() int64 {
	return func() int64 {
		id, err := s.Next(context.Background())
		if err != nil {
			panic(fmt.Sprintf("storage: id sequence %s: %v", s.name, err))
		}
		return id
	}
}

// refill takes the next segment. Callers hold s.mu (or are still constructing
// the Sequence), so the local bookkeeping needs no further locking.
//
// A missing row is seeded here rather than by every consumer's migration: the
// INSERT IGNORE and the FOR UPDATE read share one transaction, so two processes
// that both need a brand-new sequence cannot walk away with the same segment —
// the loser of the INSERT blocks on the winner's row lock and reads the advanced
// value. A row a migration seeded with a specific start value is respected (the
// insert is a no-op for it), which is how an operator pre-positions an allocator.
func (s *Sequence) refill(ctx context.Context) error {
	var start int64
	err := Tx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO id_sequence (name, next_value, segment_size) VALUES (?, 1, ?)`,
			s.name, s.segmentSize); err != nil {
			return err
		}
		// FOR UPDATE: two allocators that read the same start value would hand
		// out the same ids, and the collision would surface later, in whichever
		// request happened to write second.
		err := tx.QueryRowContext(ctx,
			`SELECT next_value FROM id_sequence WHERE name = ? FOR UPDATE`, s.name).Scan(&start)
		if errors.Is(err, sql.ErrNoRows) {
			// Unreachable after the insert above unless the table itself is
			// missing, which the insert would have reported.
			return fmt.Errorf("%w: %q (is the schema migrated?)", ErrSequenceUnknown, s.name)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE id_sequence SET next_value = ? WHERE name = ?`, start+s.segmentSize, s.name)
		return err
	})
	if err != nil {
		return fmt.Errorf("storage: refill sequence %s: %w", s.name, err)
	}
	s.next, s.limit = start, start+s.segmentSize
	return nil
}

// EnsureMigrated fails when the schema carries no migration history, which is
// the signature of a DSN pointing at a database sqlmigrate never touched.
//
// Without this check a service starts happily against an empty schema and then
// fails every request with "table ... doesn't exist" — an outage that looks like
// a code bug, discovered traffic-first instead of at startup with the command
// that fixes it. Persistence is opt-in; pointing it at an unmigrated schema is a
// configuration error, and configuration errors belong at boot.
func EnsureMigrated(ctx context.Context, db *sql.DB, schema string) error {
	var applied int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migration`).Scan(&applied); err != nil {
		return fmt.Errorf("storage: schema %s has no migration history (run tools/sqlmigrate): %w", schema, err)
	}
	if applied == 0 {
		return fmt.Errorf("storage: schema %s has an empty schema_migration table (run tools/sqlmigrate)", schema)
	}
	return nil
}
