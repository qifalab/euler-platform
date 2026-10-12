package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/resource"
)

// fakeOrders stands in for svc-order: orders keyed by (account, id).
type fakeOrders struct {
	orders map[int64]paidOrder // orderID → order, all owned by account 100123
	err    error
}

func (f fakeOrders) Order(_ context.Context, accountID, orderID int64) (paidOrder, error) {
	if f.err != nil {
		return paidOrder{}, f.err
	}
	o, ok := f.orders[orderID]
	if !ok || accountID != 100123 {
		return paidOrder{}, errOrderNotFound
	}
	return o, nil
}

func paid(id int64, product, sku string) paidOrder {
	return paidOrder{OrderID: id, OrderNo: "SO" + jsonInt(id), State: "PAID", ProductCode: product,
		SKUCode: sku, RegionID: "cn-north-1", ChargeType: "PREPAID", Payable: pricing.MustParseAmount("100")}
}

func jsonInt(v int64) string { b, _ := json.Marshal(v); return string(b) }

func newTestStore(orders ...paidOrder) *lifecycleStore {
	s := newStore()
	m := map[int64]paidOrder{}
	for _, o := range orders {
		m[o.OrderID] = o
	}
	s.orderSrc = fakeOrders{orders: m}
	s.orderSink = &fakeReports{}
	return s
}

func resourceCount(t *testing.T, s *lifecycleStore) int {
	t.Helper()
	l, err := s.ledger.List(100123)
	if err != nil {
		t.Fatal(err)
	}
	return len(l)
}

// Regression: an order id the orchestrator had never seen used to be mirrored
// as PAID and provisioned — free resources for any caller.
func TestFulfillRefusesUnknownOrder(t *testing.T) {
	s := newTestStore()
	before := resourceCount(t, s)
	rec := doFulfill(t, s, fulfillRequest{OrderID: 987654321, ProductCode: "euecs", Region: "cn-north-1", SpecCode: "euecs.c8.xlarge"})
	if rec.Code != 404 {
		t.Fatalf("want 404 for an order svc-order does not know, got %d %s", rec.Code, rec.Body.String())
	}
	if resourceCount(t, s) != before {
		t.Fatal("a resource was provisioned for a non-existent order")
	}
}

func TestFulfillRefusesUnpaidOrder(t *testing.T) {
	o := paid(11, "euecs", "euecs.c1")
	o.State = "PENDING_PAYMENT"
	s := newTestStore(o)
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 11, ProductCode: "euecs"}); rec.Code != 409 {
		t.Fatalf("want 409 for an unpaid order, got %d %s", rec.Code, rec.Body.String())
	}
}

// The spec provisioned is the spec paid for: a cheap order cannot be fulfilled
// as a big instance, and an order for one product cannot open another.
func TestFulfillBindsToOrderLine(t *testing.T) {
	s := newTestStore(paid(12, "euecs", "euecs.c1"))
	for name, req := range map[string]fulfillRequest{
		"bigger spec":   {OrderID: 12, ProductCode: "euecs", SpecCode: "euecs.c8.xlarge"},
		"other product": {OrderID: 12, ProductCode: "eurds", SpecCode: "euecs.c1"},
		"other region":  {OrderID: 12, ProductCode: "euecs", Region: "cn-east-1"},
	} {
		if rec := doFulfill(t, s, req); rec.Code != 409 {
			t.Errorf("%s: want 409, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	rec := doFulfill(t, s, fulfillRequest{OrderID: 12, ProductCode: "euecs", ChargeType: "POSTPAID"})
	if rec.Code != 200 {
		t.Fatalf("matching fulfil: %d %s", rec.Code, rec.Body.String())
	}
	var d fulfillData
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	inst, err := s.ledger.Get(100123, d.Data.ResourceId)
	if err != nil || inst == nil {
		t.Fatalf("resource: %v %v", inst, err)
	}
	if inst.SpecCode != "euecs.c1" || inst.Region != "cn-north-1" || inst.ChargeType != resource.ChargePrepaid {
		t.Fatalf("resource not built from the order line: %+v", inst)
	}
}

func TestFulfillRefusesOrderWithoutLine(t *testing.T) {
	s := newTestStore(paid(13, "euecs", ""))
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 13, ProductCode: "euecs", SpecCode: "euecs.c1"}); rec.Code != 409 {
		t.Fatalf("want 409, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestFulfillFailsClosedWhenOrderServiceDown(t *testing.T) {
	s := newStore()
	s.orderSrc = fakeOrders{err: errors.New("connection refused")}
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 14, ProductCode: "euecs"}); rec.Code != 503 {
		t.Fatalf("want 503, got %d %s", rec.Code, rec.Body.String())
	}
}

// failingLedger makes Create fail the way MySQL does on an over-long column.
type failingLedger struct{ resourceLedger }

func (failingLedger) Create(*resource.Instance, ...createOption) error {
	return errors.New("Error 1406: Data too long")
}
func (failingLedger) ByOrder(int64, int64) (*resource.Instance, error) {
	return nil, nil
}

// Regression: a ledger write failure returned while still holding the store
// mutex, so every later fulfil blocked forever.
func TestFulfillLedgerFailureReleasesLock(t *testing.T) {
	s := newTestStore(paid(15, "euecs", "euecs.c1"), paid(16, "euecs", "euecs.c1"))
	s.ledger = failingLedger{s.ledger}
	for _, id := range []int64{15, 16} {
		done := make(chan int, 1)
		go func(id int64) { done <- doFulfill(t, s, fulfillRequest{OrderID: id, ProductCode: "euecs"}).Code }(id)
		select {
		case code := <-done:
			if code != 500 {
				t.Fatalf("order %d: want 500, got %d", id, code)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("fulfil for order %d hung: the store lock was leaked", id)
		}
	}
}

func doAction(t *testing.T, s *lifecycleStore, resID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/orchestrator/resources/"+resID+"/actions", strings.NewReader(body))
	req.SetPathValue("id", resID)
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleResourceAction(rec, req)
	return rec
}

// A prepaid instance paid for at one spec cannot be upgraded for free.
func TestUpgradePrepaidRequiresOrder(t *testing.T) {
	s := newTestStore()
	rec := doAction(t, s, "euecs-cn-north-1-01-a1b2c3d4", `{"action":"upgrade","specCode":"euecs.c8.xlarge"}`)
	if rec.Code != 409 {
		t.Fatalf("want 409, got %d %s", rec.Code, rec.Body.String())
	}
	inst, _ := s.ledger.Get(100123, "euecs-cn-north-1-01-a1b2c3d4")
	if inst.SpecCode != "euecs.s2.large" {
		t.Fatalf("spec changed to %s", inst.SpecCode)
	}
}

func TestUpgradePostpaidStaysWithinProduct(t *testing.T) {
	s := newTestStore()
	if rec := doAction(t, s, "euvpc-cn-north-1-01-vpc0a1b2c", `{"action":"upgrade","specCode":"euecs.c8.xlarge"}`); rec.Code != 400 {
		t.Fatalf("cross-product spec: want 400, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doAction(t, s, "euvpc-cn-north-1-01-vpc0a1b2c", `{"action":"upgrade","specCode":"euvpc.premium"}`); rec.Code != 200 {
		t.Fatalf("postpaid upgrade: want 200, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPOrderSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(accountIDHeader) != "100123" || r.URL.Path != "/api/v1/orders/77" {
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]any{"Code": "Order.NotFound"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": map[string]any{
			"orderId": 77, "orderNo": "SO77", "state": "PAID", "productCode": "euecs",
			"skuCode": "euecs.c1", "regionId": "cn-north-1", "chargeType": "PREPAID", "payableAmount": "21.6",
		}})
	}))
	defer srv.Close()
	t.Setenv("EULER_SVC_ORDER_URL", srv.URL)
	src := newHTTPOrderSource()
	o, err := src.Order(context.Background(), 100123, 77)
	if err != nil || o.SKUCode != "euecs.c1" || o.State != "PAID" || o.Payable != pricing.MustParseAmount("21.6") {
		t.Fatalf("order = %+v, %v", o, err)
	}
	if _, err := src.Order(context.Background(), 200001, 77); !errors.Is(err, errOrderNotFound) {
		t.Fatalf("other account: want errOrderNotFound, got %v", err)
	}
}
