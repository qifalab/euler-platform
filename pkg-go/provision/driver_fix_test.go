package provision

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixSpec(id string) Spec {
	return Spec{
		ResourceID: id, AccountID: 100123, ProjectID: 1,
		ProductCode: "euecs", Region: "cn-north-1",
		IdempotencyKey: "ord-" + id,
	}
}

// A ZONAL product must carry a Zone: a VM with no AZ of residence cannot be
// placed, and deferring the choice to the backend is a mis-layering.
func TestValidateZonalRequiresZone(t *testing.T) {
	s := fixSpec("r-1")
	s.RegionScope = "ZONAL"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "Zone") {
		t.Fatalf("ZONAL without Zone must fail: %v", err)
	}
	s.Zone = "cn-north-1-a"
	if err := s.Validate(); err != nil {
		t.Fatalf("ZONAL with Zone must pass: %v", err)
	}
	// REGIONAL and legacy (empty scope) specs remain valid without a zone.
	s2 := fixSpec("r-2")
	s2.RegionScope = "REGIONAL"
	if err := s2.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := fixSpec("r-3").Validate(); err != nil {
		t.Fatal(err)
	}
}

// MockDriver's full surface and the Registry must be safe under -race:
// concurrent reconcile loops, usage collectors and reclaim paths all touch the
// same maps.
func TestMockDriverAndRegistryConcurrency(t *testing.T) {
	d := NewMockDriver(func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) })
	d.ProvisioningDelay = 2
	reg := NewRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("r-%d", i%4) // deliberate collisions
			spec := fixSpec(id)
			if _, err := d.Apply(spec); err != nil {
				t.Errorf("Apply: %v", err)
			}
			_, _ = d.Query(id)
			_, _ = d.CollectUsage(id)
			_ = d.Exists(id)
			if i%2 == 0 {
				_ = d.Delete(id)
			}
			reg.Register(d)
			reg.BindProduct(fmt.Sprintf("p%d", i), DriverMock)
			_, _ = reg.DriverFor("p0")
			_ = reg.BoundProducts()
		}(i)
	}
	wg.Wait()
}
