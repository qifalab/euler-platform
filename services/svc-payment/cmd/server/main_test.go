package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doPost(t *testing.T, h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set(accountIDHeader, "100123")
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func payData(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"Data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad envelope: %v", err)
	}
	return env.Data
}

func TestRechargeRequiresIdempotencyKey(t *testing.T) {
	ps := newInMemoryPaymentServer()
	w := doPost(t, ps.handleRecharge, "/api/v1/payment/recharge", `{"amountMinor":100}`)
	if w.Code != 400 {
		t.Fatalf("want 400 without idempotency key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRechargeOutTradeNoAsIdempotencyKey(t *testing.T) {
	ps := newInMemoryPaymentServer()
	w := doPost(t, ps.handleRecharge, "/api/v1/payment/recharge", `{"amountMinor":100,"outTradeNo":"ch-001"}`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRechargeDuplicateIsIdempotentSuccess(t *testing.T) {
	ps := newInMemoryPaymentServer()
	body := `{"amountMinor":100,"idempotencyKey":"idem-1"}`
	first := doPost(t, ps.handleRecharge, "/api/v1/payment/recharge", body)
	if first.Code != 200 {
		t.Fatalf("first recharge: %d %s", first.Code, first.Body.String())
	}
	second := doPost(t, ps.handleRecharge, "/api/v1/payment/recharge", body)
	if second.Code != 200 {
		t.Fatalf("replay should be 200, got %d: %s", second.Code, second.Body.String())
	}
	d := payData(t, second)
	if d["idempotent"] != true {
		t.Fatalf("replay should report idempotent:true, got %v", d["idempotent"])
	}
	// Balance must not be double-credited: seed 50_000 + 100.
	bal := d["balance"].(map[string]any)
	if got := bal["availableMinor"].(float64); got != 50_100 {
		t.Fatalf("want availableMinor 50100, got %v", got)
	}
}

func TestConsumeDuplicateIsIdempotentSuccess(t *testing.T) {
	ps := newInMemoryPaymentServer()
	body := `{"amountMinor":500,"idempotencyKey":"order-42"}`
	doPost(t, ps.handleConsume, "/api/v1/payment/consume", body)
	second := doPost(t, ps.handleConsume, "/api/v1/payment/consume", body)
	if second.Code != 200 {
		t.Fatalf("replay should be 200, got %d: %s", second.Code, second.Body.String())
	}
	if d := payData(t, second); d["idempotent"] != true {
		t.Fatalf("want idempotent:true, got %v", d["idempotent"])
	}
}

func TestFreezeDuplicateIsIdempotentSuccess(t *testing.T) {
	ps := newInMemoryPaymentServer()
	body := `{"amountMinor":300,"idempotencyKey":"freeze-42"}`
	doPost(t, ps.handleFreeze, "/api/v1/payment/freeze", body)
	second := doPost(t, ps.handleFreeze, "/api/v1/payment/freeze", body)
	if second.Code != 200 {
		t.Fatalf("replay should be 200, got %d: %s", second.Code, second.Body.String())
	}
	if d := payData(t, second); d["idempotent"] != true {
		t.Fatalf("want idempotent:true, got %v", d["idempotent"])
	}
}

func TestConsumeInsufficientBalanceStableError(t *testing.T) {
	ps := newInMemoryPaymentServer()
	w := doPost(t, ps.handleConsume, "/api/v1/payment/consume", `{"amountMinor":99999999,"idempotencyKey":"big-1"}`)
	if w.Code != 422 {
		t.Fatalf("want 422, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "available") {
		t.Fatalf("error message should be stable, not leak ledger details: %s", w.Body.String())
	}
}
