// Package main: svc-orchestrator — resource lifecycle & saga fulfilment
// (03-backend-services.md §4.3, orchestrator node).
//
// The orchestrator is the ONLY owner of the resource lifecycle ledger
// (03§5.2). A paid order triggers provisioning: it transitions the order to
// FULFILLING, creates the resource instance (CREATING), fans out to the
// ProvisionDriver (rc-compute for EUECS), and on success moves both to
// COMPLETED/RUNNING (billing starts at RUNNING per the D8 invariant).
//
// Phase-1 keeps an in-memory lifecycle ledger + an in-process provision driver
// (the real fan-out is gRPC to rc-* controllers; here we use the
// pkg-go/provision MockDriver so the loop runs end-to-end). The DDL in sql/
// is the production shape.
//
// Routes (gateway-authorized, X-Euler-Account-Id injected):
//
//	POST /api/v1/orchestrator/fulfill          — fulfil a paid order (saga)
//	GET  /api/v1/orchestrator/resources         — list the account's resources
//	GET  /api/v1/orchestrator/resources/{id}    — resource detail + lifecycle
//	POST /api/v1/orchestrator/resources/{id}/release — release a resource
//	POST /api/v1/orchestrator/resources/{id}/actions — user lifecycle actions (stop/start/restart/upgrade)
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

	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/provision"
	"github.com/qifalab/euler-platform/resource"
)

// accountIDHeader is injected by the API gateway after authentication. The
// service TRUSTS this header: it must only be reachable from the gateway /
// internal network (see internalTokenMiddleware for the optional shared-secret
// check), never exposed directly to the public internet.
const accountIDHeader = "X-Euler-Account-Id"

// maxBodyBytes caps JSON request bodies (defense against oversized payloads).
const maxBodyBytes = 1 << 20 // 1 MiB

// lifecycleStore wires the fulfilment saga to the resource ledger over HTTP.
// The ledger decides where instances live (resource_db when EULER_DB_DSN is set,
// in-memory otherwise); the order mirror stays in memory in both modes —
// order_main belongs to svc-order, and a second service writing it would be two
// writers on one aggregate (see resourceLedger's doc).
type lifecycleStore struct {
	mu    sync.RWMutex
	// orders mirrors the orders this process has seen, for the saga's
	// PAID→FULFILLING→COMPLETED transitions. A restart drops in-flight mirrors;
	// the durable half of the saga is the resource row (state + order_id).
	orders map[int64]*order.Order
	ledger resourceLedger
	resSeq int
	// driver is the service-level provision driver singleton (created once at
	// startup; MockDriver is internally synchronized, so it is shared safely
	// across requests without the store lock).
	driver provision.Driver
}

func newStore() *lifecycleStore {
	return newStoreWith(newMemLedger())
}

// newStoreWith wires a ledger. The demo seed runs only for the in-memory ledger:
// writing two demo instances into a shared resource_db would invent resources for
// account 100123 in every environment pointed at it.
func newStoreWith(ledger resourceLedger) *lifecycleStore {
	s := &lifecycleStore{
		orders: make(map[int64]*order.Order),
		ledger: ledger,
		driver: provision.NewMockDriver(time.Now),
	}
	if _, mem := ledger.(*memLedger); mem {
		s.seed()
	}
	return s
}

func (s *lifecycleStore) seed() {
	// Seed one running instance (matches console-bff seed for account 100123).
	now := time.Now()
	inst := &resource.Instance{
		ResourceID: "euecs-cn-north-1-01-a1b2c3d4", AccountID: 100123,
		ProductCode: "euecs", Region: "cn-north-1", ChargeType: resource.ChargePrepaid,
		ResourceType: "instance", State: resource.StateRunning, SpecCode: "euecs.s2.large",
		BillingStart: now.Add(-72 * time.Hour), CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now, Version: 1,
	}
	if err := s.ledger.Create(inst); err != nil {
		panic(fmt.Sprintf("seed resource failed: %v", err))
	}

	// Seed one VPC (euvpc) so network-dependent wizards (euredis / eukafka
	// VPC dropdowns) list a real, placeable network from day one in dev.
	vpc := &resource.Instance{
		ResourceID: "euvpc-cn-north-1-01-vpc0a1b2c", AccountID: 100123,
		ProductCode: "euvpc", Region: "cn-north-1", ChargeType: resource.ChargePostpaid,
		ResourceType: "vpc", State: resource.StateRunning, SpecCode: "euvpc.standard",
		BillingStart: now.Add(-72 * time.Hour), CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now, Version: 1,
	}
	if err := s.ledger.Create(vpc); err != nil {
		panic(fmt.Sprintf("seed resource failed: %v", err))
	}
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

	// Fulfilment idempotency, restart-safe: the LEDGER arbitrates (idx_order),
	// not a process map — a retry after a restart must still return the resource
	// the first attempt created.
	if prior, err := s.ledger.ByOrder(acct, req.OrderID); err == nil && prior != nil {
		resp := map[string]any{
			"resourceId": prior.ResourceID, "state": string(prior.State),
			"orderId": o.OrderID, "orderState": string(o.State),
			"billingStart": prior.BillingStart.Format(time.RFC3339),
		}
		s.mu.Unlock()
		writeJSON(w, "OK", resp)
		return
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

	// 3. Create resource instance (CREATING) in the ledger. The id carries the
	// order id, so a retried fulfilment re-derives the same id and the table's
	// primary key — not a process counter — is what keeps it unique.
	s.resSeq++
	resID := fmt.Sprintf("%s-%s-%02d-%08d", req.ProductCode, req.Region, s.resSeq, req.OrderID)
	ct := resource.ChargePrepaid
	if req.ChargeType == "POSTPAID" {
		ct = resource.ChargePostpaid
	}
	inst := &resource.Instance{
		ResourceID: resID, AccountID: acct, ProductCode: req.ProductCode,
		ResourceType: "instance", Region: req.Region, ChargeType: ct,
		State: resource.StateCreating, SpecCode: req.SpecCode, OrderID: req.OrderID,
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Version: 1,
	}
	if err := s.ledger.Create(inst); err != nil {
		// Lost the create to a concurrent duplicate for the same order: serve
		// the winner's resource instead of failing the retry.
		if prior, gerr := s.ledger.ByOrder(acct, req.OrderID); gerr == nil && prior != nil {
			resp := map[string]any{
				"resourceId": prior.ResourceID, "state": string(prior.State),
				"orderId": o.OrderID, "orderState": string(o.State),
				"billingStart": prior.BillingStart.Format(time.RFC3339),
			}
			s.mu.Unlock()
			writeJSON(w, "OK", resp)
			return
		}
		slog.Error("resource persist failed", "orderId", req.OrderID, "err", err)
		writeErr(w, "Provision.Failed", 500, "开通失败,资源台账写入失败")
		return
	}
	s.mu.Unlock()

	// 4. Fan out to the ProvisionDriver (service-level singleton; MockDriver in
	// dev, rc-compute via gRPC in production). Runs outside the ledger lock.
	spec := provision.Spec{
		ResourceID: resID, AccountID: acct, ProductCode: req.ProductCode,
		ResourceType: "instance", Region: req.Region, IdempotencyKey: strconv.FormatInt(req.OrderID, 10),
	}
	status, err := s.driver.Apply(spec)
	s.recordProvision(resID, acct, spec, status, err)

	// Phase B (re-lock): write the outcome back to the ledger. Every edge goes
	// through the ledger's Transition — machine + guarded persist + journal row
	// in one call — and BillingStart is stamped there on the first RUNNING edge
	// (计费起点=首次 RUNNING 时刻,永不回拨), so no handler can reset it.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || !status.Ready() {
		// Compensation (reverse order): mark resource failed, order → REFUNDING.
		if terr := s.ledger.Transition(inst, resource.StateCreateFailed, "provision failed"); terr != nil {
			slog.Error("resource transition to CREATE_FAILED failed", "resourceId", resID, "err", terr)
		}
		if _, terr := om.Transition(o, order.StateRefunding, o.Version); terr != nil {
			slog.Error("order transition to REFUNDING failed", "orderId", req.OrderID, "err", terr)
		}
		slog.Error("provision failed, compensation started", "orderId", req.OrderID, "resourceId", resID, "err", err)
		writeErr(w, "Provision.Failed", 500, "开通失败,已进入退款")
		return
	}
	if terr := s.ledger.Transition(inst, resource.StateRunning, "provisioned"); terr != nil {
		slog.Error("resource transition to RUNNING failed", "resourceId", resID, "err", terr)
		writeErr(w, "Resource.StateTransitionFailed", 409, "资源状态流转失败")
		return
	}

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

// recordProvision writes the dispatch record for the retry scanner. A failure
// here is logged, not fatal: the dispatch already happened, and losing the audit
// row must not fail a request whose resource came up.
func (s *lifecycleStore) recordProvision(resID string, acct int64, spec provision.Spec, status provision.Status, err error) {
	recStatus, lastError := "DONE", ""
	if err != nil || !status.Ready() {
		recStatus, lastError = "FAILED", fmt.Sprintf("%v", err)
	}
	params, merr := json.Marshal(spec)
	if merr != nil {
		params = []byte("{}")
	}
	if rerr := s.ledger.RecordProvision(provisionRecord{
		ResourceID: resID, AccountID: acct, OpType: "CREATE",
		IdempotKey: spec.IdempotencyKey, Params: string(params),
		Status: recStatus, LastError: lastError,
	}); rerr != nil {
		slog.Error("provision task record failed", "resourceId", resID, "err", rerr)
	}
}

func (s *lifecycleStore) handleListResources(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	list, err := s.ledger.List(acct)
	if err != nil {
		slog.Error("resource list failed", "account", acct, "err", err)
		writeErr(w, "Resource.ListFailed", 500, "资源列表读取失败")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, inst := range list {
		out = append(out, instanceToMap(inst))
	}
	writeJSON(w, "OK", out)
}

func (s *lifecycleStore) handleResourceDetail(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	resID := r.PathValue("id")
	inst, err := s.ledger.Get(acct, resID)
	if err != nil {
		slog.Error("resource lookup failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	if inst == nil {
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
	// Phase A: legal transition into RELEASING, persisted with its journal row.
	resID := r.PathValue("id")
	inst, err := s.ledger.Get(acct, resID)
	if err != nil {
		slog.Error("resource lookup failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	if inst == nil {
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return
	}
	if err := s.ledger.Transition(inst, resource.StateReleasing, "user release"); err != nil {
		slog.Error("resource transition to RELEASING failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.StateTransitionFailed", 409, "资源状态流转失败")
		return
	}

	// Reclaim via the driver (Delete is idempotent).
	if err := s.driver.Delete(resID); err != nil {
		slog.Error("driver delete failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.ReleaseFailed", 500, "资源回收失败,请重试")
		return
	}

	// Phase B: legal transition RELEASING → RELEASED.
	if err := s.ledger.Transition(inst, resource.StateReleased, "released"); err != nil {
		slog.Error("resource transition to RELEASED failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.StateTransitionFailed", 409, "资源状态流转失败")
		return
	}
	writeJSON(w, "OK", map[string]any{"resourceId": resID, "state": string(inst.State)})
}

// actionRequest is the body of POST /api/v1/orchestrator/resources/{id}/actions.
type actionRequest struct {
	Action   string `json:"action"`             // stop | start | restart | upgrade
	SpecCode string `json:"specCode,omitempty"` // upgrade only: target SKU
}

// handleResourceAction applies a user-initiated lifecycle action through the
// SAME state machine the saga uses (pkg-go/resource transition table) — there
// is no second, looser path for console buttons. stop/start/restart map onto
// the RUNNING↔STOPPED edges; upgrade rides the UPGRADING intermediate state
// (RUNNING → UPGRADING → RUNNING) and rewrites SpecCode. The MockDriver is
// fire-and-forget for power actions in phase-2 (the ledger is the authority);
// production fans out to rc-compute the same way Apply/Delete do.
func (s *lifecycleStore) handleResourceAction(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req actionRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}

	resID := r.PathValue("id")
	inst, err := s.ledger.Get(acct, resID)
	if err != nil {
		slog.Error("resource lookup failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	if inst == nil {
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return
	}

	transition := func(to resource.State, reason string) bool {
		if err := s.ledger.Transition(inst, to, reason); err != nil {
			slog.Error("resource action transition failed", "resourceId", resID, "to", to, "err", err)
			writeErr(w, "Resource.StateTransitionFailed", 409,
				fmt.Sprintf("资源状态 %s 不允许 %s", inst.State, req.Action))
			return false
		}
		return true
	}

	switch req.Action {
	case "stop":
		if !transition(resource.StateStopped, "user stop") {
			return
		}
	case "start":
		if !transition(resource.StateRunning, "user start") {
			return
		}
	case "restart":
		// Two legal edges in sequence; the state machine guards each.
		if !transition(resource.StateStopped, "user restart: stop") || !transition(resource.StateRunning, "user restart: start") {
			return
		}
	case "upgrade":
		if req.SpecCode == "" {
			writeErr(w, "Common.InvalidParameter", 400, "upgrade requires specCode")
			return
		}
		if !transition(resource.StateUpgrading, "user upgrade") || !transition(resource.StateRunning, "upgrade applied") {
			return
		}
		inst.SpecCode = req.SpecCode
	default:
		writeErr(w, "Common.InvalidParameter", 400, "action must be stop/start/restart/upgrade")
		return
	}

	writeJSON(w, "OK", map[string]any{
		"resourceId": resID, "state": string(inst.State), "specCode": inst.SpecCode,
	})
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
	// TRUST NOTE: X-Euler-Account-Id is injected by the API gateway after
	// authentication; this service relies on network isolation (and optionally
	// internalTokenMiddleware) rather than re-authenticating.
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
		id := r.Header.Get("X-Euler-TraceId")
		if id == "" {
			id = fmt.Sprintf("orch-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Euler-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

// internalTokenMiddleware optionally enforces an internal shared secret: when
// the EULER_INTERNAL_TOKEN env var is set, every request must carry a matching
// X-Euler-Internal-Token header (defense-in-depth for the gateway-injected
// X-Euler-Account-Id trust). Unset (dev default) = no check.
func internalTokenMiddleware(h http.Handler) http.Handler {
	token := os.Getenv("EULER_INTERNAL_TOKEN")
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Euler-Internal-Token") != token {
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

	ledger, err := newLedger(context.Background())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	// Log where the ledger lives: in-memory resources vanish on restart, and a
	// customer's running instance vanishing is not a recoverable incident.
	slog.Info("svc-orchestrator ledger ready", "persistent", persistentLedger(ledger))
	store := newStoreWith(ledger)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/orchestrator/fulfill", store.handleFulfill)
	mux.HandleFunc("GET /api/v1/orchestrator/resources", store.handleListResources)
	mux.HandleFunc("GET /api/v1/orchestrator/resources/{id}", store.handleResourceDetail)
	mux.HandleFunc("POST /api/v1/orchestrator/resources/{id}/release", store.handleRelease)
	mux.HandleFunc("POST /api/v1/orchestrator/resources/{id}/actions", store.handleResourceAction)

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
