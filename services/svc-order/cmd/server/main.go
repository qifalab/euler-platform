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
//	POST   /api/v1/orders/{id}/pay    — mark order paid (→ triggers orchestrator in real deploy)
//	POST   /api/v1/orders/{id}/cancel — cancel a pending order
//	GET    /api/v1/orders/autorenew   — list the account's auto-renew settings
//	PUT    /api/v1/orders/autorenew   — set/clear a resource's auto-renew flag
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

// minorToAmount converts minor units (分) to pricing.Amount (micro-yuan),
// matching svc-payment's conversion: 1 分 = 10_000 micro-yuan.
func minorToAmount(minor int64) pricing.Amount {
	return pricing.Amount(minor) * 10_000
}

type orderStore struct {
	mu      sync.RWMutex
	orders  map[int64]*order.Order
	byToken map[string]int64 // "accountID|clientToken" → orderID (idempotency index)
	// autoRenews holds the account's auto-renew settings, keyed
	// "accountID|resourceId". Auto-renew is a trade-plane concern — when the
	// renewal scheduler fires it issues a RENEW order through this same model —
	// so the flag lives with orders, not on the resource plane.
	autoRenews map[string]bool
	machine    *order.Machine
	seq        int64
}

func newOrderStore() *orderStore {
	s := &orderStore{
		orders:     make(map[int64]*order.Order),
		byToken:    make(map[string]int64),
		autoRenews: make(map[string]bool),
		machine:    order.NewMachine(time.Now),
	}
	s.seed()
	return s
}

func (s *orderStore) seed() {
	// Seed one pending-payment order (matches console-bff seed for account 100123).
	o := &order.Order{
		OrderID: 9001, OrderNo: "SO202608110001", AccountID: 100123,
		Type: order.TypeNew, State: order.StatePendingPayment,
		ProductCode: "scecs", PayableAmount: pricing.MustParseAmount("2160"),
		CreatedAt: time.Now().Add(-2 * time.Hour), UpdatedAt: time.Now(), Version: 1,
	}
	s.orders[9001] = o
	s.seq = 9001
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
	if req.AmountMinor < 0 {
		writeErr(w, "Common.InvalidParameter", 400, "amountMinor must be non-negative")
		return
	}
	if req.Quantity == 0 {
		req.Quantity = 1
	}
	if req.Duration == 0 {
		req.Duration = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Idempotency: a retried create with the same clientToken returns the
	// existing order instead of creating a duplicate.
	if req.ClientToken != "" {
		if existingID, hit := s.byToken[tokenKey(acct, req.ClientToken)]; hit {
			if existing, ok := s.orders[existingID]; ok {
				writeJSON(w, "OK", orderToMap(existing))
				return
			}
		}
	}
	s.seq++
	orderID := s.seq
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
	amount := minorToAmount(req.AmountMinor)
	o, _, err := s.machine.Create(order.CreateRequest{
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
	s.orders[orderID] = o
	s.byToken[tokenKey(acct, clientToken)] = orderID
	writeJSON(w, "OK", orderToMap(o))
}

func tokenKey(acct int64, token string) string {
	return fmt.Sprintf("%d|%s", acct, token)
}

func (s *orderStore) handleList(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0)
	for _, o := range s.orders {
		if o.AccountID == acct {
			out = append(out, orderToMap(o))
		}
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, exists := s.orders[id]
	if !exists || o.AccountID != acct {
		writeErr(w, "Order.NotFound", 404, "订单不存在")
		return
	}
	writeJSON(w, "OK", orderToMap(o))
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
	s.mu.Lock()
	defer s.mu.Unlock()
	o, exists := s.orders[id]
	if !exists || o.AccountID != acct {
		writeErr(w, "Order.NotFound", 404, "订单不存在")
		return
	}
	// MarkPaid enforces the paid-amount check against the order's payable
	// amount before transitioning (Transition itself bumps Version).
	if _, err := s.machine.MarkPaid(o, o.PayableAmount, o.Version); err != nil {
		slog.Warn("order pay transition failed", "orderId", o.OrderID, "err", err)
		writeErr(w, "Order.StateTransitionFailed", 409, "order state transition failed")
		return
	}
	// Real deploy: emit order.created event → svc-orchestrator fulfills.
	// Phase-1: the orchestrator's /fulfill endpoint is called by the frontend
	// or a webhook; here we just transition to PAID.
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
	s.mu.Lock()
	defer s.mu.Unlock()
	o, exists := s.orders[id]
	if !exists || o.AccountID != acct {
		writeErr(w, "Order.NotFound", 404, "订单不存在")
		return
	}
	if _, err := s.machine.Transition(o, order.StateCancelled, o.Version); err != nil {
		slog.Warn("order cancel transition failed", "orderId", o.OrderID, "err", err)
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

func autoRenewKey(acct int64, resourceID string) string {
	return fmt.Sprintf("%d|%s", acct, resourceID)
}

// handleAutoRenewList implements GET /api/v1/orders/autorenew: every setting the
// account has ever touched (enabled or not), so the console's renewal page can
// render the persisted state instead of a client-side guess.
func (s *orderStore) handleAutoRenewList(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]autoRenewSetting, 0, len(s.autoRenews))
	for k, enabled := range s.autoRenews {
		// key layout: "accountID|resourceId|productCode"
		parts := strings.SplitN(k, "|", 3)
		if len(parts) != 3 || parts[0] != strconv.FormatInt(acct, 10) {
			continue
		}
		out = append(out, autoRenewSetting{ResourceID: parts[1], ProductCode: parts[2], Enabled: enabled})
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
	s.mu.Lock()
	key := autoRenewKey(acct, req.ResourceID) + "|" + req.ProductCode
	if *req.Enabled {
		s.autoRenews[key] = true
	} else {
		delete(s.autoRenews, key)
	}
	s.mu.Unlock()
	writeJSON(w, "OK", autoRenewSetting{ResourceID: req.ResourceID, ProductCode: req.ProductCode, Enabled: *req.Enabled})
}

func orderToMap(o *order.Order) map[string]any {
	return map[string]any{
		"orderId": o.OrderID, "orderNo": o.OrderNo, "type": string(o.Type),
		"state": string(o.State), "productCode": o.ProductCode,
		"payableAmount": o.PayableAmount.String(), "createdAt": o.CreatedAt.Format(time.RFC3339),
		"version": o.Version,
	}
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

	store := newOrderStore()
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
