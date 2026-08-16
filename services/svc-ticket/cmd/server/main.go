// Package main is the entry point for the svc-ticket HTTP service.
//
// svc-ticket is the customer support ticket service (03§4.4.4 工单系统). Its
// responsibility is ticket 创建/流转/评价, classification & priority, SLA
// timing, and the customer-service workbench interfaces. The ticket channel is
// on the "不可后置" (cannot-be-postponed) list, so the MVP ships in the minimal
// form: create / list / reply / close with an in-memory store.
//
// The service follows the shared scaffold (_tmpl-go/cmd/server) for the
// cross-cutting concerns every service MUST carry on Day 1 (架构原则 9):
// /healthz and /readyz for K8s probes, /metrics, request-id/logging/recover
// middleware, and graceful shutdown on SIGTERM (08§6.2).
//
// The domain logic and the ticket store are defined inline here because there
// is no pkg-go/ticket package in phase-1; the in-memory store mirrors the
// Store-interface pattern used by pkg-go/order and pkg-go/quota.
//
// The gateway (APISIX) injects the authenticated tenant identity via the
// X-Sc-Account-Id header; a missing header is rejected with 403 (03§3.3).
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

	"github.com/google/uuid"
	"github.com/starcloud/sc-platform/identifier"
)

// --- Ticket domain model ----------------------------------------------------

// TicketStatus is the lifecycle state of a ticket (03§4.4.4).
type TicketStatus string

const (
	StatusOpen          TicketStatus = "OPEN"           // 新建待处理
	StatusProcessing    TicketStatus = "PROCESSING"     // 处理中
	StatusWaitingReply  TicketStatus = "WAITING_REPLY"  // 待用户回复
	StatusClosed        TicketStatus = "CLOSED"         // 已关闭
)

// Ticket is one support ticket. SLA deadline = created + 4h for high priority,
// +24h otherwise (MVP minimal form, 03§4.4.4).
type Ticket struct {
	TicketID   string      `json:"ticket_id"`
	AccountID  int64       `json:"account_id"`
	Category   string      `json:"category"`
	Priority   string      `json:"priority"`
	Status     TicketStatus `json:"status"`
	SLADeadline time.Time  `json:"sla_deadline"`
	Assignee   string      `json:"assignee"`
	Messages   []Message   `json:"messages"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

// Message is a single entry in the ticket thread.
type Message struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"` // account_id or the support assignee
	FromUser  bool      `json:"from_user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// slaDeadline computes the MVP SLA deadline for a ticket.
func slaDeadline(priority string, created time.Time) time.Time {
	if priority == "HIGH" {
		return created.Add(4 * time.Hour)
	}
	return created.Add(24 * time.Hour)
}

// --- Store ------------------------------------------------------------------

// Store persists tickets. The in-memory implementation mirrors the
// Store-interface pattern of pkg-go/order and pkg-go/quota; production backs
// this with MySQL sharded by account_id.
type Store interface {
	Create(t *Ticket) error
	Get(accountID int64, ticketID string) (*Ticket, error)
	ListByAccount(accountID int64) []*Ticket
}

// memoryStore is the phase-1 in-memory Store.
type memoryStore struct {
	mu      sync.RWMutex
	byID    map[string]*Ticket
	byAcct  map[int64][]*Ticket
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		byID:   make(map[string]*Ticket),
		byAcct: make(map[int64][]*Ticket),
	}
}

func (s *memoryStore) Create(t *Ticket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[t.TicketID]; exists {
		return fmt.Errorf("ticket: duplicate ticket id %s", t.TicketID)
	}
	s.byID[t.TicketID] = t
	s.byAcct[t.AccountID] = append(s.byAcct[t.AccountID], t)
	return nil
}

func (s *memoryStore) Get(accountID int64, ticketID string) (*Ticket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byID[ticketID]
	if !ok || t.AccountID != accountID {
		return nil, fmt.Errorf("ticket: not found")
	}
	return t, nil
}

func (s *memoryStore) ListByAccount(accountID int64) []*Ticket {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*Ticket, 0, len(s.byAcct[accountID]))
	list = append(list, s.byAcct[accountID]...)
	// newest first
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	return list
}

// --- Application ------------------------------------------------------------

// app wires the store and the now clock.
type app struct {
	store Store
	now   func() time.Time
}

func newApp(store Store) *app {
	return &app{store: store, now: time.Now}
}

// --- Handlers ---------------------------------------------------------------

// createReq is the POST /internal/tickets body.
type createReq struct {
	Category string `json:"category"`
	Priority string `json:"priority"`
	Message  string `json:"message"`
}

func (a *app) handleCreate(w http.ResponseWriter, r *http.Request) {
	accountID, ok := accountFrom(r)
	if !ok {
		writeErr(w, http.StatusForbidden, "Ticket.MissingAccount", "account_id header required")
		return
	}
	var body createReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Ticket.InvalidBody", "invalid JSON body")
		return
	}
	body.Category = strings.TrimSpace(body.Category)
	body.Priority = strings.ToUpper(strings.TrimSpace(body.Priority))
	if body.Category == "" {
		writeErr(w, http.StatusBadRequest, "Ticket.InvalidCategory", "category is required")
		return
	}
	switch body.Priority {
	case "HIGH", "NORMAL", "LOW", "":
	default:
		writeErr(w, http.StatusBadRequest, "Ticket.InvalidPriority", "priority must be HIGH/NORMAL/LOW")
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeErr(w, http.StatusBadRequest, "Ticket.InvalidMessage", "initial message is required")
		return
	}
	if body.Priority == "" {
		body.Priority = "NORMAL"
	}

	now := a.now()
	t := &Ticket{
		TicketID:   uuid.NewString(),
		AccountID:  accountID,
		Category:   body.Category,
		Priority:   body.Priority,
		Status:     StatusOpen,
		SLADeadline: slaDeadline(body.Priority, now),
		Assignee:   identifier.ServiceName("ticket"),
		Messages: []Message{{
			ID:        uuid.NewString(),
			Author:    strconv.FormatInt(accountID, 10),
			FromUser:  true,
			Body:      body.Message,
			CreatedAt: now,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := a.store.Create(t); err != nil {
		writeErr(w, http.StatusInternalServerError, "Common.InternalError", "failed to create ticket")
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (a *app) handleList(w http.ResponseWriter, r *http.Request) {
	accountID, ok := accountFrom(r)
	if !ok {
		writeErr(w, http.StatusForbidden, "Ticket.MissingAccount", "account_id header required")
		return
	}
	list := a.store.ListByAccount(accountID)
	writeJSON(w, http.StatusOK, map[string]any{"tickets": list})
}

// replyReq is the POST /internal/tickets/{id}/reply body.
type replyReq struct {
	Message string `json:"message"`
}

func (a *app) handleReply(w http.ResponseWriter, r *http.Request) {
	accountID, ok := accountFrom(r)
	if !ok {
		writeErr(w, http.StatusForbidden, "Ticket.MissingAccount", "account_id header required")
		return
	}
	ticketID := ticketIDFromPath(r.URL.Path)
	if ticketID == "" {
		writeErr(w, http.StatusBadRequest, "Ticket.InvalidID", "ticket id required")
		return
	}
	var body replyReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Ticket.InvalidBody", "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeErr(w, http.StatusBadRequest, "Ticket.InvalidMessage", "message is required")
		return
	}

	t, err := a.store.Get(accountID, ticketID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "Ticket.NotFound", "ticket not found")
		return
	}
	if t.Status == StatusClosed {
		writeErr(w, http.StatusConflict, "Ticket.Closed", "ticket is closed")
		return
	}

	now := a.now()
	t.Messages = append(t.Messages, Message{
		ID:        uuid.NewString(),
		Author:    strconv.FormatInt(accountID, 10),
		FromUser:  true,
		Body:      body.Message,
		CreatedAt: now,
	})
	t.Status = StatusWaitingReply
	t.UpdatedAt = now
	writeJSON(w, http.StatusOK, t)
}

func (a *app) handleClose(w http.ResponseWriter, r *http.Request) {
	accountID, ok := accountFrom(r)
	if !ok {
		writeErr(w, http.StatusForbidden, "Ticket.MissingAccount", "account_id header required")
		return
	}
	ticketID := ticketIDFromPath(r.URL.Path)
	if ticketID == "" {
		writeErr(w, http.StatusBadRequest, "Ticket.InvalidID", "ticket id required")
		return
	}
	t, err := a.store.Get(accountID, ticketID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "Ticket.NotFound", "ticket not found")
		return
	}
	if t.Status == StatusClosed {
		writeErr(w, http.StatusConflict, "Ticket.Closed", "ticket is already closed")
		return
	}
	t.Status = StatusClosed
	t.UpdatedAt = a.now()
	writeJSON(w, http.StatusOK, t)
}

// --- Public API handlers (envelope-wrapped) --------------------------------

// apiCreateReq is the POST /api/v1/tickets body. It mirrors createReq but the
// handler returns the platform envelope so @sc/sdk can unwrap Data.
type apiCreateReq struct {
	Category string `json:"category"`
	Priority string `json:"priority"`
	Title    string `json:"title"`  // human title; persisted as the first message body
	Message  string `json:"message"`
}

// handleAPICreate is the public create endpoint. It reuses the Store/app
// validation + creation but returns the platform envelope {RequestId,Code,...}.
func (a *app) handleAPICreate(w http.ResponseWriter, r *http.Request) {
	accountID, ok := accountFrom(r)
	if !ok {
		writeEnvelopedErr(w, r, http.StatusForbidden, "Ticket.MissingAccount", "account_id header required")
		return
	}
	var body apiCreateReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelopedErr(w, r, http.StatusBadRequest, "Ticket.InvalidBody", "invalid JSON body")
		return
	}
	body.Category = strings.TrimSpace(body.Category)
	body.Priority = strings.ToUpper(strings.TrimSpace(body.Priority))
	if body.Category == "" {
		writeEnvelopedErr(w, r, http.StatusBadRequest, "Ticket.InvalidCategory", "category is required")
		return
	}
	switch body.Priority {
	case "HIGH", "NORMAL", "LOW", "":
	default:
		writeEnvelopedErr(w, r, http.StatusBadRequest, "Ticket.InvalidPriority", "priority must be HIGH/NORMAL/LOW")
		return
	}
	// title doubles as the first message body when an explicit message is absent;
	// the /internal contract requires a non-empty initial message.
	body.Message = strings.TrimSpace(body.Message)
	body.Title = strings.TrimSpace(body.Title)
	if body.Message == "" && body.Title == "" {
		writeEnvelopedErr(w, r, http.StatusBadRequest, "Ticket.InvalidMessage", "title or message is required")
		return
	}
	if body.Priority == "" {
		body.Priority = "NORMAL"
	}
	initialMsg := body.Message
	if initialMsg == "" {
		initialMsg = body.Title
	}

	now := a.now()
	t := &Ticket{
		TicketID:    uuid.NewString(),
		AccountID:   accountID,
		Category:    body.Category,
		Priority:    body.Priority,
		Status:      StatusOpen,
		SLADeadline: slaDeadline(body.Priority, now),
		Assignee:    identifier.ServiceName("ticket"),
		Messages: []Message{{
			ID:        uuid.NewString(),
			Author:    strconv.FormatInt(accountID, 10),
			FromUser:  true,
			Body:      initialMsg,
			CreatedAt: now,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := a.store.Create(t); err != nil {
		writeEnvelopedErr(w, r, http.StatusInternalServerError, "Common.InternalError", "failed to create ticket")
		return
	}
	writeEnvelope(w, r, http.StatusCreated, "OK", "", toTicketDTO(t))
}

// handleAPIList is the public list endpoint, scoped to the authenticated
// account and wrapped in the platform envelope.
func (a *app) handleAPIList(w http.ResponseWriter, r *http.Request) {
	accountID, ok := accountFrom(r)
	if !ok {
		writeEnvelopedErr(w, r, http.StatusForbidden, "Ticket.MissingAccount", "account_id header required")
		return
	}
	list := a.store.ListByAccount(accountID)
	dto := make([]ticketDTO, 0, len(list))
	for _, t := range list {
		dto = append(dto, toTicketDTO(t))
	}
	writeEnvelope(w, r, http.StatusOK, "OK", "", map[string]any{"tickets": dto})
}

// ticketDTO is the JSON projection surfaced to the console. Field names match
// the TicketList/CreateTicket frontend contract.
type ticketDTO struct {
	TicketID    string       `json:"ticket_id"`
	Category    string       `json:"category"`
	Priority    string       `json:"priority"`
	Status      TicketStatus `json:"status"`
	Assignee    string       `json:"assignee"`
	SLADeadline time.Time    `json:"sla_deadline"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Message     string       `json:"message"` // first thread message (title)
}

func toTicketDTO(t *Ticket) ticketDTO {
	msg := ""
	if len(t.Messages) > 0 {
		msg = t.Messages[0].Body
	}
	return ticketDTO{
		TicketID:    t.TicketID,
		Category:    t.Category,
		Priority:    t.Priority,
		Status:      t.Status,
		Assignee:    t.Assignee,
		SLADeadline: t.SLADeadline,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
		Message:     msg,
	}
}

// --- Routing helpers --------------------------------------------------------

// accountFrom reads the authenticated tenant id injected by the gateway.
func accountFrom(r *http.Request) (int64, bool) {
	raw := r.Header.Get("X-Sc-Account-Id")
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// ticketIDFromPath extracts the id segment from /internal/tickets/{id}/reply
// or /internal/tickets/{id}/close.
func ticketIDFromPath(path string) string {
	// /internal/tickets/{id}/reply
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 4 && parts[0] == "internal" && parts[1] == "tickets" {
		return parts[2]
	}
	return ""
}

// --- Probes ----------------------------------------------------------------

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyz(w http.ResponseWriter, _ *http.Request) {
	// TODO(svc-ticket): return 503 until Nacos registration + DB ping succeed.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// TODO(svc-ticket): expose RED metrics via prometheus/client_golang.
	_, _ = w.Write([]byte("# HELP sc_ticket_dummy 0\n# TYPE sc_ticket_dummy counter\nsc_ticket_dummy 0\n"))
}

// --- JSON helpers -----------------------------------------------------------

// writeJSON writes the raw object as JSON. Used by the /internal routes.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiResponse wraps every JSON payload with the platform envelope
// {RequestId, Code, Message, Data} so the @sc/sdk client can branch on a stable
// Code and unwrap Data (03§9.3). Mirrors svc-notify's envelope.
type apiResponse struct {
	Code      string `json:"Code"`
	Message   string `json:"Message,omitempty"`
	RequestId string `json:"RequestId"`
	Data      any    `json:"Data,omitempty"`
}

// writeEnvelope writes the platform JSON envelope. RequestId rides along so
// 客服/排障 can correlate (03§9.3). Used by the /api/v1 routes.
func writeEnvelope(w http.ResponseWriter, r *http.Request, status int, code, msg string, data any) {
	rid, _ := r.Context().Value(requestIDKey).(string)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResponse{
		Code:      code,
		Message:   msg,
		RequestId: rid,
		Data:      data,
	})
}

// writeErr writes a JSON error envelope with the given status.
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"Code": code, "Message": msg})
}

// writeEnvelopedErr writes a JSON error using the platform envelope. Used by
// the /api/v1 routes.
func writeEnvelopedErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeEnvelope(w, r, status, code, msg, nil)
}

// --- main -------------------------------------------------------------------

func main() {
	var (
		httpAddr = flag.String("http", ":8080", "HTTP listen address")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	a := newApp(newMemoryStore())

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/metrics", metrics)

	mux.HandleFunc("/internal/tickets", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			a.handleCreate(w, r)
		case http.MethodGet:
			a.handleList(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "Common.MethodNotAllowed", "method not allowed")
		}
	})
	mux.HandleFunc("/internal/tickets/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(r.URL.Path, "/")
		switch {
		case strings.HasSuffix(path, "/reply") && r.Method == http.MethodPost:
			a.handleReply(w, r)
		case strings.HasSuffix(path, "/close") && r.Method == http.MethodPost:
			a.handleClose(w, r)
		default:
			writeErr(w, http.StatusNotFound, "Ticket.NotFound", "not found")
		}
	})

	// Public API (platform-envelope, consumed by @sc/sdk in web-ticket). The
	// Vite dev proxy forwards /api/v1/tickets here with X-Sc-Account-Id set.
	mux.HandleFunc("/api/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			a.handleAPICreate(w, r)
		case http.MethodGet:
			a.handleAPIList(w, r)
		default:
			writeEnvelopedErr(w, r, http.StatusMethodNotAllowed, "Common.MethodNotAllowed", "method not allowed")
		}
	})
	mux.HandleFunc("/api/v1/tickets/", func(w http.ResponseWriter, r *http.Request) {
		// Placeholder for future sub-resource routes (reply/close) on the public
		// API; the internal routes remain authoritative for now.
		writeEnvelopedErr(w, r, http.StatusNotFound, "Ticket.NotFound", "not found")
	})

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

	// Graceful shutdown (08§6.2 preStop hook).
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

// withMiddleware wraps the mux with the cross-cutting middleware chain every
// service must apply (03§2.3.4).
func withMiddleware(h http.Handler) http.Handler {
	return recoverMiddleware(requestIDMiddleware(loggingMiddleware(h)))
}
