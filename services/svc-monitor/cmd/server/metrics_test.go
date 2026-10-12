package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

var metricsNow = time.Date(2026, 10, 9, 12, 0, 30, 0, time.UTC)

type metricsReply struct {
	status int
	Code   string `json:"Code"`
	Data   struct {
		Metric     string        `json:"metric"`
		ResourceID string        `json:"resourceId"`
		Start      string        `json:"start"`
		End        string        `json:"end"`
		Period     int64         `json:"period"`
		Source     string        `json:"source"`
		Points     []metricPoint `json:"points"`
	} `json:"Data"`
}

func queryMetrics(t *testing.T, b *metricsBackend, query string) metricsReply {
	t.Helper()
	s := newRuleStoreWith(newMemRuleRepo())
	s.metrics = b
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/monitor/metrics", s.handleQueryMetrics)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/metrics?"+query, nil)
	req.Header.Set(accountIDHeader, "100123")
	rr := httptest.NewRecorder()
	recoverMiddleware(requestIDMiddleware(mux)).ServeHTTP(rr, req)
	var out metricsReply
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	out.status = rr.Code
	return out
}

func syntheticBackend() *metricsBackend {
	return &metricsBackend{now: func() time.Time { return metricsNow }}
}

// fakePrometheus serves /api/v1/query_range with reply and records the
// requests' query strings.
type fakePrometheus struct {
	mu    sync.Mutex
	asked []url.Values
	paths []string
	reply func(w http.ResponseWriter, q url.Values)
}

func (f *fakePrometheus) start(t *testing.T, prefix string) *metricsBackend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.asked = append(f.asked, r.URL.Query())
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		f.reply(w, r.URL.Query())
	}))
	t.Cleanup(srv.Close)
	prom, err := newPromClient(srv.URL + prefix + "/")
	if err != nil {
		t.Fatal(err)
	}
	return &metricsBackend{prom: prom, now: func() time.Time { return metricsNow }}
}

func matrix(values ...[2]any) map[string]any {
	return map[string]any{"status": "success", "data": map[string]any{
		"resultType": "matrix",
		"result":     []any{map[string]any{"metric": map[string]string{"resource_id": "x"}, "values": values}},
	}}
}

// Every metric the rule templates offer must be queryable.
func TestEveryTemplateMetricHasAQuery(t *testing.T) {
	rr := httptest.NewRecorder()
	newRuleStore().handleTemplates(rr, httptest.NewRequest(http.MethodGet, "/api/v1/monitor/templates", nil))
	var env struct {
		Data []struct {
			Metric string `json:"metric"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || len(env.Data) == 0 {
		t.Fatalf("templates: %v %s", err, rr.Body.String())
	}
	for _, tpl := range env.Data {
		if _, ok := metricQueries[tpl.Metric]; !ok {
			t.Errorf("template metric %q has no PromQL mapping", tpl.Metric)
		}
	}
}

func TestMetricsPrometheusQueryRange(t *testing.T) {
	f := &fakePrometheus{reply: func(w http.ResponseWriter, q url.Values) {
		_ = json.NewEncoder(w).Encode(matrix(
			[2]any{1791540000, "12.5"},
			[2]any{1791540300, "NaN"}, // a gap, not a JSON error
			[2]any{1791540600.5, "40"},
		))
	}}
	b := f.start(t, "/select/0/prometheus")

	got := queryMetrics(t, b, "metric=request_count&resourceId=euoss-cn-north-1-01-a1b2&start=2026-10-09T10:00:00Z&end=1791540600&period=300")
	if got.status != 200 || got.Code != "OK" {
		t.Fatalf("status %d %s", got.status, got.Code)
	}
	if got.Data.Source != "prometheus" || got.Data.Metric != "request_count" || got.Data.ResourceID != "euoss-cn-north-1-01-a1b2" ||
		got.Data.Period != 300 || got.Data.Start != "2026-10-09T10:00:00Z" || got.Data.End != "2026-10-09T10:10:00Z" {
		t.Fatalf("data = %+v", got.Data)
	}
	want := []metricPoint{{"2026-10-09T10:00:00Z", 12.5}, {"2026-10-09T10:10:00Z", 40}}
	if len(got.Data.Points) != len(want) || got.Data.Points[0] != want[0] || got.Data.Points[1] != want[1] {
		t.Fatalf("points = %+v, want %+v", got.Data.Points, want)
	}

	if len(f.asked) != 1 {
		t.Fatalf("prometheus asked %d times", len(f.asked))
	}
	q := f.asked[0]
	if f.paths[0] != "/select/0/prometheus/api/v1/query_range" {
		t.Fatalf("path = %q (base URL prefix and trailing slash must be kept/trimmed)", f.paths[0])
	}
	if q.Get("query") != `sum(increase(euler_resource_requests_total{resource_id="euoss-cn-north-1-01-a1b2"}[300s]))` ||
		q.Get("start") != "1791540000" || q.Get("end") != "1791540600" || q.Get("step") != "300" {
		t.Fatalf("query_range params = %v", q)
	}
}

func TestMetricsPrometheusDefaults(t *testing.T) {
	f := &fakePrometheus{reply: func(w http.ResponseWriter, q url.Values) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{}}})
	}}
	got := queryMetrics(t, f.start(t, ""), "resourceId=euecs-cn-north-1-01-a1")
	if got.status != 200 || got.Data.Source != "prometheus" || got.Data.Metric != "cpu_utilization" || got.Data.Period != 60 {
		t.Fatalf("reply = %d %+v", got.status, got.Data)
	}
	// No series is an empty chart, not an error (and never null).
	if got.Data.Points == nil || len(got.Data.Points) != 0 {
		t.Fatalf("points = %#v, want []", got.Data.Points)
	}
	q := f.asked[0]
	if q.Get("query") != `avg(euler_resource_cpu_utilization_percent{resource_id="euecs-cn-north-1-01-a1"})` ||
		q.Get("end") != "1791547230" || q.Get("start") != "1791543630" || q.Get("step") != "60" {
		t.Fatalf("query_range params = %v (default: the last hour at 60s)", q)
	}
}

func TestMetricsPrometheusFailureIs503(t *testing.T) {
	for name, reply := range map[string]func(w http.ResponseWriter, q url.Values){
		"server error": func(w http.ResponseWriter, _ url.Values) { w.WriteHeader(500); _, _ = w.Write([]byte("boom")) },
		"query error": func(w http.ResponseWriter, _ url.Values) {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "errorType": "bad_data", "error": "parse error"})
		},
		"two series": func(w http.ResponseWriter, _ url.Values) {
			m := matrix([2]any{1791547200, "1"})
			res := m["data"].(map[string]any)["result"].([]any)
			m["data"].(map[string]any)["result"] = append(res, res[0])
			_ = json.NewEncoder(w).Encode(m)
		},
		"not a matrix": func(w http.ResponseWriter, _ url.Values) {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := queryMetrics(t, (&fakePrometheus{reply: reply}).start(t, ""), "resourceId=r-1")
			if got.status != 503 || got.Code != "Monitor.MetricsUnavailable" {
				t.Fatalf("%d %s, want 503 Monitor.MetricsUnavailable", got.status, got.Code)
			}
		})
	}
	// Unreachable backend.
	prom, _ := newPromClient("http://127.0.0.1:1")
	got := queryMetrics(t, &metricsBackend{prom: prom, now: func() time.Time { return metricsNow }}, "resourceId=r-1")
	if got.status != 503 || got.Code != "Monitor.MetricsUnavailable" {
		t.Fatalf("unreachable: %d %s", got.status, got.Code)
	}
}

func TestMetricsSyntheticHonoursQuery(t *testing.T) {
	got := queryMetrics(t, syntheticBackend(), "metric=memory_utilization&resourceId=euecs-a&start=2026-10-09T10:00:00Z&end=2026-10-09T10:10:00Z&period=120")
	if got.status != 200 || got.Data.Source != "synthetic" || got.Data.ResourceID != "euecs-a" || got.Data.Period != 120 {
		t.Fatalf("reply = %d %+v", got.status, got.Data)
	}
	if len(got.Data.Points) != 6 || got.Data.Points[0].Timestamp != "2026-10-09T10:00:00Z" || got.Data.Points[5].Timestamp != "2026-10-09T10:10:00Z" {
		t.Fatalf("points = %+v, want 10:00..10:10 every 2m", got.Data.Points)
	}
	for _, p := range got.Data.Points {
		if p.Value < 0 || p.Value > 100 {
			t.Fatalf("synthetic value out of range: %+v", p)
		}
	}
	again := queryMetrics(t, syntheticBackend(), "metric=memory_utilization&resourceId=euecs-a&start=2026-10-09T10:00:00Z&end=2026-10-09T10:10:00Z&period=120")
	other := queryMetrics(t, syntheticBackend(), "metric=memory_utilization&resourceId=euecs-b&start=2026-10-09T10:00:00Z&end=2026-10-09T10:10:00Z&period=120")
	same, differs := true, false
	for i := range got.Data.Points {
		same = same && got.Data.Points[i] == again.Data.Points[i]
		differs = differs || got.Data.Points[i].Value != other.Data.Points[i].Value
	}
	if !same || !differs {
		t.Fatalf("synthetic series must be stable per resource and differ across resources (same=%v differs=%v)", same, differs)
	}
	// Defaults: the last hour at 60s, 61 points ending at now.
	def := queryMetrics(t, syntheticBackend(), "resourceId=euecs-a")
	if len(def.Data.Points) != 61 || def.Data.End != "2026-10-09T12:00:30Z" || def.Data.Start != "2026-10-09T11:00:30Z" {
		t.Fatalf("default window = %s..%s, %d points", def.Data.Start, def.Data.End, len(def.Data.Points))
	}
}

func TestMetricsProductionWithoutBackendIs503(t *testing.T) {
	b := syntheticBackend()
	b.production = true
	got := queryMetrics(t, b, "resourceId=euecs-a")
	if got.status != 503 || got.Code != "Monitor.MetricsUnavailable" {
		t.Fatalf("%d %s, want 503 Monitor.MetricsUnavailable", got.status, got.Code)
	}
	// The environment decides for the default backend too.
	t.Setenv("EULER_ENV", "prod")
	t.Setenv(prometheusURLEnv, "")
	env, err := newMetricsBackendFromEnv()
	if err != nil || env.prom != nil || !env.production {
		t.Fatalf("prod backend = %+v, %v", env, err)
	}
	if !defaultMetricsBackend().production {
		t.Fatal("default backend serves synthetic data in production")
	}
}

func TestMetricsBackendFromEnv(t *testing.T) {
	t.Setenv("EULER_ENV", "dev")
	for _, bad := range []string{"ftp://prom:9090", "prometheus:9090", "http://", "http://prom:9090/?x=1"} {
		t.Setenv(prometheusURLEnv, bad)
		if _, err := newMetricsBackendFromEnv(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	t.Setenv(prometheusURLEnv, " http://vmselect:8481/select/0/prometheus/ ")
	b, err := newMetricsBackendFromEnv()
	if err != nil || b.prom == nil || b.prom.base != "http://vmselect:8481/select/0/prometheus" {
		t.Fatalf("backend = %+v, %v", b, err)
	}
	t.Setenv(prometheusURLEnv, "")
	if b, err := newMetricsBackendFromEnv(); err != nil || b.prom != nil || b.production {
		t.Fatalf("unset outside production = %+v, %v", b, err)
	}
}

func TestMetricsRejectsBadQueries(t *testing.T) {
	for name, query := range map[string]string{
		"unknown metric":     "metric=disk_magic&resourceId=r-1",
		"no resource":        "metric=cpu_utilization",
		"promql injection":   "resourceId=" + url.QueryEscape(`r"}or vector(1)#`),
		"period not integer": "resourceId=r-1&period=1m",
		"period too small":   "resourceId=r-1&period=5",
		"period too large":   "resourceId=r-1&period=86401",
		"bad start":          "resourceId=r-1&start=yesterday",
		"bad end":            "resourceId=r-1&end=-5",
		"start after end":    "resourceId=r-1&start=2026-10-09T11:00:00Z&end=2026-10-09T10:00:00Z",
		"start equals end":   "resourceId=r-1&start=2026-10-09T10:00:00Z&end=2026-10-09T10:00:00Z",
		"range too long":     "resourceId=r-1&start=2026-08-01T00:00:00Z&end=2026-10-09T00:00:00Z&period=86400",
		"too many points":    "resourceId=r-1&start=2026-10-01T00:00:00Z&end=2026-10-09T00:00:00Z&period=60",
	} {
		f := &fakePrometheus{reply: func(w http.ResponseWriter, _ url.Values) { t.Errorf("%s reached prometheus", name) }}
		for _, b := range []*metricsBackend{syntheticBackend(), f.start(t, "")} {
			if got := queryMetrics(t, b, query); got.status != 400 || got.Code != "Common.InvalidParameter" {
				t.Errorf("%s: %d %s, want 400 Common.InvalidParameter", name, got.status, got.Code)
			}
		}
	}
	// Millisecond timestamps (13 digits) are accepted, as OpenAPI clients send them.
	got := queryMetrics(t, syntheticBackend(), "resourceId=r-1&start=1791540000000&end=1791540600000&period=60")
	if got.status != 200 || got.Data.Start != "2026-10-09T10:00:00Z" || len(got.Data.Points) != 11 {
		t.Fatalf("ms timestamps: %d %+v", got.status, got.Data)
	}
	if !strings.Contains(strings.Join(supportedMetrics(), ","), "cpu_utilization") {
		t.Fatal("supportedMetrics lost cpu_utilization")
	}
}
