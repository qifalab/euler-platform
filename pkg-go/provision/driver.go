// Package provision defines the data-plane fulfilment abstraction used by the
// rc-* resource controllers (06-kubernetes-productization.md §4, §6.2).
//
// # The four iron rules (06§4.1)
//
//  1. The CR is the only fact interface. The control plane never SSHes into a
//     host or reaches into a product's internals — if a fact is not in the CR,
//     the control plane does not know it.
//  2. Status must be machine-readable: a structured phase plus conditions,
//     never a log line to be parsed. Parsing logs to decide whether a customer
//     is being billed is how billing silently breaks on a log-format change.
//  3. Operators must be idempotent and re-entrant. Reconcile runs repeatedly
//     with the same input by design.
//  4. No metering, no launch. The metering point is a first-class field in
//     status.usage, not an afterthought (架构原则 6).
//
// # Driver abstraction
//
// 06§6.2 puts a ProvisionDriver between the controller and the backend, with
// k8s, vm and mock implementations. The point is that switching a product from
// containers to VMs is a catalogue configuration change (driver: vm), not a
// rewrite of the control plane. The VM driver is stubbed in phase 1 — the
// interface is frozen now precisely so that phase 2 does not have to renegotiate
// it (架构原则 12 模型先行).
//
// # CR phase is observation, platform state is authority
//
// Adjudication S21: svc-orchestrator owns the lifecycle. A controller reporting
// Ready is evidence that the platform's state machine evaluates; it is not a
// state transition in itself. Two systems both claiming authority is how a
// resource ends up billed as running while its CR says it was deleted an hour
// ago.
package provision

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DriverType selects the fulfilment backend for a product.
type DriverType string

const (
	// DriverK8s creates Kubernetes custom resources. Phase-1 default.
	DriverK8s DriverType = "k8s"
	// DriverVM targets a virtualization platform. Interface frozen,
	// implementation deferred to phase 2 (06§6.2, decision R-03 KubeVirt).
	DriverVM DriverType = "vm"
	// DriverMock is for integration and chaos testing.
	DriverMock DriverType = "mock"
)

// Phase is the CR status.phase (06§4.2). It is an OBSERVATION of the backend,
// which svc-orchestrator maps onto the authoritative platform state.
type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseProvisioning Phase = "Provisioning"
	PhaseReady        Phase = "Ready"
	PhaseModifying    Phase = "Modifying"
	PhaseSuspended    Phase = "Suspended"
	PhaseFailed       Phase = "Failed"
	PhaseDeleting     Phase = "Deleting"
)

// Finalizer is set on every product CR so the controller can reclaim backing
// resources and push a final metering reading before the object disappears
// (06§4.2). "CR truly gone" is the release-complete signal — without the
// finalizer, deletion races the last usage report and the customer is either
// over- or under-billed for their final minutes.
const Finalizer = "products.cloud.platform/cleanup"

// ReclaimNoticeWindow is the minimum advance notice before a preemptible
// (spot) resource may be reclaimed (09-roadmap §4.2: "提前5分钟通知"). The
// reclaim path blocks until this window has elapsed since the notice was
// delivered — the platform cannot reclaim what it has not warned about.
const ReclaimNoticeWindow = 5 * time.Minute

// Mandatory CR labels (06§4.2). All four are required on every product CR:
// without them a resource cannot be attributed to a tenant, which breaks
// isolation, billing and audit simultaneously.
const (
	LabelTenant   = "cloud.platform/tenant"
	LabelProject  = "cloud.platform/project"
	LabelProduct  = "cloud.platform/product"
	LabelInstance = "cloud.platform/instance"
)

// Op is a fulfilment operation.
type Op string

const (
	OpCreate  Op = "CREATE"
	OpModify  Op = "MODIFY"
	OpSuspend Op = "SUSPEND" // arrears freeze: stop serving, NEVER delete data
	OpResume  Op = "RESUME"
	OpDelete  Op = "DELETE"
)

// Spec is the declarative desired state dispatched to a controller.
//
// Declarative rather than imperative (00§2.2.3): a command-style RPC loses its
// effect when a node restarts mid-flight, whereas a desired state is
// re-convergeable — the controller simply compares and closes the gap, however
// many times it is asked.
type Spec struct {
	ResourceID   string
	AccountID    int64
	ProjectID    int64
	ProductCode  string
	ResourceType string
	Region       string
	Zone         string
	// RegionScope is the catalogue's placement scope for the product:
	// "REGIONAL" (spread across AZs) or "ZONAL" (pinned to one AZ). A ZONAL
	// spec MUST carry a Zone — a VM with no AZ of residence cannot be placed.
	// Empty is tolerated for legacy callers and skips the check.
	RegionScope string
	Edition     string // registered enum value only
	Params      map[string]string
	// Suspend freezes service while retaining data. This is the arrears lock
	// (01§5.4 D8): a suspended resource must come back intact when the
	// customer pays.
	Suspend bool
	// IdempotencyKey is usually the order id. The controller dedupes on it so
	// a redelivered dispatch does not create a second resource.
	IdempotencyKey string
}

// Labels returns the four mandatory CR labels for this spec.
func (s Spec) Labels() map[string]string {
	return map[string]string{
		LabelTenant:   fmt.Sprintf("%d", s.AccountID),
		LabelProject:  fmt.Sprintf("%d", s.ProjectID),
		LabelProduct:  s.ProductCode,
		LabelInstance: s.ResourceID,
	}
}

// Validate checks the spec before dispatch. Rejecting here keeps malformed
// resources out of the cluster entirely.
func (s Spec) Validate() error {
	if s.ResourceID == "" {
		return errors.New("provision: ResourceID required")
	}
	if s.AccountID == 0 {
		// A resource with no tenant cannot be isolated, billed or audited.
		return errors.New("provision: AccountID required (tenant attribution)")
	}
	if s.ProductCode == "" {
		return errors.New("provision: ProductCode required")
	}
	if s.Region == "" {
		return errors.New("provision: Region required")
	}
	if s.RegionScope == "ZONAL" && s.Zone == "" {
		// A ZONAL resource is pinned to one AZ at create time; dispatching it
		// without a zone defers the placement decision to the backend, which
		// has no business making it.
		return errors.New("provision: Zone required for ZONAL products")
	}
	if s.IdempotencyKey == "" {
		return errors.New("provision: IdempotencyKey required")
	}
	return nil
}

// Condition is a structured status condition (06§4.2). Machine-readable by
// construction: no consumer should ever parse a log to learn this.
type Condition struct {
	Type               string
	Status             string // "True" | "False" | "Unknown"
	Reason             string
	Message            string
	LastTransitionTime time.Time
}

// UsagePoint is a metering reading carried on the CR status. Metering is a
// first-class status field, not a side channel — 06§4.1 rule 4: 不可计量的产品
// 不允许上架.
type UsagePoint struct {
	MeteringItem  string
	Quantity      string // decimal string; never float
	WindowStart   time.Time
	WindowSeconds int
}

// Status is the observed state reported by a controller.
type Status struct {
	ResourceID         string
	Phase              Phase
	ObservedGeneration int64
	Conditions         []Condition
	Endpoints          []string
	// Usage carries the metering readings for this reconcile round.
	Usage []UsagePoint
	// Message is human-facing detail; consumers must key on Phase and
	// Conditions, never on this string.
	Message   string
	UpdatedAt time.Time
}

// Ready reports whether the resource is serving.
func (s Status) Ready() bool { return s.Phase == PhaseReady }

// ConditionByType returns a condition, if present.
func (s Status) ConditionByType(t string) (Condition, bool) {
	for _, c := range s.Conditions {
		if c.Type == t {
			return c, true
		}
	}
	return Condition{}, false
}

// Errors.
var (
	ErrNotFound           = errors.New("provision: resource not found")
	ErrDriverNotReady     = errors.New("provision: driver not implemented")
	ErrInvalidSpec        = errors.New("provision: invalid spec")
	ErrUnknownDriver      = errors.New("provision: unknown driver type")
	ErrNotPreemptible     = errors.New("provision: resource is not preemptible")
	ErrNoticeNotDelivered = errors.New("provision: reclaim notice not yet delivered")
)

// Driver is the fulfilment backend abstraction (06§6.2).
//
// Every method must be idempotent: the control plane retries, polls, and
// redelivers, and a driver that cannot tolerate being asked twice will corrupt
// state under exactly the conditions it is most likely to be asked twice.
type Driver interface {
	// Type identifies the backend.
	Type() DriverType
	// Apply creates or updates a resource to match the spec. Applying an
	// unchanged spec is a no-op, not an error.
	Apply(spec Spec) (Status, error)
	// Delete reclaims the resource. Deleting an already-deleted resource
	// SUCCEEDS — this is what makes saga compensation safe to retry.
	Delete(resourceID string) error
	// Query returns observed status, the polling fallback for lost callbacks.
	Query(resourceID string) (Status, error)
	// CollectUsage returns metering readings since the last collection.
	CollectUsage(resourceID string) ([]UsagePoint, error)
}

// Preemptor is an optional Driver capability for backends that support the
// 抢占式 (spot) reclaim path (09-roadmap §4.2, M-4.2). Not every product is
// preemptible — only spot instances — so Preempt is a separate interface
// rather than a Driver method, and callers type-assert.
//
// The reclaim is a two-step handshake, not a single call:
//  1. NotifyReclaim delivers the 5-minute warning. The driver records the
//     notice as delivered; the resource is NOT yet reclaimed.
//  2. Reclaim performs the actual reclaim, but only after the notice window
//     has elapsed. Reclaiming before the window is an error — the platform
//     may not reclaim what it has not warned about.
//
// Both steps are idempotent: a redelivered notice is a no-op, and a repeated
// reclaim against an already-reclaimed resource succeeds. This mirrors the
// Delete contract so saga compensation stays safe.
type Preemptor interface {
	// NotifyReclaim delivers the 5-minute warning to the resource owner.
	// Returns the instant the notice is considered delivered; a replay returns
	// the original instant (idempotent).
	NotifyReclaim(resourceID string, deliveredAt time.Time) (time.Time, error)
	// Reclaim reclaims the resource after the notice window has elapsed.
	// Returns ErrNoticeNotDelivered if no notice was sent, or if the window
	// has not yet elapsed.
	Reclaim(resourceID string, at time.Time) error
	// ReclaimableAt returns the earliest instant the resource may be reclaimed
	// (notice-delivered + 5m), or (_, false) if no notice has been delivered.
	ReclaimableAt(resourceID string) (time.Time, bool)
}

// PhaseMapping translates a CR phase to the platform resource state
// (06§4.3). The platform state is authoritative; this mapping is how evidence
// becomes a proposed transition, which svc-orchestrator then validates against
// its own state machine.
var PhaseMapping = map[Phase][]string{
	PhasePending:      {"INIT", "CREATING"},
	PhaseProvisioning: {"CREATING", "UPGRADING"},
	PhaseReady:        {"RUNNING"},
	PhaseModifying:    {"UPGRADING"},
	PhaseSuspended:    {"LOCKED", "EXPIRED"},
	PhaseFailed:       {"CREATE_FAILED"},
	PhaseDeleting:     {"RELEASING"},
}

// ProposedState returns the platform state a phase suggests, given the current
// platform state.
//
// Where a phase maps to several states the current state disambiguates: a
// Suspended CR means LOCKED for an arrears freeze but EXPIRED for a lapsed
// subscription, and the backend cannot tell which — only the platform knows
// why it asked for the suspension.
func ProposedState(phase Phase, currentState string) (string, bool) {
	candidates, ok := PhaseMapping[phase]
	if !ok || len(candidates) == 0 {
		return "", false
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	// Prefer staying in the current state if it is a legal reading of this
	// phase — the backend is confirming, not changing, the situation.
	for _, c := range candidates {
		if c == currentState {
			return c, true
		}
	}
	return candidates[0], true
}

// MockDriver is an in-memory Driver for integration and chaos testing
// (06§6.2). It is a first-class implementation, not a test fixture: 06 lists
// it alongside k8s and vm precisely so failure injection is available without
// a cluster.
type MockDriver struct {
	mu        sync.RWMutex
	resources map[string]*mockResource
	now       func() time.Time
	// FailApply, if set, makes Apply fail for matching resource ids —
	// the chaos hook.
	FailApply func(resourceID string) error
	// FailDelete similarly injects deletion failures, for exercising the
	// compensation-failure path.
	FailDelete func(resourceID string) error
	// ProvisioningDelay controls how many Query calls elapse before a resource
	// reports Ready, simulating real provisioning latency.
	ProvisioningDelay int
}

type mockResource struct {
	spec              Spec
	status            Status
	queryCount        int
	deleted           bool
	noticeDeliveredAt time.Time // zero = no reclaim notice delivered
}

// NewMockDriver builds a MockDriver.
func NewMockDriver(now func() time.Time) *MockDriver {
	if now == nil {
		now = time.Now
	}
	return &MockDriver{resources: make(map[string]*mockResource), now: now}
}

// Type identifies the driver.
func (d *MockDriver) Type() DriverType { return DriverMock }

// Apply creates or updates the resource.
func (d *MockDriver) Apply(spec Spec) (Status, error) {
	if err := spec.Validate(); err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	if d.FailApply != nil {
		if err := d.FailApply(spec.ResourceID); err != nil {
			return Status{}, err
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	existing, ok := d.resources[spec.ResourceID]
	if ok && !existing.deleted {
		// Idempotent: re-applying the same spec is a no-op that returns
		// current status rather than an error.
		if spec.Suspend != existing.spec.Suspend {
			existing.spec = spec
			existing.status.Phase = PhaseReady
			if spec.Suspend {
				existing.status.Phase = PhaseSuspended
			}
			existing.status.UpdatedAt = d.now()
			existing.status.ObservedGeneration++
		}
		return existing.status, nil
	}

	phase := PhaseProvisioning
	if d.ProvisioningDelay == 0 {
		phase = PhaseReady
	}
	res := &mockResource{
		spec: spec,
		status: Status{
			ResourceID:         spec.ResourceID,
			Phase:              phase,
			ObservedGeneration: 1,
			Conditions: []Condition{{
				Type: "Provisioned", Status: "False",
				Reason: "Provisioning", Message: "resource is being created",
				LastTransitionTime: d.now(),
			}},
			UpdatedAt: d.now(),
		},
	}
	d.resources[spec.ResourceID] = res
	return res.status, nil
}

// Delete reclaims the resource. Deleting a missing or already-deleted resource
// succeeds — saga compensation retries this, and an error would turn a
// successful cleanup into an escalation.
func (d *MockDriver) Delete(resourceID string) error {
	if d.FailDelete != nil {
		if err := d.FailDelete(resourceID); err != nil {
			return err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	res, ok := d.resources[resourceID]
	if !ok {
		return nil // already gone
	}
	res.deleted = true
	res.status.Phase = PhaseDeleting
	res.status.UpdatedAt = d.now()
	return nil
}

// Query returns observed status.
func (d *MockDriver) Query(resourceID string) (Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, ok := d.resources[resourceID]
	if !ok {
		return Status{}, ErrNotFound
	}
	res.queryCount++
	// Simulate provisioning latency: become Ready once enough polls elapse.
	if !res.deleted && res.status.Phase == PhaseProvisioning && res.queryCount >= d.ProvisioningDelay {
		res.status.Phase = PhaseReady
		res.status.Conditions = []Condition{{
			Type: "Provisioned", Status: "True",
			Reason: "Ready", Message: "resource is serving",
			LastTransitionTime: d.now(),
		}}
		res.status.UpdatedAt = d.now()
	}
	return res.status, nil
}

// CollectUsage returns metering readings. A suspended or deleted resource
// reports nothing: billing must stop the moment service stops.
func (d *MockDriver) CollectUsage(resourceID string) ([]UsagePoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	res, ok := d.resources[resourceID]
	if !ok {
		return nil, ErrNotFound
	}
	if res.deleted || res.spec.Suspend || res.status.Phase != PhaseReady {
		return nil, nil
	}
	return []UsagePoint{{
		MeteringItem:  "cpu_core_hour",
		Quantity:      "0.033333",
		WindowStart:   d.now().Truncate(time.Minute),
		WindowSeconds: 60,
	}}, nil
}

// Exists reports whether the driver still holds a live resource, for tests.
func (d *MockDriver) Exists(resourceID string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	res, ok := d.resources[resourceID]
	return ok && !res.deleted
}

// NotifyReclaim delivers the 5-minute warning to a spot resource. Idempotent:
// a replay returns the original delivered instant rather than resetting the
// clock — a customer must not lose notice time to a retried dispatch.
func (d *MockDriver) NotifyReclaim(resourceID string, deliveredAt time.Time) (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, ok := d.resources[resourceID]
	if !ok || res.deleted {
		return time.Time{}, ErrNotFound
	}
	if !res.noticeDeliveredAt.IsZero() {
		return res.noticeDeliveredAt, nil // idempotent replay
	}
	res.noticeDeliveredAt = deliveredAt
	return deliveredAt, nil
}

// ReclaimableAt returns the earliest reclaim instant (notice + 5m).
func (d *MockDriver) ReclaimableAt(resourceID string) (time.Time, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	res, ok := d.resources[resourceID]
	if !ok || res.deleted || res.noticeDeliveredAt.IsZero() {
		return time.Time{}, false
	}
	return res.noticeDeliveredAt.Add(ReclaimNoticeWindow), true
}

// Reclaim performs the reclaim after the notice window has elapsed. Reclaiming
// before the window, or without a prior notice, is an error — the platform
// cannot reclaim what it has not warned about.
func (d *MockDriver) Reclaim(resourceID string, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, ok := d.resources[resourceID]
	if !ok || res.deleted {
		return ErrNotFound
	}
	if res.noticeDeliveredAt.IsZero() {
		return ErrNoticeNotDelivered
	}
	if at.Before(res.noticeDeliveredAt.Add(ReclaimNoticeWindow)) {
		return ErrNoticeNotDelivered
	}
	res.status.Phase = PhaseDeleting
	// The backing resource is reclaimed; Delete completes the finalizer work.
	// Reclaim does NOT delete outright — the controller still runs the
	// finalizer (final metering + cleanup) so the last usage is not lost.
	return nil
}

// VMDriver is the phase-2 virtualization backend. The interface is frozen now
// so the control plane can be written against it; the implementation lands
// with KubeVirt in phase 2 (decision R-03).
//
// It returns ErrDriverNotReady rather than silently doing nothing, so a
// misconfigured catalogue entry fails loudly at dispatch instead of leaving an
// order stuck in CREATING until it times out.
type VMDriver struct{}

// Type identifies the driver.
func (VMDriver) Type() DriverType { return DriverVM }

// Apply is not implemented in phase 1.
func (VMDriver) Apply(Spec) (Status, error) { return Status{}, ErrDriverNotReady }

// Delete is not implemented in phase 1.
func (VMDriver) Delete(string) error { return ErrDriverNotReady }

// Query is not implemented in phase 1.
func (VMDriver) Query(string) (Status, error) { return Status{}, ErrDriverNotReady }

// CollectUsage is not implemented in phase 1.
func (VMDriver) CollectUsage(string) ([]UsagePoint, error) { return nil, ErrDriverNotReady }

// Registry resolves a product to its driver, so the control plane dispatches
// without knowing which backend a product uses. Safe for concurrent use:
// registration typically happens at startup, but a hot catalogue rebind may
// race dispatches, so both maps are guarded.
type Registry struct {
	mu      sync.RWMutex
	drivers map[DriverType]Driver
	// byProduct maps productCode → driver type, from the catalogue.
	byProduct map[string]DriverType
}

// NewRegistry builds an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		drivers:   make(map[DriverType]Driver),
		byProduct: make(map[string]DriverType),
	}
}

// Register adds a driver implementation.
func (r *Registry) Register(d Driver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drivers[d.Type()] = d
}

// BindProduct declares which driver fulfils a product. Switching a product to a
// different backend is this one line of catalogue configuration (06§6.2).
func (r *Registry) BindProduct(productCode string, t DriverType) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byProduct[productCode] = t
}

// DriverFor resolves the driver for a product.
func (r *Registry) DriverFor(productCode string) (Driver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byProduct[productCode]
	if !ok {
		return nil, fmt.Errorf("%w: product %q has no driver binding", ErrUnknownDriver, productCode)
	}
	d, ok := r.drivers[t]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownDriver, t)
	}
	return d, nil
}

// BoundProducts lists registered products, sorted, for diagnostics.
func (r *Registry) BoundProducts() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byProduct))
	for p := range r.byProduct {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// CRName returns the Kubernetes object name for a resource. It is the platform
// resource id verbatim (06§4.2), so an operator looking at a CR in the cluster
// and a support engineer looking at a bill are talking about the same string.
func CRName(resourceID string) string { return resourceID }

// CRKind returns the CRD kind for a product: {Product}Instance (06§4.2).
func CRKind(productCode string) string {
	if productCode == "" {
		return ""
	}
	// scecs → ScecsInstance
	return strings.ToUpper(productCode[:1]) + productCode[1:] + "Instance"
}

// CRNamespace returns the namespace for a shared-product CR: plat-{product}
// (06§4.2). Tenant-dedicated workloads use t-{account}-p-{project} instead.
func CRNamespace(productCode string) string { return "plat-" + productCode }

// TenantNamespace returns the per-tenant project namespace (06§3.2).
func TenantNamespace(accountID, projectID int64) string {
	return fmt.Sprintf("t-%d-p-%d", accountID, projectID)
}
