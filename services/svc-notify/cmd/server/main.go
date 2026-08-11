// Package main is the HTTP server entry point for svc-notify.
//
// svc-notify implements the notification pipeline (03-backend-services.md
// §4.4.2). This is a runnable stdlib HTTP reference implementation: it wires
// pkg-go/notify (Dispatcher, Notification, Class) behind JSON handlers, with
// an in-memory store standing in for the Vitess/Redis-backed repository and
// mock senders standing in for the SMS gateway / mail relay / webhook push.
//
// Three classes — 欠费催收, 到期提醒, 释放预告 — are a trust boundary, not a
// feature (03§4.4.2): they MUST be persisted and queryable before dispatch and
// their delivery outcome recorded, because a failure to warn the customer
// blocks the destructive action. That rule is enforced here by Dispatcher.Send
// (persist-before-send, BizKey required) and exposed for the lifecycle via
// GET /internal/notifications/verify. Per-tenant rate limiting (10/minute,
// 05§8.3) protects the customer's attention but never throttles a
// trust-critical warning.
//
// The gateway injects the caller's identity; the handlers read account_id from
// the X-Sc-Account-Id header (04§3.1 / 07§3.3) and reject with 403 when it is
// absent. Service name follows the 全局标识规范: svc-{domain} for control-plane.
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/starcloud/sc-platform/notify"
)

func main() {
	httpAddr := flag.String("http", ":8080", "HTTP listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	store := newMemStore()
	disp := notify.NewDispatcher(store, time.Now, uuid.NewString)
	// Register a mock sender per channel so a notification always has a
	// delivery path in this reference implementation. Production registers the
	// SMS gateway, mail relay, in-app store and webhook push adapters instead.
	for _, s := range []notify.Sender{
		&mockSender{ch: notify.ChannelInApp},
		&mockSender{ch: notify.ChannelSMS},
		&mockSender{ch: notify.ChannelEmail},
		&mockSender{ch: notify.ChannelWebhook},
	} {
		disp.Register(s)
	}

	app := &app{disp: disp, store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/metrics", metrics)
	mux.HandleFunc("/internal/notifications", app.handleNotifications)
	mux.HandleFunc("/internal/notifications/verify", app.handleVerify)

	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           withMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("http server listening", "addr", *httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen error", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown: drain in-flight requests before exit (08§6.2).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutdown signal received, draining")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
	slog.Info("server exited")
}

// app wires the pkg-go/notify domain logic to HTTP handlers.
type app struct {
	disp  *notify.Dispatcher
	store *memStore
}

// withMiddleware wraps the mux with the cross-cutting middleware chain every
// service must apply (03§2.3.4).
func withMiddleware(h http.Handler) http.Handler {
	return recoverMiddleware(requestIDMiddleware(loggingMiddleware(h)))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte("# HELP sc_service_dummy 0\n# TYPE sc_service_dummy counter\nsc_service_dummy 0\n"))
}

// --- JSON request/response shapes --------------------------------------------

// apiResponse wraps every JSON payload with the platform envelope so clients
// can branch on a stable Code rather than parsing transport errors.
type apiResponse struct {
	Code    string      `json:"Code"`
	Message string      `json:"Message,omitempty"`
	RequestId string   `json:"RequestId"`
	Data    interface{} `json:"Data,omitempty"`
}

// sendRequest is the body of POST /internal/notifications.
type sendRequest struct {
	Class      notify.Class      `json:"Class"`
	TemplateID string            `json:"TemplateId"`
	Params     map[string]string `json:"Params,omitempty"`
	Channels   []notify.Channel  `json:"Channels,omitempty"`
	BizKey     string            `json:"BizKey,omitempty"`
}

// notificationDTO is the JSON projection of a persisted Notification, including
// the per-channel delivery evidence a support agent needs for a dispute.
type notificationDTO struct {
	NotificationID string             `json:"NotificationId"`
	AccountID      int64              `json:"AccountId"`
	Class          notify.Class       `json:"Class"`
	TemplateID     string             `json:"TemplateId"`
	BizKey         string             `json:"BizKey,omitempty"`
	Status         notify.Status      `json:"Status"`
	Attempts       int                `json:"Attempts"`
	CreatedAt      time.Time          `json:"CreatedAt"`
	SentAt         time.Time          `json:"SentAt,omitempty"`
	Deliveries     []notify.Delivery  `json:"Deliveries,omitempty"`
	LastError      string             `json:"LastError,omitempty"`
}

func toDTO(n notify.Notification) notificationDTO {
	return notificationDTO{
		NotificationID: n.NotificationID,
		AccountID:      n.AccountID,
		Class:          n.Class,
		TemplateID:     n.TemplateID,
		BizKey:         n.BizKey,
		Status:         n.Status,
		Attempts:       n.Attempts,
		CreatedAt:      n.CreatedAt,
		SentAt:         n.SentAt,
		Deliveries:     n.Deliveries,
		LastError:      n.LastError,
	}
}

// handleNotifications routes POST (dispatch) and GET (list) on the collection.
func (a *app) handleNotifications(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		a.dispatch(w, r)
	case http.MethodGet:
		a.list(w, r)
	default:
		writeErr(w, r, http.StatusMethodNotAllowed, "Common.InvalidAction", "method not allowed")
	}
}

// dispatch implements POST /internal/notifications.
//
// account_id is taken from the X-Sc-Account-Id header the gateway injects
// (04§3.1); it is the shard key and must never be trusted from the body.
func (a *app) dispatch(w http.ResponseWriter, r *http.Request) {
	accountID, ok := accountIDFromHeader(r)
	if !ok {
		writeErr(w, r, http.StatusForbidden, "Common.MissingAccount", "X-Sc-Account-Id header required")
		return
	}

	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, r, http.StatusBadRequest, "Common.InvalidParameter", "malformed request body")
		return
	}

	n := notify.Notification{
		AccountID:  accountID,
		Class:      req.Class,
		TemplateID: req.TemplateID,
		Params:     req.Params,
		Channels:   req.Channels,
		BizKey:     req.BizKey,
	}
	// Default channels per class when the caller did not specify any, so the
	// trust-critical fan-out (站内信+短信+邮件) applies by default (01§5.4 D8).
	if len(n.Channels) == 0 {
		n.Channels = notify.ChannelsFor(n.Class)
	}

	sent, err := a.disp.Send(n)
	if err != nil {
		switch {
		case errors.Is(err, notify.ErrRateLimited):
			// The message was persisted as SUPPRESSED; the envelope carries it
			// so the caller can reconcile, but the status is throttled.
			writeJSON(w, r, http.StatusOK, "Notify.RateLimited", "tenant rate limit exceeded", toDTO(sent))
		case errors.Is(err, notify.ErrNotDelivered):
			// Persisted as FAILED with per-channel evidence — the destructive
			// action must not proceed, which is what VerifyDelivered answers.
			writeJSON(w, r, http.StatusOK, "Notify.NotDelivered", "message not delivered on any channel", toDTO(sent))
		case errors.Is(err, notify.ErrMissingAccount):
			writeErr(w, r, http.StatusForbidden, "Notify.MissingAccount", err.Error())
		default:
			writeErr(w, r, http.StatusBadRequest, "Common.InvalidParameter", err.Error())
		}
		return
	}

	writeJSON(w, r, http.StatusOK, "OK", "", toDTO(sent))
}

// handleVerify implements GET /internal/notifications/verify.
//
// This is the gate the resource lifecycle calls before releasing data
// (03§5.4): it answers, from persisted evidence, whether the customer was
// warned. It only accepts trust-critical classes — Dispatcher.VerifyDelivered
// rejects any other.
func (a *app) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, r, http.StatusMethodNotAllowed, "Common.InvalidAction", "method not allowed")
		return
	}
	accountID, ok := accountIDFromHeader(r)
	if !ok {
		writeErr(w, r, http.StatusForbidden, "Common.MissingAccount", "X-Sc-Account-Id header required")
		return
	}

	class := notify.Class(r.URL.Query().Get("class"))
	bizKey := r.URL.Query().Get("bizKey")
	if bizKey == "" {
		writeErr(w, r, http.StatusBadRequest, "Common.InvalidParameter", "bizKey query parameter required")
		return
	}

	delivered, err := a.disp.VerifyDelivered(accountID, class, bizKey)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "Common.InvalidParameter", err.Error())
		return
	}
	if !delivered {
		writeJSON(w, r, http.StatusOK, "Notify.NotVerified", "no delivered warning on record", map[string]bool{"Delivered": false})
		return
	}
	writeJSON(w, r, http.StatusOK, "OK", "", map[string]bool{"Delivered": true})
}

// list implements GET /internal/notifications, returning the notifications
// recorded for the calling tenant, most recent first.
func (a *app) list(w http.ResponseWriter, r *http.Request) {
	accountID, ok := accountIDFromHeader(r)
	if !ok {
		writeErr(w, r, http.StatusForbidden, "Common.MissingAccount", "X-Sc-Account-Id header required")
		return
	}
	ns := a.store.ListByAccount(accountID)
	dto := make([]notificationDTO, 0, len(ns))
	for _, n := range ns {
		dto = append(dto, toDTO(n))
	}
	writeJSON(w, r, http.StatusOK, "OK", "", map[string]interface{}{"Notifications": dto, "Count": len(dto)})
}

// accountIDFromHeader reads the account identity the gateway injected. It is
// the shard key for the notification store, so it must come from the gateway,
// never from the request body (07§3.3).
func accountIDFromHeader(r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.Header.Get("X-Sc-Account-Id"))
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// writeErr writes a JSON error envelope with the given status.
func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, r, status, code, msg, nil)
}

// writeJSON writes the platform JSON envelope. RequestId always rides along so
// 客服/排障 can correlate (03§9.3).
func writeJSON(w http.ResponseWriter, r *http.Request, status int, code, msg string, data interface{}) {
	rid, _ := r.Context().Value(requestIDKey).(string)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResponse{
		Code:      code,
		Message:   msg,
		RequestId: rid,
		Data:      data,
	})
}

// --- in-memory Store (replaced by Vitess (vtgate)/Redis in production) ------

// memStore is a concurrency-safe in-memory notify.Store. account_id is the
// shard key; every query is scoped by it.
type memStore struct {
	mu    sync.RWMutex
	items map[string]notify.Notification
}

func newMemStore() *memStore { return &memStore{items: make(map[string]notify.Notification)} }

func (m *memStore) Save(n notify.Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[n.NotificationID] = n
	return nil
}

func (m *memStore) Get(id string) (notify.Notification, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.items[id]
	if !ok {
		return notify.Notification{}, fmt.Errorf("notification %s not found", id)
	}
	return n, nil
}

func (m *memStore) CountInWindow(accountID int64, since time.Time) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, n := range m.items {
		if n.AccountID == accountID && n.Status != notify.StatusSuppressed && !n.CreatedAt.Before(since) {
			count++
		}
	}
	return count, nil
}

func (m *memStore) FindByBizKey(accountID int64, class notify.Class, bizKey string) ([]notify.Notification, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []notify.Notification
	for _, n := range m.items {
		if n.AccountID == accountID && n.Class == class && n.BizKey == bizKey {
			out = append(out, n)
		}
	}
	notify.SortByTime(out)
	return out, nil
}

// ListByAccount returns the notifications recorded for a tenant, newest first.
// It is an HTTP-view convenience method on top of the Store; the domain only
// needs the queries defined by the Store interface.
func (m *memStore) ListByAccount(accountID int64) []notify.Notification {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []notify.Notification
	for _, n := range m.items {
		if n.AccountID == accountID {
			out = append(out, n)
		}
	}
	notify.SortByTime(out)
	// Newest first for the support view.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// mockSender is a mock notify.Sender per channel. In production these are
// replaced by the SMS gateway, mail relay, in-app store and webhook push
// adapters; the delivery record shape is unchanged.
type mockSender struct {
	ch notify.Channel
}

func (s *mockSender) Channel() notify.Channel { return s.ch }

func (s *mockSender) Send(n notify.Notification) (notify.Delivery, error) {
	return notify.Delivery{
		Channel:   s.ch,
		Success:   true,
		SentAt:    time.Now(),
		Provider:  string(s.ch) + "-provider",
		MessageID: uuid.NewString(),
	}, nil
}
