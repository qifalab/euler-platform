// Package main: svc-orchestrator — resource lifecycle & saga fulfilment
// (03-backend-services.md §4.3, orchestrator node).
//
// The orchestrator is the ONLY owner of the resource lifecycle ledger
// (03§5.2). A paid order triggers provisioning: it transitions the order to
// FULFILLING, creates the resource instance (CREATING), fans out to the
// ProvisionDriver (rc-compute for SCECS), and on success moves both to
// COMPLETED/RUNNING (billing starts at RUNNING per the D8 invariant).
//
// Phase-1 keeps an in-memory lifecycle ledger + an in-process provision driver
// (the real fan-out is gRPC to rc-* controllers; here we use the
// pkg-go/provision MockDriver so the loop runs end-to-end). The DDL in sql/
// is the production shape.
//
// Routes (gateway-authorized, X-Sc-Account-Id injected):
//
//	POST /api/v1/orchestrator/fulfill          — fulfil a paid order (saga)
//	GET  /api/v1/orchestrator/resources         — list the account's resources
//	GET  /api/v1/orchestrator/resources/{id}    — resource detail + lifecycle
//	POST /api/v1/orchestrator/resources/{id}/release — release a resource
//
// stdlib-HTTP service (repo convention). Envelope {RequestId,Code,Data} (03§9.3).
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
	"sync"
	"syscall"
	"time"

	"github.com/starcloud/sc-platform/order"
	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/provision"
	"github.com/starcloud/sc-platform/resource"
)

// accountIDHeader is injected by the API gateway after authentication. The
// service TRUSTS this header: it must only be reachable from the gateway /
// internal network (see internalTokenMiddleware for the optional shared-secret
// check), never exposed directly to the public internet.
const accountIDHeader = "X-Sc-Account-Id"

// maxBodyBytes caps JSON request bodies (defense against oversized payloads).
const maxBodyBytes = 1 << 20 // 1 MiB

// lifecycleStore is the in-memory resource lifecycle ledger + order mirror.
// In production this is MySQL (sharded by account_id) + the order service.
type lifecycleStore struct {
	mu        sync.RWMutex
	resources map[string]*resource.Instance // ResourceID → instance
	orders    map[int64]*order.Order        // OrderID → order (mirror for fulfilment)
	orderRes  map[int64]string              // OrderID → ResourceID (fulfilment idempotency)
	resSeq    int
	// driver is the service-level provision driver singleton (created once at
	// startup; MockDriver is internally synchronized, so it is shared safely
	// across requests without the store lock).
	driver provision.Driver
}

func newStore() *lifecycleStore {
	s := &lifecycleStore{
		resources: make(map[string]*resource.Instance),
		orders:    make(map[int64]*order.Order),
		orderRes:  make(map[int64]string),
		driver:    provision.NewMockDriver(time.Now),
	}
	s.seed()
	return s
}

func (s *lifecycleStore) seed() {
	// Seed one running instance (matches console-bff seed for account 100123).
	now := time.Now()
	inst := &resource.Instance{
		ResourceID: "scecs-cn-north-1-01-a1b2c3d4", AccountID: 100123,
		ProductCode: "scecs", Region: "cn-north-1", ChargeType: resource.ChargePrepaid,
		State: resource.StateRunning, SpecCode: "scecs.s2.large",
		BillingStart: now.Add(-72 * time.Hour), CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now, Version: 1,
	}
	s.resources[inst.ResourceID] = inst
}

// --- fulfilment saga (03§4.3.3) ---
//
// 1. Validate order is PAID (only a paid order may provision — invariant).
// 2. Transition order PAID → FULFILLING.
// 3. Create resource instance (CREATING).
// 4. Fan out to ProvisionDriver (rc-compute). On success → RUNNING.
// 5. Transition order FULFILLING → COMPLETED.
// Compensation (on driver failure): reverse — mark resource CREATE_FAILED,
// order → REFUNDING. Compensation runs in reverse order (workflow invariant).

type fulfillRequest struct {
	OrderID    int64  `json:"orderId"`
	OrderNo    string `json:"orderNo"`
	ProductCode string `json:"productCode"`
	Region     string `json:"region"`
	SpecCode   string `json:"specCode"`
	ChargeType string `json:"chargeType"` // PREPAID | POSTPAID
}

func (s *lifecycleStore) handleFulfill(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req fulfillRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.OrderID == 0 || req.ProductCode == "" {
		writeErr(w, "Common.InvalidParameter", 400, "orderId and productCode are required")
		return
	}

	// Phase A (under lock): validate the order, transition PAID → FULFILLING,
	// register the resource row (CREATING). The driver call happens OUTSIDE the
	// lock — a slow driver must not block every other request on the ledger.
	s.mu.Lock()

	om := order.NewMachine(time.Now)
	o, exists := s.orders[req.OrderID]
	if !exists {
		// Seed a PAID order mirror if the orchestrator hasn't seen it (real deploy:
		// the orchestrator receives the order.created event from svc-order).
		o = &order.Order{
			OrderID: req.OrderID, OrderNo: req.OrderNo, AccountID: acct,
			Type: order.TypeNew, State: order.StatePaid,
			ProductCode: req.ProductCode, PayableAmount: pricing.MustParseAmount("0"),
			CreatedAt: time.Now(), UpdatedAt: time.Now(), Version: 1,
		}
		s.orders[req.OrderID] = o
	}

	// Fulfilment idempotency: a retry for an order that already produced a
	// resource returns the existing resourceId (200), not a 409.
	if resID, dup := s.orderRes[req.OrderID]; dup {
		if inst, ok := s.resources[resID]; ok && inst.AccountID == acct {
			resp := map[string]any{
				"resourceId": resID, "state": string(inst.State),
				"orderId": o.OrderID, "orderState": string(o.State),
				"billingStart": inst.BillingStart.Format(time.RFC3339),
			}
			s.mu.Unlock()
			writeJSON(w, "OK", resp)
			return
		}
	}

	// 1. Order must be PAID (the only provisioning trigger, D8 invariant).
	if o.State != order.StatePaid {
		st := o.State
		s.mu.Unlock()
		writeErr(w, "Order.NotPaid", 409, fmt.Sprintf("订单状态 %s,仅 PAID 可履约", st))
		return
	}

	// 2. Transition order PAID → FULFILLING (Transition bumps Version itself).
	if _, err := om.Transition(o, order.StateFulfilling, o.Version); err != nil {
		s.mu.Unlock()
		slog.Error("order transition to FULFILLING failed", "orderId", req.OrderID, "err", err)
		writeErr(w, "Order.StateTransitionFailed", 409, "订单状态流转失败")
		return
	}

	// 3. Create resource instance (CREATING).
	s.resSeq++
	resID := fmt.Sprintf("%s-%s-%02d-%08d", req.ProductCode, req.Region, s.resSeq, req.OrderID)
	ct := resource.ChargePrepaid
	if req.ChargeType == "POSTPAID" {
		ct = resource.ChargePostpaid
	}
	inst := &resource.Instance{
		ResourceID: resID, AccountID: acct, ProductCode: req.ProductCode,
		Region: req.Region, ChargeType: ct, State: resource.StateCreating,
		SpecCode: req.SpecCode, CreatedAt: time.Now(), UpdatedAt: time.Now(), Version: 1,
	}
	s.resources[resID] = inst
	s.orderRes[req.OrderID] = resID
	s.mu.Unlock()

	// 4. Fan out to the ProvisionDriver (service-level singleton; MockDriver in
	// dev, rc-compute via gRPC in production). Runs outside the ledger lock.
	spec := provision.Spec{
		ResourceID: resID, AccountID: acct, ProductCode: req.ProductCode,
		ResourceType: "instance", Region: req.Region, IdempotencyKey: strconv.FormatInt(req.OrderID, 10),
	}
	status, err := s.driver.Apply(spec)

	// Phase B (re-lock): write the outcome back to the ledger.
	s.mu.Lock()
	defer s.mu.Unlock()
	rm := resource.NewMachine(time.Now)
	if err != nil || !status.Ready() {
		// Compensation (reverse order): mark resource failed, order → REFUNDING.
		if _, terr := rm.Transition(inst, resource.StateCreateFailed, inst.Version, "provision failed"); terr != nil {
			slog.Error("resource transition to CREATE_FAILED failed", "resourceId", resID, "err", terr)
		}
		if _, terr := om.Transition(o, order.StateRefunding, o.Version); terr != nil {
			slog.Error("order transition to REFUNDING failed", "orderId", req.OrderID, "err", terr)
		}
		slog.Error("provision failed, compensation started", "orderId", req.OrderID, "resourceId", resID, "err", err)
		writeErr(w, "Provision.Failed", 500, "开通失败,已进入退款")
		return
	}
	if _, terr := rm.Transition(inst, resource.StateRunning, inst.Version, "provisioned"); terr != nil {
		slog.Error("resource transition to RUNNING failed", "resourceId", resID, "err", terr)
		writeErr(w, "Resource.StateTransitionFailed", 409, "资源状态流转失败")
		return
	}
	inst.BillingStart = time.Now() // billing starts at RUNNING, never resets (D8)

	// 5. Transition order FULFILLING → COMPLETED.
	if _, terr := om.Transition(o, order.StateCompleted, o.Version); terr != nil {
		slog.Error("order transition to COMPLETED failed", "orderId", req.OrderID, "err", terr)
		writeErr(w, "Order.StateTransitionFailed", 409, "订单状态流转失败")
		return
	}

	writeJSON(w, "OK", map[string]any{
		"resourceId": resID, "state": string(inst.State),
		"orderId": o.OrderID, "orderState": string(o.State),
		"billingStart": inst.BillingStart.Format(time.RFC3339),
	})
}

func (s *lifecycleStore) handleListResources(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0)
	for _, inst := range s.resources {
		if inst.AccountID == acct {
			out = append(out, instanceToMap(inst))
		}
	}
	writeJSON(w, "OK", out)
}

func (s *lifecycleStore) handleResourceDetail(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	resID := r.PathValue("id")
	inst, exists := s.resources[resID]
	if !exists || inst.AccountID != acct {
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return
	}
	writeJSON(w, "OK", instanceToMap(inst))
}

func (s *lifecycleStore) handleRelease(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	// Phase A (under lock): legal transition into RELEASING.
	s.mu.Lock()
	resID := r.PathValue("id")
	inst, exists := s.resources[resID]
	if !exists || inst.AccountID != acct {
		s.mu.Unlock()
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return
	}
	m := resource.NewMachine(time.Now)
	if _, err := m.Transition(inst, resource.StateReleasing, inst.Version, "user release"); err != nil {
		s.mu.Unlock()
		slog.Error("resource transition to RELEASING failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.StateTransitionFailed", 409, "资源状态流转失败")
		return
	}
	s.mu.Unlock()

	// Reclaim via the driver outside the ledger lock (Delete is idempotent).
	if err := s.driver.Delete(resID); err != nil {
		slog.Error("driver delete failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.ReleaseFailed", 500, "资源回收失败,请重试")
		return
	}

	// Phase B: legal transition RELEASING → RELEASED.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := m.Transition(inst, resource.StateReleased, inst.Version, "released"); err != nil {
		slog.Error("resource transition to RELEASED failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.StateTransitionFailed", 409, "资源状态流转失败")
		return
	}
	writeJSON(w, "OK", map[string]any{"resourceId": resID, "state": string(inst.State)})
}

// --- helpers ---

func instanceToMap(i *resource.Instance) map[string]any {
	return map[string]any{
		"resourceId": i.ResourceID, "productCode": i.ProductCode, "region": i.Region,
		"chargeType": string(i.ChargeType), "state": string(i.State), "specCode": i.SpecCode,
		"billingStart": i.BillingStart.Format(time.RFC3339),
		"createdAt": i.CreatedAt.Format(time.RFC3339), "version": i.Version,
	}
}

func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	// TRUST NOTE: X-Sc-Account-Id is injected by the API gateway after
	// authentication; this service relies on network isolation (and optionally
	// internalTokenMiddleware) rather than re-authenticating.
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
			id = fmt.Sprintf("orch-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Sc-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

// internalTokenMiddleware optionally enforces an internal shared secret: when
// the SC_INTERNAL_TOKEN env var is set, every request must carry a matching
// X-Sc-Internal-Token header (defense-in-depth for the gateway-injected
// X-Sc-Account-Id trust). Unset (dev default) = no check.
func internalTokenMiddleware(h http.Handler) http.Handler {
	token := os.Getenv("SC_INTERNAL_TOKEN")
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Sc-Internal-Token") != token {
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
	addr := flag.String("http", ":9203", "HTTP listen address")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	store := newStore()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/orchestrator/fulfill", store.handleFulfill)
	mux.HandleFunc("GET /api/v1/orchestrator/resources", store.handleListResources)
	mux.HandleFunc("GET /api/v1/orchestrator/resources/{id}", store.handleResourceDetail)
	mux.HandleFunc("POST /api/v1/orchestrator/resources/{id}/release", store.handleRelease)

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(internalTokenMiddleware(mux))), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("svc-orchestrator listening", "addr", *addr)
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
