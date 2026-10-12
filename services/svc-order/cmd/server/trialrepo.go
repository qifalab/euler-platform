package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/trial"
)

// --- trial state (B7 免费试用) ------------------------------------------------------
//
// pkg-go/trial decides; this file keeps what it decides on. Per-account claim
// history (t_trial_record), the cross-account identity registry
// (t_trial_identity) and the activity budget (t_trial_activity.active_count)
// are shared by every replica in SQL mode: a claim is ONE transaction that
// locks the activity row, retires lapsed vouchers, reads the counters, runs
// trial.Admit and — when admitted — inserts the voucher into t_coupon and
// bumps all three counters. The activity row lock serialises claims across
// replicas, so two replicas cannot both spend the last budget slot or both
// admit one identity twice.

// trialActivityID is the default activity (svc-catalog V3 seed).
const trialActivityID = "trial-default"

// trialClaim is an admitted claim's voucher.
type trialClaim struct {
	Coupon    couponRecord
	ValidDays int
}

// trialRepo holds the trial admission state.
type trialRepo interface {
	// Claim evaluates the claim and, when admitted, mints the voucher and
	// records the claim atomically. A denial returns the verdict and no claim.
	Claim(acct int64, rn realNameStatus, p trial.Policy, now time.Time) (trial.Verdict, *trialClaim, error)
	// Preview evaluates the claim without making it.
	Preview(acct int64, rn realNameStatus, p trial.Policy, now time.Time) (trial.Verdict, error)
}

// trialAccountState is the engine's account input with the real-name fields
// taken from rn.
func trialAccountState(active, lifetime int, last time.Time, rn realNameStatus) trial.AccountState {
	return trial.AccountState{
		Active: active, Lifetime: lifetime, LastClaimedAt: last,
		RealNameVerified: rn.Verified, IdentityKey: rn.IdentityKey,
	}
}

// trialVoucher builds the voucher an admitted claim mints. Expiry is kept to
// whole seconds: t_coupon.expire_at is DATETIME, and both stores must agree on
// the instant the voucher lapses.
func trialVoucher(id string, acct int64, face pricing.Amount, validDays int, now time.Time) couponRecord {
	return couponRecord{
		Coupon: pricing.Coupon{
			CouponID: id, AccountID: acct, Kind: pricing.KindVoucher,
			FaceValue: face, RemainValue: face,
			ExpireAt: now.UTC().Add(time.Duration(validDays) * 24 * time.Hour).Truncate(time.Second),
			Status:   couponUnused,
		},
		Source:    couponSourceTrial,
		PromoID:   trialActivityID,
		CreatedAt: now.UTC(),
	}
}

// --- in-memory store -----------------------------------------------------------------

// memTrialRepo is the single-process twin. Its vouchers go into the same
// memCouponRepo the order repo spends from.
type memTrialRepo struct {
	mu      sync.Mutex
	coupons *memCouponRepo
	// accounts: per-account Active / Lifetime / LastClaimedAt.
	accounts map[int64]trial.AccountState
	// identityCounts: identityKey → claims across accounts (t_trial_identity).
	identityCounts map[string]int64
	// globalActive: live vouchers platform-wide (t_trial_activity.active_count).
	globalActive int64
	// live lists the vouchers still counted as active; releaseExpired retires
	// them from Active and globalActive when their validity ends.
	live []liveTrial
	seq  int64
	// faceValue / validDays mirror the t_trial_activity seed.
	faceValue pricing.Amount
	validDays int
}

// liveTrial is one issued voucher still counted against the active limits.
type liveTrial struct {
	account  int64
	expireAt time.Time
}

var _ trialRepo = (*memTrialRepo)(nil)

func newMemTrialRepo(coupons *memCouponRepo) *memTrialRepo {
	return &memTrialRepo{
		coupons:        coupons,
		accounts:       make(map[int64]trial.AccountState),
		identityCounts: make(map[string]int64),
		faceValue:      trialFaceValue,
		validDays:      trial.VoucherValidDays,
	}
}

// releaseExpired retires every voucher whose validity has ended (callers hold
// mu). Active and globalActive count trials that are claimed and not yet
// expired; without this they only ever grew.
func (m *memTrialRepo) releaseExpired(now time.Time) {
	kept := m.live[:0]
	for _, v := range m.live {
		if now.Before(v.expireAt) {
			kept = append(kept, v)
			continue
		}
		if st, ok := m.accounts[v.account]; ok && st.Active > 0 {
			st.Active--
			m.accounts[v.account] = st
		}
		if m.globalActive > 0 {
			m.globalActive--
		}
	}
	m.live = kept
}

func (m *memTrialRepo) admit(acct int64, rn realNameStatus, p trial.Policy, now time.Time) trial.Verdict {
	m.releaseExpired(now)
	st := m.accounts[acct]
	return trial.Admit(p, trial.State{
		Account:        trialAccountState(st.Active, st.Lifetime, st.LastClaimedAt, rn),
		GlobalActive:   m.globalActive,
		IdentityCounts: m.identityCounts,
	}, now)
}

func (m *memTrialRepo) Preview(acct int64, rn realNameStatus, p trial.Policy, now time.Time) (trial.Verdict, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.admit(acct, rn, p, now), nil
}

func (m *memTrialRepo) Claim(acct int64, rn realNameStatus, p trial.Policy, now time.Time) (trial.Verdict, *trialClaim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.admit(acct, rn, p, now)
	if !v.Allowed {
		return v, nil, nil
	}
	m.seq++
	c := trialVoucher(fmt.Sprintf("TC%d", m.seq), acct, m.faceValue, m.validDays, now)
	if err := m.coupons.Issue(c); err != nil {
		return trial.Verdict{}, nil, err
	}
	st := m.accounts[acct]
	st.Active++
	st.Lifetime++
	st.LastClaimedAt = now
	m.accounts[acct] = st
	if rn.IdentityKey != "" {
		m.identityCounts[rn.IdentityKey]++
	}
	m.globalActive++
	m.live = append(m.live, liveTrial{account: acct, expireAt: c.ExpireAt})
	return v, &trialClaim{Coupon: c, ValidDays: m.validDays}, nil
}

// --- SQL store -------------------------------------------------------------------

// sqlTrialRepo keeps the trial state in trade_db (svc-catalog V3 tables).
type sqlTrialRepo struct {
	db *sql.DB
	// couponIDs mints t_coupon ids ("TC{n}") from the 号段 allocator.
	couponIDs *storage.Sequence
}

var _ trialRepo = (*sqlTrialRepo)(nil)

func newSQLTrialRepo(ctx context.Context, db *sql.DB) (*sqlTrialRepo, error) {
	ids, err := storage.OpenSequence(ctx, db, "t_coupon", 100)
	if err != nil {
		return nil, err
	}
	return &sqlTrialRepo{db: db, couponIDs: ids}, nil
}

// trialActivity is the locked t_trial_activity row.
type trialActivity struct {
	maxActive int64
	active    int64
	faceValue pricing.Amount
	validDays int
}

// errTrialRecorded aborts a claim whose t_trial_record row already exists
// (uk_account_activity): the account has claimed this activity before.
var errTrialRecorded = errors.New("trial: account already holds a claim on this activity")

func (r *sqlTrialRepo) Preview(acct int64, rn realNameStatus, p trial.Policy, now time.Time) (trial.Verdict, error) {
	v, _, err := r.run(acct, rn, p, now, false)
	return v, err
}

func (r *sqlTrialRepo) Claim(acct int64, rn realNameStatus, p trial.Policy, now time.Time) (trial.Verdict, *trialClaim, error) {
	return r.run(acct, rn, p, now, true)
}

func (r *sqlTrialRepo) run(acct int64, rn realNameStatus, p trial.Policy, now time.Time, claim bool) (trial.Verdict, *trialClaim, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	now = now.UTC()
	var (
		verdict trial.Verdict
		out     *trialClaim
	)
	err := storage.Tx(ctx, r.db, func(tx *sql.Tx) error {
		verdict, out = trial.Verdict{}, nil
		act, err := r.lockActivity(ctx, tx)
		if err != nil {
			return err
		}
		if err := r.releaseExpired(ctx, tx, &act, now); err != nil {
			return err
		}
		active, lifetime, last, err := r.accountState(ctx, tx, acct)
		if err != nil {
			return err
		}
		ids := map[string]int64{}
		if rn.IdentityKey != "" {
			n, err := r.identityCount(ctx, tx, rn.IdentityKey)
			if err != nil {
				return err
			}
			ids[rn.IdentityKey] = n
		}
		// The activity row is the budget's authority (运营 can resize it).
		p.MaxActiveGlobal = act.maxActive
		verdict = trial.Admit(p, trial.State{
			Account:        trialAccountState(active, lifetime, last, rn),
			GlobalActive:   act.active,
			IdentityCounts: ids,
		}, now)
		if !verdict.Allowed || !claim {
			return nil // the expiry sweep above still commits
		}
		seq, err := r.couponIDs.Next(ctx)
		if err != nil {
			return err
		}
		c := trialVoucher("TC"+strconv.FormatInt(seq, 10), acct, act.faceValue, act.validDays, now)
		if err := insertCoupon(ctx, tx, c); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO t_trial_record (activity_id, account_id, coupon_id, identity_key, state, claimed_at)
			 VALUES (?, ?, ?, ?, 0, ?)`,
			trialActivityID, acct, c.CouponID, rn.IdentityKey, now); err != nil {
			if storage.IsDuplicateKey(err) {
				return errTrialRecorded
			}
			return fmt.Errorf("trial: record claim of %d: %w", acct, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE t_trial_activity SET active_count = active_count + 1 WHERE activity_id = ?`,
			trialActivityID); err != nil {
			return fmt.Errorf("trial: bump active count: %w", err)
		}
		if rn.IdentityKey != "" {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO t_trial_identity (identity_key, claim_count, last_account, last_claimed_at)
				 VALUES (?, 1, ?, ?)
				 ON DUPLICATE KEY UPDATE claim_count = claim_count + 1, last_account = ?, last_claimed_at = ?`,
				rn.IdentityKey, acct, now, acct, now); err != nil {
				return fmt.Errorf("trial: identity registry: %w", err)
			}
		}
		out = &trialClaim{Coupon: c, ValidDays: act.validDays}
		return nil
	})
	if errors.Is(err, errTrialRecorded) {
		// The table-level backstop (uk_account_activity) fired: report it as
		// the lifetime rule it enforces.
		return trial.Verdict{Rule: trial.RuleLifetimeLimit, Detail: "账号累计试用次数已达上限"}, nil, nil
	}
	if err != nil {
		return trial.Verdict{}, nil, err
	}
	return verdict, out, nil
}

// lockActivity reads the activity row under FOR UPDATE: every claim (and
// preview) of the activity serialises here, across replicas.
func (r *sqlTrialRepo) lockActivity(ctx context.Context, tx *sql.Tx) (trialActivity, error) {
	var (
		a    trialActivity
		face string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT max_active_global, active_count, voucher_face_value, voucher_valid_days
		   FROM t_trial_activity WHERE activity_id = ? FOR UPDATE`, trialActivityID).
		Scan(&a.maxActive, &a.active, &face, &a.validDays)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("trial: activity %s is not configured (svc-catalog V3 seed)", trialActivityID)
	}
	if err != nil {
		return a, fmt.Errorf("trial: lock activity: %w", err)
	}
	if a.faceValue, err = pricing.ParseAmount(face); err != nil {
		return a, fmt.Errorf("trial: activity face value: %w", err)
	}
	return a, nil
}

// releaseExpired retires ACTIVE claims whose voucher has lapsed (→ EXPIRED)
// and returns their budget slots, under the activity lock the caller holds.
func (r *sqlTrialRepo) releaseExpired(ctx context.Context, tx *sql.Tx, a *trialActivity, now time.Time) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE t_trial_record r
		   JOIN t_coupon c ON c.coupon_id = r.coupon_id
		    SET r.state = 2, r.settled_at = ?
		  WHERE r.activity_id = ? AND r.state = 0 AND c.expire_at <= ?`,
		now, trialActivityID, now)
	if err != nil {
		return fmt.Errorf("trial: retire expired claims: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	a.active -= n
	if a.active < 0 {
		a.active = 0
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE t_trial_activity SET active_count = ? WHERE activity_id = ?`, a.active, trialActivityID); err != nil {
		return fmt.Errorf("trial: release budget: %w", err)
	}
	return nil
}

// accountState derives the account's counters from t_trial_record.
func (r *sqlTrialRepo) accountState(ctx context.Context, tx *sql.Tx, acct int64) (active, lifetime int, last time.Time, err error) {
	var lastAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(state = 0), 0), COUNT(*), MAX(claimed_at)
		   FROM t_trial_record WHERE account_id = ?`, acct).Scan(&active, &lifetime, &lastAt)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("trial: account state of %d: %w", acct, err)
	}
	return active, lifetime, lastAt.Time, nil
}

func (r *sqlTrialRepo) identityCount(ctx context.Context, tx *sql.Tx, key string) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx,
		`SELECT claim_count FROM t_trial_identity WHERE identity_key = ? FOR UPDATE`, key).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("trial: identity registry: %w", err)
	}
	return n, nil
}

// newTrialRepoFor builds the trial store matching the order repo's backend:
// trade_db when orders are persistent, the in-memory twin (sharing the order
// repo's coupon store) otherwise.
func newTrialRepoFor(ctx context.Context, repo orderRepo) (trialRepo, error) {
	switch r := repo.(type) {
	case *sqlOrderRepo:
		return newSQLTrialRepo(ctx, r.db)
	case *memOrderRepo:
		return newMemTrialRepo(r.coupons), nil
	default:
		return nil, fmt.Errorf("svc-order: no trial store for order repo %T", repo)
	}
}
