package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	s := newOrderStore()
	w := doCreate(t, s, `{"productCode":"scecs","amountMinor":-1}`)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateClientTokenIdempotent(t *testing.T) {
	s := newOrderStore()
	body := `{"productCode":"scecs","amountMinor":2160,"clientToken":"tok-1"}`
	first := dataOf(t, doCreate(t, s, body))
	second := dataOf(t, doCreate(t, s, body))
	if first["orderId"] != second["orderId"] {
		t.Fatalf("idempotent replay created a new order: %v vs %v", first["orderId"], second["orderId"])
	}
}

func TestCreateAmountMinorUnits(t *testing.T) {
	s := newOrderStore()
	d := dataOf(t, doCreate(t, s, `{"productCode":"scecs","amountMinor":2160,"clientToken":"tok-amt"}`))
	// 2160 分 = ¥21.60 → pricing.Amount string "21.6".
	if got := d["payableAmount"]; got != "21.6" {
		t.Fatalf("want payableAmount 21.6, got %v", got)
	}
}

func TestPayVersionIncrementsOnce(t *testing.T) {
	s := newOrderStore()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders/9001/pay", nil)
	r.SetPathValue("id", "9001")
	r.Header.Set(accountIDHeader, "100123")
	w := httptest.NewRecorder()
	s.handlePay(w, r)
	if w.Code != 200 {
		t.Fatalf("pay failed: %d %s", w.Code, w.Body.String())
	}
	o := s.orders[9001]
	if o.Version != 2 { // seed Version 1 + exactly one Transition bump
		t.Fatalf("want version 2, got %d", o.Version)
	}
	if string(o.State) != "PAID" {
		t.Fatalf("want PAID, got %s", o.State)
	}
}

func TestCancelVersionIncrementsOnce(t *testing.T) {
	s := newOrderStore()
	d := dataOf(t, doCreate(t, s, `{"productCode":"scecs","amountMinor":100,"clientToken":"tok-c"}`))
	id := int64(d["orderId"].(float64))
	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders/x/cancel", nil)
	r.SetPathValue("id", jsonNum(id))
	r.Header.Set(accountIDHeader, "100123")
	w := httptest.NewRecorder()
	s.handleCancel(w, r)
	if w.Code != 200 {
		t.Fatalf("cancel failed: %d %s", w.Code, w.Body.String())
	}
	if got := s.orders[id].Version; got != 1 {
		t.Fatalf("want version 1 after cancel, got %d", got)
	}
}

func jsonNum(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
