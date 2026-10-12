package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/qifalab/euler-platform/provision"
	"github.com/qifalab/euler-platform/resource"
)

// fakeReports stands in for svc-order's fulfilment endpoint.
type fakeReports struct {
	mu  sync.Mutex
	got []reportCall
	err error
}

type reportCall struct {
	orderID int64
	rep     fulfilmentReport
}

func (f *fakeReports) ReportFulfilment(_ context.Context, _, orderID int64, rep fulfilmentReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, reportCall{orderID: orderID, rep: rep})
	return f.err
}

func (f *fakeReports) calls() []reportCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reportCall(nil), f.got...)
}

func reportsOf(s *lifecycleStore) *fakeReports { return s.orderSink.(*fakeReports) }

// setOrderState changes what the fake svc-order reports for an order.
func setOrderState(s *lifecycleStore, id int64, state string) {
	o := s.orderSrc.(fakeOrders).orders[id]
	o.State = state
	s.orderSrc.(fakeOrders).orders[id] = o
}

// B1: a delivered resource completes the order in svc-order (it used to stay
// PAID forever).
func TestFulfilReportsCompletedToOrderService(t *testing.T) {
	s := newTestStore(paid(51, "euecs", "euecs.c1"))
	b := decodeFulfil(t, doFulfill(t, s, fulfillRequest{OrderID: 51, ProductCode: "euecs"}))
	got := reportsOf(s).calls()
	if len(got) != 1 || got[0].orderID != 51 || got[0].rep.Result != fulfilmentCompleted || got[0].rep.ResourceID != b.Data.ResourceID {
		t.Fatalf("reports = %+v, want one COMPLETED for order 51 naming %s", got, b.Data.ResourceID)
	}
}

// A lost report does not change a successful fulfil, and the next replay
// re-sends it — also after a restart (fresh mirror, same ledger).
func TestFulfilLostReportHealsOnReplay(t *testing.T) {
	s := newTestStore(paid(52, "euecs", "euecs.c1"))
	reportsOf(s).err = errors.New("svc-order: connection refused")
	rec := doFulfill(t, s, fulfillRequest{OrderID: 52, ProductCode: "euecs"})
	if rec.Code != 200 {
		t.Fatalf("fulfil with svc-order unreachable: %d %s, want 200 (the resource is up)", rec.Code, rec.Body.String())
	}
	reportsOf(s).err = nil
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 52, ProductCode: "euecs"}); rec.Code != 200 {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	if got := reportsOf(s).calls(); len(got) != 2 || got[1].rep.Result != fulfilmentCompleted {
		t.Fatalf("reports = %+v, want the COMPLETED report re-sent on replay", got)
	}

	restarted := newTestStore(paid(52, "euecs", "euecs.c1"))
	restarted.ledger = s.ledger
	if rec := doFulfill(t, restarted, fulfillRequest{OrderID: 52, ProductCode: "euecs"}); rec.Code != 200 {
		t.Fatalf("replay after restart: %d %s", rec.Code, rec.Body.String())
	}
	if got := reportsOf(restarted).calls(); len(got) != 1 || got[0].rep.Result != fulfilmentCompleted {
		t.Fatalf("reports after restart = %+v", got)
	}
}

// B1: a rolled-back provisioning reports FAILED (svc-order refunds); the
// response stays Provision.Failed whatever the report did, and a replay
// re-sends it.
func TestFulfilCompensationReportsFailed(t *testing.T) {
	s := newTestStore(paid(53, "euecs", "euecs.c1"))
	md := provision.NewMockDriver(nil)
	md.FailApply = func(string) error { return errors.New("quota exhausted in rc-compute") }
	s.driver = md
	reportsOf(s).err = errors.New("svc-order: 503 Order.RefundPending")

	rec := doFulfill(t, s, fulfillRequest{OrderID: 53, ProductCode: "euecs"})
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "Provision.Failed") {
		t.Fatalf("failed provisioning: %d %s, want Provision.Failed", rec.Code, rec.Body.String())
	}
	got := reportsOf(s).calls()
	if len(got) != 1 || got[0].orderID != 53 || got[0].rep.Result != fulfilmentFailed {
		t.Fatalf("reports = %+v, want one FAILED for order 53", got)
	}

	reportsOf(s).err = nil
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 53, ProductCode: "euecs"}); rec.Code != 409 || !strings.Contains(rec.Body.String(), "Provision.Failed") {
		t.Fatalf("replay: %d %s, want 409 Provision.Failed", rec.Code, rec.Body.String())
	}
	if got := reportsOf(s).calls(); len(got) != 2 || got[1].rep.Result != fulfilmentFailed {
		t.Fatalf("reports = %+v, want FAILED re-sent on replay", got)
	}
}

// Once svc-order has moved the order on (COMPLETED / REFUNDING / REFUNDED), a
// replay is still answered from the ledger and re-sends the report.
func TestFulfilReplayOfSettledOrder(t *testing.T) {
	s := newTestStore(paid(54, "euecs", "euecs.c1"), paid(55, "euecs", "euecs.c1"))
	first := decodeFulfil(t, doFulfill(t, s, fulfillRequest{OrderID: 54, ProductCode: "euecs"}))
	setOrderState(s, 54, "COMPLETED")
	rec := doFulfill(t, s, fulfillRequest{OrderID: 54, ProductCode: "euecs"})
	if rec.Code != 200 {
		t.Fatalf("replay of a COMPLETED order: %d %s, want 200", rec.Code, rec.Body.String())
	}
	if b := decodeFulfil(t, rec); b.Data.ResourceID != first.Data.ResourceID || b.Data.State != "RUNNING" {
		t.Fatalf("replay = %+v, want the existing resource", b.Data)
	}

	md := provision.NewMockDriver(nil)
	md.FailApply = func(string) error { return errors.New("rc-compute: no capacity") }
	s.driver = md
	_ = doFulfill(t, s, fulfillRequest{OrderID: 55, ProductCode: "euecs"})
	for _, st := range []string{"REFUNDING", "REFUNDED"} {
		setOrderState(s, 55, st)
		if rec := doFulfill(t, s, fulfillRequest{OrderID: 55, ProductCode: "euecs"}); rec.Code != 409 || !strings.Contains(rec.Body.String(), "Provision.Failed") {
			t.Fatalf("replay of a %s order: %d %s, want 409 Provision.Failed", st, rec.Code, rec.Body.String())
		}
	}
	var completed, failed int
	for _, c := range reportsOf(s).calls() {
		switch {
		case c.orderID == 54 && c.rep.Result == fulfilmentCompleted:
			completed++
		case c.orderID == 55 && c.rep.Result == fulfilmentFailed:
			failed++
		}
	}
	// 54: the fulfil + its replay; 55: the compensation + two replays (a
	// REFUNDING order finishes its refund through the re-sent report).
	if completed != 2 || failed != 3 {
		t.Fatalf("reports: %d COMPLETED for 54, %d FAILED for 55; want 2 and 3", completed, failed)
	}
}

// An order svc-order settled without a resource here was never provisioned by
// this saga and must not be provisioned now.
func TestFulfilRefusesSettledOrderWithoutResource(t *testing.T) {
	var orders []paidOrder
	for i, st := range []string{"COMPLETED", "REFUNDING", "REFUNDED"} {
		o := paid(int64(56+i), "euecs", "euecs.c1")
		o.State = st
		orders = append(orders, o)
	}
	s := newTestStore(orders...)
	before := resourceCount(t, s)
	for _, o := range orders {
		rec := doFulfill(t, s, fulfillRequest{OrderID: o.OrderID, ProductCode: "euecs"})
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "Order.NotPaid") {
			t.Fatalf("%s order without a resource: %d %s, want 409 Order.NotPaid", o.State, rec.Code, rec.Body.String())
		}
	}
	if resourceCount(t, s) != before || len(reportsOf(s).calls()) != 0 {
		t.Fatal("a settled order was provisioned or reported")
	}
}

// A resource still booting has not settled: nothing is reported yet.
func TestFulfilInProgressReportsNothing(t *testing.T) {
	s := newTestStore(paid(59, "euecs", "euecs.c1"))
	md := provision.NewMockDriver(nil)
	md.ProvisioningDelay = 2
	s.driver = md
	if rec := doFulfill(t, s, fulfillRequest{OrderID: 59, ProductCode: "euecs"}); rec.Code != 202 {
		t.Fatalf("want 202, got %d %s", rec.Code, rec.Body.String())
	}
	if got := reportsOf(s).calls(); len(got) != 0 {
		t.Fatalf("reports = %+v, want none while CREATING", got)
	}
}

func TestFulfilmentOutcome(t *testing.T) {
	for st, want := range map[resource.State]string{
		resource.StateCreating:     "",
		resource.StateCreateFailed: fulfilmentFailed,
		resource.StateRunning:      fulfilmentCompleted,
		resource.StateReleased:     fulfilmentCompleted,
	} {
		rep, ok := fulfilmentOutcome(&resource.Instance{ResourceID: "r", State: st})
		if ok != (want != "") || rep.Result != want {
			t.Errorf("%s: %+v %v, want %q", st, rep, ok, want)
		}
	}
}

func TestHTTPOrderSourceReportsFulfilment(t *testing.T) {
	var gotPath, gotAcct, gotToken string
	var gotBody fulfilmentReport
	status, code := 200, "OK"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		gotAcct, gotToken = r.Header.Get(accountIDHeader), r.Header.Get("X-Euler-Internal-Token")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": code})
	}))
	defer srv.Close()
	t.Setenv("EULER_SVC_ORDER_URL", srv.URL)
	t.Setenv("EULER_INTERNAL_TOKEN", "s3cret")
	src := newHTTPOrderSource()

	rep := fulfilmentReport{Result: fulfilmentCompleted, ResourceID: "euecs-cn-north-1-07-0000004d"}
	if err := src.ReportFulfilment(context.Background(), 100123, 77, rep); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /internal/orders/77/fulfilment" || gotAcct != "100123" || gotToken != "s3cret" || gotBody != rep {
		t.Fatalf("request = %s acct=%s token=%q body=%+v", gotPath, gotAcct, gotToken, gotBody)
	}
	status, code = 503, "Order.RefundPending"
	if err := src.ReportFulfilment(context.Background(), 100123, 77, fulfilmentReport{Result: fulfilmentFailed}); err == nil {
		t.Fatal("a 503 from svc-order must be an error (the report is retried)")
	}
}
