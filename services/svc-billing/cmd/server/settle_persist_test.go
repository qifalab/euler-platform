package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/billing"
	"github.com/qifalab/euler-platform/ledger"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/reservepack"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

// sqlDB builds apps on one database: each one is a replica, or the same
// process after a restart.
type sqlDB struct {
	t  *testing.T
	db *sql.DB
}

// newSQLTestDB is trade_db as the migrator builds it: svc-payment owns the cash
// ledger and the id allocator, svc-billing the packs and the settle records.
func newSQLTestDB(t *testing.T) *sqlDB {
	t.Helper()
	return &sqlDB{t: t, db: sqltest.Open(t, sqltest.Services("svc-payment"), sqltest.Services("svc-billing"))}
}

func (d *sqlDB) app() *app {
	d.t.Helper()
	a, err := newSQLApp(context.Background(), d.db)
	if err != nil {
		d.t.Fatal(err)
	}
	return a
}

func settleRaw(t *testing.T, a *app, acct int64, aggID string) (int, string) {
	t.Helper()
	rr := doReq(t, a.handleSettle, http.MethodPost, "/internal/settle", acct, settleBody(aggID))
	return rr.Code, rr.Body.String()
}

func packConsumes(t *testing.T, a *app, packID string) int {
	t.Helper()
	entries, err := a.packStore.ListEntries(packID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Type == reservepack.EntryConsume {
			n++
		}
	}
	return n
}

func billOf(t *testing.T, a *app, acct int64) map[string]any {
	t.Helper()
	period := time.Now().UTC().Format("2006-01")
	rr := doReq(t, a.handleBills, http.MethodGet, "/internal/bills?period="+period, acct, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bills: %d %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A settled charge replays byte-for-byte, in memory and from the database.
func TestSettleReplayIsIdentical(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T) *app{
		"memory": func(*testing.T) *app { return newInMemoryApp() },
		"mysql":  func(t *testing.T) *app { return newSQLTestDB(t).app() },
	} {
		t.Run(name, func(t *testing.T) {
			a := mk(t)
			const acct = int64(6001)
			recharge(t, a, acct, "100")
			code, first := settleRaw(t, a, acct, "agg-replay")
			if code != http.StatusOK {
				t.Fatalf("settle: %d %s", code, first)
			}
			code, again := settleRaw(t, a, acct, "agg-replay")
			if code != http.StatusOK || again != first {
				t.Fatalf("replay = %d %s\nwant 200 %s", code, again, first)
			}
		})
	}
}

// A restart (a new app on the same database) must replay a settled charge,
// not charge it again — and the bill must still contain it.
func TestSQLSettleSurvivesRestart(t *testing.T) {
	d := newSQLTestDB(t)
	a := d.app()
	const acct = int64(6002)
	recharge(t, a, acct, "100")
	if _, _, err := a.packs.Purchase("pk-r", acct, "euecs", "sku", amt("0.3"), time.Time{}, "seed"); err != nil {
		t.Fatal(err)
	}
	code, first := settleRaw(t, a, acct, "agg-restart")
	if code != http.StatusOK {
		t.Fatalf("settle: %d %s", code, first)
	}

	b := d.app() // restarted process
	code, again := settleRaw(t, b, acct, "agg-restart")
	if code != http.StatusOK || again != first {
		t.Fatalf("replay after restart = %d %s\nwant 200 %s", code, again, first)
	}
	if got := available(t, b, acct); got != amt("99.8") {
		t.Fatalf("available = %s, want 99.8 (pack 0.3 + cash 0.2, charged once)", got)
	}
	if n := packConsumes(t, b, "pk-r"); n != 1 {
		t.Fatalf("pack consumed %d times, want 1", n)
	}
	bill := billOf(t, b, acct)
	if bill["ChargeCount"] != float64(1) || bill["TotalAmount"] != "0.5" || bill["PaidAmount"] != "0.2" {
		t.Fatalf("bill after restart = %v, want the one charge", bill)
	}
	rr := doReq(t, b.handleCostAnalysis, http.MethodGet, "/internal/cost-analysis?period="+time.Now().UTC().Format("2006-01"), acct, nil)
	var cost map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &cost)
	if rr.Code != http.StatusOK || cost["total"] != "0.5" {
		t.Fatalf("cost analysis after restart = %d %s", rr.Code, rr.Body.String())
	}
}

// The first attempt exhausts the pack and then dies before the cash debit. A
// recomputed waterfall would no longer see the pack (it is EXHAUSTED) and bill
// its share to cash; the persisted plan pins it.
func TestSQLSettleResumesPersistedPlanAfterRestart(t *testing.T) {
	d := newSQLTestDB(t)
	a := d.app()
	const acct = int64(6003)
	flaky := &flakyLedgerStore{Store: a.store}
	a.store = flaky
	a.ledger = ledger.New(flaky, time.Now, guardedCounter())
	recharge(t, a, acct, "100")
	if _, _, err := a.packs.Purchase("pk-x", acct, "euecs", "sku", amt("0.3"), time.Time{}, "seed"); err != nil {
		t.Fatal(err)
	}
	flaky.failConsumes = 1
	if code, body := settleRaw(t, a, acct, "agg-resume"); code != http.StatusInternalServerError {
		t.Fatalf("first attempt: %d %s, want 500", code, body)
	}
	rec, ok, err := a.settles.GetSettle(acct, billing.ChargeID("agg-resume"))
	if err != nil || !ok || rec.Settled || !rec.PacksDone || rec.PackTaken["pk-x"] != amt("0.3") {
		t.Fatalf("persisted plan = %+v (ok=%v err=%v), want packs done with pk-x 0.3", rec, ok, err)
	}

	b := d.app() // another replica / a restart picks the retry up
	code, body := settleRaw(t, b, acct, "agg-resume")
	if code != http.StatusOK {
		t.Fatalf("resumed settle: %d %s", code, body)
	}
	var res settleResp
	_ = json.Unmarshal([]byte(body), &res)
	if d := deductionsOf(res); d["RESOURCE_PACK|pk-x"] != "0.3" || d["BALANCE|"] != "0.2" {
		t.Fatalf("resumed deductions = %v, want pack 0.3 + cash 0.2", d)
	}
	if got := available(t, b, acct); got != amt("99.8") {
		t.Fatalf("available = %s, want 99.8", got)
	}
	if n := packConsumes(t, b, "pk-x"); n != 1 {
		t.Fatalf("pack consumed %d times, want 1", n)
	}
}

// A crash between a pack consume and the plan write that records it: only the
// intent is stored. The resume replays the consume under the ChargeID (the pack
// ledger returns what it took) instead of recomputing without the pack.
func TestSQLSettleResumesFromPackIntent(t *testing.T) {
	d := newSQLTestDB(t)
	a := d.app()
	const acct = int64(6004)
	recharge(t, a, acct, "100")
	if _, _, err := a.packs.Purchase("pk-i", acct, "euecs", "sku", amt("0.3"), time.Time{}, "seed"); err != nil {
		t.Fatal(err)
	}
	chargeID := billing.ChargeID("agg-intent")
	rec := billing.SettleRecord{AccountID: acct, ChargeID: chargeID,
		PackIntent: map[string]pricing.Amount{"pk-i": amt("0.3")}}
	if err := a.settles.SaveSettle(&rec); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.packs.Consume("pk-i", "euecs", amt("0.3"), chargeID, chargeID); err != nil {
		t.Fatal(err)
	}

	b := d.app()
	code, body := settleRaw(t, b, acct, "agg-intent")
	if code != http.StatusOK {
		t.Fatalf("resumed settle: %d %s", code, body)
	}
	if got := available(t, b, acct); got != amt("99.8") {
		t.Fatalf("available = %s, want 99.8 (the exhausted pack still covers its 0.3)", got)
	}
	if n := packConsumes(t, b, "pk-i"); n != 1 {
		t.Fatalf("pack consumed %d times, want 1", n)
	}
}

// Two replicas settle concurrently: duplicates of one aggregate charge once and
// answer identically, and distinct aggregates of one account all land exactly
// (the account lock serialises them, so no balance race is lost).
func TestSQLSettleConcurrentReplicas(t *testing.T) {
	d := newSQLTestDB(t)
	replicas := []*app{d.app(), d.app()}
	const acct = int64(6005)
	recharge(t, replicas[0], acct, "100")
	if _, _, err := replicas[0].packs.Purchase("pk-c", acct, "euecs", "sku", amt("0.3"), time.Time{}, "seed"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	bodies := make([]string, 8)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, body := settleRaw(t, replicas[i%2], acct, "agg-dup")
			if code != http.StatusOK {
				t.Errorf("duplicate %d: %d %s", i, code, body)
			}
			bodies[i] = body
		}(i)
	}
	wg.Wait()
	for i := range bodies {
		if bodies[i] != bodies[0] {
			t.Fatalf("replica answers differ:\n%s\n%s", bodies[0], bodies[i])
		}
	}
	if got := available(t, replicas[0], acct); got != amt("99.8") {
		t.Fatalf("available after duplicates = %s, want 99.8 (charged once)", got)
	}
	if n := packConsumes(t, replicas[1], "pk-c"); n != 1 {
		t.Fatalf("pack consumed %d times, want 1", n)
	}

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if code, body := settleRaw(t, replicas[i%2], acct, "agg-distinct-"+string(rune('a'+i))); code != http.StatusOK {
				t.Errorf("distinct %d: %d %s", i, code, body)
			}
		}(i)
	}
	wg.Wait()
	if got := available(t, replicas[1], acct); got != amt("96.8") {
		t.Fatalf("available = %s, want 96.8 (six more charges of 0.5)", got)
	}
	if bill := billOf(t, replicas[1], acct); bill["ChargeCount"] != float64(7) || bill["TotalAmount"] != "3.5" {
		t.Fatalf("bill = %v, want 7 charges totalling 3.5", bill)
	}
}
