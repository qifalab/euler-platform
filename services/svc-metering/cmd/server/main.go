// Package main: svc-metering — the 计量 link (03-backend-services.md §4.2.5,
// 05-data-observability.md §5).
//
// Owns usage ingest → hourly aggregation (pkg-go/metering) → settlement +
// monthly bill (pkg-go/billing). The governing principle 03§4.2.5 is 计量宁可
// 重采不可漏采: the aggregator deduplicates by deterministic record_id, so
// over-collection is safe and a failed aggregation job can be re-run without
// compensating logic.
//
// Routes (gateway-authorized, X-Sc-Account-Id injected):
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

	"github.com/starcloud/sc-platform/billing"
	"github.com/starcloud/sc-platform/metering"
	"github.com/starcloud/sc-platform/pricing"
)

const accountIDHeader = "X-Sc-Account-Id"

// maxBodyBytes caps request bodies before JSON decoding (defense against
// oversized payloads).
const maxBodyBytes = 1 << 20 // 1 MiB

// internalToken is the optional shared secret for service-to-service calls.
// When SC_INTERNAL_TOKEN is set, every request must carry a matching
// X-Sc-Internal-Token header. Unset (dev default) disables the check.
var internalToken = os.Getenv("SC_INTERNAL_TOKEN")

// seededResourceID matches the demo + console-bff seed for account 100123.
const seededResourceID = "scecs-cn-north-1-01-a1b2c3d4"

// meteringStore holds raw usage records keyed by (resourceID, meteringItem) so
// aggregation can be re-run over the same data. Hourly aggregates are cached by
// their deterministic AggID and re-derived on demand. Charges are materialized
// from aggregates via the billing Engine and folded into a monthly bill via
// billing.Summarize.
type meteringStore struct {
	mu       sync.RWMutex
	// records indexed by (resourceID|meteringItem) -> []UsageRecord
	records  map[string][]metering.UsageRecord
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
}

func newMeteringStore() *meteringStore {
	s := &meteringStore{
		records:     make(map[string][]metering.UsageRecord),
		accounts:    make(map[string]int64),
		regions:     make(map[string]string),
		types:       make(map[string]string),
		products:    make(map[string]string),
		aggCache:    make(map[string]metering.HourlyUsage),
		unitPrices:  make(map[string]pricing.Amount),
		snapshotIDs: make(map[string]string),
		pools:       make(map[int64][]billing.Pool),
		now:         time.Now,
	}
	s.seed()
	return s
}

// seed plants the seeded scecs resource with a few hours of CPU usage for
// account 100123. A 2-core instance reports 2 core-minutes per minute window,
// i.e. 0.033333 core-hours per window. Three complete hours (60 windows each)
// are seeded so the monthly bill has real rows and reconciles.
func (s *meteringStore) seed() {
	const acct = int64(100123)
	base := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)

	// Register the resource metadata.
	s.accounts[seededResourceID] = acct
	s.regions[seededResourceID] = "cn-north-1"
	s.types[seededResourceID] = "ecs"
	s.products[seededResourceID] = "scecs"

	// Price snapshot frozen at order time (01§12.3 rule 4): cpu_core_hour @
	// 0.25 CNY. Historical bills must be reproducible against the snapshot.
	s.unitPrices["cpu_core_hour"] = pricing.MustParseAmount("0.25")
	s.snapshotIDs["cpu_core_hour"] = "snap-9001"

	// A real cash-balance pool so settlements reconcile rather than fall into
	// arrears. 1000 CNY is comfortably more than a few hours of a 2C instance.
	s.pools[acct] = []billing.Pool{
		{Source: billing.SourceBalance, Available: pricing.MustParseAmount("1000")},
	}

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
				ResourceType: "ecs",
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
		if usage, err := agg.Aggregate(s.records[key], hr); err == nil {
			s.aggCache[usage.AggID] = usage
		}
	}
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
		MeteringItem:   req.Metric,
		Quantity:       qty,
		WindowStart:    ws,
		WindowSeconds:  60,
		CollectTS:      s.now(),
		CollectorID:    "http-ingest",
		BatchID:        metering.BatchRealtime,
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
	if usage, err := agg.Aggregate(s.records[key], hourStart); err == nil {
		s.aggCache[usage.AggID] = usage
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
		"resourceId":  resourceID,
		"metric":      metric,
		"period":      period,
		"aggregates":  out,
		"hourCount":   len(out),
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

	pools := s.pools[acct]
	charges := make([]billing.Charge, 0)
	// Settle every cached aggregate for resources owned by this account in the
	// requested period. The unit price comes from the snapshot, never the live
	// price (01§12.3 rule 4).
	for _, usage := range s.aggCache {
		if usage.AccountID != acct {
			continue
		}
		if usage.HourStart.Format("2006-01") != period {
			continue
		}
		productCode := s.products[usage.ResourceID]
		unitPrice, ok := s.unitPrices[usage.MeteringItem]
		if !ok {
			// No price snapshot for this metric: the charge cannot be computed
			// reproducibly, so skip rather than synthesize a price.
			continue
		}
		snap := s.snapshotIDs[usage.MeteringItem]
		engine := billing.NewEngine(s.now)
		sett, err := engine.Settle(usage, unitPrice, productCode, snap, pools)
		if err != nil {
			// A frozen period rejects settle; surface it but keep going.
			slog.Warn("settle skipped", "resource", usage.ResourceID, "hour", usage.HourStart, "err", err)
			continue
		}
		charges = append(charges, sett.Charge)
	}

	bill := billing.Summarize(acct, period, charges)
	writeJSON(w, "OK", billToMap(bill, charges))
}

func hourlyUsageToMap(u metering.HourlyUsage) map[string]any {
	return map[string]any{
		"aggId":          u.AggID,
		"resourceId":     u.ResourceID,
		"metric":         u.MeteringItem,
		"totalQuantity":  u.TotalQuantity.String(),
		"hourStart":      u.HourStart.Format(time.RFC3339),
		"coveredRatio":   u.CoveredRatio,
		"complete":       u.Complete(),
		"windowsSeen":    u.WindowsSeen,
		"windowsExpected": u.WindowsExpected,
		"batch":          u.BatchID,
	}
}

func billToMap(b billing.MonthlyBill, charges []billing.Charge) map[string]any {
	lines := make([]map[string]any, 0, len(charges))
	for _, c := range charges {
		deductions := make([]map[string]any, 0, len(c.Deductions))
		for _, d := range c.Deductions {
			deductions = append(deductions, map[string]any{
				"source": string(d.Source), "ref": d.Ref, "amount": d.Amount.String(),
			})
		}
		lines = append(lines, map[string]any{
			"chargeId":      c.ChargeID,
			"resourceId":    c.ResourceID,
			"productCode":   c.ProductCode,
			"metric":        c.MeteringItem,
			"quantity":      c.Quantity.String(),
			"unitPrice":     c.UnitPrice.String(),
			"pretaxAmount":  c.PretaxAmount.String(),
			"payAmount":     c.PayAmount.String(),
			"coveredRatio":  c.CoveredRatio,
			"incomplete":    c.Incomplete(),
			"reconciles":    c.Reconciles(),
			"snapshotId":    c.SnapshotID,
			"deductions":    deductions,
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
		// seeded hours are returned even when now() has moved past them.
		return !h.Before(ref.Add(-24*time.Hour)) && !h.After(ref)
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

// accountIDFrom extracts the caller's account id.
//
// TRUST BOUNDARY: X-Sc-Account-Id is trusted only because these routes are
// reachable exclusively via the APISIX gateway, which strips any
// client-supplied copy and injects the authenticated account. Deployments that
// cannot guarantee that network isolation should set SC_INTERNAL_TOKEN so
// callers must also present the shared X-Sc-Internal-Token secret.
func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if internalToken != "" && r.Header.Get("X-Sc-Internal-Token") != internalToken {
		writeErr(w, "Common.Forbidden", 403, "invalid internal token")
		return 0, false
	}
	raw := r.Header.Get(accountIDHeader)
	if raw == "" {
		writeErr(w, "Common.MissingAccountId", 403, "X-Sc-Account-Id header is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed X-Sc-Account-Id")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, code string, data any) {
	rid := w.Header().Get("X-Sc-TraceId")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Data": data})
}

func writeErr(w http.ResponseWriter, code string, status int, msg string) {
	rid := w.Header().Get("X-Sc-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Message": msg})
}

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Sc-TraceId")
		if id == "" {
			id = fmt.Sprintf("metering-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Sc-TraceId", id)
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
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	store := newMeteringStore()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/metering/ingest", store.handleIngest)
	mux.HandleFunc("GET /api/v1/metering/aggregate", store.handleAggregate)
	mux.HandleFunc("GET /api/v1/metering/bills", store.handleBills)

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(mux)), ReadHeaderTimeout: 5 * time.Second}
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
