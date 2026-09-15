package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
)

// SQLStore is the MySQL/Vitess implementation of Store, over trade_db's
// account_balance + ledger_entry (03-backend-services §4.2.3/§4.2.4, 04§6.3,
// account_id single-key sharding).
//
// # Both tables move as one
//
// The port's contract is atomicity: the balance and its journal entry commit
// together or not at all. A balance that no journal explains is the worst state
// the platform can be in — it cannot be reconciled, so it cannot be trusted —
// and 03§8.5's T+1 reconciliation depends on SUM(signed amount) reproducing the
// stored balance exactly. Both statements therefore run inside storage.Tx.
//
// # Version 0 means "no row", and that is load-bearing
//
// GetBalance reports Version 0 for an account with no ledger history, and a
// stored row's version is always ≥ 1 because the first write inserts version 1.
// Apply uses that to choose INSERT over UPDATE without a second round trip, and
// the guarded UPDATE is what arbitrates a race: the loser matches no row and
// gets ErrVersionConflict, so two concurrent charges cannot both spend the same
// money. A plain last-write-wins UPDATE here would let a customer double-spend,
// which is why this is a database-level guarantee and not a Go-level check.
type SQLStore struct{ db *sql.DB }

// NewSQLStore wraps a pool bound to trade_db.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// The port is satisfied at compile time, not at the first call site that
// happens to exercise it.
var _ Store = (*SQLStore)(nil)

// statementTimeout bounds one store call. The port has no context parameter
// (the domain calls it synchronously), so the bound lives here: a wedged
// connection must fail the request rather than pin the caller forever.
const statementTimeout = 10 * time.Second

// errVersionConflict ties the domain sentinel to the storage one, so a caller
// branching on ledger.ErrVersionConflict and a helper retrying on
// storage.ErrVersionConflict both recognise the same loss.
var errVersionConflict = fmt.Errorf("%w: %w", ErrVersionConflict, storage.ErrVersionConflict)

// entryColumns is the journal projection, in scanEntry's order. Kept in one
// place so the three read paths cannot drift apart.
const entryColumns = `entry_id, account_id, entry_type, amount, balance_after,
	biz_type, biz_key, idempotency_key, remark, occurred_at`

// GetBalance returns the account's position, or a zero balance when there is no
// ledger history yet (the Store contract: an account that has never been
// charged is not an error).
func (s *SQLStore) GetBalance(accountID int64) (Balance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	var (
		available string
		frozen    string
		bal       Balance
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT available, frozen, version, updated_at
		   FROM account_balance
		  WHERE account_id = ?`, accountID).
		Scan(&available, &frozen, &bal.Version, &bal.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Balance{AccountID: accountID}, nil
	}
	if err != nil {
		return Balance{}, fmt.Errorf("ledger: get balance for account %d: %w", accountID, err)
	}

	// DECIMAL crosses the wire as text; parse it with the same strict parser
	// that guards prices. A malformed balance is a data-integrity failure, not
	// something to round away.
	if bal.Available, err = pricing.ParseAmount(available); err != nil {
		return Balance{}, fmt.Errorf("ledger: account %d available: %w", accountID, err)
	}
	if bal.Frozen, err = pricing.ParseAmount(frozen); err != nil {
		return Balance{}, fmt.Errorf("ledger: account %d frozen: %w", accountID, err)
	}
	bal.AccountID = accountID
	return bal, nil
}

// Apply writes the entry and the new balance in one transaction, guarded by
// expectedVersion.
func (s *SQLStore) Apply(entry Entry, newBalance Balance, expectedVersion int) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if expectedVersion == 0 {
			// First write for this account. A duplicate key here means another
			// writer created the row between our read and this insert: that is
			// exactly the race the version guard exists to catch, so it is
			// reported as a conflict rather than retried silently.
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO account_balance
				   (account_id, available, frozen, version, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				entry.AccountID, newBalance.Available.String(), newBalance.Frozen.String(),
				newBalance.Version, newBalance.UpdatedAt, newBalance.UpdatedAt); err != nil {
				if storage.IsDuplicateKey(err) {
					return errVersionConflict
				}
				return fmt.Errorf("ledger: create balance for account %d: %w", entry.AccountID, err)
			}
		} else {
			res, err := tx.ExecContext(ctx,
				`UPDATE account_balance
				    SET available = ?, frozen = ?, version = ?, updated_at = ?
				  WHERE account_id = ? AND version = ?`,
				newBalance.Available.String(), newBalance.Frozen.String(), newBalance.Version,
				newBalance.UpdatedAt, entry.AccountID, expectedVersion)
			if err != nil {
				return fmt.Errorf("ledger: update balance for account %d: %w", entry.AccountID, err)
			}
			// Zero rows matched: somebody moved the balance out from under us.
			// The whole transaction is abandoned, so the journal gains nothing.
			if err := storage.Affected(res, nil); err != nil {
				return errVersionConflict
			}
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_entry
			   (entry_id, account_id, entry_type, amount, balance_after,
			    biz_type, biz_key, idempotency_key, remark, occurred_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			entry.EntryID, entry.AccountID, string(entry.Type), entry.Amount.String(),
			entry.BalanceAfter.String(), entry.BizType, entry.BizKey, entry.IdempotencyKey,
			nullableString(entry.Remark), entry.OccurredAt); err != nil {
			// uk_idempotency (account_id, idempotency_key) is the journal's
			// exactly-once arbiter. The domain pre-checks the key, so reaching
			// here means two requests raced with the same key: the loser has
			// applied nothing and reports it as the idempotent no-op it is.
			if storage.IsDuplicateKey(err) {
				return fmt.Errorf("%w: %s", ErrDuplicateEntry, entry.IdempotencyKey)
			}
			return fmt.Errorf("ledger: insert entry %d: %w", entry.EntryID, err)
		}
		return nil
	})
}

// FindByIdempotencyKey returns a previously applied entry, if any.
func (s *SQLStore) FindByIdempotencyKey(accountID int64, key string) (Entry, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	row := s.db.QueryRowContext(ctx,
		`SELECT `+entryColumns+`
		   FROM ledger_entry
		  WHERE account_id = ? AND idempotency_key = ?`, accountID, key)
	entry, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("ledger: find idempotency key %q: %w", key, err)
	}
	return entry, true, nil
}

// ListEntries returns the journal in occurrence order. entry_id is the order:
// it is minted by the segment service monotonically (04§6.6), so it is both
// unique and the sequence Reconcile needs to walk the BalanceAfter chain — an
// occurred_at sort would be ambiguous for entries written in the same
// millisecond, and a journal whose replay order is ambiguous cannot prove
// anything.
func (s *SQLStore) ListEntries(accountID int64) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryColumns+`
		   FROM ledger_entry
		  WHERE account_id = ?
		  ORDER BY entry_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("ledger: list entries for account %d: %w", accountID, err)
	}
	defer rows.Close()

	// Non-nil empty slice: a caller ranging over a nil slice and a caller
	// comparing against nil should see the same thing as the in-memory store.
	entries := make([]Entry, 0, 16)
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("ledger: scan entry for account %d: %w", accountID, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list entries for account %d: %w", accountID, err)
	}
	return entries, nil
}

// rowScanner is the shared shape of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanEntry reads one journal row. Amounts are parsed through pricing, and a
// NULL remark (the DDL's default) becomes the empty string the domain uses for
// "no note" rather than an error.
func scanEntry(sc rowScanner) (Entry, error) {
	var (
		entry      Entry
		entryType  string
		amount     string
		balanceOut string
		remark     sql.NullString
	)
	if err := sc.Scan(&entry.EntryID, &entry.AccountID, &entryType, &amount, &balanceOut,
		&entry.BizType, &entry.BizKey, &entry.IdempotencyKey, &remark, &entry.OccurredAt); err != nil {
		return Entry{}, err
	}
	entry.Type = EntryType(entryType)
	var err error
	if entry.Amount, err = pricing.ParseAmount(amount); err != nil {
		return Entry{}, fmt.Errorf("entry %d amount: %w", entry.EntryID, err)
	}
	if entry.BalanceAfter, err = pricing.ParseAmount(balanceOut); err != nil {
		return Entry{}, fmt.Errorf("entry %d balance_after: %w", entry.EntryID, err)
	}
	entry.Remark = remark.String
	return entry, nil
}

// nullableString writes an empty string as SQL NULL, matching the DDL's
// nullable remark column: an absent note is an absent value, not "".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
