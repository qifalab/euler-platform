package main

import (
	"strings"
	"testing"
)

func balanceMinor(t *testing.T, ps *paymentServer) int64 {
	t.Helper()
	bal, err := ps.ledger.Balance(100123)
	if err != nil {
		t.Fatal(err)
	}
	return amountToMinor(bal.Available)
}

// Regression: the tenant-callable consume/freeze took any idempotency key, so a
// tenant could pre-plant a 1-分 CONSUME under svc-order's order-pay-{id} and
// have the order's real debit replay onto it. Internal keys are reserved.
func TestTenantRoutesRefuseReservedKeys(t *testing.T) {
	ps := newInMemoryPaymentServer()
	for _, key := range []string{"order-pay-9001", "ORDER-Pay-9001", "chg-agg-1", "pack:pk-1:ord-1", "svc-x", "refund:order-pay-1", "recharge:ch-1"} {
		body := `{"amountMinor":1,"idempotencyKey":"` + key + `"}`
		if w := doPost(t, ps.handleConsume, "/api/v1/payment/consume", body); w.Code != 400 || !strings.Contains(w.Body.String(), "Payment.ReservedIdempotencyKey") {
			t.Fatalf("consume %q: %d %s, want 400 Payment.ReservedIdempotencyKey", key, w.Code, w.Body.String())
		}
		if w := doPost(t, ps.handleFreeze, "/api/v1/payment/freeze", body); w.Code != 400 {
			t.Fatalf("freeze %q: %d %s, want 400", key, w.Code, w.Body.String())
		}
	}
	if got := balanceMinor(t, ps); got != 50_000 {
		t.Fatalf("balance = %d, a refused request moved money", got)
	}
	// An ordinary tenant key still works.
	if w := doPost(t, ps.handleConsume, "/api/v1/payment/consume", `{"amountMinor":1,"idempotencyKey":"my-order-1"}`); w.Code != 200 {
		t.Fatalf("tenant key: %d %s", w.Code, w.Body.String())
	}
}

func TestInternalConsumeRequiresReservedKey(t *testing.T) {
	ps := newInMemoryPaymentServer()
	if w := doPost(t, ps.handleInternalConsume, "/internal/payment/consume", `{"amountMinor":100,"idempotencyKey":"tenant-key"}`); w.Code != 400 {
		t.Fatalf("non-reserved key: %d %s, want 400", w.Code, w.Body.String())
	}
	w := doPost(t, ps.handleInternalConsume, "/internal/payment/consume", `{"amountMinor":100,"idempotencyKey":"order-pay-7","bizKey":"SO7"}`)
	if w.Code != 200 {
		t.Fatalf("internal consume: %d %s", w.Code, w.Body.String())
	}
	if d := payData(t, w); d["type"] != "CONSUME" || d["amountMinor"].(float64) != 100 {
		t.Fatalf("result = %v", d)
	}
}

// The same key carrying a different amount is a conflict, never a replay: a
// 1-分 entry must not stand in for a ¥2160 order payment.
func TestReusedKeyWithDifferentAmountIsConflict(t *testing.T) {
	ps := newInMemoryPaymentServer()
	if w := doPost(t, ps.handleInternalConsume, "/internal/payment/consume", `{"amountMinor":1,"idempotencyKey":"order-pay-9001"}`); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	w := doPost(t, ps.handleInternalConsume, "/internal/payment/consume", `{"amountMinor":216000,"idempotencyKey":"order-pay-9001"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "Payment.IdempotencyKeyConflict") {
		t.Fatalf("reused key, other amount: %d %s, want 409 Payment.IdempotencyKeyConflict", w.Code, w.Body.String())
	}
	if got := balanceMinor(t, ps); got != 49_999 {
		t.Fatalf("balance = %d, want 49999", got)
	}
}

func TestInternalRefundReversesExactlyOneDebit(t *testing.T) {
	ps := newInMemoryPaymentServer()
	if w := doPost(t, ps.handleInternalConsume, "/internal/payment/consume", `{"amountMinor":500,"idempotencyKey":"order-pay-42","bizKey":"SO42"}`); w.Code != 200 {
		t.Fatalf("consume: %d %s", w.Code, w.Body.String())
	}
	if got := balanceMinor(t, ps); got != 49_500 {
		t.Fatalf("after debit balance = %d", got)
	}
	if w := doPost(t, ps.handleInternalRefund, "/internal/payment/refund", `{"consumeKey":"order-pay-42","amountMinor":499}`); w.Code != 409 {
		t.Fatalf("mismatched amount: %d %s, want 409", w.Code, w.Body.String())
	}
	for i := 0; i < 3; i++ {
		w := doPost(t, ps.handleInternalRefund, "/internal/payment/refund", `{"consumeKey":"order-pay-42","amountMinor":500,"reason":"order cancelled"}`)
		if w.Code != 200 {
			t.Fatalf("refund #%d: %d %s", i, w.Code, w.Body.String())
		}
		d := payData(t, w)
		if d["refunded"] != true || d["type"] != "REFUND" || d["idempotent"] != (i > 0) {
			t.Fatalf("refund #%d = %v", i, d)
		}
	}
	if got := balanceMinor(t, ps); got != 50_000 {
		t.Fatalf("balance = %d, want 50000 (refunded exactly once)", got)
	}
	// Nothing debited under the key: nothing to give back, and no error.
	w := doPost(t, ps.handleInternalRefund, "/internal/payment/refund", `{"consumeKey":"order-pay-43","amountMinor":500}`)
	if w.Code != 200 || payData(t, w)["refunded"] != false {
		t.Fatalf("unknown key: %d %s", w.Code, w.Body.String())
	}
	// Only internal debits are refundable through this route.
	if w := doPost(t, ps.handleInternalRefund, "/internal/payment/refund", `{"consumeKey":"tenant-1","amountMinor":500}`); w.Code != 400 {
		t.Fatalf("tenant key: %d %s, want 400", w.Code, w.Body.String())
	}
	if got := balanceMinor(t, ps); got != 50_000 {
		t.Fatalf("balance = %d, want 50000", got)
	}
}

// ledger_entry.idempotency_key / biz_key are VARCHAR(64): oversized inputs are
// client errors, not 500s or silently truncated (colliding) keys.
func TestOversizedKeysAreRejected(t *testing.T) {
	ps := newTestPaymentServer()
	long := strings.Repeat("k", 65)
	for name, body := range map[string]string{
		"idempotencyKey": `{"amountMinor":1,"idempotencyKey":"` + long + `"}`,
		"bizKey":         `{"amountMinor":1,"idempotencyKey":"k-1","bizKey":"` + long + `"}`,
		"reason":         `{"amountMinor":1,"idempotencyKey":"k-2","reason":"` + strings.Repeat("理", 257) + `"}`,
		"non-ascii key":  `{"amountMinor":1,"idempotencyKey":"órder-pay-1"}`,
	} {
		if w := doPost(t, ps.handleConsume, "/api/v1/payment/consume", body); w.Code != 400 {
			t.Errorf("%s: %d %s, want 400", name, w.Code, w.Body.String())
		}
	}
	// recharge:{outTradeNo} must fit 64 bytes, so outTradeNo is at most 55.
	trade := strings.Repeat("t", 56)
	if w := doRecharge(t, ps, `{"amountMinor":100,"outTradeNo":"`+trade+`"}`, validSig(100, trade)); w.Code != 400 {
		t.Fatalf("56-byte outTradeNo: %d %s, want 400", w.Code, w.Body.String())
	}
	trade = strings.Repeat("t", 55)
	if w := doRecharge(t, ps, `{"amountMinor":100,"outTradeNo":"`+trade+`"}`, validSig(100, trade)); w.Code != 200 {
		t.Fatalf("55-byte outTradeNo: %d %s, want 200", w.Code, w.Body.String())
	}
	if got := balanceMinor(t, ps); got != 50_100 {
		t.Fatalf("balance = %d, want 50100", got)
	}
}
