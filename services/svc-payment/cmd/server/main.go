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
// Routes (gateway-authorized, X-Euler-Account-Id injected):
//
//	POST /api/v1/payment/recharge — credit cash in from a payment channel; only
//	                                accepted with a valid channel signature
//	                                (EULER_PAYMENT_CHANNEL_SECRET), never from
//	                                a tenant
//	GET  /api/v1/payment/balance  — current available + frozen balance
//	POST /api/v1/payment/freeze   — reserve funds for an in-flight order
//	POST /api/v1/payment/consume  — debit to pay an order or hourly bill
//
// Internal routes (service-to-service only; never routed by the gateway):
//
//	POST /internal/payment/consume — debit under a key in the reserved
//	                                 internal namespace (svc-order's
//	                                 order-pay-{id}); tenant routes refuse
//	                                 those keys, so a tenant cannot pre-plant
//	                                 an entry the owning service would accept
//	                                 as "already paid"
//	POST /internal/payment/refund  — refund exactly one earlier internal
//	                                 debit, idempotently (refund:{consumeKey})
//
// amountMinor is in 分 (cents, 1/100 of a yuan); pricing.Amount carries
// micro-units (1/1,000,000), so the conversion factor is 10_000.
//
// stdlib-HTTP service. Envelope {RequestId,Code,Data} (03§9.3). In-memory store
// (Vitess on trade_db, sharded by account_id, in production).
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	"unicode/utf8"

	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/ledger"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/runmode"
	"github.com/qifalab/euler-platform/storage"
)

// accountIDHeader carries the caller's account id. TRUST NOTE: this header is
// injected by the API gateway after authentication; this service trusts it and
// must never be exposed directly to the public network. Deployments that want
// defense-in-depth set EULER_INTERNAL_TOKEN, which makes the service additionally
// require a matching X-Euler-Internal-Token shared-secret header (dev default:
// unset, check disabled).
const accountIDHeader = "X-Euler-Account-Id"

// maxBodyBytes bounds request bodies before JSON decoding (~1MB).
const maxBodyBytes = 1 << 20

// minorToAmount converts a minor-units (分) integer into a pricing.Amount
// (micro-units of a yuan). 1 分 = 1/100 元 = 10_000/1_000_000 元.
//
// Precondition: minor must be within the range decodeAmountRequest enforces
// (0 < minor <= maxMinor). The multiplication wraps outside it, and a wrapped
// recharge is a corrupted balance rather than a rejected request.
func minorToAmount(minor int64) pricing.Amount {
	return pricing.Amount(minor) * 10_000
}

// maxMinor bounds amountMinor so that minor × 10_000 cannot overflow int64.
const maxMinor = int64(1<<63-1) / 10_000

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
	// channelSecret authenticates recharge callbacks from the payment channel
	// (EULER_PAYMENT_CHANNEL_SECRET). Empty = recharge is refused outright:
	// a balance credit nobody can prove was paid for is minted money.
	channelSecret []byte
	// persistent reports whether the ledger is backed by trade_db. It is what
	// decides whether the demo seed runs: seeding a shared database would
	// invent ¥500 for account 100123 in every environment pointed at it.
	persistent bool
}

// newPaymentServer wires the ledger. Persistence is opt-in (pkg-go/storage
// doc): with EULER_DB_DSN set the ledger is trade_db's account_balance +
// ledger_entry, so balances survive a restart; unset, the service keeps the
// in-memory store that makes the runnable demo dependency-free.
//
// A configured DSN that cannot be reached — or a schema sqlmigrate never
// touched — is a startup failure, not a runtime surprise: coming up "healthy"
// against an unmigrated database turns the first recharge into a 500.
//
// Entry ids come from the 号段 allocator (storage.Sequence over
// trade_db.id_sequence, 04§6.6) whenever the store is persistent. The in-process
// counter below is only correct for the in-memory store: pointed at a real
// database it restarts at 1 and collides with the journal rows the previous run
// committed.
func newPaymentServer(ctx context.Context) (*paymentServer, error) {
	db, ok, err := storage.MustOpenFor(ctx, "trade_db")
	if err != nil {
		return nil, err
	}
	if !ok {
		return newInMemoryPaymentServer(), nil
	}
	if err := storage.EnsureMigrated(ctx, db, "trade_db"); err != nil {
		return nil, err
	}
	ids, err := storage.OpenSequence(ctx, db, "ledger_entry", 1000)
	if err != nil {
		return nil, err
	}
	return &paymentServer{
		ledger:        ledger.New(ledger.NewSQLStore(db), time.Now, ids.NextFunc()),
		persistent:    true,
		channelSecret: []byte(os.Getenv(channelSecretEnv)),
	}, nil
}

// newInMemoryPaymentServer builds the demo server: in-memory store, local id
// counter, seeded balance. Handler tests construct it directly, so they never
// depend on whether the developer's shell has EULER_DB_DSN set — a test that wrote
// demo recharges into trade_db would be a data incident.
func newInMemoryPaymentServer() *paymentServer {
	ps := &paymentServer{
		ledger:        ledger.New(newInMemStore(), time.Now, localCounter()),
		channelSecret: []byte(os.Getenv(channelSecretEnv)),
	}
	ps.seed()
	return ps
}

// localCounter mints the journal ids of the in-memory store. It is guarded
// because concurrent handlers share it, and it is deliberately not used for the
// persistent store (see newPaymentServer).
func localCounter() func() int64 {
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
	AmountMinor int64  `json:"amountMinor"`    // 分; must be positive
	Reason      string `json:"reason"`         // biz remark for the journal entry
	BizKey      string `json:"bizKey"`         // optional, e.g. order id
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
	if req.AmountMinor > maxMinor {
		writeErr(w, "Common.InvalidParameter", 400, "amountMinor out of range")
		return req, false
	}
	if req.IdemKey == "" {
		req.IdemKey = req.OutTradeNo
	}
	if req.IdemKey == "" {
		writeErr(w, "Common.InvalidParameter", 400, "idempotencyKey or outTradeNo is required")
		return req, false
	}
	// ledger_entry.biz_key is VARCHAR(64) and remark VARCHAR(256): an oversized
	// value is a client error, not a database failure surfacing as a 500.
	if len(req.BizKey) > ledger.MaxKeyLen {
		writeErr(w, "Common.InvalidParameter", 400, "bizKey is too long")
		return req, false
	}
	if utf8.RuneCountInString(req.Reason) > maxReasonRunes {
		writeErr(w, "Common.InvalidParameter", 400, "reason is too long")
		return req, false
	}
	return req, true
}

// maxReasonRunes is the width of ledger_entry.remark (VARCHAR(256)).
const maxReasonRunes = 256

// reservedKeyPrefixes is the internal idempotency-key namespace.
//
// The journal's keyspace is per account and shared by every writer of
// trade_db's ledger: svc-order's order payments, svc-billing's settlements and
// pack purchases, channel recharges and this service's refunds. A tenant that
// could pick a key in that space could pre-plant a 1-分 entry under, say,
// order-pay-{id}, which the owning service would then find as "already paid".
// Tenant-facing endpoints therefore refuse these prefixes and the internal
// endpoints accept nothing else, so the two keyspaces never meet.
var reservedKeyPrefixes = []string{
	"svc-",       // generic internal namespace
	"order-pay-", // svc-order: order payment, order-pay-{orderId}
	"refund:",    // this service: refund of an internal debit, refund:{consumeKey}
	"recharge:",  // this service: channel recharge, recharge:{outTradeNo}
	"pack:",      // svc-billing: resource-pack purchase
	"pack-undo:", // svc-billing: pack purchase compensation
	"chg-",       // svc-billing: hourly settlement (billing.ChargeID)
}

// refundKeyPrefix keys the refund of an internal debit. One refund per debit
// key is what bounds the refunded total by what was actually taken.
const refundKeyPrefix = "refund:"

// reservedKey reports whether key lies in the internal namespace. Matching is
// case-insensitive because the journal's collation is: ORDER-PAY-1 and
// order-pay-1 are the same row to MySQL.
func reservedKey(key string) bool {
	k := strings.ToLower(key)
	for _, p := range reservedKeyPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// keyShapeOK reports whether key is 1..max bytes of printable, space-free
// ASCII. The journal compares keys under a case- and accent-insensitive
// collation; restricting the alphabet keeps "the same key" meaning the same
// thing here and in MySQL (no accented look-alike of a reserved prefix).
func keyShapeOK(key string, max int) bool {
	if key == "" || len(key) > max {
		return false
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// tenantKeyOK validates an idempotency key supplied on a tenant-facing route.
// Returns false if a 400 was already written.
func tenantKeyOK(w http.ResponseWriter, key string) bool {
	if !keyShapeOK(key, ledger.MaxKeyLen) {
		writeErr(w, "Common.InvalidParameter", 400, "idempotencyKey must be 1-64 printable ASCII characters")
		return false
	}
	if reservedKey(key) {
		writeErr(w, "Payment.ReservedIdempotencyKey", 400, "idempotencyKey uses a reserved internal prefix")
		return false
	}
	return true
}

// internalKeyOK validates an idempotency key on an internal route: it must be
// in the reserved namespace. Returns false if a 400 was already written.
func internalKeyOK(w http.ResponseWriter, key string, max int) bool {
	if !keyShapeOK(key, max) || !reservedKey(key) {
		writeErr(w, "Payment.InternalKeyRequired", 400, "internal debits must use a reserved-namespace idempotency key")
		return false
	}
	return true
}

// writeEntryResult writes the 200 envelope for a ledger operation, marking
// idempotent replays (ErrDuplicateEntry returns the prior entry + balance).
func writeEntryResult(w http.ResponseWriter, entry ledger.Entry, bal ledger.Balance, idempotent bool) {
	writeJSON(w, "OK", entryResult(entry, bal, idempotent))
}

func entryResult(entry ledger.Entry, bal ledger.Balance, idempotent bool) map[string]any {
	return map[string]any{
		"entryId":     entry.EntryID,
		"type":        string(entry.Type),
		"amountMinor": amountToMinor(entry.Amount),
		"balance":     balanceToMap(bal),
		"idempotent":  idempotent,
	}
}

// channelSecretEnv names the shared secret the payment channel (or its
// callback adapter) signs recharge notifications with.
const channelSecretEnv = "EULER_PAYMENT_CHANNEL_SECRET"

// channelSignatureHeader carries hex(HMAC-SHA256(secret, rechargeSigningString)).
const channelSignatureHeader = "X-Euler-Channel-Signature"

// rechargeSigningString binds the signature to the account, the amount and the
// channel transaction number, so a captured callback cannot be replayed for a
// different account or amount (and the same outTradeNo is idempotent).
func rechargeSigningString(accountID, amountMinor int64, outTradeNo string) string {
	return fmt.Sprintf("recharge|%d|%d|%s", accountID, amountMinor, outTradeNo)
}

// signRecharge computes the channel signature (exported for the channel
// adapter and tests).
func signRecharge(secret []byte, accountID, amountMinor int64, outTradeNo string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(rechargeSigningString(accountID, amountMinor, outTradeNo)))
	return hex.EncodeToString(mac.Sum(nil))
}

// handleRecharge credits the balance for a payment the CHANNEL confirmed.
//
// It used to credit whatever amount any caller with an X-Euler-Account-Id
// asked for. A recharge now requires: a configured channel secret, the
// channel's transaction number (outTradeNo, which is also the idempotency
// key), and a valid HMAC over account|amount|outTradeNo.
func (ps *paymentServer) handleRecharge(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	if len(ps.channelSecret) == 0 {
		writeErr(w, "Payment.RechargeChannelNotConfigured", 403, "recharge is only accepted from a configured payment channel")
		return
	}
	req, ok := decodeAmountRequest(w, r)
	if !ok {
		return
	}
	if req.OutTradeNo == "" {
		writeErr(w, "Common.InvalidParameter", 400, "outTradeNo (channel transaction number) is required")
		return
	}
	// The ledger key is recharge:{outTradeNo} and must fit the journal's
	// VARCHAR(64) idempotency_key; a longer one would be a 500 (or a
	// truncated, colliding key) instead of a rejected callback.
	if !keyShapeOK(req.OutTradeNo, ledger.MaxKeyLen-len("recharge:")) {
		writeErr(w, "Common.InvalidParameter", 400, "outTradeNo must be 1-55 printable ASCII characters")
		return
	}
	got, err := hex.DecodeString(r.Header.Get(channelSignatureHeader))
	want, _ := hex.DecodeString(signRecharge(ps.channelSecret, acct, req.AmountMinor, req.OutTradeNo))
	if err != nil || !hmac.Equal(got, want) {
		slog.Warn("recharge rejected: bad channel signature", "account", acct, "outTradeNo", req.OutTradeNo)
		writeErr(w, "Payment.InvalidChannelSignature", 403, "invalid channel signature")
		return
	}
	bizKey := req.BizKey
	if bizKey == "" {
		bizKey = "recharge"
	}
	// The channel transaction number is the idempotency key: a retried
	// notification for the same payment is a no-op, whatever key the body names.
	entry, bal, err := ps.ledger.Recharge(acct, minorToAmount(req.AmountMinor),
		bizKey, "recharge:"+req.OutTradeNo, req.Reason)
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
	if !ok || !tenantKeyOK(w, req.IdemKey) {
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
	if !ok || !tenantKeyOK(w, req.IdemKey) {
		return
	}
	ps.consume(w, acct, req)
}

// handleInternalConsume is the service-to-service debit (svc-order paying an
// order). Its keys live in the reserved namespace the tenant routes refuse, so
// whatever entry it finds under its key was written by an internal caller.
func (ps *paymentServer) handleInternalConsume(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	req, ok := decodeAmountRequest(w, r)
	if !ok || !internalKeyOK(w, req.IdemKey, ledger.MaxKeyLen) {
		return
	}
	ps.consume(w, acct, req)
}

func (ps *paymentServer) consume(w http.ResponseWriter, acct int64, req amountRequest) {
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

// refundRequest is the body of POST /internal/payment/refund.
type refundRequest struct {
	// ConsumeKey is the idempotency key of the debit being reversed.
	ConsumeKey string `json:"consumeKey"`
	// AmountMinor is what the caller believes was debited (分); it must match
	// the journal exactly. The refund amount always comes from the journal.
	AmountMinor int64  `json:"amountMinor"`
	Reason      string `json:"reason"`
}

// handleInternalRefund reverses one internal debit — e.g. an order payment
// that lost the race to a cancellation. The refund is keyed refund:{consumeKey},
// so a debit can be refunded at most once however often the compensation is
// retried, and the amount is the journal's, never the caller's. A key with no
// debit behind it is not an error: there is nothing to give back.
func (ps *paymentServer) handleInternalRefund(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req refundRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if !internalKeyOK(w, req.ConsumeKey, ledger.MaxKeyLen-len(refundKeyPrefix)) {
		return
	}
	if req.AmountMinor <= 0 || req.AmountMinor > maxMinor {
		writeErr(w, "Common.InvalidParameter", 400, "amountMinor must be positive")
		return
	}
	if utf8.RuneCountInString(req.Reason) > maxReasonRunes {
		writeErr(w, "Common.InvalidParameter", 400, "reason is too long")
		return
	}
	orig, found, err := ps.ledger.FindEntry(acct, req.ConsumeKey)
	if err != nil {
		slog.Error("refund lookup failed", "account", acct, "key", req.ConsumeKey, "err", err)
		writeErr(w, "Payment.InternalError", 500, "internal error")
		return
	}
	if !found {
		writeJSON(w, "OK", map[string]any{"refunded": false})
		return
	}
	if orig.Type != ledger.EntryConsume {
		writeErr(w, "Payment.NotRefundable", 409, "only a consume entry can be refunded")
		return
	}
	if orig.Amount != minorToAmount(req.AmountMinor) {
		writeErr(w, "Payment.RefundAmountMismatch", 409, "refund amount does not match the debit")
		return
	}
	// The stored key's spelling (not the request's) names the refund: the
	// journal matched case-insensitively, the in-memory store would not.
	entry, bal, err := ps.ledger.Refund(acct, orig.Amount, orig.BizKey,
		refundKeyPrefix+orig.IdempotencyKey, req.Reason)
	if err != nil && !errors.Is(err, ledger.ErrDuplicateEntry) {
		writeLedgerErr(w, err)
		return
	}
	out := entryResult(entry, bal, errors.Is(err, ledger.ErrDuplicateEntry))
	out["refunded"] = true
	writeJSON(w, "OK", out)
}

func balanceToMap(b ledger.Balance) map[string]any {
	return map[string]any{
		"accountId":      b.AccountID,
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
	case errors.Is(err, ledger.ErrKeyTooLong):
		writeErr(w, "Common.InvalidParameter", 400, "idempotency or biz key too long")
	case errors.Is(err, ledger.ErrIdempotencyMismatch):
		// The key already names a DIFFERENT movement: never report it as paid.
		writeErr(w, "Payment.IdempotencyKeyConflict", 409, "idempotency key already used for a different operation")
	case errors.Is(err, ledger.ErrDuplicateEntry):
		// Handled by callers via the idempotent flag; reaching here is a bug.
		writeErr(w, "Payment.DuplicateEntry", 409, "duplicate entry")
	default:
		writeErr(w, "Payment.InternalError", 500, "internal error")
	}
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
		id := httpmw.SanitizeRequestID(r.Header.Get("X-Euler-TraceId"))
		if id == "" {
			id = fmt.Sprintf("pay-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Euler-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

// internalTokenMiddleware optionally enforces a shared-secret header for
// service-to-service calls. When EULER_INTERNAL_TOKEN is set, every request must
// carry a matching X-Euler-Internal-Token; unset (dev default) the check is off.
// This complements — not replaces — the gateway trust on X-Euler-Account-Id.
func internalTokenMiddleware(h http.Handler) http.Handler {
	return httpmw.InternalToken(h)
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
	// Production refuses to start without the internal shared secret: the
	// X-Euler-Account-Id this service trusts must provably come from the gateway.
	runtimeRequireSecrets()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	srv, err := newPaymentServer(context.Background())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	// Log where the money actually lands: an operator reading logs needs to know
	// whether this process is holding balances in memory (and will lose them on
	// restart) or in trade_db.
	slog.Info("svc-payment ledger ready", "persistent", srv.persistent, "schema", storage.Schema())
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/payment/recharge", srv.handleRecharge)
	mux.HandleFunc("GET /api/v1/payment/balance", srv.handleBalance)
	mux.HandleFunc("POST /api/v1/payment/freeze", srv.handleFreeze)
	mux.HandleFunc("POST /api/v1/payment/consume", srv.handleConsume)
	mux.HandleFunc("POST /internal/payment/consume", srv.handleInternalConsume)
	mux.HandleFunc("POST /internal/payment/refund", srv.handleInternalRefund)

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

// runtimeRequireSecrets enforces the production secret set (pkg-go/runmode).
func runtimeRequireSecrets() {
	runmode.MustRequireSecrets(httpmw.InternalTokenEnv)
}
