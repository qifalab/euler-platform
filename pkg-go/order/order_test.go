package order

import (
	"errors"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/pricing"
)

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func newMachine() *Machine {
	return NewMachine(func() time.Time { return testNow })
}

func validQuote() pricing.Result {
	return pricing.Result{
		ListAmount:    pricing.MustParseAmount("180"),
		PromoAmount:   pricing.MustParseAmount("27"),
		CouponAmount:  0,
		PayableAmount: pricing.MustParseAmount("153"),
	}
}

func validCreateReq() CreateRequest {
	return CreateRequest{
		AccountID:    100123,
		Type:         TypeNew,
		ProductCode:  "euecs",
		ChargeType:   pricing.ChargePrepaid,
		SKUCode:      "euecs.s2.large.prepaid",
		RegionID:     "cn-north-1",
		Quantity:     1,
		Duration:     1,
		DurationUnit: pricing.DurationMonth,
		Quote:        validQuote(),
		SnapshotID:   "snap-001",
		ClientToken:  "token-abc-123",
		At:           testNow,
	}
}

func mustCreate(t *testing.T, m *Machine, req CreateRequest) *Order {
	t.Helper()
	o, _, err := m.Create(req, 9001, "SO202608080001")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return o
}

// --- Creation & validation ---

func TestCreateStartsPendingPayment(t *testing.T) {
	m := newMachine()
	o, evt, err := m.Create(validCreateReq(), 9001, "SO202608080001")
	if err != nil {
		t.Fatal(err)
	}
	if o.State != StatePendingPayment {
		t.Fatalf("new order state = %s, want PENDING_PAYMENT", o.State)
	}
	if o.PayableAmount.String() != "153" {
		t.Fatalf("payable = %s, want 153", o.PayableAmount)
	}
	// Discount is the sum of promotion and voucher.
	if o.DiscountAmount.String() != "27" {
		t.Fatalf("discount = %s, want 27", o.DiscountAmount)
	}
	if evt.ToState != StatePendingPayment {
		t.Fatalf("creation event state = %s", evt.ToState)
	}
}

func TestCreateRequiresClientToken(t *testing.T) {
	m := newMachine()
	req := validCreateReq()
	req.ClientToken = ""
	if _, _, err := m.Create(req, 1, "x"); !errors.Is(err, ErrMissingClientToken) {
		t.Fatalf("expected ErrMissingClientToken, got %v", err)
	}
}

func TestCreateRejectsUnknownType(t *testing.T) {
	m := newMachine()
	req := validCreateReq()
	req.Type = "TRANSFER"
	if _, _, err := m.Create(req, 1, "x"); !errors.Is(err, ErrInvalidType) {
		t.Fatalf("expected ErrInvalidType, got %v", err)
	}
}

func TestNonNewTypesRequireResourceID(t *testing.T) {
	m := newMachine()
	for _, typ := range []Type{TypeRenew, TypeUpgrade, TypeDowngrade, TypeRefund} {
		req := validCreateReq()
		req.Type = typ
		req.ResourceID = ""
		if _, _, err := m.Create(req, 1, "x"); !errors.Is(err, ErrMissingResource) {
			t.Errorf("%s without ResourceID: expected ErrMissingResource, got %v", typ, err)
		}
		// With a resource id it succeeds.
		req.ResourceID = "euecs-cn-north-1-01-a1b2c3d4"
		if _, _, err := m.Create(req, 1, "x"); err != nil {
			t.Errorf("%s with ResourceID should succeed: %v", typ, err)
		}
	}
	// NEW does not need one.
	req := validCreateReq()
	req.ResourceID = ""
	if _, _, err := m.Create(req, 1, "x"); err != nil {
		t.Errorf("NEW without ResourceID should succeed: %v", err)
	}
}

func TestCreateAcceptsResourcePackChargeType(t *testing.T) {
	// Phase 2 (09-roadmap M-4.1) opens 资源包 for sale. A resource-pack order
	// is TypeNew + ChargeResourcePack: it buys quota, not a resource instance,
	// so it must NOT require a ResourceID and must pass the sellable gate.
	m := newMachine()
	req := validCreateReq()
	req.ChargeType = pricing.ChargeResourcePack
	req.ResourceID = ""
	if _, _, err := m.Create(req, 1, "x"); err != nil {
		t.Fatalf("resource-pack order must be accepted in phase 2, got %v", err)
	}
}

func TestCreateRejectsExpiredSnapshot(t *testing.T) {
	// A stale quote must not become an order: prices move, and honouring an
	// expired quote is an unbounded liability.
	m := newMachine()
	req := validCreateReq()
	req.SnapshotExpiresAt = testNow.Add(-time.Minute)
	if _, _, err := m.Create(req, 1, "x"); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("expected ErrSnapshotExpired, got %v", err)
	}
}

// --- State machine ---

func TestHappyPathNewOrder(t *testing.T) {
	m := newMachine()
	o := mustCreate(t, m, validCreateReq())

	paidEvt, err := m.MarkPaid(o, pricing.MustParseAmount("153"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if o.State != StatePaid || paidEvt.EventType() != "cloud.trade.order.paid" {
		t.Fatalf("after payment: state=%s event=%s", o.State, paidEvt.EventType())
	}
	if o.PaidAt.IsZero() {
		t.Fatal("PaidAt not stamped")
	}

	fulfilEvt, err := m.StartFulfilment(o, 1)
	if err != nil {
		t.Fatal(err)
	}
	// This is the one event that starts provisioning.
	if !fulfilEvt.TriggersProvisioning() {
		t.Fatal("paid → fulfilling must trigger provisioning")
	}

	doneEvt, err := m.CompleteFulfilment(o, "euecs-cn-north-1-01-a1b2c3d4", 2)
	if err != nil {
		t.Fatal(err)
	}
	if o.State != StateCompleted {
		t.Fatalf("final state = %s, want COMPLETED", o.State)
	}
	if o.ResourceID != "euecs-cn-north-1-01-a1b2c3d4" {
		t.Fatalf("resource id not captured: %q", o.ResourceID)
	}
	if doneEvt.ResourceID != o.ResourceID {
		t.Fatal("completion event missing resource id")
	}
}

// TestOnlyPaidToFulfillingTriggersProvisioning is the guard on the rule that
// nothing but a paid order can create a resource (03§4.2.2).
func TestOnlyPaidToFulfillingTriggersProvisioning(t *testing.T) {
	m := newMachine()
	cases := []struct{ from, to State }{
		{StatePendingPayment, StatePaid},
		{StatePendingPayment, StateCancelled},
		{StateFulfilling, StateCompleted},
		{StateCompleted, StateRefunding},
		{StateRefunding, StateRefunded},
		{StatePaid, StateRefunding},
	}
	for _, c := range cases {
		evt := Event{FromState: c.from, ToState: c.to}
		if evt.TriggersProvisioning() {
			t.Errorf("%s → %s must NOT trigger provisioning", c.from, c.to)
		}
	}
	if !(Event{FromState: StatePaid, ToState: StateFulfilling}).TriggersProvisioning() {
		t.Error("paid → fulfilling must trigger provisioning")
	}
	_ = m
}

func TestIllegalTransitionsRejected(t *testing.T) {
	m := newMachine()
	illegal := []struct {
		from, to State
		why      string
	}{
		{StatePendingPayment, StateFulfilling, "cannot provision without payment"},
		{StatePendingPayment, StateCompleted, "cannot complete without payment"},
		{StatePaid, StateCompleted, "cannot complete without fulfilment"},
		{StateCompleted, StateCancelled, "a completed order is cancelled by refunding, not cancelling"},
		{StateCancelled, StatePaid, "terminal state"},
		{StateRefunded, StateRefunding, "terminal state"},
		{StateRefunded, StatePaid, "terminal state"},
		{StateCancelled, StateFulfilling, "terminal state"},
	}
	for _, c := range illegal {
		o := &Order{OrderID: 1, State: c.from, Version: 0}
		if _, err := m.Transition(o, c.to, 0); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s → %s (%s): expected ErrInvalidTransition, got %v",
				c.from, c.to, c.why, err)
		}
		// The order must be untouched by a rejected transition.
		if o.State != c.from || o.Version != 0 {
			t.Errorf("%s → %s: rejected transition mutated the order (now %s v%d)",
				c.from, c.to, o.State, o.Version)
		}
	}
}

func TestFulfilmentFailureGoesToRefundingNotCancelled(t *testing.T) {
	// The customer has already paid, so a failed provision must return the
	// money rather than silently cancelling.
	m := newMachine()
	o := mustCreate(t, m, validCreateReq())
	if _, err := m.MarkPaid(o, pricing.MustParseAmount("153"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartFulfilment(o, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(o, StateCancelled, 2); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("a paid order must not be cancellable; it must be refunded")
	}
	if _, err := m.StartRefund(o, false, 2); err != nil {
		t.Fatalf("failed fulfilment should be refundable: %v", err)
	}
	if o.State != StateRefunding {
		t.Fatalf("state = %s, want REFUNDING", o.State)
	}
}

// --- Idempotency / optimistic locking ---

func TestVersionConflictRejectsStaleUpdate(t *testing.T) {
	m := newMachine()
	o := mustCreate(t, m, validCreateReq())

	if _, err := m.MarkPaid(o, pricing.MustParseAmount("153"), 0); err != nil {
		t.Fatal(err)
	}
	// A duplicate payment callback arrives carrying the old version. It must
	// be rejected rather than applied a second time.
	if _, err := m.MarkPaid(o, pricing.MustParseAmount("153"), 0); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("duplicate callback: expected ErrVersionConflict, got %v", err)
	}
	if o.Version != 1 {
		t.Fatalf("version = %d, want 1 (duplicate must not advance it)", o.Version)
	}
}

func TestDuplicatePaymentCallbackIsIdempotent(t *testing.T) {
	// Simulates the real guard: a repeated callback finds the row already
	// advanced. In SQL this is `WHERE status=? AND version=?` affecting zero
	// rows; here the version check stands in for it.
	m := newMachine()
	o := mustCreate(t, m, validCreateReq())
	if _, err := m.MarkPaid(o, pricing.MustParseAmount("153"), 0); err != nil {
		t.Fatal(err)
	}
	stateAfterFirst, versionAfterFirst := o.State, o.Version

	for i := 0; i < 3; i++ {
		_, _ = m.MarkPaid(o, pricing.MustParseAmount("153"), 0)
	}
	if o.State != stateAfterFirst || o.Version != versionAfterFirst {
		t.Fatalf("repeated callbacks changed the order: %s v%d → %s v%d",
			stateAfterFirst, versionAfterFirst, o.State, o.Version)
	}
}

func TestPaymentAmountMismatchRejected(t *testing.T) {
	// A channel reporting a different amount than the order expects is a
	// reconciliation problem; accepting it quietly creates a silent shortfall.
	m := newMachine()
	o := mustCreate(t, m, validCreateReq())
	if _, err := m.MarkPaid(o, pricing.MustParseAmount("100"), 0); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("expected ErrAmountMismatch, got %v", err)
	}
	if o.State != StatePendingPayment {
		t.Fatal("mismatched payment must not advance the order")
	}
}

// --- Refund ordering (03§8.4 钱货两清有先后) ---

func TestRefundBlockedUntilResourceReleased(t *testing.T) {
	m := newMachine()
	o := mustCreate(t, m, validCreateReq())
	if _, err := m.MarkPaid(o, pricing.MustParseAmount("153"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartFulfilment(o, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompleteFulfilment(o, "euecs-cn-north-1-01-a1b2c3d4", 2); err != nil {
		t.Fatal(err)
	}

	// Resource still running: refund must be refused.
	if _, err := m.StartRefund(o, false, 3); !errors.Is(err, ErrResourceNotReleased) {
		t.Fatalf("expected ErrResourceNotReleased, got %v", err)
	}
	if o.State != StateCompleted {
		t.Fatal("blocked refund must not change state")
	}

	// After release it proceeds.
	if _, err := m.StartRefund(o, true, 3); err != nil {
		t.Fatalf("refund after release should succeed: %v", err)
	}
	if _, err := m.CompleteRefund(o, 4); err != nil {
		t.Fatal(err)
	}
	if o.State != StateRefunded {
		t.Fatalf("final state = %s, want REFUNDED", o.State)
	}
}

// --- Proration ---

func TestRefundProration(t *testing.T) {
	cases := []struct {
		name                    string
		amount                  string
		total, used             int64
		want                    string
	}{
		{"half the term used", "1200", 12, 6, "600"},
		{"one month of twelve", "1200", 12, 1, "1100"},
		{"fully consumed", "1200", 12, 12, "0"},
		{"over-consumed", "1200", 12, 15, "0"},
		{"nothing used", "1200", 12, 0, "1200"},
		{"monthly, half used", "180", 1, 0, "180"},
	}
	for _, c := range cases {
		b := ProrationBasis{
			SnapshotAmount: pricing.MustParseAmount(c.amount),
			TotalPeriods:   c.total,
			UsedPeriods:    c.used,
		}
		if got := b.RefundAmount().String(); got != c.want {
			t.Errorf("%s: refund = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestRefundNeverNegative(t *testing.T) {
	b := ProrationBasis{
		SnapshotAmount: pricing.MustParseAmount("100"),
		TotalPeriods:   0, // degenerate input
		UsedPeriods:    5,
	}
	if got := b.RefundAmount(); got.IsNegative() {
		t.Fatalf("refund = %s, must never be negative", got)
	}
}

// --- Event contract ---

func TestEventTypeNaming(t *testing.T) {
	// Event names feed the cloud.trade.order.event topic; downstream consumers
	// switch on them, so the mapping is a contract.
	cases := map[State]string{
		StatePaid:       "cloud.trade.order.paid",
		StateFulfilling: "cloud.trade.order.fulfilling",
		StateCompleted:  "cloud.trade.order.completed",
		StateCancelled:  "cloud.trade.order.cancelled",
		StateRefunding:  "cloud.trade.order.refunding",
		StateRefunded:   "cloud.trade.order.refunded",
	}
	for state, want := range cases {
		if got := (Event{ToState: state}).EventType(); got != want {
			t.Errorf("state %s → event %q, want %q", state, got, want)
		}
	}
}

func TestTerminalStates(t *testing.T) {
	terminal := []State{StateCompleted, StateCancelled, StateRefunded}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	nonTerminal := []State{StatePendingPayment, StatePaid, StateFulfilling, StateRefunding}
	for _, s := range nonTerminal {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestAllFiveTransactionTypesSupported(t *testing.T) {
	// 对标启示 9: one unified order model covers all five types. If a type
	// falls out of the model, a product line will build its own order table.
	types := []Type{TypeNew, TypeRenew, TypeUpgrade, TypeDowngrade, TypeRefund}
	m := newMachine()
	for _, typ := range types {
		if !typ.Valid() {
			t.Errorf("%s should be a valid type", typ)
		}
		req := validCreateReq()
		req.Type = typ
		if typ.RequiresExistingResource() {
			req.ResourceID = "euecs-cn-north-1-01-a1b2c3d4"
		}
		o, _, err := m.Create(req, 1, "x")
		if err != nil {
			t.Errorf("%s order should be creatable: %v", typ, err)
			continue
		}
		if o.Type != typ {
			t.Errorf("type not preserved: %s", o.Type)
		}
	}
}
