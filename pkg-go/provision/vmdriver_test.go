package provision

import (
	"errors"
	"testing"
	"time"
)

// The VM driver tests run the full KubeVirt-shaped lifecycle against the
// in-memory client: create → boot → Ready → meter → arrears freeze → resume →
// delete, plus the fail-loudly contract of an unconfigured driver.

var vmTestNow = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func newVMTestDriver(readyDelay int) (VMDriver, *MemoryVMClient) {
	c := NewMemoryVMClient(readyDelay)
	return NewVMDriver(c, func() time.Time { return vmTestNow }), c
}

// advance moves the clock by d and returns the new instant.
func advance(d time.Duration) time.Time {
	vmTestNow = vmTestNow.Add(d)
	return vmTestNow
}

func TestVMDriverZeroValueFailsLoudly(t *testing.T) {
	// An unconfigured driver must refuse everything: a catalogue entry bound
	// to vm without a cluster would otherwise strand orders in CREATING.
	var d Driver = VMDriver{}
	if _, err := d.Apply(validSpec()); !errors.Is(err, ErrDriverNotReady) {
		t.Fatalf("Apply: %v", err)
	}
	if err := d.Delete("x"); !errors.Is(err, ErrDriverNotReady) {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := d.Query("x"); !errors.Is(err, ErrDriverNotReady) {
		t.Fatalf("Query: %v", err)
	}
	if _, err := d.CollectUsage("x"); !errors.Is(err, ErrDriverNotReady) {
		t.Fatalf("CollectUsage: %v", err)
	}
}

func TestVMDriverLifecycle(t *testing.T) {
	d, _ := newVMTestDriver(1) // VMI becomes Ready on the 2nd observation

	// Create: VMI exists but is booting → Provisioning, not Ready.
	st, err := d.Apply(validSpec())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if st.Phase != PhaseProvisioning {
		t.Fatalf("right after apply: phase %s, want Provisioning", st.Phase)
	}
	// The CR coordinates follow the 06§4.2 conventions: resource id verbatim
	// as the object name, shared product namespace, four mandatory labels.
	ref := d.state.refs[validSpec().ResourceID]
	if ref.Name != "scecs-cn-north-1-01-a1b2c3d4" || ref.Namespace != "plat-scecs" {
		t.Fatalf("CR coordinates wrong: %s/%s", ref.Namespace, ref.Name)
	}

	// Poll: the guest boots → Ready, with an endpoint to hand the console.
	st, err = d.Query(validSpec().ResourceID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if st.Phase != PhaseReady {
		t.Fatalf("after boot: phase %s, want Ready", st.Phase)
	}
	if len(st.Endpoints) == 0 || st.Endpoints[0] == "" {
		t.Errorf("Ready VM must expose a guest endpoint: %+v", st)
	}
	if c, ok := st.ConditionByType("Provisioned"); !ok || c.Status != "True" {
		t.Errorf("Provisioned condition must be True when Ready: %+v", c)
	}
}

func TestVMDriverApplyIdempotent(t *testing.T) {
	d, _ := newVMTestDriver(0)
	s := validSpec()
	first, err := d.Apply(s)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for i := 0; i < 4; i++ {
		again, err := d.Apply(s)
		if err != nil || again.Phase != first.Phase {
			t.Fatalf("re-apply %d: %v %s vs %s", i, err, again.Phase, first.Phase)
		}
	}
}

func TestVMDriverArrearsFreezeAndResume(t *testing.T) {
	d, _ := newVMTestDriver(0)
	s := validSpec()
	if _, err := d.Apply(s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := d.CollectUsage(s.ResourceID); err != nil {
		t.Fatalf("usage while running: %v", err)
	}

	// 欠费冻结: runStrategy → Halted. Guest stops, disks (the VM object) stay.
	s.Suspend = true
	st, err := d.Apply(s)
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if st.Phase != PhaseSuspended {
		t.Fatalf("suspended phase = %s, want Suspended", st.Phase)
	}
	if usage, _ := d.CollectUsage(s.ResourceID); len(usage) != 0 {
		t.Fatalf("billing must stop with service: %v", usage)
	}

	// Customer pays: runStrategy → Always. The VM (and its disks) come back
	// intact — same object, fresh guest.
	s.Suspend = false
	st, err = d.Apply(s)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if st.Phase == PhaseSuspended {
		t.Fatal("resumed VM still reports Suspended")
	}
}

func TestVMDriverMeteringPerSecond(t *testing.T) {
	d, _ := newVMTestDriver(0)
	s := validSpec() // cpu=2, mem_gb=4
	if _, err := d.Apply(s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	d.Query(s.ResourceID) // boot → Ready

	// First collection charges a 60s window: 2 cores × 60s = 0.033333
	// core-hours; 4 GB × 60s = 0.066667 GB-hours. Integer math, no drift.
	advance(2 * time.Minute)
	pts, err := d.CollectUsage(s.ResourceID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("want 2 usage points (cpu+mem), got %d", len(pts))
	}
	want := map[string]string{
		meteringCPUItem: "0.033333",
		meteringMemItem: "0.066667",
	}
	for _, p := range pts {
		if p.Quantity != want[p.MeteringItem] {
			t.Errorf("%s = %s, want %s", p.MeteringItem, p.Quantity, want[p.MeteringItem])
		}
		if p.WindowSeconds != 60 {
			t.Errorf("%s window = %ds, want 60 (first-collection granularity)", p.MeteringItem, p.WindowSeconds)
		}
	}

	// Second collection charges only the elapsed window since the last one.
	advance(30 * time.Second)
	pts, _ = d.CollectUsage(s.ResourceID)
	if len(pts) != 2 || pts[0].WindowSeconds != 30 {
		t.Fatalf("second collection should carry a 30s window: %+v", pts)
	}
}

func TestVMDriverMeteringStopsWhenNotReady(t *testing.T) {
	d, _ := newVMTestDriver(5) // long boot
	s := validSpec()
	if _, err := d.Apply(s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Still booting: no billing before the guest OS is up (01§5.2: 计费起点
	// 为 RUNNING 时刻).
	if pts, _ := d.CollectUsage(s.ResourceID); len(pts) != 0 {
		t.Fatalf("billing before Ready: %v", pts)
	}
}

func TestVMDriverDeleteIdempotent(t *testing.T) {
	d, _ := newVMTestDriver(0)
	s := validSpec()
	if _, err := d.Apply(s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := d.Delete(s.ResourceID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := d.Delete(s.ResourceID); err != nil {
			t.Fatalf("repeat delete %d: %v", i, err)
		}
	}
	// Deleting a resource the driver never saw also succeeds.
	if err := d.Delete("scecs-cn-north-1-01-ffffffff"); err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
	if _, err := d.Query(s.ResourceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("query after delete: %v", err)
	}
}

func TestVMDriverRejectsInvalidSpec(t *testing.T) {
	d, _ := newVMTestDriver(0)
	s := validSpec()
	s.AccountID = 0 // no tenant attribution
	if _, err := d.Apply(s); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
}

func TestVMDriverDefaultsResources(t *testing.T) {
	// A spec with no cpu/mem params still provisions — the catalogue floor
	// SKU applies rather than a zero-core VM.
	d, _ := newVMTestDriver(0)
	s := validSpec()
	s.Params = nil
	if _, err := d.Apply(s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ref := d.state.refs[s.ResourceID]
	mem := d.state.client.(*MemoryVMClient).vms[vmKey(ref.Namespace, ref.Name)]
	if mem.vm.CPUCores != DefaultVMCores || mem.vm.MemoryGB != DefaultVMMemGB {
		t.Fatalf("defaults not applied: %d cores / %d GB", mem.vm.CPUCores, mem.vm.MemoryGB)
	}
}

func TestVMRegistrySwitchesBackend(t *testing.T) {
	// 06§6.2: switching SCECS from containers to VMs is one catalogue line,
	// and the VM driver now actually works.
	r := NewRegistry()
	vmDriver, _ := newVMTestDriver(0)
	r.Register(NewMockDriver(func() time.Time { return vmTestNow }))
	r.Register(vmDriver)

	r.BindProduct("scecs", DriverMock)
	if d, err := r.DriverFor("scecs"); err != nil || d.Type() != DriverMock {
		t.Fatalf("mock binding broken: %v %v", d, err)
	}
	r.BindProduct("scecs", DriverVM)
	d, err := r.DriverFor("scecs")
	if err != nil || d.Type() != DriverVM {
		t.Fatalf("vm binding broken: %v %v", d, err)
	}
	st, err := d.Apply(validSpec())
	if err != nil || st.Phase != PhaseReady {
		t.Fatalf("VM driver via registry: %v %s", err, st.Phase)
	}
}
