package main

import (
	"encoding/json"
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
	s := newInMemoryOrderStore()
	w := doCreate(t, s, `{"productCode":"euecs","amountMinor":-1}`)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateClientTokenIdempotent(t *testing.T) {
	s := newInMemoryOrderStore()
	body := `{"productCode":"euecs","amountMinor":2160,"clientToken":"tok-1"}`
	first := dataOf(t, doCreate(t, s, body))
	second := dataOf(t, doCreate(t, s, body))
	if first["orderId"] != second["orderId"] {
		t.Fatalf("idempotent replay created a new order: %v vs %v", first["orderId"], second["orderId"])
	}
}

func TestCreateAmountMinorUnits(t *testing.T) {
	s := newInMemoryOrderStore()
	d := dataOf(t, doCreate(t, s, `{"productCode":"euecs","amountMinor":2160,"clientToken":"tok-amt"}`))
	// 2160 分 = ¥21.60 → pricing.Amount string "21.6".
	if got := d["payableAmount"]; got != "21.6" {
		t.Fatalf("want payableAmount 21.6, got %v", got)
	}
}

// doPay posts a payment callback. The seeded order 9001 has a payable of
// ¥2160 = 216000 分.
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
	s := newInMemoryOrderStore()
	w := doPay(t, s, "9001", `{"paymentId":"pay-9001","paidAmountMinor":216000}`)
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

// A payment callback without a reference is an unverified self-mark; it must
// be refused before any state change.
func TestPayRequiresPaymentReference(t *testing.T) {
	s := newInMemoryOrderStore()
	if w := doPay(t, s, "9001", `{"paidAmountMinor":216000}`); w.Code != 400 {
		t.Fatalf("want 400 without paymentId, got %d: %s", w.Code, w.Body.String())
	}
	if string(seededOrder(t, s).State) != "PENDING_PAYMENT" {
		t.Fatal("order must not move without a payment reference")
	}
}

// The reported paid amount must equal the order's payable: a callback for a
// different figure is a reconciliation problem, not a state transition.
func TestPayRejectsAmountMismatch(t *testing.T) {
	s := newInMemoryOrderStore()
	if w := doPay(t, s, "9001", `{"paymentId":"pay-9001","paidAmountMinor":1}`); w.Code != 409 {
		t.Fatalf("want 409 on amount mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if string(seededOrder(t, s).State) != "PENDING_PAYMENT" {
		t.Fatal("mismatched payment must not mark the order paid")
	}
}

// A retried callback with the same payment id is idempotent; a different
// payment against the settled order is a double payment and must be refused.
func TestPayIsIdempotentPerPayment(t *testing.T) {
	s := newInMemoryOrderStore()
	body := `{"paymentId":"pay-9001","paidAmountMinor":216000}`
	if w := doPay(t, s, "9001", body); w.Code != 200 {
		t.Fatalf("first pay: %d %s", w.Code, w.Body.String())
	}
	if w := doPay(t, s, "9001", body); w.Code != 200 {
		t.Fatalf("replay must be 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := seededOrder(t, s).Version; got != 1 {
		t.Fatalf("replay must not bump the version again, got %d", got)
	}
	if w := doPay(t, s, "9001", `{"paymentId":"pay-other","paidAmountMinor":216000}`); w.Code != 409 {
		t.Fatalf("second payment must be 409, got %d: %s", w.Code, w.Body.String())
	}
}

// minor × 10_000 overflows int64 for large inputs; the conversion must reject
// them instead of wrapping into a plausible-looking amount.
func TestCreateRejectsOverflowingAmount(t *testing.T) {
	s := newInMemoryOrderStore()
	w := doCreate(t, s, `{"productCode":"euecs","amountMinor":1000000000000000000}`)
	if w.Code != 400 {
		t.Fatalf("want 400 on overflowing amountMinor, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCancelVersionIncrementsOnce(t *testing.T) {
	s := newInMemoryOrderStore()
	d := dataOf(t, doCreate(t, s, `{"productCode":"euecs","amountMinor":100,"clientToken":"tok-c"}`))
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
