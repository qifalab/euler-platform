// Package main: svc-monitor — monitoring & alerting product (03§4.4.1).
//
// svc-monitor manages tenant alert rules (CRUD) and acts as a metrics query
// proxy (rule evaluation itself is alert-engine's job, 03§4.4.5). This is the
// user-facing service backing the SCMON console and the console-monitor
// sub-app. Phase-1 keeps an in-memory store seeded with demo rules; the DDL in
// sql/ is the production shape.
//
// Routes (gateway-authorized, X-Sc-Account-Id injected):
//
//	GET    /api/v1/monitor/rules             — list the account's alert rules
//	POST   /api/v1/monitor/rules             — create a rule (idempotent on account+product+metric)
//	PUT    /api/v1/monitor/rules/{id}        — update a rule (optimistic version)
//	DELETE /api/v1/monitor/rules/{id}        — delete a rule
//	GET    /api/v1/monitor/metrics           — query a metric for a resource (query proxy)
//	GET    /api/v1/monitor/templates         — list platform rule templates
//	GET    /api/v1/monitor/slo               — M-9 SLO dashboard: error budget, burn rates, release policy
//	GET    /api/v1/monitor/chaos             — M-9 chaos drills: six mandatory subjects + remediation verdicts
//
// stdlib-HTTP service (repo convention). Every handler emits the platform
// envelope {RequestId, Code, Message, Data} (03§9.3).
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
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/starcloud/sc-platform/chaos"
	"github.com/starcloud/sc-platform/slo"
)

const accountIDHeader = "X-Sc-Account-Id"

// maxBodyBytes caps JSON request bodies.
const maxBodyBytes = 1 << 20 // 1 MiB

// Comparison operator values follow proto-hub
// proto/starcloud/monitor/v1/monitor.proto ComparisonOperator (lines 46-52):
// 1 = GREATER_THAN (>), 2 = GREATER_THAN_OR_EQUAL (≥),
// 3 = LESS_THAN (<),    4 = LESS_THAN_OR_EQUAL (≤).
const (
	cmpGreaterThan        = 1
	cmpGreaterThanOrEqual = 2
	cmpLessThan           = 3
	cmpLessThanOrEqual    = 4
)

// comparisonSymbol renders a proto ComparisonOperator value; empty string for
// unknown/unspecified values.
func comparisonSymbol(op int) string {
	switch op {
	case cmpGreaterThan:
		return ">"
	case cmpGreaterThanOrEqual:
		return ">="
	case cmpLessThan:
		return "<"
	case cmpLessThanOrEqual:
		return "<="
	default:
		return ""
	}
}

// validComparisonOperator reports whether op is a defined (non-UNSPECIFIED)
// proto ComparisonOperator.
func validComparisonOperator(op int) bool {
	return op >= cmpGreaterThan && op <= cmpLessThanOrEqual
}

// AlertRule mirrors alert_rule (sql/V1__support_db_monitor_schema.sql).
type AlertRule struct {
	RuleID                 int64             `json:"ruleId"`
	AccountID              int64             `json:"accountId"`
	ProductCode            string            `json:"productCode"`
	ResourceType           string            `json:"resourceType"`
	Metric                 string            `json:"metric"`
	Threshold              string            `json:"threshold"` // DECIMAL as string
	ComparisonOperator     int               `json:"comparisonOperator"` // proto ComparisonOperator: 1> 2≥ 3< 4≤ (monitor.proto:46-52)
	Period                 int               `json:"period"`             // seconds
	EvalPeriods            int               `json:"evalPeriods"`
	NotificationChannels   []string          `json:"notificationChannels"`
	Status                 int               `json:"status"` // 1 enabled 2 disabled
	Version                int               `json:"version"`
	CreatedAt              time.Time         `json:"createdAt"`
	UpdatedAt              time.Time         `json:"updatedAt"`
}

type ruleStore struct {
	mu    sync.RWMutex
	rules map[int64]*AlertRule // ruleId → rule
	seq   int64
}

func newRuleStore() *ruleStore {
	s := &ruleStore{rules: make(map[int64]*AlertRule)}
	// Seeds use proto operator semantics: "cpu ≥ 80" → 2 (GTE), "req > 1000" → 1 (GT).
	s.seed(100123, "scecs", "instance", "cpu_utilization", "80.0000", cmpGreaterThanOrEqual, 60, 1, []string{"IN_APP", "EMAIL"})
	s.seed(100123, "scecs", "instance", "memory_utilization", "90.0000", cmpGreaterThanOrEqual, 60, 1, []string{"IN_APP"})
	s.seed(100123, "scoss", "bucket", "request_count", "1000.0000", cmpGreaterThan, 300, 2, []string{"SMS"})
	return s
}

func (s *ruleStore) seed(acct int64, product, rtype, metric, threshold string, cmp, period, evalP int, channels []string) {
	s.seq++
	r := &AlertRule{
		RuleID: s.seq, AccountID: acct, ProductCode: product, ResourceType: rtype,
		Metric: metric, Threshold: threshold, ComparisonOperator: cmp, Period: period,
		EvalPeriods: evalP, NotificationChannels: channels, Status: 1, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.rules[r.RuleID] = r
}

func (s *ruleStore) list(acct int64) []*AlertRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*AlertRule
	for _, r := range s.rules {
		if r.AccountID == acct {
			out = append(out, r)
		}
	}
	return out
}

// --- HTTP helpers (envelope, 03§9.3) ---

func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	// TRUST NOTE: X-Sc-Account-Id is injected by the API gateway after
	// authentication; this service relies on network isolation (and optionally
	// internalTokenMiddleware) rather than re-authenticating.
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

// --- handlers ---

func (s *ruleStore) handleListRules(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	writeJSON(w, "OK", s.list(acct))
}

type createRuleRequest struct {
	ProductCode          string   `json:"productCode"`
	ResourceType         string   `json:"resourceType"`
	Metric               string   `json:"metric"`
	Threshold            string   `json:"threshold"`
	ComparisonOperator   int      `json:"comparisonOperator"`
	Period               int      `json:"period"`
	EvalPeriods          int      `json:"evalPeriods"`
	NotificationChannels []string `json:"notificationChannels"`
}

func (s *ruleStore) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req createRuleRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.ProductCode == "" || req.Metric == "" || req.Threshold == "" {
		writeErr(w, "Common.InvalidParameter", 400, "productCode, metric, threshold are required")
		return
	}
	if req.Period == 0 {
		req.Period = 60
	}
	if req.EvalPeriods == 0 {
		req.EvalPeriods = 1
	}
	if req.ComparisonOperator == 0 {
		// Default "≥ threshold" per proto: GREATER_THAN_OR_EQUAL = 2.
		req.ComparisonOperator = cmpGreaterThanOrEqual
	}
	if !validComparisonOperator(req.ComparisonOperator) {
		writeErr(w, "Common.InvalidParameter", 400, "comparisonOperator must be 1(>) 2(>=) 3(<) 4(<=)")
		return
	}
	if len(req.NotificationChannels) == 0 {
		req.NotificationChannels = []string{"IN_APP"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Idempotent: uk_acc_rule (account, product, resource_type, metric) — upsert.
	for _, existing := range s.rules {
		if existing.AccountID == acct && existing.ProductCode == req.ProductCode &&
			existing.ResourceType == req.ResourceType && existing.Metric == req.Metric {
			existing.Threshold = req.Threshold
			existing.ComparisonOperator = req.ComparisonOperator
			existing.Period = req.Period
			existing.EvalPeriods = req.EvalPeriods
			existing.NotificationChannels = req.NotificationChannels
			existing.Version++
			existing.UpdatedAt = time.Now()
			writeJSON(w, "OK", existing)
			return
		}
	}
	s.seq++
	rule := &AlertRule{
		RuleID: s.seq, AccountID: acct, ProductCode: req.ProductCode, ResourceType: req.ResourceType,
		Metric: req.Metric, Threshold: req.Threshold, ComparisonOperator: req.ComparisonOperator,
		Period: req.Period, EvalPeriods: req.EvalPeriods, NotificationChannels: req.NotificationChannels,
		Status: 1, Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.rules[rule.RuleID] = rule
	writeJSON(w, "OK", rule)
}

func (s *ruleStore) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "invalid rule id")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rule, exists := s.rules[id]
	if !exists || rule.AccountID != acct {
		writeErr(w, "Monitor.RuleNotFound", 404, "告警规则不存在")
		return
	}
	delete(s.rules, id)
	writeJSON(w, "OK", map[string]any{"ruleId": id, "deleted": true})
}

// handleQueryMetrics is the metrics query proxy (03§4.4.1). Phase-1 returns
// synthetic series points so the console-monitor dashboard renders; prod fans
// out to the tenant VictoriaMetrics cluster.
func (s *ruleStore) handleQueryMetrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDFrom(w, r); !ok {
		return
	}
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "cpu_utilization"
	}
	// Synthetic 12-point series (prod: query tenant VM cluster).
	points := make([]map[string]any, 12)
	now := time.Now()
	for i := 0; i < 12; i++ {
		ts := now.Add(time.Duration(-11+i) * time.Minute)
		// Deterministic-ish value varying by index (no Math.random in this context).
		val := 20 + (i*7)%60
		points[i] = map[string]any{"timestamp": ts.Format(time.RFC3339), "value": val}
	}
	writeJSON(w, "OK", map[string]any{"metric": metric, "points": points})
}

func (s *ruleStore) handleTemplates(w http.ResponseWriter, r *http.Request) {
	// alert_rule_template — platform preset rules (03§4.4.1).
	templates := []map[string]any{
		{"templateId": 1, "productCode": "scecs", "metric": "cpu_utilization", "threshold": "80", "period": 60},
		{"templateId": 2, "productCode": "scecs", "metric": "memory_utilization", "threshold": "90", "period": 60},
		{"templateId": 3, "productCode": "scoss", "metric": "request_count", "threshold": "1000", "period": 300},
	}
	writeJSON(w, "OK", templates)
}

// --- M-9 stability platform: SLO + chaos (09-roadmap §5.2 M-9, 08§9.5/§10) -----

// sloObservation is the observed budget consumption for one objective: the
// 1h/3d multi-window burn inputs plus the total consumed over the SLO window
// and the consecutive quarters the SLO has been met. Production feeds this from
// the metrics pipeline; the verdicts below are always computed by pkg-go/slo.
type sloObservation struct {
	consumed1h    time.Duration
	consumed3d    time.Duration
	consumedTotal time.Duration
	quartersMet   int
}

// sloObservations seeds one observation per PlatformObjectives entry, keyed by
// objective name. The mix is deliberately varied: metering is in fast burn
// (pages) with its budget exhausted (release freeze), billing is in slow burn
// with a SLOW_DOWN release policy, and the gateway/login objectives have two
// clean quarters (SLA-gate eligible).
var sloObservations = map[string]sloObservation{
	"openapi-gateway-availability":   {consumed1h: 1200 * time.Millisecond, consumed3d: 90 * time.Second, consumedTotal: 400 * time.Second, quartersMet: 2},
	"resource-control-plane-success": {consumed1h: 3 * time.Second, consumed3d: 200 * time.Second, consumedTotal: 900 * time.Second, quartersMet: 1},
	"metering-no-loss":               {consumed1h: 60 * time.Second, consumed3d: 80 * time.Second, consumedTotal: 280 * time.Second, quartersMet: 0},
	"billing-on-time":                {consumed1h: 10 * time.Second, consumed3d: 4000 * time.Second, consumedTotal: 7800 * time.Second, quartersMet: 1},
	"login-auth-success":             {consumed1h: 800 * time.Millisecond, consumed3d: 60 * time.Second, consumedTotal: 300 * time.Second, quartersMet: 2},
}

// handleSLO returns the M-9 SLO dashboard feed: every platform objective with
// its error budget, multi-window burn rates, alert severity, release policy,
// and the SLA gate verdict. All math is pkg-go/slo's — this handler assembles.
func (s *ruleStore) handleSLO(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDFrom(w, r); !ok {
		return
	}
	objectives := make([]map[string]any, 0, len(slo.PlatformObjectives))
	for _, o := range slo.PlatformObjectives {
		if err := o.Validate(); err != nil {
			writeErr(w, "Common.InternalError", 500, err.Error())
			return
		}
		obs := sloObservations[o.Name]
		budget := o.ErrorBudget()
		remaining := budget - obs.consumedTotal
		objectives = append(objectives, map[string]any{
			"name":           o.Name,
			"target":         o.Target,
			"windowMs":       o.Window.Milliseconds(),
			"errorBudgetMs":  budget.Milliseconds(),
			"consumed1hMs":   obs.consumed1h.Milliseconds(),
			"consumed3dMs":   obs.consumed3d.Milliseconds(),
			"consumedTotalMs": obs.consumedTotal.Milliseconds(),
			"remainingMs":    remaining.Milliseconds(),
			"burnRate1h":     o.BurnRate(obs.consumed1h, slo.FastBurnWindow),
			"burnRate3d":     o.BurnRate(obs.consumed3d, slo.SlowBurnWindow),
			"alert1h":        string(o.Alert(obs.consumed1h, slo.FastBurnWindow)),
			"alert3d":        string(o.Alert(obs.consumed3d, slo.SlowBurnWindow)),
			"budgetPolicy":   string(o.BudgetPolicy(remaining)),
			"quartersMet":    obs.quartersMet,
			"slaEligible":    slo.CanCommitSLA(obs.quartersMet),
		})
	}
	writeJSON(w, "OK", map[string]any{
		"objectives": objectives,
		"slaGate": map[string]any{
			"consecutiveQuartersRequired": 2,
			"note":                        "SLA 仅对连续两个季度满足 SLO 的服务开放 (08§10.1)",
		},
		"thresholds": map[string]any{
			"fastBurnWindowMs":   slo.FastBurnWindow.Milliseconds(),
			"fastBurnThreshold":  slo.FastBurnThreshold,
			"slowBurnWindowMs":   slo.SlowBurnWindow.Milliseconds(),
			"slowBurnThreshold":  slo.SlowBurnThreshold,
		},
	})
}

// chaosDrill is one seeded drill entry: the plan (subject/stage/radius/abort
// path) plus the expected-vs-actual recovery result of its last execution.
type chaosDrill struct {
	plan   chaos.DrillPlan
	result chaos.DrillResult
	at     time.Time
}

// chaosDrills seeds the six 必练科目 (08§9.5) with their latest run. mysql
// primary failover exceeded 150% of expected recovery — pkg-go/chaos flags it
// for remediation (偏差 > 50% 立项整改).
var chaosDrills = []chaosDrill{
	{plan: chaos.DrillPlan{Kind: chaos.DrillNacosSplitBrain, Stage: chaos.StageStaging}, result: chaos.DrillResult{Kind: chaos.DrillNacosSplitBrain, Expected: 300 * time.Second, Actual: 240 * time.Second}, at: time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)},
	{plan: chaos.DrillPlan{Kind: chaos.DrillRedisFailover, Stage: chaos.StageProd, BlastRadius: "单个 redis 副本", AbortDeadline: 10 * time.Minute}, result: chaos.DrillResult{Kind: chaos.DrillRedisFailover, Expected: 30 * time.Second, Actual: 28 * time.Second}, at: time.Date(2026, 6, 11, 14, 0, 0, 0, time.UTC)},
	{plan: chaos.DrillPlan{Kind: chaos.DrillMySQLFailover, Stage: chaos.StageProd, BlastRadius: "较轻的 MySQL AZ", AbortDeadline: 10 * time.Minute}, result: chaos.DrillResult{Kind: chaos.DrillMySQLFailover, Expected: 120 * time.Second, Actual: 210 * time.Second}, at: time.Date(2026, 6, 11, 15, 30, 0, 0, time.UTC)},
	{plan: chaos.DrillPlan{Kind: chaos.DrillApisixEtcdSelfHeal, Stage: chaos.StageStaging}, result: chaos.DrillResult{Kind: chaos.DrillApisixEtcdSelfHeal, Expected: 180 * time.Second, Actual: 160 * time.Second}, at: time.Date(2026, 5, 14, 11, 0, 0, 0, time.UTC)},
	{plan: chaos.DrillPlan{Kind: chaos.DrillKafkaBrokerLoss, Stage: chaos.StageProd, BlastRadius: "单个 kafka broker", AbortDeadline: 10 * time.Minute}, result: chaos.DrillResult{Kind: chaos.DrillKafkaBrokerLoss, Expected: 60 * time.Second, Actual: 55 * time.Second}, at: time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)},
	{plan: chaos.DrillPlan{Kind: chaos.DrillSingleAZLoss, Stage: chaos.StageStaging}, result: chaos.DrillResult{Kind: chaos.DrillSingleAZLoss, Expected: 600 * time.Second, Actual: 540 * time.Second}, at: time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)},
}

// handleChaosDrills returns the M-9 chaos discipline feed: the six mandatory
// drill subjects, each with its plan discipline (stage/radius/abort path,
// validated by pkg-go/chaos) and the remediation verdict of its latest run.
func (s *ruleStore) handleChaosDrills(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDFrom(w, r); !ok {
		return
	}
	drills := make([]map[string]any, 0, len(chaosDrills))
	for _, d := range chaosDrills {
		if err := d.plan.Validate(); err != nil {
			writeErr(w, "Common.InternalError", 500, err.Error())
			return
		}
		drills = append(drills, map[string]any{
			"kind":            string(d.plan.Kind),
			"stage":           string(d.plan.Stage),
			"blastRadius":     d.plan.BlastRadius,
			"abortDeadlineMs": d.plan.AbortDeadline.Milliseconds(),
			"expectedMs":      d.result.Expected.Milliseconds(),
			"actualMs":        d.result.Actual.Milliseconds(),
			"needsRemediation": d.result.NeedsRemediation(),
			"drilledAt":       d.at.Format(time.RFC3339),
		})
	}
	writeJSON(w, "OK", map[string]any{
		"drills":             drills,
		"prodAbortDeadlineMs": chaos.ProdAbortDeadline.Milliseconds(),
		"mandatoryCount":     len(chaos.MandatoryDrills),
		"note":               "每季度至少一次全科目演练, 生产演练须具名爆炸半径且 10 分钟内可终止 (08§9.5)",
	})
}

// --- middleware ---

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Sc-TraceId")
		if id == "" {
			id = fmt.Sprintf("mon-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Sc-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

// internalTokenMiddleware optionally enforces an internal shared secret: when
// the SC_INTERNAL_TOKEN env var is set, every request must carry a matching
// X-Sc-Internal-Token header (defense-in-depth for the gateway-injected
// X-Sc-Account-Id trust). Unset (dev default) = no check.
func internalTokenMiddleware(h http.Handler) http.Handler {
	token := os.Getenv("SC_INTERNAL_TOKEN")
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Sc-Internal-Token") != token {
			writeErr(w, "Common.Forbidden", 403, "invalid internal token")
			return
		}
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
	addr := flag.String("http", ":9202", "HTTP listen address")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	store := newRuleStore()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("GET /api/v1/monitor/rules", store.handleListRules)
	mux.HandleFunc("POST /api/v1/monitor/rules", store.handleCreateRule)
	mux.HandleFunc("DELETE /api/v1/monitor/rules/{id}", store.handleDeleteRule)
	mux.HandleFunc("GET /api/v1/monitor/metrics", store.handleQueryMetrics)
	mux.HandleFunc("GET /api/v1/monitor/templates", store.handleTemplates)
	mux.HandleFunc("GET /api/v1/monitor/slo", store.handleSLO)
	mux.HandleFunc("GET /api/v1/monitor/chaos", store.handleChaosDrills)

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(internalTokenMiddleware(mux))), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("svc-monitor listening", "addr", *addr)
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
