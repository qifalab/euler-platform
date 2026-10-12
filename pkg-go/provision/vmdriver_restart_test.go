package provision

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// memSpecs is a SpecSource over a map — what svc-orchestrator's provision_task
// is to the real deployment.
type memSpecs struct {
	mu      sync.Mutex
	specs   map[string]Spec
	err     error
	lookups int
}

func (m *memSpecs) AppliedSpec(resourceID string) (Spec, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookups++
	if m.err != nil {
		return Spec{}, false, m.err
	}
	s, ok := m.specs[resourceID]
	return s, ok, nil
}

// restartedVMDriver applies spec on one driver, then returns a second driver
// over the same cluster that has never seen it — a restarted process.
func restartedVMDriver(t *testing.T, spec Spec, specs SpecSource) (VMDriver, *MemoryVMClient) {
	t.Helper()
	cluster := NewMemoryVMClient(0)
	now := func() time.Time { return time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC) }
	if _, err := NewVMDriver(cluster, now).Apply(spec); err != nil {
		t.Fatalf("apply before restart: %v", err)
	}
	return NewVMDriverWithSpecs(cluster, now, specs), cluster
}

// O7: after a restart the driver rebuilds the VM coordinates from the
// persisted spec, so Delete really deletes (it used to answer "never applied
// here" and leave the VM running) and Query/CollectUsage keep working.
func TestVMDriverRebuildsRefsAfterRestart(t *testing.T) {
	spec := validSpec()
	specs := &memSpecs{specs: map[string]Spec{spec.ResourceID: spec}}
	d, cluster := restartedVMDriver(t, spec, specs)

	st, err := d.Query(spec.ResourceID)
	if err != nil || st.Phase != PhaseReady {
		t.Fatalf("query after restart: %+v %v, want Ready", st, err)
	}
	if usage, err := d.CollectUsage(spec.ResourceID); err != nil || len(usage) != 2 {
		t.Fatalf("usage after restart: %v %v", usage, err)
	}
	if err := d.Delete(spec.ResourceID); err != nil {
		t.Fatalf("delete after restart: %v", err)
	}
	if _, err := cluster.Get(CRNamespace(spec.ProductCode), CRName(spec.ResourceID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the VM survived a delete after restart: %v", err)
	}
	// The rebuilt entry is cached: one lookup served all three calls.
	if specs.lookups != 1 {
		t.Fatalf("spec lookups = %d, want 1", specs.lookups)
	}
	// Deleting again (a compensation retry) still succeeds.
	if err := d.Delete(spec.ResourceID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// Without a SpecSource the restarted driver cannot see the VM — the gap the
// SpecSource closes, kept as the documented behaviour of NewVMDriver.
func TestVMDriverWithoutSpecsForgetsOnRestart(t *testing.T) {
	spec := validSpec()
	d, cluster := restartedVMDriver(t, spec, nil)
	if _, err := d.Query(spec.ResourceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("query: %v, want ErrNotFound", err)
	}
	if err := d.Delete(spec.ResourceID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := cluster.Get(CRNamespace(spec.ProductCode), CRName(spec.ResourceID)); err != nil {
		t.Fatalf("without a spec source the VM is untouched: %v", err)
	}
}

func TestVMDriverSpecSourceFailureIsNotSuccess(t *testing.T) {
	spec := validSpec()
	specs := &memSpecs{err: errors.New("resource_db unreachable")}
	d, cluster := restartedVMDriver(t, spec, specs)
	if err := d.Delete(spec.ResourceID); err == nil {
		t.Fatal("delete reported success while the spec store was unreachable")
	}
	if _, err := d.Query(spec.ResourceID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("query: %v, want a lookup error (not ErrNotFound)", err)
	}
	if _, err := d.CollectUsage(spec.ResourceID); err == nil {
		t.Fatal("usage collection hid a lookup failure")
	}
	if _, err := cluster.Get(CRNamespace(spec.ProductCode), CRName(spec.ResourceID)); err != nil {
		t.Fatalf("VM gone after a failed delete: %v", err)
	}
	// Once the store is back, the retry deletes.
	specs.mu.Lock()
	specs.err, specs.specs = nil, map[string]Spec{spec.ResourceID: spec}
	specs.mu.Unlock()
	if err := d.Delete(spec.ResourceID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := cluster.Get(CRNamespace(spec.ProductCode), CRName(spec.ResourceID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry did not delete: %v", err)
	}
}

func TestVMDriverIgnoresForeignSpecRecord(t *testing.T) {
	spec := validSpec()
	other := spec
	other.ResourceID = "euecs-cn-north-1-01-ffffffff"
	specs := &memSpecs{specs: map[string]Spec{spec.ResourceID: other}}
	d, cluster := restartedVMDriver(t, spec, specs)
	if _, err := d.Query(spec.ResourceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("query with a record naming another resource: %v, want ErrNotFound", err)
	}
	if err := d.Delete(spec.ResourceID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := cluster.Get(CRNamespace(spec.ProductCode), CRName(spec.ResourceID)); err != nil {
		t.Fatalf("a foreign record must not steer the delete: %v", err)
	}
	if _, err := d.Query("never-applied"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown resource: %v, want ErrNotFound", err)
	}
}
