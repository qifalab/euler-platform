package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/qifalab/euler-platform/storage"
)

// SQLStore implements the keyed operations over support_db.quota_idempotency
// (services/svc-quota/sql V5): uk_acc_op_key (account_id, op, idem_key) is the
// arbiter, and the record is inserted in the same transaction as the usage
// change (and, for an occupy, the token) it stands for.
var _ IdempotentStore = (*SQLStore)(nil)

const idempotencyColumns = `account_id, op, idem_key, quota_code, region, amount, token_id, biz_key, expires_at, created_at`

// GetIdempotency returns the record for (accountID, op, key), if any.
func (s *SQLStore) GetIdempotency(accountID int64, op IdempotencyOp, key string) (IdempotencyRecord, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	var (
		rec     IdempotencyRecord
		opName  string
		expires sql.NullTime
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT `+idempotencyColumns+`
		   FROM quota_idempotency
		  WHERE account_id = ? AND op = ? AND idem_key = ?`, accountID, string(op), key).
		Scan(&rec.AccountID, &opName, &rec.Key, &rec.QuotaCode, &rec.Region, &rec.Amount,
			&rec.TokenID, &rec.BizKey, &expires, &rec.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return IdempotencyRecord{}, false, fmt.Errorf("quota: get idempotency %d/%s/%q: %w", accountID, op, key, err)
	}
	rec.Op = IdempotencyOp(opName)
	if expires.Valid {
		rec.ExpiresAt = expires.Time
	}
	return rec, true, nil
}

// OccupyOnce records the key, moves the usage row and inserts the token in one
// transaction.
func (s *SQLStore) OccupyOnce(u Usage, expectedVersion int, tok Token, rec IdempotencyRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		// The key first: a concurrent first attempt with the same key blocks
		// here until the winner commits, then fails on the unique key without
		// having touched the counter.
		if err := insertIdempotency(ctx, tx, rec); err != nil {
			return err
		}
		if err := updateUsageTx(ctx, tx, u, expectedVersion); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO quota_token
			   (token_id, account_id, quota_code, region, amount, biz_key, expires_at, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			tok.TokenID, tok.AccountID, tok.QuotaCode, tok.Region, tok.Amount, tok.BizKey,
			tok.ExpiresAt.UTC(), tok.CreatedAt.UTC()); err != nil {
			return fmt.Errorf("quota: insert token %s: %w", tok.TokenID, err)
		}
		return nil
	})
}

// UpdateUsageOnce records the key and moves the usage row in one transaction.
func (s *SQLStore) UpdateUsageOnce(u Usage, expectedVersion int, rec IdempotencyRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if err := insertIdempotency(ctx, tx, rec); err != nil {
			return err
		}
		return updateUsageTx(ctx, tx, u, expectedVersion)
	})
}

func insertIdempotency(ctx context.Context, tx *sql.Tx, rec IdempotencyRecord) error {
	var expires any
	if !rec.ExpiresAt.IsZero() {
		expires = rec.ExpiresAt.UTC()
	}
	created := rec.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO quota_idempotency (`+idempotencyColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.AccountID, string(rec.Op), rec.Key, rec.QuotaCode, rec.Region, rec.Amount,
		rec.TokenID, rec.BizKey, expires, created.UTC()); err != nil {
		if storage.IsDuplicateKey(err) {
			return fmt.Errorf("%w: %s %q", ErrIdempotencyKeyTaken, rec.Op, rec.Key)
		}
		return fmt.Errorf("quota: record idempotency %s %q: %w", rec.Op, rec.Key, err)
	}
	return nil
}
