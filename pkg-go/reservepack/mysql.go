package reservepack

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
// resource_pack + resource_pack_journal (02-09 M-4.1, 04§6.3, account_id
// single-key sharding). It is the production counterpart to MemoryStore.
//
// # Same contract as the cash ledger
//
// A pack mutation and its journal row commit in one transaction (storage.Tx),
// the pack moves under an optimistic lock, and the journal's uk_idem makes a
// retried consume a no-op instead of a double debit. A pack whose quota was
// drawn twice, or drawn below zero, is a billing error the customer discovers
// on an invoice — so none of those are enforced in Go alone: the DDL's CHECK
// constraints and unique indexes are the last line, and this store maps their
// violations onto the package's own errors rather than leaking driver codes.
//
// # Codes, not strings, on the wire
//
// status and entry_type are TINYINT in the DDL (1ACTIVE 2EXHAUSTED 3EXPIRED;
// 1PURCHASE 2CONSUME 3REFUND 4EXPIRE) while the domain speaks in named
// constants. The two mappings below are the only places that translation
// happens, and both reject an unknown value loudly: a status the domain does
// not know is a schema drift, not something to guess at.
type SQLStore struct{ db *sql.DB }

// NewSQLStore wraps a pool bound to trade_db.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

var _ Store = (*SQLStore)(nil)

// statementTimeout bounds one store call; the port has no context parameter.
const statementTimeout = 10 * time.Second

// errVersionConflict carries both sentinels, so the package's retry loop
// (errors.Is(err, ErrVersionConflict)) and storage.WithVersionRetry both see
// the loss.
var errVersionConflict = fmt.Errorf("%w: %w", ErrVersionConflict, storage.ErrVersionConflict)

// packColumns is the pack projection, in scanPack's order.
const packColumns = `pack_id, account_id, product_code, sku_code, face_value,
	remaining, purchased_at, expire_at, status, version`

// journalColumns is the journal projection, in scanEntry's order.
const journalColumns = `entry_id, pack_id, entry_type, amount, balance_after,
	biz_key, idempotency_key, created_at`

// statusCode maps the lifecycle state onto the DDL's TINYINT.
func statusCode(s Status) (int, error) {
	switch s {
	case StatusActive:
		return 1, nil
	case StatusExhausted:
		return 2, nil
	case StatusExpired:
		return 3, nil
	}
	return 0, fmt.Errorf("reservepack: unknown status %q", s)
}

func statusFromCode(code int) (Status, error) {
	switch code {
	case 1:
		return StatusActive, nil
	case 2:
		return StatusExhausted, nil
	case 3:
		return StatusExpired, nil
	}
	return "", fmt.Errorf("reservepack: unknown status code %d in resource_pack", code)
}

// entryTypeCode maps the journal entry kind onto the DDL's TINYINT.
func entryTypeCode(t EntryType) (int, error) {
	switch t {
	case EntryPurchase:
		return 1, nil
	case EntryConsume:
		return 2, nil
	case EntryRefund:
		return 3, nil
	case EntryExpire:
		return 4, nil
	}
	return 0, fmt.Errorf("%w: %q", ErrInvalidEntryType, t)
}

func entryTypeFromCode(code int) (EntryType, error) {
	switch code {
	case 1:
		return EntryPurchase, nil
	case 2:
		return EntryConsume, nil
	case 3:
		return EntryRefund, nil
	case 4:
		return EntryExpire, nil
	}
	return "", fmt.Errorf("reservepack: unknown entry_type code %d in resource_pack_journal", code)
}

// GetPack returns the pack, or (_, false, nil) when there is no such row.
func (s *SQLStore) GetPack(packID string) (Pack, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	row := s.db.QueryRowContext(ctx,
		`SELECT `+packColumns+` FROM resource_pack WHERE pack_id = ?`, packID)
	pack, err := scanPack(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Pack{}, false, nil
	}
	if err != nil {
		return Pack{}, false, fmt.Errorf("reservepack: get pack %s: %w", packID, err)
	}
	return pack, true, nil
}

// ListActiveByAccount returns the packs a waterfall may draw on, plus the
// past-deadline ones the expiry sweeper still has to settle.
//
// The predicate mirrors MemoryStore's exactly, including the part that looks
// wrong at first read: a pack whose expire_at has passed is returned even when
// its remaining is zero, because the sweeper needs to see it to move it to the
// EXPIRED terminal state. Filtering on "remaining > 0" alone would leave those
// rows ACTIVE forever, and any later code that grants an extension would
// resurrect quota the customer had already forfeited.
func (s *SQLStore) ListActiveByAccount(accountID int64, t time.Time) ([]Pack, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	status, err := statusCode(StatusActive)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+packColumns+`
		   FROM resource_pack
		  WHERE account_id = ?
		    AND status = ?
		    AND ((expire_at IS NOT NULL AND expire_at <= ?) OR remaining > 0)
		  ORDER BY purchased_at, pack_id`, accountID, status, t.UTC())
	if err != nil {
		return nil, fmt.Errorf("reservepack: list packs for account %d: %w", accountID, err)
	}
	defer rows.Close()

	packs := make([]Pack, 0, 8)
	for rows.Next() {
		pack, err := scanPack(rows)
		if err != nil {
			return nil, fmt.Errorf("reservepack: scan pack for account %d: %w", accountID, err)
		}
		packs = append(packs, pack)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reservepack: list packs for account %d: %w", accountID, err)
	}
	return packs, nil
}

// Apply mutates the pack and appends its journal row in one transaction,
// guarded by expectedVersion. expectedVersion 0 means "this pack is being
// created" (Purchase reads the pack first, so a version of 0 can only mean
// absent — a stored pack is written with version 1).
func (s *SQLStore) Apply(entry Entry, newPack Pack, expectedVersion int) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	status, err := statusCode(newPack.Status)
	if err != nil {
		return err
	}
	entryType, err := entryTypeCode(entry.Type)
	if err != nil {
		return err
	}

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if expectedVersion == 0 {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO resource_pack
				   (pack_id, account_id, product_code, sku_code, face_value, remaining,
				    purchased_at, expire_at, status, version, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				newPack.PackID, newPack.AccountID, newPack.ProductCode, newPack.SKUCode,
				newPack.FaceValue.String(), newPack.Remaining.String(), newPack.PurchasedAt,
				nullableTime(newPack.ExpireAt), status, newPack.Version,
				entry.CreatedAt, entry.CreatedAt); err != nil {
				// uk_pack(account_id, pack_id): the pack appeared between
				// Purchase's existence check and this insert. That is the race
				// the version guard exists to catch, not a retryable duplicate.
				if storage.IsDuplicateKey(err) {
					return errVersionConflict
				}
				return fmt.Errorf("reservepack: create pack %s: %w", newPack.PackID, err)
			}
		} else {
			res, err := tx.ExecContext(ctx,
				`UPDATE resource_pack
				    SET product_code = ?, sku_code = ?, face_value = ?, remaining = ?,
				        expire_at = ?, status = ?, version = ?, updated_at = ?
				  WHERE pack_id = ? AND version = ?`,
				newPack.ProductCode, newPack.SKUCode, newPack.FaceValue.String(),
				newPack.Remaining.String(), nullableTime(newPack.ExpireAt), status,
				newPack.Version, entry.CreatedAt, newPack.PackID, expectedVersion)
			if err != nil {
				return fmt.Errorf("reservepack: update pack %s: %w", newPack.PackID, err)
			}
			if err := storage.Affected(res, nil); err != nil {
				// Zero rows matched. Distinguish the two causes so the caller
				// is not sent down a 500 path for a retryable condition: a
				// vanished row is ErrPackNotFound, a moved version is
				// ErrVersionConflict (the package's error doc is explicit that
				// conflating them is a bug).
				var current int
				checkErr := tx.QueryRowContext(ctx,
					`SELECT version FROM resource_pack WHERE pack_id = ?`, newPack.PackID).Scan(&current)
				switch {
				case errors.Is(checkErr, sql.ErrNoRows):
					return ErrPackNotFound
				case checkErr != nil:
					return fmt.Errorf("reservepack: re-read pack %s: %w", newPack.PackID, checkErr)
				default:
					return errVersionConflict
				}
			}
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO resource_pack_journal
			   (entry_id, pack_id, account_id, entry_type, amount, balance_after,
			    biz_key, idempotency_key, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			entry.EntryID, entry.PackID, newPack.AccountID, entryType,
			entry.Amount.String(), entry.Balance.String(), entry.BizKey,
			entry.IdempotencyKey, entry.CreatedAt); err != nil {
			// uk_idem(account_id, pack_id, idempotency_key) — the exactly-once
			// arbiter. Reaching it means a concurrent caller applied this key
			// first; this caller has applied nothing and says so.
			if storage.IsDuplicateKey(err) {
				return fmt.Errorf("%w: %s", ErrDuplicateEntry, entry.IdempotencyKey)
			}
			return fmt.Errorf("reservepack: insert journal entry %d: %w", entry.EntryID, err)
		}
		return nil
	})
}

// FindByIdempotencyKey returns a previously applied entry, if any.
func (s *SQLStore) FindByIdempotencyKey(packID, key string) (Entry, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	row := s.db.QueryRowContext(ctx,
		`SELECT `+journalColumns+`
		   FROM resource_pack_journal
		  WHERE pack_id = ? AND idempotency_key = ?`, packID, key)
	entry, err := scanJournalEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("reservepack: find idempotency key %q: %w", key, err)
	}
	return entry, true, nil
}

// ListEntries returns the journal for a pack, oldest first. entry_id orders it
// for the same reason it does in the cash ledger: it is minted monotonically
// and is unambiguous, where created_at can tie.
func (s *SQLStore) ListEntries(packID string) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+journalColumns+`
		   FROM resource_pack_journal
		  WHERE pack_id = ?
		  ORDER BY entry_id`, packID)
	if err != nil {
		return nil, fmt.Errorf("reservepack: list journal for pack %s: %w", packID, err)
	}
	defer rows.Close()

	entries := make([]Entry, 0, 8)
	for rows.Next() {
		entry, err := scanJournalEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("reservepack: scan journal for pack %s: %w", packID, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reservepack: list journal for pack %s: %w", packID, err)
	}
	return entries, nil
}

// rowScanner is the shared shape of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanPack reads one pack row. expire_at NULL means "never expires" and maps
// onto the domain's zero time, which is what Pack.Available tests against.
func scanPack(sc rowScanner) (Pack, error) {
	var (
		pack                 Pack
		faceValue, remaining string
		expireAt             sql.NullTime
		statusCode           int
	)
	if err := sc.Scan(&pack.PackID, &pack.AccountID, &pack.ProductCode, &pack.SKUCode,
		&faceValue, &remaining, &pack.PurchasedAt, &expireAt, &statusCode, &pack.Version); err != nil {
		return Pack{}, err
	}
	var err error
	if pack.FaceValue, err = pricing.ParseAmount(faceValue); err != nil {
		return Pack{}, fmt.Errorf("pack %s face_value: %w", pack.PackID, err)
	}
	if pack.Remaining, err = pricing.ParseAmount(remaining); err != nil {
		return Pack{}, fmt.Errorf("pack %s remaining: %w", pack.PackID, err)
	}
	if pack.Status, err = statusFromCode(statusCode); err != nil {
		return Pack{}, err
	}
	if expireAt.Valid {
		pack.ExpireAt = expireAt.Time
	}
	return pack, nil
}

// scanJournalEntry reads one journal row.
func scanJournalEntry(sc rowScanner) (Entry, error) {
	var (
		entry           Entry
		entryTypeCode   int
		amount, balance string
	)
	if err := sc.Scan(&entry.EntryID, &entry.PackID, &entryTypeCode, &amount, &balance,
		&entry.BizKey, &entry.IdempotencyKey, &entry.CreatedAt); err != nil {
		return Entry{}, err
	}
	var err error
	if entry.Type, err = entryTypeFromCode(entryTypeCode); err != nil {
		return Entry{}, err
	}
	if entry.Amount, err = pricing.ParseAmount(amount); err != nil {
		return Entry{}, fmt.Errorf("entry %d amount: %w", entry.EntryID, err)
	}
	if entry.Balance, err = pricing.ParseAmount(balance); err != nil {
		return Entry{}, fmt.Errorf("entry %d balance_after: %w", entry.EntryID, err)
	}
	return entry, nil
}

// nullableTime writes the domain's zero time as SQL NULL: the DDL's expire_at
// is nullable precisely because "no expiry" is not a timestamp.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
