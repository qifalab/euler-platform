package accesskey

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/starcloud/sc-platform/storage"
)

// SQLStore is the MySQL-backed Store over account_db.access_key (07§2.2).
// It is the production half of the port: the gateway-facing verification path
// reads this table (behind a cache), so a restarted verifier must still resolve
// every live key — an in-memory store forgets that a key exists, which for a
// credential system is the same as deleting the credential.
type SQLStore struct {
	db *sql.DB
}

var _ Store = (*SQLStore)(nil)

// NewSQLStore wires the store to account_db.
func NewSQLStore(_ context.Context, db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

const recordColumns = `ak, account_id, owner_type, owner_id, sk_cipher, sk_key_version,
	status, last_used_at, last_used_ip, rotated_at, grace_until, deleted_at, created_at`

// nullableOwnerID renders the owner id for storage: the schema keeps NULL for a
// master account (owner_type=1), which the port's zero value represents.
func nullableOwnerID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func (s *SQLStore) Insert(r Record) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO access_key
		   (ak, account_id, owner_type, owner_id, sk_cipher, sk_key_version,
		    status, last_used_at, last_used_ip, rotated_at, grace_until, deleted_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.AK, r.AccountID, r.OwnerType, nullableOwnerID(r.OwnerID), r.SKCipher, r.SKKeyVersion,
		r.Status, nullableTime(r.LastUsedAt), nullableString(r.LastUsedIP),
		nullableTime(r.RotatedAt), nullableTime(r.GraceUntil), nullableTime(r.DeletedAt),
		r.CreatedAt.UTC()); err != nil {
		if storage.IsDuplicateKey(err) {
			// The AK primary key: two issuers handing out the same id would be
			// a credential collision — fail loudly rather than overwrite.
			return fmt.Errorf("accesskey: insert %s: duplicate ak", r.AK)
		}
		return fmt.Errorf("accesskey: insert %s: %w", r.AK, err)
	}
	return nil
}

func (s *SQLStore) Get(ak string) (Record, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r, err := scanRecord(s.db.QueryRowContext(ctx,
		`SELECT `+recordColumns+` FROM access_key WHERE ak = ?`, ak))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("accesskey: get %s: %w", ak, err)
	}
	return r, nil
}

func (s *SQLStore) ListByOwner(accountID int64, ownerType OwnerType, ownerID int64) ([]Record, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+recordColumns+` FROM access_key
		  WHERE account_id = ? AND owner_type = ? AND ((? = 0 AND owner_id IS NULL) OR owner_id = ?)
		  ORDER BY created_at, ak`,
		accountID, ownerType, ownerID, ownerID)
	if err != nil {
		return nil, fmt.Errorf("accesskey: list keys for account %d: %w", accountID, err)
	}
	defer rows.Close()

	out := make([]Record, 0, 2)
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("accesskey: scan for account %d: %w", accountID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("accesskey: list keys for account %d: %w", accountID, err)
	}
	return out, nil
}

func (s *SQLStore) Update(r Record) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := s.db.ExecContext(ctx,
		`UPDATE access_key
		    SET status = ?, last_used_at = ?, last_used_ip = ?, rotated_at = ?,
		        grace_until = ?, deleted_at = ?, sk_cipher = ?, sk_key_version = ?
		  WHERE ak = ?`,
		r.Status, nullableTime(r.LastUsedAt), nullableString(r.LastUsedIP),
		nullableTime(r.RotatedAt), nullableTime(r.GraceUntil), nullableTime(r.DeletedAt),
		r.SKCipher, r.SKKeyVersion, r.AK)
	if err != nil {
		return fmt.Errorf("accesskey: update %s: %w", r.AK, err)
	}
	// Zero rows is ErrNotFound, not a silent success: the caller just read the
	// record, so a miss means it was physically deleted (soft-delete retention
	// sweep) between the read and the write.
	return storage.Affected(res, nil)
}

func scanRecord(sc interface{ Scan(...any) error }) (Record, error) {
	var (
		r                     Record
		ownerID               sql.NullInt64
		lastUsedAt, rotatedAt sql.NullTime
		graceUntil, deletedAt sql.NullTime
		lastUsedIP            sql.NullString
	)
	if err := sc.Scan(&r.AK, &r.AccountID, &r.OwnerType, &ownerID, &r.SKCipher, &r.SKKeyVersion,
		&r.Status, &lastUsedAt, &lastUsedIP, &rotatedAt, &graceUntil, &deletedAt, &r.CreatedAt); err != nil {
		return Record{}, err
	}
	if ownerID.Valid {
		r.OwnerID = ownerID.Int64
	}
	if lastUsedAt.Valid {
		r.LastUsedAt = lastUsedAt.Time
	}
	r.LastUsedIP = lastUsedIP.String
	if rotatedAt.Valid {
		r.RotatedAt = rotatedAt.Time
	}
	if graceUntil.Valid {
		r.GraceUntil = graceUntil.Time
	}
	if deletedAt.Valid {
		r.DeletedAt = deletedAt.Time
	}
	return r, nil
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
