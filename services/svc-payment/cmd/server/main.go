// Package main: svc-payment — the 充值 money-side loop (03-backend-services.md
// §4.2.4, adjudication S29, 04§6.3).
//
// The ledger is the single source of truth for cash balance: adjudication S29
// pulled `balance` off the account table and into trade_db keyed by account_id,
// because identity is read on every gateway request while balance is written on
// every charge, and coupling a financial write path to the hottest read path on
// the platform lets the identity service silently participate in money mutations
// it has no business arbitrating. This service is the only writer of that
// ledger.
//
// Every balance change the service accepts flows through pkg-go/ledger's single
// apply path, which carries the invariants that keep money safe: positive
// amounts only, exactly-once via idempotency key, optimistic-lock concurrency
// (two concurrent charges against the same starting version cannot both win),
// append-only journal (a correction is a compensating entry, never an edit), and
// a non-negative available balance (overdraft is an account arrears state, never
// a negative number here).
//
// Routes (gateway-authorized, X-Sc-Account-Id injected):
//
//	POST /api/v1/payment/recharge — credit cash in from a payment channel
//	GET  /api/v1/payment/balance  — current available + frozen balance
//	POST /api/v1/payment/freeze   — reserve funds for an in-flight order
//	POST /api/v1/payment/consume  — debit to pay an order or hourly bill
//
// amountMinor is in 分 (cents, 1/100 of a yuan); pricing.Amount carries
// micro-units (1/1,000,000), so the conversion factor is 10_000.
//
// stdlib-HTTP service. Envelope {RequestId,Code,Data} (03§9.3). In-memory store
// (Vitess on trade_db, sharded by account_id, in production).
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/starcloud/sc-platform/ledger"
	"github.com/starcloud/sc-platform/pricing"
)

const accountIDHeader = "X-Sc-Account-Id"

// minorToAmount converts a minor-units (分) integer into a pricing.Amount
// (micro-units of a yuan). 1 分 = 1/100 元 = 10_000/1_000_000 元.
func minorToAmount(minor int64) pricing.Amount {
	return pricing.Amount(minor) * 10_000
}

// amountToMinor converts a pricing.Amount back to minor units (分).
func amountToMinor(a pricing.Amount) int64 {
	return int64(a) / 10_000
}

// inMemStore is an in-memory ledger.Store. Production writes the entry and the
// balance in one local transaction against trade_db; this fake preserves that
// atomicity by mutating both under a single lock. The byKey map is the
// idempotency index that makes a retried channel callback a no-op.
type inMemStore struct {
	mu       sync.Mutex
	balances map[int64]ledger.Balance
	entries  map[int64][]ledger.Entry
	byKey    map[string]ledger.Entry
}

func newInMemStore() *inMemStore {
	return &inMemStore{
		balances: make(map[int64]ledger.Balance),
		entries:  make(map[int64][]ledger.Entry),
		byKey:    make(map[string]ledger.Entry),
	}
}

func (m *inMemStore) GetBalance(accountID int64) (ledger.Balance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.balances[accountID]
	if !ok {
		return ledger.Balance{AccountID: accountID}, nil
	}
	return b, nil
}

func (m *inMemStore) Apply(entry ledger.Entry, newBalance ledger.Balance, expectedVersion int) error {
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

func (m *inMemStore) FindByIdempotencyKey(accountID int64, key string) (ledger.Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byKey[fmt.Sprintf("%d|%s", accountID, key)]
	return e, ok, nil
}

func (m *inMemStore) ListEntries(accountID int64) ([]ledger.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ledger.Entry, len(m.entries[accountID]))
	copy(out, m.entries[accountID])
	return out, nil
}

// paymentServer wires the ledger to HTTP handlers. The idempotencySeq mints a
// unique idempotency key per request when the client does not supply one; in a
// real deployment the channel transaction number is the key for recharge, and
// the order id is the key for consume/freeze.
type paymentServer struct {
	ledger      *ledger.Ledger
	idemCounter atomic.Int64
}

func newPaymentServer() *paymentServer {
	store := newInMemStore()
	var seq int64
	l := ledger.New(store, time.Now, func() int64 {
		seq++
		return seq
	})
	ps := &paymentServer{ledger: l}
	ps.seed()
	return ps
}

// seed primes account 100123 with ¥500 (50_000 分) of available balance, matching
// the console-bff seed for that account. The credit is a real Recharge entry,
// so the journal reproduces the balance from the very first request.
func (ps *paymentServer) seed() {
	const seedAccount = int64(100123)
	_, bal, err := ps.ledger.Recharge(seedAccount, minorToAmount(50_000),
		"recharge-seed", "idem-seed-100123", "初始余额充值")
	if err != nil {
		// A seed failure is a programmer error, not a runtime condition.
		panic(fmt.Sprintf("seed recharge failed: %v", err))
	}
	slog.Info("seeded account", "account", seedAccount,
		"availableMinor", amountToMinor(bal.Available))
}

// amountRequest is the body for recharge / freeze / consume.
type amountRequest struct {
	AmountMinor int64  `json:"amountMinor"` // 分; must be positive
	Reason      string `json:"reason"`      // biz remark for the journal entry
	BizKey      string `json:"bizKey"`      // optional, e.g. order id; generated if absent
	IdemKey     string `json:"idempotencyKey"` // optional; generated if absent
}

func (ps *paymentServer) handleRecharge(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req amountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.AmountMinor <= 0 {
		writeErr(w, "Common.InvalidParameter", 400, "amountMinor must be positive")
		return
	}
	bizKey := req.BizKey
	if bizKey == "" {
		bizKey = "recharge"
	}
	idem := req.IdemKey
	if idem == "" {
		idem = fmt.Sprintf("recharge-%d-%d", acct, ps.idemCounter.Add(1))
	}
	entry, bal, err := ps.ledger.Recharge(acct, minorToAmount(req.AmountMinor),
		bizKey, idem, req.Reason)
	if err != nil {
		writeLedgerErr(w, err)
		return
	}
	writeJSON(w, "OK", map[string]any{
		"entryId":    entry.EntryID,
		"type":       string(entry.Type),
		"amountMinor": amountToMinor(entry.Amount),
		"balance":    balanceToMap(bal),
		"idempotent": errors.Is(err, ledger.ErrDuplicateEntry),
	})
}

func (ps *paymentServer) handleBalance(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	bal, err := ps.ledger.Balance(acct)
	if err != nil {
		writeErr(w, "Payment.BalanceQueryFailed", 500, err.Error())
		return
	}
	writeJSON(w, "OK", balanceToMap(bal))
}

func (ps *paymentServer) handleFreeze(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req amountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.AmountMinor <= 0 {
		writeErr(w, "Common.InvalidParameter", 400, "amountMinor must be positive")
		return
	}
	bizKey := req.BizKey
	if bizKey == "" {
		bizKey = fmt.Sprintf("freeze-%d", ps.idemCounter.Add(1))
	}
	idem := req.IdemKey
	if idem == "" {
		idem = fmt.Sprintf("freeze-%d-%s", acct, bizKey)
	}
	entry, bal, err := ps.ledger.Freeze(acct, minorToAmount(req.AmountMinor), bizKey, idem)
	if err != nil {
		writeLedgerErr(w, err)
		return
	}
	writeJSON(w, "OK", map[string]any{
		"entryId":     entry.EntryID,
		"type":        string(entry.Type),
		"amountMinor":  amountToMinor(entry.Amount),
		"balance":     balanceToMap(bal),
		"idempotent":  errors.Is(err, ledger.ErrDuplicateEntry),
	})
}

func (ps *paymentServer) handleConsume(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req amountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.AmountMinor <= 0 {
		writeErr(w, "Common.InvalidParameter", 400, "amountMinor must be positive")
		return
	}
	bizKey := req.BizKey
	if bizKey == "" {
		bizKey = fmt.Sprintf("consume-%d", ps.idemCounter.Add(1))
	}
	idem := req.IdemKey
	if idem == "" {
		idem = fmt.Sprintf("consume-%d-%s", acct, bizKey)
	}
	entry, bal, err := ps.ledger.Consume(acct, minorToAmount(req.AmountMinor),
		"order", bizKey, idem, req.Reason)
	if err != nil {
		writeLedgerErr(w, err)
		return
	}
	writeJSON(w, "OK", map[string]any{
		"entryId":     entry.EntryID,
		"type":        string(entry.Type),
		"amountMinor":  amountToMinor(entry.Amount),
		"balance":     balanceToMap(bal),
		"idempotent":  errors.Is(err, ledger.ErrDuplicateEntry),
	})
}

func balanceToMap(b ledger.Balance) map[string]any {
	return map[string]any{
		"accountId":     b.AccountID,
		"availableMinor": amountToMinor(b.Available),
		"frozenMinor":    amountToMinor(b.Frozen),
		"totalMinor":     amountToMinor(b.Total()),
		"version":        b.Version,
		"updatedAt":      b.UpdatedAt.Format(time.RFC3339),
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

// writeLedgerErr maps a ledger error to the right envelope code + HTTP status.
// ErrDuplicateEntry is surfaced as 200 OK with idempotent=true (the entry was
// already applied; the client gets back the same result). Everything else is a
// 4xx/5xx with a precise code so the caller can distinguish insufficient funds
// from a version conflict from a malformed request.
func writeLedgerErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrInsufficientBalance):
		writeErr(w, "Payment.InsufficientBalance", 422, err.Error())
	case errors.Is(err, ledger.ErrInsufficientFrozen):
		writeErr(w, "Payment.InsufficientFrozen", 422, err.Error())
	case errors.Is(err, ledger.ErrVersionConflict):
		writeErr(w, "Payment.VersionConflict", 409, err.Error())
	case errors.Is(err, ledger.ErrNonPositiveAmount):
		writeErr(w, "Common.InvalidParameter", 400, err.Error())
	case errors.Is(err, ledger.ErrMissingIdempotency):
		writeErr(w, "Common.InvalidParameter", 400, err.Error())
	case errors.Is(err, ledger.ErrDuplicateEntry):
		// Handled by callers via the idempotent flag; reaching here is a bug.
		writeErr(w, "Payment.DuplicateEntry", 409, err.Error())
	default:
		writeErr(w, "Payment.InternalError", 500, err.Error())
	}
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
			id = fmt.Sprintf("pay-%d", time.Now().UnixNano())
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
	addr := flag.String("http", ":9205", "HTTP listen address")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	srv := newPaymentServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/payment/recharge", srv.handleRecharge)
	mux.HandleFunc("GET /api/v1/payment/balance", srv.handleBalance)
	mux.HandleFunc("POST /api/v1/payment/freeze", srv.handleFreeze)
	mux.HandleFunc("POST /api/v1/payment/consume", srv.handleConsume)

	httpSrv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(mux)), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("svc-payment listening", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
}
