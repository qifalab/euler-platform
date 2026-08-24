// Package main: console-bff fan-out aggregation store.
//
// console-bff is the control-plane aggregation layer (03§4, console-bff node
// in the 03§3 diagram). Unlike a stub, this store fans out to the internal
// services behind it and aggregates their responses into the shapes the web
// console needs:
//
//   - resources + resource count → svc-orchestrator (/api/v1/orchestrator/resources)
//   - balance → svc-billing (/internal/balance)
//   - bills → svc-billing (/internal/bills, one period per call)
//   - pending orders → svc-order (/api/v1/orders)
//
// Internal service base URLs come from env (SC_SVC_*), defaulting to the dev
// ports. The account id is gateway-injected (X-Sc-Account-Id header) and
// forwarded verbatim to each downstream service — the BFF never falls back to a
// client-supplied account in the body (07§3.3).
//
// Each handler emits the platform envelope {RequestId, Code, Message, Data}
// (03§9.3). A downstream that is unreachable returns 503 Common.UpstreamUnavailable
// rather than silently degrading to stale data — a half-real BFF is worse than
// an honest error (the whole point of wiring fan-out is that what the console
// shows IS what the services have).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/starcloud/sc-platform/errors"
)

// accountIDHeader is injected by the APISIX gateway on every request. The BFF
// TRUSTS this header (network isolation behind the gateway); services can
// additionally enable a shared-secret check via SC_INTERNAL_TOKEN.
const accountIDHeader = "X-Sc-Account-Id"

// maxBodyBytes caps JSON request bodies accepted by the BFF's mutation handlers.
const maxBodyBytes = 1 << 20 // 1 MiB

// consoleStore is a fan-out client: it holds nothing of its own except the
// downstream base URLs and a shared HTTP client. In the target architecture
// the BFF also caches aggregated views in Redis (03§4 console-bff storage);
// phase-1 fan-out is live, caching is not.
type consoleStore struct {
	httpClient      *http.Client
	orchestratorURL string // e.g. http://localhost:9203
	billingURL      string // e.g. http://localhost:9210 (9206 is svc-metering)
	orderURL        string // e.g. http://localhost:9204
	catalogURL      string // e.g. http://localhost:9207
}

func newConsoleStore() *consoleStore {
	return &consoleStore{
		httpClient:      &http.Client{Timeout: 5 * time.Second},
		orchestratorURL: envOrDefault("SC_SVC_ORCHESTRATOR_URL", "http://localhost:9203"),
		billingURL:      envOrDefault("SC_SVC_BILLING_URL", "http://localhost:9210"),
		orderURL:        envOrDefault("SC_SVC_ORDER_URL", "http://localhost:9204"),
		catalogURL:      envOrDefault("SC_SVC_CATALOG_URL", "http://localhost:9207"),
	}
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// --- HTTP helpers -------------------------------------------------------------

// accountIDFrom extracts the caller's account id from the gateway-injected
// header. A missing/blank header is a 403 — the BFF must never fall back to a
// client-supplied account in the body (07§3.3).
func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.Header.Get(accountIDHeader))
	if raw == "" {
		writeError(w, errorsx.New("Common.MissingAccountId", errorsx.StatusForbidden,
			"X-Sc-Account-Id header is required (injected by gateway)"))
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest,
			"malformed X-Sc-Account-Id"))
		return 0, false
	}
	return id, true
}

// requestID reads the trace id the middleware set on the response header, so
// the envelope's RequestId matches X-Sc-TraceId. Get canonicalizes the key
// (Go stores headers canonicalized, e.g. "X-Sc-Traceid").
func requestID(w http.ResponseWriter) string {
	return w.Header().Get("X-Sc-TraceId")
}

// envelope is the platform response body {RequestId, Code, Message, Data}
// (03§9.3). Data/Message are omitted when empty so success responses stay tidy.
type envelope struct {
	RequestId string `json:"RequestId"`
	Code      string `json:"Code"`
	Message   string `json:"Message,omitempty"`
	Data      any    `json:"Data,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{RequestId: requestID(w), Code: "OK", Data: data})
}

func writeError(w http.ResponseWriter, e *errorsx.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.HTTPStatus)
	_ = json.NewEncoder(w).Encode(envelope{RequestId: requestID(w), Code: e.Code, Message: e.Message})
}

// upstreamEnvelope mirrors the {RequestId,Code,Message,Data} body every internal
// service returns (03§9.3). Only Data is consumed by the BFF.
type upstreamEnvelope struct {
	Code    string          `json:"Code"`
	Message string          `json:"Message"`
	Data    json.RawMessage `json:"Data"`
}

// getJSON calls a downstream service and unwraps its response. The account id
// header is forwarded so the downstream sees the same caller identity. A
// transport error or non-2xx status becomes a 503 Common.UpstreamUnavailable —
// never a silent fallback (see package doc).
//
// Two wire conventions exist among the phase-1 services and both are handled:
//  1. Platform envelope {Code,Message,Data} (svc-orchestrator, svc-order) —
//     a 2xx with Code=="OK" is unwrapped to Data; a non-OK Code is surfaced.
//  2. Bare body (svc-billing success) — the JSON is the payload directly;
//     errors still use {Code,Message} with a non-2xx status.
// getJSON detects by whether the body has a top-level "Data" field AND a
// "Code" field. Bare bodies without those keys are taken as-is.
func (s *consoleStore) getJSON(ctx context.Context, url string, accountID int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errorsx.New("Common.UpstreamUnavailable", errorsx.StatusUnavailable, "无法构造下游请求")
	}
	req.Header.Set(accountIDHeader, strconv.FormatInt(accountID, 10))

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return errorsx.New("Common.UpstreamUnavailable", errorsx.StatusUnavailable,
			"下游服务不可达: "+url)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Try the envelope shape first. svc-orchestrator/svc-order wrap every
	// response; svc-billing wraps only errors. A body that decodes into an
	// envelope with a non-empty Code is treated as envelope-shaped.
	var env upstreamEnvelope
	if jsonErr := json.Unmarshal(body, &env); jsonErr == nil && env.Code != "" {
		if resp.StatusCode >= 500 {
			return errorsx.New("Common.UpstreamUnavailable", errorsx.StatusUnavailable,
				fmt.Sprintf("下游响应异常 (HTTP %d): %s", resp.StatusCode, url))
		}
		if resp.StatusCode != 200 || env.Code != "OK" {
			status := resp.StatusCode
			if status < 400 {
				status = http.StatusBadGateway
			}
			return errorsx.New(env.Code, status, env.Message)
		}
		// Envelope success: unwrap Data. If Data is absent/empty, leave out as-is.
		if out == nil || len(env.Data) == 0 {
			return nil
		}
		return json.Unmarshal(env.Data, out)
	}

	// Bare-body convention (svc-billing success). A non-2xx here is an error
	// even though it lacked the envelope Code field.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorsx.New("Common.UpstreamUnavailable", errorsx.StatusUnavailable,
			fmt.Sprintf("下游响应异常 (HTTP %d): %s", resp.StatusCode, url))
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

// --- console aggregation handlers ---------------------------------------------

// handleOverview aggregates the account overview the console home renders:
// cash balance (svc-billing), resource count (svc-orchestrator), pending
// orders (svc-order) — fanned out concurrently (03§4). Each leg is
// independent; a leg failure fails the whole overview (no partial/seed data).
func (s *consoleStore) handleOverview(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	type balanceResp struct {
		Available string `json:"Available"`
		Frozen    string `json:"Frozen"`
		Total     string `json:"Total"`
	}
	type resourceRow struct {
		ResourceId string `json:"resourceId"`
	}
	type orderRow struct {
		State string `json:"state"`
	}

	var (
		bal      balanceResp
		resources []resourceRow
		orders    []orderRow
		balErr, resErr, ordErr error
	)

	// Concurrent fan-out: each leg writes to its own slot; no shared state.
	var done [3]chan struct{}
	for i := range done {
		done[i] = make(chan struct{})
	}
	go func() { balErr = s.getJSON(ctx, s.billingURL+"/internal/balance", acct, &bal); close(done[0]) }()
	go func() { resErr = s.getJSON(ctx, s.orchestratorURL+"/api/v1/orchestrator/resources", acct, &resources); close(done[1]) }()
	go func() { ordErr = s.getJSON(ctx, s.orderURL+"/api/v1/orders", acct, &orders); close(done[2]) }()
	<-done[0]; <-done[1]; <-done[2]

	for _, e := range []error{balErr, resErr, ordErr} {
		if e != nil {
			// comma-ok narrowing: getJSON should only return *errorsx.Error, but a
			// bad assertion must degrade to a wrapped internal error, not a panic.
			writeError(w, e2ptr(e))
			return
		}
	}

	var pending int64
	for _, o := range orders {
		if strings.EqualFold(o.State, "PENDING_PAYMENT") {
			pending++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"AccountId": acct,
		"Balance": map[string]any{
			"Available": bal.Available,
			"Frozen":    bal.Frozen,
			"Total":     bal.Total,
		},
		"ResourceCount": map[string]any{
			"Total": len(resources),
		},
		"PendingOrders": pending,
	})
}

// handleResources returns the account's resource list, fanned out to
// svc-orchestrator (03§4 console 资源列表). Orchestrator returns camelCase;
// the BFF re-maps to the PascalCase contract the frontend already consumes
// (sdk.ts ResourceItem), so the frontend does not change.
func (s *consoleStore) handleResources(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	type orchResource struct {
		ResourceId   string `json:"resourceId"`
		ProductCode  string `json:"productCode"`
		Region       string `json:"region"`
		ChargeType   string `json:"chargeType"`
		State        string `json:"state"`
		SpecCode     string `json:"specCode"`
		BillingStart string `json:"billingStart"`
		CreatedAt    string `json:"createdAt"`
		ExpiredAt    string `json:"expiredAt"`
	}
	var orch []orchResource
	if err := s.getJSON(ctx, s.orchestratorURL+"/api/v1/orchestrator/resources", acct, &orch); err != nil {
		writeError(w, e2ptr(err))
		return
	}

	res := make([]map[string]any, 0, len(orch))
	for _, o := range orch {
		res = append(res, map[string]any{
			"ResourceId":   o.ResourceId,
			"ProductCode":  o.ProductCode,
			"Region":       o.Region,
			"ChargeType":   o.ChargeType,
			"State":        o.State,
			"SpecCode":     o.SpecCode,
			"BillingStart": o.BillingStart,
			"ExpiredAt":    o.ExpiredAt,
			"CreatedAt":    o.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, res)
}

// handleBills returns the account's bill list, one row per period, fanned out
// to svc-billing (/internal/bills, which returns a single period per call).
// The BFF queries the current period + the preceding 5 (6 total) and returns
// them as a list (03§4 账单列表). svc-billing produces a zero-amount bill for
// periods with no charges, which surfaces as a legitimate "暂无账单" empty state.
func (s *consoleStore) handleBills(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	now := time.Now().UTC()
	periods := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		t := now.AddDate(0, -i, 0)
		periods = append(periods, t.Format("2006-01"))
	}

	type billRow struct {
		BillPeriod        string `json:"BillPeriod"`
		TotalAmount       string `json:"TotalAmount"`
		PaidAmount        string `json:"PaidAmount"`
		ChargeCount       int    `json:"ChargeCount"`
		IncompleteCharges int    `json:"IncompleteCharges"`
		UnreconciledCount int    `json:"UnreconciledCount"`
		Final             bool   `json:"Final"`
	}

	// Query each period concurrently; collect the rows that come back.
	results := make([]*billRow, len(periods))
	errs := make([]error, len(periods))
	var wg sync.WaitGroup
	for i, p := range periods {
		wg.Add(1)
		go func(idx int, period string) {
			defer wg.Done()
			var row billRow
			u := s.billingURL + "/internal/bills?period=" + url.QueryEscape(period)
			if err := s.getJSON(ctx, u, acct, &row); err != nil {
				errs[idx] = err
				return
			}
			// svc-billing may return the row with a zeroed period if it normalises;
			// stamp it from what we asked for so the list is always labelled.
			if row.BillPeriod == "" {
				row.BillPeriod = period
			}
			results[idx] = &row
		}(i, p)
	}
	wg.Wait()

	// If every period failed (e.g. svc-billing down), surface the error. A
	// single period erroring is tolerated (its slot stays nil) so one bad
	// period does not blank the whole list.
	allFailed := true
	for i := range periods {
		if errs[i] == nil {
			allFailed = false
			break
		}
	}
	if allFailed && len(periods) > 0 {
		writeError(w, e2ptr(errs[0]))
		return
	}

	bills := make([]map[string]any, 0, len(results))
	for _, row := range results {
		if row == nil {
			continue
		}
		bills = append(bills, map[string]any{
			"BillPeriod":        row.BillPeriod,
			"TotalAmount":       row.TotalAmount,
			"PaidAmount":        row.PaidAmount,
			"ChargeCount":       row.ChargeCount,
			"IncompleteCharges": row.IncompleteCharges,
			"UnreconciledCount": row.UnreconciledCount,
			"Final":             row.Final,
		})
	}
	writeJSON(w, http.StatusOK, bills)
}

// --- error helpers ------------------------------------------------------------

// postJSON forwards a POST to a downstream service, streaming the request body
// through and unwrapping the response with the same envelope/bare-body logic as
// getJSON. It forwards the account-id header verbatim. Used for the phase-2
// billing mutations (resource-pack purchase, invoice draft/issue/void) which
// the BFF proxies rather than aggregates — the BFF never invents billing state.
func (s *consoleStore) postJSON(ctx context.Context, url string, accountID int64, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return errorsx.New("Common.UpstreamUnavailable", errorsx.StatusUnavailable, "无法构造下游请求")
	}
	req.Header.Set(accountIDHeader, strconv.FormatInt(accountID, 10))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return errorsx.New("Common.UpstreamUnavailable", errorsx.StatusUnavailable, "下游服务不可达: "+url)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	// svc-billing bare-body on success, envelope {Code,Message,Data} on error.
	var env upstreamEnvelope
	if jsonErr := json.Unmarshal(respBody, &env); jsonErr == nil && env.Code != "" {
		if resp.StatusCode >= 500 {
			return errorsx.New("Common.UpstreamUnavailable", errorsx.StatusUnavailable, fmt.Sprintf("下游响应异常 (HTTP %d): %s", resp.StatusCode, url))
		}
		if resp.StatusCode != 200 || env.Code != "OK" {
			status := resp.StatusCode
			if status < 400 {
				status = http.StatusBadGateway
			}
			return errorsx.New(env.Code, status, env.Message)
		}
		if out == nil || len(env.Data) == 0 {
			return nil
		}
		return json.Unmarshal(env.Data, out)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorsx.New("Common.UpstreamUnavailable", errorsx.StatusUnavailable, fmt.Sprintf("下游响应异常 (HTTP %d): %s", resp.StatusCode, url))
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

// --- phase-2 billing-form handlers (M-4.4) -----------------------------------

// handleReservePacks lists the account's active resource packs (fan-out
// svc-billing /internal/reservepacks).
func (s *consoleStore) handleReservePacks(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var raw map[string]any
	if err := s.getJSON(r.Context(), s.billingURL+"/internal/reservepacks", acct, &raw); err != nil {
		writeError(w, e2ptr(err))
		return
	}
	writeJSON(w, http.StatusOK, raw)
}

type reservePackPurchaseReq struct {
	PackID      string `json:"packId"`
	ProductCode string `json:"productCode"`
	SKUCode     string `json:"skuCode"`
	FaceValue   string `json:"faceValue"`
	ExpireAt    string `json:"expireAt"`
	OrderKey    string `json:"orderKey"`
}

// handleReservePackPurchase proxies a resource-pack purchase to svc-billing.
func (s *consoleStore) handleReservePackPurchase(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req reservePackPurchaseReq
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest, "malformed request"))
		return
	}
	body, _ := json.Marshal(req)
	var out map[string]any
	if err := s.postJSON(r.Context(), s.billingURL+"/internal/reservepacks", acct, bytes.NewReader(body), &out); err != nil {
		writeError(w, e2ptr(err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleInvoices lists the account's invoices (fan-out svc-billing).
func (s *consoleStore) handleInvoices(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var raw map[string]any
	if err := s.getJSON(r.Context(), s.billingURL+"/internal/invoices", acct, &raw); err != nil {
		writeError(w, e2ptr(err))
		return
	}
	writeJSON(w, http.StatusOK, raw)
}

type invoiceDraftReq struct {
	InvoiceID  string             `json:"invoiceId"`
	BillPeriod string             `json:"billPeriod"`
	Title      string             `json:"title"`
	TaxNo      string             `json:"taxNo"`
	Items      []invoiceDraftItem `json:"items"`
}
type invoiceDraftItem struct {
	Description string `json:"description"`
	Amount      string `json:"amount"`
}

// handleInvoiceDraft proxies an invoice draft to svc-billing.
func (s *consoleStore) handleInvoiceDraft(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req invoiceDraftReq
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest, "malformed request"))
		return
	}
	body, _ := json.Marshal(req)
	var out map[string]any
	if err := s.postJSON(r.Context(), s.billingURL+"/internal/invoices", acct, bytes.NewReader(body), &out); err != nil {
		writeError(w, e2ptr(err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleInvoiceIssue proxies an invoice issue to svc-billing.
func (s *consoleStore) handleInvoiceIssue(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		InvoiceID string `json:"invoiceId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest, "malformed request"))
		return
	}
	body, _ := json.Marshal(req)
	var out map[string]any
	if err := s.postJSON(r.Context(), s.billingURL+"/internal/invoices/issue", acct, bytes.NewReader(body), &out); err != nil {
		writeError(w, e2ptr(err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleInvoiceVoid proxies a 红冲 reversal to svc-billing.
func (s *consoleStore) handleInvoiceVoid(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		OriginalID string `json:"originalId"`
		ReversalID string `json:"reversalId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest, "malformed request"))
		return
	}
	body, _ := json.Marshal(req)
	var out map[string]any
	if err := s.postJSON(r.Context(), s.billingURL+"/internal/invoices/void", acct, bytes.NewReader(body), &out); err != nil {
		writeError(w, e2ptr(err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCostAnalysis returns the account's cost breakdown (fan-out svc-billing
// /internal/cost-analysis, scoped to a billing period).
func (s *consoleStore) handleCostAnalysis(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = time.Now().UTC().Format("2006-01")
	}
	var raw map[string]any
	u := s.billingURL + "/internal/cost-analysis?period=" + url.QueryEscape(period)
	if err := s.getJSON(r.Context(), u, acct, &raw); err != nil {
		writeError(w, e2ptr(err))
		return
	}
	writeJSON(w, http.StatusOK, raw)
}

// --- error helpers (original) ------------------------------------------------

// handleSearch implements GET /console/search?q= — the console ⌘K palette's
// backend (02§7.1). It fans out concurrently to the account's resources
// (svc-orchestrator) and the product catalogue (svc-catalog) and server-side
// filters both by the query against the fields a user can see (resource id /
// spec / state; product code / name / category). Catalogue search is
// anonymous-grade data, but the resource leg is account-scoped, so the whole
// endpoint stays behind the account-id gate.
func (s *consoleStore) handleSearch(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	ctx := r.Context()

	type orchResource struct {
		ResourceId  string `json:"resourceId"`
		ProductCode string `json:"productCode"`
		Region      string `json:"region"`
		State       string `json:"state"`
		SpecCode    string `json:"specCode"`
	}
	type catalogProduct struct {
		ProductCode string `json:"productCode"`
		ProductName string `json:"productName"`
		Category    string `json:"category"`
		Description string `json:"description"`
	}

	var (
		resources []orchResource
		products  []catalogProduct
		resErr    error
		prodErr   error
	)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		resErr = s.getJSON(ctx, s.orchestratorURL+"/api/v1/orchestrator/resources", acct, &resources)
	}()
	go func() {
		defer wg.Done()
		prodErr = s.getJSON(ctx, s.catalogURL+"/api/v1/catalog/products", acct, &products)
	}()
	wg.Wait()
	// The palette degrades per-leg, not all-or-nothing: a down catalogue must
	// not blank resource hits (and vice versa). Only both-down is an error.
	if resErr != nil && prodErr != nil {
		writeError(w, e2ptr(resErr))
		return
	}

	matches := func(fields ...string) bool {
		if q == "" {
			return true
		}
		for _, f := range fields {
			if strings.Contains(strings.ToLower(f), q) {
				return true
			}
		}
		return false
	}

	resHits := make([]map[string]any, 0)
	if resErr == nil {
		for _, r0 := range resources {
			if matches(r0.ResourceId, r0.SpecCode, r0.State, r0.ProductCode) {
				resHits = append(resHits, map[string]any{
					"resourceId": r0.ResourceId, "productCode": r0.ProductCode,
					"region": r0.Region, "state": r0.State, "specCode": r0.SpecCode,
				})
			}
		}
	}
	prodHits := make([]map[string]any, 0)
	if prodErr == nil {
		for _, p := range products {
			if matches(p.ProductCode, p.ProductName, p.Category) {
				prodHits = append(prodHits, map[string]any{
					"productCode": p.ProductCode, "productName": p.ProductName,
					"category": p.Category, "description": p.Description,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":     q,
		"resources": resHits,
		"products":  prodHits,
	})
}

// e2ptr narrows an error returned by getJSON back to the *errorsx.Error it
// always produces. getJSON only ever returns *errorsx.Error, so this is safe.
func e2ptr(err error) *errorsx.Error {
	if err == nil {
		return errorsx.New("Common.InternalError", errorsx.StatusInternalError, "")
	}
	if e, ok := err.(*errorsx.Error); ok {
		return e
	}
	return errorsx.New("Common.InternalError", errorsx.StatusInternalError, err.Error())
}
