package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qifalab/euler-platform/pricing"
)

// These exercise the real HTTP clients against stand-ins that speak the
// svc-catalog / svc-payment wire format ({RequestId,Code,Message,Data}).

func TestCatalogQuoterParsesQuote(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/catalog/quote" || r.Header.Get(accountIDHeader) != "100123" {
			t.Errorf("unexpected request %s acct=%q", r.URL.Path, r.Header.Get(accountIDHeader))
		}
		if r.Header.Get(internalTokenHeader) != "s3cret" {
			t.Errorf("internal token not forwarded")
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": "r", "Code": "OK", "Data": map[string]any{
			"payableAmount": "64.8", "chargeType": "PREPAID", "durationUnit": "MONTH",
		}})
	}))
	defer srv.Close()
	t.Setenv("EULER_INTERNAL_TOKEN", "s3cret")
	t.Setenv("EULER_SVC_CATALOG_URL", srv.URL)
	res, err := newCatalogQuoter().Quote(context.Background(), 100123, quoteInput{
		ProductCode: "euecs", SKUCode: "euecs.c1", ChargeType: "PREPAID", Duration: 3, Quantity: 1, ZoneID: "cn-north-1a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Payable != pricing.MustParseAmount("64.8") || res.ChargeType != pricing.ChargePrepaid || res.DurationUnit != pricing.DurationMonth {
		t.Fatalf("quote = %+v", res)
	}
	if got["specCode"] != "euecs.c1" || got["zoneId"] != "cn-north-1a" {
		t.Fatalf("request body = %v", got)
	}
	if _, ok := got["userTag"]; ok {
		t.Fatal("userTag must not be forwarded (caller cannot pick its own discount tier)")
	}
}

func TestCatalogQuoterSurfacesClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "Catalog.ZoneRequired", "Message": "ZONAL product euecs requires zoneId"})
	}))
	defer srv.Close()
	t.Setenv("EULER_SVC_CATALOG_URL", srv.URL)
	_, err := newCatalogQuoter().Quote(context.Background(), 1, quoteInput{ProductCode: "euecs", SKUCode: "euecs.c1"})
	var de *downstreamError
	if !errors.As(err, &de) || de.Status != 400 || de.Code != "Catalog.ZoneRequired" {
		t.Fatalf("err = %v", err)
	}
}

func TestBalancePayerConsume(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/payment/consume" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["amountMinor"].(float64) > 1000 {
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"Code": "Payment.InsufficientBalance", "Message": "insufficient balance"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": map[string]any{
			"entryId": 4242, "type": "CONSUME", "amountMinor": body["amountMinor"], "idempotent": false}})
	}))
	defer srv.Close()
	t.Setenv("EULER_SVC_PAYMENT_URL", srv.URL)
	p := newBalancePayer()
	id, err := p.Consume(context.Background(), 100123, 500, "order-pay-1", "SO1", "pay order SO1")
	if err != nil || id != "4242" {
		t.Fatalf("consume = %q, %v", id, err)
	}
	if body["idempotencyKey"] != "order-pay-1" {
		t.Fatalf("idempotency key not sent: %v", body)
	}
	if _, err := p.Consume(context.Background(), 100123, 5000, "order-pay-2", "SO2", ""); !errors.Is(err, errInsufficientBalance) {
		t.Fatalf("want errInsufficientBalance, got %v", err)
	}
}

// Regression: the payer accepted ANY entry id as proof of payment, so an entry
// pre-planted under the order's key (1 分, or not even a debit) paid the order.
// The entry must be a CONSUME of exactly the order's payable.
func TestBalancePayerRejectsForeignEntry(t *testing.T) {
	var reply map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": reply})
	}))
	defer srv.Close()
	t.Setenv("EULER_SVC_PAYMENT_URL", srv.URL)
	p := newBalancePayer()
	for name, d := range map[string]map[string]any{
		"1 分 replay":   {"entryId": 7, "type": "CONSUME", "amountMinor": 1, "idempotent": true},
		"freeze":       {"entryId": 7, "type": "FREEZE", "amountMinor": 216000, "idempotent": true},
		"no amount":    {"entryId": 7, "type": "CONSUME"},
		"zero entryId": {"entryId": 0, "type": "CONSUME", "amountMinor": 216000},
	} {
		reply = d
		if _, err := p.Consume(context.Background(), 100123, 216000, "order-pay-9001", "SO1", ""); err == nil {
			t.Fatalf("%s: accepted as payment", name)
		}
	}
	reply = map[string]any{"entryId": 7, "type": "CONSUME", "amountMinor": 216000, "idempotent": true}
	if id, err := p.Consume(context.Background(), 100123, 216000, "order-pay-9001", "SO1", ""); err != nil || id != "7" {
		t.Fatalf("matching replay = %q, %v", id, err)
	}
}

func TestBalancePayerSurfacesKeyConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "Payment.IdempotencyKeyConflict", "Message": "conflict"})
	}))
	defer srv.Close()
	t.Setenv("EULER_SVC_PAYMENT_URL", srv.URL)
	if _, err := newBalancePayer().Consume(context.Background(), 1, 5, "order-pay-1", "SO1", ""); !errors.Is(err, errPaymentConflict) {
		t.Fatalf("err = %v, want errPaymentConflict", err)
	}
}

func TestBalancePayerRefund(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/payment/refund" || r.Header.Get(accountIDHeader) != "100123" {
			t.Errorf("unexpected request %s acct=%q", r.URL.Path, r.Header.Get(accountIDHeader))
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": map[string]any{"refunded": body["consumeKey"] == "order-pay-1"}})
	}))
	defer srv.Close()
	t.Setenv("EULER_SVC_PAYMENT_URL", srv.URL)
	p := newBalancePayer()
	if ok, err := p.Refund(context.Background(), 100123, "order-pay-1", 500, "cancelled"); err != nil || !ok {
		t.Fatalf("refund = %v, %v", ok, err)
	}
	if body["amountMinor"].(float64) != 500 || body["reason"] != "cancelled" {
		t.Fatalf("request body = %v", body)
	}
	if ok, err := p.Refund(context.Background(), 100123, "order-pay-2", 500, ""); err != nil || ok {
		t.Fatalf("nothing to refund = %v, %v", ok, err)
	}
}

func TestRoundToCent(t *testing.T) {
	for in, want := range map[string]string{"0.123456": "0.12", "0.125": "0.13", "21.6": "21.6", "0": "0", "0.004999": "0"} {
		if got := roundToCent(pricing.MustParseAmount(in)).String(); got != want {
			t.Errorf("roundToCent(%s) = %s, want %s", in, got, want)
		}
	}
}
