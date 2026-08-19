// Package main: svc-marketplace — the cloud marketplace service
// (09-roadmap §5.1 goal 3, §5.2 M-10 生态门户; 01§3.2 第三批·生态与差异化).
//
// The marketplace is the platform's third-party commerce surface: partners
// (ISVs) publish 镜像/SaaS/服务 listings, the platform reviews and approves
// them, customers purchase them, and every order is split between the partner
// and the platform via pkg-go/settlement (分账/结算).
//
// Phase-3 MVP form (matching the M-10 "最小版" scope, 09§5.1): publish →
// approve → list → settle, with an in-memory store. The partner's product is
// fulfilled by the partner's own OpenAPI callback (06 章口径说明: 三方产品按
// 回调其 OpenAPI 方式履约), NOT by an rc-* controller — that is the whole
// point of a marketplace: the platform does not operate the partner's product.
//
// Routes (gateway-authorized, X-Sc-Account-Id injected):
//
//	POST /api/v1/marketplace/listings             — publish a listing (partner)
//	POST /api/v1/marketplace/listings/{id}/approve — platform review verdict
//	GET  /api/v1/marketplace/listings             — list APPROVED listings (对客目录)
//	POST /api/v1/marketplace/settlements          — settle a marketplace order (分账)
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
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/settlement"
)

const accountIDHeader = "X-Sc-Account-Id"

// --- Marketplace domain -----------------------------------------------------

// ListingCategory is the third-party product class (01§3.2: 镜像/SaaS/服务).
type ListingCategory string

const (
	CategoryImage   ListingCategory = "IMAGE"
	CategorySaaS    ListingCategory = "SAAS"
	CategoryService ListingCategory = "SERVICE"
)

func validCategory(c ListingCategory) bool {
	return c == CategoryImage || c == CategorySaaS || c == CategoryService
}

// ListingStatus is the review lifecycle (09§5.2 M-10 上架审核).
type ListingStatus string

const (
	StatusDraft           ListingStatus = "DRAFT"
	StatusPendingApproval ListingStatus = "PENDING_APPROVAL"
	StatusApproved        ListingStatus = "APPROVED"
	StatusRejected        ListingStatus = "REJECTED"
	StatusOffShelf        ListingStatus = "OFF_SHELF"
)

// Listing is one marketplace product listing.
type Listing struct {
	ListingID     int64           `json:"listingId"`
	PartnerID     int64           `json:"partnerId"`
	Name          string          `json:"name"`
	Category      ListingCategory `json:"category"`
	OpenAPIURL    string          `json:"openapiUrl"`
	Status        ListingStatus   `json:"status"`
	PartnerRateBps int64          `json:"partnerRateBps"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// SettlementRecord is one settled order split (immutable, 对账用).
type SettlementRecord struct {
	SettlementID   int64           `json:"settlementId"`
	OrderID        string          `json:"orderId"`
	ListingID      int64           `json:"listingId"`
	PartnerID      int64           `json:"partnerId"`
	Gross          pricing.Amount  `json:"gross"`          // micro-units
	PartnerRateBps int64           `json:"partnerRateBps"`
	Partner        pricing.Amount  `json:"partner"`        // micro-units
	Platform       pricing.Amount  `json:"platform"`       // micro-units
	SettledAt      time.Time       `json:"settledAt"`
}

// --- Store ------------------------------------------------------------------

// Store persists listings and settlements. In-memory here; production backs it
// with MySQL sharded by account_id (see sql/V1).
type Store interface {
	CreateListing(l *Listing) error
	GetListing(id int64) (*Listing, error)
	ListApproved(category ListingCategory) []*Listing
	UpdateListingStatus(id int64, from, to ListingStatus) (*Listing, error)
	CreateSettlement(s *SettlementRecord) error
	GetSettlementByOrder(orderID string) (*SettlementRecord, bool)
}

type memoryStore struct {
	mu          sync.RWMutex
	listings    map[int64]*Listing
	settlements map[string]*SettlementRecord // orderID → record (idempotency)
	seq         int64
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		listings:    make(map[int64]*Listing),
		settlements: make(map[string]*SettlementRecord),
	}
}

func (s *memoryStore) nextID() int64 {
	s.seq++
	return s.seq
}

func (s *memoryStore) CreateListing(l *Listing) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.ListingID == 0 {
		l.ListingID = s.nextID()
	}
	if _, exists := s.listings[l.ListingID]; exists {
		return fmt.Errorf("marketplace: duplicate listing id %d", l.ListingID)
	}
	s.listings[l.ListingID] = l
	return nil
}

func (s *memoryStore) GetListing(id int64) (*Listing, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.listings[id]
	if !ok {
		return nil, fmt.Errorf("marketplace: listing %d not found", id)
	}
	return l, nil
}

func (s *memoryStore) ListApproved(category ListingCategory) []*Listing {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Listing, 0)
	for _, l := range s.listings {
		if l.Status != StatusApproved {
			continue
		}
		if category != "" && l.Category != category {
			continue
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ListingID < out[j].ListingID })
	return out
}

func (s *memoryStore) UpdateListingStatus(id int64, from, to ListingStatus) (*Listing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.listings[id]
	if !ok {
		return nil, fmt.Errorf("marketplace: listing %d not found", id)
	}
	if l.Status != from {
		return nil, fmt.Errorf("marketplace: listing %d is %s, not %s", id, l.Status, from)
	}
	l.Status = to
	l.UpdatedAt = time.Now()
	return l, nil
}

func (s *memoryStore) CreateSettlement(r *SettlementRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.settlements[r.OrderID]; exists {
		return fmt.Errorf("marketplace: order %s already settled", r.OrderID)
	}
	if r.SettlementID == 0 {
		r.SettlementID = s.nextID()
	}
	s.settlements[r.OrderID] = r
	return nil
}

func (s *memoryStore) GetSettlementByOrder(orderID string) (*SettlementRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.settlements[orderID]
	return r, ok
}

// --- Application + handlers --------------------------------------------------

type app struct {
	store Store
	now   func() time.Time
}

func newApp(store Store) *app { return &app{store: store, now: time.Now} }

func accountFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.Header.Get(accountIDHeader)
	if raw == "" {
		writeErr(w, 403, "Common.MissingAccountId", "X-Sc-Account-Id header is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, 400, "Common.InvalidParameter", "malformed X-Sc-Account-Id")
		return 0, false
	}
	return id, true
}

// publishReq is POST /api/v1/marketplace/listings.
type publishReq struct {
	Name          string          `json:"name"`
	Category      ListingCategory `json:"category"`
	OpenAPIURL    string          `json:"openapiUrl"`
	PartnerRateBps int64          `json:"partnerRateBps"`
}

func (a *app) handlePublish(w http.ResponseWriter, r *http.Request) {
	partnerID, ok := accountFrom(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req publishReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "Common.InvalidParameter", "malformed body")
		return
	}
	if req.Name == "" || !validCategory(req.Category) || req.OpenAPIURL == "" {
		writeErr(w, 400, "Marketplace.InvalidListing", "name, category (IMAGE/SAAS/SERVICE) and openapiUrl are required")
		return
	}
	if !settlement.Rate(req.PartnerRateBps).Valid() {
		writeErr(w, 400, "Marketplace.InvalidRate", "partnerRateBps must be in [0, 10000]")
		return
	}
	now := a.now()
	l := &Listing{
		PartnerID:      partnerID,
		Name:           req.Name,
		Category:       req.Category,
		OpenAPIURL:     req.OpenAPIURL,
		Status:         StatusPendingApproval, // publish → review queue, not straight to shelf
		PartnerRateBps: req.PartnerRateBps,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := a.store.CreateListing(l); err != nil {
		writeErr(w, 500, "Common.InternalError", "failed to publish listing")
		return
	}
	writeOK(w, l)
}

// approveReq is POST /api/v1/marketplace/listings/{id}/approve.
type approveReq struct {
	Approved   bool   `json:"approved"`
	ReviewNote string `json:"reviewNote"`
}

func (a *app) handleApprove(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountFrom(w, r); !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "Common.InvalidParameter", "invalid listing id")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req approveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "Common.InvalidParameter", "malformed body")
		return
	}
	to := StatusApproved
	if !req.Approved {
		to = StatusRejected
	}
	// Review may only act on the review queue — a DRAFT (never published) or an
	// already-approved listing cannot be re-approved through this path.
	l, err := a.store.UpdateListingStatus(id, StatusPendingApproval, to)
	if err != nil {
		writeErr(w, 409, "Marketplace.InvalidTransition", err.Error())
		return
	}
	writeOK(w, l)
}

func (a *app) handleList(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountFrom(w, r); !ok {
		return
	}
	category := ListingCategory(r.URL.Query().Get("category"))
	if category != "" && !validCategory(category) {
		writeErr(w, 400, "Marketplace.InvalidCategory", "category must be IMAGE/SAAS/SERVICE")
		return
	}
	writeOK(w, map[string]any{"listings": a.store.ListApproved(category)})
}

// settleReq is POST /api/v1/marketplace/settlements.
type settleReq struct {
	OrderID    string `json:"orderId"`
	ListingID  int64  `json:"listingId"`
	GrossMicro int64  `json:"grossMicro"` // micro-units (pricing.Amount)
}

func (a *app) handleSettle(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountFrom(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req settleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "Common.InvalidParameter", "malformed body")
		return
	}
	if req.OrderID == "" || req.GrossMicro <= 0 {
		writeErr(w, 400, "Marketplace.InvalidOrder", "orderId and a positive grossMicro are required")
		return
	}
	// Idempotent: settling the same order twice returns the same split, never a
	// double payout.
	if existing, ok := a.store.GetSettlementByOrder(req.OrderID); ok {
		writeOK(w, existing)
		return
	}
	l, err := a.store.GetListing(req.ListingID)
	if err != nil || l.Status != StatusApproved {
		writeErr(w, 409, "Marketplace.ListingNotApproved", "settlement requires an APPROVED listing")
		return
	}
	gross := pricing.Amount(req.GrossMicro)
	split, err := settlement.Settle(gross, settlement.Rate(l.PartnerRateBps))
	if err != nil {
		writeErr(w, 500, "Common.InternalError", "settlement split failed: "+err.Error())
		return
	}
	rec := &SettlementRecord{
		OrderID:        req.OrderID,
		ListingID:      l.ListingID,
		PartnerID:      l.PartnerID,
		Gross:          gross,
		PartnerRateBps: l.PartnerRateBps,
		Partner:        split.Partner,
		Platform:       split.Platform,
		SettledAt:      a.now(),
	}
	if err := a.store.CreateSettlement(rec); err != nil {
		writeErr(w, 409, "Marketplace.AlreadySettled", err.Error())
		return
	}
	writeOK(w, rec)
}

// --- JSON helpers -----------------------------------------------------------

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

// --- middleware -------------------------------------------------------------

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Sc-TraceId")
		if id == "" {
			id = fmt.Sprintf("mkt-%d", time.Now().UnixNano())
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
				writeErr(w, 500, "Common.InternalError", "internal error")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// newMux wires the routes. Extracted from main so the handler suite can be
// exercised over httptest without binding a port.
func newMux(a *app) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/marketplace/listings", a.handlePublish)
	mux.HandleFunc("POST /api/v1/marketplace/listings/{id}/approve", a.handleApprove)
	mux.HandleFunc("GET /api/v1/marketplace/listings", a.handleList)
	mux.HandleFunc("POST /api/v1/marketplace/settlements", a.handleSettle)
	return recoverMiddleware(requestIDMiddleware(mux))
}

func main() {
	addr := flag.String("http", ":9212", "HTTP listen address")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	a := newApp(newMemoryStore())
	srv := &http.Server{Addr: *addr, Handler: newMux(a), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("svc-marketplace listening", "addr", *addr)
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
