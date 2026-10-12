// Command svc-billing serves the internal billing HTTP API (03-backend-services.md
// §4.2.4) using the platform's stdlib-only pattern: pkg-go domain logic behind
// plain net/http handlers, no Kratos/grpc/prometheus.
//
// Routes (all /internal/* are gateway-egress only — APISIX injects the caller
// identity; the service must not trust a client-supplied account):
//
//	GET  /internal/balance  -> ledger.Balance for the account (pkg-go/ledger)
//	POST /internal/settle   -> billing.Engine.Settle over the account's resource
//	                           packs, registry coupons and its ledger cash
//	                           balance (debited for the cash share); returns
//	                           the Charge + deductions + arrears state
//	GET  /internal/bills    -> billing.Summarize for a period, over the settled
//	                           charges in the settle store (trade_db
//	                           settle_charge when persistent)
//
// Every handler reads the caller's account_id from the X-Euler-Account-Id header
// the gateway injects (07§3.3); a missing header is a 403, never a guess.
//
// /healthz, /readyz and /metrics are the probe/metric endpoints every service
// carries on Day 1 (03§2.3.4).
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/qifalab/euler-platform/billing"
	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/invoice"
	"github.com/qifalab/euler-platform/ledger"
	"github.com/qifalab/euler-platform/metering"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/reservepack"
	"github.com/qifalab/euler-platform/runmode"
	"github.com/qifalab/euler-platform/storage"
)

func main() {
	// Dev port allocation for svc-billing is :9210 — the 92xx block the console
	// plane uses (:9206 belongs to svc-metering). console-bff's EULER_SVC_BILLING_URL
	// default points at the same port; production overrides via -http / Helm.
	var httpAddr = flag.String("http", ":9210", "HTTP listen address")
	flag.Parse()
	// Production refuses to start without the internal shared secret: the
	// X-Euler-Account-Id this service trusts must provably come from the gateway.
	runtimeRequireSecrets()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	app, err := newApp(context.Background())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	slog.Info("svc-billing stores ready", "persistent", storage.Enabled())

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
// logging, and recover (03§2.3.4). The chain itself lives in pkg-go/httpmw so
// the services cannot drift apart.
func withMiddleware(h http.Handler) http.Handler {
	return httpmw.Chain(httpmw.InternalToken(h))
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
	_, _ = w.Write([]byte("# HELP eu_service_dummy 0\n# TYPE eu_service_dummy counter\nsc_service_dummy 0\n"))
}

// ---------------------------------------------------------------------------
// App + in-memory stores
// ---------------------------------------------------------------------------

// app holds the wired pkg-go domain objects and the stores behind them: MySQL
// (trade_db) when EULER_DB_DSN is set, in-memory otherwise.
type app struct {
	ledger *ledger.Ledger
	// store is the ledger's persistence port — *memLedgerStore in the demo,
	// ledger.SQLStore against trade_db.
	store     ledger.Store
	engine    *billing.Engine
	pools     *poolRegistry
	packs     *reservepack.Ledger
	packStore reservepack.Store
	// invoices holds the invoice documents — trade_db's invoice tables when
	// persistent (spend cap enforced under a per-account row lock), an
	// invoice.Book otherwise (invoicestore.go).
	invoices invoiceStore

	// settles holds every charge's settlement record — the plan while it is
	// being committed, the final charge once settled — keyed by the
	// deterministic ChargeID, and serialises one account's settlements across
	// replicas (billing.SQLSettleStore against trade_db, in-memory otherwise).
	// A retried or concurrently duplicated settlement replays the stored
	// result; one that died part-way resumes from the stored plan. Bills and
	// cost analysis read their charges from it.
	settles billing.SettleStore

	// packMu serialises resource-pack purchases: the pre-flight, the debit, the
	// credit and any compensation of one purchase must not interleave with a
	// concurrent attempt at the same purchase.
	packMu sync.Mutex
}

// cashSpent is what the account actually paid: CONSUME − REFUND in its cash
// ledger. The invoice store caps the account's live invoices at it.
//
// Invoices used to accept any line items the caller typed, so a tenant could
// issue 发票 for amounts it never paid — a tax-compliance incident, not a
// display bug.
func (a *app) cashSpent(acct int64) (pricing.Amount, error) {
	entries, err := a.store.ListEntries(acct)
	if err != nil {
		return 0, err
	}
	var spent pricing.Amount
	for _, e := range entries {
		switch e.Type {
		case ledger.EntryConsume:
			spent = spent.Add(e.Amount)
		case ledger.EntryRefund:
			spent = spent.Sub(e.Amount)
		}
	}
	return spent, nil
}

// newApp wires the domain objects. Persistence is opt-in (pkg-go/storage doc):
// with EULER_DB_DSN set, the cash ledger, the resource-pack ledger and the
// settlement records (settle plans and settled charges) are trade_db's tables, so
// balances, packs, their journals and the bills survive a restart — and a retried
// settlement on any replica replays instead of charging again; unset, the
// service keeps the in-memory stores that make the demo and `go test`
// dependency-free.
//
// A configured DSN that cannot be reached, or a schema sqlmigrate never touched,
// is a startup failure: an "healthy" process whose first settle 500s is worse
// than one that refuses to come up.
//
// Journal ids come from the 号段 allocator (trade_db.id_sequence, 04§6.6) when
// the store is persistent. The in-process counters are only correct for the
// in-memory store — pointed at a real database they restart at 1 and collide with
// the rows the previous run committed.
//
// Invoices follow the same rule: trade_db's invoice / invoice_item when
// persistent (svc-billing V3/V5), an in-memory invoice.Book otherwise.
func newApp(ctx context.Context) (*app, error) {
	db, ok, err := storage.MustOpenFor(ctx, "trade_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newInMemoryApp(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "trade_db"); err != nil {
		return nil, err
	}
	return newSQLApp(ctx, db)
}

// newSQLApp wires the app to a migrated trade_db pool. Two apps built on one
// database behave as two replicas (or one process before and after a restart):
// every piece of settlement state they share lives in the database.
func newSQLApp(ctx context.Context, db *sql.DB) (*app, error) {
	ledgerIDs, err := storage.OpenSequence(ctx, db, "ledger_entry", 1000)
	if err != nil {
		return nil, err
	}
	packIDs, err := storage.OpenSequence(ctx, db, "resource_pack_journal", 1000)
	if err != nil {
		return nil, err
	}
	invoiceIDs, err := storage.OpenSequence(ctx, db, "invoice", 100)
	if err != nil {
		return nil, err
	}

	app := &app{
		engine:   billing.NewEngine(time.Now),
		pools:    newPoolRegistry(),
		settles:  billing.NewSQLSettleStore(db),
		invoices: &sqlInvoiceStore{db: db, ids: invoiceIDs, now: time.Now},
	}
	app.store = ledger.NewSQLStore(db)
	app.ledger = ledger.New(app.store, time.Now, ledgerIDs.NextFunc())
	app.packStore = reservepack.NewSQLStore(db)
	app.packs = reservepack.New(app.packStore, time.Now, packIDs.NextFunc())
	return app, nil
}

// newInMemoryApp builds the demo app: in-memory stores, guarded local id
// counters, invoice book. Tests construct it directly, so they never depend on
// whether the developer's shell has EULER_DB_DSN set.
func newInMemoryApp() *app {
	app := &app{
		engine:   billing.NewEngine(time.Now),
		pools:    newPoolRegistry(),
		settles:  billing.NewMemorySettleStore(),
		invoices: newMemInvoiceStore(time.Now),
	}
	app.store = newMemLedgerStore()
	app.ledger = ledger.New(app.store, time.Now, guardedCounter())
	app.packStore = reservepack.NewMemoryStore()
	app.packs = reservepack.New(app.packStore, time.Now, guardedCounter())
	return app
}

// guardedCounter mints the in-memory ids. The closures are shared by concurrent
// handlers, so the counter is mutex-guarded: an unguarded ++ both races under
// -race and can hand two journal entries the same id.
func guardedCounter() func() int64 {
	var (
		mu  sync.Mutex
		seq int64
	)
	return func() int64 {
		mu.Lock()
		defer mu.Unlock()
		seq++
		return seq
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
// When the EULER_INTERNAL_TOKEN environment variable is set, every /internal/*
// request must carry a matching X-Euler-Internal-Token header. Unset (the dev
// default) disables the check.
var internalToken = os.Getenv("EULER_INTERNAL_TOKEN")

// checkInternalToken enforces the optional shared-secret header. Returns false
// (and writes 403) when the token is configured and the request's header does
// not match.
func checkInternalToken(w http.ResponseWriter, r *http.Request) bool {
	if internalToken == "" {
		return true // dev default: gateway network isolation is the only guard
	}
	if r.Header.Get("X-Euler-Internal-Token") != internalToken {
		writeErr(w, http.StatusForbidden, "Forbidden", "invalid internal token")
		return false
	}
	return true
}

// accountIDFrom extracts the caller's account id from the gateway-injected
// header. A missing/blank header is 403 — the service must never fall back to
// a client-supplied account in the body (07§3.3).
//
// TRUST BOUNDARY: X-Euler-Account-Id is trusted only because /internal/* routes
// are reachable exclusively via the APISIX gateway, which strips any
// client-supplied copy and injects the authenticated account. Deployments that
// cannot guarantee network isolation should set EULER_INTERNAL_TOKEN so callers
// must also present the shared X-Euler-Internal-Token secret.
func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if !checkInternalToken(w, r) {
		return 0, false
	}
	raw := strings.TrimSpace(r.Header.Get("X-Euler-Account-Id"))
	if raw == "" {
		writeErr(w, http.StatusForbidden, "Forbidden", "missing X-Euler-Account-Id header")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed X-Euler-Account-Id")
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
	AggID         string   `json:"AggId"`
	ResourceID    string   `json:"ResourceId"`
	MeteringItem  string   `json:"MeteringItem"`
	TotalQuantity string   `json:"TotalQuantity"` // decimal, e.g. "2" or "1.99998"
	HourStart     string   `json:"HourStart"`     // RFC3339
	CoveredRatio  int      `json:"CoveredRatio"`
	ProductCode   string   `json:"ProductCode"`
	UnitPrice     string   `json:"UnitPrice"` // decimal, e.g. "0.25"
	SnapshotID    string   `json:"SnapshotId"`
	PoolRefs      []string `json:"PoolRefs"` // optional refs to restrict which pools apply
}

// Column widths of settle_charge (svc-billing V4). The settlement record is
// written after the money moved, so a value the column cannot hold is refused
// before anything is charged rather than failing that final write.
const (
	maxSettleResourceIDLen   = 64
	maxSettleMeteringItemLen = 64
	maxSettleProductCodeLen  = 32
	maxSettleSnapshotIDLen   = 64
)

// settleLockTimeout bounds one settlement's critical section: the wait for the
// account's settlement lock plus the commit. The lock is held by a transaction
// bound to this deadline (it outlives a client that hangs up, so a half-done
// commit is not abandoned mid-way), and a settlement that overruns it loses the
// lock — the record's version guard then refuses its stale write.
const settleLockTimeout = 30 * time.Second

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
	// The ChargeID is the cash debit's ledger key and biz key (VARCHAR(64)), and
	// the idempotency key of the whole settlement: an empty AggId would make
	// every such settlement the same charge.
	if strings.TrimSpace(req.AggID) == "" || len(billing.ChargeID(req.AggID)) > ledger.MaxKeyLen {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "AggId is required and must be at most 60 bytes")
		return
	}
	if len(req.ResourceID) > maxSettleResourceIDLen || len(req.MeteringItem) > maxSettleMeteringItemLen ||
		len(req.ProductCode) > maxSettleProductCodeLen || len(req.SnapshotID) > maxSettleSnapshotIDLen {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "ResourceId, MeteringItem, ProductCode or SnapshotId is too long")
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
	// (billing.ChargeID(AggID)), so it doubles as the settle idempotency key. A
	// settled charge is final on every replica: the stored result is replayed
	// (200) and the pools and the ledger are never touched a second time.
	chargeID := billing.ChargeID(req.AggID)
	if rec, found, err := a.settles.GetSettle(acct, chargeID); err != nil {
		slog.Error("settle record lookup failed", "account", acct, "charge", chargeID, "err", err)
		writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		return
	} else if found && rec.Settled {
		writeJSON(w, http.StatusOK, settleResponse(rec.Charge))
		return
	}

	// One account settles one charge at a time, across every replica: the
	// waterfall decides against the cash balance and the packs it reads, and a
	// concurrent settlement of the same account (or a duplicate of this one)
	// must not decide against the same figures.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), settleLockTimeout)
	defer cancel()
	release, err := a.settles.LockAccount(ctx, acct)
	if err != nil {
		slog.Warn("settle lock unavailable", "account", acct, "charge", chargeID, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "Billing.SettleBusy", "settlement for this account is busy, retry")
		return
	}
	defer release()

	rec, found, err := a.settles.GetSettle(acct, chargeID)
	if err != nil {
		slog.Error("settle record lookup failed", "account", acct, "charge", chargeID, "err", err)
		writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		return
	}
	if found && rec.Settled {
		// A duplicate settled it while this request waited for the lock.
		writeJSON(w, http.StatusOK, settleResponse(rec.Charge))
		return
	}
	if !found {
		rec = billing.SettleRecord{AccountID: acct, ChargeID: chargeID}
	}

	s, registryDeductions, apiErr := a.settleCommit(acct, req, usage, unitPrice, &rec)
	if apiErr != nil {
		writeErr(w, apiErr.status, apiErr.code, apiErr.msg)
		return
	}

	// Record the charge: from here on every replay — on any replica, after any
	// restart — returns it instead of settling again, and bills include it.
	rec.Settled = true
	rec.CashPaid = s.Charge.PayAmount
	rec.Charge = s.Charge
	if err := a.settles.SaveSettle(&rec); err != nil {
		// Packs and cash are committed under the ChargeID, so a retry resumes
		// (replaying both) and records the same charge.
		slog.Error("settle record write failed", "account", acct, "charge", chargeID, "err", err)
		if errors.Is(err, billing.ErrSettleConflict) {
			writeErr(w, http.StatusConflict, "Billing.ConcurrentUpdate", "concurrent settlement update, retry")
			return
		}
		writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		return
	}
	// Registry pools (coupons) last, once the charge is recorded: a failed
	// attempt leaves them untouched for the retry, and a recorded charge is
	// replayed above, never committed twice.
	a.pools.commit(acct, req.PoolRefs, registryDeductions)
	writeJSON(w, http.StatusOK, settleResponse(rec.Charge))
}

// settleResponse renders a settled charge. The first settlement and every
// replay render the stored charge, so they answer byte-for-byte the same.
func settleResponse(c billing.Charge) map[string]any {
	deductions := make([]map[string]any, 0, len(c.Deductions))
	for _, d := range c.Deductions {
		deductions = append(deductions, map[string]any{
			"Source": d.Source,
			"Ref":    d.Ref,
			"Amount": d.Amount.String(),
		})
	}
	return map[string]any{
		"Charge": map[string]any{
			"ChargeId":     c.ChargeID,
			"AccountId":    c.AccountID,
			"ResourceId":   c.ResourceID,
			"ProductCode":  c.ProductCode,
			"MeteringItem": c.MeteringItem,
			"BillPeriod":   c.BillPeriod,
			"Quantity":     c.Quantity.String(),
			"UnitPrice":    c.UnitPrice.String(),
			"PretaxAmount": c.PretaxAmount.String(),
			"PayAmount":    c.PayAmount.String(),
			"CoveredRatio": c.CoveredRatio,
			"Incomplete":   c.Incomplete(),
			"Reconciles":   c.Reconciles(),
			"SnapshotId":   c.SnapshotID,
			"SettledAt":    c.SettledAt.Format(time.RFC3339),
		},
		"Deductions": deductions,
		// Settlement.Shortfall is the charge's own shortfall; arrears is
		// derived from it, so a replay cannot disagree with the original.
		"Shortfall": c.Shortfall.String(),
		"InArrears": !c.Shortfall.IsZero(),
	}
}

// apiError is a handler verdict carried out of a helper.
type apiError struct {
	status    int
	code, msg string
}

var errSettleInternal = &apiError{http.StatusInternalServerError, "Billing.InternalError", "internal error"}

// pinnedPacks renders the consumed pack quota as waterfall pools that cover
// exactly what was taken. Pack tier goes first, so each pinned pool is spent in
// full and the remainder flows to coupons, then cash.
func pinnedPacks(taken map[string]pricing.Amount) []billing.Pool {
	out := make([]billing.Pool, 0, len(taken))
	for ref, amount := range taken {
		if amount.IsZero() || amount.IsNegative() {
			continue
		}
		out = append(out, billing.Pool{Source: billing.SourceResourcePack, Ref: ref, Available: amount})
	}
	return out
}

// withoutCash drops registry cash pools: cash lives in the ledger, and a static
// BALANCE pool would let the waterfall "pay" with money the ledger never debits.
func withoutCash(pools []billing.Pool) []billing.Pool {
	out := make([]billing.Pool, 0, len(pools))
	for _, p := range pools {
		if p.Source != billing.SourceBalance {
			out = append(out, p)
		}
	}
	return out
}

// saveProgress persists the plan after a failed step, best effort: the pack
// consumes it records are idempotent on the ChargeID, so a retry that finds an
// older plan replays them and arrives at the same figures.
func (a *app) saveProgress(rec *billing.SettleRecord) {
	if rec.Version == 0 {
		return
	}
	if err := a.settles.SaveSettle(rec); err != nil {
		slog.Warn("settle plan progress not saved", "account", rec.AccountID, "charge", rec.ChargeID, "err", err)
	}
}

// settleCommit computes and commits one charge, in an order that a failure at
// any step — or a crash, or a retry landing on another replica — can be resumed
// from without double-charging. Progress lives in rec, which the caller loaded
// from (and the steps below write back to) the settle store:
//
//  1. resource packs — the waterfall's pack intent is persisted BEFORE any pack
//     moves, then each pack is consumed with the ChargeID as key and pinned at
//     the quota actually taken (a pack that ran dry or went terminal covers
//     less, or nothing, and its share moves down the waterfall). A resumed plan
//     replays the intent's consumes — the pack ledger returns what each
//     originally took — and never recomputes them;
//  2. cash — the ledger's own balance is the BALANCE pool, and the cash share is
//     debited under the ChargeID (re-read and recomputed if the balance moved);
//  3. registry pools (coupons) — returned to the caller, which commits them only
//     once the charge is recorded.
func (a *app) settleCommit(acct int64, req settleRequest, usage metering.HourlyUsage, unitPrice pricing.Amount, rec *billing.SettleRecord) (billing.Settlement, []billing.Deduction, *apiError) {
	chargeID := rec.ChargeID
	// forAccount returns a copy, so the waterfall below never reads the shared
	// registry slice outside the registry's lock.
	registry := withoutCash(a.pools.forAccount(acct, req.PoolRefs))
	if rec.PackIntent == nil {
		rec.PackIntent = map[string]pricing.Amount{}
	}
	if rec.PackTaken == nil {
		rec.PackTaken = map[string]pricing.Amount{}
	}
	storePacks := make(map[string]bool, len(rec.PackIntent))
	for ref := range rec.PackIntent {
		storePacks[ref] = true
	}

	if !rec.PacksDone {
		if len(rec.PackIntent) == 0 {
			// A fresh plan: decide what each active pack is asked to cover.
			packPools, err := reservepack.PoolsForAccount(a.packStore, acct, time.Now())
			if err != nil {
				slog.Error("pack pool lookup failed", "account", acct, "err", err)
				return billing.Settlement{}, nil, errSettleInternal
			}
			for _, p := range packPools {
				storePacks[p.Ref] = true
			}
			cash, apiErr := a.cashPool(acct, chargeID)
			if apiErr != nil {
				return billing.Settlement{}, nil, apiErr
			}
			pools := append(append(append([]billing.Pool{}, registry...), packPools...), cash...)
			s, serr := a.engine.Settle(usage, unitPrice, req.ProductCode, req.SnapshotID, pools)
			if serr != nil {
				slog.Warn("settle rejected", "account", acct, "agg", req.AggID, "err", serr)
				return billing.Settlement{}, nil, &apiError{http.StatusBadRequest, "Billing.SettleFailed", "settlement failed"}
			}
			for _, d := range s.Charge.Deductions {
				if d.Source == billing.SourceResourcePack && storePacks[d.Ref] {
					rec.PackIntent[d.Ref] = rec.PackIntent[d.Ref].Add(d.Amount)
				}
			}
			if len(rec.PackIntent) > 0 {
				// Durable BEFORE the first consume: whatever happens next, a
				// retry knows every pack that may hold a consume under this key.
				if err := a.settles.SaveSettle(rec); err != nil {
					slog.Error("settle plan write failed", "account", acct, "charge", chargeID, "err", err)
					if errors.Is(err, billing.ErrSettleConflict) {
						return billing.Settlement{}, nil, &apiError{http.StatusConflict, "Billing.ConcurrentUpdate", "concurrent settlement update, retry"}
					}
					return billing.Settlement{}, nil, errSettleInternal
				}
			}
		}

		refs := make([]string, 0, len(rec.PackIntent))
		for ref := range rec.PackIntent {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		for _, ref := range refs {
			if _, pinned := rec.PackTaken[ref]; pinned {
				continue
			}
			// The charge id is the consume key: a replay returns the entry an
			// earlier attempt applied, and its Amount — not the request — is
			// what the pack actually covered.
			_, entry, _, err := a.packs.Consume(ref, req.ProductCode, rec.PackIntent[ref], chargeID, chargeID)
			switch {
			case err == nil:
				rec.PackTaken[ref] = entry.Amount
			case errors.Is(err, reservepack.ErrPackTerminal), errors.Is(err, reservepack.ErrPackExhausted),
				errors.Is(err, reservepack.ErrInsufficientQuota), errors.Is(err, reservepack.ErrPackExpired),
				errors.Is(err, reservepack.ErrPackNotFound):
				// The pack could not cover anything after all: its share is
				// NOT recorded as pack coverage, it moves to coupons/cash.
				rec.PackTaken[ref] = 0
			case errors.Is(err, reservepack.ErrVersionConflict):
				// A concurrent writer (a refund, an expiry sweep) moved the
				// pack under us; the caller can retry the same aggregate (the
				// plan keeps what was taken).
				a.saveProgress(rec)
				return billing.Settlement{}, nil, &apiError{http.StatusConflict, "Billing.ConcurrentUpdate", "concurrent pack update, retry"}
			default:
				slog.Error("pack consume failed", "account", acct, "pack", ref, "err", err)
				a.saveProgress(rec)
				return billing.Settlement{}, nil, errSettleInternal
			}
		}
		rec.PacksDone = true
		if rec.Version > 0 {
			if err := a.settles.SaveSettle(rec); err != nil {
				slog.Error("settle plan write failed", "account", acct, "charge", chargeID, "err", err)
				if errors.Is(err, billing.ErrSettleConflict) {
					return billing.Settlement{}, nil, &apiError{http.StatusConflict, "Billing.ConcurrentUpdate", "concurrent settlement update, retry"}
				}
				return billing.Settlement{}, nil, errSettleInternal
			}
		}
	}

	for attempt := 0; attempt < 3; attempt++ {
		cash, apiErr := a.cashPool(acct, chargeID)
		if apiErr != nil {
			return billing.Settlement{}, nil, apiErr
		}
		pools := append(append(append([]billing.Pool{}, registry...), pinnedPacks(rec.PackTaken)...), cash...)
		s, err := a.engine.Settle(usage, unitPrice, req.ProductCode, req.SnapshotID, pools)
		if err != nil {
			slog.Warn("settle rejected", "account", acct, "agg", req.AggID, "err", err)
			return billing.Settlement{}, nil, &apiError{http.StatusBadRequest, "Billing.SettleFailed", "settlement failed"}
		}
		if !s.Charge.PayAmount.IsZero() {
			_, _, err := a.ledger.Consume(acct, s.Charge.PayAmount, "bill", chargeID, chargeID, "settle "+req.AggID)
			switch {
			case err == nil, errors.Is(err, ledger.ErrDuplicateEntry):
				// ErrDuplicateEntry: an earlier attempt already debited exactly
				// this — idempotent success.
			case errors.Is(err, ledger.ErrInsufficientBalance), errors.Is(err, ledger.ErrVersionConflict):
				// The balance moved since it was read: recompute against it.
				continue
			case errors.Is(err, ledger.ErrIdempotencyMismatch):
				slog.Error("settle cash key conflict", "account", acct, "charge", chargeID, "err", err)
				return billing.Settlement{}, nil, &apiError{http.StatusConflict, "Billing.IdempotencyConflict", "charge id already used for a different debit"}
			default:
				slog.Error("ledger consume failed", "account", acct, "charge", chargeID, "err", err)
				return billing.Settlement{}, nil, errSettleInternal
			}
		}
		// Only what came from the registry is the caller's to commit: store
		// packs are committed in the pack ledger, cash in the cash ledger.
		registryDeductions := make([]billing.Deduction, 0, len(s.Charge.Deductions))
		for _, d := range s.Charge.Deductions {
			if d.Source == billing.SourceBalance || (d.Source == billing.SourceResourcePack && storePacks[d.Ref]) {
				continue
			}
			registryDeductions = append(registryDeductions, d)
		}
		return s, registryDeductions, nil
	}
	return billing.Settlement{}, nil, &apiError{http.StatusConflict, "Billing.ConcurrentUpdate", "concurrent balance update, retry"}
}

// cashPool is the account's cash as a waterfall pool, read from the ledger. If
// this charge's cash share was already debited (an attempt that failed or died
// after the debit), the pool is pinned at that debit so the retry reproduces
// it instead of computing a different figure under the same key.
func (a *app) cashPool(acct int64, chargeID string) ([]billing.Pool, *apiError) {
	if prior, found, err := a.ledger.FindEntry(acct, chargeID); err != nil {
		slog.Error("settle cash lookup failed", "account", acct, "charge", chargeID, "err", err)
		return nil, errSettleInternal
	} else if found {
		if prior.Type != ledger.EntryConsume {
			return nil, &apiError{http.StatusConflict, "Billing.IdempotencyConflict", "charge id already used for a different ledger entry"}
		}
		return []billing.Pool{{Source: billing.SourceBalance, Available: prior.Amount}}, nil
	}
	bal, err := a.ledger.Balance(acct)
	if err != nil {
		slog.Error("balance lookup failed", "account", acct, "err", err)
		return nil, errSettleInternal
	}
	if bal.Available.IsZero() || bal.Available.IsNegative() {
		return nil, nil
	}
	return []billing.Pool{{Source: billing.SourceBalance, Available: bal.Available}}, nil
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
	// Charges come from the settle store, so a bill survives restarts and is
	// the same on every replica.
	charges, err := a.settles.ListCharges(acct, period)
	if err != nil {
		slog.Error("bill charge lookup failed", "account", acct, "period", period, "err", err)
		writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		return
	}
	bill := billing.Summarize(acct, period, charges)
	writeJSON(w, http.StatusOK, map[string]any{
		"AccountId":         bill.AccountID,
		"BillPeriod":        bill.BillPeriod,
		"TotalAmount":       bill.TotalAmount.String(),
		"PaidAmount":        bill.PaidAmount.String(),
		"ChargeCount":       bill.ChargeCount,
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
			FaceValue   string `json:"faceValue"` // decimal yuan
			ExpireAt    string `json:"expireAt"`  // RFC3339, empty = no expiry
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
		if face.IsZero() || face.IsNegative() {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "faceValue must be positive")
			return
		}
		if strings.TrimSpace(req.OrderKey) == "" {
			// The order key is both the idempotency key of the purchase and the
			// key tying the quota to a settled order.
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "orderKey is required")
			return
		}
		if strings.TrimSpace(req.PackID) == "" {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "packId is required")
			return
		}
		// Column widths: resource_pack.pack_id / sku_code VARCHAR(64),
		// product_code VARCHAR(32), and the order key is the ledger biz_key
		// (VARCHAR(64)).
		if len(req.PackID) > 64 || len(req.SKUCode) > 64 || len(req.ProductCode) > 32 || len(req.OrderKey) > ledger.MaxKeyLen {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "packId, skuCode, productCode or orderKey is too long")
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
		a.packMu.Lock()
		defer a.packMu.Unlock()
		// Pre-flight the pack state BEFORE taking money: re-purchasing a pack id
		// under a different order key is rejected by the ledger, and discovering
		// that after the debit would need a compensating refund. Pack ids are
		// global, so a pack owned by ANOTHER account is refused outright — never
		// replayed to this caller as if it were theirs.
		if existing, ok, err := a.packStore.GetPack(req.PackID); err != nil {
			slog.Error("pack lookup failed", "account", acct, "pack", req.PackID, "err", err)
			writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
			return
		} else if ok {
			if existing.AccountID != acct {
				writeErr(w, http.StatusConflict, "Billing.PackExists", "pack id already purchased")
				return
			}
			// Same order key: idempotent replay, nothing to charge again.
			if _, found, err := a.packStore.FindByIdempotencyKey(req.PackID, req.OrderKey); err != nil {
				slog.Error("pack idempotency lookup failed", "account", acct, "pack", req.PackID, "err", err)
				writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
				return
			} else if found {
				writeJSON(w, http.StatusOK, packToMap(existing))
				return
			}
			writeErr(w, http.StatusConflict, "Billing.PackExists", "pack id already purchased")
			return
		}

		// A pack is BOUGHT. Crediting quota without collecting its price is a
		// free-quota mint (and, since packs are waterfall rank 0, free compute).
		// Debit the face value from the cash balance first; production does the
		// debit, the credit and the journal write in one local transaction.
		//
		// The ledger key binds the pack AND the order. Keyed on the order alone,
		// buying pack A (¥1) and then pack B (¥1,000,000) under one orderKey
		// replayed A's debit as B's payment — B was free — and a failed B then
		// "refunded" ¥1,000,000 that was never taken.
		payKey := packLedgerKey("pack:", req.PackID, req.OrderKey)
		undoKey := packLedgerKey("pack-undo:", req.PackID, req.OrderKey)
		_, _, err = a.ledger.Consume(acct, face, "pack", req.OrderKey, payKey,
			"purchase resource pack "+req.PackID)
		switch {
		case err == nil:
		case errors.Is(err, ledger.ErrDuplicateEntry):
			// An earlier attempt at THIS purchase (same pack, order and price)
			// already paid. If that attempt also reversed its payment, the
			// purchase is unpaid: replaying the debit would mint a free pack.
			if _, reversed, ferr := a.ledger.FindEntry(acct, undoKey); ferr != nil {
				slog.Error("pack reversal lookup failed", "account", acct, "pack", req.PackID, "err", ferr)
				writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
				return
			} else if reversed {
				writeErr(w, http.StatusConflict, "Billing.PurchaseReversed", "this purchase was reversed; use a new order key")
				return
			}
		case errors.Is(err, ledger.ErrIdempotencyMismatch):
			writeErr(w, http.StatusConflict, "Billing.IdempotencyConflict", "order key already used for a different payment")
			return
		case errors.Is(err, ledger.ErrInsufficientBalance):
			writeErr(w, http.StatusConflict, "Billing.InsufficientBalance", "insufficient balance for the pack")
			return
		default:
			slog.Error("pack payment failed", "account", acct, "pack", req.PackID, "err", err)
			writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
			return
		}
		pack, _, err := a.packs.Purchase(req.PackID, acct, req.ProductCode, req.SKUCode, face, expire, req.OrderKey)
		if err == nil && pack.AccountID != acct {
			// Lost a cross-instance race to another account's purchase of the
			// same pack id and order key: that pack is not ours.
			err = reservepack.ErrPackExists
		}
		if err != nil {
			// The pack was not credited to this purchase. Give back exactly the
			// debit this purchase made — the journal's amount, under a key that
			// can be applied once — and nothing when no debit exists.
			if _, credited, ferr := a.packStore.FindByIdempotencyKey(req.PackID, req.OrderKey); ferr == nil && credited {
				// A concurrent attempt (another instance) completed this very
				// purchase for this account: it is paid and credited.
				if cur, ok, gerr := a.packStore.GetPack(req.PackID); gerr == nil && ok && cur.AccountID == acct {
					writeJSON(w, http.StatusOK, packToMap(cur))
					return
				}
			}
			if paid, found, ferr := a.ledger.FindEntry(acct, payKey); ferr != nil {
				slog.Error("pack purchase compensation lookup failed", "account", acct, "pack", req.PackID, "err", ferr)
			} else if found && paid.Type == ledger.EntryConsume {
				if _, _, rerr := a.ledger.Refund(acct, paid.Amount, req.OrderKey, undoKey,
					"pack purchase reversed "+req.PackID); rerr != nil && !errors.Is(rerr, ledger.ErrDuplicateEntry) {
					slog.Error("pack purchase compensation failed", "account", acct, "pack", req.PackID, "err", rerr)
				}
			}
			slog.Warn("pack purchase failed", "account", acct, "pack", req.PackID, "err", err)
			writeErr(w, http.StatusBadRequest, "Billing.PurchaseFailed", "pack purchase failed")
			return
		}
		writeJSON(w, http.StatusOK, packToMap(pack))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// packLedgerKey names a pack purchase's ledger movement under prefix ("pack:"
// for the debit, "pack-undo:" for its reversal). It binds the pack id AND the
// order key. The readable form prefix+packID+":"+orderKey is used when it fits
// the journal's VARCHAR(64); otherwise prefix+"sha256-"+digest of both. The two
// forms cannot collide: a readable key always has a ':' after the prefix, a
// digest never does.
func packLedgerKey(prefix, packID, orderKey string) string {
	if k := prefix + packID + ":" + orderKey; len(k) <= ledger.MaxKeyLen {
		return k
	}
	sum := sha256.Sum256([]byte(packID + "\x00" + orderKey))
	k := prefix + "sha256-" + hex.EncodeToString(sum[:])
	return k[:ledger.MaxKeyLen]
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
		invs, err := a.invoices.List(acct)
		if err != nil {
			slog.Error("invoice list failed", "account", acct, "err", err)
			writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
			return
		}
		out := make([]map[string]any, 0, len(invs))
		for _, inv := range invs {
			out = append(out, invoiceToMap(inv))
		}
		writeJSON(w, http.StatusOK, map[string]any{"invoices": out, "total": len(out)})
	case http.MethodPost:
		var req struct {
			InvoiceID  string          `json:"invoiceId"`
			BillPeriod string          `json:"billPeriod"`
			Title      string          `json:"title"`
			TaxNo      string          `json:"taxNo"`
			Items      []invoiceLineIn `json:"items"`
		}
		limitBody(w, r)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed request")
			return
		}
		if msg := invoiceFieldsInvalid(req.InvoiceID, req.BillPeriod, req.Title, req.TaxNo); msg != "" {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", msg)
			return
		}
		items := make([]invoice.LineItem, 0, len(req.Items))
		for _, it := range req.Items {
			amt, err := pricing.ParseAmount(it.Amount)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "invalid item amount")
				return
			}
			if amt <= 0 {
				writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "item amounts must be positive")
				return
			}
			// invoice_item.amount is DECIMAL(12,2): a 发票 is issued in whole 分.
			if int64(amt)%10_000 != 0 {
				writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "item amounts must be whole 分 (at most 2 decimals)")
				return
			}
			if utf8.RuneCountInString(it.Description) > maxInvoiceTextRunes {
				writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "item description is too long")
				return
			}
			items = append(items, invoice.LineItem{Description: it.Description, Amount: amt})
		}
		spent, err := a.cashSpent(acct)
		if err != nil {
			slog.Error("invoiceable amount lookup failed", "account", acct, "err", err)
			writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
			return
		}
		inv, err := a.invoices.Draft(acct, req.InvoiceID, req.BillPeriod, req.Title, req.TaxNo, items, spent)
		if err != nil {
			var exceeds *exceedsSpendError
			switch {
			case errors.As(err, &exceeds):
				writeErr(w, http.StatusConflict, "Billing.InvoiceExceedsSpend",
					"开票金额超过可开票额度(已消费金额减去已开票金额): 可开 "+exceeds.Avail.String())
			case errors.Is(err, invoice.ErrInvoiceExists):
				writeErr(w, http.StatusConflict, "Billing.InvoiceIdTaken", "invoice id is not available")
			case isInvoiceRuleError(err):
				slog.Warn("invoice draft refused", "account", acct, "invoice", req.InvoiceID, "err", err)
				writeErr(w, http.StatusBadRequest, "Billing.InvoiceDraftFailed", "invoice draft failed")
			default:
				slog.Error("invoice draft failed", "account", acct, "invoice", req.InvoiceID, "err", err)
				writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
			}
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

// Column widths of svc-billing V3's invoice tables. A longer value is a client
// error, not a 500 (or, without strict mode, a silently truncated tax document).
const (
	maxInvoiceIDLen     = 64  // invoice.invoice_id
	maxInvoiceTextRunes = 256 // invoice.title, invoice_item.description
	maxTaxNoRunes       = 64  // invoice.tax_no
)

// invoiceFieldsInvalid checks a draft's fields against the columns they land in.
func invoiceFieldsInvalid(id, period, title, taxNo string) string {
	switch {
	case len(id) > maxInvoiceIDLen:
		return "invoiceId is too long"
	case period != "" && !validBillPeriod(period):
		return "billPeriod must be YYYY-MM"
	case utf8.RuneCountInString(title) > maxInvoiceTextRunes:
		return "title is too long"
	case utf8.RuneCountInString(taxNo) > maxTaxNoRunes:
		return "taxNo is too long"
	}
	return ""
}

// validBillPeriod accepts a YYYY-MM month (invoice.bill_period CHAR(7)).
func validBillPeriod(p string) bool {
	_, err := time.Parse("2006-01", p)
	return err == nil && len(p) == 7
}

// isInvoiceRuleError reports a domain refusal (pkg-go/invoice) — a client
// error, as opposed to a store failure.
func isInvoiceRuleError(err error) bool {
	for _, e := range []error{invoice.ErrInvoiceNotFound, invoice.ErrInvoiceNotDraft, invoice.ErrInvoiceExists,
		invoice.ErrInvoiceTerminal, invoice.ErrInvoiceAlreadyVoid, invoice.ErrMissingReversalID,
		invoice.ErrReversalIDTaken, invoice.ErrVoidAmountMismatch, invoice.ErrEmptyTitle,
		invoice.ErrEmptyItems, invoice.ErrNonPositiveItem} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
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
	var req struct {
		InvoiceID string `json:"invoiceId"`
	}
	limitBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed request")
		return
	}
	// Re-check the quota at issue time (inside the store, under the account's
	// invoice lock): a refund since the draft may have shrunk what the account
	// has actually paid.
	spent, err := a.cashSpent(acct)
	if err != nil {
		slog.Error("invoiceable amount lookup failed", "account", acct, "err", err)
		writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		return
	}
	inv, err := a.invoices.Issue(acct, req.InvoiceID, spent)
	if err != nil {
		var exceeds *exceedsSpendError
		switch {
		case errors.Is(err, invoice.ErrInvoiceNotFound):
			// IDOR guard: a foreign or unknown invoice is 404 (not 403) so
			// IDs cannot be enumerated.
			writeErr(w, http.StatusNotFound, "Billing.InvoiceNotFound", "invoice not found")
		case errors.As(err, &exceeds):
			writeErr(w, http.StatusConflict, "Billing.InvoiceExceedsSpend", "开票金额超过可开票额度: 可开 "+exceeds.Avail.String())
		case isInvoiceRuleError(err):
			slog.Warn("invoice issue refused", "account", acct, "invoice", req.InvoiceID, "err", err)
			writeErr(w, http.StatusBadRequest, "Billing.InvoiceIssueFailed", "invoice issue failed")
		default:
			slog.Error("invoice issue failed", "account", acct, "invoice", req.InvoiceID, "err", err)
			writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		}
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
		OriginalID string `json:"originalId"`
		ReversalID string `json:"reversalId"`
	}
	limitBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed request")
		return
	}
	if strings.TrimSpace(req.ReversalID) == "" {
		// The 红冲 document needs its own id; the store refuses an empty one
		// (and refuses to overwrite an existing document with it).
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "reversalId is required")
		return
	}
	if len(req.ReversalID) > maxInvoiceIDLen {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "reversalId is too long")
		return
	}
	reversal, err := a.invoices.Void(acct, req.OriginalID, req.ReversalID)
	if err != nil {
		switch {
		case errors.Is(err, invoice.ErrInvoiceNotFound):
			// IDOR guard: foreign/unknown invoices are 404 so IDs cannot be
			// enumerated.
			writeErr(w, http.StatusNotFound, "Billing.InvoiceNotFound", "invoice not found")
		case isInvoiceRuleError(err):
			slog.Warn("invoice void refused", "account", acct, "invoice", req.OriginalID, "err", err)
			writeErr(w, http.StatusBadRequest, "Billing.InvoiceVoidFailed", "invoice void failed")
		default:
			slog.Error("invoice void failed", "account", acct, "invoice", req.OriginalID, "err", err)
			writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		}
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
	// The same stored charges as /internal/bills: a cost figure and a bill
	// figure must never disagree.
	charges, err := a.settles.ListCharges(acct, period)
	if err != nil {
		slog.Error("cost analysis charge lookup failed", "account", acct, "period", period, "err", err)
		writeErr(w, http.StatusInternalServerError, "Billing.InternalError", "internal error")
		return
	}
	report := billing.CostAnalysis(acct, period, charges)
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
		"invoiceId":  inv.InvoiceID,
		"accountId":  inv.AccountID,
		"billPeriod": inv.BillPeriod,
		"title":      inv.Title,
		"taxNo":      inv.TaxNo,
		"amount":     inv.Amount.String(),
		"status":     inv.Status,
		"issuedAt":   issued,
		"voidedAt":   voided,
		"voidedById": inv.VoidedByID,
		"reversesId": inv.ReversesID,
		"items":      items,
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
//
// Deductions are matched on (Source, Ref): a pool's identity is both, and a
// pool with an empty Ref is still a pool that must shrink when spent — skipping
// Ref == "" left such a pool permanently full. Cash is never a registry pool
// (the ledger holds it), so a BALANCE deduction is ignored here.
func (p *poolRegistry) commit(acct int64, refs []string, deductions []billing.Deduction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	type poolKey struct {
		source billing.DeductionSource
		ref    string
	}
	spent := make(map[poolKey]pricing.Amount)
	for _, d := range deductions {
		if d.Source == billing.SourceBalance {
			continue // cash portion is handled by the ledger
		}
		k := poolKey{d.Source, d.Ref}
		spent[k] = spent[k].Add(d.Amount)
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
		k := poolKey{cur[i].Source, cur[i].Ref}
		if amt, ok := spent[k]; ok {
			// One deduction per waterfall pool: spend it on the first match
			// only, so two registry pools sharing (Source, Ref) are not both
			// charged for it.
			cur[i].Available = cur[i].Available.Sub(amt)
			delete(spent, k)
		}
	}
}

// runtimeRequireSecrets enforces the production secret set (pkg-go/runmode).
func runtimeRequireSecrets() {
	runmode.MustRequireSecrets(httpmw.InternalTokenEnv)
}
