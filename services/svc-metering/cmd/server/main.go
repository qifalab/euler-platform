// Package main: svc-metering — the 计量 link (03-backend-services.md §4.2.5,
// 05-data-observability.md §5).
//
// Owns usage ingest → hourly aggregation (pkg-go/metering) → settlement +
// monthly bill (pkg-go/billing). The governing principle 03§4.2.5 is 计量宁可
// 重采不可漏采: the aggregator deduplicates by deterministic record_id, so
// over-collection is safe and a failed aggregation job can be re-run without
// compensating logic.
//
// Routes (gateway-authorized, X-Euler-Account-Id injected):
//
//	POST /api/v1/metering/ingest    — record a usage point (resourceId,
//	                                 productCode, metric, value) for the
//	                                 current minute window
//	GET  /api/v1/metering/aggregate — aggregated usage for a resourceId over a
//	                                 period (hour|day|month); re-runs the real
//	                                 Aggregator so it is infinitely repeatable
//	GET  /api/v1/metering/bills     — monthly bill summary via billing.Summarize
//	                                 (Settle + Summarize over stored aggregates)
//
// stdlib-HTTP service. Envelope {RequestId,Code,Data} (03§9.3). In-memory store
// (Kafka raw → ClickHouse aggregates in production).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qifalab/euler-platform/anomaly"
	"github.com/qifalab/euler-platform/billing"
	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/metering"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/runmode"
)

const accountIDHeader = "X-Euler-Account-Id"

// maxBodyBytes caps request bodies before JSON decoding (defense against
// oversized payloads).
const maxBodyBytes = 1 << 20 // 1 MiB

// internalToken is the optional shared secret for service-to-service calls.
// When EULER_INTERNAL_TOKEN is set, every request must carry a matching
// X-Euler-Internal-Token header. Unset (dev default) disables the check.
var internalToken = os.Getenv("EULER_INTERNAL_TOKEN")

// seededResourceID matches the demo + console-bff seed for account 100123.
const seededResourceID = "euecs-cn-north-1-01-a1b2c3d4"

// meteringStore holds raw usage records keyed by (resourceID, meteringItem) so
// aggregation can be re-run over the same data. Hourly aggregates are cached by
// their deterministic AggID and re-derived on demand. Charges are materialized
// from aggregates via the billing Engine and folded into a monthly bill via
// billing.Summarize.
type meteringStore struct {
	mu sync.RWMutex
	// records indexed by (resourceID|meteringItem) -> []UsageRecord
	records map[string][]metering.UsageRecord
	// account/resource metadata discovered at ingest time, so aggregation
	// does not need to re-read it from the resource service.
	accounts map[string]int64  // resourceID -> accountID
	regions  map[string]string // resourceID -> region
	types    map[string]string // resourceID -> resourceType
	products map[string]string // resourceID -> productCode
	// aggCache caches the most recent aggregate per aggID, matching the
	// ClickHouse ReplacingMergeTree semantics (upsert by deterministic key).
	aggCache map[string]metering.HourlyUsage
	// unitPrices holds the per-metering-item unit price (price snapshot in
	// production; seeded here for the demo resource).
	unitPrices map[string]pricing.Amount
	// snapshotIDs holds the price-snapshot reference per metering item.
	snapshotIDs map[string]string
	// pools are the deduction pools available to the seeded account. Phase 1
	// sells no resource packs (decision D6), so it is cash balance only.
	pools map[int64][]billing.Pool
	now   func() time.Time
	// owners verifies resource ownership (and liveness) against the ledger.
	owners ownerVerifier
	// ownerChecks caches the ledger's verdict per resource for ownerCheckTTL
	// (owners.go): a resource released after its first reading is rejected
	// once the cached verdict expires, instead of being billed forever.
	ownerChecks map[string]ownerCheck
	// agg is the durable aggregate ledger (nil = in-memory only); every write
	// into aggCache goes through cacheAgg, and a restart rebuilds the cache
	// from the ledger (repo.go).
	agg aggRepo
	// restored holds the aggregates rehydrated from the ledger at startup.
	// Their raw records did not survive the restart, so a post-restart
	// re-aggregation of the same hour sees only the new windows; it is folded
	// on top of the restored total instead of replacing it (withRestoredLocked).
	restored map[string]metering.HourlyUsage
}

// newMeteringStore is the in-memory (demo/test) store. It goes through the
// same constructor main() uses, so every field main() relies on — the owner
// verifier above all — is wired identically in both.
func newMeteringStore() *meteringStore {
	s, err := newMeteringStoreWith(nil)
	if err != nil {
		// Unreachable: the in-memory mode has no ledger to fail on.
		panic(err)
	}
	return s
}

// seededAccountID is the demo account the price/pool config and the demo
// usage belong to.
const seededAccountID = int64(100123)

// loadPriceConfig installs the price snapshot and the deduction pools. This is
// process configuration, not ledger state, so it is loaded in BOTH modes: a
// persistent store that skipped it would settle nothing (no unit price) and
// report an empty bill as final.
func (s *meteringStore) loadPriceConfig() {
	// Price snapshot frozen at order time (01§12.3 rule 4): cpu_core_hour @
	// 0.25 CNY. Historical bills must be reproducible against the snapshot.
	s.unitPrices["cpu_core_hour"] = pricing.MustParseAmount("0.25")
	s.snapshotIDs["cpu_core_hour"] = "snap-9001"

	// A real cash-balance pool so settlements reconcile rather than fall into
	// arrears. 1000 CNY is comfortably more than a few hours of a 2C instance.
	s.pools[seededAccountID] = []billing.Pool{
		{Source: billing.SourceBalance, Available: pricing.MustParseAmount("1000")},
	}
}

// seed plants the seeded euecs resource with a few hours of CPU usage for
// account 100123. A 2-core instance reports 2 core-minutes per minute window,
// i.e. 0.033333 core-hours per window. Three complete hours (60 windows each)
// are seeded so the monthly bill has real rows and reconciles.
func (s *meteringStore) seed() {
	const acct = seededAccountID
	base := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)

	// Register the resource metadata.
	s.accounts[seededResourceID] = acct
	s.regions[seededResourceID] = "cn-north-1"
	s.types[seededResourceID] = "ecs"
	s.products[seededResourceID] = "euecs"

	// Three complete hours of CPU usage for the seeded resource.
	agg := metering.NewAggregator(s.now)
	for h := 0; h < 3; h++ {
		hr := base.Add(time.Duration(h) * time.Hour)
		for i := 0; i < 60; i++ {
			ws := hr.Add(time.Duration(i) * time.Minute)
			s.recordLocked(metering.UsageRecord{
				RecordID:      metering.RecordID(seededResourceID, "cpu_core_hour", ws),
				AccountID:     acct,
				Region:        "cn-north-1",
				ResourceType:  "ecs",
				ResourceID:    seededResourceID,
				MeteringItem:  "cpu_core_hour",
				Quantity:      metering.MustParseQuantity("0.033333"),
				WindowStart:   ws,
				WindowSeconds: 60,
				CollectTS:     ws.Add(time.Second),
				CollectorID:   "agent-seed",
				BatchID:       metering.BatchRealtime,
			})
		}
		// Cache the aggregate so /bills can settle without a prior /aggregate
		// call. Matches the ClickHouse upsert-by-AggID semantics.
		key := seededResourceID + "|" + "cpu_core_hour"
		usage, err := agg.Aggregate(s.hourRecordsLocked(key, hr), hr)
		if err != nil {
			slog.Error("seed aggregation failed", "hour", hr, "err", err)
			continue
		}
		s.cacheAgg(usage)
	}
}

// hourRecordsLocked returns the raw records of one (resource|item) key whose
// window falls in the given hour. Caller must hold s.mu. The aggregator folds
// exactly ONE hour and rejects anything outside it (ErrWindowOutsideHour), so
// handing it the resource's whole history fails every hour after the first.
func (s *meteringStore) hourRecordsLocked(key string, hourStart time.Time) []metering.UsageRecord {
	hour := metering.HourOf(hourStart)
	var out []metering.UsageRecord
	for _, r := range s.records[key] {
		if metering.HourOf(r.WindowStart).Equal(hour) {
			out = append(out, r)
		}
	}
	return out
}

// recordLocked appends a raw reading without acquiring the lock. Caller must
// hold s.mu. Records are indexed by (resourceID|meteringItem) so the aggregator
// receives only the records for one (resource, item) pair — Aggregate rejects
// mixed resources with ErrMixedResource.
func (s *meteringStore) recordLocked(r metering.UsageRecord) {
	key := r.ResourceID + "|" + r.MeteringItem
	s.records[key] = append(s.records[key], r)
	if _, ok := s.accounts[r.ResourceID]; !ok {
		s.accounts[r.ResourceID] = r.AccountID
	}
	if _, ok := s.regions[r.ResourceID]; !ok {
		s.regions[r.ResourceID] = r.Region
	}
	if _, ok := s.types[r.ResourceID]; !ok {
		s.types[r.ResourceID] = r.ResourceType
	}
}

type ingestRequest struct {
	ResourceID  string `json:"resourceId"`
	ProductCode string `json:"productCode"`
	Metric      string `json:"metric"`
	Value       string `json:"value"` // decimal string, e.g. "0.033333"
}

func (s *meteringStore) handleIngest(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req ingestRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.ResourceID == "" || req.Metric == "" || req.Value == "" {
		writeErr(w, "Common.InvalidParameter", 400, "resourceId, metric and value are required")
		return
	}
	qty, err := metering.ParseQuantity(req.Value)
	if err != nil {
		slog.Warn("ingest value rejected", "resource", req.ResourceID, "err", err)
		writeErr(w, "Common.InvalidParameter", 400, "invalid value")
		return
	}
	// A reading must come from the resource's owner, as recorded by the
	// resource ledger — not whoever happens to name the id first — and the
	// resource must still be live. The ledger's verdict is cached for
	// ownerCheckTTL, then asked again: checking only the first reading let a
	// released resource be billed for as long as its producer kept sending.
	switch err := s.checkOwner(r.Context(), acct, req.ResourceID); {
	case errors.Is(err, errResourceInactive):
		writeErr(w, "Metering.ResourceInactive", 409, "resource is released or was never created")
		return
	case errors.Is(err, errResourceNotOwned):
		writeErr(w, "Metering.ResourceNotOwned", 403, "resource belongs to another account")
		return
	case err != nil:
		slog.Error("resource owner lookup failed", "resource", req.ResourceID, "err", err)
		writeErr(w, "Metering.OwnerLookupFailed", 503, "resource ownership could not be verified")
		return
	}

	// Window = current minute, aligned. The collector writes one reading per
	// minute window per (resource, item); the deterministic RecordID makes a
	// retransmit collapse rather than double-charge.
	ws := s.now().UTC().Truncate(time.Minute)

	s.mu.Lock()
	defer s.mu.Unlock()
	// The account that owns the resource must match the caller. For a new
	// resource (first ingest), adopt the caller's account.
	if owner, exists := s.accounts[req.ResourceID]; exists && owner != acct {
		writeErr(w, "Metering.ResourceNotOwned", 403, "resource belongs to another account")
		return
	}
	// s.regions / s.types are shared maps; they must only be read while
	// holding s.mu, so the record is built inside the lock.
	rec := metering.UsageRecord{
		RecordID:      metering.RecordID(req.ResourceID, req.Metric, ws),
		AccountID:     acct,
		Region:        s.regions[req.ResourceID],
		ResourceType:  s.types[req.ResourceID],
		ResourceID:    req.ResourceID,
		MeteringItem:  req.Metric,
		Quantity:      qty,
		WindowStart:   ws,
		WindowSeconds: 60,
		CollectTS:     s.now(),
		CollectorID:   "http-ingest",
		BatchID:       metering.BatchRealtime,
	}
	if req.ProductCode != "" {
		s.products[req.ResourceID] = req.ProductCode
	}
	s.recordLocked(rec)

	// Idempotent: a retransmit of the same window produces the same RecordID,
	// and Aggregate dedups by key — so we do not maintain a separate dedup set
	// here. Re-run aggregation to refresh the cache for that hour.
	key := req.ResourceID + "|" + req.Metric
	hourStart := metering.HourOf(ws)
	agg := metering.NewAggregator(s.now)
	if usage, err := agg.Aggregate(s.hourRecordsLocked(key, hourStart), hourStart); err != nil {
		// The reading itself is stored; the aggregate is re-derivable from it.
		slog.Error("ingest aggregation failed", "resource", req.ResourceID, "metric", req.Metric, "hour", hourStart, "err", err)
	} else {
		s.cacheAgg(s.withRestoredLocked(usage))
	}

	writeJSON(w, "OK", map[string]any{
		"recordId":    rec.RecordID,
		"resourceId":  rec.ResourceID,
		"metric":      rec.MeteringItem,
		"quantity":    rec.Quantity.String(),
		"windowStart": rec.WindowStart.Format(time.RFC3339),
	})
}

func (s *meteringStore) handleAggregate(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	resourceID := r.URL.Query().Get("resourceId")
	if resourceID == "" {
		writeErr(w, "Common.InvalidParameter", 400, "resourceId is required")
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "hour"
	}
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "cpu_core_hour"
	}

	s.mu.RLock()
	owner, known := s.accounts[resourceID]
	s.mu.RUnlock()
	if !known {
		writeErr(w, "Metering.ResourceNotFound", 404, "no usage recorded for resource")
		return
	}
	if owner != acct {
		writeErr(w, "Metering.ResourceNotOwned", 403, "resource belongs to another account")
		return
	}

	// Re-run aggregation over the stored raw records for this (resource, item).
	// Aggregation is infinitely re-runnable (03§4.2.5) and dedupes by record_id,
	// so the result is stable regardless of how many times a window was ingested.
	s.mu.RLock()
	key := resourceID + "|" + metric
	records := append([]metering.UsageRecord(nil), s.records[key]...)
	s.mu.RUnlock()

	agg := metering.NewAggregator(s.now)
	// Group records by hour, aggregate each hour, keep the most recent.
	byHour := make(map[time.Time][]metering.UsageRecord)
	for _, rec := range records {
		h := metering.HourOf(rec.WindowStart)
		byHour[h] = append(byHour[h], rec)
	}
	hours := make([]time.Time, 0, len(byHour))
	for h := range byHour {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })

	// Filter to the requested period, evaluated relative to the newest
	// aggregate present (not wall-clock now) so historical seeded data shows.
	var newest time.Time
	if len(hours) > 0 {
		newest = hours[len(hours)-1]
	}

	// Filter to the requested period.
	var out []map[string]any
	for _, h := range hours {
		if !inPeriod(h, period, newest) {
			continue
		}
		usage, err := agg.Aggregate(byHour[h], h)
		if err != nil {
			continue
		}
		out = append(out, hourlyUsageToMap(usage))
	}

	writeJSON(w, "OK", map[string]any{
		"resourceId": resourceID,
		"metric":     metric,
		"period":     period,
		"aggregates": out,
		"hourCount":  len(out),
	})
}

func (s *meteringStore) handleBills(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		// Default to the current month in UTC, matching FreezeBoundary's frame.
		period = s.now().UTC().Format("2006-01")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Settle against a WORKING COPY of the account's pools: each charge spends
	// what the previous ones left, so the paid amount cannot exceed the
	// balance and a drained balance shows up as arrears (shortfall).
	pools := append([]billing.Pool(nil), s.pools[acct]...)
	// Settle every cached aggregate for resources owned by this account in the
	// requested period, oldest hour first (map order is random; the waterfall
	// is order-dependent once pools run dry). The unit price comes from the
	// snapshot, never the live price (01§12.3 rule 4).
	usages := make([]metering.HourlyUsage, 0)
	for _, usage := range s.aggCache {
		if usage.AccountID != acct || usage.HourStart.UTC().Format("2006-01") != period {
			continue
		}
		usages = append(usages, usage)
	}
	sort.Slice(usages, func(i, j int) bool {
		a, b := usages[i], usages[j]
		if !a.HourStart.Equal(b.HourStart) {
			return a.HourStart.Before(b.HourStart)
		}
		if a.ResourceID != b.ResourceID {
			return a.ResourceID < b.ResourceID
		}
		return a.MeteringItem < b.MeteringItem
	})

	charges := make([]billing.Charge, 0, len(usages))
	// unsettled counts aggregates that could not become a charge (no price
	// snapshot, or a settle error). Each is usage the bill does not explain,
	// so it counts as unreconciled and keeps the bill from being final.
	unsettled := 0
	for _, usage := range usages {
		productCode := s.products[usage.ResourceID]
		unitPrice, ok := s.unitPrices[usage.MeteringItem]
		if !ok {
			// No price snapshot for this metric: the charge cannot be computed
			// reproducibly, so do not synthesize a price — but do not hide the
			// usage either.
			slog.Warn("no unit price for metered usage", "resource", usage.ResourceID, "metric", usage.MeteringItem, "hour", usage.HourStart)
			unsettled++
			continue
		}
		snap := s.snapshotIDs[usage.MeteringItem]
		// This is a READ of the period's bill, not a backfill: the freeze
		// boundary guards writes into a closed month, and applying it here
		// made every past month read back empty. Settle as of the moment the
		// hour closed — the settlement this view reproduces.
		engine := billing.NewEngine(settleClock(usage.HourStart))
		sett, err := engine.Settle(usage, unitPrice, productCode, snap, pools)
		if err != nil {
			slog.Warn("settle failed", "resource", usage.ResourceID, "hour", usage.HourStart, "err", err)
			unsettled++
			continue
		}
		consumePools(pools, productCode, sett.Charge.Deductions)
		charges = append(charges, sett.Charge)
	}

	bill := billing.Summarize(acct, period, charges)
	bill.UnreconciledCount += unsettled
	out := billToMap(bill, charges)
	out["unsettledCount"] = unsettled
	writeJSON(w, "OK", out)
}

// settleClock pins the settlement clock of the bill read path to the close of
// the aggregate's hour (always inside the open billing period, so the backfill
// freeze does not apply, and pool expiry is judged when the usage happened).
func settleClock(hourStart time.Time) func() time.Time {
	at := metering.HourOf(hourStart).Add(time.Hour)
	return func() time.Time { return at }
}

// consumePools subtracts a settlement's deductions from the working pools so
// the next charge settles against the remaining balance (Settle itself never
// mutates pools: its deductions are the intent to consume).
func consumePools(pools []billing.Pool, productCode string, deductions []billing.Deduction) {
	for _, d := range deductions {
		remaining := d.Amount
		for i := range pools {
			if remaining.IsZero() {
				break
			}
			p := &pools[i]
			if p.Source != d.Source || p.Ref != d.Ref || !poolApplies(*p, productCode) {
				continue
			}
			if p.Available.IsZero() || p.Available.IsNegative() {
				continue
			}
			take := pricing.Min(p.Available, remaining)
			p.Available = p.Available.Sub(take)
			remaining = remaining.Sub(take)
		}
	}
}

// poolApplies mirrors billing's product restriction: empty means universal.
func poolApplies(p billing.Pool, productCode string) bool {
	if len(p.ProductCodes) == 0 {
		return true
	}
	for _, c := range p.ProductCodes {
		if c == productCode {
			return true
		}
	}
	return false
}

func hourlyUsageToMap(u metering.HourlyUsage) map[string]any {
	return map[string]any{
		"aggId":           u.AggID,
		"resourceId":      u.ResourceID,
		"metric":          u.MeteringItem,
		"totalQuantity":   u.TotalQuantity.String(),
		"hourStart":       u.HourStart.Format(time.RFC3339),
		"coveredRatio":    u.CoveredRatio,
		"complete":        u.Complete(),
		"windowsSeen":     u.WindowsSeen,
		"windowsExpected": u.WindowsExpected,
		"batch":           u.BatchID,
	}
}

func billToMap(b billing.MonthlyBill, charges []billing.Charge) map[string]any {
	lines := make([]map[string]any, 0, len(charges))
	var shortfall pricing.Amount
	for _, c := range charges {
		shortfall = shortfall.Add(c.Shortfall)
		deductions := make([]map[string]any, 0, len(c.Deductions))
		for _, d := range c.Deductions {
			deductions = append(deductions, map[string]any{
				"source": string(d.Source), "ref": d.Ref, "amount": d.Amount.String(),
			})
		}
		lines = append(lines, map[string]any{
			"chargeId":     c.ChargeID,
			"resourceId":   c.ResourceID,
			"productCode":  c.ProductCode,
			"metric":       c.MeteringItem,
			"quantity":     c.Quantity.String(),
			"unitPrice":    c.UnitPrice.String(),
			"pretaxAmount": c.PretaxAmount.String(),
			"payAmount":    c.PayAmount.String(),
			"coveredRatio": c.CoveredRatio,
			"incomplete":   c.Incomplete(),
			"reconciles":   c.Reconciles(),
			"snapshotId":   c.SnapshotID,
			"shortfall":    c.Shortfall.String(),
			"deductions":   deductions,
		})
	}
	return map[string]any{
		"accountId":         b.AccountID,
		"billPeriod":        b.BillPeriod,
		"totalAmount":       b.TotalAmount.String(),
		"paidAmount":        b.PaidAmount.String(),
		"chargeCount":       b.ChargeCount,
		"incompleteCharges": b.IncompleteCharges,
		"unreconciledCount": b.UnreconciledCount,
		"shortfallAmount":   shortfall.String(),
		"inArrears":         !shortfall.IsZero(),
		"final":             b.Final(),
		"lines":             lines,
	}
}

// inPeriod reports whether an hour boundary falls inside the requested period.
// period is a calendar keyword — "hour" (last 24 aggregated hours), "day"
// (today's date), or "month" (this calendar month) — evaluated relative to the
// newest aggregate present, so historical seeded data is visible even when the
// wall clock is past it. An explicit "YYYY-MM" or "YYYY-MM-DD" period string is
// matched literally. Unknown keywords default to including everything.
func inPeriod(h time.Time, period string, newest time.Time) bool {
	ref := newest.UTC()
	h = h.UTC()
	switch strings.ToLower(period) {
	case "month":
		return h.Year() == ref.Year() && h.Month() == ref.Month()
	case "day":
		return h.Year() == ref.Year() && h.Month() == ref.Month() && h.Day() == ref.Day()
	case "hour", "":
		// Include the 24 hours up to the newest aggregate, so the most recent
		// seeded hours are returned even when now() has moved past them. The
		// window is (ref-24h, ref]: both ends inclusive would be 25 hours.
		return h.After(ref.Add(-24*time.Hour)) && !h.After(ref)
	default:
		// An explicit YYYY-MM or YYYY-MM-DD: match literally by formatted string.
		if len(period) == 7 {
			return h.Format("2006-01") == period
		}
		if len(period) == 10 {
			return h.Format("2006-01-02") == period
		}
		return true
	}
}

// handleAnomalyScan implements GET /api/v1/metering/anomaly-scan — phase-3
// D-2 异常用量检测 (deferred from phase 2, 09§4.4 B7): runs the pkg-go/anomaly
// z-score detector over a resource's stored usage series and returns the
// verdict (SPIKE/DROP/NONE) plus the trailing baseline. The verdict is a SIGNAL
// for alert-center, never a billing decision — metering does not drop or
// relabel a usage record on an anomaly verdict (03§4.2.5 宁可重采不可漏采).
func (s *meteringStore) handleAnomalyScan(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	resourceID := r.URL.Query().Get("resourceId")
	metric := r.URL.Query().Get("metric")
	if resourceID == "" || metric == "" {
		writeErr(w, "Metering.InvalidParameter", 400, "resourceId and metric are required")
		return
	}
	// Owner check, same as every other read path in this service: the usage
	// series of another tenant's resource is not this caller's data.
	s.mu.RLock()
	owner, known := s.accounts[resourceID]
	s.mu.RUnlock()
	if !known {
		writeErr(w, "Metering.ResourceNotFound", 404, "no usage recorded for resource")
		return
	}
	if owner != acct {
		writeErr(w, "Metering.ResourceNotOwned", 403, "resource belongs to another account")
		return
	}
	key := resourceID + "|" + metric
	s.mu.RLock()
	records := make([]metering.UsageRecord, len(s.records[key]))
	copy(records, s.records[key])
	s.mu.RUnlock()

	// A retransmit carries the same deterministic RecordID; it is one reading,
	// not two (last writer wins, as in the aggregator). Counting it twice would
	// skew the baseline toward whatever the collector happened to resend.
	latest := make(map[string]metering.UsageRecord, len(records))
	for _, rec := range records {
		latest[rec.RecordID] = rec
	}
	records = records[:0]
	for _, rec := range latest {
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].WindowStart.Before(records[j].WindowStart) })
	series := make([]float64, 0, len(records))
	for _, rec := range records {
		// Quantity is fixed-point micro-units; the detector works in the
		// metric's own unit.
		series = append(series, float64(rec.Quantity)/1e6)
	}
	if len(series) < 2 {
		writeJSON(w, "OK", map[string]any{
			"resourceId": resourceID, "metric": metric,
			"anomalous": false, "kind": "NONE", "reason": "insufficient samples",
		})
		return
	}
	// Baseline over the trailing window (all but the latest), verdict on the
	// latest point — the baseline never includes the point under test.
	var b anomaly.Baseline
	for _, x := range series[:len(series)-1] {
		b.Add(x)
	}
	result := anomaly.Detect(series[len(series)-1], b.Mean, b.StdDev())
	writeJSON(w, "OK", map[string]any{
		"resourceId":     resourceID,
		"metric":         metric,
		"anomalous":      result.Anomalous,
		"kind":           string(result.Kind),
		"score":          result.Score,
		"baselineMean":   b.Mean,
		"baselineStdDev": b.StdDev(),
	})
}

// accountIDFrom extracts the caller's account id.
//
// TRUST BOUNDARY: X-Euler-Account-Id is trusted only because these routes are
// reachable exclusively via the APISIX gateway, which strips any
// client-supplied copy and injects the authenticated account. Deployments that
// cannot guarantee that network isolation should set EULER_INTERNAL_TOKEN so
// callers must also present the shared X-Euler-Internal-Token secret.
func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if internalToken != "" && r.Header.Get("X-Euler-Internal-Token") != internalToken {
		writeErr(w, "Common.Forbidden", 403, "invalid internal token")
		return 0, false
	}
	raw := r.Header.Get(accountIDHeader)
	if raw == "" {
		writeErr(w, "Common.MissingAccountId", 403, "X-Euler-Account-Id header is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed X-Euler-Account-Id")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, code string, data any) {
	rid := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Data": data})
}

func writeErr(w http.ResponseWriter, code string, status int, msg string) {
	rid := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Message": msg})
}

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := httpmw.SanitizeRequestID(r.Header.Get("X-Euler-TraceId"))
		if id == "" {
			id = fmt.Sprintf("metering-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Euler-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

func recoverMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "rec", rec, "path", r.URL.Path)
				writeErr(w, "Common.InternalError", 500, "internal error")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("http", ":9206", "HTTP listen address")
	flag.Parse()
	// Production refuses to start without the internal shared secret: the
	// X-Euler-Account-Id this service trusts must provably come from the gateway.
	runtimeRequireSecrets()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	agg, err := newAggRepo(context.Background())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	store, err := newMeteringStoreWith(agg)
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	// Log where the aggregate ledger lives: aggregates are the basis every bill
	// reconciles against, and a cache-only store forgets them on restart.
	slog.Info("svc-metering aggregate ledger ready", "persistent", agg != nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/metering/ingest", store.handleIngest)
	mux.HandleFunc("GET /api/v1/metering/aggregate", store.handleAggregate)
	mux.HandleFunc("GET /api/v1/metering/bills", store.handleBills)
	mux.HandleFunc("GET /api/v1/metering/anomaly-scan", store.handleAnomalyScan)

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(httpmw.InternalToken(mux))), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("svc-metering listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutdown signal received, draining")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
}

// runtimeRequireSecrets enforces the production secret set (pkg-go/runmode).
func runtimeRequireSecrets() {
	runmode.MustRequireSecrets(httpmw.InternalTokenEnv)
}
