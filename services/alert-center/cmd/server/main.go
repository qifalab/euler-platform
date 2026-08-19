// Package main: alert-center — the alert convergence / customer-notification
// gateway (03§4.4.6; phase-3 D-1, deferred from phase 2 per 09§4.0).
//
// alert-center sits between the platform's alert producers (platform
// Alertmanager webhook L0/L1, tenant alert-engine) and svc-notify. Its product
// logic is the four-stage convergence in pkg-go/alertcenter — 去重/分组/抑制/
// 静默 — plus the single-tenant notification rate limit (10 条/分钟, 防通知风暴).
//
// The naming convention is alert-center, NOT svc-* (03§1.1: alert components
// are alert-engine/alert-center). This service is the phase-3 "高级告警
// 产品化" — phase-2 svc-monitor already sells basic monitoring/alert rules;
// alert-center adds the advanced convergence + routing channel (09§4.0).
//
// Routes (gateway-authorized, X-Sc-Account-Id injected):
//
//	POST /api/v1/alertcenter/ingest  — ingest one alert (buffered)
//	POST /api/v1/alertcenter/flush   — run converge + rate-limit, emit notifications
//	GET  /api/v1/alertcenter/alerts  — list emitted notifications (history)
//
// stdlib-HTTP service (repo convention). Envelope-wrapped responses (03§9.3).
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
	"sync"
	"syscall"
	"time"

	"github.com/starcloud/sc-platform/alertcenter"
)

const accountIDHeader = "X-Sc-Account-Id"

// notification is one emitted customer notification (the MVP record standing in
// for the cloud.notify.message fan-out to svc-notify).
type notification struct {
	AlertID   string                  `json:"alertId"`
	TenantID  int64                   `json:"tenantId"`
	Product   string                  `json:"product"`
	Severity  alertcenter.Severity    `json:"severity"`
	Channels  []string                `json:"channels"`
	EmittedAt time.Time               `json:"emittedAt"`
}

// store buffers ingested alerts and retains emitted notifications.
type store struct {
	mu            sync.Mutex
	buffer        []alertcenter.Alert
	notifications []notification
	limiter       *alertcenter.TenantLimiter
	silence       *alertcenter.Silence
	now           func() time.Time
}

func newStore() *store {
	return &store{
		limiter: alertcenter.NewTenantLimiter(time.Now()),
		silence: alertcenter.NewSilence(),
		now:     time.Now,
	}
}

func (s *store) ingest(a alertcenter.Alert) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buffer = append(s.buffer, a)
}

func (s *store) flush() []notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// Four-stage convergence over the buffered batch.
	converged := alertcenter.Converge(s.buffer, s.silence)
	s.buffer = s.buffer[:0]

	var emitted []notification
	for _, a := range converged {
		if !s.limiter.Allow(a.TenantID, now) {
			continue // 10 条/分钟 rate limit (03§4.4.6), per tenant
		}
		n := notification{
			AlertID:   a.AlertID,
			TenantID:  a.TenantID,
			Product:   a.Product,
			Severity:  a.Severity,
			Channels:  []string{"IN_APP"},
			EmittedAt: now,
		}
		s.notifications = append(s.notifications, n)
		emitted = append(emitted, n)
	}
	sort.Slice(s.notifications, func(i, j int) bool {
		return s.notifications[i].EmittedAt.After(s.notifications[j].EmittedAt)
	})
	return emitted
}

func (s *store) list() []notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]notification, len(s.notifications))
	copy(out, s.notifications)
	return out
}

// --- handlers ------------------------------------------------------------------

type ingestReq struct {
	AlertID  string            `json:"alertId"`
	Product  string            `json:"product"`
	Metric   string            `json:"metric"`
	Severity alertcenter.Severity `json:"severity"`
	Labels   map[string]string `json:"labels"`
}

func (s *store) handleIngest(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req ingestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "Common.InvalidParameter", "malformed body")
		return
	}
	if req.Product == "" || req.Metric == "" {
		writeErr(w, 400, "AlertCenter.InvalidAlert", "product and metric are required")
		return
	}
	if req.Severity == "" {
		req.Severity = alertcenter.SeverityWarning
	}
	s.ingest(alertcenter.Alert{
		AlertID:    req.AlertID,
		TenantID:   tenantID,
		Product:    req.Product,
		Metric:     req.Metric,
		Severity:   req.Severity,
		Labels:     req.Labels,
		OccurredAt: time.Now(),
	})
	writeOK(w, map[string]any{"buffered": true})
}

func (s *store) handleFlush(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantFrom(w, r); !ok {
		return
	}
	writeOK(w, map[string]any{"notifications": s.flush()})
}

func (s *store) handleList(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	all := s.list()
	out := make([]notification, 0)
	for _, n := range all {
		if n.TenantID == tenantID {
			out = append(out, n)
		}
	}
	writeOK(w, map[string]any{"alerts": out})
}

// --- helpers ------------------------------------------------------------------

func tenantFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.Header.Get(accountIDHeader)
	if raw == "" {
		writeErr(w, 403, "Common.MissingAccountId", "X-Sc-Account-Id header is required")
		return 0, false
	}
	var id int64
	if _, err := fmt.Sscanf(raw, "%d", &id); err != nil {
		writeErr(w, 400, "Common.InvalidParameter", "malformed X-Sc-Account-Id")
		return 0, false
	}
	return id, true
}

func writeOK(w http.ResponseWriter, data any) {
	rid := w.Header().Get("X-Sc-TraceId")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": "OK", "Data": data})
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	rid := w.Header().Get("X-Sc-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Message": msg})
}

func newMux(s *store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/alertcenter/ingest", s.handleIngest)
	mux.HandleFunc("POST /api/v1/alertcenter/flush", s.handleFlush)
	mux.HandleFunc("GET /api/v1/alertcenter/alerts", s.handleList)
	return mux
}

func main() {
	addr := flag.String("http", ":9213", "HTTP listen address")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	s := newStore()
	srv := &http.Server{Addr: *addr, Handler: newMux(s), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("alert-center listening", "addr", *addr)
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
