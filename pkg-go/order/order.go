// Package order implements the unified order model and its state machine
// (03-backend-services.md §4.2.2, 01-product-catalog.md §5.2).
//
// The order centre is the hub of commercialisation complexity (对标启示 9):
// one order model covers all five transaction types, and product lines are
// forbidden from building their own. A per-product order table is how a
// platform ends up unable to answer "what did this customer buy last quarter".
//
// # Five transaction types
//
//	NEW       新购  create a new resource
//	RENEW     续费  extend a prepaid resource's expiry
//	UPGRADE   升配  raise the spec, charging the prorated difference
//	DOWNGRADE 降配  lower the spec, refunding the difference to cash balance
//	REFUND    退订  release the resource and refund the unused remainder
//
// # Two rules that prevent financial loss
//
//  1. paid → fulfilling is the ONLY transition that triggers resource
//     creation (03§4.2.2). Nothing else may provision, so a resource can never
//     exist without a paid order behind it.
//
//  2. Refunds run AFTER the resource is released, never before
//     (03§8.4: 退订 = 释放资源流程成功后再触发退款流程, 钱货两清有先后).
//     Refunding first leaves a window where the customer has both their money
//     and a running resource.
//
// # Idempotency
//
// Every write carries a ClientToken (UUID, 10-minute window). The gateway
// screens duplicates with Redis SETNX; the database's unique index on
// (account_id, client_token) is the final arbiter (03§8.3). State transitions
// additionally guard on the expected prior state plus a version, so a repeated
// callback is a no-op rather than a double-charge.
package order

import (
	"errors"
	"fmt"
	"time"

	"github.com/starcloud/sc-platform/pricing"
)

// Type is the transaction type. One model, five types (对标启示 9).
type Type string

const (
	TypeNew       Type = "NEW"       // 新购
	TypeRenew     Type = "RENEW"     // 续费
	TypeUpgrade   Type = "UPGRADE"   // 升配
	TypeDowngrade Type = "DOWNGRADE" // 降配
	TypeRefund    Type = "REFUND"    // 退订
)

// Valid reports whether t is a known transaction type.
func (t Type) Valid() bool {
	switch t {
	case TypeNew, TypeRenew, TypeUpgrade, TypeDowngrade, TypeRefund:
		return true
	}
	return false
}

// RequiresExistingResource reports whether this type operates on a resource
// that must already exist. Only NEW creates one.
func (t Type) RequiresExistingResource() bool {
	return t != TypeNew
}

// State is the order state (03§6.3 order_main.status).
type State string

const (
	StatePendingPayment State = "PENDING_PAYMENT" // 1 待支付
	StatePaid           State = "PAID"            // 2 已支付
	StateFulfilling     State = "FULFILLING"      // 3 履约中
	StateCompleted      State = "COMPLETED"       // 4 已完成
	StateCancelled      State = "CANCELLED"       // 5 已取消
	StateRefunding      State = "REFUNDING"       // 6 退款中
	StateRefunded       State = "REFUNDED"        // 7 已退款
)

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool {
	return s == StateCompleted || s == StateCancelled || s == StateRefunded
}

// transitions is the complete legal transition table. Anything absent here is
// rejected — an unlisted transition is a bug, not an edge case, and silently
// allowing it is how orders end up in states no downstream consumer expects.
var transitions = map[State][]State{
	StatePendingPayment: {StatePaid, StateCancelled},
	// paid → fulfilling is the ONLY provisioning trigger.
	StatePaid: {StateFulfilling, StateRefunding},
	// Fulfilment failure goes to refunding, not cancelled: the customer has
	// already paid, so the money must come back rather than the order simply
	// disappearing.
	StateFulfilling: {StateCompleted, StateRefunding},
	StateCompleted:  {StateRefunding}, // 退订 of a completed order
	StateRefunding:  {StateRefunded},
	StateCancelled:  {}, // terminal
	StateRefunded:   {}, // terminal
}

// CanTransition reports whether from → to is legal.
func CanTransition(from, to State) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Errors.
var (
	ErrInvalidTransition   = errors.New("order: illegal state transition")
	ErrVersionConflict     = errors.New("order: version conflict (concurrent update)")
	ErrInvalidType         = errors.New("order: unknown transaction type")
	ErrMissingClientToken  = errors.New("order: ClientToken required for write operations")
	ErrMissingResource     = errors.New("order: this order type requires a target resource")
	ErrResourceNotReleased = errors.New("order: cannot refund before the resource is released")
	ErrSnapshotExpired     = errors.New("order: price snapshot expired, re-quote required")
	ErrAmountMismatch      = errors.New("order: payment amount does not match the order")
)

// Order is the aggregate root (03§6.3 order_main).
type Order struct {
	OrderID   int64
	OrderNo   string // customer-facing number
	AccountID int64  // shard key
	Type      Type
	State     State

	ProductCode string
	ChargeType  pricing.ChargeType

	// SnapshotID references the frozen price. Every downstream amount comes
	// from the snapshot, never from the live catalogue — that is what makes a
	// historical bill explicable after prices move (01§12.3).
	SnapshotID     string
	OriginalAmount pricing.Amount
	DiscountAmount pricing.Amount
	PayableAmount  pricing.Amount

	// ResourceID is empty for NEW until fulfilment reports back; for all other
	// types it identifies the resource being acted on.
	ResourceID string

	ClientToken string // idempotency key, unique per (account_id, client_token)
	Version     int    // optimistic lock

	CreatedAt time.Time
	PaidAt    time.Time
	UpdatedAt time.Time
}

// Item is an order line (03§6.3 order_item).
type Item struct {
	ID           int64
	OrderID      int64
	AccountID    int64 // redundant shard key, avoids a cross-shard join
	ResourceID   string
	SKUCode      string
	Quantity     int64
	Duration     int64
	DurationUnit pricing.DurationUnit
	// PriceSnapshot is the serialized quote for this line — the reconciliation
	// basis (03§6.3 price_snapshot JSON column).
	PriceSnapshotID string
	ItemAmount      pricing.Amount
}

// Event is emitted on every state transition, written to the outbox in the
// same transaction as the order row (03§8.2). Topic cloud.trade.order.event,
// partitioned by order_id (04§5.4).
type Event struct {
	OrderID     int64
	AccountID   int64
	OrderNo     string
	Type        Type
	FromState   State
	ToState     State
	ProductCode string
	ChargeType  pricing.ChargeType
	ResourceID  string
	Amount      pricing.Amount
	OccurredAt  time.Time
}

// EventType renders the domain event name for the envelope, e.g.
// "cloud.trade.order.paid".
func (e Event) EventType() string {
	switch e.ToState {
	case StatePaid:
		return "cloud.trade.order.paid"
	case StateFulfilling:
		return "cloud.trade.order.fulfilling"
	case StateCompleted:
		return "cloud.trade.order.completed"
	case StateCancelled:
		return "cloud.trade.order.cancelled"
	case StateRefunding:
		return "cloud.trade.order.refunding"
	case StateRefunded:
		return "cloud.trade.order.refunded"
	default:
		return "cloud.trade.order.created"
	}
}

// TriggersProvisioning reports whether this event is the one that starts
// resource creation. Exactly one transition does: paid → fulfilling.
// svc-orchestrator consumes cloud.trade.order.event and acts only on this.
func (e Event) TriggersProvisioning() bool {
	return e.FromState == StatePaid && e.ToState == StateFulfilling
}

// CreateRequest is the input to placing an order.
type CreateRequest struct {
	AccountID   int64
	Type        Type
	ProductCode string
	ChargeType  pricing.ChargeType
	SKUCode     string
	RegionID    string
	Quantity    int64
	Duration    int64
	DurationUnit pricing.DurationUnit

	// ResourceID must be set for every type except NEW.
	ResourceID string

	// Quote is the frozen price from svc-catalog.
	Quote      pricing.Result
	SnapshotID string
	// SnapshotExpiresAt guards against ordering at a stale quote.
	SnapshotExpiresAt time.Time

	ClientToken string
	At          time.Time
}

// Validate checks the request before any state is created. Rejecting here
// keeps invalid orders out of the database entirely rather than relying on
// downstream steps to notice.
func (r CreateRequest) Validate() error {
	if !r.Type.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidType, r.Type)
	}
	if r.ClientToken == "" {
		return ErrMissingClientToken
	}
	if r.Type.RequiresExistingResource() && r.ResourceID == "" {
		return fmt.Errorf("%w: %s requires ResourceID", ErrMissingResource, r.Type)
	}
	if !r.ChargeType.Sellable() {
		return fmt.Errorf("%w: %s", pricing.ErrChargeTypeUnsold, r.ChargeType)
	}
	if !r.SnapshotExpiresAt.IsZero() && !r.At.Before(r.SnapshotExpiresAt) {
		return ErrSnapshotExpired
	}
	if r.Quote.PayableAmount.IsNegative() {
		return fmt.Errorf("order: payable amount is negative")
	}
	return nil
}

// Machine applies transitions to orders. It holds no state itself; the caller
// supplies the order and persists the result, which keeps the transition rules
// testable without a database.
type Machine struct {
	Now func() time.Time
}

// NewMachine builds a Machine. now is injectable so tests control time.
func NewMachine(now func() time.Time) *Machine {
	if now == nil {
		now = time.Now
	}
	return &Machine{Now: now}
}

// Create builds a new order in PENDING_PAYMENT.
//
// A zero-payable order (fully covered by a trial voucher) still passes through
// PENDING_PAYMENT rather than jumping to PAID: the payment service records a
// zero-amount payment so the ledger has an entry for every order, and the
// trial flow exercises exactly the same path as a paid one (decision D7).
func (m *Machine) Create(req CreateRequest, orderID int64, orderNo string) (*Order, Event, error) {
	if err := req.Validate(); err != nil {
		return nil, Event{}, err
	}
	now := req.At
	if now.IsZero() {
		now = m.Now()
	}

	o := &Order{
		OrderID:        orderID,
		OrderNo:        orderNo,
		AccountID:      req.AccountID,
		Type:           req.Type,
		State:          StatePendingPayment,
		ProductCode:    req.ProductCode,
		ChargeType:     req.ChargeType,
		SnapshotID:     req.SnapshotID,
		OriginalAmount: req.Quote.ListAmount,
		DiscountAmount: req.Quote.PromoAmount.Add(req.Quote.CouponAmount),
		PayableAmount:  req.Quote.PayableAmount,
		ResourceID:     req.ResourceID,
		ClientToken:    req.ClientToken,
		Version:        0,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	evt := Event{
		OrderID:     o.OrderID,
		AccountID:   o.AccountID,
		OrderNo:     o.OrderNo,
		Type:        o.Type,
		FromState:   "",
		ToState:     StatePendingPayment,
		ProductCode: o.ProductCode,
		ChargeType:  o.ChargeType,
		ResourceID:  o.ResourceID,
		Amount:      o.PayableAmount,
		OccurredAt:  now,
	}
	return o, evt, nil
}

// Transition moves the order to a new state, enforcing the transition table
// and the optimistic lock.
//
// expectedVersion mirrors the SQL guard
// `WHERE status = ? AND version = ?`: a repeated callback finds the row
// already advanced, affects zero rows, and is therefore idempotent rather than
// a double-apply (03§8.3).
func (m *Machine) Transition(o *Order, to State, expectedVersion int) (Event, error) {
	if o.Version != expectedVersion {
		return Event{}, fmt.Errorf("%w: order %d at version %d, caller expected %d",
			ErrVersionConflict, o.OrderID, o.Version, expectedVersion)
	}
	if !CanTransition(o.State, to) {
		return Event{}, fmt.Errorf("%w: %s → %s (order %d)",
			ErrInvalidTransition, o.State, to, o.OrderID)
	}

	from := o.State
	now := m.Now()
	o.State = to
	o.Version++
	o.UpdatedAt = now
	if to == StatePaid {
		o.PaidAt = now
	}

	return Event{
		OrderID:     o.OrderID,
		AccountID:   o.AccountID,
		OrderNo:     o.OrderNo,
		Type:        o.Type,
		FromState:   from,
		ToState:     to,
		ProductCode: o.ProductCode,
		ChargeType:  o.ChargeType,
		ResourceID:  o.ResourceID,
		Amount:      o.PayableAmount,
		OccurredAt:  now,
	}, nil
}

// MarkPaid records payment and moves to PAID. The amount is checked against
// the order: a channel callback reporting a different figure than the order
// expects is a reconciliation problem, not something to accept quietly.
func (m *Machine) MarkPaid(o *Order, paidAmount pricing.Amount, expectedVersion int) (Event, error) {
	if paidAmount != o.PayableAmount {
		return Event{}, fmt.Errorf("%w: order %d expects %s, payment reports %s",
			ErrAmountMismatch, o.OrderID, o.PayableAmount, paidAmount)
	}
	return m.Transition(o, StatePaid, expectedVersion)
}

// StartFulfilment moves PAID → FULFILLING, the single transition that triggers
// resource provisioning. svc-orchestrator consumes the resulting event.
func (m *Machine) StartFulfilment(o *Order, expectedVersion int) (Event, error) {
	return m.Transition(o, StateFulfilling, expectedVersion)
}

// CompleteFulfilment records that the resource is running. resourceID is
// captured here because for a NEW order the id does not exist until the
// controller reports back.
func (m *Machine) CompleteFulfilment(o *Order, resourceID string, expectedVersion int) (Event, error) {
	evt, err := m.Transition(o, StateCompleted, expectedVersion)
	if err != nil {
		return Event{}, err
	}
	if resourceID != "" {
		o.ResourceID = resourceID
		evt.ResourceID = resourceID
	}
	return evt, nil
}

// StartRefund moves the order to REFUNDING.
//
// resourceReleased encodes the ordering rule from 03§8.4: for an order that
// reached COMPLETED, the resource must be released before the refund begins.
// A refund issued while the resource still runs leaves the customer holding
// both the money and the service.
//
// Orders that never reached fulfilment (still PAID, or failed mid-FULFILLING)
// have nothing to release, so the check does not apply to them.
func (m *Machine) StartRefund(o *Order, resourceReleased bool, expectedVersion int) (Event, error) {
	if o.State == StateCompleted && !resourceReleased {
		return Event{}, fmt.Errorf("%w: order %d resource %s still active",
			ErrResourceNotReleased, o.OrderID, o.ResourceID)
	}
	return m.Transition(o, StateRefunding, expectedVersion)
}

// CompleteRefund finalises the refund.
func (m *Machine) CompleteRefund(o *Order, expectedVersion int) (Event, error) {
	return m.Transition(o, StateRefunded, expectedVersion)
}

// Cancel abandons an unpaid order.
func (m *Machine) Cancel(o *Order, expectedVersion int) (Event, error) {
	return m.Transition(o, StateCancelled, expectedVersion)
}

// ProrationBasis carries the inputs for computing a refund or upgrade
// difference. All amounts derive from the frozen price snapshot, never the
// current catalogue price (01§12.3 rule 4).
type ProrationBasis struct {
	// SnapshotAmount is the amount originally paid, from the price snapshot.
	SnapshotAmount pricing.Amount
	// TotalPeriods is the purchased duration in whole periods.
	TotalPeriods int64
	// UsedPeriods is how many periods have elapsed, rounded up: a partially
	// used period counts as consumed, which is the conventional cloud-vendor
	// rule and avoids refunding time the customer actually had.
	UsedPeriods int64
}

// RefundAmount computes the unused remainder.
//
// The proration ratio is applied directly as remaining/total rather than being
// converted to a percentage first. Converting 11/12 to basis points truncates
// it to 9166 (from 9166.67), which under-refunds a 1200 subscription by 8 分 —
// small per order, but always in the platform's favour and repeated on every
// refund. See pricing.Amount.MulDiv.
//
// Returns zero rather than a negative amount when usage meets or exceeds the
// purchased term — a fully consumed subscription refunds nothing, and a
// negative refund would be a charge disguised as one.
func (b ProrationBasis) RefundAmount() pricing.Amount {
	if b.TotalPeriods <= 0 || b.UsedPeriods >= b.TotalPeriods {
		return 0
	}
	remaining := b.TotalPeriods - b.UsedPeriods
	return b.SnapshotAmount.MulDiv(remaining, b.TotalPeriods)
}
