package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/pricing"
)

// doFulfilment posts svc-orchestrator's settlement report for an order.
func doFulfilment(t *testing.T, s *orderStore, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/internal/orders/"+id+"/fulfilment", strings.NewReader(body))
	r.SetPathValue("id", id)
	r.Header.Set(accountIDHeader, "100123")
	w := httptest.NewRecorder()
	s.handleFulfilment(w, r)
	return w
}

// paidSeededOrder pays the seeded order 9001 (¥2160 = 216000 分).
func paidSeededOrder(t *testing.T, s *orderStore) {
	t.Helper()
	if w := doPay(t, s, "9001", ``); w.Code != 200 {
		t.Fatalf("pay: %d %s", w.Code, w.Body.String())
	}
}

const completedReport = `{"result":"COMPLETED","resourceId":"euecs-cn-north-1-07-00002329"}`
const failedReport = `{"result":"FAILED","reason":"provisioning failed; the resource was rolled back"}`

// B1: the orchestrator's success report completes the order (it used to stay
// PAID forever) and records the resource; replays are idempotent and a
// contradicting FAILED is refused without touching the money.
func TestFulfilmentCompletedSettlesPaidOrder(t *testing.T) {
	s, _, p := newTestStore()
	paidSeededOrder(t, s)

	w := doFulfilment(t, s, "9001", completedReport)
	if w.Code != 200 {
		t.Fatalf("COMPLETED: %d %s", w.Code, w.Body.String())
	}
	o := seededOrder(t, s)
	if o.State != order.StateCompleted || o.ResourceID != "euecs-cn-north-1-07-00002329" || o.Version != 3 {
		t.Fatalf("order = %s %q v%d, want COMPLETED with the resource at v3 (PAID → FULFILLING → COMPLETED)",
			o.State, o.ResourceID, o.Version)
	}
	if w := doFulfilment(t, s, "9001", completedReport); w.Code != 200 {
		t.Fatalf("replayed COMPLETED: %d %s", w.Code, w.Body.String())
	}
	if got := seededOrder(t, s).Version; got != 3 {
		t.Fatalf("replay moved the order again: v%d", got)
	}
	if w := doFulfilment(t, s, "9001", failedReport); w.Code != 409 {
		t.Fatalf("FAILED for a COMPLETED order: %d %s, want 409", w.Code, w.Body.String())
	}
	if o := seededOrder(t, s); o.State != order.StateCompleted || p.refunds != 0 || p.balance != 1_000_000-216000 {
		t.Fatalf("contradicting report changed the order or the money: %s refunds=%d balance=%d", o.State, p.refunds, p.balance)
	}
}

// B1: a failed provisioning refunds the payment and ends REFUNDED — the
// customer no longer pays for nothing. A replay refunds nothing twice.
func TestFulfilmentFailedRefundsThePayment(t *testing.T) {
	s, _, p := newTestStore()
	paidSeededOrder(t, s)

	if w := doFulfilment(t, s, "9001", failedReport); w.Code != 200 {
		t.Fatalf("FAILED: %d %s", w.Code, w.Body.String())
	}
	if o := seededOrder(t, s); o.State != order.StateRefunded {
		t.Fatalf("order = %s, want REFUNDED", o.State)
	}
	if p.balance != 1_000_000 || !p.refunded["order-pay-9001"] {
		t.Fatalf("balance = %d refunded = %v: the payment was not given back", p.balance, p.refunded)
	}
	if w := doFulfilment(t, s, "9001", failedReport); w.Code != 200 {
		t.Fatalf("replayed FAILED: %d %s", w.Code, w.Body.String())
	}
	if p.refunds != 1 || p.balance != 1_000_000 {
		t.Fatalf("replay refunded again: refunds=%d balance=%d", p.refunds, p.balance)
	}
	if w := doFulfilment(t, s, "9001", completedReport); w.Code != 409 {
		t.Fatalf("COMPLETED for a REFUNDED order: %d %s, want 409", w.Code, w.Body.String())
	}
}

// B1: when svc-payment cannot refund, the order stays REFUNDING and the caller
// is told to retry (503); the retry finishes the refund exactly once.
func TestFulfilmentFailedRefundOutageIsRetryable(t *testing.T) {
	s, _, p := newTestStore()
	paidSeededOrder(t, s)

	p.err = errors.New("dial tcp: connection refused")
	w := doFulfilment(t, s, "9001", failedReport)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Order.RefundPending") {
		t.Fatalf("refund outage: %d %s, want 503 Order.RefundPending", w.Code, w.Body.String())
	}
	if o := seededOrder(t, s); o.State != order.StateRefunding {
		t.Fatalf("order = %s, want REFUNDING until the refund is made", o.State)
	}

	p.err = nil
	if w := doFulfilment(t, s, "9001", failedReport); w.Code != 200 {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	if o := seededOrder(t, s); o.State != order.StateRefunded || p.balance != 1_000_000 {
		t.Fatalf("after retry: %s balance=%d, want REFUNDED and the money back", o.State, p.balance)
	}
}

// A report that half-applied (the order stopped in FULFILLING) resumes on replay,
// whichever way it went.
func TestFulfilmentResumesFromFulfilling(t *testing.T) {
	for _, tc := range []struct {
		body string
		want order.State
	}{
		{completedReport, order.StateCompleted},
		{failedReport, order.StateRefunded},
	} {
		s, _, _ := newTestStore()
		paidSeededOrder(t, s)
		o := seededOrder(t, s)
		from, prev := o.State, o.Version
		evt, err := s.machine.StartFulfilment(o, o.Version)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.repo.SaveTransition(o, from, prev, "test", evt, ""); err != nil {
			t.Fatal(err)
		}
		if w := doFulfilment(t, s, "9001", tc.body); w.Code != 200 {
			t.Fatalf("%s from FULFILLING: %d %s", tc.want, w.Code, w.Body.String())
		}
		if got := seededOrder(t, s).State; got != tc.want {
			t.Fatalf("order = %s, want %s", got, tc.want)
		}
	}
}

func TestFulfilmentRejectsUnpaidOrdersAndBadInput(t *testing.T) {
	s, _, p := newTestStore()
	for name, tc := range map[string]struct {
		id, body string
		status   int
	}{
		"unpaid COMPLETED":  {"9001", completedReport, 409},
		"unpaid FAILED":     {"9001", failedReport, 409},
		"unknown result":    {"9001", `{"result":"DONE"}`, 400},
		"malformed body":    {"9001", `{`, 400},
		"long resourceId":   {"9001", `{"result":"COMPLETED","resourceId":"` + strings.Repeat("r", 65) + `"}`, 400},
		"unknown order":     {"424242", completedReport, 404},
		"non-numeric order": {"abc", completedReport, 400},
	} {
		if w := doFulfilment(t, s, tc.id, tc.body); w.Code != tc.status {
			t.Errorf("%s: %d %s, want %d", name, w.Code, w.Body.String(), tc.status)
		}
	}
	if o := seededOrder(t, s); o.State != order.StatePendingPayment || o.Version != 0 || p.refunds != 0 {
		t.Fatalf("a refused report changed the order: %s v%d refunds=%d", o.State, o.Version, p.refunds)
	}
	r := httptest.NewRequest(http.MethodPost, "/internal/orders/9001/fulfilment", strings.NewReader(completedReport))
	r.SetPathValue("id", "9001")
	w := httptest.NewRecorder()
	s.handleFulfilment(w, r)
	if w.Code != 403 {
		t.Fatalf("no account header: %d, want 403", w.Code)
	}
}

// An over-long failure reason is cut to the journal column, not a 500.
func TestFulfilmentTruncatesReason(t *testing.T) {
	if got := truncateRunes(strings.Repeat("失", 300), maxStateLogReasonRunes); len([]rune(got)) != maxStateLogReasonRunes {
		t.Fatalf("truncated to %d runes", len([]rune(got)))
	}
	s, _, _ := newTestStore()
	paidSeededOrder(t, s)
	if w := doFulfilment(t, s, "9001", `{"result":"FAILED","reason":"`+strings.Repeat("失", 300)+`"}`); w.Code != 200 {
		t.Fatalf("long reason: %d %s", w.Code, w.Body.String())
	}
}

// B1 against trade_db: both outcomes persist (state, resource id, journal).
func TestSQLFulfilmentPersistsOutcome(t *testing.T) {
	repo := newSQLTestRepo(t)
	s := newOrderStoreWith(repo)
	_, q, p := newTestStore()
	s.quoter, s.payer = q, p

	newPaidOrder := func(token string) string {
		t.Helper()
		w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","regionId":"cn-north-1","clientToken":"`+token+`"}`)
		if w.Code != 200 {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		id := jsonNum(int64(dataOf(t, w)["orderId"].(float64)))
		if w := doPay(t, s, id, ``); w.Code != 200 {
			t.Fatalf("pay: %d %s", w.Code, w.Body.String())
		}
		return id
	}
	status := func(id string) (int, string) {
		t.Helper()
		var st int
		var res *string
		if err := repo.db.QueryRow(`SELECT status, resource_id FROM order_main WHERE order_id = ?`, id).Scan(&st, &res); err != nil {
			t.Fatal(err)
		}
		if res == nil {
			return st, ""
		}
		return st, *res
	}

	done := newPaidOrder("tok-sql-fulfil-ok")
	if w := doFulfilment(t, s, done, completedReport); w.Code != 200 {
		t.Fatalf("COMPLETED: %d %s", w.Code, w.Body.String())
	}
	if st, res := status(done); st != 4 || res != "euecs-cn-north-1-07-00002329" {
		t.Fatalf("persisted status %d resource %q, want 4 (COMPLETED) with the resource", st, res)
	}
	doneID, _ := strconv.ParseInt(done, 10, 64)
	if n := countJournal(t, repo, doneID); n != 4 { // created, paid, fulfilling, completed
		t.Fatalf("journal rows = %d, want 4", n)
	}

	failed := newPaidOrder("tok-sql-fulfil-fail")
	if w := doFulfilment(t, s, failed, failedReport); w.Code != 200 {
		t.Fatalf("FAILED: %d %s", w.Code, w.Body.String())
	}
	if st, _ := status(failed); st != 7 {
		t.Fatalf("persisted status %d, want 7 (REFUNDED)", st)
	}
	// Two orders paid ¥21.6 each; the failed one was refunded.
	if p.balance != 1_000_000-2160 {
		t.Fatalf("balance = %d, want 997840 (the failed order refunded)", p.balance)
	}
	if w := doFulfilment(t, s, failed, failedReport); w.Code != 200 {
		t.Fatalf("replayed FAILED: %d %s", w.Code, w.Body.String())
	}
}

// reasonRecorder records the journal reason of every transition it saves,
// keyed by the state the order moved to.
type reasonRecorder struct {
	orderRepo
	mu      sync.Mutex
	reasons map[order.State]string
}

func (r *reasonRecorder) SaveTransition(o *order.Order, from order.State, prevVersion int, reason string, evt order.Event, paymentRef string) error {
	err := r.orderRepo.SaveTransition(o, from, prevVersion, reason, evt, paymentRef)
	if err == nil {
		r.mu.Lock()
		r.reasons[o.State] = reason
		r.mu.Unlock()
	}
	return err
}

func (r *reasonRecorder) reason(st order.State) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reasons[st]
}

// captureLogs routes the default slog logger into a JSON buffer for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// logRecords decodes the captured JSON log lines.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// L5: a FAILED report for a paid order whose debit svc-payment cannot find
// (refunded=false while the payable is > 0) ended REFUNDED silently. svc-payment
// owns the money and says nothing was taken, so the order still settles
// REFUNDED (it holds no money; REFUNDING would never settle), but the journal
// says no debit was found and a reconciliation warning names the order and its
// payable. A refund that did happen carries no such note.
func TestFulfilmentFailedWithoutDebitWarnsForReconciliation(t *testing.T) {
	s, _, p := newTestStore()
	journal := &reasonRecorder{orderRepo: s.repo, reasons: map[order.State]string{}}
	s.repo = journal
	paidSeededOrder(t, s)
	// svc-payment has no debit under the order's pay key.
	p.mu.Lock()
	delete(p.debits, "order-pay-9001")
	p.balance = 1_000_000
	p.mu.Unlock()
	logs := captureLogs(t)

	if w := doFulfilment(t, s, "9001", failedReport); w.Code != 200 {
		t.Fatalf("FAILED without a debit: %d %s", w.Code, w.Body.String())
	}
	o := seededOrder(t, s)
	if o.State != order.StateRefunded || p.balance != 1_000_000 {
		t.Fatalf("order %s balance %d, want REFUNDED with nothing given back", o.State, p.balance)
	}
	got := journal.reason(order.StateRefunded)
	if !strings.Contains(got, "provisioning failed") || !strings.Contains(got, "no debit found under order-pay-9001, nothing refunded") {
		t.Fatalf("REFUNDED journal reason = %q, want the failure and the missing debit", got)
	}
	var warned int
	for _, rec := range logRecords(t, logs) {
		if rec["level"] == "WARN" && strings.Contains(fmt.Sprint(rec["msg"]), "reconciliation") {
			warned++
			if rec["orderId"] != float64(9001) || rec["payable"] != o.PayableAmount.String() || rec["payKey"] != "order-pay-9001" {
				t.Fatalf("reconciliation warning = %v, want orderId 9001, payable %s, payKey order-pay-9001", rec, o.PayableAmount.String())
			}
		}
	}
	if warned != 1 {
		t.Fatalf("%d reconciliation warnings, want 1; logs:\n%s", warned, logs.String())
	}

	// The replay is the settled 200 and does not warn again.
	logs.Reset()
	if w := doFulfilment(t, s, "9001", failedReport); w.Code != 200 {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(logs.String(), "reconciliation") {
		t.Fatalf("replay warned again: %s", logs.String())
	}

	// With the debit in place the refund is made and the journal has no note.
	s2, _, p2 := newTestStore()
	journal2 := &reasonRecorder{orderRepo: s2.repo, reasons: map[order.State]string{}}
	s2.repo = journal2
	paidSeededOrder(t, s2)
	logs.Reset()
	if w := doFulfilment(t, s2, "9001", failedReport); w.Code != 200 || p2.balance != 1_000_000 {
		t.Fatalf("FAILED with a debit: %d %s balance %d", w.Code, w.Body.String(), p2.balance)
	}
	if got := journal2.reason(order.StateRefunded); strings.Contains(got, "no debit") || strings.Contains(logs.String(), "reconciliation") {
		t.Fatalf("a real refund was flagged: reason %q, logs %s", got, logs.String())
	}
}

// L5 against trade_db: the note survives the journal column's truncation of a
// long failure reason.
func TestSQLFulfilmentFailedWithoutDebitJournalsIt(t *testing.T) {
	repo := newSQLTestRepo(t)
	s := newOrderStoreWith(repo)
	_, q, p := newTestStore()
	s.quoter, s.payer = q, p
	_ = captureLogs(t)

	w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","regionId":"cn-north-1","clientToken":"tok-sql-fulfil-nodebit"}`)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	id := jsonNum(int64(dataOf(t, w)["orderId"].(float64)))
	if w := doPay(t, s, id, ``); w.Code != 200 {
		t.Fatalf("pay: %d %s", w.Code, w.Body.String())
	}
	p.mu.Lock()
	delete(p.debits, "order-pay-"+id)
	p.mu.Unlock()

	if w := doFulfilment(t, s, id, `{"result":"FAILED","reason":"`+strings.Repeat("失", 300)+`"}`); w.Code != 200 {
		t.Fatalf("FAILED without a debit: %d %s", w.Code, w.Body.String())
	}
	var st int
	if err := repo.db.QueryRow(`SELECT status FROM order_main WHERE order_id = ?`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := repo.db.QueryRow(`SELECT reason FROM order_state_log WHERE order_id = ? AND to_status = ?`, id, st).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if st != 7 || !strings.HasSuffix(reason, "; no debit found under order-pay-"+id+", nothing refunded") ||
		len([]rune(reason)) > maxStateLogReasonRunes {
		t.Fatalf("status %d, journal reason %q (%d runes); want 7 (REFUNDED) ending with the no-debit note",
			st, reason, len([]rune(reason)))
	}
}

// --- B2: one unit per order -----------------------------------------------------

func TestCreateRejectsQuantityOtherThanOne(t *testing.T) {
	s, q, _ := newTestStore()
	for _, qty := range []string{"2", "1000", "-1"} {
		w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","quantity":`+qty+`,"clientToken":"tok-qty-`+qty+`"}`)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "Order.QuantityUnsupported") {
			t.Fatalf("quantity %s: %d %s, want 400 Order.QuantityUnsupported", qty, w.Code, w.Body.String())
		}
	}
	if len(q.calls) != 0 {
		t.Fatal("a refused quantity was priced")
	}
	// Omitted (or 0) still defaults to one unit.
	for _, body := range []string{
		`{"productCode":"euecs","skuCode":"euecs.c1","clientToken":"tok-qty-none"}`,
		`{"productCode":"euecs","skuCode":"euecs.c1","quantity":0,"clientToken":"tok-qty-0"}`,
		`{"productCode":"euecs","skuCode":"euecs.c1","quantity":1,"clientToken":"tok-qty-1"}`,
	} {
		w := doCreate(t, s, body)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
		if d := dataOf(t, w); d["quantity"] != float64(1) || d["payableAmount"] != "21.6" {
			t.Fatalf("%s: order = %v, want one unit at ¥21.6", body, d)
		}
	}
}

// --- B7: which unique key a create collided with ---------------------------------

func TestDuplicateKeyName(t *testing.T) {
	for msg, want := range map[string]string{
		"Error 1062 (23000): Duplicate entry '100123-tok' for key 'order_main.uk_client_token'": "uk_client_token",
		"Error 1062 (23000): Duplicate entry 'SO2026000042' for key 'order_main.uk_order_no'":   "uk_order_no",
		"Error 1062 (23000): Duplicate entry '42' for key 'order_main.PRIMARY'":                 "PRIMARY",
		"Error 1062: Duplicate entry 'SO2026000042' for key 'uk_order_no'":                      "uk_order_no",
		// The entry is client data: a token naming another key must not fool it.
		"Error 1062 (23000): Duplicate entry '100123-x' for key 'uk_order_no' for key 'order_main.uk_client_token'": "uk_client_token",
		"connection refused": "",
	} {
		if got := duplicateKeyName(errors.New(msg)); got != want {
			t.Errorf("duplicateKeyName(%q) = %q, want %q", msg, got, want)
		}
	}
}

// collidingRepo reports the first n creates as a taken order id/number.
type collidingRepo struct {
	orderRepo
	n     int
	tried []int64
}

func (c *collidingRepo) Create(o *order.Order, evt order.Event, coupons []pricing.CouponUse) error {
	c.tried = append(c.tried, o.OrderID)
	if c.n > 0 {
		c.n--
		return fmt.Errorf("%w: order %d", errDuplicateOrderKey, o.OrderID)
	}
	return c.orderRepo.Create(o, evt, coupons)
}

// Regression: a taken order number was reported as a duplicate client token,
// the token lookup found nothing and the create failed with a 500.
func TestCreateRetriesTakenOrderNumberOnce(t *testing.T) {
	s, _, _ := newTestStore()
	repo := &collidingRepo{orderRepo: s.repo, n: 1}
	s.repo = repo
	w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","clientToken":"tok-collide"}`)
	if w.Code != 200 {
		t.Fatalf("create after one collision: %d %s", w.Code, w.Body.String())
	}
	d := dataOf(t, w)
	if len(repo.tried) != 2 || int64(d["orderId"].(float64)) != repo.tried[1] || repo.tried[0] == repo.tried[1] {
		t.Fatalf("tried ids %v, order %v: want a second attempt under a fresh id", repo.tried, d["orderId"])
	}
	if d["orderNo"] != orderNumber(repo.tried[1]) {
		t.Fatalf("orderNo = %v, want the fresh id's number", d["orderNo"])
	}

	// Colliding again is a generic 500, not a loop and not a leak.
	repo.n = 2
	w = doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","clientToken":"tok-collide-2"}`)
	if w.Code != 500 || !strings.Contains(w.Body.String(), "Order.CreateFailed") || strings.Contains(w.Body.String(), "duplicate") {
		t.Fatalf("two collisions: %d %s, want a generic 500", w.Code, w.Body.String())
	}
}

func TestSQLRepoClassifiesDuplicateKeys(t *testing.T) {
	repo := newSQLTestRepo(t)
	first, evt := sqlTestOrder(repo, "x' for key 'uk_order_no")
	if err := repo.Create(first, evt, nil); err != nil {
		t.Fatal(err)
	}

	sameToken, e1 := sqlTestOrder(repo, first.ClientToken)
	if err := repo.Create(sameToken, e1, nil); !errors.Is(err, errDuplicateClientToken) {
		t.Fatalf("same client token: %v, want errDuplicateClientToken", err)
	}
	sameNo, e2 := sqlTestOrder(repo, "tok-other-no")
	sameNo.OrderNo, e2.OrderNo = first.OrderNo, first.OrderNo
	if err := repo.Create(sameNo, e2, nil); !errors.Is(err, errDuplicateOrderKey) || errors.Is(err, errDuplicateClientToken) {
		t.Fatalf("same order number: %v, want errDuplicateOrderKey", err)
	}
	sameID, e3 := testOrder(first.OrderID, "tok-other-id")
	sameID.OrderNo = "SO-other-id"
	if err := repo.Create(sameID, e3, nil); !errors.Is(err, errDuplicateOrderKey) {
		t.Fatalf("same order id: %v, want errDuplicateOrderKey", err)
	}
}

// reissuingRepo hands out an id that is already taken before the real ones.
type reissuingRepo struct {
	*sqlOrderRepo
	taken int64
}

func (r *reissuingRepo) NewOrderID() int64 {
	if id := r.taken; id != 0 {
		r.taken = 0
		return id
	}
	return r.sqlOrderRepo.NewOrderID()
}

func TestSQLCreateRetriesTakenOrderID(t *testing.T) {
	sqlRepo := newSQLTestRepo(t)
	existing, evt := sqlTestOrder(sqlRepo, "tok-existing")
	existing.OrderNo = orderNumber(existing.OrderID)
	evt.OrderNo = existing.OrderNo
	if err := sqlRepo.Create(existing, evt, nil); err != nil {
		t.Fatal(err)
	}
	s := newOrderStoreWith(&reissuingRepo{sqlOrderRepo: sqlRepo, taken: existing.OrderID})
	_, q, _ := newTestStore()
	s.quoter = q
	w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","regionId":"cn-north-1","clientToken":"tok-reissued"}`)
	if w.Code != 200 {
		t.Fatalf("create over a reissued id: %d %s", w.Code, w.Body.String())
	}
	id := int64(dataOf(t, w)["orderId"].(float64))
	if id == existing.OrderID {
		t.Fatal("the new order reused the taken id")
	}
	got, err := sqlRepo.ByClientToken(100123, "tok-reissued")
	if err != nil || got == nil || got.OrderID != id || got.PayableAmount != pricing.MustParseAmount("21.6") {
		t.Fatalf("persisted order = %+v, %v", got, err)
	}
}
