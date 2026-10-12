// Package main: svc-orchestrator — resource lifecycle & saga fulfilment
// (03-backend-services.md §4.3, orchestrator node).
//
// The orchestrator is the ONLY owner of the resource lifecycle ledger
// (03§5.2). A paid order triggers provisioning: it transitions the order to
// FULFILLING, creates the resource instance (CREATING), fans out to the
// ProvisionDriver (rc-compute for EUECS), and on success moves both to
// COMPLETED/RUNNING (billing starts at RUNNING per the D8 invariant).
//
// Phase-1 keeps an in-memory lifecycle ledger + an in-process provision driver
// (the real fan-out is gRPC to rc-* controllers; here we use the
// pkg-go/provision MockDriver so the loop runs end-to-end). The DDL in sql/
// is the production shape.
//
// Routes (gateway-authorized, X-Euler-Account-Id injected):
//
//	POST   /api/v1/orchestrator/fulfill          — fulfil a paid order (saga; body params → attributes)
//	GET    /api/v1/orchestrator/resources         — list the account's resources (with attributes; ?product= filters by product code)
//	GET    /api/v1/orchestrator/resources/{id}    — resource detail + lifecycle + attributes
//	PATCH  /api/v1/orchestrator/resources/{id}/attributes — change mutable attributes
//	POST   /api/v1/orchestrator/resources/{id}/release — release a resource
//	POST   /api/v1/orchestrator/resources/{id}/actions — user lifecycle actions (stop/start/restart/upgrade)
//	POST   /api/v1/orchestrator/resources/{id}/backups — snapshot an eubackup resource
//	GET    /api/v1/orchestrator/resources/{id}/backups — its live snapshots, newest first
//	DELETE /api/v1/orchestrator/resources/{id}/backups/{backupId} — delete a snapshot
//
// A background poller (poller.go) settles CREATING resources the fulfil call
// left provisioning, and compensates those stuck past the CREATING timeout.
//
// stdlib-HTTP service (repo convention). Envelope {RequestId,Code,Data} (03§9.3).
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

	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/identifier"
	"github.com/qifalab/euler-platform/order"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/provision"
	"github.com/qifalab/euler-platform/resource"
	"github.com/qifalab/euler-platform/runmode"
)

// accountIDHeader is injected by the API gateway after authentication. The
// service TRUSTS this header: it must only be reachable from the gateway /
// internal network (see internalTokenMiddleware for the optional shared-secret
// check), never exposed directly to the public internet.
const accountIDHeader = "X-Euler-Account-Id"

// maxBodyBytes caps JSON request bodies (defense against oversized payloads).
const maxBodyBytes = 1 << 20 // 1 MiB

// lifecycleStore wires the fulfilment saga to the resource ledger over HTTP.
// The ledger decides where instances live (resource_db when EULER_DB_DSN is set,
// in-memory otherwise); the order mirror stays in memory in both modes —
// order_main belongs to svc-order, and a second service writing it would be two
// writers on one aggregate (see resourceLedger's doc).
type lifecycleStore struct {
	mu sync.RWMutex
	// orders mirrors the orders this process has seen, for the saga's
	// PAID→FULFILLING→COMPLETED transitions. A restart drops in-flight mirrors;
	// the durable half of the saga is the resource row (state + order_id).
	orders map[int64]*order.Order
	ledger resourceLedger
	// driver is the service-level provision driver singleton (created once at
	// startup; MockDriver is internally synchronized, so it is shared safely
	// across requests without the store lock).
	driver provision.Driver
	// orderSrc is svc-order: fulfilment only proceeds for an order that exists
	// there, belongs to the caller and is PAID.
	orderSrc orderSource
	// orderSink is svc-order again: it is told how each saga settled, so the
	// order completes, or is refunded when provisioning failed.
	orderSink fulfilmentReporter
	// quota is svc-quota (occupy before provisioning, commit at RUNNING,
	// release on compensation); nil when quota is not enforced (dev without
	// EULER_SVC_QUOTA_URL — production refuses to start that way).
	quota quotaReserver
	// clock is the time source for decisions the tests must control: the
	// CREATING timeout, prepaid expiry and backup retention. The ledger's own
	// stamps (BillingStart, UpdatedAt) stay on the wall clock.
	clock func() time.Time
}

func newStore() *lifecycleStore {
	return newStoreWith(newMemLedger())
}

// newStoreWith wires a ledger. The demo seed runs only for the in-memory ledger:
// writing two demo instances into a shared resource_db would invent resources for
// account 100123 in every environment pointed at it.
func newStoreWith(ledger resourceLedger) *lifecycleStore {
	orders := newHTTPOrderSource()
	s := &lifecycleStore{
		orders:    make(map[int64]*order.Order),
		ledger:    ledger,
		driver:    provision.NewMockDriver(time.Now),
		orderSrc:  orders,
		orderSink: orders,
		quota:     newQuotaReserverFromEnv(),
		clock:     time.Now,
	}
	if _, mem := ledger.(*memLedger); mem {
		s.seed()
	}
	return s
}

func (s *lifecycleStore) seed() {
	// Seed one running instance (matches console-bff seed for account 100123).
	now := time.Now()
	inst := &resource.Instance{
		ResourceID: "euecs-cn-north-1-01-a1b2c3d4", AccountID: 100123,
		ProductCode: "euecs", Region: "cn-north-1", ChargeType: resource.ChargePrepaid,
		ResourceType: "instance", State: resource.StateRunning, SpecCode: "euecs.s2.large",
		BillingStart: now.Add(-72 * time.Hour), CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now, Version: 1,
	}
	if err := s.ledger.Create(inst); err != nil {
		panic(fmt.Sprintf("seed resource failed: %v", err))
	}

	// Seed one VPC (euvpc) so network-dependent wizards (euredis / eukafka
	// VPC dropdowns) list a real, placeable network from day one in dev.
	vpc := &resource.Instance{
		ResourceID: "euvpc-cn-north-1-01-vpc0a1b2c", AccountID: 100123,
		ProductCode: "euvpc", Region: "cn-north-1", ChargeType: resource.ChargePostpaid,
		ResourceType: "vpc", State: resource.StateRunning, SpecCode: "euvpc.standard",
		BillingStart: now.Add(-72 * time.Hour), CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now, Version: 1,
	}
	if err := s.ledger.Create(vpc); err != nil {
		panic(fmt.Sprintf("seed resource failed: %v", err))
	}
}

// --- fulfilment saga (03§4.3.3) ---
//
// 1. Validate order is PAID (only a paid order may provision — invariant).
// 2. Transition order PAID → FULFILLING.
// 3. Create resource instance (CREATING).
// 4. Fan out to ProvisionDriver (rc-compute). On success → RUNNING.
// 5. Transition order FULFILLING → COMPLETED.
// Compensation (on driver failure): reverse — mark resource CREATE_FAILED,
// order → REFUNDING. Compensation runs in reverse order (workflow invariant).
// 6. Once the resource has settled (RUNNING, or CREATE_FAILED after the
// compensation), report the outcome to svc-order, which owns the order:
// COMPLETED, or FAILED → it refunds the payment. Every replay of a settled
// fulfil re-sends the report, so a lost one heals on the next retry.

type fulfillRequest struct {
	OrderID     int64  `json:"orderId"`
	OrderNo     string `json:"orderNo"`
	ProductCode string `json:"productCode"`
	Region      string `json:"region"`
	// Zone is the availability zone ({region}-{letter}, cn-north-1-a). The
	// order's zoneId is authoritative: the body's zone only fills in for an
	// order that carries none, and one contradicting the order's is refused.
	// zoneId is accepted as an alias.
	Zone       string `json:"zone,omitempty"`
	ZoneID     string `json:"zoneId,omitempty"`
	SpecCode   string `json:"specCode"`
	ChargeType string `json:"chargeType"` // PREPAID | POSTPAID
	// Params are the creation parameters ({string:string}; numbers are
	// accepted), validated per product and stored as the resource's
	// attributes (attributes.go). A replay of a fulfil whose resource already
	// exists ignores them.
	Params attrValues `json:"params,omitempty"`
}

// creation is what a new resource needs, prepared before the store lock: its
// validated attributes (customer params plus the quota reservation and the
// purchased term) and the create options that make its name claims and peer
// checks atomic with the row.
type creation struct {
	attrs map[string]string
	opts  []createOption
}

func (s *lifecycleStore) handleFulfill(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req fulfillRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.OrderID == 0 || req.ProductCode == "" {
		writeErr(w, "Common.InvalidParameter", 400, "orderId and productCode are required")
		return
	}
	bodyZone := strings.TrimSpace(req.Zone)
	if alias := strings.TrimSpace(req.ZoneID); alias != "" {
		if bodyZone != "" && bodyZone != alias {
			writeErr(w, "Common.InvalidParameter", 400, "zone and zoneId disagree")
			return
		}
		bodyZone = alias
	}

	// Phase 0 (no lock): the order must exist in svc-order, belong to the
	// caller and be PAID — the only provisioning trigger (D8). What gets
	// provisioned comes from the order, never from this request: the request
	// may only repeat it. (This used to fabricate a PAID mirror for any order
	// id it had not seen, so any caller could provision for free.)
	po, err := s.orderSrc.Order(r.Context(), acct, req.OrderID)
	switch {
	case errors.Is(err, errOrderNotFound):
		writeErr(w, "Order.NotFound", 404, "订单不存在")
		return
	case err != nil:
		slog.Error("order lookup failed", "orderId", req.OrderID, "err", err)
		writeErr(w, "Order.LookupFailed", 503, "订单服务不可用,请稍后重试")
		return
	}
	// svc-order moves a fulfilled order on (COMPLETED, or REFUNDING/REFUNDED
	// after a failure). FULFILLING is a COMPLETED report that stopped half-way:
	// still paid, still provisionable.
	settled := false
	switch order.State(po.State) {
	case order.StatePaid, order.StateFulfilling:
	case order.StateCompleted, order.StateRefunding, order.StateRefunded:
		settled = true
	default:
		writeErr(w, "Order.NotPaid", 409, fmt.Sprintf("订单状态 %s,仅 PAID 可履约", po.State))
		return
	}
	if po.ProductCode != req.ProductCode {
		writeErr(w, "Order.ProductMismatch", 409, "履约产品与订单不一致")
		return
	}
	if po.SKUCode == "" {
		// Orders created before the purchased line was recorded: refuse rather
		// than provision whatever spec the request names.
		writeErr(w, "Order.LineMissing", 409, "订单缺少规格信息,无法履约")
		return
	}
	if req.SpecCode != "" && req.SpecCode != po.SKUCode {
		writeErr(w, "Order.SpecMismatch", 409, "履约规格与订单不一致")
		return
	}
	if po.RegionID != "" && req.Region != "" && req.Region != po.RegionID {
		writeErr(w, "Order.RegionMismatch", 409, "履约地域与订单不一致")
		return
	}
	if po.ZoneID != "" && bodyZone != "" && bodyZone != po.ZoneID {
		writeErr(w, "Order.ZoneMismatch", 409, "履约可用区与订单不一致")
		return
	}
	req.SpecCode = po.SKUCode
	if po.RegionID != "" {
		req.Region = po.RegionID
	}
	req.ChargeType = po.ChargeType
	if req.OrderNo == "" {
		req.OrderNo = po.OrderNo
	}
	zone := po.ZoneID
	if zone == "" {
		zone = bodyZone
	}
	if settled {
		// Already settled in svc-order: only a replay of the fulfil that
		// settled it is answered, from the ledger, re-sending the report (a
		// REFUNDING order finishes its refund this way). An order settled
		// without a resource here was never provisioned and must not be now.
		prior, err := s.ledger.ByOrder(acct, req.OrderID)
		if err != nil {
			slog.Error("resource lookup by order failed", "orderId", req.OrderID, "err", err)
			writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
			return
		}
		if prior == nil {
			writeErr(w, "Order.NotPaid", 409, fmt.Sprintf("订单状态 %s,仅 PAID 可履约", po.State))
			return
		}
		writeFulfilResult(w, prior, po.OrderID, order.State(po.State))
		s.reportFulfilment(r.Context(), acct, req.OrderID, prior)
		s.healQuota(r.Context(), prior)
		return
	}

	// Phase 0b (no lock): a resource this order has not produced yet is
	// prepared here — its parameters validated and its quota occupied — so
	// neither a 400 nor the svc-quota call ever runs under the store lock. A
	// replay of an order whose resource exists skips this: its parameters
	// were fixed when it was created.
	prior, err := s.ledger.ByOrder(acct, req.OrderID)
	if err != nil {
		slog.Error("resource lookup by order failed", "orderId", req.OrderID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	var prep *creation
	if prior == nil {
		if zone != "" && !zoneInRegion(zone, req.Region) {
			writeErr(w, "Common.InvalidParameter", 400, fmt.Sprintf("可用区 %s 不属于地域 %s", zone, req.Region))
			return
		}
		var aerr *apiError
		if prep, aerr = s.prepareCreation(r.Context(), acct, req, po); aerr != nil {
			writeAPIError(w, aerr)
			return
		}
	}

	// Phase A (under lock): transition PAID → FULFILLING on the mirror and
	// register the resource row (CREATING). The driver call happens OUTSIDE the
	// lock — a slow driver must not block every other request on the ledger.
	s.mu.Lock()

	om := order.NewMachine(time.Now)
	o, exists := s.orders[req.OrderID]
	if exists && o.AccountID != acct {
		// Mirrors are keyed by order id; svc-order already scoped the lookup to
		// the caller, so a foreign mirror here is a bug — never act on it.
		s.mu.Unlock()
		writeErr(w, "Order.NotFound", 404, "订单不存在")
		return
	}
	if !exists {
		// Mirror the verified PAID order (the real deploy consumes the
		// order.paid event from svc-order instead).
		o = &order.Order{
			OrderID: po.OrderID, OrderNo: po.OrderNo, AccountID: acct,
			Type: order.TypeNew, State: order.StatePaid,
			ProductCode: po.ProductCode, ChargeType: pricing.ChargeType(po.ChargeType),
			SKUCode: po.SKUCode, RegionID: po.RegionID, PayableAmount: po.Payable,
			CreatedAt: time.Now(), UpdatedAt: time.Now(), Version: 1,
		}
		s.orders[req.OrderID] = o
	}

	// Fulfilment idempotency, restart-safe: the LEDGER arbitrates (uk_order),
	// not a process map — a retry after a restart must still return the resource
	// the first attempt created.
	prior, err = s.ledger.ByOrder(acct, req.OrderID)
	if err == nil && prior != nil && (prior.State != resource.StateCreating ||
		(o.State != order.StatePaid && o.State != order.StateFulfilling)) {
		oid, ost := o.OrderID, o.State
		s.mu.Unlock()
		writeFulfilResult(w, prior, oid, ost)
		s.reportFulfilment(r.Context(), acct, req.OrderID, prior)
		s.healQuota(r.Context(), prior)
		return
	}

	var inst *resource.Instance
	if err == nil && prior != nil {
		// A retry of an unfinished saga (the resource is still CREATING: the
		// driver was still provisioning, or a failed provisioning could not be
		// reclaimed). Re-drive it: Apply is idempotent per resource, so the retry
		// either finds the resource up and completes the order, or fails again
		// and runs the compensation again. After a restart the mirror is fresh
		// (PAID) and takes its FULFILLING edge here.
		inst = prior
		if o.State == order.StatePaid {
			if _, err := om.Transition(o, order.StateFulfilling, o.Version); err != nil {
				s.mu.Unlock()
				slog.Error("order transition to FULFILLING failed", "orderId", req.OrderID, "err", err)
				writeErr(w, "Order.StateTransitionFailed", 409, "订单状态流转失败")
				return
			}
		}
	} else {
		if prep == nil {
			// The resource the lock-free pre-check saw is not visible here.
			// Never create one without its validated parameters and quota.
			s.mu.Unlock()
			slog.Error("resource lookup by order disagreed across one fulfil", "orderId", req.OrderID, "err", err)
			writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
			return
		}
		// 1. Order must be PAID (the only provisioning trigger, D8 invariant).
		if o.State != order.StatePaid {
			st := o.State
			s.mu.Unlock()
			writeErr(w, "Order.NotPaid", 409, fmt.Sprintf("订单状态 %s,仅 PAID 可履约", st))
			return
		}

		// 2. Transition order PAID → FULFILLING (Transition bumps Version itself).
		if _, err := om.Transition(o, order.StateFulfilling, o.Version); err != nil {
			s.mu.Unlock()
			slog.Error("order transition to FULFILLING failed", "orderId", req.OrderID, "err", err)
			writeErr(w, "Order.StateTransitionFailed", 409, "订单状态流转失败")
			return
		}

		// 3. Create resource instance (CREATING) in the ledger, with its
		// attributes, in one step. The id is derived from the order alone, so
		// every attempt at one order — a retry, a restarted process, another
		// replica — names the same resource, and the ledger's keys (resource
		// id, uk_order) let exactly one of them create it.
		ct := resource.ChargePrepaid
		if req.ChargeType == "POSTPAID" {
			ct = resource.ChargePostpaid
		}
		inst = &resource.Instance{
			ResourceID: fulfilResourceID(req.ProductCode, req.Region, acct, req.OrderID),
			AccountID:  acct, ProductCode: req.ProductCode,
			ResourceType: "instance", Region: req.Region, Zone: zone, ChargeType: ct,
			State: resource.StateCreating, SpecCode: req.SpecCode, OrderID: req.OrderID,
			CreatedAt: time.Now(), UpdatedAt: time.Now(), Version: 1,
		}
		if err := s.ledger.Create(inst, prep.opts...); err != nil {
			// Lost the create to a concurrent duplicate for the same order: serve
			// the winner's resource instead of failing the retry.
			if winner, gerr := s.ledger.ByOrder(acct, req.OrderID); gerr == nil && winner != nil {
				oid, ost := o.OrderID, o.State
				s.mu.Unlock()
				writeFulfilResult(w, winner, oid, ost)
				s.reportFulfilment(r.Context(), acct, req.OrderID, winner)
				return
			}
			// Nothing was created: drop the FULFILLING mirror, or every retry
			// would 409 on it until the process restarts. The next attempt
			// re-mirrors the order from svc-order, where it is still PAID.
			// (The lock is released before returning: this path once returned
			// while holding s.mu and wedged every later fulfil.)
			delete(s.orders, req.OrderID)
			s.mu.Unlock()
			// A parameter the ledger refused (a taken bucket name, an
			// overlapping VPC block) leaves the order PAID: the caller may
			// retry with other parameters on the same order. The quota
			// reservation stays with the order (same idempotency key) until
			// that retry or svc-quota's TTL sweep.
			var aerr *apiError
			var taken *nameTakenError
			switch {
			case errors.As(err, &aerr):
				writeAPIError(w, aerr)
			case errors.As(err, &taken):
				writeErr(w, "Resource.BucketExists", 409, fmt.Sprintf("存储桶名 %s 在地域 %s 已被占用", taken.Claim.Name, taken.Region))
			default:
				slog.Error("resource persist failed", "orderId", req.OrderID, "err", err)
				writeErr(w, "Provision.Failed", 500, "开通失败,资源台账写入失败")
			}
			return
		}
	}
	resID := inst.ResourceID
	s.mu.Unlock()

	// 4. Fan out to the ProvisionDriver (service-level singleton; MockDriver in
	// dev, rc-compute via gRPC in production). Runs outside the ledger lock.
	// A resource whose compensation is claimed (an earlier attempt — or
	// another replica — is reclaiming it, or reclaimed it and the
	// CREATE_FAILED edge did not land) is not applied again: this attempt
	// resumes that compensation (settleCreating).
	attrs, err := s.ledger.Attrs(acct, resID)
	if err != nil {
		slog.Error("resource attribute read failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	var obs provisionObservation
	if attrs[attrCompensating] == "" {
		spec := provisionSpec(inst, attrs)
		status, aerr := s.driver.Apply(spec)
		s.recordProvision(resID, acct, spec, status, aerr)
		obs = provisionObservation{observed: true, status: status, err: aerr}
	}

	// Phase B: settle the outcome through the ledger's guarded edges (the
	// same code the poller runs). Re-read the row, then its attributes: a
	// concurrent attempt at the same order — or the poller — may have
	// settled it or claimed its compensation while the driver ran.
	cur, err := s.ledger.Get(acct, resID)
	if err != nil || cur == nil {
		slog.Error("resource re-read failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	if cur.State == resource.StateCreating {
		if attrs, err = s.ledger.Attrs(acct, resID); err != nil {
			slog.Error("resource attribute read failed", "resourceId", resID, "err", err)
			writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
			return
		}
	}
	res := settleResult{kind: settledElsewhere, inst: cur}
	if cur.State == resource.StateCreating {
		res = s.settleCreating(r.Context(), cur, attrs, obs, "", prepaidTerm{Duration: po.Duration, Unit: po.DurationUnit})
	}

	// 5./6. Answer from the outcome; the order mirror follows it, and a
	// settled resource is reported to svc-order (the report is an HTTP call:
	// it runs without the store lock).
	switch res.kind {
	case settledElsewhere:
		writeFulfilResult(w, res.inst, req.OrderID, s.mirrorState(req.OrderID))
		s.reportFulfilment(r.Context(), acct, req.OrderID, res.inst)
	case settleInProgress:
		// Accepted, not finished (the VM is still booting): the resource stays
		// CREATING and the order FULFILLING; a retry of this call or the
		// provisioning poller settles it. Treating "not Ready yet" as a
		// failure refunded every order slower than one request.
		writeFulfilResult(w, res.inst, req.OrderID, s.mirrorState(req.OrderID))
	case settleRunningFailed:
		// The row is still CREATING: drop the mirror like the create failure
		// above, so a retry re-drives the saga instead of 409-ing on a stale
		// FULFILLING mirror.
		s.dropMirror(req.OrderID)
		slog.Error("resource transition to RUNNING failed", "resourceId", resID, "err", res.err)
		writeErr(w, "Resource.StateTransitionFailed", 409, "资源状态流转失败")
	case settleReclaimPending:
		// Not (yet) rolled back: the mirror stays FULFILLING (REFUNDING would
		// announce a refund nobody reported) and the resource CREATING; a
		// retry or the poller finishes the compensation.
		slog.Error("provision failed and the compensation could not finish", "orderId", req.OrderID,
			"resourceId", resID, "err", res.err)
		writeErr(w, "Provision.Failed", 500, "开通失败,资源回收中,请稍后重试")
	case settleCompensated:
		if _, err := s.settleMirror(req.OrderID, order.StateRefunding); err != nil {
			slog.Error("order transition to REFUNDING failed", "orderId", req.OrderID, "err", err)
		}
		s.reportFulfilment(r.Context(), acct, req.OrderID, res.inst) // FAILED: svc-order refunds
		slog.Error("provision failed, compensation done", "orderId", req.OrderID, "resourceId", resID, "err", res.err)
		writeErr(w, "Provision.Failed", 500, "开通失败,已进入退款")
	case settleDelivered:
		// The resource is delivered: svc-order completes the order whatever
		// the mirror does.
		s.reportFulfilment(r.Context(), acct, req.OrderID, res.inst)
		ost, err := s.settleMirror(req.OrderID, order.StateCompleted)
		if err != nil {
			slog.Error("order transition to COMPLETED failed", "orderId", req.OrderID, "err", err)
			writeErr(w, "Order.StateTransitionFailed", 409, "订单状态流转失败")
			return
		}
		writeFulfilResult(w, res.inst, req.OrderID, ost)
	}
}

// prepareCreation validates a new resource's parameters and occupies its quota
// (O1, O6). Quota is occupied after validation — a bad parameter costs
// nothing — and before provisioning; the order id is the idempotency key, so
// every attempt at this order holds the same reservation. An exceeded quota
// fails the order: svc-order is told FAILED (it refunds) and the caller gets
// 409 Quota.Exceeded. svc-quota being unreachable fails closed (503): nothing
// is provisioned past an unchecked quota, and the order stays PAID for a
// retry.
func (s *lifecycleStore) prepareCreation(ctx context.Context, acct int64, req fulfillRequest, po paidOrder) (*creation, *apiError) {
	attrs, aerr := validateCreateParams(req.ProductCode, req.Params)
	if aerr != nil {
		return nil, aerr
	}
	c := &creation{attrs: attrs}
	switch req.ProductCode {
	case "euoss":
		c.opts = append(c.opts, withNameClaim(nameClaim{Namespace: claimBucket, Name: attrs[attrBucket]}))
	case "euvpc":
		c.opts = append(c.opts, withPeerCheck(vpcOverlapCheck(attrs[attrCIDRBlock])))
	}
	for k, v := range (prepaidTerm{Duration: po.Duration, Unit: po.DurationUnit}).attrs() {
		attrs[k] = v
	}
	if s.quota != nil {
		probe := &resource.Instance{ProductCode: req.ProductCode, ResourceType: "instance", Region: req.Region}
		resv, err := s.occupyActive(ctx, acct, quotaOccupyFor(probe, req.OrderID, ""))
		switch {
		case errors.Is(err, errQuotaExceeded):
			reason := "quota exceeded: " + err.Error()
			if rerr := s.orderSink.ReportFulfilment(context.WithoutCancel(ctx), acct, req.OrderID,
				fulfilmentReport{Result: fulfilmentFailed, Reason: reason}); rerr != nil {
				slog.Error("fulfilment report to svc-order failed; a fulfil retry re-sends it",
					"orderId", req.OrderID, "result", fulfilmentFailed, "err", rerr)
			}
			return nil, &apiError{Code: "Quota.Exceeded", Status: http.StatusConflict, Msg: "配额不足,订单将退款: " + err.Error()}
		case errors.Is(err, errQuotaUnmodelled):
			// svc-quota models no quota for this product: nothing to hold.
		case err != nil:
			slog.Error("quota occupy failed", "orderId", req.OrderID, "err", err)
			return nil, &apiError{Code: "Quota.Unavailable", Status: http.StatusServiceUnavailable, Msg: "配额服务不可用,请稍后重试"}
		default:
			attrs[attrQuotaReservation] = resv
			attrs[attrQuotaState] = quotaOccupied
		}
	}
	c.opts = append(c.opts, withAttrs(attrs))
	return c, nil
}

// zoneInRegion reports whether zone is an AZ of region: {region}-{suffix},
// the suffix lowercase letters/digits (identifier.AZName), within the
// column's 32 characters.
func zoneInRegion(zone, region string) bool {
	suffix, ok := strings.CutPrefix(zone, region+"-")
	if !ok || suffix == "" || len(zone) > 32 {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		if c := suffix[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// reportFulfilment tells svc-order how inst's order settled: COMPLETED once the
// resource was delivered, FAILED once it was rolled back (CREATE_FAILED). A
// resource still CREATING has not settled and reports nothing. A delivery
// failure is logged, never surfaced: the fulfil response stands, svc-order
// applies reports idempotently, and the next fulfil replay re-sends it.
func (s *lifecycleStore) reportFulfilment(ctx context.Context, acct, orderID int64, inst *resource.Instance) {
	rep, ok := fulfilmentOutcome(inst)
	if !ok {
		return
	}
	// A caller that hung up must not cancel the report of what already happened.
	if err := s.orderSink.ReportFulfilment(context.WithoutCancel(ctx), acct, orderID, rep); err != nil {
		slog.Error("fulfilment report to svc-order failed; a fulfil retry re-sends it",
			"orderId", orderID, "resourceId", inst.ResourceID, "result", rep.Result, "err", err)
	}
}

// fulfilmentOutcome maps a resource row onto the report its order is owed.
func fulfilmentOutcome(inst *resource.Instance) (fulfilmentReport, bool) {
	switch inst.State {
	case resource.StateCreating:
		return fulfilmentReport{}, false
	case resource.StateCreateFailed:
		return fulfilmentReport{Result: fulfilmentFailed, Reason: "provisioning failed; resource " + inst.ResourceID + " was rolled back"}, true
	default:
		// RUNNING, or any later lifecycle state: the resource was delivered.
		return fulfilmentReport{Result: fulfilmentCompleted, ResourceID: inst.ResourceID}, true
	}
}

// provisionInProgress reports a dispatch the backend accepted but has not
// finished — not a failure.
func provisionInProgress(st provision.Status) bool {
	return st.Phase == provision.PhasePending || st.Phase == provision.PhaseProvisioning
}

// writeFulfilResult answers a fulfil call from the resource row: 200 for a
// settled resource, 202 while it is still CREATING (accepted, provisioning),
// and 409 for a CREATE_FAILED one — a replayed fulfil of a failed, refunded
// provisioning must not read as "instance created" to the console.
func writeFulfilResult(w http.ResponseWriter, inst *resource.Instance, orderID int64, orderState order.State) {
	if inst.State == resource.StateCreateFailed {
		writeErr(w, "Provision.Failed", 409, "开通失败,已进入退款")
		return
	}
	httpStatus := http.StatusOK
	if inst.State == resource.StateCreating {
		httpStatus = http.StatusAccepted
	}
	writeJSONStatus(w, httpStatus, "OK", map[string]any{
		"resourceId": inst.ResourceID, "state": string(inst.State),
		"orderId": orderID, "orderState": string(orderState),
		"billingStart": formatTime(inst.BillingStart), "expiredAt": formatTime(inst.ExpiredAt),
	})
}

// fulfilResourceID derives a resource id from the order:
// {productCode}-{regionId}-{shard factor}-{order id, ≥8 hex digits} — the
// pkg-go/identifier grammar for order ids below 2^32, and unique beyond. It is
// a pure function of the order, so a retry, a restarted process or a second
// replica all name the same resource and the ledger arbitrates a concurrent
// fulfil (the old per-process counter let two replicas mint two resources).
func fulfilResourceID(productCode, region string, accountID, orderID int64) string {
	return fmt.Sprintf("%s-%s-%s-%08x", productCode, region,
		identifier.ShardFactorFor(accountID, 8, 16), orderID)
}

// prepaidExpiry is start plus the purchased term. It is zero when the order
// carries no usable term (older svc-order builds, USAGE units, nonsense
// values): no expiry is better than an invented one.
func prepaidExpiry(start time.Time, duration int64, unit pricing.DurationUnit) time.Time {
	const maxMonths = 12 * 100
	if duration <= 0 {
		return time.Time{}
	}
	clock := func(per time.Duration) time.Time {
		if duration > int64(100*365*24*time.Hour/per) {
			return time.Time{}
		}
		return start.Add(time.Duration(duration) * per)
	}
	switch unit {
	case pricing.DurationYear:
		if duration > maxMonths/12 {
			return time.Time{}
		}
		return addMonthsClamped(start, int(duration)*12)
	case pricing.DurationMonth:
		if duration > maxMonths {
			return time.Time{}
		}
		return addMonthsClamped(start, int(duration))
	case pricing.DurationHour:
		return clock(time.Hour)
	case pricing.DurationMinute:
		return clock(time.Minute)
	case pricing.DurationSecond:
		return clock(time.Second)
	}
	return time.Time{}
}

// addMonthsClamped adds calendar months, clamping the day to the target
// month's last day: a term bought on Jan 31 ends on Feb 28/29, not on the
// Mar 3 that time.AddDate's normalisation would produce.
func addMonthsClamped(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, 0, 0, 0, 0, t.Location())
	if last := first.AddDate(0, 1, -1).Day(); d > last {
		d = last
	}
	return time.Date(first.Year(), first.Month(), d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// recordProvision writes the dispatch record for the retry scanner. A failure
// here is logged, not fatal: the dispatch already happened, and losing the audit
// row must not fail a request whose resource came up.
func (s *lifecycleStore) recordProvision(resID string, acct int64, spec provision.Spec, status provision.Status, err error) {
	recStatus, lastError := provisionRecordStatus(status, err)
	params, merr := json.Marshal(spec)
	if merr != nil {
		params = []byte("{}")
	}
	if rerr := s.ledger.RecordProvision(provisionRecord{
		ResourceID: resID, AccountID: acct, OpType: "CREATE",
		IdempotKey: spec.IdempotencyKey, Params: string(params),
		Status: recStatus, LastError: lastError,
	}); rerr != nil {
		slog.Error("provision task record failed", "resourceId", resID, "err", rerr)
	}
}

// provisionRecordStatus maps a dispatch outcome onto provision_task: DONE when
// Ready, DISPATCHED while the backend is still provisioning, FAILED otherwise.
// last_error is empty unless something failed (fmt "%v" of a nil error used to
// store the literal "<nil>").
func provisionRecordStatus(status provision.Status, err error) (string, string) {
	switch {
	case err != nil:
		return "FAILED", err.Error()
	case status.Ready():
		return "DONE", ""
	case provisionInProgress(status):
		return "DISPATCHED", ""
	default:
		return "FAILED", fmt.Sprintf("driver reported phase %q", status.Phase)
	}
}

// handleListResources lists the account's resources. The optional ?product=
// query narrows the list to one product code (case-insensitive, as the console
// and catalogue spell codes in lower case but a hand-typed query may not); an
// absent or blank value lists every product. The filter runs after the
// account-scoped ledger read, so it can only ever narrow the caller's own rows
// and both ledger backends behave identically.
func (s *lifecycleStore) handleListResources(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	list, err := s.ledger.List(acct)
	if err != nil {
		slog.Error("resource list failed", "account", acct, "err", err)
		writeErr(w, "Resource.ListFailed", 500, "资源列表读取失败")
		return
	}
	if product := strings.TrimSpace(r.URL.Query().Get("product")); product != "" {
		kept := list[:0]
		for _, inst := range list {
			if strings.EqualFold(inst.ProductCode, product) {
				kept = append(kept, inst)
			}
		}
		list = kept
	}
	attrs, err := s.ledger.AccountAttrs(acct)
	if err != nil {
		slog.Error("resource attribute list failed", "account", acct, "err", err)
		writeErr(w, "Resource.ListFailed", 500, "资源列表读取失败")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, inst := range list {
		out = append(out, resourceView(inst, attrs[inst.ResourceID]))
	}
	writeJSON(w, "OK", out)
}

func (s *lifecycleStore) handleResourceDetail(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	s.writeResourceDetail(w, acct, r.PathValue("id"))
}

// writeResourceDetail answers with the resource's detail record (also the
// answer of the attributes PATCH).
func (s *lifecycleStore) writeResourceDetail(w http.ResponseWriter, acct int64, resID string) {
	inst, err := s.ledger.Get(acct, resID)
	if err != nil {
		slog.Error("resource lookup failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	if inst == nil {
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return
	}
	attrs, err := s.ledger.Attrs(acct, resID)
	if err != nil {
		slog.Error("resource attribute read failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	writeJSON(w, "OK", resourceView(inst, attrs))
}

// handleRelease releases a resource: RELEASING (journalled), the driver's
// idempotent Delete, RELEASED. The state machine decides where release may
// start: RUNNING, STOPPED, UPGRADING, LOCKED (arrears), EXPIRED (lapsed
// subscription), PREEMPTING and CREATE_FAILED — the last a rolled-back
// creation, whose Delete finds nothing left and succeeds. CREATING is refused
// (409): its saga owns it until it settles. RELEASED frees the resource's
// unique names (a bucket) in the same transaction.
//
// Quota: between the reclaim and RELEASED, the resource's used capacity goes
// back to the account (freeQuota, svc-quota /internal/quota/free, keyed by
// the resource id). A failed free fails the release, which stays RELEASING —
// not billable — until a retry completes it.
func (s *lifecycleStore) handleRelease(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	// Phase A: legal transition into RELEASING, persisted with its journal row.
	resID := r.PathValue("id")
	inst, err := s.ledger.Get(acct, resID)
	if err != nil {
		slog.Error("resource lookup failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	if inst == nil {
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return
	}
	// An instance already RELEASING is the retry of a release whose driver
	// Delete failed. RELEASING → RELEASING is not an edge, so re-transitioning
	// would refuse every retry and strand the resource there; go straight to
	// the (idempotent) Delete instead.
	if inst.State != resource.StateReleasing {
		if err := s.ledger.Transition(inst, resource.StateReleasing, "user release"); err != nil {
			slog.Error("resource transition to RELEASING failed", "resourceId", resID, "err", err)
			writeErr(w, "Resource.StateTransitionFailed", 409, "资源状态流转失败")
			return
		}
	}

	// Reclaim via the driver (Delete is idempotent).
	if err := s.driver.Delete(resID); err != nil {
		slog.Error("driver delete failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.ReleaseFailed", 500, "资源回收失败,请重试")
		return
	}
	// Give the used quota back (keyed by the resource: a retry frees once).
	if err := s.freeQuota(r.Context(), inst); err != nil {
		slog.Error("quota free failed; the release stays RELEASING until a retry frees it", "resourceId", resID, "err", err)
		writeErr(w, "Resource.ReleaseFailed", 500, "配额归还失败,请重试")
		return
	}

	// Phase B: legal transition RELEASING → RELEASED.
	if err := s.ledger.Transition(inst, resource.StateReleased, "released"); err != nil {
		slog.Error("resource transition to RELEASED failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.StateTransitionFailed", 409, "资源状态流转失败")
		return
	}
	writeJSON(w, "OK", map[string]any{"resourceId": resID, "state": string(inst.State)})
}

// actionRequest is the body of POST /api/v1/orchestrator/resources/{id}/actions.
type actionRequest struct {
	Action   string `json:"action"`             // stop | start | restart | upgrade
	SpecCode string `json:"specCode,omitempty"` // upgrade only: target SKU
}

// handleResourceAction applies a user-initiated lifecycle action through the
// SAME state machine the saga uses (pkg-go/resource transition table) — there
// is no second, looser path for console buttons. stop/start/restart map onto
// the RUNNING↔STOPPED edges; upgrade rides the UPGRADING intermediate state
// (RUNNING → UPGRADING → RUNNING) and rewrites SpecCode. The MockDriver is
// fire-and-forget for power actions in phase-2 (the ledger is the authority);
// production fans out to rc-compute the same way Apply/Delete do.
func (s *lifecycleStore) handleResourceAction(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	var req actionRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}

	resID := r.PathValue("id")
	inst, err := s.ledger.Get(acct, resID)
	if err != nil {
		slog.Error("resource lookup failed", "resourceId", resID, "err", err)
		writeErr(w, "Resource.LookupFailed", 500, "资源查询失败")
		return
	}
	if inst == nil {
		writeErr(w, "Resource.NotFound", 404, "资源不存在")
		return
	}

	transition := func(to resource.State, reason string) bool {
		if err := s.ledger.Transition(inst, to, reason); err != nil {
			slog.Error("resource action transition failed", "resourceId", resID, "to", to, "err", err)
			writeErr(w, "Resource.StateTransitionFailed", 409,
				fmt.Sprintf("资源状态 %s 不允许 %s", inst.State, req.Action))
			return false
		}
		return true
	}

	// refuse answers an action the resource's current state does not allow.
	refuse := func() {
		writeErr(w, "Resource.StateTransitionFailed", 409,
			fmt.Sprintf("资源状态 %s 不允许 %s", inst.State, req.Action))
	}

	switch req.Action {
	case "stop":
		if !transition(resource.StateStopped, "user stop") {
			return
		}
	case "start":
		// start is the STOPPED → RUNNING edge only. The table also leads to
		// RUNNING from CREATING, UPGRADING, LOCKED and EXPIRED, but those edges
		// belong to provisioning, upgrade completion, recharge and renewal: a
		// console button taking them would skip provisioning or payment.
		if inst.State != resource.StateStopped {
			refuse()
			return
		}
		if !transition(resource.StateRunning, "user start") {
			return
		}
	case "restart":
		// Restart power-cycles a running instance; from any other state the
		// stop edge would be refused anyway, or (STOPPED) restart would be a
		// start in disguise.
		if inst.State != resource.StateRunning {
			refuse()
			return
		}
		// Two legal edges in sequence; the state machine guards each.
		if !transition(resource.StateStopped, "user restart: stop") || !transition(resource.StateRunning, "user restart: start") {
			return
		}
	case "upgrade":
		if req.SpecCode == "" {
			writeErr(w, "Common.InvalidParameter", 400, "upgrade requires specCode")
			return
		}
		// 包年包月: the term was paid for at the old spec. Changing spec here
		// would hand out the bigger spec for free; it needs an UPGRADE order
		// that collects the price difference, which is not wired yet.
		if inst.ChargeType == resource.ChargePrepaid {
			writeErr(w, "Resource.UpgradeRequiresOrder", 409, "包年包月实例变更规格需通过变配订单支付差价,暂不支持直接变更")
			return
		}
		// 按量: the new spec is metered from now on, but it must be a spec of
		// the same product and of the instance's charge type — a .prepaid SKU
		// on a postpaid instance would be metered at a price that was never
		// meant to be metered.
		if !strings.HasPrefix(req.SpecCode, inst.ProductCode+".") {
			writeErr(w, "Common.InvalidParameter", 400, "specCode does not belong to this product")
			return
		}
		if ct := skuChargeType(req.SpecCode); ct != "" && ct != inst.ChargeType {
			writeErr(w, "Common.InvalidParameter", 400, "specCode charge type does not match the instance")
			return
		}
		if !transition(resource.StateUpgrading, "user upgrade") {
			return
		}
		// Stage the new spec BEFORE the edge that persists it: the ledger
		// writes the instance it is handed, so assigning after the last
		// transition changed the response but never resource_db.
		inst.SpecCode = req.SpecCode
		if !transition(resource.StateRunning, "upgrade applied") {
			return
		}
	default:
		writeErr(w, "Common.InvalidParameter", 400, "action must be stop/start/restart/upgrade")
		return
	}

	writeJSON(w, "OK", map[string]any{
		"resourceId": resID, "state": string(inst.State), "specCode": inst.SpecCode,
	})
}

// --- helpers ---

func instanceToMap(i *resource.Instance) map[string]any {
	return map[string]any{
		"resourceId": i.ResourceID, "productCode": i.ProductCode, "region": i.Region,
		"zone": i.Zone, "chargeType": string(i.ChargeType), "state": string(i.State), "specCode": i.SpecCode,
		"billingStart": formatTime(i.BillingStart), "expiredAt": formatTime(i.ExpiredAt),
		"createdAt": formatTime(i.CreatedAt), "version": i.Version,
	}
}

// resourceView is the detail/list record: the instance plus its customer
// attributes (always an object, never the system keys).
func resourceView(i *resource.Instance, attrs map[string]string) map[string]any {
	m := instanceToMap(i)
	m["attributes"] = publicAttrs(attrs)
	return m
}

// formatTime renders a ledger timestamp as RFC3339, and an unset one (a
// resource that never reached RUNNING, a postpaid resource's expiry) as "" —
// never as "0001-01-01T00:00:00Z".
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// skuChargeType is the charge type a SKU code declares by the catalogue's
// naming convention ({product}.{spec}.prepaid|postpaid, svc-catalog seeds);
// "" when the code does not say.
func skuChargeType(sku string) resource.ChargeType {
	switch {
	case strings.HasSuffix(sku, ".prepaid"):
		return resource.ChargePrepaid
	case strings.HasSuffix(sku, ".postpaid"):
		return resource.ChargePostpaid
	}
	return ""
}

func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	// TRUST NOTE: X-Euler-Account-Id is injected by the API gateway after
	// authentication; this service relies on network isolation (and optionally
	// internalTokenMiddleware) rather than re-authenticating.
	raw := r.Header.Get(accountIDHeader)
	if raw == "" {
		writeErr(w, "Common.MissingAccountId", 403, "X-Euler-Account-Id header is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed X-Euler-Account-Id")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, code string, data any) {
	writeJSONStatus(w, http.StatusOK, code, data)
}

func writeJSONStatus(w http.ResponseWriter, status int, code string, data any) {
	rid := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Data": data})
}

func writeErr(w http.ResponseWriter, code string, status int, msg string) {
	rid := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Message": msg})
}

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := httpmw.SanitizeRequestID(r.Header.Get("X-Euler-TraceId"))
		if id == "" {
			id = fmt.Sprintf("orch-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Euler-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

// internalTokenMiddleware optionally enforces an internal shared secret: when
// the EULER_INTERNAL_TOKEN env var is set, every request must carry a matching
// X-Euler-Internal-Token header (defense-in-depth for the gateway-injected
// X-Euler-Account-Id trust). Unset (dev default) = no check.
func internalTokenMiddleware(h http.Handler) http.Handler {
	return httpmw.InternalToken(h)
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
	addr := flag.String("http", ":9203", "HTTP listen address")
	flag.Parse()
	// Production refuses to start without the internal shared secret: the
	// X-Euler-Account-Id this service trusts must provably come from the gateway.
	runtimeRequireSecrets()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Quota (O6): production must enforce it; elsewhere an unset URL is
	// allowed and said once, here.
	if strings.TrimSpace(os.Getenv(quotaURLEnv)) == "" {
		if runmode.Production() {
			slog.Error("refusing to start without quota enforcement", "env", runmode.Env(), "missing", quotaURLEnv)
			os.Exit(1)
		}
		slog.Warn("quota not enforced: "+quotaURLEnv+" is unset (allowed outside production only)", "env", runmode.Env())
	}
	pollEvery, err := durationEnv(pollIntervalEnv, defaultPollInterval)
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	creatingTimeout, err := durationEnv(creatingTimeoutEnv, defaultCreatingTimeout)
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}

	ledger, err := newLedger(context.Background())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	// Log where the ledger lives: in-memory resources vanish on restart, and a
	// customer's running instance vanishing is not a recoverable incident.
	slog.Info("svc-orchestrator ledger ready", "persistent", persistentLedger(ledger))
	store := newStoreWith(ledger)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/orchestrator/fulfill", store.handleFulfill)
	mux.HandleFunc("GET /api/v1/orchestrator/resources", store.handleListResources)
	mux.HandleFunc("GET /api/v1/orchestrator/resources/{id}", store.handleResourceDetail)
	mux.HandleFunc("PATCH /api/v1/orchestrator/resources/{id}/attributes", store.handlePatchAttributes)
	mux.HandleFunc("POST /api/v1/orchestrator/resources/{id}/release", store.handleRelease)
	mux.HandleFunc("POST /api/v1/orchestrator/resources/{id}/actions", store.handleResourceAction)
	mux.HandleFunc("POST /api/v1/orchestrator/resources/{id}/backups", store.handleCreateBackup)
	mux.HandleFunc("GET /api/v1/orchestrator/resources/{id}/backups", store.handleListBackups)
	mux.HandleFunc("DELETE /api/v1/orchestrator/resources/{id}/backups/{backupId}", store.handleDeleteBackup)

	// The provisioning poller runs for the life of the process; shutdown stops
	// it before draining HTTP (no new settles start while requests finish).
	pollCtx, stopPoller := context.WithCancel(context.Background())
	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		newProvisionPoller(store, pollEvery, creatingTimeout).run(pollCtx)
	}()
	slog.Info("provisioning poller started", "interval", pollEvery.String(), "creatingTimeout", creatingTimeout.String())

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(internalTokenMiddleware(mux))), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("svc-orchestrator listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutdown signal received, draining")
	stopPoller()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
	select {
	case <-pollerDone:
	case <-ctx.Done():
		slog.Error("provisioning poller did not stop before the shutdown deadline")
	}
}

// runtimeRequireSecrets enforces the production secret set (pkg-go/runmode).
func runtimeRequireSecrets() {
	runmode.MustRequireSecrets(httpmw.InternalTokenEnv)
}
