package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/authz"
	"github.com/qifalab/euler-platform/storage"
)

// sqlStore is the account_db-backed authStore (EULER_DB_DSN). Tables:
// account (+ V3 real-name / token-version columns), mfa_device (sealed TOTP
// seed), auth_refresh_session, password_reset_code, email_verification_code
// (V4), iam_user / iam_role /
// ram_policy and the ram_user_policy / ram_role_policy attachments. Business
// ids come from account_db's id_sequence (04§6.6), never from a process
// counter.
type sqlStore struct {
	db *sql.DB

	accountIDs, userIDs, roleIDs, policyIDs *storage.Sequence

	sweepMu   sync.Mutex
	lastSweep time.Time
}

var _ authStore = (*sqlStore)(nil)

// newSQLStore opens the id sequences (seeded by the V3 migration).
func newSQLStore(ctx context.Context, db *sql.DB) (*sqlStore, error) {
	s := &sqlStore{db: db, lastSweep: time.Now()}
	for _, seq := range []struct {
		name string
		dst  **storage.Sequence
	}{
		{"account", &s.accountIDs}, {"iam_user", &s.userIDs}, {"iam_role", &s.roleIDs}, {"ram_policy", &s.policyIDs},
	} {
		q, err := storage.OpenSequence(ctx, db, seq.name, 100)
		if err != nil {
			return nil, err
		}
		*seq.dst = q
	}
	return s, nil
}

// queryer is what both *sql.DB and *sql.Tx offer.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// --- accounts ---

const accountSelect = `SELECT a.account_id, a.account_name, a.password_hash, a.real_name_status, a.mfa_bound, a.status,
	a.real_name, a.enterprise_name, a.id_no_masked, a.identity_fingerprint, a.token_version, a.version, a.created_at,
	d.secret_cipher, d.key_version
  FROM account a
  LEFT JOIN mfa_device d ON d.owner_type = 1 AND d.owner_id = a.account_id AND d.device_type = 1`

func scanAccount(row interface{ Scan(...any) error }) (account, error) {
	var (
		a                                     account
		mfaBound                              int
		realName, entName, idMasked, identity sql.NullString
		cipher                                []byte
		keyVersion                            sql.NullInt64
	)
	if err := row.Scan(&a.AccountID, &a.AccountName, &a.PasswordHash, &a.RealNameStatus, &mfaBound, &a.Status,
		&realName, &entName, &idMasked, &identity, &a.TokenVersion, &a.version, &a.CreatedAt,
		&cipher, &keyVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return account{}, errNotFound
		}
		return account{}, err
	}
	a.MFAEnabled = mfaBound == 1
	a.RealName, a.EnterpriseName, a.IDCardNo, a.IdentityFingerprint = realName.String, entName.String, idMasked.String, identity.String
	a.RealNameType = realNameTypeOf(a.RealNameStatus)
	if len(cipher) > 0 {
		a.MFASecretCipher = cipher
		a.MFAKeyVersion = int(keyVersion.Int64)
	}
	a.CreatedAt = a.CreatedAt.UTC()
	return a, nil
}

// realNameTypeOf derives the stored type from real_name_status (07§2.3).
func realNameTypeOf(status int) string {
	switch status {
	case 1:
		return "individual"
	case 2:
		return "enterprise"
	}
	return ""
}

func (s *sqlStore) CreateAccount(ctx context.Context, a account) (account, error) {
	if a.AccountID == 0 {
		id, err := s.accountIDs.Next(ctx)
		if err != nil {
			return account{}, err
		}
		a.AccountID = id
	}
	a.CreatedAt = createdAt(a.CreatedAt)
	err := storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO account (account_id, account_name, email, password_hash, account_type, real_name_status,
			   mfa_bound, status, real_name, enterprise_name, id_no_masked, identity_fingerprint, token_version,
			   version, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			a.AccountID, a.AccountName, a.AccountName, a.PasswordHash, accountType(a), a.RealNameStatus,
			boolInt(a.MFAEnabled), a.Status, nullStr(a.RealName), nullStr(a.EnterpriseName), nullStr(a.IDCardNo),
			nullStr(a.IdentityFingerprint), a.TokenVersion, a.CreatedAt); err != nil {
			if storage.IsDuplicateKey(err) {
				return errExists
			}
			return err
		}
		return writeMFADevice(ctx, tx, a)
	})
	if err != nil {
		return account{}, err
	}
	return s.AccountByID(ctx, a.AccountID)
}

func accountType(a account) int {
	if a.RealNameType == "enterprise" {
		return 2
	}
	return 1
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// writeMFADevice makes mfa_device hold exactly the account's seed (or none).
func writeMFADevice(ctx context.Context, q queryer, a account) error {
	if len(a.MFASecretCipher) == 0 {
		_, err := q.ExecContext(ctx,
			`DELETE FROM mfa_device WHERE owner_type = 1 AND owner_id = ? AND device_type = 1`, a.AccountID)
		return err
	}
	_, err := q.ExecContext(ctx,
		`INSERT INTO mfa_device (account_id, owner_type, owner_id, device_type, secret_cipher, key_version, bound_at)
		 VALUES (?, 1, ?, 1, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE secret_cipher = VALUES(secret_cipher), key_version = VALUES(key_version), bound_at = VALUES(bound_at)`,
		a.AccountID, a.AccountID, a.MFASecretCipher, a.MFAKeyVersion, storeTime(time.Now()))
	return err
}

func (s *sqlStore) AccountByID(ctx context.Context, id int64) (account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, accountSelect+` WHERE a.account_id = ?`, id))
}

func (s *sqlStore) AccountByName(ctx context.Context, name string) (account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, accountSelect+` WHERE a.account_name = ?`, name))
}

func (s *sqlStore) UpdateAccount(ctx context.Context, id int64, fn func(*account) error) (account, error) {
	var out account
	err := storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		cur, err := scanAccount(tx.QueryRowContext(ctx, accountSelect+` WHERE a.account_id = ? FOR UPDATE`, id))
		if err != nil {
			return err
		}
		next := cloneAccount(cur)
		if err := fn(&next); err != nil {
			return err
		}
		next.AccountID, next.CreatedAt, next.version = cur.AccountID, cur.CreatedAt, cur.version
		if err := storage.Affected(tx.ExecContext(ctx,
			`UPDATE account SET account_name = ?, email = ?, password_hash = ?, account_type = ?, real_name_status = ?,
			        mfa_bound = ?, status = ?, real_name = ?, enterprise_name = ?, id_no_masked = ?,
			        identity_fingerprint = ?, token_version = ?, version = version + 1
			  WHERE account_id = ? AND version = ?`,
			next.AccountName, next.AccountName, next.PasswordHash, accountType(next), next.RealNameStatus,
			boolInt(next.MFAEnabled), next.Status, nullStr(next.RealName), nullStr(next.EnterpriseName),
			nullStr(next.IDCardNo), nullStr(next.IdentityFingerprint), next.TokenVersion,
			cur.AccountID, cur.version)); err != nil {
			if storage.IsDuplicateKey(err) {
				return errExists
			}
			return err
		}
		if !bytes.Equal(cur.MFASecretCipher, next.MFASecretCipher) || cur.MFAKeyVersion != next.MFAKeyVersion {
			if err := writeMFADevice(ctx, tx, next); err != nil {
				return err
			}
		}
		next.version++
		out = next
		return nil
	})
	if err != nil {
		return account{}, err
	}
	return out, nil
}

// --- refresh sessions ---

const sessionSelect = `SELECT token_hash, account_id, token_version, family_id, parent_hash, replaced_by, expires_at,
	revoked_at, revoke_reason, created_at FROM auth_refresh_session`

func scanSession(row interface{ Scan(...any) error }) (refreshSession, error) {
	var (
		s                        refreshSession
		parent, replaced, reason sql.NullString
		revoked                  sql.NullTime
	)
	if err := row.Scan(&s.TokenHash, &s.AccountID, &s.TokenVersion, &s.FamilyID, &parent, &replaced, &s.ExpiresAt,
		&revoked, &reason, &s.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return refreshSession{}, errNotFound
		}
		return refreshSession{}, err
	}
	s.ParentHash, s.ReplacedBy, s.RevokeReason = parent.String, replaced.String, reason.String
	if revoked.Valid {
		s.RevokedAt = revoked.Time.UTC()
	}
	s.ExpiresAt, s.CreatedAt = s.ExpiresAt.UTC(), s.CreatedAt.UTC()
	return s, nil
}

func insertSession(ctx context.Context, q queryer, s refreshSession) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	_, err := q.ExecContext(ctx,
		`INSERT INTO auth_refresh_session (token_hash, account_id, token_version, family_id, parent_hash, replaced_by,
		   expires_at, revoked_at, revoke_reason, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.TokenHash, s.AccountID, s.TokenVersion, s.FamilyID, nullStr(s.ParentHash), nullStr(s.ReplacedBy), s.ExpiresAt.UTC(),
		nullTime(s.RevokedAt), nullStr(s.RevokeReason), s.CreatedAt.UTC())
	if storage.IsDuplicateKey(err) {
		return errExists
	}
	return err
}

func (s *sqlStore) CreateSession(ctx context.Context, sess refreshSession) error {
	s.maybeSweep(ctx)
	return insertSession(ctx, s.db, sess)
}

// maybeSweep deletes expired sessions and reset codes at most once a minute,
// in bounded batches, so the tables do not grow with every login ever made.
func (s *sqlStore) maybeSweep(ctx context.Context) {
	now := time.Now()
	s.sweepMu.Lock()
	if now.Sub(s.lastSweep) < time.Minute {
		s.sweepMu.Unlock()
		return
	}
	s.lastSweep = now
	s.sweepMu.Unlock()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM auth_refresh_session WHERE expires_at <= ? LIMIT 5000`, now.UTC())
	_, _ = s.db.ExecContext(ctx, `DELETE FROM password_reset_code WHERE expires_at <= ? LIMIT 5000`, now.UTC())
	_, _ = s.db.ExecContext(ctx, `DELETE FROM email_verification_code WHERE expires_at <= ? LIMIT 5000`, now.UTC())
}

func (s *sqlStore) SessionByHash(ctx context.Context, hash string) (refreshSession, error) {
	return scanSession(s.db.QueryRowContext(ctx, sessionSelect+` WHERE token_hash = ?`, hash))
}

func (s *sqlStore) RotateSession(ctx context.Context, oldHash string, next refreshSession, now time.Time) error {
	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		old, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+` WHERE token_hash = ? FOR UPDATE`, oldHash))
		if err != nil {
			return err
		}
		if !old.live(now) {
			return errNotFound
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE auth_refresh_session SET revoked_at = ?, revoke_reason = ?, replaced_by = ? WHERE token_hash = ?`,
			now.UTC(), revokeRotated, next.TokenHash, oldHash); err != nil {
			return err
		}
		return insertSession(ctx, tx, next)
	})
}

func (s *sqlStore) RevokeSession(ctx context.Context, hash, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE auth_refresh_session SET revoked_at = ?, revoke_reason = ? WHERE token_hash = ? AND revoked_at IS NULL`,
		now.UTC(), reason, hash)
	return err
}

func (s *sqlStore) RevokeSessionFamily(ctx context.Context, familyID, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE auth_refresh_session SET revoked_at = ?, revoke_reason = ? WHERE family_id = ? AND revoked_at IS NULL`,
		now.UTC(), reason, familyID)
	return err
}

func (s *sqlStore) RevokeAccountSessions(ctx context.Context, accountID int64, reason string, now time.Time) error {
	return revokeAccountSessions(ctx, s.db, accountID, reason, now)
}

func revokeAccountSessions(ctx context.Context, q queryer, accountID int64, reason string, now time.Time) error {
	_, err := q.ExecContext(ctx,
		`UPDATE auth_refresh_session SET revoked_at = ?, revoke_reason = ? WHERE account_id = ? AND revoked_at IS NULL`,
		now.UTC(), reason, accountID)
	return err
}

// --- password reset codes ---

func (s *sqlStore) PutResetCode(ctx context.Context, rc resetCode) error {
	s.maybeSweep(ctx)
	if rc.CreatedAt.IsZero() {
		rc.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO password_reset_code (account_id, code_hash, attempts, expires_at, created_at)
		 VALUES (?, ?, 0, ?, ?)
		 ON DUPLICATE KEY UPDATE code_hash = VALUES(code_hash), attempts = 0,
		    expires_at = VALUES(expires_at), created_at = VALUES(created_at)`,
		rc.AccountID, rc.CodeHash, rc.ExpiresAt.UTC(), rc.CreatedAt.UTC())
	return err
}

// --- email verification codes ---

func (s *sqlStore) PutEmailCode(ctx context.Context, ec emailCode) error {
	s.maybeSweep(ctx)
	if ec.CreatedAt.IsZero() {
		ec.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO email_verification_code (purpose, email_hash, code_hash, attempts, expires_at, created_at)
		 VALUES (?, ?, ?, 0, ?, ?)
		 ON DUPLICATE KEY UPDATE code_hash = VALUES(code_hash), attempts = 0,
		    expires_at = VALUES(expires_at), created_at = VALUES(created_at)`,
		ec.Purpose, ec.EmailHash, ec.CodeHash, ec.ExpiresAt.UTC(), ec.CreatedAt.UTC())
	return err
}

func (s *sqlStore) ConsumeEmailCode(ctx context.Context, purpose, emailHash, codeHash string, now time.Time) error {
	// As in ResetPassword, a wrong guess must COUNT although the call fails:
	// the transaction commits the attempt counter, the outcome travels out.
	var outcome error
	err := storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		outcome = nil
		var (
			stored    string
			attempts  int
			expiresAt time.Time
		)
		err := tx.QueryRowContext(ctx,
			`SELECT code_hash, attempts, expires_at FROM email_verification_code
			  WHERE purpose = ? AND email_hash = ? FOR UPDATE`, purpose, emailHash).Scan(&stored, &attempts, &expiresAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			outcome = errCodeInvalid
			return nil
		case err != nil:
			return err
		case !now.Before(expiresAt):
			outcome = errCodeInvalid
			_, err := tx.ExecContext(ctx,
				`DELETE FROM email_verification_code WHERE purpose = ? AND email_hash = ?`, purpose, emailHash)
			return err
		case attempts >= maxResetAttempts:
			outcome = errCodeAttempts
			return nil
		case !hmac.Equal([]byte(stored), []byte(codeHash)):
			outcome = errCodeInvalid
			_, err := tx.ExecContext(ctx,
				`UPDATE email_verification_code SET attempts = attempts + 1 WHERE purpose = ? AND email_hash = ?`, purpose, emailHash)
			return err
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM email_verification_code WHERE purpose = ? AND email_hash = ?`, purpose, emailHash)
		return err
	})
	if err != nil {
		return err
	}
	return outcome
}

func (s *sqlStore) ResetPassword(ctx context.Context, accountID int64, codeHash, passwordHash string, now time.Time) (account, error) {
	// A wrong guess must COUNT even though the call fails, so the outcome
	// travels outside the transaction: the transaction itself commits the
	// attempt counter and reports success.
	var outcome error
	var out account
	err := storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		outcome = nil
		var (
			stored    string
			attempts  int
			expiresAt time.Time
		)
		err := tx.QueryRowContext(ctx,
			`SELECT code_hash, attempts, expires_at FROM password_reset_code WHERE account_id = ? FOR UPDATE`,
			accountID).Scan(&stored, &attempts, &expiresAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			outcome = errCodeInvalid
			return nil
		case err != nil:
			return err
		case !now.Before(expiresAt):
			outcome = errCodeInvalid
			_, err := tx.ExecContext(ctx, `DELETE FROM password_reset_code WHERE account_id = ?`, accountID)
			return err
		case attempts >= maxResetAttempts:
			outcome = errCodeAttempts
			return nil
		case !hmac.Equal([]byte(stored), []byte(codeHash)):
			outcome = errCodeInvalid
			_, err := tx.ExecContext(ctx,
				`UPDATE password_reset_code SET attempts = attempts + 1 WHERE account_id = ?`, accountID)
			return err
		}
		cur, err := scanAccount(tx.QueryRowContext(ctx, accountSelect+` WHERE a.account_id = ? FOR UPDATE`, accountID))
		if errors.Is(err, errNotFound) {
			outcome = errCodeInvalid
			return nil
		}
		if err != nil {
			return err
		}
		if err := storage.Affected(tx.ExecContext(ctx,
			`UPDATE account SET password_hash = ?, token_version = token_version + 1, version = version + 1
			  WHERE account_id = ? AND version = ?`, passwordHash, accountID, cur.version)); err != nil {
			return err
		}
		if err := revokeAccountSessions(ctx, tx, accountID, revokeReset, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM password_reset_code WHERE account_id = ?`, accountID); err != nil {
			return err
		}
		cur.PasswordHash = passwordHash
		cur.TokenVersion++
		cur.version++
		out = cur
		return nil
	})
	if err != nil {
		return account{}, err
	}
	if outcome != nil {
		return account{}, outcome
	}
	return out, nil
}

// --- policies ---

const policySelect = `SELECT policy_id, account_id, policy_name, scope, COALESCE(document_text, CAST(document AS CHAR)), created_at
  FROM ram_policy`

func scanPolicy(row interface{ Scan(...any) error }) (storedPolicy, error) {
	var (
		p     storedPolicy
		scope int
	)
	if err := row.Scan(&p.ID, &p.AccountID, &p.Name, &scope, &p.Document, &p.CreatedAt); err != nil {
		return storedPolicy{}, err
	}
	parsed, err := authz.ParsePolicy([]byte(p.Document))
	if err != nil {
		return storedPolicy{}, fmt.Errorf("ram_policy %d: stored document does not parse: %w", p.ID, err)
	}
	p.Parsed = parsed
	p.Type = "custom"
	if scope == 1 {
		p.Type = "system"
	}
	p.CreatedAt = p.CreatedAt.UTC()
	return p, nil
}

func queryPolicies(ctx context.Context, q queryer, query string, args ...any) ([]storedPolicy, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []storedPolicy{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *sqlStore) ListPolicies(ctx context.Context, accountID int64) ([]storedPolicy, error) {
	return queryPolicies(ctx, s.db, policySelect+` WHERE account_id IN (0, ?) ORDER BY policy_id`, accountID)
}

// placeholders returns "?, ?, ..." for n arguments.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func policiesByName(ctx context.Context, q queryer, accountID int64, names []string) (map[string]storedPolicy, error) {
	out := make(map[string]storedPolicy, len(names))
	if len(names) == 0 {
		return out, nil
	}
	args := []any{accountID}
	for _, n := range names {
		args = append(args, n)
	}
	ps, err := queryPolicies(ctx, q, policySelect+` WHERE account_id IN (0, ?) AND policy_name IN (`+placeholders(len(names))+`)`, args...)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		out[p.Name] = p
	}
	return out, nil
}

func (s *sqlStore) PoliciesByName(ctx context.Context, accountID int64, names []string) (map[string]storedPolicy, error) {
	return policiesByName(ctx, s.db, accountID, names)
}

// resolvePolicyIDs maps attached policy names to ids, in order.
func resolvePolicyIDs(ctx context.Context, q queryer, accountID int64, names []string) ([]int64, error) {
	found, err := policiesByName(ctx, q, accountID, names)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(names))
	var missing []string
	for _, n := range names {
		p, ok := found[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		ids = append(ids, p.ID)
	}
	if len(missing) > 0 {
		return nil, &policyNotFoundError{names: missing}
	}
	return ids, nil
}

func (s *sqlStore) CreatePolicy(ctx context.Context, p storedPolicy) (storedPolicy, error) {
	id, err := s.policyIDs.Next(ctx)
	if err != nil {
		return storedPolicy{}, err
	}
	p.ID, p.Type, p.CreatedAt = id, "custom", createdAt(p.CreatedAt)
	err = storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		// A custom policy may not shadow a system one: names are the binding
		// key, and "EuEcsFullAccess" must mean the same thing in every account.
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM ram_policy WHERE account_id = 0 AND policy_name = ?`, p.Name).Scan(&one)
		if err == nil {
			return errExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO ram_policy (policy_id, account_id, policy_name, document, document_text, scope, created_at)
			 VALUES (?, ?, ?, ?, ?, 2, ?)`,
			p.ID, p.AccountID, p.Name, p.Document, p.Document, p.CreatedAt)
		if storage.IsDuplicateKey(err) {
			return errExists
		}
		return err
	})
	if err != nil {
		return storedPolicy{}, err
	}
	return p, nil
}

// attachedNames loads "owner id → attached policy names" (attachment order)
// for one account from ram_user_policy or ram_role_policy.
func attachedNames(ctx context.Context, q queryer, table, ownerCol string, accountID int64, ownerID int64) (map[int64][]string, error) {
	query := `SELECT x.` + ownerCol + `, p.policy_name FROM ` + table + ` x
	  JOIN ram_policy p ON p.policy_id = x.policy_id
	 WHERE x.account_id = ?`
	args := []any{accountID}
	if ownerID != 0 {
		query += ` AND x.` + ownerCol + ` = ?`
		args = append(args, ownerID)
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY x.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

// --- RAM users ---

const userSelect = `SELECT id, account_id, username, display_name, remark, status, created_at FROM iam_user`

func scanUser(row interface{ Scan(...any) error }) (ramUser, error) {
	var (
		u               ramUser
		display, remark sql.NullString
	)
	if err := row.Scan(&u.ID, &u.AccountID, &u.Username, &display, &remark, &u.Status, &u.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ramUser{}, errNotFound
		}
		return ramUser{}, err
	}
	u.DisplayName, u.Remark, u.CreatedAt = display.String, remark.String, u.CreatedAt.UTC()
	u.Policies = []string{}
	return u, nil
}

func (s *sqlStore) ListRAMUsers(ctx context.Context, accountID int64) ([]ramUser, error) {
	rows, err := s.db.QueryContext(ctx, userSelect+` WHERE account_id = ? AND status <> 3 ORDER BY id`, accountID)
	if err != nil {
		return nil, err
	}
	out := []ramUser{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names, err := attachedNames(ctx, s.db, "ram_user_policy", "user_id", accountID, 0)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if n := names[out[i].ID]; n != nil {
			out[i].Policies = n
		}
	}
	return out, nil
}

func (s *sqlStore) loadUser(ctx context.Context, row *sql.Row) (ramUser, error) {
	u, err := scanUser(row)
	if err != nil {
		return ramUser{}, err
	}
	names, err := attachedNames(ctx, s.db, "ram_user_policy", "user_id", u.AccountID, u.ID)
	if err != nil {
		return ramUser{}, err
	}
	if n := names[u.ID]; n != nil {
		u.Policies = n
	}
	return u, nil
}

func (s *sqlStore) RAMUserByID(ctx context.Context, accountID, id int64) (ramUser, error) {
	return s.loadUser(ctx, s.db.QueryRowContext(ctx, userSelect+` WHERE account_id = ? AND id = ?`, accountID, id))
}

func (s *sqlStore) RAMUserByName(ctx context.Context, accountID int64, username string) (ramUser, error) {
	return s.loadUser(ctx, s.db.QueryRowContext(ctx,
		userSelect+` WHERE account_id = ? AND username = ? AND status <> 3`, accountID, username))
}

func (s *sqlStore) CreateRAMUser(ctx context.Context, u ramUser) (ramUser, error) {
	u.Policies = dedupeNames(u.Policies)
	id, err := s.userIDs.Next(ctx)
	if err != nil {
		return ramUser{}, err
	}
	u.ID, u.CreatedAt = id, createdAt(u.CreatedAt)
	err = storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		ids, err := resolvePolicyIDs(ctx, tx, u.AccountID, u.Policies)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO iam_user (id, account_id, username, display_name, remark, status, version, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, 0, ?)`,
			u.ID, u.AccountID, u.Username, nullStr(u.DisplayName), nullStr(u.Remark), u.Status, u.CreatedAt); err != nil {
			if storage.IsDuplicateKey(err) {
				return errExists // uk_account_name: a live user holds the name
			}
			return err
		}
		for _, pid := range ids {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO ram_user_policy (account_id, user_id, policy_id, created_at) VALUES (?, ?, ?, ?)`,
				u.AccountID, u.ID, pid, u.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ramUser{}, err
	}
	if u.Policies == nil {
		u.Policies = []string{}
	}
	return u, nil
}

func (s *sqlStore) DeleteRAMUser(ctx context.Context, accountID, id int64) error {
	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var (
			status   int
			username string
		)
		err := tx.QueryRowContext(ctx,
			`SELECT status, username FROM iam_user WHERE account_id = ? AND id = ? FOR UPDATE`, accountID, id).Scan(&status, &username)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if status == 3 {
			return errAlreadyDeleted
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE iam_user SET status = 3, username = ?, version = version + 1 WHERE id = ?`,
			deletedUsername(username, id), id)
		return err
	})
}

// --- RAM roles ---

const roleSelect = `SELECT id, account_id, role_name, description, status, created_at FROM iam_role`

// defaultAssumePolicy is the trust policy of a console-created role: only
// principals of the owning account may assume it (handleAssumeRole enforces
// the same rule).
const defaultAssumePolicy = `{"Version":"1","Statement":[{"Effect":"Allow","Action":"sts:AssumeRole","Principal":{"Account":"self"}}]}`

func scanRole(row interface{ Scan(...any) error }) (ramRole, error) {
	var (
		r    ramRole
		desc sql.NullString
	)
	if err := row.Scan(&r.ID, &r.AccountID, &r.Name, &desc, &r.Status, &r.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ramRole{}, errNotFound
		}
		return ramRole{}, err
	}
	r.Description, r.CreatedAt = desc.String, r.CreatedAt.UTC()
	r.Policies = []string{}
	return r, nil
}

func (s *sqlStore) ListRAMRoles(ctx context.Context, accountID int64) ([]ramRole, error) {
	rows, err := s.db.QueryContext(ctx, roleSelect+` WHERE account_id = ? ORDER BY id`, accountID)
	if err != nil {
		return nil, err
	}
	out := []ramRole{}
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names, err := attachedNames(ctx, s.db, "ram_role_policy", "role_id", accountID, 0)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if n := names[out[i].ID]; n != nil {
			out[i].Policies = n
		}
	}
	return out, nil
}

func (s *sqlStore) loadRole(ctx context.Context, row *sql.Row) (ramRole, error) {
	r, err := scanRole(row)
	if err != nil {
		return ramRole{}, err
	}
	names, err := attachedNames(ctx, s.db, "ram_role_policy", "role_id", r.AccountID, r.ID)
	if err != nil {
		return ramRole{}, err
	}
	if n := names[r.ID]; n != nil {
		r.Policies = n
	}
	return r, nil
}

func (s *sqlStore) RAMRoleByID(ctx context.Context, accountID, id int64) (ramRole, error) {
	return s.loadRole(ctx, s.db.QueryRowContext(ctx, roleSelect+` WHERE account_id = ? AND id = ?`, accountID, id))
}

func (s *sqlStore) RAMRoleByName(ctx context.Context, accountID int64, name string) (ramRole, error) {
	return s.loadRole(ctx, s.db.QueryRowContext(ctx, roleSelect+` WHERE account_id = ? AND role_name = ?`, accountID, name))
}

func (s *sqlStore) CreateRAMRole(ctx context.Context, r ramRole) (ramRole, error) {
	r.Policies = dedupeNames(r.Policies)
	id, err := s.roleIDs.Next(ctx)
	if err != nil {
		return ramRole{}, err
	}
	r.ID, r.CreatedAt = id, createdAt(r.CreatedAt)
	if r.Status == 0 {
		r.Status = 1
	}
	err = storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		ids, err := resolvePolicyIDs(ctx, tx, r.AccountID, r.Policies)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO iam_role (id, account_id, role_name, role_type, assume_policy, max_session_seconds, description, status, created_at)
			 VALUES (?, ?, ?, 1, ?, 3600, ?, ?, ?)`,
			r.ID, r.AccountID, r.Name, defaultAssumePolicy, nullStr(r.Description), r.Status, r.CreatedAt); err != nil {
			if storage.IsDuplicateKey(err) {
				return errExists
			}
			return err
		}
		for _, pid := range ids {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO ram_role_policy (account_id, role_id, policy_id, created_at) VALUES (?, ?, ?, ?)`,
				r.AccountID, r.ID, pid, r.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ramRole{}, err
	}
	if r.Policies == nil {
		r.Policies = []string{}
	}
	return r, nil
}
