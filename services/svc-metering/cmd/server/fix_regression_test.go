package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/billing"
	"github.com/qifalab/euler-platform/metering"
	"github.com/qifalab/euler-platform/pricing"
)

// memAggRepo is an in-memory aggRepo: it puts a store in PERSISTENT mode (the
// branch main() takes when EULER_DB_DSN is set) without needing MySQL.
type memAggRepo struct {
	rows map[string]metering.HourlyUsage
}

func (m *memAggRepo) UpsertHourly(_ context.Context, u metering.HourlyUsage) error {
	m.rows[u.AggID] = u
	return nil
}

func (m *memAggRepo) LoadAll(context.Context) ([]metering.HourlyUsage, error) {
	out := make([]metering.HourlyUsage, 0, len(m.rows))
	for _, u := range m.rows {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HourStart.Before(out[j].HourStart) })
	return out, nil
}

func fullHourUsage(resourceID, item string, hour time.Time) metering.HourlyUsage {
	return metering.HourlyUsage{
		AggID: metering.AggID(resourceID, item, hour), AccountID: seededAccountID,
		Region: "cn-north-1", ResourceType: "ecs", ResourceID: resourceID,
		MeteringItem: item, TotalQuantity: metering.MustParseQuantity("2"),
		HourStart: hour, CoveredRatio: 100, WindowsSeen: 60, WindowsExpected: 60,
		BatchID: metering.BatchRealtime,
	}
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func getBill(t *testing.T, s *meteringStore, acct int64, period string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/metering/bills?period="+period, nil)
	req.Header.Set(accountIDHeader, fmt.Sprintf("%d", acct))
	rr := httptest.NewRecorder()
	s.handleBills(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bills: %d %s", rr.Code, rr.Body.String())
	}
	var env struct{ Data map[string]any }
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return env.Data
}

// E1: main() builds its store with newMeteringStoreWith, which never wired the
// owner verifier — the first reading of any new resource called a nil
// interface. Ingest through the constructor main() uses.
func TestMainConstructorIngestsNewResource(t *testing.T) {
	for _, repo := range []aggRepo{nil, &memAggRepo{rows: map[string]metering.HourlyUsage{}}} {
		s, err := newMeteringStoreWith(repo)
		if err != nil {
			t.Fatal(err)
		}
		if s.owners == nil {
			t.Fatalf("persistent=%v: owner verifier not wired", repo != nil)
		}
		if rr := ingest(t, s, 7, "euecs-brand-new", "cpu_core_hour", "1"); rr.Code != http.StatusOK {
			t.Fatalf("persistent=%v: first ingest of a new resource: %d %s", repo != nil, rr.Code, rr.Body.String())
		}
	}
}

// E2: ingest handed the aggregator every record of the resource; from the
// second hour on Aggregate failed with ErrWindowOutsideHour (ignored), so
// nothing after the first hour was ever aggregated or billed.
func TestIngestAggregatesEveryHour(t *testing.T) {
	s := newMeteringStore()
	h1 := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	h2 := h1.Add(time.Hour)
	for _, at := range []time.Time{h1.Add(5 * time.Minute), h1.Add(6 * time.Minute), h2.Add(5 * time.Minute)} {
		s.now = fixedClock(at)
		if rr := ingest(t, s, 7, "res-2h", "cpu_core_hour", "1"); rr.Code != http.StatusOK {
			t.Fatalf("ingest at %v: %d %s", at, rr.Code, rr.Body.String())
		}
	}
	u1, ok1 := s.aggCache[metering.AggID("res-2h", "cpu_core_hour", h1)]
	u2, ok2 := s.aggCache[metering.AggID("res-2h", "cpu_core_hour", h2)]
	if !ok1 || !ok2 {
		t.Fatalf("aggregates cached: hour1=%v hour2=%v, want both", ok1, ok2)
	}
	if u1.TotalQuantity.String() != "2" || u2.TotalQuantity.String() != "1" {
		t.Fatalf("hour totals %s / %s, want 2 / 1", u1.TotalQuantity, u2.TotalQuantity)
	}
	// The demo seed had the same defect: only the first of its three hours
	// was ever aggregated.
	seeded := 0
	for _, u := range newMeteringStore().aggCache {
		if u.ResourceID == seededResourceID {
			seeded++
		}
	}
	if seeded != 3 {
		t.Fatalf("seeded aggregates = %d, want 3", seeded)
	}
}

// E3: the price snapshot and pools were loaded only by seed(), which only the
// in-memory mode runs — a persistent store priced nothing and reported the
// empty bill as final. Unpriced usage must also keep the bill from being final.
func TestPersistentModeBillsAndUnpricedUsageIsUnreconciled(t *testing.T) {
	hour := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	repo := &memAggRepo{rows: map[string]metering.HourlyUsage{}}
	priced := fullHourUsage("euecs-p", "cpu_core_hour", hour)
	repo.rows[priced.AggID] = priced
	s, err := newMeteringStoreWith(repo)
	if err != nil {
		t.Fatal(err)
	}
	s.now = fixedClock(hour.Add(2 * time.Hour))
	bill := getBill(t, s, seededAccountID, "2026-09")
	if bill["chargeCount"] != float64(1) || bill["totalAmount"] != "0.5" || bill["final"] != true {
		t.Fatalf("persistent-mode bill = %v, want one 0.5 charge, final", bill)
	}

	unpriced := fullHourUsage("euecs-p", "gpu_card_hour", hour)
	repo.rows[unpriced.AggID] = unpriced
	s, err = newMeteringStoreWith(repo)
	if err != nil {
		t.Fatal(err)
	}
	s.now = fixedClock(hour.Add(2 * time.Hour))
	bill = getBill(t, s, seededAccountID, "2026-09")
	if bill["unreconciledCount"] != float64(1) || bill["unsettledCount"] != float64(1) || bill["final"] != false {
		t.Fatalf("bill with unpriced usage = %v, want 1 unreconciled and not final", bill)
	}
}

// E5: /bills re-settled through the backfill freeze, so every closed month
// read back as an empty bill.
func TestBillsForPastPeriod(t *testing.T) {
	s := newMeteringStore()
	s.now = fixedClock(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)) // August is frozen
	bill := getBill(t, s, seededAccountID, "2026-08")
	if bill["chargeCount"] != float64(3) {
		t.Fatalf("past-period bill = %v, want the 3 seeded charges", bill)
	}
	if bill["final"] != true {
		t.Fatalf("past-period bill not final: %v", bill)
	}
}

// E6: every charge settled against the same untouched pools — the paid amount
// could exceed the balance and arrears were never visible.
func TestBillsSpendPoolsInHourOrder(t *testing.T) {
	s := newMeteringStore()
	s.now = fixedClock(time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC))
	// Each seeded hour costs 1.99998 core-h × 0.25 = 0.499995.
	s.pools[seededAccountID] = []billing.Pool{
		{Source: billing.SourceBalance, Available: pricing.MustParseAmount("0.6")},
	}
	bill := getBill(t, s, seededAccountID, "2026-08")
	if bill["paidAmount"] != "0.6" {
		t.Fatalf("paidAmount = %v, want the whole 0.6 balance and no more", bill["paidAmount"])
	}
	if bill["shortfallAmount"] != "0.899985" || bill["inArrears"] != true {
		t.Fatalf("arrears not reported: shortfall=%v inArrears=%v", bill["shortfallAmount"], bill["inArrears"])
	}
	if bill["final"] != true {
		t.Fatalf("an explained shortfall still reconciles: %v", bill)
	}
	lines := bill["lines"].([]any)
	want := []string{"0", "0.39999", "0.499995"} // oldest hour paid first
	for i, l := range lines {
		if got := l.(map[string]any)["shortfall"]; got != want[i] {
			t.Fatalf("line %d shortfall = %v, want %s (lines must settle oldest first)", i, got, want[i])
		}
	}
	// The configured pool itself is untouched: the read path spends a copy.
	if s.pools[seededAccountID][0].Available != pricing.MustParseAmount("0.6") {
		t.Fatal("/bills mutated the account's pool config")
	}
}

// E8: the anomaly baseline was built from raw micro-unit quantities and
// counted a retransmitted window twice.
func TestAnomalyScanUnitsAndRetransmits(t *testing.T) {
	s := newMeteringStore()
	base := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	ing := func(minute int, v string) {
		t.Helper()
		s.now = fixedClock(base.Add(time.Duration(minute) * time.Minute))
		if rr := ingest(t, s, 7, "res-an", "cpu_core_hour", v); rr.Code != http.StatusOK {
			t.Fatalf("ingest: %d %s", rr.Code, rr.Body.String())
		}
	}
	ing(0, "1")
	ing(1, "1")
	ing(2, "1")
	ing(3, "5")
	ing(3, "5") // retransmit of the latest window
	req := httptest.NewRequest(http.MethodGet, "/api/v1/metering/anomaly-scan?resourceId=res-an&metric=cpu_core_hour", nil)
	req.Header.Set(accountIDHeader, "7")
	rr := httptest.NewRecorder()
	s.handleAnomalyScan(rr, req)
	var env struct{ Data map[string]any }
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data["baselineMean"] != float64(1) {
		t.Fatalf("baselineMean = %v, want 1 (metric units, retransmit counted once)", env.Data["baselineMean"])
	}
}

// E9: period=hour returned 25 hours ([ref-24h, ref]).
func TestInPeriodHourIs24Hours(t *testing.T) {
	ref := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	n := 0
	for h := ref.Add(-48 * time.Hour); !h.After(ref); h = h.Add(time.Hour) {
		if inPeriod(h, "hour", ref) {
			n++
		}
	}
	if n != 24 {
		t.Fatalf("period=hour covers %d hours, want 24", n)
	}
	if inPeriod(ref.Add(-24*time.Hour), "hour", ref) || !inPeriod(ref, "hour", ref) {
		t.Fatal("window must be (ref-24h, ref]")
	}
}
