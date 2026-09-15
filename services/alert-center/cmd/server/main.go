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
	"github.com/starcloud/sc-platform/notify"
	"github.com/starcloud/sc-platform/storage"
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

// store buffers ingested alerts and retains emitted notifications. When a
// durable notification store is wired (support_db via pkg-go/notify), every
// emitted notification is also written through to it — the in-memory slice is
// the process view, the notification table is the record a support agent or a
// regulator can query.
type store struct {
	mu            sync.Mutex
	buffer        []alertcenter.Alert
	notifications []notification
	limiter       *alertcenter.TenantLimiter
	silence       *alertcenter.Silence
	now           func() time.Time
	// notifyStore persists emitted notifications (nil = in-memory only).
	notifyStore notify.Store
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

// flushForTenant converges the CALLER's buffered alerts and emits their
// notifications. The buffer is shared by every tenant the process serves, so
// the flush must both converge only the caller's alerts and leave everyone
// else's in place — the pre-fix global flush returned other tenants'
// notifications in the response and let any tenant consume alerts it does not
// own.
func (s *store) flushForTenant(tenantID int64) []notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	mine := make([]alertcenter.Alert, 0, len(s.buffer))
	others := make([]alertcenter.Alert, 0, len(s.buffer))
	for _, a := range s.buffer {
		if a.TenantID == tenantID {
			mine = append(mine, a)
			continue
		}
		others = append(others, a)
	}
	s.buffer = others
	// Four-stage convergence over the caller's batch.
	converged := alertcenter.Converge(mine, s.silence)

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
		s.persistNotification(n)
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

// persistNotification writes one emitted notification to the durable store.
// Called with s.mu held. A failure is logged, not fatal: the alert remains in
// the source stream and the next ingest of the same alert re-emits, while
// blocking the flush would drop every other tenant's notifications behind one
// bad write.
func (s *store) persistNotification(n notification) {
	if s.notifyStore == nil {
		return
	}
	// The notification table's identity is the business key pair (account,
	// class, biz_key); the alert id is the biz key, so a re-emitted alert
	// upserts rather than duplicating the record.
	nn := notify.Notification{
		NotificationID: "alert-" + n.AlertID,
		AccountID:      n.TenantID,
		Class:          notify.ClassAlert,
		TemplateID:     "alert-center-" + string(n.Severity),
		BizKey:         n.AlertID,
		Channels:       []notify.Channel{notify.ChannelInApp},
		Status:         notify.StatusSent,
		CreatedAt:      n.EmittedAt,
		SentAt:         n.EmittedAt,
		Deliveries: []notify.Delivery{{
			Channel:  notify.ChannelInApp,
			Success:  true,
			SentAt:   n.EmittedAt,
			Provider: "alert-center",
		}},
	}
	if err := s.notifyStore.Save(nn); err != nil {
		slog.Error("notification persist failed", "alertId", n.AlertID, "err", err)
	}
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
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	writeOK(w, map[string]any{"notifications": s.flushForTenant(tenantID)})
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
	// The notification store decides where the emitted-notifications record
	// lives. Persistence is opt-in (pkg-go/storage doc): with SC_DB_DSN set,
	// every emitted notification also lands in support_db.notification (the
	// same rows svc-notify serves), so "you were warned" is answerable by
	// query after this process dies; unset, the in-memory slice keeps the demo.
	db, ok, err := storage.MustOpenFor(context.Background(), "support_db")
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if ok {
		if err := storage.EnsureMigrated(context.Background(), db, "support_db"); err != nil {
			slog.Error("startup failed", "err", err)
			os.Exit(1)
		}
		s.notifyStore = notify.NewSQLStore(context.Background(), db)
	}
	slog.Info("alert-center notification store ready", "persistent", ok)
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
