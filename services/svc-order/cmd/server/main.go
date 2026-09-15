// Package main: svc-order — order center (03-backend-services.md §4.2.2, 五种交易类型一个模型).
//
// Owns the unified order model + state machine (pkg-go/order). A paid order is
// the ONLY trigger for provisioning (svc-orchestrator fans out on PAID).
// Five transaction types share one model: NEW / RENEW / UPGRADE / DOWNGRADE / REFUND.
//
// Routes (gateway-authorized, X-Sc-Account-Id injected):
//
//	POST   /api/v1/orders             — create an order (TypeNew/RENEW/UPGRADE/...)
//	GET    /api/v1/orders             — list the account's orders
//	GET    /api/v1/orders/{id}        — order detail
//	POST   /api/v1/orders/{id}/pay    — payment callback: records the payment
//	                                    reference + paid amount (which must equal
//	                                    the payable) and marks the order PAID
//	POST   /api/v1/orders/{id}/cancel — cancel a pending order
//	GET    /api/v1/orders/autorenew   — list the account's auto-renew settings
//	PUT    /api/v1/orders/autorenew   — set/clear a resource's auto-renew flag
//	POST   /api/v1/trial/claim        — 免费试用领取 (B7; gated by pkg-go/trial)
//	GET    /api/v1/trial/status       — 试用资格预检 + 未消耗试用券
//
// stdlib-HTTP service. Envelope {RequestId,Code,Data} (03§9.3). In-memory store
// (MySQL sharded by account_id in production).
package main

import (
	"context"
	"crypto/subtle"
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

	"github.com/starcloud/sc-platform/order"
	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/trial"
)

// accountIDHeader carries the caller's account id. TRUST NOTE: this header is
// injected by the API gateway after authentication; this service trusts it and
// must therefore never be exposed directly to the public network. Deployments
// that want defense-in-depth set SC_INTERNAL_TOKEN, which makes the service
// additionally require a matching X-Sc-Internal-Token shared-secret header
// (dev default: unset, check disabled).
const accountIDHeader = "X-Sc-Account-Id"

const internalTokenHeader = "X-Sc-Internal-Token"

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
// The repo decides where orders live: trade_db when SC_DB_DSN is set, the
// in-memory store otherwise. Handlers hold no locks — the repo owns its own
// synchronisation, which is also what lets the SQL implementation serialise
// transitions with row locks instead of a process mutex.
type orderStore struct {
	repo    orderRepo
	machine *order.Machine
}

func newOrderStoreWith(repo orderRepo) *orderStore {
	return &orderStore{repo: repo, machine: order.NewMachine(time.Now)}
}

// newInMemoryOrderStore builds the demo store, seeded to match console-bff.
// Handler tests construct it directly, so they never depend on whether the
// developer's shell has SC_DB_DSN set.
func newInMemoryOrderStore() *orderStore {
	s := newOrderStoreWith(newMemOrderRepo())
	s.seed()
	return s
}

func (s *orderStore) seed() {
	// Seed one pending-payment order (matches console-bff seed for account 100123).
	o, evt, err := s.machine.Create(order.CreateRequest{
		AccountID: 100123, Type: order.TypeNew, ProductCode: "scecs",
		ChargeType: pricing.ChargePrepaid, SKUCode: "scecs.c1", RegionID: "cn-north-1",
		Quote: pricing.Result{PayableAmount: pricing.MustParseAmount("2160")},
		ClientToken: "seed-100123-9001",
	}, 9001, "SO202608110001")
	if err != nil {
		panic(fmt.Sprintf("seed order failed: %v", err))
	}
	if err := s.repo.Create(o, evt); err != nil {
		panic(fmt.Sprintf("seed order failed: %v", err))
	}
}

type createOrderRequest struct {
	Type        string `json:"type"`         // NEW / RENEW / UPGRADE / DOWNGRADE / REFUND
	ProductCode string `json:"productCode"`
	SKUCode     string `json:"skuCode"`
	RegionID    string `json:"regionId"`
	Quantity    int64  `json:"quantity"`
	Duration    int64  `json:"duration"`
	AmountMinor int64  `json:"amountMinor"` // 分 (minor units); dev: client-supplied quote (prod: svc-catalog)
	ClientToken string `json:"clientToken"` // idempotency token; generated server-side if absent
}

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
	// Conversion (with its bound check) happens up front: a wrapped amount must
	// be rejected before an order row exists.
	amount, err := minorToAmount(req.AmountMinor)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "amountMinor out of range")
		return
	}
	if req.Quantity == 0 {
		req.Quantity = 1
	}
	if req.Duration == 0 {
		req.Duration = 1
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
	orderID := s.repo.NewOrderID()
	orderNo := fmt.Sprintf("SO%d%06d", time.Now().Year(), orderID)
	ot := order.Type(req.Type)
	if ot == "" {
		ot = order.TypeNew
	}
	clientToken := req.ClientToken
	if clientToken == "" {
		// No client-supplied token: mint a server-side one (non-idempotent path).
		clientToken = fmt.Sprintf("ct-%d-%d-%d", acct, orderID, time.Now().UnixNano())
	}
	o, evt, err := s.machine.Create(order.CreateRequest{
		AccountID: acct, Type: ot, ProductCode: req.ProductCode,
		ChargeType: pricing.ChargePrepaid,
		SKUCode: req.SKUCode, RegionID: req.RegionID, Quantity: req.Quantity,
		Duration: req.Duration, DurationUnit: pricing.DurationMonth,
		Quote: pricing.Result{PayableAmount: amount},
		ClientToken: clientToken,
	}, orderID, orderNo)
	if err != nil {
		slog.Error("order create failed", "account", acct, "err", err)
		writeErr(w, "Order.CreateFailed", 400, "order create failed")
		return
	}
	// The event rides the same transaction as the order row (03§8.2): a create
	// with no event is an order the fulfilment chain can never see.
	if err := s.repo.Create(o, evt); err != nil {
		// Lost the uk_client_token race to a concurrent duplicate: serve the
		// winner's order rather than a 500.
		if existing, gerr := s.repo.ByClientToken(acct, clientToken); gerr == nil && existing != nil {
			writeJSON(w, "OK", orderToMap(existing))
			return
		}
		slog.Error("order persist failed", "account", acct, "orderId", orderID, "err", err)
		writeErr(w, "Order.CreateFailed", 500, "order create failed")
		return
	}
	writeJSON(w, "OK", orderToMap(o))
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

// payRequest is the POST /api/v1/orders/{id}/pay body.
//
// This endpoint is the payment service's callback, not a customer-facing
// "pay" button: it must carry the payment-side reference and the amount that
// was actually paid. Hardened deployments additionally set SC_INTERNAL_TOKEN,
// which makes the shared-secret middleware reject any caller that is not the
// payment service / gateway.
type payRequest struct {
	// PaymentID is the payment-side reference (svc-payment entry id, channel
	// transaction id). It is recorded as the payment proof and doubles as the
	// idempotency key for a callback replay.
	PaymentID string `json:"paymentId"`
	// PaidAmountMinor is the amount the caller reports as paid, in 分. It must
	// equal the order's payable amount — a callback reporting a different
	// figure is a reconciliation problem, not something to accept quietly.
	PaidAmountMinor int64 `json:"paidAmountMinor"`
}

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
	var req payRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if strings.TrimSpace(req.PaymentID) == "" {
		// Without a payment reference the transition would be an unverified
		// self-mark: nothing distinguishes it from a customer clicking "paid".
		writeErr(w, "Common.InvalidParameter", 400, "paymentId is required")
		return
	}
	paid, err := minorToAmount(req.PaidAmountMinor)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "paidAmountMinor out of range")
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
	// Idempotent replay of the SAME payment: a callback retried after a
	// network failure returns the order as it stands.
	if prior, has, err := s.repo.PaymentRef(acct, id); err != nil {
		slog.Error("payment ref lookup failed", "orderId", id, "err", err)
		writeErr(w, "Order.LookupFailed", 500, "order lookup failed")
		return
	} else if has {
		if prior == req.PaymentID {
			writeJSON(w, "OK", orderToMap(o))
			return
		}
		// A second, different payment against an already-settled order is a
		// double payment, not a retry — refuse it before any state change.
		writeErr(w, "Order.AlreadyPaid", 409, "order already has a recorded payment")
		return
	}
	// MarkPaid enforces the paid-amount check against the order's payable
	// amount before transitioning (Transition itself bumps Version).
	from, prevVersion := o.State, o.Version
	evt, err := s.machine.MarkPaid(o, paid, o.Version)
	if err != nil {
		slog.Warn("order pay transition failed", "orderId", o.OrderID, "err", err)
		writeErr(w, "Order.StateTransitionFailed", 409, "order state transition failed")
		return
	}
	// State, journal, event and payment proof commit as one unit: a crash
	// between them would leave a PAID order no relay will ever see, or a
	// callback whose replay 409s instead of returning the order.
	if err := s.repo.SaveTransition(o, from, prevVersion, "payment "+req.PaymentID, evt, req.PaymentID); err != nil {
		slog.Error("order persist failed", "orderId", o.OrderID, "err", err)
		writeErr(w, "Order.StateTransitionFailed", 409, "order state transition failed")
		return
	}
	writeJSON(w, "OK", orderToMap(o))
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
		slog.Warn("order cancel transition failed", "orderId", o.OrderID, "err", err)
		writeErr(w, "Order.StateTransitionFailed", 409, "order state transition failed")
		return
	}
	if err := s.repo.SaveTransition(o, from, prevVersion, "cancelled by customer", evt, ""); err != nil {
		slog.Error("order persist failed", "orderId", o.OrderID, "err", err)
		writeErr(w, "Order.StateTransitionFailed", 409, "order state transition failed")
		return
	}
	writeJSON(w, "OK", orderToMap(o))
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
		"payableAmount": o.PayableAmount.String(), "createdAt": o.CreatedAt.Format(time.RFC3339),
		"version": o.Version,
	}
}

// --- free trial (B7, 09-roadmap §4.4) ---------------------------------------
//
// A trial is a voucher claimed through the standard order flow (decision D7):
// 领取 mints a TRIAL-sourced VOUCHER coupon; using it goes through the same
// 询价 → 下单 → 支付 pipeline as any purchase. The claim itself is gated by
// pkg-go/trial — the anti-薅 engine. This service owns the state the engine
// reads: per-account claim history (t_trial_record), the cross-account identity
// registry (t_trial_identity), and the activity budget (t_trial_activity).
//
// In production the three counters update in one transaction with the coupon
// insert; here they are one mutex away from each other, which is the in-memory
// rendering of the same invariant.

// trialFaceValue is the default activity's voucher face value, matching the
// t_trial_activity seed (100 元). The real deploy reads it from the activity
// row selected by activityId.
var trialFaceValue = pricing.MustParseAmount("100")

type trialStore struct {
	mu sync.Mutex
	// policy mirrors t_trial_activity.policy_json for the default activity.
	policy trial.Policy
	// accounts is the dev-side account registry: real-name status and identity
	// key come from svc-iam in production; here they are seeded.
	accounts map[int64]trial.AccountState
	// identityCounts is t_trial_identity: identityKey → total claims across
	// accounts. The one structure that can stop "register N accounts, claim N
	// vouchers with one ID card".
	identityCounts map[string]int64
	// globalActive is t_trial_activity.active_count: platform-wide live
	// vouchers — the activity budget expressed in vouchers, not yuan.
	globalActive int64
	// coupons holds issued trial vouchers per account (t_coupon, source=TRIAL).
	coupons  map[int64][]pricing.Coupon
	couponSeq int64
}

func newTrialStore() *trialStore {
	s := &trialStore{
		policy:         trial.DefaultPolicy(),
		accounts:       make(map[int64]trial.AccountState),
		identityCounts: make(map[string]int64),
		coupons:        make(map[int64][]pricing.Coupon),
	}
	// Dev seed mirrors web-auth/console-bff: account 100123 is the verified
	// seed tenant. Identity key is the SHA-256 of the doc number in production;
	// the raw document never reaches this service.
	s.accounts[100123] = trial.AccountState{
		RealNameVerified: true,
		IdentityKey:      "idn-8f3a1c2d",
	}
	return s
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
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	v := trial.Admit(s.policy, trial.State{
		Account:        s.accounts[acct],
		GlobalActive:   s.globalActive,
		IdentityCounts: s.identityCounts,
	}, now)
	if !v.Allowed {
		writeErr(w, string(v.Rule), 409, v.Detail)
		return
	}

	// Admitted: mint the voucher and update every counter the engine will read
	// on the next claim. All four writes are one transaction in production
	// (t_coupon insert + t_trial_record insert + t_trial_activity.active_count
	// bump + t_trial_identity upsert).
	s.couponSeq++
	couponID := fmt.Sprintf("TC%d", s.couponSeq)
	face := trialFaceValue
	expireAt := now.Add(trial.VoucherValidDays * 24 * time.Hour)
	s.coupons[acct] = append(s.coupons[acct], pricing.Coupon{
		CouponID: couponID, AccountID: acct, Kind: pricing.KindVoucher,
		FaceValue: face, RemainValue: face, ExpireAt: expireAt,
	})

	acctState := s.accounts[acct]
	acctState.Active++
	acctState.Lifetime++
	acctState.LastClaimedAt = now
	s.accounts[acct] = acctState
	if k := acctState.IdentityKey; k != "" {
		s.identityCounts[k]++
	}
	s.globalActive++

	slog.Info("trial voucher issued", "account", acct, "coupon", couponID)
	writeJSON(w, "OK", map[string]any{
		"couponId": couponID, "kind": string(pricing.KindVoucher),
		"faceValue": face.String(), "remainValue": face.String(),
		"expireAt": expireAt.Format(time.RFC3339), "validDays": trial.VoucherValidDays,
	})
}

// handleTrialStatus implements GET /api/v1/trial/status — the console's 试用中心
// page: claim eligibility preview plus the account's outstanding vouchers.
func (s *trialStore) handleTrialStatus(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	v := trial.Admit(s.policy, trial.State{
		Account:        s.accounts[acct],
		GlobalActive:   s.globalActive,
		IdentityCounts: s.identityCounts,
	}, now)
	out := make([]map[string]any, 0, len(s.coupons[acct]))
	for _, c := range s.coupons[acct] {
		out = append(out, map[string]any{
			"couponId": c.CouponID, "faceValue": c.FaceValue.String(),
			"remainValue": c.RemainValue.String(), "expireAt": c.ExpireAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, "OK", map[string]any{
		"eligible": v.Allowed,
		"reason":   string(v.Rule),
		"reasonDetail": v.Detail,
		"coupons":  out,
	})
}

func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.Header.Get(accountIDHeader)
	if raw == "" {
		writeErr(w, "Common.MissingAccountId", 403, "X-Sc-Account-Id header is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed X-Sc-Account-Id")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, code string, data any) {
	rid := w.Header().Get("X-Sc-TraceId")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Data": data})
}

func writeErr(w http.ResponseWriter, code string, status int, msg string) {
	rid := w.Header().Get("X-Sc-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Message": msg})
}

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Sc-TraceId")
		if id == "" {
			id = fmt.Sprintf("order-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Sc-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

// internalTokenMiddleware optionally enforces a shared-secret header for
// service-to-service calls. When SC_INTERNAL_TOKEN is set, every request must
// carry a matching X-Sc-Internal-Token; unset (dev default) the check is off.
// This complements — not replaces — the gateway trust on X-Sc-Account-Id.
func internalTokenMiddleware(h http.Handler) http.Handler {
	token := os.Getenv("SC_INTERNAL_TOKEN")
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(internalTokenHeader)), []byte(token)) != 1 {
			writeErr(w, "Common.Forbidden", 403, "invalid internal token")
			return
		}
		h.ServeHTTP(w, r)
	})
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
	trials := newTrialStore()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/orders", store.handleCreate)
	mux.HandleFunc("GET /api/v1/orders", store.handleList)
	mux.HandleFunc("GET /api/v1/orders/{id}", store.handleDetail)
	mux.HandleFunc("POST /api/v1/orders/{id}/pay", store.handlePay)
	mux.HandleFunc("POST /api/v1/orders/{id}/cancel", store.handleCancel)
	// Static segment beats {id} in Go 1.22 mux precedence, so "autorenew" is
	// routed here and not swallowed by the {id} detail route.
	mux.HandleFunc("GET /api/v1/orders/autorenew", store.handleAutoRenewList)
	mux.HandleFunc("PUT /api/v1/orders/autorenew", store.handleAutoRenewSet)
	// B7 免费试用: claim is gated by pkg-go/trial; the voucher itself spends
	// through the standard order flow above (decision D7).
	mux.HandleFunc("POST /api/v1/trial/claim", trials.handleTrialClaim)
	mux.HandleFunc("GET /api/v1/trial/status", trials.handleTrialStatus)

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
