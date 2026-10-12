// Package main: svc-order — order center (03-backend-services.md §4.2.2, 五种交易类型一个模型).
//
// Owns the unified order model + state machine (pkg-go/order). A paid order is
// the ONLY trigger for provisioning (svc-orchestrator fans out on PAID).
// Five transaction types share one model: NEW / RENEW / UPGRADE / DOWNGRADE / REFUND.
//
// Routes (gateway-authorized, X-Euler-Account-Id injected):
//
//	POST   /api/v1/orders             — create an order (TypeNew/RENEW/UPGRADE/...)
//	GET    /api/v1/orders             — list the account's orders
//	GET    /api/v1/orders/{id}        — order detail
//	POST   /api/v1/orders/{id}/pay    — pay the server-priced payable from the
//	                                    account balance (svc-payment) and mark
//	                                    the order PAID; the body is ignored
//	POST   /api/v1/orders/{id}/cancel — cancel a pending order
//	GET    /api/v1/orders/autorenew   — list the account's auto-renew settings
//	PUT    /api/v1/orders/autorenew   — set/clear a resource's auto-renew flag
//	POST   /api/v1/trial/claim        — 免费试用领取 (B7; gated by pkg-go/trial)
//	GET    /api/v1/trial/status       — 试用资格预检 + 试用券
//	GET    /api/v1/coupons            — the account's coupons (t_coupon), with
//	                                    status and optional applicability
//	GET    /api/v1/coupons/{couponId} — one coupon
//
// Internal (service-to-service, never routed by the gateway):
//
//	POST   /internal/orders/{id}/fulfilment — svc-orchestrator reports how a
//	                                    paid order's provisioning ended
//	                                    (COMPLETED, or FAILED → refund)
//
// stdlib-HTTP service. Envelope {RequestId,Code,Data} (03§9.3). trade_db when
// EULER_DB_DSN is set (orders, coupons, trial state), in-memory otherwise.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/runmode"
	"github.com/qifalab/euler-platform/topology"
	"github.com/qifalab/euler-platform/trial"
)

// accountIDHeader carries the caller's account id. TRUST NOTE: this header is
// injected by the API gateway after authentication; this service trusts it and
// must therefore never be exposed directly to the public network. Deployments
// that want defense-in-depth set EULER_INTERNAL_TOKEN, which makes the service
// additionally require a matching X-Euler-Internal-Token shared-secret header
// (dev default: unset, check disabled).
const accountIDHeader = "X-Euler-Account-Id"

const internalTokenHeader = "X-Euler-Internal-Token"

// maxBodyBytes bounds request bodies before JSON decoding (~1MB).
const maxBodyBytes = 1 << 20

// errAmountOutOfRange marks a minor-unit amount that cannot be represented as
// a pricing.Amount (micro-yuan).
var errAmountOutOfRange = errors.New("svc-order: amountMinor out of range")

// minorToAmount converts minor units (分) to pricing.Amount (micro-yuan),
// matching svc-payment's conversion: 1 分 = 10_000 micro-yuan.
//
// The bound is checked BEFORE multiplying: minor × 10_000 wraps for large
// inputs, and a wrapped amount is a silently corrupted order (a 10^19 分
// request produced a ¥1.55 trillion payable in the pre-fix code) rather than
// a rejected one. A negative amount is rejected here too — the movement is a
// magnitude.
func minorToAmount(minor int64) (pricing.Amount, error) {
	const maxMinor = int64(1<<63-1) / 10_000
	if minor < 0 || minor > maxMinor {
		return 0, fmt.Errorf("%w: %d", errAmountOutOfRange, minor)
	}
	return pricing.Amount(minor * 10_000), nil
}

// orderStore wires the order machine to the HTTP handlers over an orderRepo.
// The repo decides where orders live: trade_db when EULER_DB_DSN is set, the
// in-memory store otherwise. Handlers hold no locks — the repo owns its own
// synchronisation, which is also what lets the SQL implementation serialise
// transitions with row locks instead of a process mutex.
type orderStore struct {
	repo    orderRepo
	machine *order.Machine
	// quoter prices every order server-side (svc-catalog); payer debits the
	// balance when an order is paid (svc-payment). Neither the price nor the
	// payment is ever taken from the caller.
	quoter quoter
	payer  payer
}

func newOrderStoreWith(repo orderRepo) *orderStore {
	return &orderStore{
		repo:    repo,
		machine: order.NewMachine(time.Now),
		quoter:  newCatalogQuoter(),
		payer:   newBalancePayer(),
	}
}

// newInMemoryOrderStore builds the demo store, seeded to match console-bff.
// Handler tests construct it directly, so they never depend on whether the
// developer's shell has EULER_DB_DSN set.
func newInMemoryOrderStore() *orderStore {
	s := newOrderStoreWith(newMemOrderRepo())
	s.seed()
	return s
}

func (s *orderStore) seed() {
	// Seed one pending-payment order (matches console-bff seed for account 100123).
	o, evt, err := s.machine.Create(order.CreateRequest{
		AccountID: 100123, Type: order.TypeNew, ProductCode: "euecs",
		ChargeType: pricing.ChargePrepaid, SKUCode: "euecs.c1", RegionID: "cn-north-1",
		Quote:       pricing.Result{PayableAmount: pricing.MustParseAmount("2160")},
		ClientToken: "seed-100123-9001",
	}, 9001, "SO202608110001")
	if err != nil {
		panic(fmt.Sprintf("seed order failed: %v", err))
	}
	if err := s.repo.Create(o, evt, nil); err != nil {
		panic(fmt.Sprintf("seed order failed: %v", err))
	}
}

type createOrderRequest struct {
	Type        string `json:"type"` // only NEW is accepted here; renewals are minted by the scheduler
	ProductCode string `json:"productCode"`
	SKUCode     string `json:"skuCode"`
	RegionID    string `json:"regionId"`
	ZoneID      string `json:"zoneId"`     // required by the pricing engine for ZONAL products
	ChargeType  string `json:"chargeType"` // PREPAID / POSTPAID; "" = the SKU's catalogue default
	Quantity    int64  `json:"quantity"`
	Duration    int64  `json:"duration"`
	// AmountMinor is accepted for backward compatibility and range-checked,
	// but it is NOT the price: the payable comes from svc-catalog. A client
	// that could name its own price could buy anything for one 分.
	AmountMinor int64  `json:"amountMinor"`
	ClientToken string `json:"clientToken"` // idempotency token; generated server-side if absent
	// CouponIDs are coupons (t_coupon) to apply, at most maxCouponsPerOrder.
	// They are priced server-side on top of the quote and reserved for the
	// order; the client never names a deduction.
	CouponIDs []string `json:"couponIds"`
}

// Bounds on what one order line may ask for; the pricing engine multiplies by
// both, so absurd values are refused before they reach it.
const (
	maxOrderQuantity = 1000
	maxOrderDuration = 60 // months
)

// Column widths of trade_db's order tables (V1 DDL). A longer value is a client
// error: MySQL would reject it as a 500 — or, without strict mode, truncate it,
// and a truncated client_token makes two different tokens collide.
const (
	maxClientTokenLen = 64 // order_main.client_token
	maxProductCodeLen = 32 // order_main.product_code
	maxSKUCodeLen     = 64 // order_item.sku_code
	maxRegionIDLen    = 32 // order_item.region_id
	maxZoneIDLen      = 32 // order_item.zone_id
)

// orderPayKey is the ledger idempotency key of an order's payment. It lives in
// svc-payment's reserved internal namespace (tenant routes refuse order-pay-*),
// so no tenant can pre-plant an entry under it.
func orderPayKey(orderID int64) string { return fmt.Sprintf("order-pay-%d", orderID) }

func (s *orderStore) handleCreate(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req createOrderRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.ProductCode == "" {
		writeErr(w, "Common.InvalidParameter", 400, "productCode is required")
		return
	}
	// The client amount is never used, but a malformed one is still a client
	// bug worth rejecting rather than silently ignoring.
	if _, err := minorToAmount(req.AmountMinor); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "amountMinor out of range")
		return
	}
	if req.SKUCode == "" {
		writeErr(w, "Common.InvalidParameter", 400, "skuCode is required")
		return
	}
	if len(req.ClientToken) > maxClientTokenLen || len(req.ProductCode) > maxProductCodeLen ||
		len(req.SKUCode) > maxSKUCodeLen || len(req.RegionID) > maxRegionIDLen || len(req.ZoneID) > maxZoneIDLen {
		writeErr(w, "Common.InvalidParameter", 400, "clientToken, productCode, skuCode, regionId or zoneId is too long")
		return
	}
	if msg := validateCouponIDs(req.CouponIDs); msg != "" {
		writeErr(w, "Common.InvalidParameter", 400, msg)
		return
	}
	// Placement (M-6, 00§4.4): a zone is an AZ name of the order's region
	// ({region}-{letter}, the shape svc-catalog's quote and the topology
	// config use). A zone-only order takes its region from the zone, exactly as
	// the catalogue prices it.
	if req.ZoneID != "" {
		az, err := topology.ParseAZName(req.ZoneID)
		if err != nil {
			writeErr(w, "Order.InvalidZone", 400, err.Error())
			return
		}
		switch {
		case req.RegionID == "":
			req.RegionID = az.Region
		case az.Region != req.RegionID:
			writeErr(w, "Order.ZoneRegionMismatch", 400, "zone "+req.ZoneID+" is not in region "+req.RegionID)
			return
		}
	}
	ot := order.Type(req.Type)
	if ot == "" {
		ot = order.TypeNew
	}
	if ot != order.TypeNew {
		// RENEW/UPGRADE/DOWNGRADE/REFUND carry pricing rules (proration,
		// refund-after-release) this endpoint does not implement; accepting
		// them here would price them as a fresh purchase.
		writeErr(w, "Order.TypeNotSupported", 400, "only NEW orders can be created through this API")
		return
	}
	if req.Quantity == 0 {
		req.Quantity = 1
	}
	if req.Duration == 0 {
		req.Duration = 1
	}
	// svc-orchestrator provisions exactly one resource per order, so an order
	// for N units charged list×N and delivered one. Until fulfilment fans out
	// per unit, anything but one unit is refused instead of over-charged.
	if req.Quantity != 1 {
		writeErr(w, "Order.QuantityUnsupported", 400, "only 1 per order is supported")
		return
	}
	if req.Quantity < 1 || req.Quantity > maxOrderQuantity || req.Duration < 1 || req.Duration > maxOrderDuration {
		writeErr(w, "Common.InvalidParameter", 400, "quantity or duration out of range")
		return
	}
	// Idempotency: a retried create with the same clientToken returns the
	// existing order instead of creating a duplicate. The pre-check is a fast
	// path; uk_client_token is the final arbiter and is handled after the insert.
	if req.ClientToken != "" {
		if existing, err := s.repo.ByClientToken(acct, req.ClientToken); err == nil && existing != nil {
			writeJSON(w, "OK", orderToMap(existing))
			return
		}
	}

	// Server-side price: the pricing engine is the only source of the payable.
	q, err := s.quoter.Quote(r.Context(), acct, quoteInput{
		ProductCode: req.ProductCode, SKUCode: req.SKUCode, ChargeType: req.ChargeType,
		RegionID: req.RegionID, ZoneID: req.ZoneID, Quantity: req.Quantity, Duration: req.Duration,
	})
	if err != nil {
		var de *downstreamError
		if errors.As(err, &de) {
			writeErr(w, de.Code, de.Status, de.Message)
			return
		}
		slog.Error("order pricing failed", "account", acct, "product", req.ProductCode, "err", err)
		writeErr(w, "Order.PricingUnavailable", 503, "pricing service unavailable")
		return
	}
	if !q.ChargeType.SellableInPhase1() {
		writeErr(w, "Order.ChargeTypeNotSupported", 400, "charge type "+string(q.ChargeType)+" cannot be ordered here")
		return
	}
	// The catalogue is the authority on placement scope and refuses a ZONAL
	// quote without a zone. When it reports the scope, svc-order holds the same
	// line: a ZONAL order must name its zone, and a REGIONAL product spreads
	// across zones, so a zone sent for one is not recorded (the orchestrator
	// would otherwise pin a regional resource to it).
	zoneID := req.ZoneID
	switch topology.RegionScope(q.RegionScope) {
	case topology.ScopeZonal:
		if zoneID == "" {
			writeErr(w, "Order.ZoneRequired", 400, "ZONAL product "+req.ProductCode+" requires zoneId")
			return
		}
	case topology.ScopeRegional:
		zoneID = ""
	}
	// 包年包月 is paid up front; 按量 starts at zero and is billed from metering
	// once the resource is RUNNING (arrears lifecycle covers non-payment).
	payable := pricing.Amount(0)
	if q.ChargeType == pricing.ChargePrepaid {
		payable = roundToCent(q.Payable)
	}
	// Coupons apply on top of the quote (pkg-go/pricing ③), server-side and in
	// 分: list is what the order costs before them.
	list := payable
	var couponUses []pricing.CouponUse
	if len(req.CouponIDs) > 0 {
		uses, deduction, err := s.priceCoupons(acct, req.CouponIDs, pricing.Request{
			AccountID: acct, ProductCode: req.ProductCode, SKUCode: req.SKUCode, RegionID: req.RegionID,
			ChargeType: q.ChargeType, At: s.machine.Now(),
		}, list)
		if err != nil {
			s.couponCreateFailed(w, acct, req.ClientToken, err)
			return
		}
		couponUses, payable = uses, list-deduction
	}
	if req.AmountMinor != 0 {
		if claimed, _ := minorToAmount(req.AmountMinor); claimed != payable {
			slog.Info("client amount differs from server price (ignored)",
				"account", acct, "product", req.ProductCode, "clientMinor", req.AmountMinor, "payable", payable.String())
		}
	}

	orderID := s.repo.NewOrderID()
	orderNo := orderNumber(orderID)
	clientToken := req.ClientToken
	if clientToken == "" {
		// No client-supplied token: mint a server-side one (non-idempotent path).
		clientToken = fmt.Sprintf("ct-%d-%d-%d", acct, orderID, time.Now().UnixNano())
	}
	durUnit := q.DurationUnit
	if durUnit == "" {
		durUnit = pricing.DurationMonth
	}
	o, evt, err := s.machine.Create(order.CreateRequest{
		AccountID: acct, Type: ot, ProductCode: req.ProductCode,
		ChargeType: q.ChargeType,
		SKUCode:    req.SKUCode, RegionID: req.RegionID, ZoneID: zoneID, Quantity: req.Quantity,
		Duration: req.Duration, DurationUnit: durUnit,
		Quote: pricing.Result{
			ListAmount: list, CouponAmount: list - payable, PayableAmount: payable,
			AppliedCoupons: couponUses,
		},
		ClientToken: clientToken,
	}, orderID, orderNo)
	if err != nil {
		slog.Error("order create failed", "account", acct, "err", err)
		writeErr(w, "Order.CreateFailed", 400, "order create failed")
		return
	}
	// The event rides the same transaction as the order row (03§8.2): a create
	// with no event is an order the fulfilment chain can never see.
	err = s.repo.Create(o, evt, couponUses)
	if errors.Is(err, errDuplicateOrderKey) {
		// The minted order id or order number is already taken (nothing of
		// this order was written): mint it once more under a fresh id.
		slog.Warn("order id/number already taken; retrying with a fresh one",
			"account", acct, "orderId", orderID, "orderNo", orderNo, "err", err)
		orderID = s.repo.NewOrderID()
		orderNo = orderNumber(orderID)
		o.OrderID, o.OrderNo = orderID, orderNo
		evt.OrderID, evt.OrderNo = orderID, orderNo
		err = s.repo.Create(o, evt, couponUses)
	}
	var unavailable *couponUnavailableError
	if errors.As(err, &unavailable) {
		// A selected coupon was reserved, spent or expired between pricing
		// and the reservation: nothing was written.
		s.couponCreateFailed(w, acct, req.ClientToken, invalidCoupon("%s", unavailable.Error()))
		return
	}
	if err != nil {
		// Lost the uk_client_token race to a concurrent duplicate: serve the
		// winner's order rather than a 500.
		if errors.Is(err, errDuplicateClientToken) || errors.Is(err, errDuplicateOrderKey) {
			if existing, gerr := s.repo.ByClientToken(acct, clientToken); gerr == nil && existing != nil {
				writeJSON(w, "OK", orderToMap(existing))
				return
			}
		}
		slog.Error("order persist failed", "account", acct, "orderId", orderID, "err", err)
		writeErr(w, "Order.CreateFailed", 500, "order create failed")
		return
	}
	writeJSON(w, "OK", orderToMap(o))
}

// couponCreateFailed answers a create whose coupons could not be applied. A
// retried create whose first attempt already reserved them (same clientToken)
// is served the order that attempt made, as the idempotency contract promises.
func (s *orderStore) couponCreateFailed(w http.ResponseWriter, acct int64, clientToken string, err error) {
	if clientToken != "" {
		if existing, gerr := s.repo.ByClientToken(acct, clientToken); gerr == nil && existing != nil {
			writeJSON(w, "OK", orderToMap(existing))
			return
		}
	}
	var invalid *couponInvalidError
	if errors.As(err, &invalid) {
		writeErr(w, "Order.CouponInvalid", 400, invalid.Error())
		return
	}
	slog.Error("order coupon pricing failed", "account", acct, "err", err)
	writeErr(w, "Order.CreateFailed", 500, "order create failed")
}

// orderNumber is the customer-facing number of a freshly minted order id.
func orderNumber(orderID int64) string {
	return fmt.Sprintf("SO%d%06d", time.Now().Year(), orderID)
}

func (s *orderStore) handleList(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	orders, err := s.repo.List(acct)
	if err != nil {
		slog.Error("order list failed", "account", acct, "err", err)
		writeErr(w, "Order.ListFailed", 500, "order list failed")
		return
	}
	out := make([]map[string]any, 0, len(orders))
	for _, o := range orders {
		out = append(out, orderToMap(o))
	}
	writeJSON(w, "OK", out)
}

func (s *orderStore) handleDetail(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "invalid order id")
		return
	}
	o, err := s.repo.Get(acct, id)
	if err != nil {
		slog.Error("order lookup failed", "orderId", id, "err", err)
		writeErr(w, "Order.LookupFailed", 500, "order lookup failed")
		return
	}
	if o == nil {
		writeErr(w, "Order.NotFound", 404, "订单不存在")
		return
	}
	writeJSON(w, "OK", orderToMap(o))
}

// handlePay implements POST /api/v1/orders/{id}/pay — pay an order from the
// account's cash balance.
//
// The request body is ignored. This endpoint used to be a "payment callback"
// that accepted whatever paymentId and paidAmountMinor the caller reported,
// which let any caller mark its own order PAID without moving money. Now the
// amount is the order's server-priced payable and the payment proof is the
// svc-payment ledger entry this handler itself creates.
func (s *orderStore) handlePay(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "invalid order id")
		return
	}

	o, err := s.repo.Get(acct, id)
	if err != nil {
		slog.Error("order lookup failed", "orderId", id, "err", err)
		writeErr(w, "Order.LookupFailed", 500, "order lookup failed")
		return
	}
	if o == nil {
		writeErr(w, "Order.NotFound", 404, "订单不存在")
		return
	}
	// Already settled: paying again is an idempotent no-op, never a second debit.
	if _, has, err := s.repo.PaymentRef(acct, id); err != nil {
		slog.Error("payment ref lookup failed", "orderId", id, "err", err)
		writeErr(w, "Order.LookupFailed", 500, "order lookup failed")
		return
	} else if has {
		writeJSON(w, "OK", orderToMap(o))
		return
	}
	if o.State != order.StatePendingPayment {
		if o.State == order.StateCancelled {
			// A /pay that debited and then lost (or crashed) before it could
			// give the money back left a debit on a cancelled order; any later
			// attempt settles it. Idempotent, and a no-op without a debit.
			_, _ = s.refundOrderPayment(r.Context(), o, "order cancelled before payment completed")
		}
		writeErr(w, "Order.StateTransitionFailed", 409, "order is not awaiting payment")
		return
	}
	// The order was priced with its coupons; one that lapsed while the order
	// waited cannot pay for it. Checked before any money moves.
	if len(o.CouponIDs) > 0 {
		uses, err := s.repo.CouponUses(acct, id)
		if err != nil {
			slog.Error("order coupon lookup failed", "orderId", id, "err", err)
			writeErr(w, "Order.LookupFailed", 500, "order lookup failed")
			return
		}
		now := s.machine.Now()
		for _, u := range uses {
			if u.State == useLocked && !u.ExpireAt.IsZero() && !now.Before(u.ExpireAt) {
				writeErr(w, "Order.CouponExpired", 409,
					"coupon "+u.CouponID+" expired before payment; cancel this order and order again")
				return
			}
		}
	}

	ref := "free"
	if o.PayableAmount > 0 {
		if int64(o.PayableAmount)%10_000 != 0 {
			// Orders are priced in whole 分; anything else is a reconciliation
			// defect that must not be papered over by rounding at pay time.
			slog.Error("order payable is not cent-aligned", "orderId", o.OrderID, "payable", o.PayableAmount.String())
			writeErr(w, "Order.AmountInvalid", 409, "order payable is not payable in 分")
			return
		}
		entryID, err := s.payer.Consume(r.Context(), acct, int64(o.PayableAmount)/10_000,
			orderPayKey(o.OrderID), o.OrderNo, "pay order "+o.OrderNo)
		switch {
		case errors.Is(err, errInsufficientBalance):
			writeErr(w, "Order.InsufficientBalance", 402, "余额不足,请先充值")
			return
		case errors.Is(err, errPaymentConflict):
			// The key names some other movement: never proof of payment.
			slog.Error("order payment key conflict", "orderId", o.OrderID, "err", err)
			writeErr(w, "Order.PaymentConflict", 409, "order payment could not be verified")
			return
		case err != nil:
			slog.Error("order payment failed", "orderId", o.OrderID, "err", err)
			writeErr(w, "Order.PaymentUnavailable", 503, "payment service unavailable")
			return
		}
		ref = "ledger:" + entryID
	}

	// MarkPaid re-checks the paid amount against the payable (it is the same
	// server-side number now, so this only guards future refactors).
	unpaid := *o
	from, prevVersion := o.State, o.Version
	evt, err := s.machine.MarkPaid(o, o.PayableAmount, o.Version)
	if err != nil {
		slog.Warn("order pay transition failed", "orderId", o.OrderID, "err", err)
		writeErr(w, "Order.StateTransitionFailed", 409, "order state transition failed")
		return
	}
	// State, journal, event and payment proof commit as one unit. The debit
	// above is idempotent on order-pay-{id}, so if this write fails a retry
	// re-reads the same ledger entry instead of charging twice.
	if err := s.repo.SaveTransition(o, from, prevVersion, "payment "+ref, evt, ref); err != nil {
		// A concurrent /pay for the same order may have won the version race
		// with the same (idempotent) debit: report the settled order.
		if prior, has, perr := s.repo.PaymentRef(acct, id); perr == nil && has && prior == ref {
			if cur, gerr := s.repo.Get(acct, id); gerr == nil && cur != nil {
				writeJSON(w, "OK", orderToMap(cur))
				return
			}
		}
		// The money moved before the order was claimed, so a cancel that won
		// the race leaves a CANCELLED order holding a debit. It will never be
		// paid now: give the money back (idempotent on the pay key, and shared
		// with the cancel path, so the two cannot refund twice).
		if cur, gerr := s.repo.Get(acct, id); gerr == nil && cur != nil && cur.State == order.StateCancelled {
			if _, rerr := s.refundOrderPayment(r.Context(), &unpaid, "order cancelled while payment was in flight"); rerr != nil {
				writeErr(w, "Order.RefundPending", 503, "order was cancelled; the refund of its payment is pending")
				return
			}
			writeErr(w, "Order.StateTransitionFailed", 409, "order was cancelled; its payment has been refunded")
			return
		}
		slog.Error("order persist failed", "orderId", o.OrderID, "err", err)
		writeErr(w, "Order.StateTransitionFailed", 409, "order state transition failed")
		return
	}
	writeJSON(w, "OK", orderToMap(o))
}

// refundOrderPayment returns whatever svc-payment debited under the order's pay
// key. It is safe to call from every path that learns the order will never be
// paid (a cancel, a /pay that lost to one): svc-payment refunds a debit at most
// once (refund:{payKey}) and reports refunded=false when nothing was debited.
// refunded says whether a debit was given back (false: nothing payable, or no
// debit under the pay key). Failures are logged for reconciliation and
// returned.
func (s *orderStore) refundOrderPayment(ctx context.Context, o *order.Order, reason string) (refunded bool, err error) {
	if o.PayableAmount <= 0 || int64(o.PayableAmount)%10_000 != 0 {
		return false, nil // nothing payable, or never payable (pay refuses sub-分 amounts)
	}
	refunded, err = s.payer.Refund(ctx, o.AccountID, orderPayKey(o.OrderID), int64(o.PayableAmount)/10_000, reason)
	if err != nil {
		slog.Error("order payment refund failed; debit needs reconciliation",
			"orderId", o.OrderID, "account", o.AccountID, "err", err)
		return false, err
	}
	if refunded {
		slog.Warn("order payment refunded", "orderId", o.OrderID, "account", o.AccountID, "reason", reason)
	}
	return refunded, nil
}

func (s *orderStore) handleCancel(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "invalid order id")
		return
	}
	o, err := s.repo.Get(acct, id)
	if err != nil {
		slog.Error("order lookup failed", "orderId", id, "err", err)
		writeErr(w, "Order.LookupFailed", 500, "order lookup failed")
		return
	}
	if o == nil {
		writeErr(w, "Order.NotFound", 404, "订单不存在")
		return
	}
	from, prevVersion := o.State, o.Version
	evt, err := s.machine.Transition(o, order.StateCancelled, o.Version)
	if err != nil {
		if o.State == order.StateCancelled {
			// A repeated cancel re-runs the compensation an earlier attempt
			// may not have completed.
			_, _ = s.refundOrderPayment(r.Context(), o, "order cancelled")
		}
		slog.Warn("order cancel transition failed", "orderId", o.OrderID, "err", err)
		writeErr(w, "Order.StateTransitionFailed", 409, "order state transition failed")
		return
	}
	if err := s.repo.SaveTransition(o, from, prevVersion, "cancelled by customer", evt, ""); err != nil {
		slog.Error("order persist failed", "orderId", o.OrderID, "err", err)
		writeErr(w, "Order.StateTransitionFailed", 409, "order state transition failed")
		return
	}
	// The cancel is committed, so the order can never be paid. A /pay racing
	// it may already have debited (or crashed after debiting): give that money
	// back. Refunding only AFTER the commit matters — refunding first would let
	// a /pay that then wins the transition keep a PAID order with its money
	// returned. The racing /pay refunds too if its debit lands after this call.
	_, _ = s.refundOrderPayment(r.Context(), o, "order cancelled")
	writeJSON(w, "OK", orderToMap(o))
}

// --- fulfilment outcome (internal) ----------------------------------------

// fulfilmentReport is the body of POST /internal/orders/{id}/fulfilment, sent
// by svc-orchestrator once a paid order's provisioning saga has settled.
type fulfilmentReport struct {
	Result     string `json:"result"`     // COMPLETED | FAILED
	ResourceID string `json:"resourceId"` // the provisioned resource (COMPLETED)
	Reason     string `json:"reason"`     // why provisioning failed (FAILED)
}

const (
	fulfilmentCompleted = "COMPLETED"
	fulfilmentFailed    = "FAILED"
)

// Column widths the fulfilment report writes: order_main.resource_id
// (VARCHAR(64)) and order_state_log.reason (VARCHAR(256), characters).
const (
	maxResourceIDLen       = 64
	maxStateLogReasonRunes = 256
)

// maxFulfilmentSteps bounds handleFulfilment's re-read loop. The longest path
// is two edges; the rest is headroom for lost version races.
const maxFulfilmentSteps = 8

// handleFulfilment implements POST /internal/orders/{id}/fulfilment — the
// orchestrator's report of how a paid order's provisioning ended. Without it an
// order stayed PAID forever and a failed provisioning was never refunded: the
// customer paid and got nothing.
//
//   - COMPLETED: PAID → FULFILLING → COMPLETED, recording the resource id.
//   - FAILED: PAID/FULFILLING → REFUNDING, the pay debit is refunded through
//     svc-payment (the helper cancel uses; idempotent on the pay key), then
//     → REFUNDED. If the refund cannot be made the order stays REFUNDING and
//     the call answers 503 Order.RefundPending, so the orchestrator retries.
//     If svc-payment holds no debit under the pay key although the order was
//     paid a non-zero amount, there is nothing to give back: the order still
//     ends REFUNDED (it holds no money, and REFUNDING would never settle),
//     but the journal reason says no debit was found and a reconciliation
//     warning is logged — a paid order without its debit is a discrepancy,
//     never a silent refund.
//
// Every edge is a legal transition of the order machine persisted on its own,
// so a replay resumes from wherever the previous attempt stopped, and a report
// that already took effect answers 200. A report that contradicts the order's
// state (FAILED for a COMPLETED order, anything for an unpaid or cancelled
// one) is 409.
func (s *orderStore) handleFulfilment(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "invalid order id")
		return
	}
	var rep fulfilmentReport
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if rep.Result != fulfilmentCompleted && rep.Result != fulfilmentFailed {
		writeErr(w, "Common.InvalidParameter", 400, "result must be COMPLETED or FAILED")
		return
	}
	if len(rep.ResourceID) > maxResourceIDLen {
		writeErr(w, "Common.InvalidParameter", 400, "resourceId is too long")
		return
	}
	reason := "fulfilment completed"
	if rep.Result == fulfilmentFailed {
		reason = "fulfilment failed"
		if rep.Reason != "" {
			reason += ": " + rep.Reason
		}
	}
	reason = truncateRunes(reason, maxStateLogReasonRunes)

	// One edge per pass, each re-reading the order: a version race lost to a
	// concurrent replay continues from the state the winner left.
	for step := 0; step < maxFulfilmentSteps; step++ {
		o, err := s.repo.Get(acct, id)
		if err != nil {
			slog.Error("order lookup failed", "orderId", id, "err", err)
			writeErr(w, "Order.LookupFailed", 500, "order lookup failed")
			return
		}
		if o == nil {
			writeErr(w, "Order.NotFound", 404, "订单不存在")
			return
		}
		from, prevVersion := o.State, o.Version
		stepReason := reason
		var evt order.Event
		switch {
		case rep.Result == fulfilmentCompleted && o.State == order.StateCompleted,
			rep.Result == fulfilmentFailed && o.State == order.StateRefunded:
			writeJSON(w, "OK", orderToMap(o)) // already settled: idempotent replay
			return
		case rep.Result == fulfilmentCompleted && o.State == order.StatePaid:
			evt, err = s.machine.StartFulfilment(o, o.Version)
		case rep.Result == fulfilmentCompleted && o.State == order.StateFulfilling:
			evt, err = s.machine.CompleteFulfilment(o, rep.ResourceID, o.Version)
		case rep.Result == fulfilmentFailed && (o.State == order.StatePaid || o.State == order.StateFulfilling):
			// FAILED is only reported after the orchestrator reclaimed the
			// resource (CREATE_FAILED), so nothing is left running.
			evt, err = s.machine.StartRefund(o, true, o.Version)
		case rep.Result == fulfilmentFailed && o.State == order.StateRefunding:
			refunded, rerr := s.refundOrderPayment(r.Context(), o, "fulfilment failed for order "+o.OrderNo)
			if rerr != nil {
				writeErr(w, "Order.RefundPending", 503, "fulfilment failed; the refund of the order payment is pending")
				return
			}
			if !refunded && o.PayableAmount > 0 {
				note := "; no debit found under " + orderPayKey(o.OrderID) + ", nothing refunded"
				slog.Warn("order reconciliation: fulfilment failed but svc-payment holds no debit under the pay key; nothing refunded",
					"orderId", o.OrderID, "orderNo", o.OrderNo, "account", o.AccountID,
					"payable", o.PayableAmount.String(), "payKey", orderPayKey(o.OrderID))
				stepReason = truncateRunes(reason, maxStateLogReasonRunes-utf8.RuneCountInString(note)) + note
			}
			evt, err = s.machine.CompleteRefund(o, o.Version)
		default:
			writeErr(w, "Order.StateTransitionFailed", 409,
				fmt.Sprintf("order is %s; fulfilment result %s does not apply", o.State, rep.Result))
			return
		}
		if err != nil {
			slog.Error("order fulfilment transition failed", "orderId", id, "result", rep.Result, "err", err)
			writeErr(w, "Order.StateTransitionFailed", 409, "order state transition failed")
			return
		}
		if err := s.repo.SaveTransition(o, from, prevVersion, stepReason, evt, ""); err != nil {
			if errors.Is(err, order.ErrVersionConflict) {
				continue
			}
			slog.Error("order persist failed", "orderId", id, "err", err)
			writeErr(w, "Order.PersistFailed", 500, "order state could not be saved")
			return
		}
	}
	slog.Warn("order fulfilment report kept losing version races", "orderId", id, "result", rep.Result)
	writeErr(w, "Order.Busy", 503, "order is being updated concurrently; retry")
}

// truncateRunes cuts s to at most n characters, never inside one.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// --- auto-renew settings -------------------------------------------------

// autoRenewSetting is one row of the account's auto-renew settings. ProductCode
// is carried so the console can label rows without a second join.
type autoRenewSetting struct {
	ResourceID  string `json:"resourceId"`
	ProductCode string `json:"productCode"`
	Enabled     bool   `json:"enabled"`
}

type autoRenewReq struct {
	ResourceID  string `json:"resourceId"`
	ProductCode string `json:"productCode"`
	Enabled     *bool  `json:"enabled"`
}

// handleAutoRenewList implements GET /api/v1/orders/autorenew: every setting the
// account has ever touched (enabled or not), so the console's renewal page can
// render the persisted state instead of a client-side guess.
func (s *orderStore) handleAutoRenewList(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	out, err := s.repo.ListAutoRenew(acct)
	if err != nil {
		slog.Error("auto-renew list failed", "account", acct, "err", err)
		writeErr(w, "Order.ListFailed", 500, "auto-renew list failed")
		return
	}
	writeJSON(w, "OK", out)
}

// handleAutoRenewSet implements PUT /api/v1/orders/autorenew — persist the flag
// the renewal scheduler later reads when it mints the RENEW order.
func (s *orderStore) handleAutoRenewSet(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req autoRenewReq
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.ResourceID == "" {
		writeErr(w, "Common.InvalidParameter", 400, "resourceId is required")
		return
	}
	if req.Enabled == nil {
		writeErr(w, "Common.InvalidParameter", 400, "enabled is required")
		return
	}
	setting := autoRenewSetting{ResourceID: req.ResourceID, ProductCode: req.ProductCode, Enabled: *req.Enabled}
	if err := s.repo.SetAutoRenew(acct, setting); err != nil {
		slog.Error("auto-renew persist failed", "account", acct, "resource", req.ResourceID, "err", err)
		writeErr(w, "Order.PersistFailed", 500, "auto-renew persist failed")
		return
	}
	writeJSON(w, "OK", setting)
}

func orderToMap(o *order.Order) map[string]any {
	return map[string]any{
		"orderId": o.OrderID, "orderNo": o.OrderNo, "type": string(o.Type),
		"state": string(o.State), "productCode": o.ProductCode,
		"skuCode": o.SKUCode, "regionId": o.RegionID, "chargeType": string(o.ChargeType),
		// The AZ a ZONAL resource is pinned to ("" for REGIONAL products):
		// svc-orchestrator places the resource from it.
		"zoneId": o.ZoneID,
		// listAmount is the server price before coupons (the catalogue quote,
		// promotions included); couponDeduction what the order's coupons take
		// off it; payableAmount the cash due (= listAmount − couponDeduction).
		"listAmount":      orderListAmount(o).String(),
		"couponDeduction": o.DiscountAmount.String(),
		"couponIds":       nonNilStrings(o.CouponIDs),
		"payableAmount":   o.PayableAmount.String(), "createdAt": o.CreatedAt.Format(time.RFC3339),
		"version": o.Version,
		// The purchased term, as the create request names it: svc-orchestrator
		// derives a prepaid resource's ExpiredAt from these.
		"quantity": o.Quantity, "duration": o.Duration, "durationUnit": string(o.DurationUnit),
	}
}

// orderListAmount is the order's pre-coupon price. Orders written before
// coupons existed stored no original amount (0): theirs is the payable.
func orderListAmount(o *order.Order) pricing.Amount {
	if sum := o.PayableAmount + o.DiscountAmount; o.OriginalAmount < sum {
		return sum
	}
	return o.OriginalAmount
}

// --- free trial (B7, 09-roadmap §4.4) ---------------------------------------
//
// A trial is a voucher claimed through the standard order flow (decision D7):
// 领取 mints a TRIAL-sourced VOUCHER coupon into t_coupon; using it goes through
// the same 询价 → 下单(couponIds) → 支付 pipeline as any purchase. The claim
// itself is gated by pkg-go/trial — the anti-薅 engine. The state the engine
// reads (per-account claim history, the cross-account identity registry, the
// activity budget) lives in a trialRepo: trade_db in SQL mode, shared by every
// replica and updated in one transaction with the coupon insert; the in-memory
// twin otherwise (trialrepo.go).

// trialFaceValue is the default activity's voucher face value, matching the
// t_trial_activity seed (100 元). The SQL store reads it from the activity row.
var trialFaceValue = pricing.MustParseAmount("100")

type trialStore struct {
	// policy mirrors t_trial_activity.policy_json for the default activity
	// (the SQL store takes the budget, max_active_global, from the row).
	policy trial.Policy
	// repo holds the admission state and mints the vouchers.
	repo trialRepo
	// coupons is where the vouchers land (the status page lists them).
	coupons couponRepo
	// realName is svc-iam's 实名 status (EULER_SVC_IAM_URL). nil = the seeded
	// dev registry below, allowed outside production only
	// (newTrialStoreFromEnv).
	realName realNameSource
	// devRealNames is the dev registry: account → 实名 status.
	devMu        sync.Mutex
	devRealNames map[int64]realNameStatus
	// now is the clock (injectable for tests).
	now func() time.Time
}

// newTrialStore is the standalone in-memory store (its own coupon store).
func newTrialStore() *trialStore {
	coupons := newMemCouponRepo()
	return newTrialStoreWith(newMemTrialRepo(coupons), coupons)
}

func newTrialStoreWith(repo trialRepo, coupons couponRepo) *trialStore {
	return &trialStore{
		policy:  trial.DefaultPolicy(),
		repo:    repo,
		coupons: coupons,
		// Dev seed mirrors web-auth/console-bff: account 100123 is the
		// verified seed tenant. Identity key is the hash of the doc number in
		// production; the raw document never reaches this service.
		devRealNames: map[int64]realNameStatus{
			100123: {Verified: true, Type: "personal", IdentityKey: "idn-8f3a1c2d"},
		},
		now: time.Now,
	}
}

// newTrialStoreFromEnv wires trial admission to svc-iam's 实名 status when
// EULER_SVC_IAM_URL is set. Without it the seeded registry only knows the demo
// account, so every real account fails RealName: allowed for local dev, a
// startup failure in production (pkg-go/runmode).
func newTrialStoreFromEnv(repo trialRepo, coupons couponRepo) (*trialStore, error) {
	s := newTrialStoreWith(repo, coupons)
	base := strings.TrimSpace(os.Getenv(iamURLEnv))
	if base == "" {
		if runmode.Production() {
			return nil, fmt.Errorf("%s=%s requires %s: trial admission needs svc-iam's real-name status",
				runmode.EnvVar, runmode.Env(), iamURLEnv)
		}
		slog.Warn("trial real-name status uses the seeded dev registry (allowed outside production only)", "set", iamURLEnv)
		return s, nil
	}
	s.realName = newIAMRealName(base)
	return s, nil
}

// realNameTimeout bounds the svc-iam lookup of one claim/status call.
const realNameTimeout = 3 * time.Second

// lookupRealName asks svc-iam, when configured, or the dev registry. A
// failure is reported, never guessed: the gate fails closed (503) rather than
// admitting or denying on a status it does not have.
func (s *trialStore) lookupRealName(r *http.Request, acct int64) (realNameStatus, error) {
	if s.realName == nil {
		s.devMu.Lock()
		defer s.devMu.Unlock()
		return s.devRealNames[acct], nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), realNameTimeout)
	defer cancel()
	return s.realName.RealName(ctx, acct)
}

// handleTrialClaim implements POST /api/v1/trial/claim. Denials surface the
// deciding rule as the envelope Code (e.g. TRIAL.IDENTITY_LIMIT) so an operator
// sees which gate fired, not a bare "rejected" — the same deny-first, rule
// named convention as pkg-go/authz.
func (s *trialStore) handleTrialClaim(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	rn, err := s.lookupRealName(r, acct)
	if err != nil {
		slog.Error("trial real-name lookup failed", "account", acct, "err", err)
		writeErr(w, "Order.RealNameUnavailable", 503, "实名状态暂不可用,请稍后重试")
		return
	}
	v, claim, err := s.repo.Claim(acct, rn, s.policy, s.now())
	if err != nil {
		slog.Error("trial claim failed", "account", acct, "err", err)
		writeErr(w, "Order.TrialUnavailable", 503, "试用领取暂不可用,请稍后重试")
		return
	}
	if !v.Allowed {
		writeErr(w, string(v.Rule), 409, v.Detail)
		return
	}
	c := claim.Coupon
	slog.Info("trial voucher issued", "account", acct, "coupon", c.CouponID)
	writeJSON(w, "OK", map[string]any{
		"couponId": c.CouponID, "kind": string(pricing.KindVoucher),
		"faceValue": c.FaceValue.String(), "remainValue": c.RemainValue.String(),
		"expireAt": c.ExpireAt.UTC().Format(time.RFC3339), "validDays": claim.ValidDays,
	})
}

// handleTrialStatus implements GET /api/v1/trial/status — the console's 试用中心
// page: claim eligibility preview plus the account's trial vouchers.
func (s *trialStore) handleTrialStatus(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	rn, err := s.lookupRealName(r, acct)
	if err != nil {
		slog.Error("trial real-name lookup failed", "account", acct, "err", err)
		writeErr(w, "Order.RealNameUnavailable", 503, "实名状态暂不可用,请稍后重试")
		return
	}
	now := s.now()
	v, err := s.repo.Preview(acct, rn, s.policy, now)
	if err != nil {
		slog.Error("trial preview failed", "account", acct, "err", err)
		writeErr(w, "Order.TrialUnavailable", 503, "试用状态暂不可用,请稍后重试")
		return
	}
	coupons, err := s.coupons.List(acct)
	if err != nil {
		slog.Error("trial coupon list failed", "account", acct, "err", err)
		writeErr(w, "Order.TrialUnavailable", 503, "试用状态暂不可用,请稍后重试")
		return
	}
	out := make([]map[string]any, 0, len(coupons))
	for _, c := range coupons {
		if c.Source != couponSourceTrial {
			continue
		}
		out = append(out, map[string]any{
			"couponId": c.CouponID, "faceValue": c.FaceValue.String(),
			"remainValue": c.RemainValue.String(), "expireAt": c.ExpireAt.UTC().Format(time.RFC3339),
			"status": couponAPIStatus(c, now),
		})
	}
	writeJSON(w, "OK", map[string]any{
		"eligible":     v.Allowed,
		"reason":       string(v.Rule),
		"reasonDetail": v.Detail,
		"coupons":      out,
	})
}

func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.Header.Get(accountIDHeader)
	if raw == "" {
		writeErr(w, "Common.MissingAccountId", 403, "X-Euler-Account-Id header is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed X-Euler-Account-Id")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, code string, data any) {
	rid := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Data": data})
}

func writeErr(w http.ResponseWriter, code string, status int, msg string) {
	rid := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Message": msg})
}

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := httpmw.SanitizeRequestID(r.Header.Get("X-Euler-TraceId"))
		if id == "" {
			id = fmt.Sprintf("order-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Euler-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

// internalTokenMiddleware optionally enforces a shared-secret header for
// service-to-service calls. When EULER_INTERNAL_TOKEN is set, every request must
// carry a matching X-Euler-Internal-Token; unset (dev default) the check is off.
// This complements — not replaces — the gateway trust on X-Euler-Account-Id.
func internalTokenMiddleware(h http.Handler) http.Handler {
	return httpmw.InternalToken(h)
}

func recoverMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "rec", rec, "path", r.URL.Path)
				writeErr(w, "Common.InternalError", 500, "internal error")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("http", ":9204", "HTTP listen address")
	flag.Parse()
	// Production refuses to start without the internal shared secret: the
	// X-Euler-Account-Id this service trusts must provably come from the gateway.
	runtimeRequireSecrets()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	repo, err := newOrderRepo(context.Background())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	store := newOrderStoreWith(repo)
	// Log where orders land: in-memory orders vanish on restart, and a customer's
	// paid order vanishing is not a recoverable incident.
	slog.Info("svc-order store ready", "persistent", persistentRepo(repo))
	trialRepo, err := newTrialRepoFor(context.Background(), repo)
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	trials, err := newTrialStoreFromEnv(trialRepo, repo.Coupons())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/orders", store.handleCreate)
	mux.HandleFunc("GET /api/v1/orders", store.handleList)
	mux.HandleFunc("GET /api/v1/orders/{id}", store.handleDetail)
	mux.HandleFunc("POST /api/v1/orders/{id}/pay", store.handlePay)
	mux.HandleFunc("POST /api/v1/orders/{id}/cancel", store.handleCancel)
	// svc-orchestrator's settlement of a paid order (internal: the gateway
	// never routes /internal/*).
	mux.HandleFunc("POST /internal/orders/{id}/fulfilment", store.handleFulfilment)
	// Static segment beats {id} in Go 1.22 mux precedence, so "autorenew" is
	// routed here and not swallowed by the {id} detail route.
	mux.HandleFunc("GET /api/v1/orders/autorenew", store.handleAutoRenewList)
	mux.HandleFunc("PUT /api/v1/orders/autorenew", store.handleAutoRenewSet)
	// B7 免费试用: claim is gated by pkg-go/trial; the voucher itself spends
	// through the standard order flow above (decision D7).
	mux.HandleFunc("POST /api/v1/trial/claim", trials.handleTrialClaim)
	mux.HandleFunc("GET /api/v1/trial/status", trials.handleTrialStatus)
	// Coupons (t_coupon) the account can apply through couponIds on create.
	mux.HandleFunc("GET /api/v1/coupons", store.handleCouponList)
	mux.HandleFunc("GET /api/v1/coupons/{couponId}", store.handleCouponDetail)

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(internalTokenMiddleware(mux))), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("svc-order listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutdown signal received, draining")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
}

// runtimeRequireSecrets enforces the production secret set (pkg-go/runmode).
func runtimeRequireSecrets() {
	runmode.MustRequireSecrets(httpmw.InternalTokenEnv)
}
