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
	"github.com/starcloud/sc-platform/ledger"
	"github.com/starcloud/sc-platform/metering"
	"github.com/starcloud/sc-platform/pricing"
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
	ledger  *ledger.Ledger
	store   *memLedgerStore
	engine  *billing.Engine
	pools   *poolRegistry
	charges *chargeRegistry
}

func newApp() *app {
	store := newMemLedgerStore()
	return &app{
		ledger:  ledger.New(store, time.Now, nil),
		store:   store,
		engine:  billing.NewEngine(time.Now),
		pools:   newPoolRegistry(),
		charges: newChargeRegistry(),
	}
}

// accountIDFrom extracts the caller's account id from the gateway-injected
// header. A missing/blank header is 403 — the service must never fall back to
// a client-supplied account in the body (07§3.3).
func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
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
		writeErr(w, http.StatusInternalServerError, "Billing.InternalError", err.Error())
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

	pools := a.pools.forAccount(acct, req.PoolRefs)
	s, err := a.engine.Settle(usage, unitPrice, req.ProductCode, req.SnapshotID, pools)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Billing.SettleFailed", err.Error())
		return
	}

	// Commit the deduction intent: spend the pools and debit the cash portion
	// from the ledger. The pool and ledger writes are both in-memory here; in
	// production they share one local transaction.
	a.pools.commit(acct, req.PoolRefs, s.Charge.Deductions)
	if !s.Charge.PayAmount.IsZero() {
		if _, _, err := a.ledger.Consume(acct, s.Charge.PayAmount, "bill",
			s.Charge.ChargeID, s.Charge.ChargeID, "settle "+req.AggID); err != nil {
			writeErr(w, http.StatusInternalServerError, "Billing.InternalError", err.Error())
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

	writeJSON(w, http.StatusOK, map[string]any{
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
	})
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

// forAccount returns the account's pools, optionally filtered to the requested
// refs. A zero ref list means all pools.
func (p *poolRegistry) forAccount(acct int64, refs []string) []billing.Pool {
	p.mu.Lock()
	defer p.mu.Unlock()
	all := p.pools[acct]
	if len(refs) == 0 {
		return all
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
