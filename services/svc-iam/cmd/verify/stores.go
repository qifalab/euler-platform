package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/sts"
	"github.com/qifalab/euler-platform/verify"
)

// sqlNonceStore is the multi-replica anti-replay store (account_db.openapi_nonce).
// An in-process map only protects the replica that saw the request first.
type sqlNonceStore struct {
	db        *sql.DB
	mu        sync.Mutex
	lastSweep time.Time
	now       func() time.Time
}

func newSQLNonceStore(db *sql.DB) *sqlNonceStore {
	return &sqlNonceStore{db: db, now: time.Now, lastSweep: time.Now()}
}

// SetNX records the nonce and reports whether it was new. A row that exists
// but has expired is taken over (rows affected 2); an unexpired one is left
// untouched (rows affected 0) — that is a replay.
func (s *sqlNonceStore) SetNX(key string, ttl time.Duration) (bool, error) {
	sum := sha256.Sum256([]byte(key))
	k := hex.EncodeToString(sum[:])
	now := s.now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO openapi_nonce (nonce_key, expires_at) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE expires_at = IF(expires_at <= ?, VALUES(expires_at), expires_at)`,
		k, now.Add(ttl), now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	s.maybeSweep(now)
	return n > 0, nil
}

// maybeSweep deletes expired rows at most once a minute, in bounded batches.
func (s *sqlNonceStore) maybeSweep(now time.Time) {
	s.mu.Lock()
	if now.Sub(s.lastSweep) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.lastSweep = now
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM openapi_nonce WHERE expires_at <= ? LIMIT 5000`, now)
}

// sqlAccountStatus answers "is this account usable" from account_db.account.
// It replaced a stub that said yes for every account, so a frozen or closed
// account's AccessKeys kept working. A missing row is NOT usable (fail closed).
// Answers are cached briefly to stay inside the verify latency budget; a
// freeze therefore takes effect within accountCacheTTL.
type sqlAccountStatus struct {
	db    *sql.DB
	mu    sync.Mutex
	cache map[int64]accountCacheEntry
	now   func() time.Time
}

type accountCacheEntry struct {
	usable bool
	at     time.Time
}

const accountCacheTTL = 10 * time.Second

func newSQLAccountStatus(db *sql.DB) *sqlAccountStatus {
	return &sqlAccountStatus{db: db, cache: map[int64]accountCacheEntry{}, now: time.Now}
}

func (s *sqlAccountStatus) IsAccountUsable(accountID int64) (bool, error) {
	now := s.now()
	s.mu.Lock()
	if e, ok := s.cache[accountID]; ok && now.Sub(e.at) < accountCacheTTL {
		s.mu.Unlock()
		return e.usable, nil
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var status int
	err := s.db.QueryRowContext(ctx, `SELECT status FROM account WHERE account_id = ?`, accountID).Scan(&status)
	usable := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
		usable = false
	case err != nil:
		return false, err // not cached: a DB blip must not pin a verdict
	default:
		usable = status == 1
	}
	s.mu.Lock()
	s.cache[accountID] = accountCacheEntry{usable: usable, at: now}
	s.mu.Unlock()
	return usable, nil
}

// stsValidator adapts pkg-go/sts (sealed tokens) to verify.STSValidator.
type stsValidator struct{ issuer *sts.Issuer }

func (v stsValidator) ValidateToken(token string, now time.Time) (verify.STSCredential, error) {
	c, err := v.issuer.Validate(token, now)
	if err != nil {
		return verify.STSCredential{}, err
	}
	return verify.STSCredential{
		AccessKey: c.AccessKey,
		SecretKey: c.SecretKey,
		AccountID: c.AccountID,
		Principal: "role/" + c.Role,
	}, nil
}

// devSeedAccountRow makes the dev seed AK's owner exist in account_db, so the
// fail-closed account check does not reject the local e2e flow.
func devSeedAccountRow(ctx context.Context, db *sql.DB, accountID int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT IGNORE INTO account (account_id, account_name, password_hash, status)
		 VALUES (?, ?, '!dev-seed-no-login', 1)`,
		accountID, "dev-seed-"+strconv.FormatInt(accountID, 10))
	return err
}
