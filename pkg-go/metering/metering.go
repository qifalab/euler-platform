// Package metering implements usage collection and hourly aggregation
// (05-data-observability.md §5, 03-backend-services.md §4.2.5).
//
// # The governing principle
//
// 03§4.2.5: 计量宁可重采不可漏采 — over-collect rather than under-collect.
// A duplicate reading is removed by the idempotency key; a missing reading is
// revenue that was earned and never billed, and it is usually discovered by a
// customer noticing their bill is suspiciously low, which is the worst way to
// find out.
//
// Every deduplication mechanism here exists so that over-collection is safe,
// which is what makes the "collect twice" default affordable.
//
// # Deterministic idempotency keys
//
// record_id = sha1(resource_id|metering_item|window_start)[:24]
// agg_id    = sha1(resource_id|metering_item|hour_start)[:24]
//
// These are derived, not generated (05§5.5.1). An agent that retransmits after
// a network failure computes the same record_id, so the duplicate collapses at
// every layer: Kafka producer idempotence, the aggregation SETNX, the
// ClickHouse ReplacingMergeTree sort key, and the billing unique index all key
// on the same value. A random UUID would defeat all four.
//
// # Aggregation is infinitely re-runnable
//
// Aggregation keys on (resource_id, item, hour) with upsert semantics, so a
// failed aggregation job can simply be re-run — no compensating logic, no
// partial-state cleanup. This is why 03§4.2.5 can say 聚合任务失败可无限重算.
//
// # covered_ratio is the honesty signal
//
// An hour's aggregate carries the fraction of expected collection windows that
// actually arrived. Anything below 100% means the total is an undercount and
// backfill is required (05§5.5.2 L1). Publishing an aggregate without this
// signal would make an incomplete hour indistinguishable from a quiet one.
package metering

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// LateWatermark is how long after the hour closes the aggregator waits for
// stragglers before publishing (05§5.4). Data arriving after this is tagged
// BatchLate and merged into its historical window.
const LateWatermark = 10 * time.Minute

// Batch identifies the collection round a record belongs to.
const (
	BatchRealtime = "rt"
	BatchLate     = "late"
	BatchBackfill = "backfill"
)

// Quantity is a fixed-point usage amount carried as micro-units, matching the
// DECIMAL(18,6) quantity column in metering_record.
//
// Usage is not money, but it multiplies into money, so the same reasoning
// applies: float accumulation across millions of hourly rows drifts, and a
// drifting quantity produces a bill nobody can reconcile.
type Quantity int64

const quantityScale = 1_000_000

// ParseQuantity converts a decimal string into a Quantity.
func ParseQuantity(s string) (Quantity, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("metering: empty quantity")
	}
	neg := false
	if s[0] == '-' {
		neg, s = true, s[1:]
	}
	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" && fracPart == "" {
		// "." or "-" carry no digits at all; parsing them as 0 would silently
		// turn a malformed reading into a free hour.
		return 0, fmt.Errorf("metering: malformed quantity %q", s)
	}
	if len(fracPart) > 6 {
		return 0, fmt.Errorf("metering: quantity %q exceeds 6 decimal places", s)
	}
	var units int64
	for i := 0; i < len(intPart); i++ {
		c := intPart[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("metering: malformed quantity %q", s)
		}
		next := units*10 + int64(c-'0')
		if next < units {
			return 0, fmt.Errorf("metering: quantity %q overflows", s)
		}
		units = next
	}
	frac := int64(0)
	for i := 0; i < 6; i++ {
		frac *= 10
		if i < len(fracPart) {
			c := fracPart[i]
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("metering: malformed quantity %q", s)
			}
			frac += int64(c - '0')
		}
	}
	const maxInt64 = 1<<63 - 1
	if units > (maxInt64-frac)/quantityScale {
		return 0, fmt.Errorf("metering: quantity %q overflows", s)
	}
	total := units*quantityScale + frac
	if neg {
		total = -total
	}
	return Quantity(total), nil
}

// MustParseQuantity is ParseQuantity for known-good literals.
func MustParseQuantity(s string) Quantity {
	q, err := ParseQuantity(s)
	if err != nil {
		panic(err)
	}
	return q
}

// String renders the quantity with trailing zeros trimmed.
func (q Quantity) String() string {
	neg := q < 0
	v := int64(q)
	if neg {
		v = -v
	}
	units, frac := v/quantityScale, v%quantityScale
	out := fmt.Sprintf("%d", units)
	if frac != 0 {
		f := strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
		out += "." + f
	}
	if neg {
		out = "-" + out
	}
	return out
}

// Add returns q + other.
func (q Quantity) Add(other Quantity) Quantity { return q + other }

// IsZero reports whether the quantity is exactly zero.
func (q Quantity) IsZero() bool { return q == 0 }

// UsageRecord is one raw reading, as published to cloud.metering.usage.raw
// (partition key resource_id, 64 partitions, 3-day retention).
type UsageRecord struct {
	RecordID      string // deterministic; see RecordID
	AccountID     int64
	Region        string
	ResourceType  string
	ResourceID    string
	MeteringItem  string
	Quantity      Quantity
	WindowStart   time.Time // minute-aligned
	WindowSeconds int       // usually 60
	CollectTS     time.Time
	CollectorID   string
	BatchID       string
	TraceID       string
}

// RecordID derives the idempotency key for a raw reading.
//
// Deterministic by construction: an agent retransmitting the same window
// computes the same id, so the duplicate collapses at every downstream layer
// rather than becoming a second charge.
func RecordID(resourceID, meteringItem string, windowStart time.Time) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%d", resourceID, meteringItem, windowStart.UTC().Unix())))
	return hex.EncodeToString(h[:])[:24]
}

// AggID derives the idempotency key for an hourly aggregate.
func AggID(resourceID, meteringItem string, hourStart time.Time) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%d", resourceID, meteringItem, hourStart.UTC().Unix())))
	return hex.EncodeToString(h[:])[:24]
}

// HourlyUsage is the aggregation output, published to
// cloud.metering.billing.event and consumed by svc-billing.
type HourlyUsage struct {
	AggID         string
	AccountID     int64
	Region        string
	ResourceType  string
	ResourceID    string
	MeteringItem  string
	TotalQuantity Quantity
	HourStart     time.Time
	// CoveredRatio is the percentage of expected collection windows that
	// actually arrived, 0-100. Below 100 means the total is an undercount and
	// a backfill task is generated (05§5.5.2 L1).
	CoveredRatio int
	ProducedTS   time.Time
	BatchID      string
	// WindowsSeen and WindowsExpected are carried so the backfill job knows
	// how much is missing without recomputing.
	WindowsSeen     int
	WindowsExpected int
}

// Complete reports whether every expected window arrived.
func (h HourlyUsage) Complete() bool { return h.CoveredRatio >= 100 }

// Errors.
var (
	ErrEmptyWindow       = errors.New("metering: no records in window")
	ErrMixedResource     = errors.New("metering: records span multiple resources or items")
	ErrWindowMisaligned  = errors.New("metering: window start is not minute-aligned")
	ErrWindowOutsideHour = errors.New("metering: record window falls outside the aggregation hour")
	ErrFrozenPeriod      = errors.New("metering: billing period is frozen, backfill rejected")
)

// HourOf truncates a time to its hour boundary.
func HourOf(t time.Time) time.Time {
	return t.UTC().Truncate(time.Hour)
}

// Aggregator folds raw readings into hourly totals.
type Aggregator struct {
	Now func() time.Time
	// ExpectedWindowSeconds is the nominal collection period; expected window
	// count for an hour is 3600 / this.
	ExpectedWindowSeconds int
}

// NewAggregator builds an Aggregator with the standard 60-second collection
// period (05§5.2).
func NewAggregator(now func() time.Time) *Aggregator {
	if now == nil {
		now = time.Now
	}
	return &Aggregator{Now: now, ExpectedWindowSeconds: 60}
}

// Aggregate folds records for ONE (resource, item, hour) into an HourlyUsage.
//
// Duplicate record_ids are collapsed, not summed — this is where
// over-collection becomes safe. Records are deduplicated before summation
// precisely so that the "collect twice rather than miss once" default does not
// produce double charges.
func (a *Aggregator) Aggregate(records []UsageRecord, hourStart time.Time) (HourlyUsage, error) {
	if len(records) == 0 {
		return HourlyUsage{}, ErrEmptyWindow
	}

	resourceID := records[0].ResourceID
	item := records[0].MeteringItem
	seen := make(map[string]UsageRecord, len(records))
	hour := HourOf(hourStart)

	for _, r := range records {
		if r.ResourceID != resourceID || r.MeteringItem != item {
			return HourlyUsage{}, fmt.Errorf("%w: %s/%s vs %s/%s",
				ErrMixedResource, resourceID, item, r.ResourceID, r.MeteringItem)
		}
		if r.WindowStart.Second() != 0 || r.WindowStart.Nanosecond() != 0 {
			return HourlyUsage{}, fmt.Errorf("%w: %v", ErrWindowMisaligned, r.WindowStart)
		}
		// A record whose window falls outside [hourStart, hourStart+1h) belongs
		// to a different aggregate; summing it here would double-charge one hour
		// and undercount another.
		ws := r.WindowStart.UTC()
		if ws.Before(hour) || !ws.Before(hour.Add(time.Hour)) {
			return HourlyUsage{}, fmt.Errorf("%w: window %v vs hour %v", ErrWindowOutsideHour, r.WindowStart, hour)
		}
		id := r.RecordID
		if id == "" {
			id = RecordID(r.ResourceID, r.MeteringItem, r.WindowStart)
		}
		// Last writer wins for the same window: a re-collection of the same
		// window is a correction, not an addition.
		seen[id] = r
	}

	var total Quantity
	windows := make(map[int64]bool, len(seen))
	batch := BatchRealtime
	for _, r := range seen {
		total = total.Add(r.Quantity)
		windows[r.WindowStart.UTC().Unix()] = true
		if r.BatchID == BatchLate || r.BatchID == BatchBackfill {
			batch = r.BatchID
		}
	}

	// Guard the divisor BEFORE dividing: a zero-value Aggregator (or a
	// misconfigured negative period) must fall back to the standard 60-second
	// period rather than panicking with an integer divide by zero.
	windowSeconds := a.ExpectedWindowSeconds
	if windowSeconds <= 0 {
		windowSeconds = 60
	}
	expected := 3600 / windowSeconds
	if expected <= 0 {
		expected = 60
	}
	covered := len(windows) * 100 / expected
	if covered > 100 {
		// More windows than expected means the collection period is finer than
		// configured. Cap the ratio rather than reporting >100%, which would
		// read as "more than complete" and hide a misconfiguration.
		covered = 100
	}

	first := records[0]
	return HourlyUsage{
		AggID:           AggID(resourceID, item, hourStart),
		AccountID:       first.AccountID,
		Region:          first.Region,
		ResourceType:    first.ResourceType,
		ResourceID:      resourceID,
		MeteringItem:    item,
		TotalQuantity:   total,
		HourStart:       HourOf(hourStart),
		CoveredRatio:    covered,
		ProducedTS:      a.Now(),
		BatchID:         batch,
		WindowsSeen:     len(windows),
		WindowsExpected: expected,
	}, nil
}

// Merge folds a late or backfilled aggregate into an existing one for the same
// hour.
//
// Merging is upsert-by-window rather than addition: a late record for a window
// already counted must not be added twice. The caller supplies the union of
// raw records, so Merge is expressed as a re-aggregation, which is what makes
// the operation infinitely repeatable (03§4.2.5).
func (a *Aggregator) Merge(existing HourlyUsage, lateRecords []UsageRecord, allRecords []UsageRecord) (HourlyUsage, error) {
	merged, err := a.Aggregate(append(append([]UsageRecord{}, allRecords...), lateRecords...), existing.HourStart)
	if err != nil {
		return HourlyUsage{}, err
	}
	merged.AggID = existing.AggID // stable across re-aggregation
	if len(lateRecords) > 0 {
		merged.BatchID = BatchLate
	}
	return merged, nil
}

// WatermarkPassed reports whether the hour is settled enough to publish: the
// hour has ended and the late-arrival watermark has elapsed.
func (a *Aggregator) WatermarkPassed(hourStart time.Time) bool {
	return !a.Now().Before(HourOf(hourStart).Add(time.Hour + LateWatermark))
}

// BackfillTask describes an incomplete hour that must be re-collected.
type BackfillTask struct {
	ResourceID   string
	MeteringItem string
	HourStart    time.Time
	CoveredRatio int
	MissingCount int
}

// NeedsBackfill returns a task when the aggregate is incomplete (05§5.5.2 L1).
func NeedsBackfill(h HourlyUsage) (BackfillTask, bool) {
	if h.Complete() {
		return BackfillTask{}, false
	}
	return BackfillTask{
		ResourceID:   h.ResourceID,
		MeteringItem: h.MeteringItem,
		HourStart:    h.HourStart,
		CoveredRatio: h.CoveredRatio,
		MissingCount: h.WindowsExpected - h.WindowsSeen,
	}, true
}

// FreezeBoundary returns the moment a billing month becomes read-only: the 1st
// of the following month at 06:00 UTC (05§5.6).
//
// After the freeze, backfill for that month is rejected. Without a boundary,
// a late correction could silently change a bill the customer already paid and
// reconciled, which is worse than the small undercount it fixes.
func FreezeBoundary(month time.Time) time.Time {
	m := month.UTC()
	next := time.Date(m.Year(), m.Month(), 1, 6, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	return next
}

// Frozen reports whether the hour's billing period is closed to backfill.
func Frozen(hourStart time.Time, now time.Time) bool {
	return !now.Before(FreezeBoundary(hourStart))
}

// ReconcileL2 compares expected billable duration against actual metered
// hours for one resource (05§5.5.2 L2, T+1).
//
// Expected comes from the resource lifecycle timeline
// (cloud.resource.lifecycle.event is authoritative), actual from the
// aggregates. The comparison is what catches a resource that ran but was never
// metered — the silent revenue leak that no amount of within-pipeline checking
// would find, because the pipeline never saw the data at all.
type ReconcileL2 struct {
	ResourceID    string
	ExpectedHours int
	ActualHours   int
	DiffRatio     float64 // |expected-actual| / expected
	NeedsBackfill bool    // >0.1%
	NeedsAlert    bool    // >0.5%
}

// Reconcile compares expected vs actual metered hours.
func Reconcile(resourceID string, expectedHours, actualHours int) ReconcileL2 {
	r := ReconcileL2{
		ResourceID:    resourceID,
		ExpectedHours: expectedHours,
		ActualHours:   actualHours,
	}
	if expectedHours > 0 {
		diff := expectedHours - actualHours
		if diff < 0 {
			diff = -diff
		}
		r.DiffRatio = float64(diff) / float64(expectedHours)
	}
	// Thresholds are ENGINEERING triggers, not the acceptance criterion.
	// Adjudication S17: commercial acceptance is 无未解释差异 — every
	// difference must be root-caused and closed, whatever its size. These
	// numbers only decide whether a machine or a human reacts first.
	r.NeedsBackfill = r.DiffRatio > 0.001
	r.NeedsAlert = r.DiffRatio > 0.005
	return r
}

// SortRecords orders records by window for deterministic aggregation output,
// which matters when a test or a reconciliation report compares two runs.
func SortRecords(records []UsageRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].WindowStart.Before(records[j].WindowStart)
	})
}
