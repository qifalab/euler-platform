package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/billing"
	"github.com/qifalab/euler-platform/ledger"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/reservepack"
)

func amt(s string) pricing.Amount { return pricing.MustParseAmount(s) }

type settleResp struct {
	Charge struct {
		PayAmount  string
		Reconciles bool
	}
	Deductions []struct {
		Source string
		Ref    string
		Amount string
	}
	Shortfall string
	InArrears bool
}

func settle(t *testing.T, a *app, acct int64, aggID string) (*httptest.ResponseRecorder, settleResp) {
	t.Helper()
	rr := doReq(t, a.handleSettle, http.MethodPost, "/internal/settle", acct, settleBody(aggID))
	var out settleResp
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rr, out
}

func deductionsOf(r settleResp) map[string]string {
	out := map[string]string{}
	for _, d := range r.Deductions {
		out[d.Source+"|"+d.Ref] = d.Amount
	}
	return out
}

func available(t *testing.T, a *app, acct int64) pricing.Amount {
	t.Helper()
	bal, err := a.ledger.Balance(acct)
	if err != nil {
		t.Fatal(err)
	}
	return bal.Available
}

func recharge(t *testing.T, a *app, acct int64, amount string) {
	t.Helper()
	if _, _, err := a.ledger.Recharge(acct, amt(amount), "seed", "seed-"+amount, "seed"); err != nil {
		t.Fatal(err)
	}
}

// T3: settle charged cash only through a static BALANCE pool that nothing but
// tests seeded, so production settlements never debited the ledger.
func TestSettleDebitsCashFromLedger(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(5001)
	recharge(t, a, acct, "100")
	rr, res := settle(t, a, acct, "agg-cash")
	if rr.Code != http.StatusOK {
		t.Fatalf("settle: %d %s", rr.Code, rr.Body.String())
	}
	if res.Charge.PayAmount != "0.5" || deductionsOf(res)["BALANCE|"] != "0.5" || !res.Charge.Reconciles {
		t.Fatalf("settlement = %+v, want 0.5 paid from cash", res)
	}
	if got := available(t, a, acct); got != amt("99.5") {
		t.Fatalf("ledger available = %s, want 99.5", got)
	}
	e, found, err := a.ledger.FindEntry(acct, billing.ChargeID("agg-cash"))
	if err != nil || !found || e.Type != ledger.EntryConsume || e.Amount != amt("0.5") {
		t.Fatalf("ledger entry under the charge id = %+v %v %v", e, found, err)
	}
}

func TestSettleShortfallWhenCashRunsOut(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(5002)
	recharge(t, a, acct, "0.2")
	// A static cash pool is not money: it must not cover anything.
	a.pools.set(acct, []billing.Pool{{Source: billing.SourceBalance, Available: amt("100")}})
	rr, res := settle(t, a, acct, "agg-short")
	if rr.Code != http.StatusOK {
		t.Fatalf("settle: %d %s", rr.Code, rr.Body.String())
	}
	if res.Charge.PayAmount != "0.2" || res.Shortfall != "0.3" || !res.InArrears || !res.Charge.Reconciles {
		t.Fatalf("settlement = %+v, want 0.2 cash + 0.3 arrears", res)
	}
	if got := available(t, a, acct); !got.IsZero() {
		t.Fatalf("ledger available = %s, want 0", got)
	}
}

// stalePackStore reports packs as the waterfall saw them a moment ago: more
// quota than they hold (inflate), or a pack that has since gone terminal.
type stalePackStore struct {
	reservepack.Store
	inflate pricing.Amount
	ghosts  []reservepack.Pack
}

func (s *stalePackStore) ListActiveByAccount(acct int64, t time.Time) ([]reservepack.Pack, error) {
	packs, err := s.Store.ListActiveByAccount(acct, t)
	if err != nil {
		return nil, err
	}
	for i := range packs {
		packs[i].Remaining = packs[i].Remaining.Add(s.inflate)
	}
	return append(packs, s.ghosts...), nil
}

func withPackStore(a *app, store reservepack.Store) {
	a.packStore = store
	a.packs = reservepack.New(store, time.Now, guardedCounter())
}

// T6: the pack consume's shortfall was discarded and its terminal/quota errors
// swallowed, so the bill recorded pack coverage that was never consumed (and
// nobody paid that share).
func TestSettleMovesPackShortfallToCash(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(5003)
	recharge(t, a, acct, "100")
	stale := &stalePackStore{Store: reservepack.NewMemoryStore(), inflate: amt("1")}
	withPackStore(a, stale)
	if _, _, err := a.packs.Purchase("pk-small", acct, "euecs", "sku", amt("0.2"), time.Time{}, "seed"); err != nil {
		t.Fatal(err)
	}
	rr, res := settle(t, a, acct, "agg-pack-short")
	if rr.Code != http.StatusOK {
		t.Fatalf("settle: %d %s", rr.Code, rr.Body.String())
	}
	d := deductionsOf(res)
	if d["RESOURCE_PACK|pk-small"] != "0.2" || d["BALANCE|"] != "0.3" || res.Charge.PayAmount != "0.3" || !res.Charge.Reconciles {
		t.Fatalf("deductions = %v (pay %s), want pack 0.2 + cash 0.3", d, res.Charge.PayAmount)
	}
	if got := available(t, a, acct); got != amt("99.7") {
		t.Fatalf("ledger available = %s, want 99.7", got)
	}
}

func TestSettleTerminalPackCoversNothing(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(5004)
	recharge(t, a, acct, "100")
	stale := &stalePackStore{Store: reservepack.NewMemoryStore()}
	withPackStore(a, stale)
	if _, _, err := a.packs.Purchase("pk-gone", acct, "euecs", "sku", amt("0.2"), time.Time{}, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.packs.Consume("pk-gone", "euecs", amt("0.2"), "drain", "drain"); err != nil {
		t.Fatal(err)
	}
	gone, _, _ := stale.GetPack("pk-gone")
	gone.Status, gone.Remaining = reservepack.StatusActive, amt("5") // the stale view
	stale.ghosts = []reservepack.Pack{gone}

	rr, res := settle(t, a, acct, "agg-pack-gone")
	if rr.Code != http.StatusOK {
		t.Fatalf("settle: %d %s", rr.Code, rr.Body.String())
	}
	if d := deductionsOf(res); d["RESOURCE_PACK|pk-gone"] != "" || d["BALANCE|"] != "0.5" {
		t.Fatalf("deductions = %v, want no pack coverage and 0.5 cash", d)
	}
	if got := available(t, a, acct); got != amt("99.5") {
		t.Fatalf("ledger available = %s, want 99.5", got)
	}
}

// flakyLedgerStore fails the next N CONSUME writes, like a trade_db outage
// between the pool commits and the cash debit.
type flakyLedgerStore struct {
	ledger.Store
	failConsumes int
}

func (s *flakyLedgerStore) Apply(e ledger.Entry, b ledger.Balance, v int) error {
	if s.failConsumes > 0 && e.Type == ledger.EntryConsume {
		s.failConsumes--
		return errors.New("simulated trade_db outage")
	}
	return s.Store.Apply(e, b, v)
}

// T7: registry pools were committed before the pack consume and the cash debit.
// A failure after that left the coupon spent with the charge unrecorded, and
// the retry billed the coupon's (and the exhausted pack's) share to cash again.
func TestSettleRetryAfterFailureChargesOnce(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(5005)
	flaky := &flakyLedgerStore{Store: a.store}
	a.store = flaky
	a.ledger = ledger.New(flaky, time.Now, guardedCounter())
	recharge(t, a, acct, "100")
	if _, _, err := a.packs.Purchase("pk-exact", acct, "euecs", "sku", amt("0.3"), time.Time{}, "seed"); err != nil {
		t.Fatal(err)
	}
	a.pools.set(acct, []billing.Pool{{Source: billing.SourceCoupon, Ref: "cp-1", Available: amt("0.1")}})

	flaky.failConsumes = 1
	if rr, _ := settle(t, a, acct, "agg-retry"); rr.Code != http.StatusInternalServerError {
		t.Fatalf("first attempt: %d %s, want 500", rr.Code, rr.Body.String())
	}
	if got := a.pools.forAccount(acct, nil)[0].Available; got != amt("0.1") {
		t.Fatalf("failed attempt spent the coupon: %s left", got)
	}
	if got := available(t, a, acct); got != amt("100") {
		t.Fatalf("failed attempt moved cash: %s", got)
	}

	rr, res := settle(t, a, acct, "agg-retry")
	if rr.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", rr.Code, rr.Body.String())
	}
	d := deductionsOf(res)
	if d["RESOURCE_PACK|pk-exact"] != "0.3" || d["COUPON|cp-1"] != "0.1" || d["BALANCE|"] != "0.1" {
		t.Fatalf("retry deductions = %v, want pack 0.3 + coupon 0.1 + cash 0.1", d)
	}
	if got := available(t, a, acct); got != amt("99.9") {
		t.Fatalf("ledger available = %s, want 99.9 (cash charged once, for its share only)", got)
	}
	if got := a.pools.forAccount(acct, nil)[0].Available; !got.IsZero() {
		t.Fatalf("coupon left %s, want 0", got)
	}
	entries, _ := a.packStore.ListEntries("pk-exact")
	if len(entries) != 2 { // purchase + one consume
		t.Fatalf("pack journal has %d entries, want 2", len(entries))
	}
}

// commit used to skip Ref == "" deductions, so an unnamed registry pool was
// spent by the waterfall but never shrank.
func TestPoolCommitShrinksUnnamedPools(t *testing.T) {
	p := newPoolRegistry()
	p.set(7, []billing.Pool{{Source: billing.SourceCoupon, Available: amt("1")}})
	p.commit(7, nil, []billing.Deduction{{Source: billing.SourceCoupon, Amount: amt("0.4")}})
	if got := p.forAccount(7, nil)[0].Available; got != amt("0.6") {
		t.Fatalf("unnamed coupon pool = %s, want 0.6", got)
	}
}

func buyPack(t *testing.T, a *app, acct int64, packID, face, orderKey string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, a.handleReservePacks, http.MethodPost, "/internal/reservepacks", acct, map[string]any{
		"packId": packID, "productCode": "euecs", "skuCode": "euecs.pack", "faceValue": face, "orderKey": orderKey,
	})
}

// T1: the pack debit was keyed "pack:"+orderKey and a duplicate was treated as
// paid, so pack B bought under pack A's order key was free.
func TestPackPurchaseKeyBindsPackID(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(6001)
	recharge(t, a, acct, "2")
	if rr := buyPack(t, a, acct, "pk-a", "1", "o-1"); rr.Code != http.StatusOK {
		t.Fatalf("pack A: %d %s", rr.Code, rr.Body.String())
	}
	if rr := buyPack(t, a, acct, "pk-b", "1000000", "o-1"); rr.Code != http.StatusConflict ||
		!strings.Contains(rr.Body.String(), "Billing.InsufficientBalance") {
		t.Fatalf("pack B under A's order key: %d %s, want 409 insufficient balance", rr.Code, rr.Body.String())
	}
	if _, ok, _ := a.packStore.GetPack("pk-b"); ok {
		t.Fatal("pack B was credited without being paid for")
	}
	if got := available(t, a, acct); got != amt("1") {
		t.Fatalf("balance = %s, want 1", got)
	}
}

// failingPackStore fails the next purchase write.
type failingPackStore struct {
	reservepack.Store
	failPurchases int
}

func (s *failingPackStore) Apply(e reservepack.Entry, p reservepack.Pack, v int) error {
	if s.failPurchases > 0 && e.Type == reservepack.EntryPurchase {
		s.failPurchases--
		return errors.New("simulated pack store outage")
	}
	return s.Store.Apply(e, p, v)
}

// T1: a failed purchase refunds exactly what THIS purchase debited, once; and a
// replay of a reversed purchase must not mint the pack for free.
func TestPackPurchaseCompensationRefundsOnlyTheDebit(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(6002)
	recharge(t, a, acct, "100")
	fail := &failingPackStore{Store: reservepack.NewMemoryStore(), failPurchases: 1}
	withPackStore(a, fail)

	if rr := buyPack(t, a, acct, "pk-1", "50", "o-1"); rr.Code != http.StatusBadRequest {
		t.Fatalf("failing purchase: %d %s, want 400", rr.Code, rr.Body.String())
	}
	if got := available(t, a, acct); got != amt("100") {
		t.Fatalf("balance = %s after a reversed purchase, want 100 (refund == debit)", got)
	}
	if rr := buyPack(t, a, acct, "pk-1", "50", "o-1"); rr.Code != http.StatusConflict {
		t.Fatalf("replay of a reversed purchase: %d %s, want 409", rr.Code, rr.Body.String())
	}
	if _, ok, _ := a.packStore.GetPack("pk-1"); ok {
		t.Fatal("a reversed purchase was credited on replay (free pack)")
	}
	if got := available(t, a, acct); got != amt("100") {
		t.Fatalf("balance = %s, want 100", got)
	}
	// A fresh order key buys it properly.
	if rr := buyPack(t, a, acct, "pk-1", "50", "o-2"); rr.Code != http.StatusOK {
		t.Fatalf("new order: %d %s", rr.Code, rr.Body.String())
	}
	if got := available(t, a, acct); got != amt("50") {
		t.Fatalf("balance = %s, want 50", got)
	}
}

func TestPackPurchaseRejectsForeignPack(t *testing.T) {
	a := newInMemoryApp()
	recharge(t, a, 6003, "100")
	recharge(t, a, 6004, "100")
	if rr := buyPack(t, a, 6003, "pk-x", "10", "o-1"); rr.Code != http.StatusOK {
		t.Fatalf("owner: %d %s", rr.Code, rr.Body.String())
	}
	rr := buyPack(t, a, 6004, "pk-x", "10", "o-1")
	if rr.Code != http.StatusConflict || strings.Contains(rr.Body.String(), "6003") {
		t.Fatalf("foreign pack: %d %s, want 409 without the owner's pack", rr.Code, rr.Body.String())
	}
	if got := available(t, a, 6004); got != amt("100") {
		t.Fatalf("intruder charged: %s", got)
	}
}

// T1/T2: an entry pre-planted under the purchase's key with another amount is
// a conflict, never proof of payment.
func TestPackPurchasePrePlantedKeyIsConflict(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(6005)
	recharge(t, a, acct, "100")
	if _, _, err := a.ledger.Consume(acct, amt("0.01"), "pack", "o-1", packLedgerKey("pack:", "pk-1", "o-1"), ""); err != nil {
		t.Fatal(err)
	}
	if rr := buyPack(t, a, acct, "pk-1", "50", "o-1"); rr.Code != http.StatusConflict ||
		!strings.Contains(rr.Body.String(), "Billing.IdempotencyConflict") {
		t.Fatalf("purchase over a planted key: %d %s, want 409 Billing.IdempotencyConflict", rr.Code, rr.Body.String())
	}
	if _, ok, _ := a.packStore.GetPack("pk-1"); ok {
		t.Fatal("pack credited against a 1-分 entry")
	}
}

func TestPackLedgerKeyFitsTheJournal(t *testing.T) {
	if k := packLedgerKey("pack:", "pk-1", "o-1"); k != "pack:pk-1:o-1" {
		t.Fatalf("short key = %q", k)
	}
	long := strings.Repeat("p", 64)
	k1 := packLedgerKey("pack-undo:", long, strings.Repeat("o", 64))
	k2 := packLedgerKey("pack-undo:", long, strings.Repeat("o", 63))
	if len(k1) > ledger.MaxKeyLen || len(k2) > ledger.MaxKeyLen || k1 == k2 || strings.Contains(k1[len("pack-undo:"):], ":") {
		t.Fatalf("long keys %q / %q", k1, k2)
	}
}

func TestSettleRejectsMissingOrOversizedAggID(t *testing.T) {
	a := newInMemoryApp()
	for _, agg := range []string{"", strings.Repeat("a", 61)} {
		if rr := doReq(t, a.handleSettle, http.MethodPost, "/internal/settle", 1, settleBody(agg)); rr.Code != http.StatusBadRequest {
			t.Fatalf("AggId %q: %d %s, want 400", agg, rr.Code, rr.Body.String())
		}
	}
}
