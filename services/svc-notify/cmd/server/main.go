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
	"github.com/starcloud/sc-platform/storage"
)

func main() {
	httpAddr := flag.String("http", ":9211", "HTTP listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// The notification store decides where the evidence lives. Persistence is
	// opt-in (pkg-go/storage doc): with SC_DB_DSN set, notifications and their
	// per-channel deliveries land in support_db — "you never told me" is then
	// answerable by query; unset, the in-memory store keeps the demo.
	//
	// A configured DSN that cannot be reached is a startup failure: a notifier
	// that cannot record what it sent must not claim to have sent it.
	var store notify.Store = newMemStore()
	persistent := false
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
		store = notify.NewSQLStore(context.Background(), db)
		persistent = true
	}
	slog.Info("svc-notify notification store ready", "persistent", persistent)
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

	app := &app{disp: disp, store: store, announcements: seedAnnouncements()}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/metrics", metrics)
	mux.HandleFunc("/internal/notifications", app.handleNotifications)
	mux.HandleFunc("/internal/notifications/verify", app.handleVerify)
	// Public site content — anonymous (the marketing site SSRs it before any
	// login); see handleAnnouncements for why this lives in svc-notify.
	mux.HandleFunc("/api/v1/announcements", app.handleAnnouncements)

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
	store notify.Store
	// announcements is the public site-content board (news / programs / videos)
	// served by GET /api/v1/announcements. It rides on svc-notify because a
	// broadcast announcement IS a notification — same audience-wide publish
	// semantics, same persistence-before-display rule; a dedicated CMS service
	// can take over the table later without changing the route contract.
	announcements []announcement
}

// announcement is one card on the marketing site's homepage. Type selects the
// slot the card lands in; Tab groups program cards under their tab. Field
// names mirror what the site renders (category/badge/title/desc/image).
type announcement struct {
	ID          string `json:"id"`
	Type        string `json:"type"`        // news | program | video
	Tab         string `json:"tab,omitempty"`         // program only: tab grouping key
	Category    string `json:"category,omitempty"`     // news only: corner tag
	Badge       string `json:"badge,omitempty"`        // program only: corner badge
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	Link        string `json:"link,omitempty"`
	Duration    string `json:"duration,omitempty"`     // video only
	PublishedAt time.Time `json:"publishedAt"`
}

// handleAnnouncements implements GET /api/v1/announcements — the public site
// content board, filterable by ?type=news|program|video (and ?tab= for
// programs). Anonymous by design: the marketing site is browsed pre-login, so
// there is no X-Sc-Account-Id to require; the payload carries no tenant data.
func (a *app) handleAnnouncements(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, r, http.StatusMethodNotAllowed, "Common.InvalidAction", "method not allowed")
		return
	}
	typ := r.URL.Query().Get("type")
	tab := r.URL.Query().Get("tab")
	out := make([]announcement, 0, len(a.announcements))
	for _, an := range a.announcements {
		if typ != "" && an.Type != typ {
			continue
		}
		if tab != "" && an.Tab != tab {
			continue
		}
		out = append(out, an)
	}
	writeJSON(w, r, http.StatusOK, "OK", "", map[string]interface{}{"items": out})
}

// seedAnnouncements mirrors the site's homepage content into the announcement
// board (t_announcement in production, edited by the marketing back-office).
// The seed preserves the shipped copy so the page renders identically while
// its data source moves server-side.
func seedAnnouncements() []announcement {
	now := time.Now().UTC()
	day := func(d int) time.Time { return now.AddDate(0, 0, -d) }
	return []announcement{
		// Homepage 最新动态 carousel.
		{ID: "ann-001", Type: "news", Category: "产品动态", Title: "欧拉 AI 威胁防御正式发布", Description: "帮助企业以 AI 驱动的安全分析,更快识别和应对威胁。", Image: "/images/update-1.jpg", PublishedAt: day(2)},
		{ID: "ann-002", Type: "news", Category: "AI 基础设施", Title: "新一代 AI 基础设施:面向 Agent 时代的扩展", Description: "支持更大规模模型推理与 Agent 编排的算力架构升级。", Image: "/images/update-2.jpg", PublishedAt: day(6)},
		{ID: "ann-003", Type: "news", Category: "数据云", Title: "Agentic 数据云的新能力", Description: "驱动「行动系统」——从数据洞察到自动化决策的全链路。", Image: "/images/update-3.jpg", PublishedAt: day(9)},
		{ID: "ann-004", Type: "news", Category: "产品动态", Title: "Gemini Enterprise:一个平台搞定 Agent 开发", Description: "统一的 Agent 开发、编排与治理平台,加速企业 AI 落地。", Image: "/images/update-4.jpg", PublishedAt: day(13)},
		// Homepage 栏目 tab cards.
		{ID: "ann-101", Type: "program", Tab: "开发者", Badge: "产品动态", Title: "Gemini 3.6 Flash 模型上线", Description: "更快的推理速度,更低的调用成本,适合高频 Agent 场景。", PublishedAt: day(1)},
		{ID: "ann-102", Type: "program", Tab: "开发者", Badge: "指南", Title: "用 Agent Platform 构建多 Agent 系统", Description: "10 分钟内构建一个可工作的 AI 应用——从零到部署。", PublishedAt: day(3)},
		{ID: "ann-103", Type: "program", Tab: "开发者", Badge: "指南", Title: "远程 MCP Server 实战", Description: "用全托管远程 MCP Server 快速接入企业工具链。", PublishedAt: day(5)},
		{ID: "ann-111", Type: "program", Tab: "企业领袖", Badge: "报告", Title: "2026 AI ROI 报告:从 Token 到回报", Description: "量化企业 AI 投资回报,找到最优的 Agent 落地路径。", PublishedAt: day(2)},
		{ID: "ann-112", Type: "program", Tab: "企业领袖", Badge: "活动", Title: "Build with Gemini 城市巡展", Description: "在你所在的城市获得 Gemini 实操经验,立即报名。", PublishedAt: day(4)},
		{ID: "ann-113", Type: "program", Tab: "企业领袖", Badge: "指南", Title: "企业级 Agentic 工作指南", Description: "用 Gemini Enterprise 重塑企业工作流的实践手册。", PublishedAt: day(7)},
		{ID: "ann-121", Type: "program", Tab: "特别计划", Badge: "新用户", Title: "¥300 免费额度 + 20+ 免费层产品", Description: "注册即享免费试用额度,覆盖计算、存储、数据库等核心产品。", PublishedAt: day(1)},
		{ID: "ann-122", Type: "program", Tab: "特别计划", Badge: "AI 构建者", Title: "GEAR 计划:每月 35 额度学 Agent", Description: "加入 Gemini Enterprise Agent Ready,学习构建企业级 Agent。", PublishedAt: day(8)},
		{ID: "ann-123", Type: "program", Tab: "特别计划", Badge: "创业公司", Title: "最高 ¥350 万云资源补贴", Description: "早期融资初创企业可通过欧拉创业计划获取云资源补贴。", PublishedAt: day(10)},
		// Homepage AI highlight video cards.
		{ID: "ann-201", Type: "video", Title: "10 分钟用 Agent Platform 构建应用", Duration: "4 分钟", Image: "/images/highlight-1.jpg", PublishedAt: day(3)},
		{ID: "ann-202", Type: "video", Title: "多 Agent 系统架构设计", Duration: "12 分钟", Image: "/images/highlight-2.jpg", PublishedAt: day(5)},
		{ID: "ann-203", Type: "video", Title: "AI 图片编辑指南", Duration: "4 分钟", Image: "/images/highlight-3.jpg", PublishedAt: day(7)},
		{ID: "ann-204", Type: "video", Title: "模型安全与 Agent 防护", Duration: "8 分钟", Image: "/images/highlight-4.jpg", PublishedAt: day(11)},
	}
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

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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
	ns, err := a.store.ListByAccount(accountID)
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "Notify.ListFailed", "通知列表读取失败")
		return
	}
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
// It is the console inbox view on top of the Store.
func (m *memStore) ListByAccount(accountID int64) ([]notify.Notification, error) {
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
	return out, nil
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
