// vmdriver.go — the phase-2 VM fulfilment backend (decision R-03: KubeVirt).
//
// # The KubeVirt contract this driver speaks
//
// KubeVirt's object model separates the desired guest from the running one:
//
//	VirtualMachine          — the persistent, customer-owned object. Deleting it
//	                          deletes the VM AND its disks; halting it only stops
//	                          the guest. This is what makes 欠费冻结 (arrears
//	                          freeze, 01§5.4 D8) a runStrategy change: stop
//	                          serving, never destroy data.
//	VirtualMachineInstance  — the running guest KubeVirt creates from the VM.
//	                          Its Ready condition is the billing-safe "the
//	                          customer's workload is live" signal (06§4.1 rule 2:
//	                          status must be machine-readable, and rule 4: no
//	                          metering without launch).
//
// The driver translates provision.Spec → VirtualMachine and maps the observed
// VMI state back to provision.Status. It never talks to the guest (iron rule
// 1: the CR is the only fact interface).
//
// # Pluggable cluster client
//
// VMClient is the exact slice of the KubeVirt API the driver needs. The real
// adapter (kubevirt client-go: VirtualMachine CRUD + VMI watch) implements it
// in the cluster deploy; MemoryVMClient implements it in-process so the full
// lifecycle — create → poll Ready → suspend → resume → delete, with per-second
// metering — is exercisable without a cluster, mirroring how MockDriver keeps
// chaos injection available (06§6.2).
//
// # Zero value fails loudly
//
// VMDriver{} (no cluster client) returns ErrDriverNotReady from every method:
// a catalogue entry bound to the VM driver without a configured cluster must
// fail at dispatch, not strand an order in CREATING until timeout.
package provision

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// RunStrategy is KubeVirt's spec.runStrategy — the supported subset. RWO/Rerun
// OnFailure etc. are container semantics that do not map to a billed VM.
type RunStrategy string

const (
	// RunStrategyAlways keeps the guest running; KubeVirt recreates the VMI
	// if it dies. This is the only strategy a billed VM runs under.
	RunStrategyAlways RunStrategy = "Always"
	// RunStrategyHalted stops the guest but keeps the VirtualMachine object —
	// the 欠费冻结 mechanism. Data (disks) survives; the VMI is terminated.
	RunStrategyHalted RunStrategy = "Halted"
)

// DefaultVMResources are the catalogue defaults when a spec carries no
// cpu/mem_gb params.
const (
	DefaultVMCores   = 2
	DefaultVMMemGB   = 2
	meteringCPUItem  = "cpu_core_hour" // matches svc-catalog scecs metering items
	meteringMemItem  = "mem_gb_hour"
	usageSecondsHour = 3600
)

// VirtualMachine is the KubeVirt VirtualMachine subset the driver writes:
// everything else in the CR (disks, networks, cloud-init) is product-templated
// by the controller and invisible to the platform contract.
type VirtualMachine struct {
	Namespace   string
	Name        string
	Labels      map[string]string
	RunStrategy RunStrategy
	CPUCores    int
	MemoryGB    int
}

// VMObserved is the observed state the driver maps onto provision.Status:
// the VirtualMachine's existence and strategy, plus the VirtualMachineInstance
// KubeVirt derived from it.
type VMObserved struct {
	Exists      bool
	RunStrategy RunStrategy
	// VMIExists: a VirtualMachineInstance object is present. Under
	// RunStrategyAlways it appears while the guest boots; under Halted it is
	// absent.
	VMIExists bool
	// VMIReady: the VMI's Ready condition is True — the guest agent reports
	// the OS booted. This, not pod scheduling, is the billing-start signal.
	VMIReady bool
	// GuestIP is the VMI's pod-network address (the console's access
	// endpoint in the dev topology).
	GuestIP string
	// Generation is the VM's metadata.generation — how many times the
	// desired state changed. Mapped to Status.ObservedGeneration.
	Generation int64
}

// VMClient is the KubeVirt API surface the driver depends on. Apply and Delete
// must be idempotent: the control plane redelivers dispatches, and KubeVirt's
// own API semantics (create-or-update, delete-absent-succeeds) already are.
type VMClient interface {
	// Apply creates or updates the VirtualMachine to the desired state and
	// returns what was observed right after.
	Apply(vm VirtualMachine) (VMObserved, error)
	// Get observes the VirtualMachine and its instance. Returns ErrNotFound
	// when the VM object is absent.
	Get(namespace, name string) (VMObserved, error)
	// Delete removes the VirtualMachine (guest + disks). Deleting an absent
	// VM succeeds — the saga-compensation contract.
	Delete(namespace, name string) error
}

// vmDriverState carries everything the pointer-free VMDriver value needs:
// value receivers keep VMDriver{} constructible (registered as a plain struct
// value), while all instances built by NewVMDriver share one state.
type vmDriverState struct {
	mu     sync.Mutex
	client VMClient
	now    func() time.Time
	// refs maps resourceID → cluster coordinates, because the Driver
	// interface's Query/Delete/CollectUsage receive only the resource id.
	refs map[string]vmRef
	// lastCollect maps resourceID → last usage-collection instant, so each
	// CollectUsage call reports exactly the window since the previous one.
	lastCollect map[string]time.Time
}

type vmRef struct {
	Namespace string
	Name      string
	Spec      Spec // CPU/mem snapshot for metering math
}

// NewVMDriver builds a VM driver against a KubeVirt API client. now is
// injectable for deterministic tests; nil means time.Now.
func NewVMDriver(client VMClient, now func() time.Time) VMDriver {
	if now == nil {
		now = time.Now
	}
	return VMDriver{state: &vmDriverState{
		client:      client,
		now:         now,
		refs:        make(map[string]vmRef),
		lastCollect: make(map[string]time.Time),
	}}
}

// Type identifies the driver.
func (d VMDriver) Type() DriverType { return DriverVM }

// notReady guards every method of an unconfigured driver.
func (d VMDriver) notReady() error {
	if d.state == nil || d.state.client == nil {
		return ErrDriverNotReady
	}
	return nil
}

// vmFromSpec translates a provision.Spec into the KubeVirt object: the CR
// name is the resource id verbatim (one string in the cluster, on the bill and
// in the console), the namespace is the shared product namespace (06§4.2),
// labels are the four mandatory tenant-attribution labels, runStrategy encodes
// the suspend flag, and cpu/mem_gb come from the spec params.
func vmFromSpec(spec Spec) VirtualMachine {
	cpu, mem := DefaultVMCores, DefaultVMMemGB
	if v, err := strconv.Atoi(spec.Params["cpu"]); err == nil && v > 0 {
		cpu = v
	}
	if v, err := strconv.Atoi(spec.Params["mem_gb"]); err == nil && v > 0 {
		mem = v
	}
	strategy := RunStrategyAlways
	if spec.Suspend {
		strategy = RunStrategyHalted
	}
	return VirtualMachine{
		Namespace:   CRNamespace(spec.ProductCode),
		Name:        CRName(spec.ResourceID),
		Labels:      spec.Labels(),
		RunStrategy: strategy,
		CPUCores:    cpu,
		MemoryGB:    mem,
	}
}

// statusFrom maps observed KubeVirt state onto the platform Status contract.
//
// The phase order mirrors the VM lifecycle: a Ready VMI is serving; a VMI that
// exists but is not Ready is still booting (Provisioning); no VMI under
// Halted is the arrears freeze (Suspended). A VM under Always with no VMI at
// all is the instant between apply and the kubevirt controller reacting —
// still Provisioning, never Failed: absence of evidence is not failure.
func statusFrom(resourceID string, obs VMObserved, at time.Time) Status {
	phase := PhaseProvisioning
	condition := Condition{
		Type: "Provisioned", Status: "False",
		Reason: "Booting", Message: "virtual machine instance is starting",
		LastTransitionTime: at,
	}
	var endpoints []string

	switch {
	case obs.VMIReady:
		phase = PhaseReady
		condition = Condition{
			Type: "Provisioned", Status: "True",
			Reason: "VMIReady", Message: "guest OS booted, VMI Ready condition true",
			LastTransitionTime: at,
		}
		if obs.GuestIP != "" {
			endpoints = []string{obs.GuestIP}
		}
	case obs.RunStrategy == RunStrategyHalted && !obs.VMIExists:
		phase = PhaseSuspended
		condition = Condition{
			Type: "Provisioned", Status: "False",
			Reason: "Halted", Message: "guest halted for arrears freeze; disks retained",
			LastTransitionTime: at,
		}
	case !obs.VMIExists:
		condition.Reason = "AwaitingVMI"
		condition.Message = "waiting for kubevirt controller to create the instance"
	}

	return Status{
		ResourceID:         resourceID,
		Phase:              phase,
		ObservedGeneration: obs.Generation,
		Conditions:         []Condition{condition},
		Endpoints:          endpoints,
		UpdatedAt:          at,
	}
}

// Apply creates or updates the VM to match the spec. Applying an unchanged
// spec is a no-op returning current status; flipping Suspend flips the
// runStrategy — the whole 欠费冻结 mechanism is one field.
func (d VMDriver) Apply(spec Spec) (Status, error) {
	if err := d.notReady(); err != nil {
		return Status{}, err
	}
	if err := spec.Validate(); err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	obs, err := d.state.client.Apply(vmFromSpec(spec))
	if err != nil {
		return Status{}, err
	}
	d.state.refs[spec.ResourceID] = vmRef{
		Namespace: CRNamespace(spec.ProductCode),
		Name:      CRName(spec.ResourceID),
		Spec:      spec,
	}
	return statusFrom(spec.ResourceID, obs, d.state.now()), nil
}

// Delete reclaims the VM and its disks. Deleting an unknown or already-deleted
// resource succeeds — saga compensation retries this.
func (d VMDriver) Delete(resourceID string) error {
	if err := d.notReady(); err != nil {
		return err
	}
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	ref, ok := d.state.refs[resourceID]
	if !ok {
		return nil // never applied here; either already gone or not ours
	}
	if err := d.state.client.Delete(ref.Namespace, ref.Name); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	delete(d.state.refs, resourceID)
	delete(d.state.lastCollect, resourceID)
	return nil
}

// Query returns observed status — the polling fallback for lost callbacks.
func (d VMDriver) Query(resourceID string) (Status, error) {
	if err := d.notReady(); err != nil {
		return Status{}, err
	}
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	ref, ok := d.state.refs[resourceID]
	if !ok {
		return Status{}, ErrNotFound
	}
	obs, err := d.state.client.Get(ref.Namespace, ref.Name)
	if err != nil {
		return Status{}, err
	}
	return statusFrom(resourceID, obs, d.state.now()), nil
}

// CollectUsage reports metering for the window since the previous collection:
// cpu_core_hour and mem_gb_hour, per-second precision (the scecs metering
// items in svc-catalog, 01§5.2 — 计费起点为 RUNNING 时刻). Quantity is a
// decimal string computed in integer arithmetic; a float here would drift the
// exact way 09 A2 (无未解释差异) forbids.
//
// A halted or not-yet-ready VM reports nothing: billing stops the moment
// service stops.
func (d VMDriver) CollectUsage(resourceID string) ([]UsagePoint, error) {
	if err := d.notReady(); err != nil {
		return nil, err
	}
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	ref, ok := d.state.refs[resourceID]
	if !ok {
		return nil, ErrNotFound
	}
	obs, err := d.state.client.Get(ref.Namespace, ref.Name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil // reclaimed between calls: nothing to bill
		}
		return nil, err
	}
	if obs.RunStrategy == RunStrategyHalted || !obs.VMIReady {
		return nil, nil
	}

	now := d.state.now()
	windowStart := d.state.lastCollect[resourceID]
	if windowStart.IsZero() {
		// First collection: charge for the last minute, the granularity the
		// billing pipeline aggregates at.
		windowStart = now.Add(-time.Minute)
	}
	seconds := int64(now.Sub(windowStart).Seconds())
	if seconds <= 0 {
		return nil, nil
	}
	d.state.lastCollect[resourceID] = now

	cpuCores := int64(mustAtoi(ref.Spec.Params["cpu"]))
	memGB := int64(mustAtoi(ref.Spec.Params["mem_gb"]))
	if cpuCores <= 0 {
		cpuCores = DefaultVMCores
	}
	if memGB <= 0 {
		memGB = DefaultVMMemGB
	}
	return []UsagePoint{
		{
			MeteringItem: meteringCPUItem,
			Quantity:     decimalRate(cpuCores*seconds, usageSecondsHour),
			WindowStart:  windowStart,
			WindowSeconds: int(seconds),
		},
		{
			MeteringItem: meteringMemItem,
			Quantity:     decimalRate(memGB*seconds, usageSecondsHour),
			WindowStart:  windowStart,
			WindowSeconds: int(seconds),
		},
	}, nil
}

func mustAtoi(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

// decimalRate renders num/den as a fixed-point decimal string with 6 places,
// rounded half-up: symmetric for both sides of a bill and drift-free (integer
// math only), the residual under a micro-unit per window.
func decimalRate(num, den int64) string {
	if den == 0 {
		return "0"
	}
	whole := num / den
	frac := ((num % den) * 1_000_000 + den/2) / den
	return fmt.Sprintf("%d.%06d", whole, frac)
}

// MemoryVMClient is the in-process VMClient: a KubeVirt-shaped control loop
// with configurable boot latency. It is a first-class backend (the same status
// MockDriver has, 06§6.2) — the dev/demo topology and the driver tests both
// run on it; the cluster deploy swaps in the kubevirt client-go adapter behind
// the same interface.
type MemoryVMClient struct {
	mu sync.Mutex
	// vms is keyed "namespace/name".
	vms map[string]*memoryVM
	// ReadyDelay is how many Get observations elapse before a VMI reports
	// Ready — KubeVirt's async boot, made deterministic.
	ReadyDelay int
	seq        int
}

type memoryVM struct {
	vm           VirtualMachine
	generation   int64
	vmiExists    bool
	vmiReady     bool
	observations int
}

// NewMemoryVMClient builds an empty in-memory KubeVirt control loop.
// readyDelay=0 means a VMI is Ready the moment it is observed.
func NewMemoryVMClient(readyDelay int) *MemoryVMClient {
	return &MemoryVMClient{vms: make(map[string]*memoryVM), ReadyDelay: readyDelay}
}

func vmKey(namespace, name string) string { return namespace + "/" + name }

// Apply creates or updates the VirtualMachine, mimicking KubeVirt's reactions:
// a new Always VM gets an instance that boots asynchronously; switching to
// Halted terminates the instance; switching back re-creates one.
func (c *MemoryVMClient) Apply(vm VirtualMachine) (VMObserved, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := vmKey(vm.Namespace, vm.Name)
	existing, ok := c.vms[key]
	if !ok {
		c.vms[key] = &memoryVM{vm: vm, generation: 1, vmiExists: vm.RunStrategy == RunStrategyAlways}
		return c.observe(c.vms[key]), nil
	}
	existing.generation++
	existing.vm = vm
	if vm.RunStrategy == RunStrategyHalted {
		existing.vmiExists = false
		existing.vmiReady = false
		existing.observations = 0
	} else if !existing.vmiExists {
		// Halted → Always: kubevirt starts a fresh guest.
		existing.vmiExists = true
		existing.vmiReady = false
		existing.observations = 0
	}
	return c.observe(existing), nil
}

// Get observes the VM; each observation advances the boot simulation.
func (c *MemoryVMClient) Get(namespace, name string) (VMObserved, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.vms[vmKey(namespace, name)]
	if !ok {
		return VMObserved{}, ErrNotFound
	}
	return c.observe(v), nil
}

// Delete removes the VM object; deleting an absent VM succeeds.
func (c *MemoryVMClient) Delete(namespace, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.vms, vmKey(namespace, name))
	return nil
}

// observe advances one boot simulation step and renders the observed state.
// Caller holds the lock.
func (c *MemoryVMClient) observe(v *memoryVM) VMObserved {
	if v.vmiExists {
		v.observations++
		if v.observations > c.ReadyDelay {
			v.vmiReady = true
		}
	}
	obs := VMObserved{
		Exists:      true,
		RunStrategy: v.vm.RunStrategy,
		VMIExists:   v.vmiExists,
		VMIReady:    v.vmiReady,
		Generation:  v.generation,
	}
	if v.vmiReady {
		c.seq++
		obs.GuestIP = fmt.Sprintf("10.244.%d.%d", 1+c.seq/254, 1+c.seq%254)
	}
	return obs
}
