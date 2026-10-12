package main

// Metrics query proxy: GET /api/v1/monitor/metrics (03§4.4.1).
//
// The series comes from a Prometheus-compatible HTTP API (Prometheus, or the
// VictoriaMetrics vmselect Prometheus endpoint) named by EULER_PROMETHEUS_URL:
// one query_range per request, the metric mapped to PromQL through
// metricQueries and narrowed to the resource by its resource_id label.
//
// Without a backend the answer depends on the environment: outside production
// a deterministic synthetic series keeps the console dashboards working and is
// flagged "source":"synthetic"; in production (pkg-go/runmode) the request is
// refused with 503 Monitor.MetricsUnavailable — a monitoring product must never
// draw invented data as if it were the customer's.
//
// Tenancy: the series is selected by resource id, so the resource must be the
// calling account's (X-Euler-Account-Id): owners.go asks the resource ledger
// (svc-orchestrator) and a resource that is not the caller's answers 404
// Monitor.ResourceNotFound, an unverifiable one 503 Monitor.OwnerLookupFailed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qifalab/euler-platform/runmode"
)

// prometheusURLEnv names the Prometheus HTTP API base URL (the part before
// /api/v1/query_range), e.g. http://prometheus.observability.svc:9090 or
// http://vmselect.observability.svc:8481/select/0/prometheus.
const prometheusURLEnv = "EULER_PROMETHEUS_URL"

// Query bounds. A request outside them is a 400, never silently clamped: the
// console must know that the chart it draws is the window it asked for.
const (
	defaultMetricMetric = "cpu_utilization"
	defaultMetricWindow = time.Hour
	defaultMetricPeriod = 60 * time.Second
	minMetricPeriod     = 10 * time.Second
	maxMetricPeriod     = 24 * time.Hour
	maxMetricRange      = 31 * 24 * time.Hour
	maxMetricPoints     = 1440
	// prometheusTimeout bounds one query_range round trip.
	prometheusTimeout = 10 * time.Second
	// maxPrometheusBody bounds the response read (one series of at most
	// maxMetricPoints samples is a few tens of KiB).
	maxPrometheusBody = 4 << 20
)

// resourceLabel is the series label carrying the platform resource id.
const resourceLabel = "resource_id"

// metricQueries maps each metric the rule templates define (handleTemplates)
// to its PromQL. %[1]s is the label matcher selecting the resource and %[2]s
// the range of one period (e.g. "60s"), so a counter turns into "per period"
// and every step is one point. Each query aggregates to a single series.
var metricQueries = map[string]string{
	"cpu_utilization":    `avg(euler_resource_cpu_utilization_percent{%[1]s})`,
	"memory_utilization": `avg(euler_resource_memory_utilization_percent{%[1]s})`,
	"request_count":      `sum(increase(euler_resource_requests_total{%[1]s}[%[2]s]))`,
}

// resourceIDPattern is what a platform resource id looks like
// ({product}-{region}-{shard}-{hex}); anything else is refused before it gets
// near a PromQL string.
var resourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// metricsBackend answers metric queries: from Prometheus when prom is set,
// otherwise synthetic (non-production) or 503 (production).
type metricsBackend struct {
	prom       *promClient
	production bool
	now        func() time.Time
}

// defaultMetricsBackend has no Prometheus; its production flag comes from
// runmode, so a store built without newMetricsBackendFromEnv still never
// serves synthetic data in production.
func defaultMetricsBackend() *metricsBackend {
	return &metricsBackend{production: runmode.Production(), now: time.Now}
}

// newMetricsBackendFromEnv reads EULER_PROMETHEUS_URL. A set but unusable
// URL is a startup error rather than a silent fallback to synthetic data.
func newMetricsBackendFromEnv() (*metricsBackend, error) {
	b := defaultMetricsBackend()
	raw := strings.TrimSpace(os.Getenv(prometheusURLEnv))
	if raw == "" {
		if b.production {
			slog.Warn("metrics backend not configured: "+prometheusURLEnv+" is unset; /api/v1/monitor/metrics answers 503", "env", runmode.Env())
		} else {
			slog.Warn("metrics backend not configured: "+prometheusURLEnv+" is unset; serving synthetic series (non-production only)", "env", runmode.Env())
		}
		return b, nil
	}
	prom, err := newPromClient(raw)
	if err != nil {
		return nil, err
	}
	b.prom = prom
	return b, nil
}

// promClient is a minimal Prometheus HTTP API client (query_range only).
type promClient struct {
	base string // scheme://host[:port][/prefix], no trailing slash
	http *http.Client
}

func newPromClient(raw string) (*promClient, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s must be an http(s) base URL without query, got %q", prometheusURLEnv, raw)
	}
	return &promClient{
		base: strings.TrimRight(u.String(), "/"),
		http: &http.Client{Timeout: prometheusTimeout},
	}, nil
}

// metricPoint is one sample of the answered series.
type metricPoint struct {
	Timestamp string  `json:"timestamp"` // RFC3339, UTC
	Value     float64 `json:"value"`
}

// promRangeResponse is the query_range body
// ({"status","data":{"resultType":"matrix","result":[{"metric","values"}]}}).
type promRangeResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// queryRange runs one PromQL range query and returns its (single) series;
// no matching series is an empty slice, not an error.
func (c *promClient) queryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]metricPoint, error) {
	q := url.Values{
		"query": {query},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(step/time.Second), 10)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPrometheusBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPrometheusBody {
		return nil, errors.New("prometheus response exceeds the size bound")
	}
	var pr promRangeResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("prometheus status %d, undecodable body: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || pr.Status != "success" {
		return nil, fmt.Errorf("prometheus status %d: %s: %s", resp.StatusCode, pr.ErrorType, pr.Error)
	}
	if pr.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("prometheus resultType %q, want matrix", pr.Data.ResultType)
	}
	points := make([]metricPoint, 0)
	if len(pr.Data.Result) == 0 {
		return points, nil
	}
	if len(pr.Data.Result) > 1 {
		// Every mapped query aggregates to one series; more is a mapping bug.
		return nil, fmt.Errorf("prometheus returned %d series, want 1", len(pr.Data.Result))
	}
	for _, v := range pr.Data.Result[0].Values {
		var ts float64
		var raw string
		if err := json.Unmarshal(v[0], &ts); err != nil {
			return nil, fmt.Errorf("prometheus sample timestamp: %w", err)
		}
		if err := json.Unmarshal(v[1], &raw); err != nil {
			return nil, fmt.Errorf("prometheus sample value: %w", err)
		}
		val, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("prometheus sample value %q: %w", raw, err)
		}
		if math.IsNaN(val) || math.IsInf(val, 0) {
			continue // no JSON encoding; a gap in the chart is the honest rendering
		}
		sec, frac := math.Modf(ts)
		points = append(points, metricPoint{
			Timestamp: time.Unix(int64(sec), int64(frac*1e9)).UTC().Format(time.RFC3339),
			Value:     val,
		})
	}
	return points, nil
}

// metricQuery is a validated GET /api/v1/monitor/metrics request.
type metricQuery struct {
	metric     string
	resourceID string
	start, end time.Time
	period     time.Duration
}

// promQL renders the query for the resource. The id already matched
// resourceIDPattern; it is still quoted as a PromQL string literal.
func (q metricQuery) promQL() string {
	matcher := resourceLabel + "=" + strconv.Quote(q.resourceID)
	return fmt.Sprintf(metricQueries[q.metric], matcher, strconv.FormatInt(int64(q.period/time.Second), 10)+"s")
}

// supportedMetrics lists metricQueries' keys, sorted (for error messages).
func supportedMetrics() []string {
	out := make([]string, 0, len(metricQueries))
	for m := range metricQueries {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// parseMetricTime accepts RFC3339 or a Unix timestamp in seconds (or in
// milliseconds when it has 13+ digits, the OpenAPI convention).
func parseMetricTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return time.Time{}, errors.New("must be RFC3339 or a Unix timestamp")
	}
	if n >= 1e12 {
		return time.UnixMilli(n).UTC(), nil
	}
	return time.Unix(n, 0).UTC(), nil
}

// parseMetricQuery validates the query string against the bounds above.
func parseMetricQuery(v url.Values, now time.Time) (metricQuery, error) {
	q := metricQuery{metric: strings.TrimSpace(v.Get("metric")), resourceID: strings.TrimSpace(v.Get("resourceId"))}
	if q.metric == "" {
		q.metric = defaultMetricMetric
	}
	if _, ok := metricQueries[q.metric]; !ok {
		return q, fmt.Errorf("metric must be one of %s", strings.Join(supportedMetrics(), ", "))
	}
	if q.resourceID == "" {
		return q, errors.New("resourceId is required")
	}
	if !resourceIDPattern.MatchString(q.resourceID) {
		return q, errors.New("resourceId is malformed")
	}

	q.period = defaultMetricPeriod
	if raw := strings.TrimSpace(v.Get("period")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < int64(minMetricPeriod/time.Second) || n > int64(maxMetricPeriod/time.Second) {
			return q, fmt.Errorf("period must be whole seconds between %d and %d", int64(minMetricPeriod/time.Second), int64(maxMetricPeriod/time.Second))
		}
		q.period = time.Duration(n) * time.Second
	}

	q.end = now.UTC().Truncate(time.Second)
	if raw := strings.TrimSpace(v.Get("end")); raw != "" {
		t, err := parseMetricTime(raw)
		if err != nil {
			return q, fmt.Errorf("end %s", err)
		}
		q.end = t
	}
	q.start = q.end.Add(-defaultMetricWindow)
	if raw := strings.TrimSpace(v.Get("start")); raw != "" {
		t, err := parseMetricTime(raw)
		if err != nil {
			return q, fmt.Errorf("start %s", err)
		}
		q.start = t
	}
	if !q.start.Before(q.end) {
		return q, errors.New("start must be before end")
	}
	if q.end.Sub(q.start) > maxMetricRange {
		return q, fmt.Errorf("end - start must not exceed %d days", int(maxMetricRange/(24*time.Hour)))
	}
	if q.pointCount() > maxMetricPoints {
		return q, fmt.Errorf("range/period yields more than %d points; widen period", maxMetricPoints)
	}
	return q, nil
}

// pointCount is how many steps fall in [start, end] — what query_range
// evaluates and what the synthetic series draws.
func (q metricQuery) pointCount() int {
	return int(q.end.Sub(q.start)/q.period) + 1
}

// synthetic draws a deterministic stand-in series over the requested steps:
// stable per (metric, resource, timestamp), so a refresh does not reshuffle
// the chart and two resources do not look identical.
func (q metricQuery) synthetic() []metricPoint {
	h := fnv.New32a()
	_, _ = h.Write([]byte(q.metric + "\x00" + q.resourceID))
	seed := int64(h.Sum32() % 60)
	n := q.pointCount()
	points := make([]metricPoint, 0, n)
	for i := 0; i < n; i++ {
		ts := q.start.Add(time.Duration(i) * q.period)
		step := ts.Unix() / int64(q.period/time.Second)
		points = append(points, metricPoint{
			Timestamp: ts.UTC().Format(time.RFC3339),
			Value:     float64(20 + (step*7+seed)%60),
		})
	}
	return points
}

// handleQueryMetrics serves GET /api/v1/monitor/metrics:
//
//	?metric=cpu_utilization|memory_utilization|request_count (default cpu_utilization)
//	&resourceId=<id> (required)
//	&start=&end= (RFC3339 or Unix seconds/ms; default the last hour)
//	&period=<seconds> (step, 10..86400; default 60)
//
// Data: {metric, resourceId, start, end, period, source, points:[{timestamp, value}]}
// with source "prometheus" or "synthetic".
//
// Errors: 400 Common.InvalidParameter (query), 404 Monitor.ResourceNotFound
// (not the caller's resource), 503 Monitor.OwnerLookupFailed (ownership could
// not be verified) or Monitor.MetricsUnavailable (no / failing backend).
func (s *ruleStore) handleQueryMetrics(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	b := s.metrics
	if b == nil {
		b = defaultMetricsBackend()
	}
	now := time.Now
	if b.now != nil {
		now = b.now
	}
	q, err := parseMetricQuery(r.URL.Query(), now())
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, err.Error())
		return
	}
	owners := s.owners
	if owners == nil {
		owners = defaultResourceOwnership()
	}
	if err := owners.check(r.Context(), acct, q.resourceID); err != nil {
		if errors.Is(err, errResourceNotOwned) {
			writeErr(w, "Monitor.ResourceNotFound", 404, "资源不存在")
			return
		}
		slog.Error("resource ownership lookup failed", "account", acct, "resourceId", q.resourceID, "err", err)
		writeErr(w, "Monitor.OwnerLookupFailed", 503, "资源归属暂无法校验,请稍后重试")
		return
	}

	var (
		points []metricPoint
		source string
	)
	switch {
	case b.prom != nil:
		points, err = b.prom.queryRange(r.Context(), q.promQL(), q.start, q.end, q.period)
		if err != nil {
			slog.Error("metrics query failed", "account", acct, "metric", q.metric, "resourceId", q.resourceID, "err", err)
			writeErr(w, "Monitor.MetricsUnavailable", 503, "监控数据暂不可用,请稍后重试")
			return
		}
		source = "prometheus"
	case b.production:
		writeErr(w, "Monitor.MetricsUnavailable", 503, "监控数据源未配置")
		return
	default:
		points, source = q.synthetic(), "synthetic"
	}
	writeJSON(w, "OK", map[string]any{
		"metric":     q.metric,
		"resourceId": q.resourceID,
		"start":      q.start.Format(time.RFC3339),
		"end":        q.end.Format(time.RFC3339),
		"period":     int64(q.period / time.Second),
		"source":     source,
		"points":     points,
	})
}
