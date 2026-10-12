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

const testChannelSecret = "test-channel-secret"

// newTestPaymentServer is the demo server with a channel secret configured.
func newTestPaymentServer() *paymentServer {
	ps := newInMemoryPaymentServer()
	ps.channelSecret = []byte(testChannelSecret)
	return ps
}

// doRecharge posts a recharge for account 100123 carrying sig (if non-empty).
func doRecharge(t *testing.T, ps *paymentServer, body, sig string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/payment/recharge", strings.NewReader(body))
	r.Header.Set(accountIDHeader, "100123")
	if sig != "" {
		r.Header.Set(channelSignatureHeader, sig)
	}
	w := httptest.NewRecorder()
	ps.handleRecharge(w, r)
	return w
}

func validSig(amount int64, outTradeNo string) string {
	return signRecharge([]byte(testChannelSecret), 100123, amount, outTradeNo)
}

// Regression: any caller with an account header could credit itself any
// amount. Without a configured channel, recharge is refused outright.
func TestRechargeRefusedWithoutChannelSecret(t *testing.T) {
	ps := newInMemoryPaymentServer()
	ps.channelSecret = nil
	if w := doRecharge(t, ps, `{"amountMinor":100,"outTradeNo":"ch-001"}`, validSig(100, "ch-001")); w.Code != 403 {
		t.Fatalf("want 403 when no channel is configured, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRechargeRequiresValidChannelSignature(t *testing.T) {
	ps := newTestPaymentServer()
	cases := map[string]string{
		"no signature":       "",
		"garbage":            "not-hex",
		"other amount":       validSig(1, "ch-001"),
		"other trade number": validSig(100000, "ch-999"),
		"other secret":       signRecharge([]byte("guess"), 100123, 100000, "ch-001"),
	}
	for name, sig := range cases {
		if w := doRecharge(t, ps, `{"amountMinor":100000,"outTradeNo":"ch-001"}`, sig); w.Code != 403 {
			t.Errorf("%s: want 403, got %d: %s", name, w.Code, w.Body.String())
		}
	}
	bal, _ := ps.ledger.Balance(100123)
	if amountToMinor(bal.Available) != 50_000 {
		t.Fatalf("balance changed by unsigned recharges: %d", amountToMinor(bal.Available))
	}
}

func TestRechargeRequiresOutTradeNo(t *testing.T) {
	ps := newTestPaymentServer()
	w := doRecharge(t, ps, `{"amountMinor":100,"idempotencyKey":"idem-1"}`, validSig(100, ""))
	if w.Code != 400 {
		t.Fatalf("want 400 without outTradeNo, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRechargeSignedSucceeds(t *testing.T) {
	ps := newTestPaymentServer()
	w := doRecharge(t, ps, `{"amountMinor":100,"outTradeNo":"ch-001"}`, validSig(100, "ch-001"))
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRechargeDuplicateIsIdempotentSuccess(t *testing.T) {
	ps := newTestPaymentServer()
	// Same channel transaction, different client idempotency keys: still one credit.
	first := doRecharge(t, ps, `{"amountMinor":100,"outTradeNo":"ch-dup","idempotencyKey":"a"}`, validSig(100, "ch-dup"))
	if first.Code != 200 {
		t.Fatalf("first recharge: %d %s", first.Code, first.Body.String())
	}
	second := doRecharge(t, ps, `{"amountMinor":100,"outTradeNo":"ch-dup","idempotencyKey":"b"}`, validSig(100, "ch-dup"))
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
