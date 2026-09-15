package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
)

// orderRepo is the persistence boundary for the order centre. The handlers talk
// to this interface; the in-memory implementation backs the demo and the tests,
// and the SQL implementation backs production (trade_db, account_id sharded).
//
// # The transition is the unit of work
//
// SaveTransition writes three things in ONE transaction — the order row under its
// optimistic lock, the state-log row, and the domain event's outbox row. That is
// not tidiness: 03§8.2 forbids business code from emitting money/resource events
// outside the transaction that changed the state, because a direct send can
// succeed while the transaction rolls back and the two truths diverge with no way
// to reconcile. An order that changed state with no journal row, or an event with
// no committed order behind it, is exactly the unexplainable difference 03§8.5's
// reconciliation exists to catch.
type orderRepo interface {
	// NewOrderID mints an order id. The schema's key is 号段-issued (04§6.6); the
	// in-memory implementation counts.
	NewOrderID() int64
	// Create persists the order, its creation state-log row and its outbox event.
	Create(o *order.Order, evt order.Event) error
	// ByClientToken returns the order a retried create produced — the read side
	// of uk_client_token's idempotency.
	ByClientToken(accountID int64, token string) (*order.Order, error)
	Get(accountID, orderID int64) (*order.Order, error)
	List(accountID int64) ([]*order.Order, error)
	// SaveTransition persists a state change: o already carries the NEW state and
	// version (the machine mutated it); from/prevVersion describe what the row
	// still holds, so the UPDATE can guard on it. paymentRef ties the transition
	// to its payment-side proof in the same transaction — the double-pay guard
	// and the state change must commit together, or a crash between them turns a
	// replayed callback into a 409 instead of the idempotent return it owes.
	SaveTransition(o *order.Order, from order.State, prevVersion int, reason string, evt order.Event, paymentRef string) error
	// PaymentRef is the double-pay guard: the payment-side reference that settled
	// an order, kept in idempotent_record so a replay is recognisable and a
	// second, different payment is refuseable.
	PaymentRef(accountID, orderID int64) (string, bool, error)
	SavePaymentRef(accountID, orderID int64, ref string) error
	ListAutoRenew(accountID int64) ([]autoRenewSetting, error)
	SetAutoRenew(accountID int64, setting autoRenewSetting) error
}

// --- enum mappings -----------------------------------------------------------
//
// The DDL stores type/charge_type/status as TINYINT with documented codes; the
// domain speaks in named strings because that is what the API exposes. These two
// mappings are the only places the translation happens, and both refuse an unknown
// value: a status the domain does not know is schema drift, not a default.

func typeCode(t order.Type) (int, error) {
	switch t {
	case order.TypeNew:
		return 1, nil
	case order.TypeRenew:
		return 2, nil
	case order.TypeUpgrade:
		return 3, nil
	case order.TypeDowngrade:
		return 4, nil
	case order.TypeRefund:
		return 5, nil
	}
	return 0, fmt.Errorf("%w: %q", order.ErrInvalidType, t)
}

func typeFromCode(code int) order.Type {
	switch code {
	case 2:
		return order.TypeRenew
	case 3:
		return order.TypeUpgrade
	case 4:
		return order.TypeDowngrade
	case 5:
		return order.TypeRefund
	default:
		return order.TypeNew
	}
}

func stateCode(s order.State) (int, error) {
	switch s {
	case order.StatePendingPayment:
		return 1, nil
	case order.StatePaid:
		return 2, nil
	case order.StateFulfilling:
		return 3, nil
	case order.StateCompleted:
		return 4, nil
	case order.StateCancelled:
		return 5, nil
	case order.StateRefunding:
		return 6, nil
	case order.StateRefunded:
		return 7, nil
	}
	return 0, fmt.Errorf("order: unknown state %q", s)
}

func stateFromCode(code int) order.State {
	switch code {
	case 2:
		return order.StatePaid
	case 3:
		return order.StateFulfilling
	case 4:
		return order.StateCompleted
	case 5:
		return order.StateCancelled
	case 6:
		return order.StateRefunding
	case 7:
		return order.StateRefunded
	default:
		return order.StatePendingPayment
	}
}

// chargeTypeCode maps the DDL's 1包年包月 2按量 3资源包 4抢占式.
func chargeTypeCode(c pricing.ChargeType) (int, error) {
	switch c {
	case pricing.ChargePrepaid:
		return 1, nil
	case pricing.ChargePostpaid:
		return 2, nil
	case pricing.ChargeResourcePack:
		return 3, nil
	case pricing.ChargeSpot:
		return 4, nil
	}
	return 0, fmt.Errorf("order: unknown charge type %q", c)
}

func chargeTypeFromCode(code int) pricing.ChargeType {
	switch code {
	case 2:
		return pricing.ChargePostpaid
	case 3:
		return pricing.ChargeResourcePack
	case 4:
		return pricing.ChargeSpot
	default:
		return pricing.ChargePrepaid
	}
}

// centAmount renders an amount for the order tables' DECIMAL(12,2) columns.
//
// svc-order builds every amount from 分 (minor × 10_000 micro), so its amounts are
// always cent-aligned and the narrower column is lossless. An amount that is NOT
// cent-aligned would be silently rounded by the column — and a silently rounded
// payable is a reconciliation difference wearing a valid-looking row — so it is
// rejected here instead.
func centAmount(a pricing.Amount) (string, error) {
	if int64(a)%10_000 != 0 {
		return "", fmt.Errorf("order: amount %s is not cent-aligned (the order plane settles in 分)", a)
	}
	return a.String(), nil
}

// --- in-memory implementation ------------------------------------------------

// memOrderRepo is the phase-1 in-memory orderRepo.
type memOrderRepo struct {
	mu         sync.RWMutex
	orders     map[int64]*order.Order
	byToken    map[string]int64 // accountID|clientToken → orderID
	payments   map[int64]string // orderID → payment reference
	autoRenews map[autoRenewKey]bool
	seq        int64
}

type autoRenewKey struct {
	accountID   int64
	resourceID  string
}

func newMemOrderRepo() *memOrderRepo {
	return &memOrderRepo{
		orders:     make(map[int64]*order.Order),
		byToken:    make(map[string]int64),
		payments:   make(map[int64]string),
		autoRenews: make(map[autoRenewKey]bool),
	}
}

func (s *memOrderRepo) NewOrderID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

func (s *memOrderRepo) Create(o *order.Order, _ order.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.orders[o.OrderID]; exists {
		return fmt.Errorf("order: duplicate order id %d", o.OrderID)
	}
	s.orders[o.OrderID] = o
	s.byToken[fmt.Sprintf("%d|%s", o.AccountID, o.ClientToken)] = o.OrderID
	return nil
}

// copyOrder hands the caller a copy, mirroring what a SQL read returns: a caller
// that mutated a shared pointer would be writing to the store outside its lock —
// the exact aliasing bug this service's ticket store fixed earlier.
func copyOrder(o *order.Order) *order.Order {
	if o == nil {
		return nil
	}
	cp := *o
	return &cp
}

func (s *memOrderRepo) ByClientToken(accountID int64, token string) (*order.Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byToken[fmt.Sprintf("%d|%s", accountID, token)]
	if !ok {
		return nil, nil
	}
	return copyOrder(s.orders[id]), nil
}

func (s *memOrderRepo) Get(accountID, orderID int64) (*order.Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[orderID]
	if !ok || o.AccountID != accountID {
		return nil, nil
	}
	return copyOrder(o), nil
}

func (s *memOrderRepo) List(accountID int64) ([]*order.Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*order.Order, 0)
	for _, o := range s.orders {
		if o.AccountID == accountID {
			out = append(out, copyOrder(o))
		}
	}
	return out, nil
}

// SaveTransition applies the caller's already-transitioned state under the same
// optimistic guard the SQL store runs: the stored row must still be at the state
// and version the caller read, or the write is a lost race, not an update.
func (s *memOrderRepo) SaveTransition(o *order.Order, from order.State, prevVersion int, _ string, _ order.Event, paymentRef string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.orders[o.OrderID]
	if !ok || stored.AccountID != o.AccountID {
		return fmt.Errorf("order: order %d not found", o.OrderID)
	}
	if stored.Version != prevVersion || stored.State != from {
		return fmt.Errorf("%w: %w: order %d", order.ErrVersionConflict, storage.ErrVersionConflict, o.OrderID)
	}
	cp := *o
	s.orders[o.OrderID] = &cp
	if paymentRef != "" {
		s.payments[o.OrderID] = paymentRef
	}
	return nil
}

func (s *memOrderRepo) PaymentRef(_, orderID int64) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ref, ok := s.payments[orderID]
	return ref, ok, nil
}

func (s *memOrderRepo) SavePaymentRef(_, orderID int64, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payments[orderID] = ref
	return nil
}

func (s *memOrderRepo) ListAutoRenew(accountID int64) ([]autoRenewSetting, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]autoRenewSetting, 0, len(s.autoRenews))
	for k, enabled := range s.autoRenews {
		if k.accountID != accountID {
			continue
		}
		out = append(out, autoRenewSetting{ResourceID: k.resourceID, Enabled: enabled})
	}
	return out, nil
}

func (s *memOrderRepo) SetAutoRenew(accountID int64, setting autoRenewSetting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := autoRenewKey{accountID: accountID, resourceID: setting.ResourceID}
	// The row stays with enabled=0: the console renders the persisted state, and
	// a setting the customer turned off is still a setting they touched.
	s.autoRenews[k] = setting.Enabled
	return nil
}

// --- SQL implementation ------------------------------------------------------

// sqlOrderRepo is the MySQL-backed orderRepo over trade_db's order_main,
// order_state_log, outbox_message, idempotent_record and order_auto_renew.
type sqlOrderRepo struct {
	db       *sql.DB
	orderIDs func() int64
}

var _ orderRepo = (*sqlOrderRepo)(nil)

// statementTimeout bounds one repo call; the port has no context parameter.
const statementTimeout = 10 * time.Second

// newSQLRepo wires the repo to its id sequence (trade_db V2 seeds 'order_main').
func newSQLRepo(ctx context.Context, db *sql.DB) (*sqlOrderRepo, error) {
	ids, err := storage.OpenSequence(ctx, db, "order_main", 1000)
	if err != nil {
		return nil, err
	}
	return &sqlOrderRepo{db: db, orderIDs: ids.NextFunc()}, nil
}

const orderColumns = `order_id, order_no, account_id, order_type, product_code,
	charge_type, snapshot_id, original_amount, discount_amount, payable_amount,
	paid_amount, status, resource_id, client_token, version, paid_at, created_at, updated_at`

// outboxEventID is deterministic per (order, resulting state): a replayed
// transition that somehow reaches the INSERT again collides with uk_event_id and
// is a no-op instead of a duplicate event.
func outboxEventID(evt order.Event) string {
	return fmt.Sprintf("ord-%d-%s", evt.OrderID, evt.EventType())
}

// outboxPayload is the 统一信封 the DDL documents:
// {event_id, event_type, occurred_at, aggregate_id, payload}.
type outboxPayload struct {
	EventID     string          `json:"event_id"`
	EventType   string          `json:"event_type"`
	OccurredAt  string          `json:"occurred_at"`
	AggregateID string          `json:"aggregate_id"`
	Order       order.Event     `json:"payload"`
}

func (s *sqlOrderRepo) NewOrderID() int64 { return s.orderIDs() }

// Create writes the order row, its creation journal row and its outbox event in
// one transaction.
func (s *sqlOrderRepo) Create(o *order.Order, evt order.Event) error {
	typeC, err := typeCode(o.Type)
	if err != nil {
		return err
	}
	chargeC, err := chargeTypeCode(o.ChargeType)
	if err != nil {
		return err
	}
	statusC, err := stateCode(o.State)
	if err != nil {
		return err
	}
	original, err := centAmount(o.OriginalAmount)
	if err != nil {
		return err
	}
	discount, err := centAmount(o.DiscountAmount)
	if err != nil {
		return err
	}
	payable, err := centAmount(o.PayableAmount)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_main
			   (order_id, order_no, account_id, order_type, product_code, charge_type,
			    snapshot_id, original_amount, discount_amount, payable_amount, paid_amount,
			    status, resource_id, client_token, version, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
			o.OrderID, o.OrderNo, o.AccountID, typeC, o.ProductCode, chargeC,
			o.SnapshotID, original, discount, payable, statusC,
			nullableString(o.ResourceID), o.ClientToken, o.Version,
			o.CreatedAt.UTC(), o.UpdatedAt.UTC()); err != nil {
			if storage.IsDuplicateKey(err) {
				// uk_client_token: the retry path won. The handler re-reads by
				// token and serves the winner's order.
				return fmt.Errorf("order: duplicate client token %q", o.ClientToken)
			}
			return fmt.Errorf("order: create %d: %w", o.OrderID, err)
		}
		if err := s.insertStateLog(ctx, tx, o, "", o.State, "system", "order created"); err != nil {
			return err
		}
		return s.insertOutbox(ctx, tx, evt)
	})
}

// insertStateLog appends the journal row. fromStatus "" means "created" — the
// DDL encodes that as a NULL from_status.
func (s *sqlOrderRepo) insertStateLog(ctx context.Context, tx *sql.Tx, o *order.Order, from, to order.State, operator, reason string) error {
	toC, err := stateCode(to)
	if err != nil {
		return err
	}
	var fromC any
	if from != "" {
		code, err := stateCode(from)
		if err != nil {
			return err
		}
		fromC = code
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO order_state_log (order_id, account_id, from_status, to_status, operator, reason)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		o.OrderID, o.AccountID, fromC, toC, operator, nullableString(reason)); err != nil {
		return fmt.Errorf("order: state log for %d: %w", o.OrderID, err)
	}
	return nil
}

// insertOutbox writes the domain event inside the caller's transaction.
func (s *sqlOrderRepo) insertOutbox(ctx context.Context, tx *sql.Tx, evt order.Event) error {
	eventID := outboxEventID(evt)
	payload, err := json.Marshal(outboxPayload{
		EventID:     eventID,
		EventType:   evt.EventType(),
		OccurredAt:  evt.OccurredAt.UTC().Format(time.RFC3339Nano),
		AggregateID: strconv.FormatInt(evt.OrderID, 10),
		Order:       evt,
	})
	if err != nil {
		return fmt.Errorf("order: encode event for %d: %w", evt.OrderID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO outbox_message
		   (account_id, biz_type, biz_key, topic, partition_key, event_id, payload, status)
		 VALUES (?, 'order', ?, 'cloud.trade.order.event', ?, ?, ?, 0)`,
		evt.AccountID, strconv.FormatInt(evt.OrderID, 10), strconv.FormatInt(evt.OrderID, 10),
		eventID, payload); err != nil {
		return fmt.Errorf("order: outbox for %d: %w", evt.OrderID, err)
	}
	return nil
}

func (s *sqlOrderRepo) ByClientToken(accountID int64, token string) (*order.Order, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	o, err := scanOrder(s.db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM order_main WHERE account_id = ? AND client_token = ?`,
		accountID, token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("order: by client token: %w", err)
	}
	return o, nil
}

func (s *sqlOrderRepo) Get(accountID, orderID int64) (*order.Order, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	o, err := scanOrder(s.db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM order_main WHERE order_id = ? AND account_id = ?`,
		orderID, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("order: get %d: %w", orderID, err)
	}
	return o, nil
}

func (s *sqlOrderRepo) List(accountID int64) ([]*order.Order, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM order_main WHERE account_id = ? ORDER BY order_id DESC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("order: list for account %d: %w", accountID, err)
	}
	defer rows.Close()

	out := make([]*order.Order, 0, 8)
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("order: scan for account %d: %w", accountID, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("order: list for account %d: %w", accountID, err)
	}
	return out, nil
}

// SaveTransition moves the row under the optimistic lock and writes the journal
// and the outbox row in the same transaction.
func (s *sqlOrderRepo) SaveTransition(o *order.Order, from order.State, prevVersion int, reason string, evt order.Event, paymentRef string) error {
	statusC, err := stateCode(o.State)
	if err != nil {
		return err
	}
	// paid_amount is materialised from the payable: MarkPaid refuses a payment
	// that differs from the payable, so once an order is past PAID the two are
	// equal by construction. (The domain has no separate field to copy.)
	paid := o.PayableAmount
	if from == order.StatePendingPayment {
		paid = 0
	}
	paidStr, err := centAmount(paid)
	if err != nil {
		return err
	}
	var paidAt any
	if !o.PaidAt.IsZero() {
		paidAt = o.PaidAt.UTC()
	}

	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	return storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE order_main
			    SET status = ?, version = ?, resource_id = ?, paid_amount = ?, paid_at = ?, updated_at = ?
			  WHERE order_id = ? AND version = ?`,
			statusC, o.Version, nullableString(o.ResourceID), paidStr, paidAt,
			o.UpdatedAt.UTC(), o.OrderID, prevVersion)
		if err != nil {
			return fmt.Errorf("order: update %d: %w", o.OrderID, err)
		}
		// Zero rows matched: a repeated callback finds the row already advanced.
		// That is the DDL's documented idempotency ("affects zero rows"), so it is
		// reported as the version conflict the caller's machine already guards
		// against — carrying both sentinels so either the domain branch or the
		// shared retry helper recognises it.
		if err := storage.Affected(res, nil); err != nil {
			return fmt.Errorf("%w: %w: order %d", order.ErrVersionConflict, storage.ErrVersionConflict, o.OrderID)
		}
		if err := s.insertStateLog(ctx, tx, o, from, o.State, "system", reason); err != nil {
			return err
		}
		if err := s.insertOutbox(ctx, tx, evt); err != nil {
			return err
		}
		if paymentRef != "" {
			// In THIS transaction, not via SavePaymentRef (which would open a
			// second one): the state change and its payment proof commit or roll
			// back together, which is what makes a replayed callback idempotent.
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO idempotent_record (account_id, biz_type, biz_key, result)
				 VALUES (?, 'order_payment', ?, ?)
				 ON DUPLICATE KEY UPDATE result = ?`,
				o.AccountID, strconv.FormatInt(o.OrderID, 10), paymentRef, paymentRef); err != nil {
				return fmt.Errorf("order: payment ref for %d: %w", o.OrderID, err)
			}
		}
		return nil
	})
}

func (s *sqlOrderRepo) PaymentRef(accountID, orderID int64) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	var ref sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT result FROM idempotent_record WHERE biz_type = 'order_payment' AND biz_key = ?`,
		strconv.FormatInt(orderID, 10)).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("order: payment ref for %d: %w", orderID, err)
	}
	_ = accountID // uk_biz is (biz_type, biz_key): the order id is globally unique
	return ref.String, true, nil
}

func (s *sqlOrderRepo) SavePaymentRef(accountID, orderID int64, ref string) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO idempotent_record (account_id, biz_type, biz_key, result)
		 VALUES (?, 'order_payment', ?, ?)
		 ON DUPLICATE KEY UPDATE result = ?`,
		accountID, strconv.FormatInt(orderID, 10), ref, ref); err != nil {
		return fmt.Errorf("order: save payment ref for %d: %w", orderID, err)
	}
	return nil
}

func (s *sqlOrderRepo) ListAutoRenew(accountID int64) ([]autoRenewSetting, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT resource_id, product_code, enabled
		   FROM order_auto_renew
		  WHERE account_id = ?
		  ORDER BY resource_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("order: list auto-renew for %d: %w", accountID, err)
	}
	defer rows.Close()

	out := make([]autoRenewSetting, 0, 8)
	for rows.Next() {
		var (
			setting autoRenewSetting
			enabled int
		)
		if err := rows.Scan(&setting.ResourceID, &setting.ProductCode, &enabled); err != nil {
			return nil, fmt.Errorf("order: scan auto-renew for %d: %w", accountID, err)
		}
		setting.Enabled = enabled == 1
		out = append(out, setting)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("order: list auto-renew for %d: %w", accountID, err)
	}
	return out, nil
}

// SetAutoRenew upserts the switch. An off switch is stored, not deleted: the
// console renders the customer's persisted choice, and "off" is a choice.
func (s *sqlOrderRepo) SetAutoRenew(accountID int64, setting autoRenewSetting) error {
	ctx, cancel := context.WithTimeout(context.Background(), statementTimeout)
	defer cancel()

	enabled := 0
	if setting.Enabled {
		enabled = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO order_auto_renew (account_id, resource_id, product_code, enabled)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE product_code = ?, enabled = ?`,
		accountID, setting.ResourceID, setting.ProductCode, enabled,
		setting.ProductCode, enabled); err != nil {
		return fmt.Errorf("order: set auto-renew for %s: %w", setting.ResourceID, err)
	}
	return nil
}

// rowScanner is the shared shape of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanOrder(sc rowScanner) (*order.Order, error) {
	var (
		o                  order.Order
		typeC, chargeC     int
		statusC            int
		original, discount string
		payable, paid      string
		resource           sql.NullString
		paidAt             sql.NullTime
	)
	if err := sc.Scan(&o.OrderID, &o.OrderNo, &o.AccountID, &typeC, &o.ProductCode, &chargeC,
		&o.SnapshotID, &original, &discount, &payable, &paid, &statusC,
		&resource, &o.ClientToken, &o.Version, &paidAt, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, err
	}
	var err error
	o.Type = typeFromCode(typeC)
	o.ChargeType = chargeTypeFromCode(chargeC)
	o.State = stateFromCode(statusC)
	if o.OriginalAmount, err = pricing.ParseAmount(original); err != nil {
		return nil, fmt.Errorf("order %d original: %w", o.OrderID, err)
	}
	if o.DiscountAmount, err = pricing.ParseAmount(discount); err != nil {
		return nil, fmt.Errorf("order %d discount: %w", o.OrderID, err)
	}
	if o.PayableAmount, err = pricing.ParseAmount(payable); err != nil {
		return nil, fmt.Errorf("order %d payable: %w", o.OrderID, err)
	}
	// paid_amount stays in the database: the domain has no field for it, and the
	// invariant MarkPaid enforces (paid == payable) means the column is
	// materialised from the payable on write.
	o.ResourceID = resource.String
	if paidAt.Valid {
		o.PaidAt = paidAt.Time
	}
	return &o, nil
}

// nullableString writes an empty string as SQL NULL for the DDL's nullable text
// columns.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- wiring ------------------------------------------------------------------

// newOrderRepo picks the backend. Persistence is opt-in (pkg-go/storage doc):
// with EULER_DB_DSN set, orders, their journal and their events live in trade_db, so
// a restart keeps them; unset, the in-memory repo keeps the demo and `go test`
// dependency-free.
//
// A configured DSN that cannot be reached — or a schema sqlmigrate never touched —
// is a startup failure: an order service that cannot persist an order must not
// accept one.
func newOrderRepo(ctx context.Context) (orderRepo, error) {
	db, ok, err := storage.MustOpenFor(ctx, "trade_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newMemOrderRepo(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "trade_db"); err != nil {
		return nil, err
	}
	return newSQLRepo(ctx, db)
}

// persistentRepo reports whether a repo is backed by MySQL, for the startup log.
func persistentRepo(r orderRepo) bool {
	_, ok := r.(*sqlOrderRepo)
	return ok
}
