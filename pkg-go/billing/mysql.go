package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/metering"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
)

// SQLSettleStore is the MySQL implementation of SettleStore, over trade_db's
// settle_charge and settle_lock (services/svc-billing/sql/V4).
//
// # One record per charge, one lock row per account
//
// settle_charge holds the plan and, once settled, the charge; uk_acc_charge
// (account_id, charge_id) is the settlement's idempotency arbiter — two
// replicas can never both create a plan for one ChargeID. settle_lock holds one
// row per account, and LockAccount takes it with SELECT ... FOR UPDATE: every
// replica serialises one account's settlements on that row, which is what lets
// a settlement read the cash balance, decide the waterfall and commit it
// without another replica's settlement of the same account interleaving.
//
// # Why the lock is a separate transaction
//
// The pack ledger and the cash ledger commit through their own stores (their
// own transactions), so the lock cannot be one transaction around all of them.
// It is a transaction that only holds the row lock, on its own pooled
// connection, for the duration of the critical section; the plan writes go
// through autocommit statements so their progress is durable the moment it is
// made — a plan written inside the lock transaction would vanish with a crash
// while the pack consumes it describes stayed committed. The record's version
// guard (and status = PLANNED on update) stays the last line of defence should
// the lock ever be lost mid-way (a dropped connection releases it).
//
// # Connection budget
//
// A lock holder parks one connection and needs others for the ledger writes.
// Waiters are therefore queued in process first (one per account, and at most
// lockSlots accounts at a time), so a burst of settlements can never park the
// whole pool on row locks and starve the holder of the connection it needs to
// finish.
type SQLSettleStore struct {
	db    *sql.DB
	local accountLocks
	slots chan struct{}
}

// DefaultSettleLockSlots bounds how many accounts one process settles at once
// against MySQL (see "Connection budget"). Each slot parks one pooled
// connection; storage.Open's pool holds 25.
const DefaultSettleLockSlots = 4

// NewSQLSettleStore wraps a pool bound to trade_db. A pool capped below
// 2×DefaultSettleLockSlots connections gets fewer slots (half its cap, at least
// one), so lock holders always leave connections for their own writes.
func NewSQLSettleStore(db *sql.DB) *SQLSettleStore {
	slots := DefaultSettleLockSlots
	if max := db.Stats().MaxOpenConnections; max > 0 && max/2 < slots {
		slots = max / 2
		if slots < 1 {
			slots = 1
		}
	}
	return &SQLSettleStore{db: db, slots: make(chan struct{}, slots)}
}

var _ SettleStore = (*SQLSettleStore)(nil)

// statementTimeout bounds one store call; the read/write ports carry no context
// (the handlers call them synchronously), so the bound lives here.
const statementTimeout = 10 * time.Second

// Record status codes (settle_charge.status).
const (
	settleStatusPlanned = 1
	settleStatusSettled = 2
)

// LockAccount takes the account's settlement row lock (see the type doc).
func (s *SQLSettleStore) LockAccount(ctx context.Context, accountID int64) (func(), error) {
	unlockLocal, err := s.local.lock(ctx, accountID)
	if err != nil {
		return nil, err
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		unlockLocal()
		return nil, ctx.Err()
	}
	fail := func(err error) (func(), error) {
		<-s.slots
		unlockLocal()
		return nil, err
	}
	// The row is created on first use in its own autocommit statement: taking
	// the lock is then a plain locking read of an existing row, which queues
	// waiters instead of setting up the insert-intention deadlock two
	// concurrent first INSERTs of one key produce (see storage.Tx).
	if _, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO settle_lock (account_id) VALUES (?)`, accountID); err != nil {
		return fail(fmt.Errorf("billing: create settle lock for account %d: %w", accountID, err))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fail(fmt.Errorf("billing: begin settle lock for account %d: %w", accountID, err))
	}
	var held int64
	if err := tx.QueryRowContext(ctx,
		`SELECT account_id FROM settle_lock WHERE account_id = ? FOR UPDATE`, accountID).Scan(&held); err != nil {
		_ = tx.Rollback()
		return fail(fmt.Errorf("billing: take settle lock for account %d: %w", accountID, err))
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// Nothing was written in the lock transaction: rolling it back
			// is how the row lock is released.
			_ = tx.Rollback()
			<-s.slots
			unlockLocal()
		})
	}, nil
}

// settleColumns is the projection scanRecord reads, in its order.
const settleColumns = `account_id, charge_id, status, packs_done, pack_intent, pack_taken, cash_paid,
	resource_id, product_code, metering_item, bill_period, billing_cycle, quantity, unit_price,
	pretax_amount, pay_amount, shortfall, covered_ratio, snapshot_id, deductions, settled_at, version`

// GetSettle returns the record for (accountID, chargeID), if any.
func (s *SQLSettleStore) GetSettle(accountID int64, chargeID string) (SettleRecord, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT `+settleColumns+` FROM settle_charge WHERE account_id = ? AND charge_id = ?`,
		accountID, chargeID)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SettleRecord{}, false, nil
	}
	if err != nil {
		return SettleRecord{}, false, fmt.Errorf("billing: get settle %d/%s: %w", accountID, chargeID, err)
	}
	return rec, true, nil
}

// SaveSettle creates (Version 0) or updates the record under its optimistic
// lock. An update never touches a settled row: the final charge is immutable.
func (s *SQLSettleStore) SaveSettle(rec *SettleRecord) error {
	if rec.ChargeID == "" {
		return fmt.Errorf("%w: empty charge id", ErrInvalidSettleRecord)
	}
	next := *rec
	next.normalize()
	vals, err := recordValues(next)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	if rec.Version == 0 {
		args := append([]any{next.AccountID, next.ChargeID}, vals...)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO settle_charge
			   (account_id, charge_id, status, packs_done, pack_intent, pack_taken, cash_paid,
			    resource_id, product_code, metering_item, bill_period, billing_cycle, quantity,
			    unit_price, pretax_amount, pay_amount, shortfall, covered_ratio, snapshot_id,
			    deductions, settled_at, version)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
			args...); err != nil {
			// uk_acc_charge: another writer created this charge's record first.
			if storage.IsDuplicateKey(err) {
				return ErrSettleConflict
			}
			return fmt.Errorf("billing: create settle %d/%s: %w", next.AccountID, next.ChargeID, err)
		}
	} else {
		args := append(vals, next.AccountID, next.ChargeID, rec.Version)
		res, err := s.db.ExecContext(ctx,
			`UPDATE settle_charge
			    SET status = ?, packs_done = ?, pack_intent = ?, pack_taken = ?, cash_paid = ?,
			        resource_id = ?, product_code = ?, metering_item = ?, bill_period = ?,
			        billing_cycle = ?, quantity = ?, unit_price = ?, pretax_amount = ?,
			        pay_amount = ?, shortfall = ?, covered_ratio = ?, snapshot_id = ?,
			        deductions = ?, settled_at = ?, version = version + 1
			  WHERE account_id = ? AND charge_id = ? AND version = ? AND status = 1`,
			args...)
		if err := storage.Affected(res, err); err != nil {
			if errors.Is(err, storage.ErrVersionConflict) {
				return ErrSettleConflict
			}
			return fmt.Errorf("billing: update settle %d/%s: %w", next.AccountID, next.ChargeID, err)
		}
	}
	next.Version = rec.Version + 1
	*rec = next
	return nil
}

// ListCharges returns the account's settled charges, optionally for one period.
func (s *SQLSettleStore) ListCharges(accountID int64, period string) ([]Charge, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()
	q := `SELECT ` + settleColumns + ` FROM settle_charge WHERE account_id = ? AND status = 2`
	args := []any{accountID}
	if period != "" {
		q += ` AND bill_period = ?`
		args = append(args, period)
	}
	q += ` ORDER BY settled_at, charge_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("billing: list charges for account %d: %w", accountID, err)
	}
	defer rows.Close()
	out := make([]Charge, 0, 16)
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("billing: scan charge for account %d: %w", accountID, err)
		}
		out = append(out, rec.Charge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billing: list charges for account %d: %w", accountID, err)
	}
	return out, nil
}

// deductionJSON is a deduction's column form. Amounts travel as decimal
// strings: a JSON number would round a micro-yuan amount through float64.
type deductionJSON struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
	Amount string `json:"amount"`
}

// recordValues renders the mutable columns, in the INSERT/UPDATE order (status
// through settled_at).
func recordValues(r SettleRecord) ([]any, error) {
	intent, err := amountsJSON(r.PackIntent)
	if err != nil {
		return nil, err
	}
	taken, err := amountsJSON(r.PackTaken)
	if err != nil {
		return nil, err
	}
	var deductions any
	if len(r.Charge.Deductions) > 0 {
		ds := make([]deductionJSON, 0, len(r.Charge.Deductions))
		for _, d := range r.Charge.Deductions {
			ds = append(ds, deductionJSON{Source: string(d.Source), Ref: d.Ref, Amount: d.Amount.String()})
		}
		raw, err := json.Marshal(ds)
		if err != nil {
			return nil, fmt.Errorf("billing: encode deductions: %w", err)
		}
		deductions = string(raw)
	}
	status := settleStatusPlanned
	if r.Settled {
		status = settleStatusSettled
	}
	c := r.Charge
	return []any{
		status, r.PacksDone, intent, taken, r.CashPaid.String(),
		c.ResourceID, c.ProductCode, c.MeteringItem, c.BillPeriod, nullableTime(c.BillingCycle),
		c.Quantity.String(), c.UnitPrice.String(), c.PretaxAmount.String(), c.PayAmount.String(),
		c.Shortfall.String(), c.CoveredRatio, c.SnapshotID, deductions, nullableTime(c.SettledAt),
	}, nil
}

// amountsJSON renders a pack → amount map as a JSON object of decimal strings,
// or NULL when empty.
func amountsJSON(m map[string]pricing.Amount) (any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.String()
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("billing: encode pack amounts: %w", err)
	}
	return string(raw), nil
}

func parseAmountsJSON(raw sql.NullString) (map[string]pricing.Amount, error) {
	out := map[string]pricing.Amount{}
	if !raw.Valid || raw.String == "" {
		return out, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw.String), &m); err != nil {
		return nil, err
	}
	for k, v := range m {
		a, err := pricing.ParseAmount(v)
		if err != nil {
			return nil, fmt.Errorf("pack %s: %w", k, err)
		}
		out[k] = a
	}
	return out, nil
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(sc rowScanner) (SettleRecord, error) {
	var (
		rec                                   SettleRecord
		status                                int
		intent, taken, deductions             sql.NullString
		cashPaid, quantity, unitPrice, pretax string
		payAmount, shortfall                  string
		billingCycle, settledAt               sql.NullTime
	)
	c := &rec.Charge
	if err := sc.Scan(&rec.AccountID, &rec.ChargeID, &status, &rec.PacksDone, &intent, &taken, &cashPaid,
		&c.ResourceID, &c.ProductCode, &c.MeteringItem, &c.BillPeriod, &billingCycle, &quantity, &unitPrice,
		&pretax, &payAmount, &shortfall, &c.CoveredRatio, &c.SnapshotID, &deductions, &settledAt,
		&rec.Version); err != nil {
		return SettleRecord{}, err
	}
	rec.Settled = status == settleStatusSettled
	c.ChargeID = rec.ChargeID
	c.AccountID = rec.AccountID
	var err error
	if rec.PackIntent, err = parseAmountsJSON(intent); err != nil {
		return SettleRecord{}, fmt.Errorf("charge %s pack_intent: %w", rec.ChargeID, err)
	}
	if rec.PackTaken, err = parseAmountsJSON(taken); err != nil {
		return SettleRecord{}, fmt.Errorf("charge %s pack_taken: %w", rec.ChargeID, err)
	}
	for _, a := range []struct {
		column string
		raw    string
		dst    *pricing.Amount
	}{
		{"cash_paid", cashPaid, &rec.CashPaid},
		{"unit_price", unitPrice, &c.UnitPrice},
		{"pretax_amount", pretax, &c.PretaxAmount},
		{"pay_amount", payAmount, &c.PayAmount},
		{"shortfall", shortfall, &c.Shortfall},
	} {
		// DECIMAL crosses the wire as text: the strict money parser, never a
		// float, so a stored amount reads back exactly.
		if *a.dst, err = pricing.ParseAmount(a.raw); err != nil {
			return SettleRecord{}, fmt.Errorf("charge %s %s: %w", rec.ChargeID, a.column, err)
		}
	}
	if c.Quantity, err = metering.ParseQuantity(quantity); err != nil {
		return SettleRecord{}, fmt.Errorf("charge %s quantity: %w", rec.ChargeID, err)
	}
	if billingCycle.Valid {
		c.BillingCycle = billingCycle.Time.UTC()
	}
	if settledAt.Valid {
		c.SettledAt = settledAt.Time.UTC()
	}
	if deductions.Valid && deductions.String != "" {
		var ds []deductionJSON
		if err := json.Unmarshal([]byte(deductions.String), &ds); err != nil {
			return SettleRecord{}, fmt.Errorf("charge %s deductions: %w", rec.ChargeID, err)
		}
		c.Deductions = make([]Deduction, 0, len(ds))
		for _, d := range ds {
			amt, err := pricing.ParseAmount(d.Amount)
			if err != nil {
				return SettleRecord{}, fmt.Errorf("charge %s deduction %s: %w", rec.ChargeID, d.Ref, err)
			}
			c.Deductions = append(c.Deductions, Deduction{Source: DeductionSource(d.Source), Ref: d.Ref, Amount: amt})
		}
	}
	return rec, nil
}
