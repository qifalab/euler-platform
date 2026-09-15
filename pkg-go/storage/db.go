// Package storage owns the platform's database plumbing (04-middleware §6):
// DSN resolution, connection-pool policy, and the few helpers every
// SQL-backed store needs — no-rows mapping, optimistic-lock retry, JSON
// columns.
//
// # Persistence is opt-in per process
//
// An empty EULER_DB_DSN keeps a service on its in-memory store, which is what
// keeps `go test` hermetic and the local dev servers dependency-free. A set
// DSN switches the same binary to MySQL/Vitess: services resolve their store
// through OpenFor and never branch on anything else.
//
// # One DSN, many schemas
//
// The platform is split into logical databases (account_db, trade_db,
// resource_db, support_db, metering_db, openapi_meta — 04§6.3). A single
// EULER_DB_DSN points at one of them; DSNFor rewrites the schema so a service
// that owns two (or a test that needs a second) does not need a second
// environment variable.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// DSNEnv is the environment variable that enables persistence.
const DSNEnv = "EULER_DB_DSN"

// pool defaults: a service is a long-lived process with a handful of
// concurrent requests, so a small pool beats the driver default (unlimited
// open connections) under a connection-storm or a slow query.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 25
	defaultConnMaxLifetime = 5 * time.Minute
	defaultConnMaxIdleTime = time.Minute
)

// Enabled reports whether persistence is configured for this process.
func Enabled() bool {
	return strings.TrimSpace(os.Getenv(DSNEnv)) != ""
}

// Schema returns the schema (database) name from the configured DSN, or "".
// It is what a service logs at startup so an operator can see which database
// the process is actually bound to.
func Schema() string {
	dsn := strings.TrimSpace(os.Getenv(DSNEnv))
	if dsn == "" {
		return ""
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return ""
	}
	return cfg.DBName
}

// NormalizeDSN parses a DSN and forces the platform's time/charset policy:
//
//   - parseTime=true      DATETIME/TIMESTAMP scan into time.Time instead of
//     []byte (a store that has to parse strings by hand
//     gets it wrong exactly once per driver version).
//   - loc=UTC             every timestamp crosses the wire in one zone; the
//     services compare and persist time.Time directly.
//   - charset/collation   utf8mb4, matching the DDL.
//
// The caller's other parameters (multiStatements for the migrator, timeouts)
// are preserved.
func NormalizeDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(strings.TrimSpace(dsn))
	if err != nil {
		return "", fmt.Errorf("storage: invalid DSN: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if _, ok := cfg.Params["charset"]; !ok {
		cfg.Params["charset"] = "utf8mb4"
	}
	return cfg.FormatDSN(), nil
}

// DSNFor returns the configured DSN with its schema replaced. Persistence
// disabled (no EULER_DB_DSN) reports ok=false rather than an error, because that
// is the normal dev/test state, not a failure.
func DSNFor(schema string) (dsn string, ok bool, err error) {
	raw := strings.TrimSpace(os.Getenv(DSNEnv))
	if raw == "" {
		return "", false, nil
	}
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		return "", false, fmt.Errorf("storage: invalid %s: %w", DSNEnv, err)
	}
	cfg.DBName = schema
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if _, ok := cfg.Params["charset"]; !ok {
		cfg.Params["charset"] = "utf8mb4"
	}
	return cfg.FormatDSN(), true, nil
}

// Open builds a tuned *sql.DB and verifies the connection with a ping, so a
// misconfigured DSN fails at startup rather than on the first request.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	normalized, err := NormalizeDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", normalized)
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", err)
	}
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)
	db.SetConnMaxIdleTime(defaultConnMaxIdleTime)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping: %w", err)
	}
	return db, nil
}

// OpenFor opens the connection for one schema. When persistence is disabled it
// returns (nil, false, nil) — the caller keeps its in-memory store.
func OpenFor(ctx context.Context, schema string) (*sql.DB, bool, error) {
	dsn, ok, err := DSNFor(schema)
	if err != nil || !ok {
		return nil, false, err
	}
	db, err := Open(ctx, dsn)
	if err != nil {
		return nil, false, err
	}
	return db, true, nil
}

// MustOpenFor is OpenFor for a service's main(): a configured DSN that cannot
// be reached is a startup failure, not something to limp along with.
func MustOpenFor(ctx context.Context, schema string) (*sql.DB, bool, error) {
	db, ok, err := OpenFor(ctx, schema)
	if err != nil {
		return nil, false, fmt.Errorf("storage: schema %s: %w", schema, err)
	}
	return db, ok, nil
}

// IsNotFound reports whether err is the database's "no row" signal, so stores
// can map it onto their own domain error.
func IsNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// WithVersionRetry runs fn, retrying while it reports ErrVersionConflict (or a
// caller-supplied conflict predicate). Optimistic-lock updates lose races by
// design; a store that surfaces the loss as a hard error turns a benign
// concurrent write into a failed request.
func WithVersionRetry(attempts int, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if !errors.Is(err, ErrVersionConflict) {
			return err
		}
	}
	return err
}

// ErrVersionConflict is the shared optimistic-lock signal: an UPDATE ... WHERE
// version = ? that matched zero rows. Services map it onto their own
// {Product}.VersionConflict error code.
var ErrVersionConflict = errors.New("storage: version conflict")

// txAttempts bounds the retry of a deadlock victim. Five is ample: a retry only
// repeats the same small key set, and a caller that keeps losing should see the
// conflict and re-read rather than spin here.
const txAttempts = 5

// txBackoff is the pause before retrying a deadlock victim: it grows with the
// attempt and carries jitter.
//
// The jitter is the part that matters. Transactions that deadlocked against
// each other are, by construction, in flight at the same moment; without jitter
// they wake in lockstep and collide on the same key again, so the retry loop
// spins through its attempts without ever making progress (observed: 8 goroutines
// creating one quota_usage row, retrying in sync). Spreading them out lets the
// winner commit, after which the others immediately see its row and lose the
// optimistic-lock check instead of contending for the insert.
func txBackoff(attempt int) time.Duration {
	base := time.Duration(attempt+1) * 5 * time.Millisecond
	return base + rand.N(base)
}

// Tx runs fn inside a transaction, rolling back on error and committing
// otherwise. Every money mutation in this platform is one transaction (the
// balance write and its journal row must be atomic — 03§8.2).
//
// A deadlock victim is retried rather than surfaced. InnoDB breaks a lock cycle
// by rolling one transaction back and reports Error 1213 expecting a retry; a
// caller cannot order the cycle away, because it is created by concurrent
// writers touching the same keys (two INSERTs of one unique key each hold a
// shared lock on the duplicate record and then request an exclusive one).
// Retrying is safe for the same reason the transaction exists: the rollback left
// no partial write, and fn re-runs from the caller's own snapshot — so a version
// that moved in the meantime still fails its optimistic-lock check instead of
// overwriting the winner.
func Tx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	var err error
	for attempt := 0; attempt < txAttempts; attempt++ {
		if err = txOnce(ctx, db, fn); err == nil {
			return nil
		}
		if !IsDeadlock(err) && !isLockWaitTimeout(err) {
			return err
		}
		if attempt < txAttempts-1 {
			time.Sleep(txBackoff(attempt))
		}
	}
	return err
}

// txOnce is one transaction attempt.
func txOnce(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// JSON marshals v for a JSON column. A nil result is stored as SQL NULL by the
// callers that pass sql.NullString, which is what the DDL's nullable JSON
// columns expect.
func JSON(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// JSONColumn renders a JSON column value as a string for scanning: an empty
// column scans as "", and callers treat that as the zero value.
func JSONColumn(raw []byte) string {
	return string(raw)
}

// IsDeadlock reports whether err is MySQL's deadlock-victim signal (1213).
//
// This is not a bug in the caller's logic. InnoDB resolves a lock cycle by
// rolling one of the transactions back, and reports it expecting the
// application to retry; the cycle is a normal consequence of concurrent writers
// touching the same keys, not something a caller can order away. Stores that
// create rows under contention see it for real: two concurrent INSERTs of the
// same unique key each hold a shared lock on the duplicate record and then ask
// for an exclusive one, which is a cycle (see storage.Tx, which retries it).
func IsDeadlock(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1213
	}
	return false
}

// isLockWaitTimeout reports whether err is MySQL's lock-wait timeout (1205):
// another transaction held a lock longer than innodb_lock_wait_timeout. It is
// transient in the same way a deadlock is, and retried alongside it.
func isLockWaitTimeout(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1205
	}
	return false
}

// IsDuplicateKey reports whether err is MySQL's duplicate-key violation (1062).
//
// A unique index is the database's half of an idempotency contract: uk_idem
// (ledger), uk_event_id (outbox), uk_order (settlement). The losing writer must
// surface its own "already applied" error, not a driver code — so the check
// lives here, in the plumbing, instead of every domain store importing the
// driver to look at error numbers.
func IsDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	return false
}

// Affected reports whether an UPDATE/DELETE matched a row, mapping "zero rows"
// onto the optimistic-lock conflict: a guarded write that matched nothing lost
// a race or targeted a missing row, and both must fail loudly.
func Affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrVersionConflict
	}
	return nil
}
