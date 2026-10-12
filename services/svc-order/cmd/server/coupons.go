package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
)

// --- coupons on orders (B7 券体系 × 订单链路) -------------------------------
//
// t_coupon (svc-catalog V1/V3 DDL) holds the coupons themselves; this service
// spends them. A coupon the customer selects at checkout is priced on top of
// the catalogue quote with pkg-go/pricing's ③ rules (pricing.ApplyCoupons) and
// then follows the order through the order_coupon_use journal (svc-order V3):
//
//	create  → LOCKED    reserved for this order (one live reservation per coupon)
//	pay     → CONSUMED  t_coupon.remain_value spent (version-guarded)
//	cancel  → RELEASED  the reservation is dropped, nothing was spent
//	refund  → RESTORED  the spent value is given back
//
// Every step commits in the transaction that moves the order (orderRepo.Create
// / SaveTransition), and each only acts on rows still in the previous state, so
// a replayed transition is a no-op per (coupon, order).

// t_coupon.status (svc-catalog V1 DDL).
const (
	couponUnused  = 0 // 未用
	couponPartial = 1 // 部分使用
	couponUsedUp  = 2 // 用尽
	couponExpired = 3 // 过期 (a sweeper's verdict; expiry is also read from expire_at)
)

// useState is order_coupon_use.state (svc-order V3 DDL).
type useState int

const (
	useLocked   useState = 0
	useConsumed useState = 1
	useReleased useState = 2
	useRestored useState = 3
)

const (
	// couponSourceTrial marks the 免费试用 vouchers (t_coupon.source).
	couponSourceTrial = "TRIAL"
	// maxCouponsPerOrder bounds couponIds on one order.
	maxCouponsPerOrder = 5
	// maxCouponIDLen is t_coupon.coupon_id's width.
	maxCouponIDLen = 32
	// centQuantum is one 分 in micro-yuan: orders settle in whole 分, so every
	// coupon deduction on an order is expressed in it (pricing.ApplyCoupons).
	centQuantum = pricing.Amount(10_000)
)

// couponRecord is one t_coupon row as the order plane reads it.
type couponRecord struct {
	pricing.Coupon
	Source    string
	PromoID   string
	Version   int
	CreatedAt time.Time
	// LockedBy is the order holding the coupon's live reservation (an
	// order_coupon_use row in LOCKED), 0 when none does.
	LockedBy int64
}

// couponUse is one order_coupon_use row, with its coupon's expiry.
type couponUse struct {
	CouponID string
	OrderID  int64
	Amount   pricing.Amount
	State    useState
	ExpireAt time.Time
}

// couponRepo is the read side of the coupon store, plus issuance (operator
// grants and tests; trial claims mint through trialRepo in their own
// transaction). The order-side effects (reserve/consume/release/restore) are
// part of orderRepo's Create and SaveTransition, which own the transaction.
type couponRepo interface {
	// List returns the account's coupons, soonest expiry first.
	List(accountID int64) ([]couponRecord, error)
	// Get returns the named coupons the account owns; a missing or foreign id
	// is simply absent from the map.
	Get(accountID int64, ids []string) (map[string]couponRecord, error)
	// Issue inserts a new coupon (errDuplicateCoupon for a taken id).
	Issue(c couponRecord) error
}

var errDuplicateCoupon = errors.New("coupon: coupon id already exists")

// couponUnavailableError reports a coupon that cannot take the step asked of
// it — reserved by another order, spent or expired in the meantime. The order
// create surfaces it as Order.CouponInvalid.
type couponUnavailableError struct {
	CouponID string
	Reason   string
}

func (e *couponUnavailableError) Error() string { return "coupon " + e.CouponID + " " + e.Reason }

// couponInvalidError is a client-facing verdict on the couponIds of a create
// (400 Order.CouponInvalid); the message names the coupon and the reason.
type couponInvalidError struct{ msg string }

func (e *couponInvalidError) Error() string { return e.msg }

func invalidCoupon(format string, args ...any) error {
	return &couponInvalidError{msg: fmt.Sprintf(format, args...)}
}

// --- coupon state rules (shared by both stores) ----------------------------------

func couponIsExpired(c couponRecord, now time.Time) bool {
	return c.Status == couponExpired || (!c.ExpireAt.IsZero() && !now.Before(c.ExpireAt))
}

func couponIsUsedUp(c couponRecord) bool {
	if c.Status == couponUsedUp {
		return true
	}
	return c.EffectiveKind() == pricing.KindVoucher && c.RemainValue <= 0
}

// couponAPIStatus is the coupon's status as the API reports it. Spent wins over
// expired (a used coupon is history either way), expired over reserved (an
// expired reservation cannot be paid with).
func couponAPIStatus(c couponRecord, now time.Time) string {
	switch {
	case couponIsUsedUp(c):
		return "USED"
	case couponIsExpired(c, now):
		return "EXPIRED"
	case c.LockedBy != 0:
		return "LOCKED"
	default:
		return "USABLE"
	}
}

// couponAPIKind maps the DDL's kind onto the API's names.
func couponAPIKind(k pricing.CouponKind) string {
	switch k {
	case pricing.KindThreshold:
		return "FULL_REDUCTION"
	case pricing.KindRate:
		return "DISCOUNT"
	case pricing.KindVoucher:
		return "VOUCHER"
	default:
		return string(k)
	}
}

// reserveCheck says whether coupon c may take a reservation of amount for an
// order of acct at now. The handler priced the coupon moments earlier; this is
// the re-check under the row lock, so a coupon spent or reserved in between is
// refused rather than double-booked.
func reserveCheck(c couponRecord, acct, orderID int64, amount pricing.Amount, now time.Time) error {
	switch {
	case c.AccountID != acct:
		return &couponUnavailableError{c.CouponID, "does not exist"}
	case couponIsUsedUp(c):
		return &couponUnavailableError{c.CouponID, "has been used up"}
	case couponIsExpired(c, now):
		return &couponUnavailableError{c.CouponID, "has expired"}
	case c.LockedBy != 0 && c.LockedBy != orderID:
		return &couponUnavailableError{c.CouponID, "is reserved by another unpaid order"}
	case c.EffectiveKind() == pricing.KindVoucher && c.RemainValue < amount:
		return &couponUnavailableError{c.CouponID, "no longer covers the deduction"}
	case c.EffectiveKind() != pricing.KindVoucher && c.Status != couponUnused:
		return &couponUnavailableError{c.CouponID, "has been used"}
	}
	return nil
}

// couponAfterConsume is the coupon's remain_value and status after amount was
// spent on it. A voucher is spent down (partially, if the order took less than
// its balance); a 满减/折扣券 is single-use.
func couponAfterConsume(c couponRecord, amount pricing.Amount) (pricing.Amount, int, error) {
	if c.EffectiveKind() != pricing.KindVoucher {
		if c.Status == couponUsedUp || c.Status == couponPartial {
			return 0, 0, &couponUnavailableError{c.CouponID, "has been used"}
		}
		return 0, couponUsedUp, nil
	}
	if amount > c.RemainValue {
		return 0, 0, &couponUnavailableError{c.CouponID, "no longer covers the deduction"}
	}
	remain := c.RemainValue - amount
	switch {
	case remain == 0:
		return 0, couponUsedUp, nil
	case c.Status == couponExpired:
		return remain, couponExpired, nil
	default:
		return remain, couponPartial, nil
	}
}

// couponAfterRestore is the coupon's remain_value and status once amount is
// given back (refund). A voucher never exceeds its face value; an expiry
// verdict is kept.
func couponAfterRestore(c couponRecord, amount pricing.Amount) (pricing.Amount, int) {
	if c.EffectiveKind() != pricing.KindVoucher {
		if c.Status == couponExpired {
			return c.FaceValue, couponExpired
		}
		return c.FaceValue, couponUnused
	}
	remain := c.RemainValue + amount
	if remain > c.FaceValue {
		remain = c.FaceValue
	}
	switch {
	case c.Status == couponExpired:
		return remain, couponExpired
	case remain >= c.FaceValue:
		return remain, couponUnused
	default:
		return remain, couponPartial
	}
}

func cloneCoupon(c couponRecord) couponRecord {
	c.ProductCodes = append([]string(nil), c.ProductCodes...)
	c.ChargeTypes = append([]pricing.ChargeType(nil), c.ChargeTypes...)
	c.SKUCodes = append([]string(nil), c.SKUCodes...)
	return c
}

func sortCoupons(cs []couponRecord) {
	sort.Slice(cs, func(i, j int) bool {
		if !cs[i].ExpireAt.Equal(cs[j].ExpireAt) {
			return cs[i].ExpireAt.Before(cs[j].ExpireAt)
		}
		return cs[i].CouponID < cs[j].CouponID
	})
}

// --- in-memory store -----------------------------------------------------------

// memCouponRepo is the in-memory twin of t_coupon + order_coupon_use.
type memCouponRepo struct {
	mu      sync.Mutex
	coupons map[string]couponRecord
	uses    []*couponUse // the journal, in insertion order (≡ id order)
}

var _ couponRepo = (*memCouponRepo)(nil)

func newMemCouponRepo() *memCouponRepo {
	return &memCouponRepo{coupons: make(map[string]couponRecord)}
}

// lockedBy is the order holding coupon id's live reservation (callers hold mu).
func (m *memCouponRepo) lockedBy(id string) int64 {
	for _, u := range m.uses {
		if u.CouponID == id && u.State == useLocked {
			return u.OrderID
		}
	}
	return 0
}

func (m *memCouponRepo) view(c couponRecord) couponRecord {
	c = cloneCoupon(c)
	c.LockedBy = m.lockedBy(c.CouponID)
	return c
}

func (m *memCouponRepo) List(accountID int64) ([]couponRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]couponRecord, 0)
	for _, c := range m.coupons {
		if c.AccountID == accountID {
			out = append(out, m.view(c))
		}
	}
	sortCoupons(out)
	return out, nil
}

func (m *memCouponRepo) Get(accountID int64, ids []string) (map[string]couponRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]couponRecord, len(ids))
	for _, id := range ids {
		if c, ok := m.coupons[id]; ok && c.AccountID == accountID {
			out[id] = m.view(c)
		}
	}
	return out, nil
}

func (m *memCouponRepo) Issue(c couponRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issueLocked(c)
}

func (m *memCouponRepo) issueLocked(c couponRecord) error {
	if c.CouponID == "" || len(c.CouponID) > maxCouponIDLen {
		return fmt.Errorf("coupon: invalid coupon id %q", c.CouponID)
	}
	if _, taken := m.coupons[c.CouponID]; taken {
		return fmt.Errorf("%w: %s", errDuplicateCoupon, c.CouponID)
	}
	c = cloneCoupon(c)
	c.LockedBy = 0
	if c.Source == "" {
		c.Source = couponSourceTrial // the column default
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	m.coupons[c.CouponID] = c
	return nil
}

// reserve writes the LOCKED rows of a new order, all or nothing (callers hold
// the order repo's lock; this takes the coupon lock).
func (m *memCouponRepo) reserve(acct, orderID int64, uses []pricing.CouponUse, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range uses {
		c, ok := m.coupons[u.CouponID]
		if !ok {
			return &couponUnavailableError{u.CouponID, "does not exist"}
		}
		if err := reserveCheck(m.view(c), acct, orderID, u.Amount, now); err != nil {
			return err
		}
	}
	for _, u := range uses {
		if m.lockedBy(u.CouponID) == orderID {
			continue // already reserved for this order: idempotent
		}
		m.uses = append(m.uses, &couponUse{CouponID: u.CouponID, OrderID: orderID, Amount: u.Amount, State: useLocked})
	}
	return nil
}

// orderRows returns the journal rows of an order in the given state (callers
// hold mu).
func (m *memCouponRepo) orderRows(acct, orderID int64, st useState) []*couponUse {
	var out []*couponUse
	for _, u := range m.uses {
		if u.OrderID == orderID && u.State == st {
			if c, ok := m.coupons[u.CouponID]; ok && c.AccountID == acct {
				out = append(out, u)
			}
		}
	}
	return out
}

// consume spends the order's reserved coupons (PENDING_PAYMENT → PAID).
func (m *memCouponRepo) consume(acct, orderID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.orderRows(acct, orderID, useLocked)
	next := make(map[string]couponRecord, len(rows))
	for _, u := range rows {
		c := m.coupons[u.CouponID]
		remain, status, err := couponAfterConsume(c, u.Amount)
		if err != nil {
			return err
		}
		c.RemainValue, c.Status = remain, status
		c.Version++
		next[u.CouponID] = c
	}
	for _, u := range rows {
		m.coupons[u.CouponID] = next[u.CouponID]
		u.State = useConsumed
	}
	return nil
}

// release drops the order's reservations (→ CANCELLED).
func (m *memCouponRepo) release(acct, orderID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.orderRows(acct, orderID, useLocked) {
		u.State = useReleased
	}
}

// restore gives back what the order spent (→ REFUNDED).
func (m *memCouponRepo) restore(acct, orderID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.orderRows(acct, orderID, useConsumed) {
		c := m.coupons[u.CouponID]
		c.RemainValue, c.Status = couponAfterRestore(c, u.Amount)
		c.Version++
		m.coupons[u.CouponID] = c
		u.State = useRestored
	}
}

func (m *memCouponRepo) orderUses(acct, orderID int64) []couponUse {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []couponUse
	for _, u := range m.uses {
		if u.OrderID != orderID {
			continue
		}
		c, ok := m.coupons[u.CouponID]
		if !ok || c.AccountID != acct {
			continue
		}
		cp := *u
		cp.ExpireAt = c.ExpireAt
		out = append(out, cp)
	}
	return out
}

// --- SQL store -------------------------------------------------------------------

// sqlCouponRepo reads and issues t_coupon rows; the order-side journal writes
// run inside sqlOrderRepo's transactions (the helpers below).
type sqlCouponRepo struct{ db *sql.DB }

var _ couponRepo = (*sqlCouponRepo)(nil)

// couponSelect reads a coupon with its live reservation (the LOCKED journal
// row, found through order_coupon_use.uk_locked_coupon).
const couponSelect = `SELECT c.coupon_id, c.account_id, c.kind, c.face_value, c.threshold, c.rate_bp,
	c.cap_amount, c.remain_value, c.scope_json, c.source, c.promo_id, c.status, c.expire_at,
	c.version, c.created_at, COALESCE(u.order_id, 0)
	FROM t_coupon c
	LEFT JOIN order_coupon_use u ON u.locked_coupon_id = c.coupon_id`

// couponScope is t_coupon.scope_json; an empty list is unrestricted.
type couponScope struct {
	ProductCodes []string `json:"product_codes"`
	ChargeTypes  []string `json:"charge_types"`
	SKUCodes     []string `json:"sku_codes"`
}

func scanCoupon(sc rowScanner) (couponRecord, error) {
	var (
		c                         couponRecord
		kind, face, capAmt        string
		remain                    string
		threshold, scope, promoID sql.NullString
		rateBP                    sql.NullInt64
	)
	if err := sc.Scan(&c.CouponID, &c.AccountID, &kind, &face, &threshold, &rateBP, &capAmt, &remain,
		&scope, &c.Source, &promoID, &c.Status, &c.ExpireAt, &c.Version, &c.CreatedAt, &c.LockedBy); err != nil {
		return couponRecord{}, err
	}
	c.Kind = pricing.CouponKind(kind)
	c.PromoID = promoID.String
	c.RateBasisPoints = rateBP.Int64
	var err error
	if c.FaceValue, err = pricing.ParseAmount(face); err != nil {
		return couponRecord{}, fmt.Errorf("coupon %s face_value: %w", c.CouponID, err)
	}
	if c.CapAmount, err = pricing.ParseAmount(capAmt); err != nil {
		return couponRecord{}, fmt.Errorf("coupon %s cap_amount: %w", c.CouponID, err)
	}
	if c.RemainValue, err = pricing.ParseAmount(remain); err != nil {
		return couponRecord{}, fmt.Errorf("coupon %s remain_value: %w", c.CouponID, err)
	}
	if threshold.Valid {
		if c.Threshold, err = pricing.ParseAmount(threshold.String); err != nil {
			return couponRecord{}, fmt.Errorf("coupon %s threshold: %w", c.CouponID, err)
		}
	}
	if scope.Valid && strings.TrimSpace(scope.String) != "" && scope.String != "null" {
		// A scope that cannot be read must not become "unrestricted".
		var sc couponScope
		if err := json.Unmarshal([]byte(scope.String), &sc); err != nil {
			return couponRecord{}, fmt.Errorf("coupon %s scope_json: %w", c.CouponID, err)
		}
		c.ProductCodes, c.SKUCodes = sc.ProductCodes, sc.SKUCodes
		for _, ct := range sc.ChargeTypes {
			c.ChargeTypes = append(c.ChargeTypes, pricing.ChargeType(ct))
		}
	}
	return c, nil
}

func (s *sqlCouponRepo) query(where string, args ...any) ([]couponRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, couponSelect+" "+where, args...)
	if err != nil {
		return nil, fmt.Errorf("coupon: query: %w", err)
	}
	defer rows.Close()
	out := make([]couponRecord, 0, 8)
	for rows.Next() {
		c, err := scanCoupon(rows)
		if err != nil {
			return nil, fmt.Errorf("coupon: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("coupon: query: %w", err)
	}
	return out, nil
}

func (s *sqlCouponRepo) List(accountID int64) ([]couponRecord, error) {
	return s.query(`WHERE c.account_id = ? ORDER BY c.expire_at, c.coupon_id`, accountID)
}

func (s *sqlCouponRepo) Get(accountID int64, ids []string) (map[string]couponRecord, error) {
	out := make(map[string]couponRecord, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{accountID}
	for _, id := range ids {
		args = append(args, id)
	}
	cs, err := s.query(`WHERE c.account_id = ? AND c.coupon_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		out[c.CouponID] = c
	}
	return out, nil
}

func (s *sqlCouponRepo) Issue(c couponRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	return insertCoupon(ctx, s.db, c)
}

// execer is what insertCoupon needs: a pool or a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertCoupon writes a new t_coupon row. Amounts are DECIMAL(10,2): a value
// that is not cent-aligned is refused rather than rounded by the column.
func insertCoupon(ctx context.Context, ex execer, c couponRecord) error {
	if c.CouponID == "" || len(c.CouponID) > maxCouponIDLen {
		return fmt.Errorf("coupon: invalid coupon id %q", c.CouponID)
	}
	face, err := centAmount(c.FaceValue)
	if err != nil {
		return err
	}
	remain, err := centAmount(c.RemainValue)
	if err != nil {
		return err
	}
	capAmt, err := centAmount(c.CapAmount)
	if err != nil {
		return err
	}
	var threshold, rateBP any
	if c.EffectiveKind() == pricing.KindThreshold {
		t, err := centAmount(c.Threshold)
		if err != nil {
			return err
		}
		threshold = t
	}
	if c.EffectiveKind() == pricing.KindRate {
		rateBP = c.RateBasisPoints
	}
	var scope any
	if len(c.ProductCodes) > 0 || len(c.ChargeTypes) > 0 || len(c.SKUCodes) > 0 {
		sc := couponScope{ProductCodes: c.ProductCodes, SKUCodes: c.SKUCodes}
		for _, ct := range c.ChargeTypes {
			sc.ChargeTypes = append(sc.ChargeTypes, string(ct))
		}
		raw, err := json.Marshal(sc)
		if err != nil {
			return fmt.Errorf("coupon: encode scope: %w", err)
		}
		scope = string(raw)
	}
	source := c.Source
	if source == "" {
		source = couponSourceTrial // the column default
	}
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO t_coupon
		   (coupon_id, kind, account_id, face_value, threshold, rate_bp, cap_amount, remain_value,
		    scope_json, source, promo_id, status, expire_at, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		c.CouponID, string(c.EffectiveKind()), c.AccountID, face, threshold, rateBP, capAmt, remain,
		scope, source, nullableString(c.PromoID), c.Status, c.ExpireAt.UTC()); err != nil {
		if storage.IsDuplicateKey(err) {
			return fmt.Errorf("%w: %s", errDuplicateCoupon, c.CouponID)
		}
		return fmt.Errorf("coupon: insert %s: %w", c.CouponID, err)
	}
	return nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// lockCoupons reads the named coupons under a row lock (FOR UPDATE OF c, in
// coupon_id order so concurrent lockers cannot deadlock on each other).
func lockCoupons(ctx context.Context, tx *sql.Tx, ids []string) (map[string]couponRecord, error) {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	args := make([]any, len(sorted))
	for i, id := range sorted {
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx,
		couponSelect+` WHERE c.coupon_id IN (`+placeholders(len(sorted))+`) ORDER BY c.coupon_id FOR UPDATE OF c`, args...)
	if err != nil {
		return nil, fmt.Errorf("coupon: lock: %w", err)
	}
	defer rows.Close()
	out := make(map[string]couponRecord, len(sorted))
	for rows.Next() {
		c, err := scanCoupon(rows)
		if err != nil {
			return nil, fmt.Errorf("coupon: lock scan: %w", err)
		}
		out[c.CouponID] = c
	}
	return out, rows.Err()
}

// sqlReserveCoupons writes a new order's LOCKED rows inside its create
// transaction. uk_locked_coupon backs the check: a reservation that raced past
// it collides there instead of double-booking the coupon.
func sqlReserveCoupons(ctx context.Context, tx *sql.Tx, acct, orderID int64, uses []pricing.CouponUse, now time.Time) error {
	if len(uses) == 0 {
		return nil
	}
	ids := make([]string, len(uses))
	for i, u := range uses {
		ids[i] = u.CouponID
	}
	locked, err := lockCoupons(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, u := range uses {
		c, ok := locked[u.CouponID]
		if !ok {
			return &couponUnavailableError{u.CouponID, "does not exist"}
		}
		if err := reserveCheck(c, acct, orderID, u.Amount, now); err != nil {
			return err
		}
	}
	for _, u := range uses {
		if locked[u.CouponID].LockedBy == orderID {
			continue // already reserved for this order: idempotent
		}
		amt, err := centAmount(u.Amount)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_coupon_use (coupon_id, order_id, account_id, amount, state) VALUES (?, ?, ?, ?, ?)`,
			u.CouponID, orderID, acct, amt, int(useLocked)); err != nil {
			if storage.IsDuplicateKey(err) {
				return &couponUnavailableError{u.CouponID, "is reserved by another unpaid order"}
			}
			return fmt.Errorf("coupon: reserve %s for order %d: %w", u.CouponID, orderID, err)
		}
	}
	return nil
}

type journalRow struct {
	id       int64
	couponID string
	amount   pricing.Amount
}

// lockJournal reads (and locks) an order's journal rows in state st.
func lockJournal(ctx context.Context, tx *sql.Tx, acct, orderID int64, st useState) ([]journalRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, coupon_id, amount FROM order_coupon_use
		  WHERE order_id = ? AND account_id = ? AND state = ?
		  ORDER BY coupon_id FOR UPDATE`, orderID, acct, int(st))
	if err != nil {
		return nil, fmt.Errorf("coupon: journal of order %d: %w", orderID, err)
	}
	defer rows.Close()
	var out []journalRow
	for rows.Next() {
		var (
			r   journalRow
			amt string
		)
		if err := rows.Scan(&r.id, &r.couponID, &amt); err != nil {
			return nil, fmt.Errorf("coupon: journal scan: %w", err)
		}
		if r.amount, err = pricing.ParseAmount(amt); err != nil {
			return nil, fmt.Errorf("coupon: journal amount: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// applyJournal moves an order's journal rows from → to, updating each coupon
// with next under its version. Nothing in state from is a no-op (a replay).
func applyJournal(ctx context.Context, tx *sql.Tx, acct, orderID int64, from, to useState,
	next func(c couponRecord, amount pricing.Amount) (pricing.Amount, int, error)) error {
	rows, err := lockJournal(ctx, tx, acct, orderID, from)
	if err != nil || len(rows) == 0 {
		return err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.couponID
	}
	coupons, err := lockCoupons(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, r := range rows {
		c, ok := coupons[r.couponID]
		if !ok {
			return fmt.Errorf("coupon: %s of order %d no longer exists", r.couponID, orderID)
		}
		remain, status, err := next(c, r.amount)
		if err != nil {
			return err
		}
		remainStr, err := centAmount(remain)
		if err != nil {
			return err
		}
		if err := storage.Affected(tx.ExecContext(ctx,
			`UPDATE t_coupon SET remain_value = ?, status = ?, version = version + 1
			  WHERE coupon_id = ? AND version = ?`,
			remainStr, status, c.CouponID, c.Version)); err != nil {
			return fmt.Errorf("coupon: update %s: %w", c.CouponID, err)
		}
		if err := storage.Affected(tx.ExecContext(ctx,
			`UPDATE order_coupon_use SET state = ? WHERE id = ? AND state = ?`,
			int(to), r.id, int(from))); err != nil {
			return fmt.Errorf("coupon: journal %s/%d: %w", c.CouponID, orderID, err)
		}
	}
	return nil
}

// sqlConsumeCoupons spends an order's reservations (→ PAID).
func sqlConsumeCoupons(ctx context.Context, tx *sql.Tx, acct, orderID int64) error {
	return applyJournal(ctx, tx, acct, orderID, useLocked, useConsumed, couponAfterConsume)
}

// sqlRestoreCoupons gives back what an order spent (→ REFUNDED).
func sqlRestoreCoupons(ctx context.Context, tx *sql.Tx, acct, orderID int64) error {
	return applyJournal(ctx, tx, acct, orderID, useConsumed, useRestored,
		func(c couponRecord, amount pricing.Amount) (pricing.Amount, int, error) {
			remain, status := couponAfterRestore(c, amount)
			return remain, status, nil
		})
}

// sqlReleaseCoupons drops an order's reservations (→ CANCELLED); the coupons
// themselves never changed.
func sqlReleaseCoupons(ctx context.Context, tx *sql.Tx, acct, orderID int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE order_coupon_use SET state = ? WHERE order_id = ? AND account_id = ? AND state = ?`,
		int(useReleased), orderID, acct, int(useLocked)); err != nil {
		return fmt.Errorf("coupon: release for order %d: %w", orderID, err)
	}
	return nil
}

func sqlOrderCouponUses(db *sql.DB, acct, orderID int64) ([]couponUse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx,
		`SELECT u.coupon_id, u.order_id, u.amount, u.state, c.expire_at
		   FROM order_coupon_use u
		   LEFT JOIN t_coupon c ON c.coupon_id = u.coupon_id
		  WHERE u.order_id = ? AND u.account_id = ?
		  ORDER BY u.id`, orderID, acct)
	if err != nil {
		return nil, fmt.Errorf("coupon: uses of order %d: %w", orderID, err)
	}
	defer rows.Close()
	var out []couponUse
	for rows.Next() {
		var (
			u      couponUse
			amt    string
			st     int
			expire sql.NullTime
		)
		if err := rows.Scan(&u.CouponID, &u.OrderID, &amt, &st, &expire); err != nil {
			return nil, fmt.Errorf("coupon: uses scan: %w", err)
		}
		if u.Amount, err = pricing.ParseAmount(amt); err != nil {
			return nil, fmt.Errorf("coupon: uses amount: %w", err)
		}
		u.State = useState(st)
		u.ExpireAt = expire.Time
		out = append(out, u)
	}
	return out, rows.Err()
}

// --- checkout: pricing the selected coupons ------------------------------------

// validateCouponIDs checks the shape of a create's couponIds (count, width,
// charset, duplicates). The ids travel through a GROUP_CONCAT on read, so the
// charset excludes its separator.
func validateCouponIDs(ids []string) string {
	if len(ids) > maxCouponsPerOrder {
		return fmt.Sprintf("at most %d coupons per order", maxCouponsPerOrder)
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || len(id) > maxCouponIDLen {
			return "couponIds must be non-empty ids of at most 32 characters"
		}
		for _, r := range id {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return "couponIds contain an invalid id: " + strconv.Quote(id)
			}
		}
		if seen[id] {
			return "couponIds lists " + id + " twice"
		}
		seen[id] = true
	}
	return ""
}

// priceCoupons validates the coupons the customer selected and prices them on
// top of list (the catalogue's quote, already in 分) with pkg-go/pricing's ③
// rules. Every selected coupon must exist on the account, be live and
// unreserved, fit the order's product/SKU/charge type, and actually deduct
// something — a coupon that would ride along unused is refused rather than
// silently reserved.
func (s *orderStore) priceCoupons(acct int64, ids []string, preq pricing.Request, list pricing.Amount) ([]pricing.CouponUse, pricing.Amount, error) {
	if list <= 0 {
		return nil, 0, invalidCoupon("this order has no upfront amount for coupons to deduct from")
	}
	got, err := s.repo.Coupons().Get(acct, ids)
	if err != nil {
		return nil, 0, err
	}
	now := preq.At
	selected := make([]pricing.Coupon, 0, len(ids))
	for _, id := range ids {
		c, ok := got[id]
		if !ok {
			return nil, 0, invalidCoupon("coupon %s does not exist", id)
		}
		switch couponAPIStatus(c, now) {
		case "USED":
			return nil, 0, invalidCoupon("coupon %s has been used up", id)
		case "EXPIRED":
			return nil, 0, invalidCoupon("coupon %s has expired", id)
		case "LOCKED":
			return nil, 0, invalidCoupon("coupon %s is reserved by unpaid order %d; pay or cancel that order first", id, c.LockedBy)
		}
		if !c.Usable(now, preq) {
			switch {
			case len(c.ProductCodes) > 0 && !containsString(c.ProductCodes, preq.ProductCode):
				return nil, 0, invalidCoupon("coupon %s does not apply to product %s", id, preq.ProductCode)
			case len(c.SKUCodes) > 0 && !containsString(c.SKUCodes, preq.SKUCode):
				return nil, 0, invalidCoupon("coupon %s does not apply to SKU %s", id, preq.SKUCode)
			case len(c.ChargeTypes) > 0:
				return nil, 0, invalidCoupon("coupon %s does not apply to %s orders", id, preq.ChargeType)
			default:
				return nil, 0, invalidCoupon("coupon %s is not usable", id)
			}
		}
		if c.EffectiveKind() == pricing.KindThreshold && list < c.Threshold {
			return nil, 0, invalidCoupon("coupon %s requires an order amount of at least %s", id, c.Threshold)
		}
		selected = append(selected, c.Coupon)
	}
	uses, total := pricing.ApplyCoupons(selected, preq, list, centQuantum)
	applied := make(map[string]bool, len(uses))
	for _, u := range uses {
		if u.Amount%centQuantum != 0 {
			// ApplyCoupons aligns every deduction for a cent-aligned list and
			// DECIMAL(10,2) coupon values; anything else is corrupt data.
			return nil, 0, fmt.Errorf("coupon %s deduction %s is not cent-aligned", u.CouponID, u.Amount)
		}
		applied[u.CouponID] = true
	}
	for _, c := range selected {
		if applied[c.CouponID] {
			continue
		}
		switch c.EffectiveKind() {
		case pricing.KindRate:
			return nil, 0, invalidCoupon("coupon %s cannot be combined: only one DISCOUNT coupon applies per order", c.CouponID)
		case pricing.KindThreshold:
			for _, o := range selected {
				if o.EffectiveKind() == pricing.KindThreshold && applied[o.CouponID] {
					return nil, 0, invalidCoupon("coupon %s cannot be combined: only one FULL_REDUCTION coupon applies per order", c.CouponID)
				}
			}
			return nil, 0, invalidCoupon("coupon %s requires an order amount of at least %s after other discounts", c.CouponID, c.Threshold)
		default:
			return nil, 0, invalidCoupon("coupon %s is not needed: the other coupons already cover the order amount", c.CouponID)
		}
	}
	return uses, total, nil
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// --- HTTP: GET /api/v1/coupons, GET /api/v1/coupons/{couponId} ---------------------

// couponToMap renders a coupon for the API.
func couponToMap(c couponRecord, status string) map[string]any {
	m := map[string]any{
		"couponId":     c.CouponID,
		"kind":         couponAPIKind(c.EffectiveKind()),
		"faceValue":    c.FaceValue.String(),
		"remainValue":  c.RemainValue.String(),
		"productCodes": nonNilStrings(c.ProductCodes),
		"skuCodes":     nonNilStrings(c.SKUCodes),
		"source":       c.Source,
		"expireAt":     c.ExpireAt.UTC().Format(time.RFC3339),
		"status":       status,
	}
	switch c.EffectiveKind() {
	case pricing.KindThreshold:
		m["thresholdAmount"] = c.Threshold.String()
	case pricing.KindRate:
		m["discountRateBp"] = c.RateBasisPoints
		if c.CapAmount > 0 {
			m["capAmount"] = c.CapAmount.String()
		}
	}
	if status == "LOCKED" {
		m["lockedOrderId"] = c.LockedBy
	}
	return m
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// couponProbe carries the optional order context of a coupon list call.
type couponProbe struct {
	req    pricing.Request
	amount *pricing.Amount
}

// applicable says whether coupon c would deduct something from an order
// described by the probe. A dimension the caller did not name (product, SKU,
// charge type) is not held against the coupon.
func (p couponProbe) applicable(c couponRecord, status string) bool {
	if status != "USABLE" {
		return false
	}
	probe := c.Coupon
	if p.req.ProductCode == "" {
		probe.ProductCodes = nil
	}
	if p.req.SKUCode == "" {
		probe.SKUCodes = nil
	}
	if p.req.ChargeType == "" {
		probe.ChargeTypes = nil
	}
	if !probe.Usable(p.req.At, p.req) {
		return false
	}
	if p.amount == nil {
		return true
	}
	_, total := pricing.ApplyCoupons([]pricing.Coupon{probe}, p.req, *p.amount, centQuantum)
	return total > 0
}

// handleCouponList implements GET /api/v1/coupons — the calling account's
// coupons, filtered by status (usable|used|expired|locked|all, default all).
// With productCode / skuCode / chargeType / amount each coupon also carries
// applicable: would it deduct from such an order.
func (s *orderStore) handleCouponList(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter := strings.ToUpper(strings.TrimSpace(q.Get("status")))
	switch filter {
	case "":
		filter = "ALL"
	case "ALL", "USABLE", "USED", "EXPIRED", "LOCKED":
	default:
		writeErr(w, "Common.InvalidParameter", 400, "status must be usable, used, expired, locked or all")
		return
	}
	now := s.machine.Now()
	probe := couponProbe{req: pricing.Request{
		AccountID: acct, ProductCode: q.Get("productCode"), SKUCode: q.Get("skuCode"),
		ChargeType: pricing.ChargeType(strings.ToUpper(q.Get("chargeType"))), At: now,
	}}
	if len(probe.req.ProductCode) > maxProductCodeLen || len(probe.req.SKUCode) > maxSKUCodeLen {
		writeErr(w, "Common.InvalidParameter", 400, "productCode or skuCode is too long")
		return
	}
	if probe.req.ChargeType != "" && !probe.req.ChargeType.Sellable() {
		writeErr(w, "Common.InvalidParameter", 400, "unknown chargeType")
		return
	}
	if raw := strings.TrimSpace(q.Get("amount")); raw != "" {
		amt, err := pricing.ParseAmount(raw)
		if err != nil || amt < 0 {
			writeErr(w, "Common.InvalidParameter", 400, "amount must be a non-negative decimal (yuan)")
			return
		}
		probe.amount = &amt
	}
	withApplicable := probe.req.ProductCode != "" || probe.req.SKUCode != "" || probe.req.ChargeType != "" || probe.amount != nil

	coupons, err := s.repo.Coupons().List(acct)
	if err != nil {
		slog.Error("coupon list failed", "account", acct, "err", err)
		writeErr(w, "Order.ListFailed", 500, "coupon list failed")
		return
	}
	out := make([]map[string]any, 0, len(coupons))
	for _, c := range coupons {
		st := couponAPIStatus(c, now)
		if filter != "ALL" && filter != st {
			continue
		}
		m := couponToMap(c, st)
		if withApplicable {
			m["applicable"] = probe.applicable(c, st)
		}
		out = append(out, m)
	}
	writeJSON(w, "OK", map[string]any{"coupons": out, "total": len(out)})
}

// handleCouponDetail implements GET /api/v1/coupons/{couponId}. A foreign or
// unknown id is 404 alike, so ids cannot be probed across accounts.
func (s *orderStore) handleCouponDetail(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	id := r.PathValue("couponId")
	if id == "" || len(id) > maxCouponIDLen {
		writeErr(w, "Order.CouponNotFound", 404, "coupon not found")
		return
	}
	got, err := s.repo.Coupons().Get(acct, []string{id})
	if err != nil {
		slog.Error("coupon lookup failed", "account", acct, "coupon", id, "err", err)
		writeErr(w, "Order.LookupFailed", 500, "coupon lookup failed")
		return
	}
	c, ok := got[id]
	if !ok {
		writeErr(w, "Order.CouponNotFound", 404, "coupon not found")
		return
	}
	writeJSON(w, "OK", couponToMap(c, couponAPIStatus(c, s.machine.Now())))
}
