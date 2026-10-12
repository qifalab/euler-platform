package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/qifalab/euler-platform/httpmw"
)

// In-memory fakes of svc-catalog, svc-order, svc-orchestrator and svc-monitor
// with the routes, JSON shapes, status codes and error codes of the real
// handlers (services/*/cmd/server), so the facade is exercised end to end over
// HTTP without the real services.

const (
	testToken = "internal-token-for-tests"
	acctA     = int64(1001)
	acctB     = int64(1002)
	acctPoor  = int64(1003)
	// leakedInternals is what a broken downstream answers; it must never
	// reach an API caller.
	leakedInternals = "dial tcp 10.0.0.12:3306: connection refused"
)

type fakeErr struct {
	status    int
	code, msg string
}

type fakeOrder struct {
	orderRecord
	account int64
	token   string
	paid    bool
}

type fakeResource struct {
	resourceRecord
	account int64
	orderID int64
}

type fakeRule struct {
	alarmRule
	account int64
}

type fakeCloud struct {
	t  *testing.T
	mu sync.Mutex

	skus    []catalogSKU
	regions []catalogRegion
	balance map[int64]int64 // 分

	orders       map[int64]*fakeOrder
	nextOrder    int64
	ordersMade   int
	payments     int
	resources    map[string]*fakeResource
	seq          int
	rules        map[int64]*fakeRule
	nextRule     int64
	traces       map[string]bool
	missingToken int

	// Knobs.
	provisionAsync bool            // fulfil leaves a new resource CREATING (202)
	fulfilErr      *fakeErr        // answered by the next fulfil
	metricsErr     *fakeErr        // answered by the next metrics query
	metricsQueries []url.Values    // every metrics query svc-monitor received
	down           map[string]bool // service name → answers 500 with internals

	catalogSrv, orderSrv, orchSrv, monitorSrv *httptest.Server
}

func newFakeCloud(t *testing.T) *fakeCloud {
	f := &fakeCloud{
		t: t,
		skus: []catalogSKU{
			{"euecs.s2.small.postpaid", "euecs", chargePostpaid, "1"},
			{"euecs.s2.large.prepaid", "euecs", chargePrepaid, "1"},
			{"euecs.s2.large.postpaid", "euecs", chargePostpaid, "1"},
			{"euecs.s2.xlarge.postpaid", "euecs", chargePostpaid, "1"},
			{"euecs.s2.legacy.postpaid", "euecs", chargePostpaid, "2"}, // off sale
			{"eubs.essd.prepaid", "eubs", chargePrepaid, "1"},
			{"eubs.essd.postpaid", "eubs", chargePostpaid, "1"},
			{"euoss.standard.postpaid", "euoss", chargePostpaid, "1"},
			{"eurds.mysql8.small.prepaid", "eurds", chargePrepaid, "1"},
			{"eurds.mysql8.small.postpaid", "eurds", chargePostpaid, "1"},
			{"eueip.bandwidth.prepaid", "eueip", chargePrepaid, "1"},
			{"eueip.traffic.postpaid", "eueip", chargePostpaid, "1"},
			{"euvpc.standard.postpaid", "euvpc", chargePostpaid, "1"},
		},
		balance:   map[int64]int64{acctA: 100000, acctB: 100000, acctPoor: 0},
		orders:    map[int64]*fakeOrder{},
		nextOrder: 7000,
		resources: map[string]*fakeResource{},
		rules:     map[int64]*fakeRule{},
		nextRule:  100,
		traces:    map[string]bool{},
		down:      map[string]bool{},
	}
	for _, rg := range []string{"cn-north-1", "cn-east-1"} {
		r := catalogRegion{RegionID: rg}
		for _, l := range []string{"a", "b"} {
			r.Zones = append(r.Zones, struct {
				ZoneID string `json:"zoneId"`
			}{rg + "-" + l})
		}
		f.regions = append(f.regions, r)
	}

	cat := http.NewServeMux()
	cat.HandleFunc("GET /api/v1/catalog/skus", f.handleSKUs)
	cat.HandleFunc("GET /api/v1/catalog/regions", f.handleRegions)
	ord := http.NewServeMux()
	ord.HandleFunc("POST /api/v1/orders", f.handleCreateOrder)
	ord.HandleFunc("POST /api/v1/orders/{id}/pay", f.handlePay)
	orch := http.NewServeMux()
	orch.HandleFunc("POST /api/v1/orchestrator/fulfill", f.handleFulfil)
	orch.HandleFunc("GET /api/v1/orchestrator/resources", f.handleListResources)
	orch.HandleFunc("GET /api/v1/orchestrator/resources/{id}", f.handleResource)
	orch.HandleFunc("POST /api/v1/orchestrator/resources/{id}/actions", f.handleAction)
	orch.HandleFunc("POST /api/v1/orchestrator/resources/{id}/release", f.handleRelease)
	orch.HandleFunc("PATCH /api/v1/orchestrator/resources/{id}/attributes", f.handlePatch)
	mon := http.NewServeMux()
	mon.HandleFunc("GET /api/v1/monitor/rules", f.handleListRules)
	mon.HandleFunc("POST /api/v1/monitor/rules", f.handleCreateRule)
	mon.HandleFunc("PUT /api/v1/monitor/rules/{id}", f.handleUpdateRule)
	mon.HandleFunc("DELETE /api/v1/monitor/rules/{id}", f.handleDeleteRule)
	mon.HandleFunc("GET /api/v1/monitor/metrics", f.handleMetrics)

	f.catalogSrv = f.serve("svc-catalog", cat)
	f.orderSrv = f.serve("svc-order", ord)
	f.orchSrv = f.serve("svc-orchestrator", orch)
	f.monitorSrv = f.serve("svc-monitor", mon)
	t.Cleanup(func() {
		for _, s := range []*httptest.Server{f.catalogSrv, f.orderSrv, f.orchSrv, f.monitorSrv} {
			s.Close()
		}
	})
	return f
}

// serve wraps a fake service with the checks every platform service applies:
// the internal token and the account header.
func (f *fakeCloud) serve(name string, mux *http.ServeMux) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		tokenOK := r.Header.Get(httpmw.InternalTokenHeader) == testToken
		if !tokenOK {
			f.missingToken++
		}
		if tid := r.Header.Get(traceIDHeader); tid != "" {
			f.traces[tid] = true
		}
		down := f.down[name]
		f.mu.Unlock()
		switch {
		case !tokenOK:
			fakeWriteErr(w, http.StatusForbidden, "Common.Forbidden", "invalid internal token")
		case r.Header.Get(accountIDHeader) == "":
			fakeWriteErr(w, http.StatusForbidden, "Common.MissingAccountId", "X-Euler-Account-Id header is required")
		case down:
			fakeWriteErr(w, http.StatusInternalServerError, "Common.InternalError", leakedInternals)
		default:
			mux.ServeHTTP(w, r)
		}
	}))
}

func fakeWriteOK(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": "x", "Code": "OK", "Data": data})
}

func fakeWriteErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": "x", "Code": code, "Message": msg})
}

func fakeAccount(r *http.Request) int64 {
	n, _ := strconv.ParseInt(r.Header.Get(accountIDHeader), 10, 64)
	return n
}

func fakeDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, _ := io.ReadAll(r.Body)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fakeWriteErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed body: "+err.Error())
		return false
	}
	return true
}

// --- svc-catalog ---

func (f *fakeCloud) handleSKUs(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []catalogSKU{}
	for _, s := range f.skus {
		if p := r.URL.Query().Get("productCode"); p == "" || s.ProductCode == p {
			out = append(out, s)
		}
	}
	fakeWriteOK(w, http.StatusOK, out)
}

func (f *fakeCloud) handleRegions(w http.ResponseWriter, _ *http.Request) {
	fakeWriteOK(w, http.StatusOK, f.regions)
}

// --- svc-order ---

func (f *fakeCloud) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	var req createOrderBody
	if !fakeDecode(w, r, &req) {
		return
	}
	acct := fakeAccount(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.ClientToken != "" {
		for _, o := range f.orders {
			if o.account == acct && o.token == req.ClientToken {
				fakeWriteOK(w, http.StatusOK, o.orderRecord)
				return
			}
		}
	}
	var sku *catalogSKU
	for i := range f.skus {
		if f.skus[i].SKUCode == req.SKUCode && f.skus[i].ProductCode == req.ProductCode {
			sku = &f.skus[i]
		}
	}
	switch {
	case sku == nil:
		fakeWriteErr(w, http.StatusNotFound, "Catalog.SKUNotFound", "规格不存在或与产品不匹配")
		return
	case sku.Status != skuStatusOnSale:
		fakeWriteErr(w, http.StatusConflict, "Catalog.NotOnSale", "产品或规格未在售")
		return
	case req.Quantity != 1:
		fakeWriteErr(w, http.StatusBadRequest, "Order.QuantityUnsupported", "only 1 per order is supported")
		return
	}
	zone := req.ZoneID
	if req.ProductCode == "euvpc" || req.ProductCode == "euoss" {
		zone = "" // REGIONAL
	} else if zone == "" {
		fakeWriteErr(w, http.StatusBadRequest, "Order.ZoneRequired", "ZONAL product requires zoneId")
		return
	}
	f.nextOrder++
	f.ordersMade++
	o := &fakeOrder{account: acct, token: req.ClientToken, orderRecord: orderRecord{
		OrderID: f.nextOrder, OrderNo: fmt.Sprintf("SO2026%06d", f.nextOrder), State: orderPendingPayment,
		ProductCode: req.ProductCode, SKUCode: req.SKUCode, RegionID: req.RegionID, ZoneID: zone,
		ChargeType: sku.ChargeType, Duration: req.Duration,
	}}
	f.orders[o.OrderID] = o
	fakeWriteOK(w, http.StatusOK, o.orderRecord)
}

func (f *fakeCloud) handlePay(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	acct := fakeAccount(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[id]
	if o == nil || o.account != acct {
		fakeWriteErr(w, http.StatusNotFound, "Order.NotFound", "订单不存在")
		return
	}
	if o.paid {
		fakeWriteOK(w, http.StatusOK, o.orderRecord) // idempotent
		return
	}
	if o.State != orderPendingPayment {
		fakeWriteErr(w, http.StatusConflict, "Order.StateTransitionFailed", "order is not awaiting payment")
		return
	}
	price := int64(0)
	if o.ChargeType == chargePrepaid {
		price = 5000 * o.Duration
	}
	if f.balance[acct] < price {
		fakeWriteErr(w, http.StatusPaymentRequired, "Order.InsufficientBalance", "余额不足,请先充值")
		return
	}
	f.balance[acct] -= price
	f.payments++
	o.paid, o.State = true, "PAID"
	fakeWriteOK(w, http.StatusOK, o.orderRecord)
}

// --- svc-orchestrator ---

func (f *fakeCloud) liveOf(acct int64, product string) []*fakeResource {
	var out []*fakeResource
	for _, r := range f.resources {
		if (acct == 0 || r.account == acct) && r.ProductCode == product && r.State != stateReleased {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeCloud) handleFulfil(w http.ResponseWriter, r *http.Request) {
	var req fulfilBody
	if !fakeDecode(w, r, &req) {
		return
	}
	acct := fakeAccount(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[req.OrderID]
	switch {
	case o == nil || o.account != acct:
		fakeWriteErr(w, http.StatusNotFound, "Order.NotFound", "订单不存在")
		return
	case !o.paid:
		fakeWriteErr(w, http.StatusConflict, "Order.NotPaid", "订单状态 PENDING_PAYMENT,仅 PAID 可履约")
		return
	case req.ProductCode != o.ProductCode:
		fakeWriteErr(w, http.StatusConflict, "Order.ProductMismatch", "履约产品与订单不一致")
		return
	case req.SpecCode != "" && req.SpecCode != o.SKUCode:
		fakeWriteErr(w, http.StatusConflict, "Order.SpecMismatch", "履约规格与订单不一致")
		return
	}
	for _, res := range f.resources {
		if res.orderID == o.OrderID {
			f.writeFulfil(w, res)
			return
		}
	}
	if e := f.fulfilErr; e != nil {
		f.fulfilErr = nil
		fakeWriteErr(w, e.status, e.code, e.msg)
		return
	}
	switch o.ProductCode {
	case "euvpc":
		cidr := req.Params["cidrBlock"]
		if cidr == "" {
			fakeWriteErr(w, http.StatusBadRequest, "Resource.InvalidAttribute", "cidrBlock 为必填属性")
			return
		}
		for _, v := range f.liveOf(acct, "euvpc") {
			if v.Attributes["cidrBlock"] == cidr {
				fakeWriteErr(w, http.StatusConflict, "Resource.InvalidAttribute", "网段 "+cidr+" 与 VPC "+v.ResourceID+" 重叠")
				return
			}
		}
	case "euoss":
		for _, b := range f.liveOf(0, "euoss") {
			if b.Attributes["bucket"] == req.Params["bucket"] {
				fakeWriteErr(w, http.StatusConflict, "Resource.BucketExists", "存储桶名已被占用")
				return
			}
		}
	case "eurds":
		if v, ok := req.Params["engineVersion"]; ok && v != "5.7" && v != "8.0" {
			fakeWriteErr(w, http.StatusBadRequest, "Resource.InvalidAttribute", "engineVersion 须为 5.7 或 8.0")
			return
		}
	}
	state := "RUNNING"
	if f.provisionAsync {
		state = stateCreating
	}
	f.seq++
	attrs := map[string]string{}
	for k, v := range req.Params {
		attrs[k] = v
	}
	res := &fakeResource{account: acct, orderID: o.OrderID, resourceRecord: resourceRecord{
		ResourceID:  fmt.Sprintf("%s-%s-%08x", o.ProductCode, o.RegionID, o.OrderID),
		ProductCode: o.ProductCode, Region: o.RegionID, Zone: o.ZoneID, ChargeType: o.ChargeType,
		State: state, SpecCode: o.SKUCode, CreatedAt: fmt.Sprintf("2026-10-09T10:%02d:00Z", f.seq),
		Attributes: attrs,
	}}
	if o.ChargeType == chargePrepaid {
		res.ExpiredAt = "2026-11-09T10:00:00Z"
	}
	f.resources[res.ResourceID] = res
	o.State = "COMPLETED"
	f.writeFulfil(w, res)
}

func (f *fakeCloud) writeFulfil(w http.ResponseWriter, res *fakeResource) {
	status := http.StatusOK
	if res.State == stateCreating {
		status = http.StatusAccepted
	}
	fakeWriteOK(w, status, map[string]any{"resourceId": res.ResourceID, "state": res.State,
		"orderId": res.orderID, "orderState": "COMPLETED", "billingStart": "", "expiredAt": res.ExpiredAt})
}

func (f *fakeCloud) handleListResources(w http.ResponseWriter, r *http.Request) {
	acct := fakeAccount(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	// Like the real ledger: every product of the account, released ones
	// included (the facade filters).
	out := []resourceRecord{}
	for _, res := range f.resources {
		if res.account == acct {
			out = append(out, res.resourceRecord)
		}
	}
	fakeWriteOK(w, http.StatusOK, out)
}

// owned returns the caller's resource or answers 404.
func (f *fakeCloud) owned(w http.ResponseWriter, r *http.Request) *fakeResource {
	res := f.resources[r.PathValue("id")]
	if res == nil || res.account != fakeAccount(r) {
		fakeWriteErr(w, http.StatusNotFound, "Resource.NotFound", "资源不存在")
		return nil
	}
	return res
}

func (f *fakeCloud) handleResource(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if res := f.owned(w, r); res != nil {
		fakeWriteOK(w, http.StatusOK, res.resourceRecord)
	}
}

func (f *fakeCloud) handleAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action   string `json:"action"`
		SpecCode string `json:"specCode,omitempty"`
	}
	if !fakeDecode(w, r, &req) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	res := f.owned(w, r)
	if res == nil {
		return
	}
	refuse := func() {
		fakeWriteErr(w, http.StatusConflict, "Resource.StateTransitionFailed", "资源状态 "+res.State+" 不允许 "+req.Action)
	}
	switch req.Action {
	case "stop":
		if res.State != "RUNNING" {
			refuse()
			return
		}
		res.State = "STOPPED"
	case "start":
		if res.State != "STOPPED" {
			refuse()
			return
		}
		res.State = "RUNNING"
	case "restart":
		if res.State != "RUNNING" {
			refuse()
			return
		}
	case "upgrade":
		if res.ChargeType == chargePrepaid {
			fakeWriteErr(w, http.StatusConflict, "Resource.UpgradeRequiresOrder", "包年包月实例变更规格需通过变配订单支付差价")
			return
		}
		if res.State != "RUNNING" {
			refuse()
			return
		}
		res.SpecCode = req.SpecCode
	default:
		fakeWriteErr(w, http.StatusBadRequest, "Common.InvalidParameter", "action must be stop/start/restart/upgrade")
		return
	}
	fakeWriteOK(w, http.StatusOK, map[string]any{"resourceId": res.ResourceID, "state": res.State, "specCode": res.SpecCode})
}

func (f *fakeCloud) handleRelease(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := f.owned(w, r)
	if res == nil {
		return
	}
	if res.State == stateCreating || res.State == stateReleased {
		fakeWriteErr(w, http.StatusConflict, "Resource.StateTransitionFailed", "资源状态流转失败")
		return
	}
	res.State = stateReleased
	fakeWriteOK(w, http.StatusOK, map[string]any{"resourceId": res.ResourceID, "state": res.State})
}

func (f *fakeCloud) handlePatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Attributes map[string]string `json:"attributes"`
	}
	if !fakeDecode(w, r, &req) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	res := f.owned(w, r)
	if res == nil {
		return
	}
	for k, v := range req.Attributes {
		if k != "name" {
			fakeWriteErr(w, http.StatusBadRequest, "Resource.AttributeImmutable", "属性 "+k+" 不可修改")
			return
		}
		res.Attributes[k] = v
	}
	fakeWriteOK(w, http.StatusOK, res.resourceRecord)
}

// --- svc-monitor ---

func (f *fakeCloud) handleListRules(w http.ResponseWriter, r *http.Request) {
	acct := fakeAccount(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []alarmRule{}
	for _, ru := range f.rules {
		if ru.account == acct {
			out = append(out, ru.alarmRule)
		}
	}
	fakeWriteOK(w, http.StatusOK, out)
}

// fakeThreshold mirrors svc-monitor's normalizeThreshold contract: a decimal
// with at most 4 places.
func fakeThreshold(v string) bool {
	if _, err := strconv.ParseFloat(v, 64); err != nil {
		return false
	}
	if i := strings.IndexByte(v, '.'); i >= 0 && len(v)-i-1 > 4 {
		return false
	}
	return true
}

func (f *fakeCloud) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProductCode          string   `json:"productCode"`
		ResourceType         string   `json:"resourceType"`
		Metric               string   `json:"metric"`
		Threshold            string   `json:"threshold"`
		ComparisonOperator   int      `json:"comparisonOperator"`
		Period               int      `json:"period"`
		EvalPeriods          int      `json:"evalPeriods"`
		NotificationChannels []string `json:"notificationChannels"`
	}
	if !fakeDecode(w, r, &req) {
		return
	}
	if req.ProductCode == "" || req.Metric == "" || req.Threshold == "" {
		fakeWriteErr(w, http.StatusBadRequest, "Common.InvalidParameter", "productCode, metric, threshold are required")
		return
	}
	if !fakeThreshold(req.Threshold) {
		fakeWriteErr(w, http.StatusBadRequest, "Common.InvalidParameter", "threshold allows at most 4 decimal places")
		return
	}
	if req.Period == 0 {
		req.Period = 60
	}
	if req.EvalPeriods == 0 {
		req.EvalPeriods = 1
	}
	if req.ComparisonOperator == 0 {
		req.ComparisonOperator = 2
	}
	if len(req.NotificationChannels) == 0 {
		req.NotificationChannels = []string{"IN_APP"}
	}
	acct := fakeAccount(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextRule++
	ru := &fakeRule{account: acct, alarmRule: alarmRule{RuleID: f.nextRule, ProductCode: req.ProductCode,
		ResourceType: req.ResourceType, Metric: req.Metric, Threshold: req.Threshold,
		ComparisonOperator: req.ComparisonOperator, Period: req.Period, EvalPeriods: req.EvalPeriods,
		NotificationChannels: req.NotificationChannels, Status: 1, Version: 1,
		CreatedAt: "2026-10-09T10:00:00Z", UpdatedAt: "2026-10-09T10:00:00Z"}}
	f.rules[ru.RuleID] = ru
	fakeWriteOK(w, http.StatusOK, ru.alarmRule)
}

func (f *fakeCloud) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Threshold            *string   `json:"threshold"`
		ComparisonOperator   *int      `json:"comparisonOperator"`
		Period               *int      `json:"period"`
		EvalPeriods          *int      `json:"evalPeriods"`
		NotificationChannels *[]string `json:"notificationChannels"`
		Status               *int      `json:"status"`
		Version              int       `json:"version"`
	}
	if !fakeDecode(w, r, &req) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	ru := f.rules[id]
	switch {
	case ru == nil || ru.account != fakeAccount(r):
		fakeWriteErr(w, http.StatusNotFound, "Monitor.RuleNotFound", "告警规则不存在")
		return
	case req.Version <= 0:
		fakeWriteErr(w, http.StatusBadRequest, "Common.InvalidParameter", "version is required")
		return
	case ru.Version != req.Version:
		fakeWriteErr(w, http.StatusConflict, "Monitor.VersionConflict", "规则已被修改,请刷新后重试")
		return
	case req.Threshold != nil && !fakeThreshold(*req.Threshold):
		fakeWriteErr(w, http.StatusBadRequest, "Common.InvalidParameter", "threshold allows at most 4 decimal places")
		return
	}
	if req.Threshold != nil {
		ru.Threshold = *req.Threshold
	}
	if req.ComparisonOperator != nil {
		ru.ComparisonOperator = *req.ComparisonOperator
	}
	if req.Period != nil {
		ru.Period = *req.Period
	}
	if req.EvalPeriods != nil {
		ru.EvalPeriods = *req.EvalPeriods
	}
	if req.NotificationChannels != nil {
		ru.NotificationChannels = *req.NotificationChannels
	}
	if req.Status != nil {
		ru.Status = *req.Status
	}
	ru.Version++
	fakeWriteOK(w, http.StatusOK, ru.alarmRule)
}

func (f *fakeCloud) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	// Like svc-monitor: deleting a missing (or foreign) rule is not reported.
	if ru := f.rules[id]; ru != nil && ru.account == fakeAccount(r) {
		delete(f.rules, id)
	}
	fakeWriteOK(w, http.StatusOK, map[string]any{"ruleId": id, "deleted": true})
}

// handleMetrics mirrors svc-monitor's GET /api/v1/monitor/metrics contract
// (metrics.go, owners.go): resourceId required, the resource must be the
// caller's (any lifecycle state), period whole seconds in 10..86400.
func (f *fakeCloud) handleMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metricsQueries = append(f.metricsQueries, q)
	if e := f.metricsErr; e != nil {
		f.metricsErr = nil
		fakeWriteErr(w, e.status, e.code, e.msg)
		return
	}
	metric, resID := q.Get("metric"), q.Get("resourceId")
	switch metric {
	case "", "cpu_utilization", "memory_utilization", "request_count":
	default:
		fakeWriteErr(w, http.StatusBadRequest, "Common.InvalidParameter", "metric must be one of cpu_utilization, memory_utilization, request_count")
		return
	}
	if metric == "" {
		metric = "cpu_utilization"
	}
	if resID == "" {
		fakeWriteErr(w, http.StatusBadRequest, "Common.InvalidParameter", "resourceId is required")
		return
	}
	period := int64(60)
	if raw := q.Get("period"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 10 || n > 86400 {
			fakeWriteErr(w, http.StatusBadRequest, "Common.InvalidParameter", "period must be whole seconds between 10 and 86400")
			return
		}
		period = n
	}
	start, end := q.Get("start"), q.Get("end")
	if start == "" {
		start = "2026-10-09T09:01:00Z"
	}
	if end == "" {
		end = "2026-10-09T10:01:00Z"
	}
	if start >= end {
		fakeWriteErr(w, http.StatusBadRequest, "Common.InvalidParameter", "start must be before end")
		return
	}
	if res := f.resources[resID]; res == nil || res.account != fakeAccount(r) {
		fakeWriteErr(w, http.StatusNotFound, "Monitor.ResourceNotFound", "资源不存在")
		return
	}
	fakeWriteOK(w, http.StatusOK, map[string]any{
		"metric": metric, "resourceId": resID, "start": start, "end": end, "period": period, "source": "synthetic",
		"points": []map[string]any{
			{"timestamp": "2026-10-09T10:00:00Z", "value": 20},
			{"timestamp": "2026-10-09T10:01:00Z", "value": 27.5},
		},
	})
}

// --- the facade under test ---

type testEnv struct {
	t     *testing.T
	cloud *fakeCloud
	h     http.Handler
	n     int
}

func newTestEnv(t *testing.T) *testEnv {
	t.Setenv(httpmw.InternalTokenEnv, testToken)
	f := newFakeCloud(t)
	cfg := config{CatalogURL: f.catalogSrv.URL, OrderURL: f.orderSrv.URL, OrchestratorURL: f.orchSrv.URL,
		MonitorURL: f.monitorSrv.URL, DefaultRegion: "cn-north-1"}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, cloud: f, h: newHandler(newServer(cfg))}
}

type apiResult struct {
	status  int
	header  http.Header
	env     map[string]any
	code    string
	message string
	data    map[string]any
}

// request sends one Action the way the gateway forwards it.
func (e *testEnv) request(acct int64, product, action string, params map[string]any) *http.Request {
	body, err := json.Marshal(params)
	if err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/?Action="+url.QueryEscape(action)+"&Version=2026-08-01", bytes.NewReader(body))
	req.Host = product + ".api.euler.emoera.com"
	req.Header.Set(accountIDHeader, strconv.FormatInt(acct, 10))
	req.Header.Set(httpmw.InternalTokenHeader, testToken)
	e.n++
	req.Header.Set(traceIDHeader, fmt.Sprintf("trace-%d", e.n))
	return req
}

func (e *testEnv) send(req *http.Request) apiResult {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	res := apiResult{status: rec.Code, header: rec.Header()}
	if err := json.Unmarshal(rec.Body.Bytes(), &res.env); err != nil {
		e.t.Fatalf("response is not JSON: %v: %s", err, rec.Body.String())
	}
	res.code, _ = res.env["Code"].(string)
	res.message, _ = res.env["Message"].(string)
	res.data, _ = res.env["Data"].(map[string]any)
	return res
}

func (e *testEnv) do(acct int64, product, action string, params map[string]any) apiResult {
	e.t.Helper()
	return e.send(e.request(acct, product, action, params))
}

// ok runs an action that must succeed and returns its Data.
func (e *testEnv) ok(acct int64, product, action string, params map[string]any) map[string]any {
	e.t.Helper()
	r := e.do(acct, product, action, params)
	if r.status != http.StatusOK || r.code != "OK" || r.data == nil {
		e.t.Fatalf("%s %s: HTTP %d %v", product, action, r.status, r.env)
	}
	return r.data
}

// fail runs an action that must fail with the given status and code.
func (e *testEnv) fail(acct int64, product, action string, params map[string]any, status int, code string) apiResult {
	e.t.Helper()
	r := e.do(acct, product, action, params)
	if r.status != status || r.code != code {
		e.t.Fatalf("%s %s: got HTTP %d %q (%v), want HTTP %d %q", product, action, r.status, r.code, r.env, status, code)
	}
	if strings.Contains(r.message, "10.0.0.12") {
		e.t.Fatalf("%s %s leaked downstream internals: %q", product, action, r.message)
	}
	return r
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("field %s missing or not a string in %v", key, m)
	}
	return v
}

// single returns the one item of a Describe page's list.
func single(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	items := list(t, m, key)
	if len(items) != 1 {
		t.Fatalf("%s holds %d items, want 1: %v", key, len(items), m)
	}
	return items[0]
}

func list(t *testing.T, m map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := m[key].([]any)
	if !ok {
		t.Fatalf("field %s missing or not an array in %v", key, m)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		out = append(out, it.(map[string]any))
	}
	return out
}
