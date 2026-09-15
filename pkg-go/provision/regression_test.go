package provision

import (
	"testing"
	"time"
)

// A halted VM (arrears freeze) must not be billed for the frozen window when
// it resumes: the collection clock has to advance while nothing is billable,
// otherwise the first post-resume window spans the whole halt.
func TestVMDriverDoesNotBillTheFrozenWindow(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	d := NewVMDriver(NewMemoryVMClient(0), func() time.Time { return clock })
	spec := fixSpec("r-freeze")
	spec.Params = map[string]string{"cpu": "2", "mem_gb": "4"}

	if _, err := d.Apply(spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	clock = base.Add(time.Minute)
	pts, err := d.CollectUsage("r-freeze")
	if err != nil || len(pts) == 0 {
		t.Fatalf("expected billable usage while running: %d points / %v", len(pts), err)
	}

	// Arrears freeze: service stops, billing stops.
	frozen := spec
	frozen.Suspend = true
	if _, err := d.Apply(frozen); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	clock = base.Add(24 * time.Hour)
	if pts, err := d.CollectUsage("r-freeze"); err != nil || len(pts) != 0 {
		t.Fatalf("frozen VM must report nothing: %d points / %v", len(pts), err)
	}

	// Resume: only a normal collection window may be billed, never the halted
	// day (the pre-fix behaviour reported the whole 24h as usage).
	resumed := spec
	resumed.Suspend = false
	if _, err := d.Apply(resumed); err != nil {
		t.Fatalf("resume: %v", err)
	}
	clock = base.Add(24*time.Hour + time.Minute)
	pts, err = d.CollectUsage("r-freeze")
	if err != nil || len(pts) == 0 {
		t.Fatalf("expected usage after resume: %d points / %v", len(pts), err)
	}
	if pts[0].WindowSeconds > 3600 {
		t.Fatalf("resumed window = %ds; the halted period must not be billed", pts[0].WindowSeconds)
	}
}

// MockDriver.Apply must land ANY spec change, not only a Suspend flip: a resize
// that is silently dropped leaves metering on the old shape and hands the
// customer a bill that disagrees with the requested configuration.
func TestMockDriverAppliesSpecChanges(t *testing.T) {
	d := NewMockDriver(func() time.Time { return time.Unix(0, 0) })
	spec := fixSpec("r-spec")
	spec.Params = map[string]string{"cpu": "2"}
	if _, err := d.Apply(spec); err != nil {
		t.Fatal(err)
	}
	resized := spec
	resized.Params = map[string]string{"cpu": "8"}
	if _, err := d.Apply(resized); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	got := d.resources["r-spec"].spec.Params["cpu"]
	d.mu.Unlock()
	if got != "8" {
		t.Fatalf("stored spec cpu = %s, want 8 (the resize was ignored)", got)
	}
}
