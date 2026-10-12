package main

import (
	"context"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/accesskey"
	"github.com/qifalab/euler-platform/kms"
	"github.com/qifalab/euler-platform/storage/sqltest"
	"github.com/qifalab/euler-platform/totp"
)

// mustSeed provisions (or finds) an account in the installed backend.
func mustSeed(t testing.TB, id int64, email, realName, pass string) account {
	t.Helper()
	if _, err := seedAccount(context.Background(), id, email, realName, pass); err != nil {
		t.Fatalf("seed account %d: %v", id, err)
	}
	return mustAccount(t, id)
}

// mustAccount reads an account from the installed backend.
func mustAccount(t testing.TB, id int64) account {
	t.Helper()
	a, err := st.AccountByID(context.Background(), id)
	if err != nil {
		t.Fatalf("account %d: %v", id, err)
	}
	return a
}

// setAccountStatus freezes (2) / closes (3) / restores (1) an account.
func setAccountStatus(t testing.TB, id int64, status int) {
	t.Helper()
	if _, err := st.UpdateAccount(context.Background(), id, func(a *account) error { a.Status = status; return nil }); err != nil {
		t.Fatalf("set status of %d: %v", id, err)
	}
}

// plantSession stores a refresh session for token, as login would.
func plantSession(t testing.TB, a account, token string, ttl time.Duration) {
	t.Helper()
	now := time.Now()
	if err := st.CreateSession(context.Background(), refreshSession{
		TokenHash: hashToken(token), AccountID: a.AccountID, TokenVersion: a.TokenVersion,
		FamilyID: newFamilyID(), ExpiresAt: now.Add(ttl), CreatedAt: now,
	}); err != nil {
		t.Fatalf("plant session: %v", err)
	}
}

// sessionLive reports whether the refresh token can still be exchanged.
func sessionLive(t testing.TB, token string) bool {
	t.Helper()
	s, err := st.SessionByHash(context.Background(), hashToken(token))
	if err != nil {
		return false
	}
	return s.live(time.Now())
}

// useBackend installs b for the duration of the test, with fresh limiters
// and TOTP replay memory (both are process-global), and restores the
// previous wiring afterwards.
func useBackend(t *testing.T, b backend) {
	t.Helper()
	prev := installBackend(b)
	saved := []*attemptLimiter{loginLimiter, mfaLimiter, passwordChangeLimiter, forgotSendEmailLimiter,
		forgotSendIPLimiter, forgotResetEmailLimiter, forgotResetIPLimiter, realnameMismatchLimiter,
		registerSendEmailLimiter, registerSendIPLimiter, registerVerifyEmailLimiter, registerVerifyIPLimiter}
	savedTOTP := totpVerifier
	loginLimiter = newAttemptLimiter(5, 15*time.Minute, 15*time.Minute)
	mfaLimiter = newAttemptLimiter(5, 15*time.Minute, 15*time.Minute)
	passwordChangeLimiter = newAttemptLimiter(5, 15*time.Minute, 15*time.Minute)
	forgotSendEmailLimiter = newAttemptLimiter(5, time.Hour, time.Hour)
	forgotSendIPLimiter = newAttemptLimiter(20, time.Hour, time.Hour)
	forgotResetEmailLimiter = newAttemptLimiter(maxResetAttempts, resetCodeTTL, resetCodeTTL)
	forgotResetIPLimiter = newAttemptLimiter(20, 15*time.Minute, 15*time.Minute)
	realnameMismatchLimiter = newAttemptLimiter(5, 24*time.Hour, 24*time.Hour)
	registerSendEmailLimiter = newAttemptLimiter(5, time.Hour, time.Hour)
	registerSendIPLimiter = newAttemptLimiter(20, time.Hour, time.Hour)
	registerVerifyEmailLimiter = newAttemptLimiter(maxResetAttempts, registerCodeTTL, registerCodeTTL)
	registerVerifyIPLimiter = newAttemptLimiter(20, 15*time.Minute, 15*time.Minute)
	totpVerifier = totp.NewVerifier()
	t.Cleanup(func() {
		installBackend(prev)
		loginLimiter, mfaLimiter, passwordChangeLimiter, forgotSendEmailLimiter = saved[0], saved[1], saved[2], saved[3]
		forgotSendIPLimiter, forgotResetEmailLimiter, forgotResetIPLimiter, realnameMismatchLimiter = saved[4], saved[5], saved[6], saved[7]
		registerSendEmailLimiter, registerSendIPLimiter, registerVerifyEmailLimiter, registerVerifyIPLimiter = saved[8], saved[9], saved[10], saved[11]
		totpVerifier = savedTOTP
	})
}

// testKeyring is a keyring with both purposes, as loadKeyring builds it.
func testKeyring(t testing.TB) *kms.KMS {
	t.Helper()
	k := kms.New()
	for _, p := range []kms.Purpose{kms.PurposeUser, kms.PurposeAK} {
		if err := k.GenerateMasterKey(p, 1); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

// sqlTestBackend is the account_db wiring over a scratch schema with the
// svc-iam DDL of record applied. Skips without EULER_DB_DSN.
func sqlTestBackend(t *testing.T) backend {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-iam"))
	s, err := newSQLStore(context.Background(), db)
	if err != nil {
		t.Fatalf("sql store: %v", err)
	}
	return backend{store: s, keys: testKeyring(t), akStore: accesskey.NewSQLStore(context.Background(), db), locks: sqlLocker{db: db}, db: db}
}

// forEachBackend runs fn once on a fresh memory backend and once on a fresh
// account_db scratch schema (skipped without EULER_DB_DSN): the parity
// harness — both must behave identically.
func forEachBackend(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		useBackend(t, memoryBackend())
		fn(t)
	})
	t.Run("mysql", func(t *testing.T) {
		b := sqlTestBackend(t)
		useBackend(t, b)
		fn(t)
	})
}
