package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/trial"
)

func doCancel(t *testing.T, s *orderStore, id string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders/"+id+"/cancel", nil)
	r.SetPathValue("id", id)
	r.Header.Set(accountIDHeader, "100123")
	w := httptest.NewRecorder()
	s.handleCancel(w, r)
	return w
}

// T4: the debit happens before the order is claimed. A cancel landing between
// the debit and the PAID transition used to leave a CANCELLED order holding the
// customer's money with nothing to refund it.
func TestPayLosingToCancelRefundsTheDebit(t *testing.T) {
	s, _, p := newTestStore()
	// The cancel commits right after svc-payment took the money.
	p.afterConsume = func() {
		if w := doCancel(t, s, "9001"); w.Code != 200 {
			t.Errorf("concurrent cancel: %d %s", w.Code, w.Body.String())
		}
	}
	w := doPay(t, s, "9001", ``)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "refunded") {
		t.Fatalf("pay that lost to cancel: %d %s, want 409 (payment refunded)", w.Code, w.Body.String())
	}
	if o := seededOrder(t, s); o.State != order.StateCancelled {
		t.Fatalf("order = %s, want CANCELLED", o.State)
	}
	if p.balance != 1_000_000 || !p.refunded["order-pay-9001"] {
		t.Fatalf("balance = %d refunded=%v: the cancelled order kept the money", p.balance, p.refunded)
	}
}

// The other interleaving: the cancel commits (finding nothing to refund) BEFORE
// the debit lands. The losing /pay must give the money back itself.
func TestPayDebitLandingAfterCancelIsRefunded(t *testing.T) {
	s, _, p := newTestStore()
	p.beforeConsume = func() {
		if w := doCancel(t, s, "9001"); w.Code != 200 {
			t.Errorf("concurrent cancel: %d %s", w.Code, w.Body.String())
		}
	}
	if w := doPay(t, s, "9001", ``); w.Code != 409 {
		t.Fatalf("pay: %d %s, want 409", w.Code, w.Body.String())
	}
	if p.debits["order-pay-9001"] != 216000 {
		t.Fatalf("the race did not debit (debits %v) — test is not exercising the window", p.debits)
	}
	if p.balance != 1_000_000 {
		t.Fatalf("balance = %d, want the debit refunded", p.balance)
	}
}

// A /pay that crashed after the debit leaves a PENDING order with the money
// taken; cancelling it must return that money.
func TestCancelRefundsDebitLeftByCrashedPay(t *testing.T) {
	s, _, p := newTestStore()
	if _, err := p.Consume(context.Background(), 100123, 216000, orderPayKey(9001), "SO202608110001", ""); err != nil {
		t.Fatal(err)
	}
	if w := doCancel(t, s, "9001"); w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if p.balance != 1_000_000 {
		t.Fatalf("balance = %d, want 1000000 (debit refunded)", p.balance)
	}
	// Retrying the cancel or a late /pay never refunds twice.
	doCancel(t, s, "9001")
	doPay(t, s, "9001", ``)
	if p.balance != 1_000_000 {
		t.Fatalf("balance = %d after retries, refunded more than once", p.balance)
	}
}

func TestCancelWithoutDebitRefundsNothing(t *testing.T) {
	s, _, p := newTestStore()
	if w := doCancel(t, s, "9001"); w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if p.balance != 1_000_000 || len(p.refunded) != 0 {
		t.Fatalf("balance %d refunded %v", p.balance, p.refunded)
	}
}

// T2: an entry under the order's pay key that is not this order's payment (a
// pre-planted 1-分 debit) must never mark the order PAID.
func TestPayConflictingKeyLeavesOrderUnpaid(t *testing.T) {
	s, _, p := newTestStore()
	p.debits["order-pay-9001"] = 1
	w := doPay(t, s, "9001", ``)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "Order.PaymentConflict") {
		t.Fatalf("pay: %d %s, want 409 Order.PaymentConflict", w.Code, w.Body.String())
	}
	if o := seededOrder(t, s); o.State != order.StatePendingPayment {
		t.Fatalf("order = %s, want PENDING_PAYMENT", o.State)
	}
}

// T9: client_token is VARCHAR(64); a longer one used to reach the INSERT and
// come back as a 500 (or be truncated into a collision).
func TestCreateRejectsOversizedFields(t *testing.T) {
	s, q, _ := newTestStore()
	long := strings.Repeat("t", 65)
	for name, body := range map[string]string{
		"clientToken": `{"productCode":"euecs","skuCode":"euecs.c1","clientToken":"` + long + `"}`,
		"productCode": `{"productCode":"` + strings.Repeat("p", 33) + `","skuCode":"euecs.c1"}`,
		"skuCode":     `{"productCode":"euecs","skuCode":"` + long + `"}`,
		"regionId":    `{"productCode":"euecs","skuCode":"euecs.c1","regionId":"` + strings.Repeat("r", 33) + `"}`,
	} {
		if w := doCreate(t, s, body); w.Code != 400 {
			t.Errorf("%s: %d %s, want 400", name, w.Code, w.Body.String())
		}
	}
	if len(q.calls) != 0 {
		t.Fatalf("oversized requests reached the pricing engine: %+v", q.calls)
	}
	if w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","clientToken":"`+long[:64]+`"}`); w.Code != 200 {
		t.Fatalf("64-byte token: %d %s", w.Code, w.Body.String())
	}
}

// Cross-service contract: svc-orchestrator derives a prepaid resource's
// ExpiredAt from the order's term, read from this JSON.
func TestOrderJSONCarriesPurchasedTerm(t *testing.T) {
	s, _, _ := newTestStore()
	d := dataOf(t, doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","quantity":1,"duration":3,"clientToken":"tok-term"}`))
	if d["quantity"] != float64(1) || d["duration"] != float64(3) || d["durationUnit"] != "MONTH" {
		t.Fatalf("order JSON = %v, want quantity 1, duration 3, durationUnit MONTH", d)
	}
}

// T8: the in-memory repo must behave like the SQL one it stands in for.
func TestMemRepoMatchesSQLSemantics(t *testing.T) {
	repo := newMemOrderRepo()
	first, evt := testOrder(repo.NewOrderID(), "tok-dup")
	if err := repo.Create(first, evt, nil); err != nil {
		t.Fatal(err)
	}
	second, evt2 := testOrder(repo.NewOrderID(), "tok-dup")
	if err := repo.Create(second, evt2, nil); err == nil {
		t.Fatal("a second order under the same (account, clientToken) was accepted")
	}
	if got, _ := repo.ByClientToken(100123, "tok-dup"); got == nil || got.OrderID != first.OrderID {
		t.Fatalf("token now resolves to %+v, want the first order", got)
	}
	// The same token under another account is a different key.
	other, evt3 := testOrder(repo.NewOrderID(), "tok-dup")
	other.AccountID = 999
	if err := repo.Create(other, evt3, nil); err != nil {
		t.Fatalf("other account, same token: %v", err)
	}

	for i := 0; i < 5; i++ {
		o, e := testOrder(repo.NewOrderID(), "tok-list-"+jsonNum(int64(i)))
		if err := repo.Create(o, e, nil); err != nil {
			t.Fatal(err)
		}
	}
	list, err := repo.List(100123)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].OrderID <= list[i].OrderID {
			t.Fatalf("List not newest-first (order_id DESC): %d before %d", list[i-1].OrderID, list[i].OrderID)
		}
	}

	_ = repo.SetAutoRenew(100123, autoRenewSetting{ResourceID: "res-b", ProductCode: "eurds", Enabled: true})
	_ = repo.SetAutoRenew(100123, autoRenewSetting{ResourceID: "res-a", ProductCode: "euecs", Enabled: false})
	settings, err := repo.ListAutoRenew(100123)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != 2 || settings[0].ResourceID != "res-a" || settings[0].ProductCode != "euecs" ||
		settings[1].ProductCode != "eurds" || !settings[1].Enabled {
		t.Fatalf("auto-renew = %+v, want product codes kept, ordered by resource id", settings)
	}
}

// sqlTestOrder is testOrder with a distinct order_no (uk_order_no).
func sqlTestOrder(repo orderRepo, token string) (*order.Order, order.Event) {
	o, evt := testOrder(repo.NewOrderID(), token)
	o.OrderNo = "SO" + jsonNum(o.OrderID)
	evt.OrderNo = o.OrderNo
	return o, evt
}

func TestSQLRepoListAndAutoRenewMatchMemory(t *testing.T) {
	repo := newSQLTestRepo(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		o, e := sqlTestOrder(repo, "tok-sql-list-"+jsonNum(int64(i)))
		if err := repo.Create(o, e, nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, o.OrderID)
	}
	list, err := repo.List(100123)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].OrderID != ids[2] || list[2].OrderID != ids[0] {
		t.Fatalf("SQL list order = %v", list)
	}
	dup, e := sqlTestOrder(repo, "tok-sql-list-0")
	if err := repo.Create(dup, e, nil); err == nil {
		t.Fatal("SQL accepted a duplicate client token")
	}
	_ = repo.SetAutoRenew(100123, autoRenewSetting{ResourceID: "res-a", ProductCode: "euecs", Enabled: true})
	settings, err := repo.ListAutoRenew(100123)
	if err != nil || len(settings) != 1 || settings[0].ProductCode != "euecs" {
		t.Fatalf("auto-renew = %+v %v", settings, err)
	}
}

// T5: paid_amount was zeroed on every transition out of PENDING_PAYMENT —
// i.e. on exactly the one that records the payment.
func TestSQLRepoRecordsPaidAmount(t *testing.T) {
	repo := newSQLTestRepo(t)
	paidAmount := func(id int64) string {
		t.Helper()
		var v string
		if err := repo.db.QueryRow(`SELECT paid_amount FROM order_main WHERE order_id = ?`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	m := order.NewMachine(func() time.Time { return testClock.Add(time.Minute) })

	o, evt := sqlTestOrder(repo, "tok-paid")
	if err := repo.Create(o, evt, nil); err != nil {
		t.Fatal(err)
	}
	from, prev := o.State, o.Version
	paidEvt, err := m.MarkPaid(o, o.PayableAmount, o.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTransition(o, from, prev, "payment", paidEvt, "ledger:1"); err != nil {
		t.Fatal(err)
	}
	if got := paidAmount(o.OrderID); got != "100.00" {
		t.Fatalf("paid_amount after PAID = %s, want 100.00", got)
	}
	from, prev = o.State, o.Version
	fulfilEvt, err := m.StartFulfilment(o, o.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTransition(o, from, prev, "fulfil", fulfilEvt, ""); err != nil {
		t.Fatal(err)
	}
	if got := paidAmount(o.OrderID); got != "100.00" {
		t.Fatalf("paid_amount after FULFILLING = %s, want 100.00", got)
	}

	c, cevt := sqlTestOrder(repo, "tok-cancel")
	if err := repo.Create(c, cevt, nil); err != nil {
		t.Fatal(err)
	}
	from, prev = c.State, c.Version
	cancelEvt, err := m.Cancel(c, c.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTransition(c, from, prev, "cancel", cancelEvt, ""); err != nil {
		t.Fatal(err)
	}
	if got := paidAmount(c.OrderID); got != "0.00" {
		t.Fatalf("paid_amount of a cancelled order = %s, want 0.00", got)
	}
}

// T12: Active and the activity budget count LIVE trials. They only ever grew,
// so a lapsed voucher kept its account at ACTIVE_LIMIT and its budget slot
// spent forever.
func TestTrialExpiryReleasesActiveCounters(t *testing.T) {
	s := newTrialStore()
	s.policy.MaxActiveGlobal = 1
	s.devRealNames[100300] = realNameStatus{Verified: true, IdentityKey: "idn-other"}
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return t0 }

	if w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100123"); w.Code != 200 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100300")
	if code, _, _ := trialEnvelope(t, w); code != string(trial.RuleGlobalBudget) {
		t.Fatalf("budget full: got %s, want %s", code, trial.RuleGlobalBudget)
	}

	// The voucher lapses.
	s.now = func() time.Time { return t0.Add(trial.VoucherValidDays*24*time.Hour + time.Second) }
	_, d, _ := trialEnvelope(t, doTrial(t, s, http.MethodGet, "/api/v1/trial/status", "100123"))
	if d["reason"] == string(trial.RuleActiveLimit) {
		t.Fatalf("expired voucher still counts as active: %v", d)
	}
	if m := memTrial(s); m.accounts[100123].Active != 0 || m.globalActive != 0 {
		t.Fatalf("counters not released: active=%d global=%d", m.accounts[100123].Active, m.globalActive)
	}
	if w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", "100300"); w.Code != 200 {
		t.Fatalf("budget slot not returned: %d %s", w.Code, w.Body.String())
	}
}
