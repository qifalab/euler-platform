package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/order"
)

func doCreate(t *testing.T, s *orderStore, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders", strings.NewReader(body))
	r.Header.Set(accountIDHeader, "100123")
	w := httptest.NewRecorder()
	s.handleCreate(w, r)
	return w
}

func dataOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"Data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad envelope: %v", err)
	}
	return env.Data
}

func TestCreateRejectsNegativeAmount(t *testing.T) {
	s, _, _ := newTestStore()
	w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","amountMinor":-1}`)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateClientTokenIdempotent(t *testing.T) {
	s, _, _ := newTestStore()
	body := `{"productCode":"euecs","skuCode":"euecs.c1","clientToken":"tok-1"}`
	first := dataOf(t, doCreate(t, s, body))
	second := dataOf(t, doCreate(t, s, body))
	if first["orderId"] != second["orderId"] {
		t.Fatalf("idempotent replay created a new order: %v vs %v", first["orderId"], second["orderId"])
	}
}

// Regression: the payable used to be the client's amountMinor, so a tenant
// could buy anything for 1 分. The price must come from the pricing engine.
func TestCreatePriceComesFromCatalogNotClient(t *testing.T) {
	s, q, _ := newTestStore()
	w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","regionId":"cn-north-1","zoneId":"cn-north-1-a",
		"chargeType":"PREPAID","duration":3,"quantity":1,"amountMinor":1,"clientToken":"tok-price"}`)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	d := dataOf(t, w)
	if got := d["payableAmount"]; got != "64.8" { // 3 months × ¥21.60
		t.Fatalf("payable = %v, want 64.8 (server price, not the client's 1 分)", got)
	}
	if d["skuCode"] != "euecs.c1" || d["regionId"] != "cn-north-1" || d["chargeType"] != "PREPAID" {
		t.Fatalf("order line not recorded: %v", d)
	}
	if len(q.calls) != 1 || q.calls[0].ZoneID != "cn-north-1-a" || q.calls[0].Duration != 3 {
		t.Fatalf("quote input not forwarded: %+v", q.calls)
	}
}

func TestCreatePostpaidHasNoUpfrontPayable(t *testing.T) {
	s, _, _ := newTestStore()
	d := dataOf(t, doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1.post","amountMinor":35,"clientToken":"tok-post"}`))
	if d["payableAmount"] != "0" || d["chargeType"] != "POSTPAID" {
		t.Fatalf("postpaid order = %v, want payable 0 (billed from metering)", d)
	}
}

func TestCreateRoundsQuoteToCent(t *testing.T) {
	s, _, _ := newTestStore()
	d := dataOf(t, doCreate(t, s, `{"productCode":"euoss","skuCode":"euoss.std","clientToken":"tok-round"}`))
	if d["payableAmount"] != "0.12" {
		t.Fatalf("payable = %v, want 0.12", d["payableAmount"])
	}
}

func TestCreateValidation(t *testing.T) {
	s, _, _ := newTestStore()
	for name, body := range map[string]string{
		"missing sku":       `{"productCode":"euecs"}`,
		"non-NEW type":      `{"type":"REFUND","productCode":"euecs","skuCode":"euecs.c1"}`,
		"negative quantity": `{"productCode":"euecs","skuCode":"euecs.c1","quantity":-1}`,
		"huge duration":     `{"productCode":"euecs","skuCode":"euecs.c1","duration":100000}`,
	} {
		if w := doCreate(t, s, body); w.Code != 400 {
			t.Errorf("%s: want 400, got %d: %s", name, w.Code, w.Body.String())
		}
	}
}

func TestCreateSurfacesCatalogVerdict(t *testing.T) {
	s, _, _ := newTestStore()
	w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.nope"}`)
	if w.Code != 404 || !strings.Contains(w.Body.String(), "Catalog.SKUNotFound") {
		t.Fatalf("want 404 Catalog.SKUNotFound, got %d %s", w.Code, w.Body.String())
	}
}

// No price, no order: an unreachable pricing engine must not fall back to
// any client-supplied figure.
func TestCreateFailsClosedWhenPricingUnavailable(t *testing.T) {
	s, q, _ := newTestStore()
	q.err = errors.New("dial tcp: connection refused")
	if w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","amountMinor":2160,"clientToken":"tok-down"}`); w.Code != 503 {
		t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
	}
	if o, _ := s.repo.ByClientToken(100123, "tok-down"); o != nil {
		t.Fatal("an order was created without a server-side price")
	}
}

// doPay posts to /pay. The seeded order 9001 has a payable of ¥2160 = 216000 分.
func doPay(t *testing.T, s *orderStore, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders/"+id+"/pay", strings.NewReader(body))
	r.SetPathValue("id", id)
	r.Header.Set(accountIDHeader, "100123")
	w := httptest.NewRecorder()
	s.handlePay(w, r)
	return w
}

// seededOrder fetches the seeded pending order through the repo, the same path a
// SQL backend would take.
func seededOrder(t *testing.T, s *orderStore) *order.Order {
	t.Helper()
	o, err := s.repo.Get(100123, 9001)
	if err != nil {
		t.Fatal(err)
	}
	if o == nil {
		t.Fatal("seeded order 9001 is missing")
	}
	return o
}

func TestPayVersionIncrementsOnce(t *testing.T) {
	s, _, _ := newTestStore()
	w := doPay(t, s, "9001", ``)
	if w.Code != 200 {
		t.Fatalf("pay failed: %d %s", w.Code, w.Body.String())
	}
	o := seededOrder(t, s)
	if o.Version != 1 { // created at version 0 + exactly one transition bump
		t.Fatalf("want version 1, got %d", o.Version)
	}
	if string(o.State) != "PAID" {
		t.Fatalf("want PAID, got %s", o.State)
	}
}

// Regression: /pay used to accept any self-reported paymentId and amount, so a
// caller could mark its own order PAID without moving money. The debit must be
// the order's own payable, taken from the balance by the server.
func TestPayDebitsServerPayableIgnoringClientClaims(t *testing.T) {
	s, _, p := newTestStore()
	w := doPay(t, s, "9001", `{"paymentId":"pay-9001","paidAmountMinor":1}`)
	if w.Code != 200 {
		t.Fatalf("pay: %d %s", w.Code, w.Body.String())
	}
	if got := p.debits["order-pay-9001"]; got != 216000 {
		t.Fatalf("debited %d 分, want the order payable 216000", got)
	}
	if p.balance != 1_000_000-216000 {
		t.Fatalf("balance = %d", p.balance)
	}
}

func TestPayInsufficientBalanceLeavesOrderPending(t *testing.T) {
	s, _, p := newTestStore()
	p.balance = 100
	if w := doPay(t, s, "9001", ``); w.Code != 402 || !strings.Contains(w.Body.String(), "Order.InsufficientBalance") {
		t.Fatalf("want 402 Order.InsufficientBalance, got %d: %s", w.Code, w.Body.String())
	}
	if o := seededOrder(t, s); string(o.State) != "PENDING_PAYMENT" || o.Version != 0 {
		t.Fatalf("order moved without payment: %s v%d", o.State, o.Version)
	}
}

func TestPayFailsClosedWhenPaymentUnavailable(t *testing.T) {
	s, _, p := newTestStore()
	p.err = errors.New("dial tcp: connection refused")
	if w := doPay(t, s, "9001", ``); w.Code != 503 {
		t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
	}
	if string(seededOrder(t, s).State) != "PENDING_PAYMENT" {
		t.Fatal("order must not be PAID when the debit could not be made")
	}
}

// Paying twice is an idempotent no-op: one debit, one version bump.
func TestPayIsIdempotent(t *testing.T) {
	s, _, p := newTestStore()
	for i := 0; i < 3; i++ {
		if w := doPay(t, s, "9001", `{"paymentId":"pay-other"}`); w.Code != 200 {
			t.Fatalf("pay #%d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if got := seededOrder(t, s).Version; got != 1 {
		t.Fatalf("replay must not bump the version again, got %d", got)
	}
	if p.balance != 1_000_000-216000 {
		t.Fatalf("charged more than once: balance %d", p.balance)
	}
}

func TestPayZeroPayableNeedsNoDebit(t *testing.T) {
	s, _, p := newTestStore()
	d := dataOf(t, doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1.post","clientToken":"tok-free"}`))
	id := jsonNum(int64(d["orderId"].(float64)))
	if w := doPay(t, s, id, ``); w.Code != 200 {
		t.Fatalf("pay: %d %s", w.Code, w.Body.String())
	}
	if p.calls != 0 {
		t.Fatalf("a zero-payable order must not call the payer (%d calls)", p.calls)
	}
}

func TestPayRejectsCancelledOrder(t *testing.T) {
	s, _, p := newTestStore()
	d := dataOf(t, doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","clientToken":"tok-cx"}`))
	id := jsonNum(int64(d["orderId"].(float64)))
	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders/x/cancel", nil)
	r.SetPathValue("id", id)
	r.Header.Set(accountIDHeader, "100123")
	s.handleCancel(httptest.NewRecorder(), r)
	if w := doPay(t, s, id, ``); w.Code != 409 {
		t.Fatalf("want 409 paying a cancelled order, got %d: %s", w.Code, w.Body.String())
	}
	if p.calls != 0 {
		t.Fatal("a cancelled order must not be charged")
	}
}

// minor × 10_000 overflows int64 for large inputs; the conversion must reject
// them instead of wrapping into a plausible-looking amount.
func TestCreateRejectsOverflowingAmount(t *testing.T) {
	s, _, _ := newTestStore()
	w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","amountMinor":1000000000000000000}`)
	if w.Code != 400 {
		t.Fatalf("want 400 on overflowing amountMinor, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCancelVersionIncrementsOnce(t *testing.T) {
	s, _, _ := newTestStore()
	d := dataOf(t, doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","clientToken":"tok-c"}`))
	id := int64(d["orderId"].(float64))
	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders/x/cancel", nil)
	r.SetPathValue("id", jsonNum(id))
	r.Header.Set(accountIDHeader, "100123")
	w := httptest.NewRecorder()
	s.handleCancel(w, r)
	if w.Code != 200 {
		t.Fatalf("cancel failed: %d %s", w.Code, w.Body.String())
	}
	cancelled, err := s.repo.Get(100123, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := cancelled.Version; got != 1 {
		t.Fatalf("want version 1 after cancel, got %d", got)
	}
}

func jsonNum(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
