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

const accountIDHeader = "X-Sc-Account-Id"

// lifecycleStore is the in-memory resource lifecycle ledger + order mirror.
// In production this is MySQL (sharded by account_id) + the order service.
type lifecycleStore struct {
	mu        sync.RWMutex
	resources map[string]*resource.Instance // ResourceID → instance
	orders    map[int64]*order.Order        // OrderID → order (mirror for fulfilment)
	resSeq    int
}

func newStore() *lifecycleStore {
	s := &lifecycleStore{
		resources: make(map[string]*resource.Instance),
		orders:    make(map[int64]*order.Order),
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.OrderID == 0 || req.ProductCode == "" {
		writeErr(w, "Common.InvalidParameter", 400, "orderId and productCode are required")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Order must be PAID (the only provisioning trigger, D8 invariant).
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
	if o.State != order.StatePaid {
		writeErr(w, "Order.NotPaid", 409, fmt.Sprintf("订单状态 %s,仅 PAID 可履约", o.State))
		return
	}

	// 2. Transition order PAID → FULFILLING.
	if _, err := om.Transition(o, order.StateFulfilling, o.Version); err != nil {
		writeErr(w, "Order.StateTransitionFailed", 409, err.Error())
		return
	}
	o.Version++

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

	// 4. Fan out to the ProvisionDriver. Phase-1 uses the in-process MockDriver
	// (rc-compute in production, via gRPC ApplyResource). The driver transitions
	// the instance CREATING → RUNNING on success.
	driver := provision.NewMockDriver(time.Now)
	spec := provision.Spec{
		ResourceID: resID, AccountID: acct, ProductCode: req.ProductCode,
		ResourceType: "instance", Region: req.Region, IdempotencyKey: strconv.FormatInt(req.OrderID, 10),
	}
	status, err := driver.Apply(spec)
	if err != nil || !status.Ready() {
		// Compensation (reverse order): mark resource failed, order → REFUNDING.
		inst.State = resource.StateCreateFailed
		om.Transition(o, order.StateRefunding, o.Version)
		o.Version++
		reason := "driver not ready"
		if err != nil {
			reason = err.Error()
		}
		writeErr(w, "Provision.Failed", 500, fmt.Sprintf("开通失败,已进入退款: %s", reason))
		return
	}
	inst.State = resource.StateRunning
	inst.BillingStart = time.Now() // billing starts at RUNNING, never resets (D8)

	// 5. Transition order FULFILLING → COMPLETED.
	om.Transition(o, order.StateCompleted, o.Version)
	o.Version++

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
	s.mu.Lock()
	defer s.mu.Unlock()
	resID := r.PathValue("id")
	inst, exists := s.resources[resID]
	if !exists || inst.AccountID != acct {
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return
	}
	m := resource.NewMachine(time.Now)
	if _, err := m.Transition(inst, resource.StateReleasing, inst.Version, "user release"); err != nil {
		writeErr(w, "Resource.StateTransitionFailed", 409, err.Error())
		return
	}
	inst.Version++
	inst.State = resource.StateReleased
	inst.UpdatedAt = time.Now()
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

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(mux)), ReadHeaderTimeout: 5 * time.Second}
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
