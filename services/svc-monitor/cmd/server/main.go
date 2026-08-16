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
)

const accountIDHeader = "X-Sc-Account-Id"

// AlertRule mirrors alert_rule (sql/V1__support_db_monitor_schema.sql).
type AlertRule struct {
	RuleID                 int64             `json:"ruleId"`
	AccountID              int64             `json:"accountId"`
	ProductCode            string            `json:"productCode"`
	ResourceType           string            `json:"resourceType"`
	Metric                 string            `json:"metric"`
	Threshold              string            `json:"threshold"` // DECIMAL as string
	ComparisonOperator     int               `json:"comparisonOperator"` // 1≥ 2> 3≤ 4< 5==
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
	s.seed(100123, "scecs", "instance", "cpu_utilization", "80.0000", 1, 60, 1, []string{"IN_APP", "EMAIL"})
	s.seed(100123, "scecs", "instance", "memory_utilization", "90.0000", 1, 60, 1, []string{"IN_APP"})
	s.seed(100123, "scoss", "bucket", "request_count", "1000.0000", 2, 300, 2, []string{"SMS"})
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
		req.ComparisonOperator = 1
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

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(mux)), ReadHeaderTimeout: 5 * time.Second}
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
