package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/starcloud/sc-platform/storage"
)

// SQLStore is the MySQL/Vitess implementation of Store, over support_db's
// quota_definition / quota_usage / quota_token (03§4.3.2, 04§6.3, account_id
// single-key sharding).
//
// # The optimistic lock is the database's, deliberately
//
// 03§4.3.2 makes the DB row authoritative and Redis only a read accelerator: a
// counter that lives in a cache loses its state on failover, and quota that
// resets on a cache restart is quota that can be spent twice. So every
// reservation is an UPDATE ... WHERE version = ?, and the loser of a race
// re-reads fresh state instead of silently overwriting the winner's count.
//
// # The store owns the version bump
//
// Unlike the ledger's Balance (where the domain computes Version+1 and hands it
// over), Manager computes only the new Used/Occupying and passes the version it
// read. The stored row must therefore advance to expectedVersion+1 here — the
// Manager's rollback path explicitly guards its compensating update on that
// post-increment value, so bumping it anywhere else desynchronises the two.
//
// # And "version 0" does NOT mean "no row" here
//
// It does in the ledger, where the first write inserts version 1. Quota rows can
// legitimately sit at version 0 — a definition or usage row seeded by SQL, which
// is exactly how a pre-provisioned account is set up. So UpdateUsage attempts the
// guarded UPDATE first and only falls back to INSERT when the row genuinely does
// not exist; assuming "0 ⇒ insert" would turn every seeded row's first reserve
// into a duplicate-key error.
type SQLStore struct{ db *sql.DB }

// NewSQLStore wraps a pool bound to support_db.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

var _ Store = (*SQLStore)(nil)

// statementTimeout bounds one store call; the port has no context parameter.
const statementTimeout = 10 * time.Second

// errVersionConflict ties the domain sentinel to the storage one, so both the
// Manager's branch and storage.WithVersionRetry recognise the loss.
var errVersionConflict = fmt.Errorf("%w: %w", ErrVersionConflict, storage.ErrVersionConflict)

// GetDefinition returns the quota rule, or ErrUnknownQuota when there is no
// such code. A missing quota code is a programming error at the call site, not
// a zero-valued rule.
func (s *SQLStore) GetDefinition(quotaCode string) (Definition, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	var (
		def        Definition
		scope      string
		adjustable int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT product_code, default_value, scope, adjustable
		   FROM quota_definition
		  WHERE quota_code = ?`, quotaCode).
		Scan(&def.ProductCode, &def.DefaultValue, &scope, &adjustable)
	if errors.Is(err, sql.ErrNoRows) {
		return Definition{}, fmt.Errorf("%w: %s", ErrUnknownQuota, quotaCode)
	}
	if err != nil {
		return Definition{}, fmt.Errorf("quota: get definition %s: %w", quotaCode, err)
	}
	def.QuotaCode = quotaCode
	switch Scope(scope) {
	case ScopeGlobal, ScopeRegion:
		def.Scope = Scope(scope)
	default:
		return Definition{}, fmt.Errorf("quota: definition %s has unknown scope %q", quotaCode, scope)
	}
	def.Adjustable = adjustable != 0
	return def, nil
}

// GetUsage returns the account's position, or a zeroed Usage (version 0) when
// the account has never reserved against this quota — the Manager lazily
// creates the row from the definition on the first reservation.
func (s *SQLStore) GetUsage(accountID int64, quotaCode, region string) (Usage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	u := Usage{AccountID: accountID, QuotaCode: quotaCode, Region: region}
	err := s.db.QueryRowContext(ctx,
		`SELECT used, occupying, hard_limit, version
		   FROM quota_usage
		  WHERE account_id = ? AND quota_code = ? AND region = ?`,
		accountID, quotaCode, region).
		Scan(&u.Used, &u.Occupying, &u.HardLimit, &u.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return u, nil
	}
	if err != nil {
		return Usage{}, fmt.Errorf("quota: get usage for account %d/%s/%s: %w", accountID, quotaCode, region, err)
	}
	return u, nil
}

// UpdateUsage applies the change under the optimistic lock and advances the
// stored version to expectedVersion+1.
//
// # Why the create path inserts first, and never reads
//
// The obvious shape — UPDATE, and if nothing matched, SELECT to find out
// whether the row is missing or merely moved, then INSERT — deadlocks: under
// REPEATABLE READ the SELECT takes a gap lock on the missing key, several
// concurrent first-writes hold that gap at once, and their INSERTs then block
// each other's insert-intention locks in a cycle (MySQL Error 1213, reproduced
// by TestSQLStoreConcurrentOccupyCannotOversell). So a first write is a single
// INSERT, and only a duplicate key sends us to the guarded UPDATE — which is
// what handles the other legitimate case: a row SQL seeded at version 0, where
// the first change must update it in place rather than collide with it.
//
// The non-zero path never inserts. Reviving a deleted usage row would reset a
// counter to a smaller number than the resources it counts, which is oversell;
// "the row is gone" and "the version moved" both mean the caller's view is
// stale, and both are reported as the conflict they are.
func (s *SQLStore) UpdateUsage(u Usage, expectedVersion int) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	next := expectedVersion + 1
	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if expectedVersion == 0 {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO quota_usage (account_id, quota_code, region, used, occupying, hard_limit, version)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				u.AccountID, u.QuotaCode, u.Region, u.Used, u.Occupying, u.HardLimit, next)
			if err == nil {
				return nil
			}
			if !storage.IsDuplicateKey(err) {
				return fmt.Errorf("quota: create usage for account %d/%s/%s: %w", u.AccountID, u.QuotaCode, u.Region, err)
			}
			// uk_acc_quota_region fired: a row is already there. Fall through to
			// the guarded UPDATE, which succeeds only if it sits at version 0.
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE quota_usage
			    SET used = ?, occupying = ?, hard_limit = ?, version = ?
			  WHERE account_id = ? AND quota_code = ? AND region = ? AND version = ?`,
			u.Used, u.Occupying, u.HardLimit, next,
			u.AccountID, u.QuotaCode, u.Region, expectedVersion)
		if err != nil {
			return fmt.Errorf("quota: update usage for account %d/%s/%s: %w", u.AccountID, u.QuotaCode, u.Region, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quota: update usage for account %d/%s/%s: %w", u.AccountID, u.QuotaCode, u.Region, err)
		}
		if n == 0 {
			return errVersionConflict
		}
		return nil
	})
}

// PutToken stores a reservation. It is an upsert: the Manager restores a token
// it failed to convert (CommitOccupy's compensating path), and a restore must
// not fail because the row it is restoring was already written back.
//
// Implemented as DELETE + INSERT rather than ON DUPLICATE KEY UPDATE so the
// statement stays portable across MySQL versions and the Vitess parser, which
// disagree about VALUES() and the row-alias spelling.
func (s *SQLStore) PutToken(t Token) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM quota_token WHERE token_id = ?`, t.TokenID); err != nil {
			return fmt.Errorf("quota: put token %s: %w", t.TokenID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO quota_token
			   (token_id, account_id, quota_code, region, amount, biz_key, expires_at, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			t.TokenID, t.AccountID, t.QuotaCode, t.Region, t.Amount, t.BizKey,
			t.ExpiresAt.UTC(), t.CreatedAt.UTC()); err != nil {
			return fmt.Errorf("quota: put token %s: %w", t.TokenID, err)
		}
		return nil
	})
}

// GetToken returns the reservation, or ErrTokenNotFound.
func (s *SQLStore) GetToken(tokenID string) (Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	row := s.db.QueryRowContext(ctx,
		`SELECT token_id, account_id, quota_code, region, amount, biz_key, expires_at, created_at
		   FROM quota_token
		  WHERE token_id = ?`, tokenID)
	tok, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, fmt.Errorf("%w: %s", ErrTokenNotFound, tokenID)
	}
	if err != nil {
		return Token{}, fmt.Errorf("quota: get token %s: %w", tokenID, err)
	}
	return tok, nil
}

// DeleteToken claims the reservation, reporting ErrTokenNotFound when it was
// already claimed.
//
// The failure is the point, not an inconvenience: both CommitOccupy and the
// expiry sweep delete FIRST and only then move the usage counter, precisely so
// that exactly one of the two racers owns the reservation. If this delete were
// tolerant, a commit racing the sweeper could convert a reservation to Used
// after the sweeper had already returned it to the pool — overselling by the
// token amount, with no error anywhere to show for it.
func (s *SQLStore) DeleteToken(tokenID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	res, err := s.db.ExecContext(ctx, `DELETE FROM quota_token WHERE token_id = ?`, tokenID)
	if err != nil {
		return fmt.Errorf("quota: delete token %s: %w", tokenID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("quota: delete token %s: %w", tokenID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrTokenNotFound, tokenID)
	}
	return nil
}

// ListExpiredTokens returns the tokens past their TTL, oldest first, for the
// sweeper. Ordering matches SortTokens (creation time) with a token_id
// tiebreak, so a sweep is reproducible rather than dependent on row order.
func (s *SQLStore) ListExpiredTokens(now time.Time) ([]Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT token_id, account_id, quota_code, region, amount, biz_key, expires_at, created_at
		   FROM quota_token
		  WHERE expires_at <= ?
		  ORDER BY created_at, token_id`, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("quota: list expired tokens: %w", err)
	}
	defer rows.Close()

	tokens := make([]Token, 0, 8)
	for rows.Next() {
		tok, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("quota: scan expired token: %w", err)
		}
		tokens = append(tokens, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quota: list expired tokens: %w", err)
	}
	return tokens, nil
}

// rowScanner is the shared shape of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanToken(sc rowScanner) (Token, error) {
	var tok Token
	if err := sc.Scan(&tok.TokenID, &tok.AccountID, &tok.QuotaCode, &tok.Region,
		&tok.Amount, &tok.BizKey, &tok.ExpiresAt, &tok.CreatedAt); err != nil {
		return Token{}, err
	}
	return tok, nil
}
