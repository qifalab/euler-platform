package metering

import (
	"errors"
	"testing"
	"time"
)

var (
	hour = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	now  = time.Date(2026, 8, 8, 13, 15, 0, 0, time.UTC)
)

func newAgg() *Aggregator {
	return NewAggregator(func() time.Time { return now })
}

// record builds a reading for the given minute offset within the hour.
func record(minute int, qty string) UsageRecord {
	ws := hour.Add(time.Duration(minute) * time.Minute)
	return UsageRecord{
		RecordID:      RecordID("scecs-cn-north-1-01-a1b2c3d4", "cpu_core_hour", ws),
		AccountID:     100123,
		Region:        "cn-north-1",
		ResourceType:  "ecs",
		ResourceID:    "scecs-cn-north-1-01-a1b2c3d4",
		MeteringItem:  "cpu_core_hour",
		Quantity:      MustParseQuantity(qty),
		WindowStart:   ws,
		WindowSeconds: 60,
		CollectTS:     ws.Add(time.Second),
		CollectorID:   "agent-01",
		BatchID:       BatchRealtime,
	}
}

func fullHour(qty string) []UsageRecord {
	out := make([]UsageRecord, 0, 60)
	for i := 0; i < 60; i++ {
		out = append(out, record(i, qty))
	}
	return out
}

// --- Quantity ---

func TestQuantityRoundTrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.033333", "0.033333"},
		{"120", "120"},
		{"0", "0"},
		{"1.5", "1.5"},
	}
	for _, c := range cases {
		q, err := ParseQuantity(c.in)
		if err != nil {
			t.Errorf("ParseQuantity(%q): %v", c.in, err)
			continue
		}
		if got := q.String(); got != c.want {
			t.Errorf("%q → %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQuantityNoFloatDrift(t *testing.T) {
	// A CPU-second reading of 0.033333 core-hours accumulated over a full hour
	// must not drift; the total multiplies into money downstream.
	unit := MustParseQuantity("0.033333")
	var total Quantity
	for i := 0; i < 60; i++ {
		total = total.Add(unit)
	}
	if got := total.String(); got != "1.99998" {
		t.Fatalf("60 × 0.033333 = %s, want exactly 1.99998", got)
	}
}

// --- Idempotency keys ---

// TestRecordIDIsDeterministic is the property the whole dedup chain rests on:
// an agent retransmitting the same window computes the same id, so the
// duplicate collapses at Kafka, aggregation, ClickHouse and billing alike.
func TestRecordIDIsDeterministic(t *testing.T) {
	ws := hour
	a := RecordID("res-1", "cpu_core_hour", ws)
	b := RecordID("res-1", "cpu_core_hour", ws)
	if a != b {
		t.Fatalf("same inputs produced different ids: %s vs %s", a, b)
	}
	if len(a) != 24 {
		t.Fatalf("record id length = %d, want 24", len(a))
	}
	// Any input change produces a different id.
	if RecordID("res-2", "cpu_core_hour", ws) == a {
		t.Error("different resource must yield a different id")
	}
	if RecordID("res-1", "mem_gb_hour", ws) == a {
		t.Error("different item must yield a different id")
	}
	if RecordID("res-1", "cpu_core_hour", ws.Add(time.Minute)) == a {
		t.Error("different window must yield a different id")
	}
}

func TestAggIDIsDeterministic(t *testing.T) {
	a := AggID("res-1", "cpu_core_hour", hour)
	if a != AggID("res-1", "cpu_core_hour", hour) {
		t.Fatal("agg id not deterministic")
	}
	if a == AggID("res-1", "cpu_core_hour", hour.Add(time.Hour)) {
		t.Error("different hour must yield a different agg id")
	}
}

// --- Aggregation ---

func TestFullHourAggregates(t *testing.T) {
	a := newAgg()
	got, err := a.Aggregate(fullHour("0.033333"), hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalQuantity.String() != "1.99998" {
		t.Fatalf("total = %s, want 1.99998", got.TotalQuantity)
	}
	if got.CoveredRatio != 100 || !got.Complete() {
		t.Fatalf("covered_ratio = %d, want 100", got.CoveredRatio)
	}
	if got.WindowsSeen != 60 || got.WindowsExpected != 60 {
		t.Fatalf("windows %d/%d, want 60/60", got.WindowsSeen, got.WindowsExpected)
	}
}

// TestDuplicateRecordsCollapse is what makes "collect twice rather than miss
// once" affordable: over-collection is free, under-collection is revenue lost.
func TestDuplicateRecordsCollapse(t *testing.T) {
	a := newAgg()
	records := fullHour("0.033333")
	// The agent retransmits the whole hour after a network failure.
	records = append(records, fullHour("0.033333")...)

	got, err := a.Aggregate(records, hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalQuantity.String() != "1.99998" {
		t.Fatalf("retransmission double-counted: total = %s, want 1.99998", got.TotalQuantity)
	}
	if got.CoveredRatio != 100 {
		t.Fatalf("covered_ratio = %d, want 100", got.CoveredRatio)
	}
}

func TestIncompleteHourReportsCoveredRatio(t *testing.T) {
	a := newAgg()
	// Only 45 of 60 windows arrived.
	partial := fullHour("0.033333")[:45]

	got, err := a.Aggregate(partial, hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.CoveredRatio != 75 {
		t.Fatalf("covered_ratio = %d, want 75", got.CoveredRatio)
	}
	if got.Complete() {
		t.Fatal("75%% coverage must not report complete")
	}

	// An incomplete hour generates a backfill task rather than silently
	// publishing an undercount.
	task, need := NeedsBackfill(got)
	if !need {
		t.Fatal("incomplete hour must trigger backfill")
	}
	if task.MissingCount != 15 {
		t.Fatalf("missing = %d, want 15", task.MissingCount)
	}
}

func TestCompleteHourNeedsNoBackfill(t *testing.T) {
	a := newAgg()
	got, _ := a.Aggregate(fullHour("1"), hour)
	if _, need := NeedsBackfill(got); need {
		t.Fatal("a complete hour must not generate backfill work")
	}
}

func TestEmptyWindowRejected(t *testing.T) {
	a := newAgg()
	if _, err := a.Aggregate(nil, hour); !errors.Is(err, ErrEmptyWindow) {
		t.Fatalf("expected ErrEmptyWindow, got %v", err)
	}
}

func TestMixedResourcesRejected(t *testing.T) {
	// Aggregating across resources would produce a total belonging to nobody.
	a := newAgg()
	records := []UsageRecord{record(0, "1")}
	other := record(1, "1")
	other.ResourceID = "scecs-cn-north-1-01-99999999"
	records = append(records, other)

	if _, err := a.Aggregate(records, hour); !errors.Is(err, ErrMixedResource) {
		t.Fatalf("expected ErrMixedResource, got %v", err)
	}
}

func TestMisalignedWindowRejected(t *testing.T) {
	a := newAgg()
	bad := record(0, "1")
	bad.WindowStart = hour.Add(30 * time.Second)
	if _, err := a.Aggregate([]UsageRecord{bad}, hour); !errors.Is(err, ErrWindowMisaligned) {
		t.Fatalf("expected ErrWindowMisaligned, got %v", err)
	}
}

// TestReAggregationIsStable covers 03§4.2.5: 聚合任务失败可无限重算.
func TestReAggregationIsStable(t *testing.T) {
	a := newAgg()
	records := fullHour("0.033333")

	first, _ := a.Aggregate(records, hour)
	for i := 0; i < 5; i++ {
		again, err := a.Aggregate(records, hour)
		if err != nil {
			t.Fatal(err)
		}
		if again.AggID != first.AggID || again.TotalQuantity != first.TotalQuantity ||
			again.CoveredRatio != first.CoveredRatio {
			t.Fatalf("re-run %d differs: %+v vs %+v", i, again, first)
		}
	}
}

// --- Late data ---

func TestLateDataMergesWithoutDoubleCounting(t *testing.T) {
	a := newAgg()
	// 58 of 60 windows arrived on time.
	onTime := fullHour("1")[:58]
	first, _ := a.Aggregate(onTime, hour)
	if first.CoveredRatio != 96 {
		t.Fatalf("initial covered_ratio = %d, want 96", first.CoveredRatio)
	}

	// The two stragglers arrive after the watermark, plus a duplicate of a
	// window already counted.
	late := []UsageRecord{record(58, "1"), record(59, "1"), record(0, "1")}
	for i := range late {
		late[i].BatchID = BatchLate
	}

	merged, err := a.Merge(first, late, onTime)
	if err != nil {
		t.Fatal(err)
	}
	if merged.CoveredRatio != 100 {
		t.Fatalf("after merge covered_ratio = %d, want 100", merged.CoveredRatio)
	}
	if merged.TotalQuantity.String() != "60" {
		t.Fatalf("merged total = %s, want 60 (the duplicate must not add)", merged.TotalQuantity)
	}
	if merged.AggID != first.AggID {
		t.Fatal("agg id must stay stable across a merge")
	}
	if merged.BatchID != BatchLate {
		t.Fatalf("batch = %s, want late", merged.BatchID)
	}
}

func TestWatermark(t *testing.T) {
	// The hour closes at 13:00; publication waits until 13:10.
	before := NewAggregator(func() time.Time { return hour.Add(time.Hour + 5*time.Minute) })
	if before.WatermarkPassed(hour) {
		t.Error("must still wait for stragglers 5 minutes after the hour")
	}
	after := NewAggregator(func() time.Time { return hour.Add(time.Hour + 11*time.Minute) })
	if !after.WatermarkPassed(hour) {
		t.Error("watermark should have passed 11 minutes after the hour")
	}
}

// --- Monthly freeze ---

func TestFreezeBoundary(t *testing.T) {
	// August usage freezes on 1 September at 06:00 UTC.
	aug := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC)
	if got := FreezeBoundary(aug); !got.Equal(want) {
		t.Fatalf("freeze boundary = %v, want %v", got, want)
	}
}

// TestFrozenPeriodRejectsBackfill guards the reason the boundary exists: a late
// correction that silently changes a bill the customer already paid and
// reconciled is worse than the small undercount it fixes.
func TestFrozenPeriodRejectsBackfill(t *testing.T) {
	augUsage := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)

	duringAugust := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	if Frozen(augUsage, duringAugust) {
		t.Error("the current month must stay open to backfill")
	}
	beforeFreeze := time.Date(2026, 9, 1, 5, 0, 0, 0, time.UTC)
	if Frozen(augUsage, beforeFreeze) {
		t.Error("still open one hour before the freeze")
	}
	afterFreeze := time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC)
	if !Frozen(augUsage, afterFreeze) {
		t.Error("August must be frozen after 1 September 06:00")
	}
}

// --- L2 reconciliation ---

func TestReconcileThresholds(t *testing.T) {
	cases := []struct {
		name                     string
		expected, actual         int
		wantBackfill, wantAlert  bool
	}{
		{"perfect", 720, 720, false, false},
		{"one hour missing in a month", 720, 719, true, false},   // 0.14% > 0.1%
		{"five hours missing", 720, 715, true, true},             // 0.69% > 0.5%
		{"over-metered", 720, 721, true, false},                  // absolute difference
		{"no expectation", 0, 0, false, false},
	}
	for _, c := range cases {
		got := Reconcile("res-1", c.expected, c.actual)
		if got.NeedsBackfill != c.wantBackfill {
			t.Errorf("%s: NeedsBackfill = %v, want %v (ratio %.4f)",
				c.name, got.NeedsBackfill, c.wantBackfill, got.DiffRatio)
		}
		if got.NeedsAlert != c.wantAlert {
			t.Errorf("%s: NeedsAlert = %v, want %v (ratio %.4f)",
				c.name, got.NeedsAlert, c.wantAlert, got.DiffRatio)
		}
	}
}

// TestReconcileCatchesUnmeteredResource covers the silent revenue leak: a
// resource that ran but produced no metering data at all. No amount of
// within-pipeline checking would find it, because the pipeline never saw it.
func TestReconcileCatchesUnmeteredResource(t *testing.T) {
	got := Reconcile("scecs-cn-north-1-01-a1b2c3d4", 24, 0)
	if !got.NeedsAlert {
		t.Fatal("a resource that ran 24h with zero metering must alert")
	}
	if got.DiffRatio != 1.0 {
		t.Fatalf("diff ratio = %.2f, want 1.0", got.DiffRatio)
	}
}

func TestHourOf(t *testing.T) {
	if got := HourOf(time.Date(2026, 8, 8, 12, 47, 33, 0, time.UTC)); !got.Equal(hour) {
		t.Fatalf("HourOf = %v, want %v", got, hour)
	}
}
