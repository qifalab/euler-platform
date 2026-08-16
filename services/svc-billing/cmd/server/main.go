// Command svc-billing serves the internal billing HTTP API (03-backend-services.md
// §4.2.4) using the platform's stdlib-only pattern: pkg-go domain logic behind
// plain net/http handlers, no Kratos/grpc/prometheus.
//
// Routes (all /internal/* are gateway-egress only — APISIX injects the caller
// identity; the service must not trust a client-supplied account):
//
//	GET  /internal/balance  -> ledger.Balance for the account (pkg-go/ledger)
//	POST /internal/settle   -> billing.Engine.Settle over the in-memory pool set;
//	                           returns the Charge + deductions + arrears state
//	GET  /internal/bills    -> billing.Summarize for a period
//
// Every handler reads the caller's account_id from the X-Sc-Account-Id header
// the gateway injects (07§3.3); a missing header is a 403, never a guess.
//
// /healthz, /readyz and /metrics are the probe/metric endpoints every service
// carries on Day 1 (03§2.3.4).
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

	"github.com/starcloud/sc-platform/billing"
	"github.com/starcloud/sc-platform/invoice"
	"github.com/starcloud/sc-platform/ledger"
	"github.com/starcloud/sc-platform/metering"
	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/reservepack"
)

func main() {
	var httpAddr = flag.String("http", ":8080", "HTTP listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	app := newApp()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/metrics", metrics)
	mux.HandleFunc("/internal/balance", app.handleBalance)
	mux.HandleFunc("/internal/settle", app.handleSettle)
	mux.HandleFunc("/internal/bills", app.handleBills)
	mux.HandleFunc("/internal/reservepacks", app.handleReservePacks)
	mux.HandleFunc("/internal/reservepacks/expire-sweep", app.handleReservePackSweep)
	mux.HandleFunc("/internal/invoices", app.handleInvoices)
	mux.HandleFunc("/internal/invoices/issue", app.handleInvoiceIssue)
	mux.HandleFunc("/internal/invoices/void", app.handleInvoiceVoid)
	mux.HandleFunc("/internal/cost-analysis", app.handleCostAnalysis)

	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           withMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("http server listening", "addr", *httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen error", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown: drain in-flight requests (08§6.2 preStop hook).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutdown signal received, draining")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
	slog.Info("server exited")
}

// withMiddleware wraps the mux with the cross-cutting middleware chain every
// service must apply: request-id injection (→ trace_id), structured access
// logging, and recover (03§2.3.4).
func withMiddleware(h http.Handler) http.Handler {
	return recoverMiddleware(requestIDMiddleware(loggingMiddleware(h)))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyz(w http.ResponseWriter, _ *http.Request) {
	// TODO(svc-billing): return 503 until the trade_db ledger + pool store are
	// reachable.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// TODO(svc-billing): expose RED metrics. Mandatory labels: service,
	// instance, region, env (05§7.2). FORBIDDEN as labels: account_id,
	// resource_id (high cardinality — 05§7.2).
	_, _ = w.Write([]byte("# HELP sc_service_dummy 0\n# TYPE sc_service_dummy counter\nsc_service_dummy 0\n"))
}

// ---------------------------------------------------------------------------
// App + in-memory stores
// ---------------------------------------------------------------------------

// app holds the wired pkg-go domain objects and the in-memory stores that
// stand in for the trade_db ledger and quota/pool tables until phase 2.
type app struct {
	ledger   *ledger.Ledger
	store    *memLedgerStore
	engine   *billing.Engine
	pools    *poolRegistry
	charges  *chargeRegistry
	packs     *reservepack.Ledger
	packStore *reservepack.MemoryStore
	invoices  *invoice.Book

	// settleMu serializes the settle commit phase so a concurrent duplicate
	// settle cannot double-deduct the pools, and settled caches the response
	// per idempotency key (the deterministic ChargeID) so a retried settlement
	// replays the original result (200) instead of re-charging.
	settleMu sync.Mutex
	settled  map[string]map[string]any
}

func newApp() *app {
	store := newMemLedgerStore()
	packStore := reservepack.NewMemoryStore()
	// The seq closures are shared by concurrent handlers; guard each with its
	// own mutex so ID generation is race-free.
	var (
		packMu  sync.Mutex
		packSeq int64
		invMu   sync.Mutex
		invSeq  int64
	)
	return &app{
		ledger:    ledger.New(store, time.Now, nil),
		store:     store,
		engine:    billing.NewEngine(time.Now),
		pools:     newPoolRegistry(),
		charges:   newChargeRegistry(),
		packs:     reservepack.New(packStore, time.Now, func() int64 { packMu.Lock(); defer packMu.Unlock(); packSeq++; return packSeq }),
		packStore: packStore,
		invoices:  invoice.NewBook(time.Now, func() string { invMu.Lock(); defer invMu.Unlock(); invSeq++; return fmt.Sprintf("inv-%d", invSeq) }),
		settled:   make(map[string]map[string]any),
	}
}

// maxBodyBytes caps request bodies before JSON decoding (defense against
// oversized payloads).
const maxBodyBytes = 1 << 20 // 1 MiB

// limitBody wraps the request body with http.MaxBytesReader. Call before any
// json.Decode.
func limitBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
}

// internalToken is the optional shared secret for service-to-service calls.
// When the SC_INTERNAL_TOKEN environment variable is set, every /internal/*
// request must carry a matching X-Sc-Internal-Token header. Unset (the dev
// default) disables the check.
var internalToken = os.Getenv("SC_INTERNAL_TOKEN")

// checkInternalToken enforces the optional shared-secret header. Returns false
// (and writes 403) when the token is configured and the request's header does
// not match.
func checkInternalToken(w http.ResponseWriter, r *http.Request) bool {
	if internalToken == "" {
		return true // dev default: gateway network isolation is the only guard
	}
	if r.Header.Get("X-Sc-Internal-Token") != internalToken {
		writeErr(w, http.StatusForbidden, "Forbidden", "invalid internal token")
		return false
	}
	return true
}

// accountIDFrom extracts the caller's account id from the gateway-injected
// header. A missing/blank header is 403 — the service must never fall back to
// a client-supplied account in the body (07§3.3).
//
// TRUST BOUNDARY: X-Sc-Account-Id is trusted only because /internal/* routes
// are reachable exclusively via the APISIX gateway, which strips any
// client-supplied copy and injects the authenticated account. Deployments that
// cannot guarantee network isolation should set SC_INTERNAL_TOKEN so callers
// must also present the shared X-Sc-Internal-Token secret.
func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if !checkInternalToken(w, r) {
		return 0, false
	}
	raw := strings.TrimSpace(r.Header.Get("X-Sc-Account-Id"))
	if raw == "" {
		writeErr(w, http.StatusForbidden, "Forbidden", "missing X-Sc-Account-Id header")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed X-Sc-Account-Id")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"Code": code, "Message": msg})
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// handleBalance returns the account's current cash position.
func (a *app) handleBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	bal, err := a.ledger.Balance(acct)
	if err != nil {
		slog.Error("balance lookup failed", "account", acct, "err", err)
		writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"AccountID": bal.AccountID,
		"Available": bal.Available.String(),
		"Frozen":    bal.Frozen.String(),
		"Total":     bal.Total().String(),
		"Version":   bal.Version,
	})
}

type settleRequest struct {
	AggID         string  `json:"AggId"`
	ResourceID    string  `json:"ResourceId"`
	MeteringItem  string  `json:"MeteringItem"`
	TotalQuantity string  `json:"TotalQuantity"` // decimal, e.g. "2" or "1.99998"
	HourStart     string  `json:"HourStart"`     // RFC3339
	CoveredRatio  int     `json:"CoveredRatio"`
	ProductCode   string  `json:"ProductCode"`
	UnitPrice     string  `json:"UnitPrice"` // decimal, e.g. "0.25"
	SnapshotID    string  `json:"SnapshotId"`
	PoolRefs      []string `json:"PoolRefs"` // optional refs to restrict which pools apply
}

// handleSettle charges one hourly aggregate against the account's pool set.
func (a *app) handleSettle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req settleRequest
	limitBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed settle request")
		return
	}

	hourStart, err := time.Parse(time.RFC3339, req.HourStart)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "invalid HourStart")
		return
	}
	qty, err := metering.ParseQuantity(req.TotalQuantity)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "invalid TotalQuantity")
		return
	}
	unitPrice, err := pricing.ParseAmount(req.UnitPrice)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "invalid UnitPrice")
		return
	}

	usage := metering.HourlyUsage{
		AggID:         req.AggID,
		AccountID:     acct,
		ResourceID:    req.ResourceID,
		MeteringItem:  req.MeteringItem,
		TotalQuantity: qty,
		HourStart:     hourStart,
		CoveredRatio:  req.CoveredRatio,
	}

	// Idempotency: the ChargeID derived from the aggregate is deterministic
	// (billing.ChargeID(AggID)), so it doubles as the settle idempotency key.
	// settleMu serializes the check-settle-commit sequence: a concurrent or
	// retried duplicate returns the cached original response (200) and never
	// deducts the pools or the ledger a second time.
	idemKey := fmt.Sprintf("%d|%s", acct, billing.ChargeID(req.AggID))
	a.settleMu.Lock()
	defer a.settleMu.Unlock()
	if cached, ok := a.settled[idemKey]; ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	// forAccount returns a copy, so the waterfall below never reads the shared
	// registry slice outside the registry's lock.
	pools := a.pools.forAccount(acct, req.PoolRefs)
	// Inject the account's active resource packs as waterfall pools (rank 0,
	// consumed before cash — 01§12.3). The waterfall reads Available without
	// mutating; the consume intent is committed below via Ledger.Consume.
	now := time.Now()
	packPools, err := reservepack.PoolsForAccount(a.packStore, acct, now)
	if err != nil {
		slog.Error("pack pool lookup failed", "account", acct, "err", err)
		writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		return
	}
	pools = append(pools, packPools...)
	s, err := a.engine.Settle(usage, unitPrice, req.ProductCode, req.SnapshotID, pools)
	if err != nil {
		slog.Warn("settle rejected", "account", acct, "agg", req.AggID, "err", err)
		writeErr(w, http.StatusBadRequest, "Billing.SettleFailed", "settlement failed")
		return
	}

	// Commit the deduction intent: spend the pools and debit the cash portion
	// from the ledger. The pool and ledger writes are both in-memory here; in
	// production they share one local transaction. settleMu is held across the
	// whole commit so duplicates cannot interleave.
	a.pools.commit(acct, req.PoolRefs, s.Charge.Deductions)
	for _, d := range s.Charge.Deductions {
		if d.Source == billing.SourceResourcePack {
			// Commit the pack consumption with the charge id as the idempotency
			// key, so a retried settlement is a no-op against the pack ledger.
			if _, _, _, err := a.packs.Consume(d.Ref, req.ProductCode, d.Amount, s.Charge.ChargeID, s.Charge.ChargeID); err != nil && !errors.Is(err, reservepack.ErrPackTerminal) && !errors.Is(err, reservepack.ErrPackExhausted) && !errors.Is(err, reservepack.ErrInsufficientQuota) {
				slog.Error("pack consume failed", "account", acct, "pack", d.Ref, "err", err)
				writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
				return
			}
		}
	}
	if !s.Charge.PayAmount.IsZero() {
		if _, _, err := a.ledger.Consume(acct, s.Charge.PayAmount, "bill",
			s.Charge.ChargeID, s.Charge.ChargeID, "settle "+req.AggID); err != nil && !errors.Is(err, ledger.ErrDuplicateEntry) {
			// ErrDuplicateEntry means an earlier attempt already debited this
			// charge — that is idempotent success, not an error.
			slog.Error("ledger consume failed", "account", acct, "charge", s.Charge.ChargeID, "err", err)
			writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
			return
		}
	}
	a.charges.append(s.Charge)

	deductions := make([]map[string]any, 0, len(s.Charge.Deductions))
	for _, d := range s.Charge.Deductions {
		deductions = append(deductions, map[string]any{
			"Source": d.Source,
			"Ref":    d.Ref,
			"Amount": d.Amount.String(),
		})
	}

	resp := map[string]any{
		"Charge": map[string]any{
			"ChargeId":      s.Charge.ChargeID,
			"AccountId":     s.Charge.AccountID,
			"ResourceId":    s.Charge.ResourceID,
			"ProductCode":   s.Charge.ProductCode,
			"MeteringItem":  s.Charge.MeteringItem,
			"BillPeriod":    s.Charge.BillPeriod,
			"Quantity":      s.Charge.Quantity.String(),
			"UnitPrice":     s.Charge.UnitPrice.String(),
			"PretaxAmount":  s.Charge.PretaxAmount.String(),
			"PayAmount":     s.Charge.PayAmount.String(),
			"CoveredRatio":  s.Charge.CoveredRatio,
			"Incomplete":    s.Charge.Incomplete(),
			"Reconciles":    s.Charge.Reconciles(),
			"SnapshotId":    s.Charge.SnapshotID,
			"SettledAt":     s.Charge.SettledAt.Format(time.RFC3339),
		},
		"Deductions": deductions,
		"Shortfall":  s.Shortfall.String(),
		"InArrears":  s.InArrears,
	}
	a.settled[idemKey] = resp
	writeJSON(w, http.StatusOK, resp)
}

// handleBills summarizes the account's charges for a billing period.
func (a *app) handleBills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "missing period query param (YYYY-MM)")
		return
	}
	bill := billing.Summarize(acct, period, a.charges.forAccount(acct))
	writeJSON(w, http.StatusOK, map[string]any{
		"AccountId":        bill.AccountID,
		"BillPeriod":       bill.BillPeriod,
		"TotalAmount":      bill.TotalAmount.String(),
		"PaidAmount":       bill.PaidAmount.String(),
		"ChargeCount":      bill.ChargeCount,
		"IncompleteCharges": bill.IncompleteCharges,
		"UnreconciledCount": bill.UnreconciledCount,
		"Final":             bill.Final(),
	})
}

// ---------------------------------------------------------------------------
// Resource-pack handlers (phase 2, M-4.1)
// ---------------------------------------------------------------------------

// handleReservePacks dispatches by method: POST purchases a pack, GET lists
// the account's active packs.
func (a *app) handleReservePacks(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		packs, err := a.packStore.ListActiveByAccount(acct, time.Now())
		if err != nil {
			slog.Error("pack list failed", "account", acct, "err", err)
			writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
			return
		}
		out := make([]map[string]any, 0, len(packs))
		for _, p := range packs {
			out = append(out, packToMap(p))
		}
		writeJSON(w, http.StatusOK, map[string]any{"packs": out, "total": len(out)})
	case http.MethodPost:
		var req struct {
			PackID      string `json:"packId"`
			ProductCode string `json:"productCode"`
			SKUCode     string `json:"skuCode"`
			FaceValue   string `json:"faceValue"`   // decimal yuan
			ExpireAt    string `json:"expireAt"`    // RFC3339, empty = no expiry
			OrderKey    string `json:"orderKey"`
		}
		limitBody(w, r)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed request")
			return
		}
		face, err := pricing.ParseAmount(req.FaceValue)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "invalid faceValue")
			return
		}
		var expire time.Time
		if req.ExpireAt != "" {
			expire, err = time.Parse(time.RFC3339, req.ExpireAt)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "invalid expireAt")
				return
			}
		}
		pack, _, err := a.packs.Purchase(req.PackID, acct, req.ProductCode, req.SKUCode, face, expire, req.OrderKey)
		if err != nil {
			slog.Warn("pack purchase failed", "account", acct, "pack", req.PackID, "err", err)
			writeErr(w, http.StatusBadRequest, "Billing.PurchaseFailed", "pack purchase failed")
			return
		}
		writeJSON(w, http.StatusOK, packToMap(pack))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleReservePackSweep runs the expiry sweep for the caller's account.
func (a *app) handleReservePackSweep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	expired, err := a.packs.SweepExpired(acct)
	if err != nil {
		slog.Error("pack sweep failed", "account", acct, "err", err)
		writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"expiredPacks": expired, "count": len(expired)})
}

func packToMap(p reservepack.Pack) map[string]any {
	expire := ""
	if !p.ExpireAt.IsZero() {
		expire = p.ExpireAt.Format(time.RFC3339)
	}
	return map[string]any{
		"packId":      p.PackID,
		"accountId":   p.AccountID,
		"productCode": p.ProductCode,
		"skuCode":     p.SKUCode,
		"faceValue":   p.FaceValue.String(),
		"remaining":   p.Remaining.String(),
		"purchasedAt": p.PurchasedAt.Format(time.RFC3339),
		"expireAt":    expire,
		"status":      p.Status,
		"version":     p.Version,
	}
}

// ---------------------------------------------------------------------------
// Invoice + cost-analysis handlers (phase 2, M-4.3)
// ---------------------------------------------------------------------------

// handleInvoices dispatches by method: POST drafts an invoice, GET lists the
// account's invoices.
func (a *app) handleInvoices(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		invs := a.invoices.ListByAccount(acct)
		out := make([]map[string]any, 0, len(invs))
		for _, inv := range invs {
			out = append(out, invoiceToMap(inv))
		}
		writeJSON(w, http.StatusOK, map[string]any{"invoices": out, "total": len(out)})
	case http.MethodPost:
		var req struct {
			InvoiceID  string            `json:"invoiceId"`
			BillPeriod string            `json:"billPeriod"`
			Title      string            `json:"title"`
			TaxNo      string            `json:"taxNo"`
			Items      []invoiceLineIn   `json:"items"`
		}
		limitBody(w, r)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed request")
			return
		}
		items := make([]invoice.LineItem, 0, len(req.Items))
		for _, it := range req.Items {
			amt, err := pricing.ParseAmount(it.Amount)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "invalid item amount")
				return
			}
			items = append(items, invoice.LineItem{Description: it.Description, Amount: amt})
		}
		inv, err := a.invoices.Draft(req.InvoiceID, acct, req.BillPeriod, req.Title, req.TaxNo, items)
		if err != nil {
			slog.Warn("invoice draft failed", "account", acct, "invoice", req.InvoiceID, "err", err)
			writeErr(w, http.StatusBadRequest, "Billing.InvoiceDraftFailed", "invoice draft failed")
			return
		}
		writeJSON(w, http.StatusOK, invoiceToMap(inv))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type invoiceLineIn struct {
	Description string `json:"description"`
	Amount      string `json:"amount"`
}

// handleInvoiceIssue promotes a DRAFT invoice to ISSUED.
func (a *app) handleInvoiceIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req struct{ InvoiceID string `json:"invoiceId"` }
	limitBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed request")
		return
	}
	// IDOR guard: the invoice must belong to the caller's account. A foreign or
	// unknown invoice is reported as 404 (not 403) so IDs cannot be enumerated.
	if existing, ok := a.invoices.Get(req.InvoiceID); !ok || existing.AccountID != acct {
		writeErr(w, http.StatusNotFound, "Billing.InvoiceNotFound", "invoice not found")
		return
	}
	inv, err := a.invoices.Issue(req.InvoiceID)
	if err != nil {
		slog.Warn("invoice issue failed", "account", acct, "invoice", req.InvoiceID, "err", err)
		writeErr(w, http.StatusBadRequest, "Billing.InvoiceIssueFailed", "invoice issue failed")
		return
	}
	writeJSON(w, http.StatusOK, invoiceToMap(inv))
}

// handleInvoiceVoid issues a 红冲 reversal against an issued invoice.
func (a *app) handleInvoiceVoid(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		OriginalID  string `json:"originalId"`
		ReversalID  string `json:"reversalId"`
	}
	limitBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed request")
		return
	}
	// IDOR guard: the original invoice must belong to the caller's account.
	// Foreign/unknown invoices are 404 so IDs cannot be enumerated.
	if orig, ok := a.invoices.Get(req.OriginalID); !ok || orig.AccountID != acct {
		writeErr(w, http.StatusNotFound, "Billing.InvoiceNotFound", "invoice not found")
		return
	}
	reversal, err := a.invoices.Void(req.OriginalID, req.ReversalID)
	if err != nil {
		slog.Warn("invoice void failed", "account", acct, "invoice", req.OriginalID, "err", err)
		writeErr(w, http.StatusBadRequest, "Billing.InvoiceVoidFailed", "invoice void failed")
		return
	}
	writeJSON(w, http.StatusOK, invoiceToMap(reversal))
}

// handleCostAnalysis returns the account's cost breakdown by product/resource.
func (a *app) handleCostAnalysis(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "missing period query param (YYYY-MM)")
		return
	}
	report := billing.CostAnalysis(acct, period, a.charges.forAccount(acct))
	byProduct := make(map[string]string, len(report.ByProduct))
	for k, v := range report.ByProduct {
		byProduct[k] = v.String()
	}
	byResource := make(map[string]string, len(report.ByResource))
	for k, v := range report.ByResource {
		byResource[k] = v.String()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accountId":  report.AccountID,
		"billPeriod": report.BillPeriod,
		"byProduct":  byProduct,
		"byResource": byResource,
		"total":      report.Total.String(),
	})
}

func invoiceToMap(inv invoice.Invoice) map[string]any {
	items := make([]map[string]any, 0, len(inv.Items))
	for _, it := range inv.Items {
		items = append(items, map[string]any{"description": it.Description, "amount": it.Amount.String()})
	}
	issued := ""
	if !inv.IssuedAt.IsZero() {
		issued = inv.IssuedAt.Format(time.RFC3339)
	}
	voided := ""
	if !inv.VoidedAt.IsZero() {
		voided = inv.VoidedAt.Format(time.RFC3339)
	}
	return map[string]any{
		"invoiceId":   inv.InvoiceID,
		"accountId":   inv.AccountID,
		"billPeriod":  inv.BillPeriod,
		"title":       inv.Title,
		"taxNo":       inv.TaxNo,
		"amount":      inv.Amount.String(),
		"status":      inv.Status,
		"issuedAt":    issued,
		"voidedAt":    voided,
		"voidedById":  inv.VoidedByID,
		"reversesId":  inv.ReversesID,
		"items":       items,
	}
}

// ---------------------------------------------------------------------------
// In-memory stores
// ---------------------------------------------------------------------------

// memLedgerStore implements ledger.Store with plain maps guarded by a mutex,
// mirroring the in-memory store in the ledger package's tests.
type memLedgerStore struct {
	mu       sync.Mutex
	balances map[int64]ledger.Balance
	entries  map[int64][]ledger.Entry
	byKey    map[string]ledger.Entry
}

func newMemLedgerStore() *memLedgerStore {
	return &memLedgerStore{
		balances: make(map[int64]ledger.Balance),
		entries:  make(map[int64][]ledger.Entry),
		byKey:    make(map[string]ledger.Entry),
	}
}

func (m *memLedgerStore) GetBalance(accountID int64) (ledger.Balance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.balances[accountID]
	if !ok {
		return ledger.Balance{AccountID: accountID}, nil
	}
	return b, nil
}

func (m *memLedgerStore) Apply(entry ledger.Entry, newBalance ledger.Balance, expectedVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.balances[entry.AccountID]
	curVersion := 0
	if ok {
		curVersion = cur.Version
	}
	if curVersion != expectedVersion {
		return ledger.ErrVersionConflict
	}
	m.balances[entry.AccountID] = newBalance
	m.entries[entry.AccountID] = append(m.entries[entry.AccountID], entry)
	m.byKey[fmt.Sprintf("%d|%s", entry.AccountID, entry.IdempotencyKey)] = entry
	return nil
}

func (m *memLedgerStore) FindByIdempotencyKey(accountID int64, key string) (ledger.Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byKey[fmt.Sprintf("%d|%s", accountID, key)]
	return e, ok, nil
}

func (m *memLedgerStore) ListEntries(accountID int64) ([]ledger.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ledger.Entry, len(m.entries[accountID]))
	copy(out, m.entries[accountID])
	return out, nil
}

// poolRegistry holds the deductible pools per account (cash is represented in
// the ledger; coupons/resource-packs live here).
type poolRegistry struct {
	mu    sync.Mutex
	pools map[int64][]billing.Pool
}

func newPoolRegistry() *poolRegistry {
	return &poolRegistry{pools: make(map[int64][]billing.Pool)}
}

// set installs the pool set for an account (dev seed hook).
func (p *poolRegistry) set(acct int64, pools []billing.Pool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pools[acct] = pools
}

// forAccount returns a copy of the account's pools, optionally filtered to the
// requested refs. A zero ref list means all pools. Returning a copy (never the
// underlying slice) keeps all reads/writes of the shared slice inside p.mu —
// callers may use the result freely outside the lock.
func (p *poolRegistry) forAccount(acct int64, refs []string) []billing.Pool {
	p.mu.Lock()
	defer p.mu.Unlock()
	all := p.pools[acct]
	if len(refs) == 0 {
		out := make([]billing.Pool, len(all))
		copy(out, all)
		return out
	}
	want := make(map[string]bool, len(refs))
	for _, r := range refs {
		want[r] = true
	}
	var out []billing.Pool
	for _, pool := range all {
		if want[pool.Ref] {
			out = append(out, pool)
		}
	}
	return out
}

// commit reduces each pool's Available by what the settlement consumed.
func (p *poolRegistry) commit(acct int64, refs []string, deductions []billing.Deduction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	spent := make(map[string]pricing.Amount)
	for _, d := range deductions {
		if d.Ref == "" {
			continue // cash portion is handled by the ledger
		}
		spent[d.Ref] = spent[d.Ref].Add(d.Amount)
	}
	if len(spent) == 0 {
		return
	}
	allow := make(map[string]bool, len(refs))
	for _, r := range refs {
		allow[r] = true
	}
	cur := p.pools[acct]
	for i := range cur {
		if len(refs) > 0 && !allow[cur[i].Ref] {
			continue
		}
		if amt, ok := spent[cur[i].Ref]; ok {
			cur[i].Available = cur[i].Available.Sub(amt)
		}
	}
}

// chargeRegistry accumulates settled charges so /internal/bills can summarize.
type chargeRegistry struct {
	mu       sync.Mutex
	charges  map[int64][]billing.Charge
}

func newChargeRegistry() *chargeRegistry {
	return &chargeRegistry{charges: make(map[int64][]billing.Charge)}
}

func (c *chargeRegistry) append(ch billing.Charge) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.charges[ch.AccountID] = append(c.charges[ch.AccountID], ch)
}

func (c *chargeRegistry) forAccount(acct int64) []billing.Charge {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]billing.Charge, len(c.charges[acct]))
	copy(out, c.charges[acct])
	return out
}
