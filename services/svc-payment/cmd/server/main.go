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
	"sync"
	"syscall"
	"time"

	"github.com/starcloud/sc-platform/ledger"
	"github.com/starcloud/sc-platform/pricing"
)

// accountIDHeader carries the caller's account id. TRUST NOTE: this header is
// injected by the API gateway after authentication; this service trusts it and
// must never be exposed directly to the public network. Deployments that want
// defense-in-depth set SC_INTERNAL_TOKEN, which makes the service additionally
// require a matching X-Sc-Internal-Token shared-secret header (dev default:
// unset, check disabled).
const accountIDHeader = "X-Sc-Account-Id"

const internalTokenHeader = "X-Sc-Internal-Token"

// maxBodyBytes bounds request bodies before JSON decoding (~1MB).
const maxBodyBytes = 1 << 20

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

// paymentServer wires the ledger to HTTP handlers. Idempotency keys are always
// client-supplied (channel outTradeNo for recharge, order id for
// freeze/consume) — the server never fabricates one.
type paymentServer struct {
	ledger *ledger.Ledger
}

func newPaymentServer() *paymentServer {
	store := newInMemStore()
	var (
		seqMu sync.Mutex
		seq   int64
	)
	l := ledger.New(store, time.Now, func() int64 {
		seqMu.Lock()
		defer seqMu.Unlock()
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

// amountRequest is the body for recharge / freeze / consume. The idempotency
// key MUST be supplied by the client: idempotencyKey directly, or outTradeNo
// (the payment-channel transaction number, used for recharge).
type amountRequest struct {
	AmountMinor int64  `json:"amountMinor"` // 分; must be positive
	Reason      string `json:"reason"`      // biz remark for the journal entry
	BizKey      string `json:"bizKey"`      // optional, e.g. order id
	IdemKey     string `json:"idempotencyKey"` // required unless outTradeNo is set
	OutTradeNo  string `json:"outTradeNo"`     // channel transaction no; fallback idempotency key
}

// decodeAmountRequest parses and validates the shared body. Returns false if a
// 4xx response was already written.
func decodeAmountRequest(w http.ResponseWriter, r *http.Request) (amountRequest, bool) {
	var req amountRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return req, false
	}
	if req.AmountMinor <= 0 {
		writeErr(w, "Common.InvalidParameter", 400, "amountMinor must be positive")
		return req, false
	}
	if req.IdemKey == "" {
		req.IdemKey = req.OutTradeNo
	}
	if req.IdemKey == "" {
		writeErr(w, "Common.InvalidParameter", 400, "idempotencyKey or outTradeNo is required")
		return req, false
	}
	return req, true
}

// writeEntryResult writes the 200 envelope for a ledger operation, marking
// idempotent replays (ErrDuplicateEntry returns the prior entry + balance).
func writeEntryResult(w http.ResponseWriter, entry ledger.Entry, bal ledger.Balance, idempotent bool) {
	writeJSON(w, "OK", map[string]any{
		"entryId":     entry.EntryID,
		"type":        string(entry.Type),
		"amountMinor": amountToMinor(entry.Amount),
		"balance":     balanceToMap(bal),
		"idempotent":  idempotent,
	})
}

func (ps *paymentServer) handleRecharge(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	req, ok := decodeAmountRequest(w, r)
	if !ok {
		return
	}
	bizKey := req.BizKey
	if bizKey == "" {
		bizKey = "recharge"
	}
	entry, bal, err := ps.ledger.Recharge(acct, minorToAmount(req.AmountMinor),
		bizKey, req.IdemKey, req.Reason)
	if err != nil && !errors.Is(err, ledger.ErrDuplicateEntry) {
		writeLedgerErr(w, err)
		return
	}
	writeEntryResult(w, entry, bal, errors.Is(err, ledger.ErrDuplicateEntry))
}

func (ps *paymentServer) handleBalance(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	bal, err := ps.ledger.Balance(acct)
	if err != nil {
		slog.Error("balance query failed", "account", acct, "err", err)
		writeErr(w, "Payment.BalanceQueryFailed", 500, "balance query failed")
		return
	}
	writeJSON(w, "OK", balanceToMap(bal))
}

func (ps *paymentServer) handleFreeze(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	req, ok := decodeAmountRequest(w, r)
	if !ok {
		return
	}
	bizKey := req.BizKey
	if bizKey == "" {
		bizKey = req.IdemKey
	}
	entry, bal, err := ps.ledger.Freeze(acct, minorToAmount(req.AmountMinor), bizKey, req.IdemKey)
	if err != nil && !errors.Is(err, ledger.ErrDuplicateEntry) {
		writeLedgerErr(w, err)
		return
	}
	writeEntryResult(w, entry, bal, errors.Is(err, ledger.ErrDuplicateEntry))
}

func (ps *paymentServer) handleConsume(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	req, ok := decodeAmountRequest(w, r)
	if !ok {
		return
	}
	bizKey := req.BizKey
	if bizKey == "" {
		bizKey = req.IdemKey
	}
	entry, bal, err := ps.ledger.Consume(acct, minorToAmount(req.AmountMinor),
		"order", bizKey, req.IdemKey, req.Reason)
	if err != nil && !errors.Is(err, ledger.ErrDuplicateEntry) {
		writeLedgerErr(w, err)
		return
	}
	writeEntryResult(w, entry, bal, errors.Is(err, ledger.ErrDuplicateEntry))
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
// ErrDuplicateEntry is handled by callers as an idempotent 200 before reaching
// here. Clients receive only stable codes/messages; err.Error() details go to
// the log.
func writeLedgerErr(w http.ResponseWriter, err error) {
	slog.Warn("ledger operation rejected", "err", err)
	switch {
	case errors.Is(err, ledger.ErrInsufficientBalance):
		writeErr(w, "Payment.InsufficientBalance", 422, "insufficient balance")
	case errors.Is(err, ledger.ErrInsufficientFrozen):
		writeErr(w, "Payment.InsufficientFrozen", 422, "insufficient frozen funds")
	case errors.Is(err, ledger.ErrVersionConflict):
		writeErr(w, "Payment.VersionConflict", 409, "concurrent balance update, please retry")
	case errors.Is(err, ledger.ErrNonPositiveAmount):
		writeErr(w, "Common.InvalidParameter", 400, "amount must be positive")
	case errors.Is(err, ledger.ErrMissingIdempotency):
		writeErr(w, "Common.InvalidParameter", 400, "idempotency key required")
	case errors.Is(err, ledger.ErrDuplicateEntry):
		// Handled by callers via the idempotent flag; reaching here is a bug.
		writeErr(w, "Payment.DuplicateEntry", 409, "duplicate entry")
	default:
		writeErr(w, "Payment.InternalError", 500, "internal error")
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

	httpSrv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(internalTokenMiddleware(mux))), ReadHeaderTimeout: 5 * time.Second}
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
