// Package main: console-bff domain model and in-memory aggregation store.
//
// console-bff is the control-plane aggregation layer (03§4, console-bff node
// in the 03§3 diagram). There is no pkg-go/console package: the BFF fans out
// to the internal services (svc-billing/ledger, svc-orchestrator/resource,
// svc-order) and aggregates their responses for the web console. Phase-1 keeps
// each handler returning stub-but-shaped responses because the services are
// separate processes, but the shapes here reuse the pkg-go domain types so the
// fan-out targets can be swapped in without changing the JSON contract.
//
//   - balance stubs come from pkg-go/ledger (Balance) behind an in-memory Store
//   - resource list uses pkg-go/resource (Instance) fields
//   - bills use pkg-go/billing (Charge -> Summarize MonthlyBill)
//   - pending-order count derives from pkg-go/order (Order.State)
//
// Every handler emits the platform envelope {RequestId, Code, Message, Data}
// (03§9.3). Account identity is injected by the APISIX gateway via the
// X-Sc-Account-Id header; a missing header is a 403 (07§3.3).
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/starcloud/sc-platform/billing"
	"github.com/starcloud/sc-platform/errors"
	"github.com/starcloud/sc-platform/ledger"
	"github.com/starcloud/sc-platform/order"
	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/resource"
)

// accountIDHeader is injected by the APISIX gateway on every request.
const accountIDHeader = "X-Sc-Account-Id"

// consoleStore is the in-memory aggregation store. In the target architecture
// the BFF caches aggregated views in Redis (03§4 console-bff storage); here the
// struct holds stub-but-shaped domain data keyed by account.
type consoleStore struct {
	mu       sync.RWMutex
	ledger   *ledger.Ledger
	ledgerDB *memLedgerStore
	instances []resource.Instance
	charges   []billing.Charge
	orders    []order.Order
}

func newConsoleStore() *consoleStore {
	db := newMemLedgerStore()
	s := &consoleStore{
		ledgerDB: db,
		ledger:   ledger.New(db, time.Now, nil),
	}
	// Seed a small deterministic dataset so the console renders non-empty
	// (dev/phase-1; a real BFF reads from the services).
	seedConsoleData(s)
	return s
}

// seedConsoleData fills a fixed account with stub-but-shaped rows across the
// domains the console aggregates. Account 100123 matches the shared dev seed.
func seedConsoleData(s *consoleStore) {
	_, _, _ = s.ledger.Recharge(100123, pricing.MustParseAmount("500.00"),
		"seed", "seed-recharge-1", "phase-1 seed balance")

	now := time.Now()
	s.instances = []resource.Instance{
		{ResourceID: "scecs-cn-north-1-01-a1b2c3d4", AccountID: 100123,
			ProductCode: "scecs", Region: "cn-north-1", ChargeType: resource.ChargePrepaid,
			State: resource.StateRunning, SpecCode: "scecs.s2.large", BillingStart: now.Add(-72 * time.Hour),
			CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now},
		{ResourceID: "scoss-cn-north-1-01-b2c3d4e5", AccountID: 100123,
			ProductCode: "scoss", Region: "cn-north-1", ChargeType: resource.ChargePostpaid,
			State: resource.StateRunning, SpecCode: "scoss.standard", BillingStart: now.Add(-24 * time.Hour),
			CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now},
	}

	period := now.Format("2006-01")
	s.charges = []billing.Charge{
		{ChargeID: "chg-0001", AccountID: 100123, ResourceID: "scecs-cn-north-1-01-a1b2c3d4",
			ProductCode: "scecs", MeteringItem: "instance.hour", BillPeriod: period,
			PretaxAmount: pricing.MustParseAmount("0.25"), PayAmount: pricing.MustParseAmount("0.25"),
			SettledAt: now},
	}

	s.orders = []order.Order{
		{OrderID: 9001, OrderNo: "SO202608110001", AccountID: 100123,
			Type: order.TypeNew, State: order.StatePendingPayment,
			ProductCode: "scecs", PayableAmount: pricing.MustParseAmount("2160"),
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now},
	}
}

// --- HTTP helpers -------------------------------------------------------------

// accountIDFrom extracts the caller's account id from the gateway-injected
// header. A missing/blank header is a 403 — the BFF must never fall back to a
// client-supplied account in the body (07§3.3).
func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.Header.Get(accountIDHeader))
	if raw == "" {
		writeError(w, errorsx.New("Common.MissingAccountId", errorsx.StatusForbidden,
			"X-Sc-Account-Id header is required (injected by gateway)"))
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest,
			"malformed X-Sc-Account-Id"))
		return 0, false
	}
	return id, true
}

// requestID reads the trace id the middleware set on the response header, so
// the envelope's RequestId matches X-Sc-TraceId. Get canonicalizes the key
// (Go stores headers canonicalized, e.g. "X-Sc-Traceid").
func requestID(w http.ResponseWriter) string {
	return w.Header().Get("X-Sc-TraceId")
}

// envelope is the platform response body {RequestId, Code, Message, Data}
// (03§9.3). Data/Message are omitted when empty so success responses stay tidy.
type envelope struct {
	RequestId string `json:"RequestId"`
	Code      string `json:"Code"`
	Message   string `json:"Message,omitempty"`
	Data      any    `json:"Data,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{RequestId: requestID(w), Code: "OK", Data: data})
}

func writeError(w http.ResponseWriter, e *errorsx.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.HTTPStatus)
	_ = json.NewEncoder(w).Encode(envelope{RequestId: requestID(w), Code: e.Code, Message: e.Message})
}

// --- console aggregation handlers --------------------------------------------

// handleOverview aggregates the account overview the console home renders:
// cash balance (ledger stub), resource count, pending orders (03§4). Each
// section would be a fan-out to the owning service; here it is in-memory.
func (s *consoleStore) handleOverview(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	bal, err := s.ledger.Balance(acct)
	if err != nil {
		writeError(w, errorsx.New("Billing.InternalError", errorsx.StatusInternalError, err.Error()))
		return
	}

	var pending int64
	for _, o := range s.orders {
		if o.AccountID == acct && o.State == order.StatePendingPayment {
			pending++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"AccountId": acct,
		"Balance": map[string]any{
			"Available": bal.Available.String(),
			"Frozen":    bal.Frozen.String(),
			"Total":     bal.Total().String(),
		},
		"ResourceCount": map[string]any{
			"Total": len(s.instancesFor(acct)),
		},
		"PendingOrders": pending,
	})
}

// handleResources returns the account's resource list (03§4 console 资源列表).
func (s *consoleStore) handleResources(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := s.instancesFor(acct)
	res := make([]map[string]any, 0, len(list))
	for _, inst := range list {
		res = append(res, map[string]any{
			"ResourceId":   inst.ResourceID,
			"ProductCode":  inst.ProductCode,
			"Region":       inst.Region,
			"ChargeType":   inst.ChargeType,
			"State":        inst.State,
			"SpecCode":     inst.SpecCode,
			"BillingStart": inst.BillingStart.Format(time.RFC3339),
			"ExpiredAt":    formatOrEmpty(inst.ExpiredAt),
			"CreatedAt":    inst.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, res)
}

// handleBills returns the account's bill list, summarized per period through
// pkg-go/billing.Summarize (03§4 账单列表).
func (s *consoleStore) handleBills(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Group the account's charges by period and summarize each with the pkg-go
	// domain logic so the JSON contract matches what svc-billing would return.
	byPeriod := map[string][]billing.Charge{}
	for _, c := range s.charges {
		if c.AccountID == acct {
			byPeriod[c.BillPeriod] = append(byPeriod[c.BillPeriod], c)
		}
	}
	periods := make([]string, 0, len(byPeriod))
	for p := range byPeriod {
		periods = append(periods, p)
	}

	bills := make([]map[string]any, 0, len(periods))
	for _, p := range periods {
		mb := billing.Summarize(acct, p, s.charges)
		bills = append(bills, map[string]any{
			"BillPeriod":        mb.BillPeriod,
			"TotalAmount":       mb.TotalAmount.String(),
			"PaidAmount":        mb.PaidAmount.String(),
			"ChargeCount":       mb.ChargeCount,
			"IncompleteCharges": mb.IncompleteCharges,
			"UnreconciledCount": mb.UnreconciledCount,
			"Final":             mb.Final(),
		})
	}
	if bills == nil {
		bills = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, bills)
}

// instancesFor returns the account's seeded instances.
func (s *consoleStore) instancesFor(acct int64) []resource.Instance {
	var out []resource.Instance
	for _, inst := range s.instances {
		if inst.AccountID == acct {
			out = append(out, inst)
		}
	}
	return out
}

func formatOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// --- in-memory ledger store (replaced by trade_db + Redis in phase 2) ---------

type memLedgerStore struct {
	mu       sync.RWMutex
	balances map[int64]ledger.Balance
	entries  map[int64][]ledger.Entry
	byIDKey  map[string]ledger.Entry
}

func newMemLedgerStore() *memLedgerStore {
	return &memLedgerStore{
		balances: make(map[int64]ledger.Balance),
		entries:  make(map[int64][]ledger.Entry),
		byIDKey:  make(map[string]ledger.Entry),
	}
}

func (m *memLedgerStore) GetBalance(accountID int64) (ledger.Balance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if b, ok := m.balances[accountID]; ok {
		return b, nil
	}
	return ledger.Balance{AccountID: accountID}, nil
}

func (m *memLedgerStore) Apply(entry ledger.Entry, newBalance ledger.Balance, expectedVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.balances[entry.AccountID]
	v := 0
	if ok {
		v = cur.Version
	}
	if v != expectedVersion {
		return ledger.ErrVersionConflict
	}
	m.balances[entry.AccountID] = newBalance
	m.entries[entry.AccountID] = append(m.entries[entry.AccountID], entry)
	m.byIDKey[idKey(entry.AccountID, entry.IdempotencyKey)] = entry
	return nil
}

func (m *memLedgerStore) FindByIdempotencyKey(accountID int64, key string) (ledger.Entry, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.byIDKey[idKey(accountID, key)]
	return e, ok, nil
}

func (m *memLedgerStore) ListEntries(accountID int64) ([]ledger.Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.entries[accountID], nil
}

func idKey(accountID int64, key string) string {
	return strconv.FormatInt(accountID, 10) + "|" + key
}
