package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

// --- helpers -------------------------------------------------------------------

// memTrial is the in-memory trial state behind a test store.
func memTrial(s *trialStore) *memTrialRepo { return s.repo.(*memTrialRepo) }

// trialCoupons lists the account's TRIAL vouchers.
func trialCoupons(t *testing.T, s *trialStore, acct int64) []couponRecord {
	t.Helper()
	all, err := s.coupons.List(acct)
	if err != nil {
		t.Fatal(err)
	}
	var out []couponRecord
	for _, c := range all {
		if c.Source == couponSourceTrial {
			out = append(out, c)
		}
	}
	return out
}

func newMemTrialStoreFromEnv() (*trialStore, error) {
	coupons := newMemCouponRepo()
	return newTrialStoreFromEnv(newMemTrialRepo(coupons), coupons)
}

func testVoucher(id string, acct int64, face string, expire time.Time) couponRecord {
	f := pricing.MustParseAmount(face)
	return couponRecord{
		Coupon: pricing.Coupon{CouponID: id, AccountID: acct, Kind: pricing.KindVoucher,
			FaceValue: f, RemainValue: f, ExpireAt: expire},
		Source: "PROMO",
	}
}

func issue(t *testing.T, repo couponRepo, cs ...couponRecord) {
	t.Helper()
	for _, c := range cs {
		if err := repo.Issue(c); err != nil {
			t.Fatalf("issue %s: %v", c.CouponID, err)
		}
	}
}

func couponByID(t *testing.T, repo couponRepo, acct int64, id string) couponRecord {
	t.Helper()
	got, err := repo.Get(acct, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := got[id]
	if !ok {
		t.Fatalf("coupon %s not found", id)
	}
	return c
}

func doCouponList(t *testing.T, s *orderStore, query string) (int, []map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/coupons?"+query, nil)
	r.Header.Set(accountIDHeader, "100123")
	w := httptest.NewRecorder()
	s.handleCouponList(w, r)
	if w.Code != 200 {
		return w.Code, nil
	}
	var env struct {
		Data struct {
			Coupons []map[string]any `json:"coupons"`
			Total   int              `json:"total"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad envelope: %v", err)
	}
	if env.Data.Total != len(env.Data.Coupons) {
		t.Fatalf("total %d != %d coupons", env.Data.Total, len(env.Data.Coupons))
	}
	return w.Code, env.Data.Coupons
}

func couponRow(rows []map[string]any, id string) map[string]any {
	for _, r := range rows {
		if r["couponId"] == id {
			return r
		}
	}
	return nil
}

func errEnvelope(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var env struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad envelope: %v (%s)", err, w.Body.String())
	}
	return env.Code, env.Message
}

// createWithCoupons orders euecs.c1 (¥21.6/month) for `months` with coupons.
func createWithCoupons(t *testing.T, s *orderStore, months int, token string, ids ...string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(ids)
	body := fmt.Sprintf(`{"productCode":"euecs","skuCode":"euecs.c1","regionId":"cn-north-1","duration":%d,"clientToken":%q,"couponIds":%s}`,
		months, token, raw)
	return doCreate(t, s, body)
}

func orderIDOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	d := dataOf(t, w)
	id, ok := d["orderId"].(float64)
	if !ok {
		t.Fatalf("no orderId in %v", d)
	}
	return strconv.FormatInt(int64(id), 10)
}

var couponClock = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// --- checkout through the HTTP surface -----------------------------------------------

func TestCreateAppliesCouponsServerSide(t *testing.T) {
	s, _, p := newTestStore()
	s.machine.Now = func() time.Time { return couponClock }
	cs := s.repo.Coupons()
	issue(t, cs,
		testVoucher("CPV30", 100123, "30", couponClock.Add(48*time.Hour)),
		testVoucher("CPV10", 100123, "10", couponClock.Add(24*time.Hour)))

	// ¥21.6 × 5 = ¥108; vouchers by expiry: CPV10 (10) then CPV30 (30).
	w := createWithCoupons(t, s, 5, "cpn-1", "CPV30", "CPV10")
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	d := dataOf(t, w)
	if d["listAmount"] != "108" || d["couponDeduction"] != "40" || d["payableAmount"] != "68" {
		t.Fatalf("amounts = list %v / deduction %v / payable %v", d["listAmount"], d["couponDeduction"], d["payableAmount"])
	}
	if ids, _ := d["couponIds"].([]any); len(ids) != 2 || ids[0] != "CPV10" || ids[1] != "CPV30" {
		t.Fatalf("couponIds = %v, want consumption order [CPV10 CPV30]", d["couponIds"])
	}
	id := orderIDOf(t, w)

	// Reserved: LOCKED with the holding order, and refused to another order.
	_, rows := doCouponList(t, s, "status=locked")
	if len(rows) != 2 || couponRow(rows, "CPV30")["lockedOrderId"] != d["orderId"] {
		t.Fatalf("locked coupons = %v", rows)
	}
	w2 := createWithCoupons(t, s, 1, "cpn-2", "CPV10")
	if code, msg := errEnvelope(t, w2); w2.Code != 400 || code != "Order.CouponInvalid" || !strings.Contains(msg, "CPV10") {
		t.Fatalf("reserved coupon reused: %d %s %s", w2.Code, code, msg)
	}

	// Pay debits the payable only, and spends the coupons.
	if w := doPay(t, s, id, ``); w.Code != 200 {
		t.Fatalf("pay: %d %s", w.Code, w.Body.String())
	}
	if p.debits[orderPayKey(mustAtoi(t, id))] != 6800 {
		t.Fatalf("debit = %v, want 6800 分", p.debits)
	}
	if c := couponByID(t, cs, 100123, "CPV30"); c.RemainValue != 0 || c.Status != couponUsedUp || c.LockedBy != 0 {
		t.Fatalf("CPV30 after pay = %+v", c)
	}
	_, used := doCouponList(t, s, "status=used")
	if len(used) != 2 {
		t.Fatalf("used coupons = %v", used)
	}

	// Fulfilment failed: the cash comes back AND the coupons come back.
	if w := doFulfilment(t, s, id, failedReport); w.Code != 200 {
		t.Fatalf("fulfilment: %d %s", w.Code, w.Body.String())
	}
	if !p.refunded[orderPayKey(mustAtoi(t, id))] {
		t.Fatal("cash not refunded")
	}
	_, usable := doCouponList(t, s, "status=usable")
	if len(usable) != 2 || couponRow(usable, "CPV30")["remainValue"] != "30" {
		t.Fatalf("coupons after refund = %v", usable)
	}
	// A replayed report restores nothing twice.
	if w := doFulfilment(t, s, id, failedReport); w.Code != 200 {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	if c := couponByID(t, cs, 100123, "CPV30"); c.RemainValue != pricing.MustParseAmount("30") {
		t.Fatalf("CPV30 after replay = %s", c.RemainValue)
	}
}

func mustAtoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCouponPartiallyConsumedAndReleasedOnCancel(t *testing.T) {
	s, _, p := newTestStore()
	s.machine.Now = func() time.Time { return couponClock }
	cs := s.repo.Coupons()
	issue(t, cs, testVoucher("CPV100", 100123, "100", couponClock.Add(time.Hour)))

	// ¥21.6 order, ¥100 voucher: fully covered, nothing to debit.
	w := createWithCoupons(t, s, 1, "part-1", "CPV100")
	d := dataOf(t, w)
	if w.Code != 200 || d["payableAmount"] != "0" || d["couponDeduction"] != "21.6" {
		t.Fatalf("create: %d %v", w.Code, d)
	}
	if w := doPay(t, s, orderIDOf(t, w), ``); w.Code != 200 {
		t.Fatalf("pay: %d %s", w.Code, w.Body.String())
	}
	if p.calls != 0 {
		t.Fatalf("a zero payable must not call svc-payment (calls=%d)", p.calls)
	}
	c := couponByID(t, cs, 100123, "CPV100")
	if c.RemainValue != pricing.MustParseAmount("78.4") || c.Status != couponPartial {
		t.Fatalf("partially spent voucher = %s status %d", c.RemainValue, c.Status)
	}

	// The remainder reserves on the next order and is released by a cancel.
	w = createWithCoupons(t, s, 1, "part-2", "CPV100")
	if w.Code != 200 {
		t.Fatalf("second create: %d %s", w.Code, w.Body.String())
	}
	id := orderIDOf(t, w)
	if c := couponByID(t, cs, 100123, "CPV100"); c.LockedBy == 0 {
		t.Fatal("coupon not reserved")
	}
	if w := doCancel(t, s, id); w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	c = couponByID(t, cs, 100123, "CPV100")
	if c.LockedBy != 0 || c.RemainValue != pricing.MustParseAmount("78.4") {
		t.Fatalf("after cancel = %+v", c)
	}
	uses, _ := s.repo.CouponUses(100123, mustAtoi(t, id))
	if len(uses) != 1 || uses[0].State != useReleased {
		t.Fatalf("journal after cancel = %+v", uses)
	}
	// The cancelled order still names its coupon.
	o, _ := s.repo.Get(100123, mustAtoi(t, id))
	if len(o.CouponIDs) != 1 || o.CouponIDs[0] != "CPV100" {
		t.Fatalf("cancelled order couponIds = %v", o.CouponIDs)
	}
}

func TestCreateRejectsInvalidCoupons(t *testing.T) {
	s, _, _ := newTestStore()
	s.machine.Now = func() time.Time { return couponClock }
	cs := s.repo.Coupons()
	rate := func(id string, bp int64) couponRecord {
		return couponRecord{Coupon: pricing.Coupon{CouponID: id, AccountID: 100123, Kind: pricing.KindRate,
			RateBasisPoints: bp, ExpireAt: couponClock.Add(time.Hour)}, Source: "PROMO"}
	}
	scoped := testVoucher("CPOSS", 100123, "5", couponClock.Add(time.Hour))
	scoped.ProductCodes = []string{"euoss"}
	th := couponRecord{Coupon: pricing.Coupon{CouponID: "CPTH", AccountID: 100123, Kind: pricing.KindThreshold,
		Threshold: pricing.MustParseAmount("500"), FaceValue: pricing.MustParseAmount("50"),
		RemainValue: pricing.MustParseAmount("50"), ExpireAt: couponClock.Add(time.Hour)}, Source: "PROMO"}
	used := testVoucher("CPUSED", 100123, "5", couponClock.Add(time.Hour))
	used.RemainValue, used.Status = 0, couponUsedUp
	issue(t, cs,
		testVoucher("CPFOREIGN", 200999, "5", couponClock.Add(time.Hour)),
		testVoucher("CPOLD", 100123, "5", couponClock.Add(-time.Second)),
		scoped, th, used, rate("CPR90", 9000), rate("CPR80", 8000),
		testVoucher("CPA", 100123, "50", couponClock.Add(time.Hour)),
		testVoucher("CPB", 100123, "50", couponClock.Add(2*time.Hour)))

	cases := []struct {
		name string
		ids  []string
		want string // message fragment
	}{
		{"unknown", []string{"CPNOPE"}, "CPNOPE does not exist"},
		{"foreign", []string{"CPFOREIGN"}, "CPFOREIGN does not exist"},
		{"expired", []string{"CPOLD"}, "CPOLD has expired"},
		{"used", []string{"CPUSED"}, "CPUSED has been used up"},
		{"product scope", []string{"CPOSS"}, "does not apply to product euecs"},
		{"threshold", []string{"CPTH"}, "at least 500"},
		{"two rate coupons", []string{"CPR90", "CPR80"}, "CPR90 cannot be combined"},
		{"not needed", []string{"CPA", "CPB"}, "CPB is not needed"},
	}
	for _, tc := range cases {
		w := createWithCoupons(t, s, 1, "inv-"+strings.ReplaceAll(tc.name, " ", "-"), tc.ids...)
		code, msg := errEnvelope(t, w)
		if w.Code != 400 || code != "Order.CouponInvalid" || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: %d %s %q, want 400 Order.CouponInvalid ~%q", tc.name, w.Code, code, msg, tc.want)
		}
	}
	// Shape errors are parameter errors.
	for _, ids := range [][]string{{"a", "b", "c", "d", "e", "f"}, {"CPA", "CPA"}, {"bad,id"}, {""}} {
		w := createWithCoupons(t, s, 1, "shape", ids...)
		if code, _ := errEnvelope(t, w); w.Code != 400 || code != "Common.InvalidParameter" {
			t.Errorf("couponIds %q: %d %s", ids, w.Code, code)
		}
	}
	// A postpaid order has nothing upfront to deduct from.
	w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1.post","regionId":"cn-north-1","couponIds":["CPA"]}`)
	if code, _ := errEnvelope(t, w); w.Code != 400 || code != "Order.CouponInvalid" {
		t.Errorf("postpaid with coupon: %d %s", w.Code, code)
	}
	// None of the refused creates reserved anything.
	all, _ := cs.List(100123)
	for _, c := range all {
		if c.LockedBy != 0 {
			t.Fatalf("refused create left %s reserved", c.CouponID)
		}
	}
	// A rate coupon prices in 分: 10% of ¥21.6 = ¥2.16.
	w = createWithCoupons(t, s, 1, "rate-ok", "CPR90")
	if d := dataOf(t, w); w.Code != 200 || d["couponDeduction"] != "2.16" || d["payableAmount"] != "19.44" {
		t.Fatalf("rate coupon: %d %v", w.Code, d)
	}
}

func TestPayRefusesExpiredCoupon(t *testing.T) {
	s, _, p := newTestStore()
	now := couponClock
	s.machine.Now = func() time.Time { return now }
	issue(t, s.repo.Coupons(), testVoucher("CPSOON", 100123, "5", couponClock.Add(time.Minute)))
	w := createWithCoupons(t, s, 1, "exp-1", "CPSOON")
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	id := orderIDOf(t, w)
	now = couponClock.Add(time.Hour)
	w = doPay(t, s, id, ``)
	if code, _ := errEnvelope(t, w); w.Code != 409 || code != "Order.CouponExpired" {
		t.Fatalf("pay with expired coupon: %d %s", w.Code, code)
	}
	if p.calls != 0 {
		t.Fatal("money moved for an order that cannot be paid")
	}
	// Cancelling returns the (expired) coupon's reservation.
	if w := doCancel(t, s, id); w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if c := couponByID(t, s.repo.Coupons(), 100123, "CPSOON"); c.LockedBy != 0 {
		t.Fatal("reservation kept after cancel")
	}
}

func TestCreateRetryWithCouponsIsIdempotent(t *testing.T) {
	s, _, _ := newTestStore()
	s.machine.Now = func() time.Time { return couponClock }
	issue(t, s.repo.Coupons(), testVoucher("CPI", 100123, "5", couponClock.Add(time.Hour)))
	first := createWithCoupons(t, s, 1, "idem-1", "CPI")
	again := createWithCoupons(t, s, 1, "idem-1", "CPI")
	if first.Code != 200 || again.Code != 200 || orderIDOf(t, first) != orderIDOf(t, again) {
		t.Fatalf("retry: %d %s / %d %s", first.Code, first.Body.String(), again.Code, again.Body.String())
	}
}

func TestCouponListAPI(t *testing.T) {
	s, _, _ := newTestStore()
	s.machine.Now = func() time.Time { return couponClock }
	cs := s.repo.Coupons()
	scoped := testVoucher("CPSKU", 100123, "5", couponClock.Add(3*time.Hour))
	scoped.SKUCodes = []string{"euecs.c2"}
	issue(t, cs,
		testVoucher("CPV", 100123, "20", couponClock.Add(time.Hour)),
		testVoucher("CPX", 100123, "20", couponClock.Add(-time.Hour)),
		testVoucher("CPOTHER", 200999, "20", couponClock.Add(time.Hour)),
		scoped,
		couponRecord{Coupon: pricing.Coupon{CouponID: "CPTH", AccountID: 100123, Kind: pricing.KindThreshold,
			Threshold: pricing.MustParseAmount("100"), FaceValue: pricing.MustParseAmount("10"),
			RemainValue: pricing.MustParseAmount("10"), ExpireAt: couponClock.Add(2 * time.Hour)}, Source: "PROMO"},
		couponRecord{Coupon: pricing.Coupon{CouponID: "CPR", AccountID: 100123, Kind: pricing.KindRate,
			RateBasisPoints: 9000, CapAmount: pricing.MustParseAmount("8"), ExpireAt: couponClock.Add(2 * time.Hour)}, Source: "PROMO"})

	_, all := doCouponList(t, s, "")
	if len(all) != 5 || couponRow(all, "CPOTHER") != nil {
		t.Fatalf("all = %v (another account's coupon must never show)", all)
	}
	if all[0]["couponId"] != "CPX" {
		t.Fatalf("list must be soonest-expiry first: %v", all)
	}
	if r := couponRow(all, "CPX"); r["status"] != "EXPIRED" || r["applicable"] != nil {
		t.Fatalf("CPX = %v", r)
	}
	if r := couponRow(all, "CPTH"); r["kind"] != "FULL_REDUCTION" || r["thresholdAmount"] != "100" || r["faceValue"] != "10" {
		t.Fatalf("CPTH = %v", r)
	}
	if r := couponRow(all, "CPR"); r["kind"] != "DISCOUNT" || r["discountRateBp"] != float64(9000) || r["capAmount"] != "8" {
		t.Fatalf("CPR = %v", r)
	}
	if r := couponRow(all, "CPV"); r["kind"] != "VOUCHER" || r["source"] != "PROMO" || r["status"] != "USABLE" {
		t.Fatalf("CPV = %v", r)
	}
	if pc, _ := couponRow(all, "CPV")["productCodes"].([]any); pc == nil || len(pc) != 0 {
		t.Fatalf("productCodes must be an empty array when unrestricted: %v", couponRow(all, "CPV"))
	}

	_, usable := doCouponList(t, s, "status=usable&productCode=euecs&skuCode=euecs.c1&amount=50")
	if len(usable) != 4 {
		t.Fatalf("usable = %v", usable)
	}
	want := map[string]bool{"CPV": true, "CPSKU": false, "CPTH": false, "CPR": true}
	for id, ok := range want {
		if got := couponRow(usable, id)["applicable"]; got != ok {
			t.Errorf("%s applicable = %v, want %v", id, got, ok)
		}
	}
	_, big := doCouponList(t, s, "status=usable&amount=150")
	if couponRow(big, "CPTH")["applicable"] != true || couponRow(big, "CPSKU")["applicable"] != true {
		t.Fatalf("threshold met / SKU not named: %v", big)
	}
	if code, _ := doCouponList(t, s, "status=bogus"); code != 400 {
		t.Fatalf("bad status filter: %d", code)
	}
	if code, _ := doCouponList(t, s, "amount=-1"); code != 400 {
		t.Fatalf("negative amount: %d", code)
	}

	// Detail: own coupon 200, foreign or unknown 404.
	for id, wantCode := range map[string]int{"CPV": 200, "CPOTHER": 404, "NOPE": 404} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/coupons/"+id, nil)
		r.SetPathValue("couponId", id)
		r.Header.Set(accountIDHeader, "100123")
		w := httptest.NewRecorder()
		s.handleCouponDetail(w, r)
		if w.Code != wantCode {
			t.Errorf("detail %s: %d, want %d", id, w.Code, wantCode)
		}
	}
}

// D7 end to end: the claimed trial voucher lands in the coupon store the order
// plane spends from, and pays for an order.
func TestTrialVoucherPaysForAnOrder(t *testing.T) {
	s, _, p := newTestStore()
	mem := s.repo.(*memOrderRepo)
	trials := newTrialStoreWith(newMemTrialRepo(mem.coupons), mem.coupons)
	w := doTrial(t, trials, http.MethodPost, "/api/v1/trial/claim", "100123")
	if w.Code != 200 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	_, d, _ := trialEnvelope(t, w)
	couponID, _ := d["couponId"].(string)

	w = createWithCoupons(t, s, 1, "trial-order", couponID)
	if od := dataOf(t, w); w.Code != 200 || od["payableAmount"] != "0" || od["couponDeduction"] != "21.6" {
		t.Fatalf("order with trial voucher: %d %s", w.Code, w.Body.String())
	}
	if w := doPay(t, s, orderIDOf(t, w), ``); w.Code != 200 || p.calls != 0 {
		t.Fatalf("pay: %d %s (payment calls %d)", w.Code, w.Body.String(), p.calls)
	}
	_, sd, _ := trialEnvelope(t, doTrial(t, trials, http.MethodGet, "/api/v1/trial/status", "100123"))
	vs, _ := sd["coupons"].([]any)
	if len(vs) != 1 || vs[0].(map[string]any)["remainValue"] != "78.4" {
		t.Fatalf("trial status coupons = %v", sd["coupons"])
	}
}

// --- store parity: memory twin vs trade_db ---------------------------------------------

// couponRepoScenario walks reserve → consume → restore and reserve → release
// at the repo level and returns the account's final coupons.
func couponRepoScenario(t *testing.T, repo orderRepo) []couponRecord {
	t.Helper()
	const acct = int64(100123)
	cs := repo.Coupons()
	issue(t, cs,
		testVoucher("PV50", acct, "50", testClock.Add(48*time.Hour)),
		testVoucher("PV20", acct, "20", testClock.Add(24*time.Hour)),
		testVoucher("PVX", 999, "20", testClock.Add(24*time.Hour)))
	if err := cs.Issue(testVoucher("PV50", acct, "1", testClock)); !errors.Is(err, errDuplicateCoupon) {
		t.Fatalf("duplicate issue: %v", err)
	}
	machine := order.NewMachine(func() time.Time { return testClock.Add(time.Minute) })
	step := func(o *order.Order, to order.State) {
		t.Helper()
		from, prev := o.State, o.Version
		evt, err := machine.Transition(o, to, o.Version)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveTransition(o, from, prev, "test", evt, ""); err != nil {
			t.Fatalf("%s → %s: %v", from, to, err)
		}
	}
	newOrder := func(token string, uses ...pricing.CouponUse) (*order.Order, error) {
		o, evt := testOrder(repo.NewOrderID(), token)
		o.OrderNo = fmt.Sprintf("SO%d", o.OrderID)
		evt.OrderID, evt.OrderNo = o.OrderID, o.OrderNo
		var ded pricing.Amount
		for _, u := range uses {
			ded += u.Amount
			o.CouponIDs = append(o.CouponIDs, u.CouponID)
		}
		o.DiscountAmount, o.PayableAmount = ded, o.OriginalAmount-ded
		return o, repo.Create(o, evt, uses)
	}

	o1, err := newOrder("par-1", pricing.CouponUse{CouponID: "PV20", Amount: pricing.MustParseAmount("20")},
		pricing.CouponUse{CouponID: "PV50", Amount: pricing.MustParseAmount("30")})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := repo.Get(acct, o1.OrderID)
	if got == nil || strings.Join(got.CouponIDs, ",") != "PV20,PV50" || got.PayableAmount != pricing.MustParseAmount("50") {
		t.Fatalf("reloaded order = %+v", got)
	}
	if c := couponByID(t, cs, acct, "PV50"); c.LockedBy != o1.OrderID {
		t.Fatalf("PV50 lockedBy = %d", c.LockedBy)
	}
	// Reserved coupons are refused to a second order, which is not written.
	var unavailable *couponUnavailableError
	if _, err := newOrder("par-2", pricing.CouponUse{CouponID: "PV50", Amount: pricing.MustParseAmount("1")}); !errors.As(err, &unavailable) {
		t.Fatalf("double reservation: %v", err)
	}
	if o, _ := repo.ByClientToken(acct, "par-2"); o != nil {
		t.Fatal("refused create wrote an order")
	}
	// Another account's coupon cannot be reserved.
	if _, err := newOrder("par-3", pricing.CouponUse{CouponID: "PVX", Amount: pricing.MustParseAmount("1")}); !errors.As(err, &unavailable) {
		t.Fatalf("foreign reservation: %v", err)
	}

	step(o1, order.StatePaid)
	if c := couponByID(t, cs, acct, "PV50"); c.RemainValue != pricing.MustParseAmount("20") || c.Status != couponPartial || c.LockedBy != 0 {
		t.Fatalf("PV50 after pay = %+v", c)
	}
	if c := couponByID(t, cs, acct, "PV20"); c.RemainValue != 0 || c.Status != couponUsedUp {
		t.Fatalf("PV20 after pay = %+v", c)
	}
	step(o1, order.StateRefunding)
	step(o1, order.StateRefunded)
	uses, err := repo.CouponUses(acct, o1.OrderID)
	if err != nil || len(uses) != 2 || uses[0].State != useRestored || uses[1].State != useRestored {
		t.Fatalf("journal after refund = %+v (%v)", uses, err)
	}
	if !uses[0].ExpireAt.Equal(testClock.Add(24 * time.Hour)) {
		t.Fatalf("journal expiry = %v", uses[0].ExpireAt)
	}

	o4, err := newOrder("par-4", pricing.CouponUse{CouponID: "PV50", Amount: pricing.MustParseAmount("10")})
	if err != nil {
		t.Fatal(err)
	}
	step(o4, order.StateCancelled)
	if uses, _ := repo.CouponUses(acct, o4.OrderID); len(uses) != 1 || uses[0].State != useReleased {
		t.Fatalf("journal after cancel = %+v", uses)
	}
	out, err := cs.List(acct)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCouponRepoParity(t *testing.T) {
	mem := couponRepoScenario(t, newMemOrderRepo())
	for _, c := range mem {
		if c.LockedBy != 0 || c.RemainValue != c.FaceValue || c.Status != couponUnused {
			t.Fatalf("memory: %s not back to new: %+v", c.CouponID, c)
		}
	}
	db := sqltest.Open(t, sqltest.Services("svc-catalog"), sqltest.Services("svc-payment"), sqltest.Services("svc-order"))
	repo, err := newSQLRepo(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	sqlOut := couponRepoScenario(t, repo)
	if len(sqlOut) != len(mem) {
		t.Fatalf("sql %d coupons, memory %d", len(sqlOut), len(mem))
	}
	for i := range mem {
		m, s := mem[i], sqlOut[i]
		if m.CouponID != s.CouponID || m.RemainValue != s.RemainValue || m.Status != s.Status ||
			m.LockedBy != s.LockedBy || !m.ExpireAt.Equal(s.ExpireAt) || m.EffectiveKind() != s.EffectiveKind() || m.Source != s.Source {
			t.Fatalf("coupon %d: memory %+v, sql %+v", i, m, s)
		}
	}
}

// Two orders racing for one coupon on trade_db: exactly one reservation wins.
func TestSQLCouponReservationRace(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-catalog"), sqltest.Services("svc-payment"), sqltest.Services("svc-order"))
	repo, err := newSQLRepo(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	issue(t, repo.Coupons(), testVoucher("RACE", 100123, "10", testClock.Add(time.Hour)))
	const n = 6
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o, evt := testOrder(repo.NewOrderID(), fmt.Sprintf("race-%d", i))
			o.OrderNo = fmt.Sprintf("SO%d", o.OrderID)
			evt.OrderID, evt.OrderNo = o.OrderID, o.OrderNo
			o.CouponIDs = []string{"RACE"}
			o.DiscountAmount, o.PayableAmount = pricing.MustParseAmount("10"), pricing.MustParseAmount("90")
			err := repo.Create(o, evt, []pricing.CouponUse{{CouponID: "RACE", Amount: pricing.MustParseAmount("10")}})
			var unavailable *couponUnavailableError
			switch {
			case err == nil:
				mu.Lock()
				wins++
				mu.Unlock()
			case !errors.As(err, &unavailable):
				t.Errorf("create %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d orders reserved one coupon", wins)
	}
}

// The full checkout on trade_db through the handlers: coupons are listed,
// reserved, spent and restored across a restart (a second store on the same
// database).
func TestSQLCheckoutWithCoupons(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-catalog"), sqltest.Services("svc-payment"), sqltest.Services("svc-order"))
	newStore := func() (*orderStore, *fakePayer) {
		repo, err := newSQLRepo(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		s := newOrderStoreWith(repo)
		mem, _, p := newTestStore()
		s.quoter, s.payer = mem.quoter, mem.payer
		s.machine.Now = func() time.Time { return couponClock }
		return s, p
	}
	s, p := newStore()
	issue(t, s.repo.Coupons(), testVoucher("SQLV", 100123, "50", couponClock.Add(time.Hour)))
	w := createWithCoupons(t, s, 5, "sql-cpn", "SQLV")
	d := dataOf(t, w)
	if w.Code != 200 || d["couponDeduction"] != "50" || d["payableAmount"] != "58" {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	id := orderIDOf(t, w)

	restarted, _ := newStore()
	restarted.payer = p
	_, rows := doCouponList(t, restarted, "status=locked")
	if len(rows) != 1 || rows[0]["couponId"] != "SQLV" {
		t.Fatalf("reservation lost across restart: %v", rows)
	}
	if w := doPay(t, restarted, id, ``); w.Code != 200 {
		t.Fatalf("pay: %d %s", w.Code, w.Body.String())
	}
	if p.debits[orderPayKey(mustAtoi(t, id))] != 5800 {
		t.Fatalf("debit = %v", p.debits)
	}
	if w := doFulfilment(t, restarted, id, failedReport); w.Code != 200 {
		t.Fatalf("fulfilment: %d %s", w.Code, w.Body.String())
	}
	_, rows = doCouponList(t, s, "status=usable")
	if len(rows) != 1 || rows[0]["remainValue"] != "50" {
		t.Fatalf("coupon not restored: %v", rows)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/orders/"+id, nil)
	r.SetPathValue("id", id)
	r.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleDetail(rec, r)
	od := dataOf(t, rec)
	if ids, _ := od["couponIds"].([]any); len(ids) != 1 || od["listAmount"] != "108" || od["state"] != "REFUNDED" {
		t.Fatalf("order detail = %v", od)
	}
}

// --- trial state on trade_db: shared by replicas ------------------------------------

func sqlTrialReplicas(t *testing.T, n int) ([]*trialStore, *orderStore) {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-catalog"), sqltest.Services("svc-payment"), sqltest.Services("svc-order"))
	out := make([]*trialStore, n)
	for i := range out {
		repo, err := newSQLTrialRepo(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		s := newTrialStoreWith(repo, &sqlCouponRepo{db: db})
		for acct, key := range map[int64]string{100200: "idn-8f3a1c2d", 100300: "idn-3", 100400: "idn-4", 100500: "idn-5", 100600: "idn-6"} {
			s.devRealNames[acct] = realNameStatus{Verified: true, IdentityKey: key}
		}
		out[i] = s
	}
	orders, err := newSQLRepo(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	os := newOrderStoreWith(orders)
	mem, _, _ := newTestStore()
	os.quoter, os.payer = mem.quoter, mem.payer
	return out, os
}

func claimRule(t *testing.T, s *trialStore, acct string) string {
	t.Helper()
	code, _, _ := trialEnvelope(t, doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", acct))
	return code
}

func setTrialBudget(t *testing.T, s *orderStore, max int) {
	t.Helper()
	db := s.repo.(*sqlOrderRepo).db
	if _, err := db.Exec(`UPDATE t_trial_activity SET max_active_global = ? WHERE activity_id = ?`, max, trialActivityID); err != nil {
		t.Fatal(err)
	}
}

func TestSQLTrialStateSharedAcrossReplicas(t *testing.T) {
	stores, orders := sqlTrialReplicas(t, 2)
	a, b := stores[0], stores[1]
	t0 := couponClock
	for _, s := range stores {
		s.now = func() time.Time { return t0 }
	}
	if code := claimRule(t, a, "100123"); code != "OK" {
		t.Fatalf("first claim: %s", code)
	}
	// The other replica sees the claim, the identity and the budget.
	if code := claimRule(t, b, "100123"); code != "TRIAL.ACTIVE_LIMIT" {
		t.Fatalf("re-claim on replica b: %s", code)
	}
	if code := claimRule(t, b, "100200"); code != "TRIAL.IDENTITY_LIMIT" {
		t.Fatalf("same identity on replica b: %s", code)
	}
	setTrialBudget(t, orders, 2)
	if code := claimRule(t, b, "100300"); code != "OK" {
		t.Fatalf("second identity: %s", code)
	}
	if code := claimRule(t, a, "100400"); code != "TRIAL.GLOBAL_BUDGET" {
		t.Fatalf("budget exhausted on replica a: %s", code)
	}

	// The voucher is a real t_coupon row the order plane spends.
	vs := trialCoupons(t, b, 100123)
	if len(vs) != 1 || vs[0].FaceValue != pricing.MustParseAmount("100") ||
		!vs[0].ExpireAt.Equal(t0.Add(30*24*time.Hour)) || vs[0].PromoID != trialActivityID {
		t.Fatalf("trial voucher = %+v", vs)
	}
	orders.machine.Now = func() time.Time { return t0 }
	w := createWithCoupons(t, orders, 1, "sql-trial", vs[0].CouponID)
	if d := dataOf(t, w); w.Code != 200 || d["payableAmount"] != "0" {
		t.Fatalf("order with trial voucher: %d %s", w.Code, w.Body.String())
	}

	// Validity ends: the slots come back on every replica; the account stays
	// at its lifetime limit.
	later := t0.Add(30*24*time.Hour + time.Second)
	for _, s := range stores {
		s.now = func() time.Time { return later }
	}
	_, d, _ := trialEnvelope(t, doTrial(t, a, http.MethodGet, "/api/v1/trial/status", "100400"))
	if d["eligible"] != true {
		t.Fatalf("budget not released after expiry: %v", d)
	}
	if code := claimRule(t, b, "100400"); code != "OK" {
		t.Fatalf("claim after release: %s", code)
	}
	if code := claimRule(t, a, "100123"); code != "TRIAL.LIFETIME_LIMIT" {
		t.Fatalf("lapsed account: %s", code)
	}
	var active int
	if err := orders.repo.(*sqlOrderRepo).db.QueryRow(
		`SELECT active_count FROM t_trial_activity WHERE activity_id = ?`, trialActivityID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active_count = %d (%v), want 1", active, err)
	}
}

func TestSQLTrialConcurrentClaimsRespectBudget(t *testing.T) {
	stores, orders := sqlTrialReplicas(t, 2)
	setTrialBudget(t, orders, 1)
	accts := []string{"100300", "100400", "100500", "100600"}
	codes := make([]string, len(accts))
	var wg sync.WaitGroup
	for i, acct := range accts {
		wg.Add(1)
		go func(i int, acct string) {
			defer wg.Done()
			s := stores[i%2]
			w := doTrial(t, s, http.MethodPost, "/api/v1/trial/claim", acct)
			var env struct{ Code string }
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			codes[i] = env.Code
		}(i, acct)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		switch c {
		case "OK":
			ok++
		case "TRIAL.GLOBAL_BUDGET":
		default:
			t.Fatalf("unexpected verdict %q (all %v)", c, codes)
		}
	}
	if ok != 1 {
		t.Fatalf("%d claims admitted against a budget of 1: %v", ok, codes)
	}
}
