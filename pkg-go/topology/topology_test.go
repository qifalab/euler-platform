package topology

import (
	"reflect"
	"sort"
	"testing"

	"github.com/qifalab/euler-platform/identifier"
)

func TestRegionScopeValid(t *testing.T) {
	cases := []struct {
		s     RegionScope
		want  bool
		zonal bool
	}{
		{ScopeRegional, true, false},
		{ScopeZonal, true, true},
		{"", false, false},
		{"regional", false, false}, // wrong case → catalogue error, not defaulted
		{"Zonal", false, false},
		{"FEDERATED", false, false},
	}
	for _, c := range cases {
		if c.s.Valid() != c.want {
			t.Errorf("%q.Valid() = %v, want %v", c.s, c.s.Valid(), c.want)
		}
		if c.s.Zonal() != c.zonal {
			t.Errorf("%q.Zonal() = %v, want %v", c.s, c.s.Zonal(), c.zonal)
		}
	}
}

func TestParseAZName(t *testing.T) {
	az, err := ParseAZName("cn-north-1-a")
	if err != nil {
		t.Fatalf("ParseAZName: %v", err)
	}
	if az.Region != "cn-north-1" || az.Letter != "a" || az.Name() != "cn-north-1-a" {
		t.Fatalf("parsed wrong: %+v name=%q", az, az.Name())
	}
}

func TestParseAZNameRejectsBad(t *testing.T) {
	cases := []string{
		"",              // empty
		"cn-north-1",    // no zone letter (this is a region name)
		"cnnorth1-a",    // region not hyphen-style
		"cn-north-1-ab", // two-letter zone
		"cn-north-1-1",  // digit zone (collides with region index)
		"cn-north-1-",   // empty letter
	}
	for _, c := range cases {
		if _, err := ParseAZName(c); err == nil {
			t.Fatalf("ParseAZName(%q) should fail", c)
		}
	}
}

func TestNewTopologyDedupesAndSorts(t *testing.T) {
	// zones passed out of order with a duplicate → sorted, deduped
	topo, err := NewTopology("cn-north-1", []string{"b", "a", "a"})
	if err != nil {
		t.Fatalf("NewTopology: %v", err)
	}
	if !topo.IsDualAZ() {
		t.Fatalf("expected dual AZ, got %d zones", len(topo.Zones))
	}
	want := []string{"cn-north-1-a", "cn-north-1-b"}
	if got := topo.ZoneNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ZoneNames = %v, want %v", got, want)
	}
}

func TestNewTopologyRejectsBadRegion(t *testing.T) {
	if _, err := NewTopology("cnnorth1", []string{"a"}); err == nil {
		t.Fatal("should reject bad region")
	}
	if _, err := NewTopology("cn-north-1", []string{}); err == nil {
		t.Fatal("should reject empty zones")
	}
	if _, err := NewTopology("cn-north-1", []string{"1"}); err == nil {
		t.Fatal("should reject digit zone letter")
	}
}

// --- the survival math; the invariant the M-6 failover contract rests on ---

func TestSurvivesAZLoss(t *testing.T) {
	topo, _ := NewTopology("cn-north-1", []string{"a", "b"})

	// The generic (non-quorum) SurvivesAZLoss rule: losing any single zone
	// must leave >= 1 replica. Even spread across 2 zones satisfies this for
	// every n >= 2 (each zone keeps at least floor(n/2) >= 1 elsewhere);
	// n = 1 never survives. Quorum systems use SurvivesAZLossMGR instead.
	for n := 2; n <= 6; n++ {
		p, _ := topo.Distribute(n)
		if !p.SurvivesAZLoss() {
			t.Errorf("even-spread %d replicas across 2 zones should survive non-quorum AZ loss (>=1 replica remains)", n)
		}
	}
	// Single replica: losing its zone loses everything.
	p1, _ := topo.Distribute(1)
	if p1.SurvivesAZLoss() {
		t.Error("1 replica can never survive AZ loss")
	}
	// All replicas concentrated in one zone never survives.
	concentrated := Placement{ByZone: map[string]int{"cn-north-1-a": 3, "cn-north-1-b": 0}, Total: 3}
	if concentrated.SurvivesAZLoss() {
		t.Error("a placement concentrated in one zone must not survive that zone's loss")
	}
}

func TestSurvivesAZLossThreeZones(t *testing.T) {
	// 3-zone topology: even spread of 3 = (1,1,1); lose any one → 2 remain > 1 → survives.
	topo, _ := NewTopology("cn-north-1", []string{"a", "b", "c"})
	p, _ := topo.Distribute(3)
	if !p.SurvivesAZLoss() {
		t.Fatal("3 replicas across 3 zones (1,1,1) should survive AZ loss")
	}
	// 6 across 3 = (2,2,2): lose one → 4 remain > 2 → survives.
	p6, _ := topo.Distribute(6)
	if !p6.SurvivesAZLoss() {
		t.Fatal("6 replicas across 3 zones (2,2,2) should survive AZ loss")
	}
}

func TestSurvivesAZLossConcentrated(t *testing.T) {
	// All replicas in one zone → never survives.
	p := Placement{ByZone: map[string]int{"cn-north-1-a": 3, "cn-north-1-b": 0}, Total: 3}
	if p.SurvivesAZLoss() {
		t.Fatal("all-replicas-in-one-AZ placement should not survive AZ loss")
	}
}

// --- MGR-specific survival: the actual MySQL single-primary contract ---

func TestSurvivesAZLossMGR(t *testing.T) {
	topo, _ := NewTopology("cn-north-1", []string{"a", "b"})

	// MGR group of 3 across 2 zones, even spread = (2,1):
	//   lose the 1-zone → 2 remain, need > floor(3/2)=1 → 2>1 ✓ survives
	//   lose the 2-zone → 1 remains, need > 1 → 1>1 ✗ does NOT survive
	// So (2,1) does NOT survive either-zone loss. MGR across 2 zones needs 4+
	// placed as (2,2): lose one → 2 remain, need > 2? floor(4/2)=2, 2>2 false → NO.
	// The genuinely-safe MGR across 2 zones is 5 nodes (3,2): lose the 3 → 2
	// remain, need > floor(5/2)=2 → 2>2 false. Still no.
	// MGR across 2 zones with EITHER-zone survival is only satisfiable with 3
	// zones (one node per zone, lose one → 2 > floor(3/2)=1 ✓). This is the
	// hard constraint the spec encodes: MySQL MGR P2 is "1 primary + 2
	// secondaries" but across 2 AZs the heavier AZ is a hazard; the runbook
	// must place it so the primary's AZ is the survivable side, and the drill
	// targets the secondary AZ. We assert the math, not the aspiration.
	for n := 1; n <= 5; n++ {
		p, _ := topo.Distribute(n)
		survives := p.SurvivesAZLossMGR()
		// Across 2 zones, even-spread MGR never survives either-zone loss for
		// n<=5 (and in fact never for any n on 2 zones). Document by asserting.
		if survives {
			t.Errorf("MGR %d across 2 zones (even) survived either-zone loss — should not", n)
		}
	}

	// 3 zones: 3 nodes (1,1,1). Lose any → 2 remain, need > floor(3/2)=1 → survives.
	topo3, _ := NewTopology("cn-north-1", []string{"a", "b", "c"})
	p3, _ := topo3.Distribute(3)
	if !p3.SurvivesAZLossMGR() {
		t.Fatal("MGR 3 nodes across 3 zones should survive either-zone loss (2 > 1 majority)")
	}
}

func TestCanSatisfy(t *testing.T) {
	dual, _ := NewTopology("cn-north-1", []string{"a", "b"})
	triple, _ := NewTopology("cn-north-1", []string{"a", "b", "c"})

	// Across 2 zones, non-quorum survival is satisfiable from 2 replicas up.
	if !dual.CanSatisfy(3, false) {
		t.Fatal("dual-AZ non-quorum survival of 3 replicas should be satisfiable")
	}
	if dual.CanSatisfy(1, false) {
		t.Fatal("a single replica can never satisfy AZ-loss survival")
	}
	// MGR across 2 zones is mathematically impossible for ANY replica count:
	// one zone always holds >= half the members, so quorum cannot survive its
	// loss — a third arbitration point is required (see SurvivesAZLossMGR doc).
	for n := 1; n <= 7; n++ {
		if dual.CanSatisfy(n, true) {
			t.Fatalf("dual-AZ MGR survival must be unsatisfiable, but %d replicas passed", n)
		}
	}
	// Across 3 zones, 3 replicas generic survives.
	if !triple.CanSatisfy(3, false) {
		t.Fatal("triple-AZ generic survival of 3 replicas should be satisfiable")
	}
	// MGR contract: 3 nodes across 3 zones survives.
	if !triple.CanSatisfy(3, true) {
		t.Fatal("MGR 3-across-3 should be satisfiable")
	}
}

func TestDistributeEvenSpread(t *testing.T) {
	topo, _ := NewTopology("cn-north-1", []string{"a", "b"})
	// 5 across 2 → (3,2), remainder on the first zone (deterministic).
	p, err := topo.Distribute(5)
	if err != nil {
		t.Fatalf("Distribute: %v", err)
	}
	if p.Total != 5 {
		t.Fatalf("Total = %d, want 5", p.Total)
	}
	if p.ByZone["cn-north-1-a"] != 3 || p.ByZone["cn-north-1-b"] != 2 {
		t.Fatalf("distribution = %v, want a:3 b:2", p.ByZone)
	}
}

func TestDistributeDeterministic(t *testing.T) {
	topo, _ := NewTopology("cn-north-1", []string{"b", "a"}) // order shouldn't matter
	p1, _ := topo.Distribute(4)
	p2, _ := topo.Distribute(4)
	if !reflect.DeepEqual(p1.ByZone, p2.ByZone) {
		t.Fatalf("non-deterministic distribution: %v vs %v", p1.ByZone, p2.ByZone)
	}
	// sorted internally so a/b order is stable regardless of input order
	if p1.ByZone["cn-north-1-a"] != 2 || p1.ByZone["cn-north-1-b"] != 2 {
		t.Fatalf("expected (2,2), got %v", p1.ByZone)
	}
}

func TestDistributeRejectsZero(t *testing.T) {
	topo, _ := NewTopology("cn-north-1", []string{"a", "b"})
	if _, err := topo.Distribute(0); err == nil {
		t.Fatal("Distribute(0) should fail")
	}
	if _, err := topo.Distribute(-1); err == nil {
		t.Fatal("Distribute(-1) should fail")
	}
}

func TestFaultDomainSpread(t *testing.T) {
	sel := map[string]string{"app": "svc-billing"}
	spread := FaultDomainSpread(sel)
	if spread["maxSkew"] != 1 {
		t.Errorf("maxSkew = %v, want 1", spread["maxSkew"])
	}
	if spread["topologyKey"] != "topology.kubernetes.io/zone" {
		t.Errorf("topologyKey = %v, want zone", spread["topologyKey"])
	}
	if spread["whenUnsatisfiable"] != "DoNotSchedule" {
		t.Errorf("whenUnsatisfiable = %v, want DoNotSchedule (hard, not ScheduleAnyway)", spread["whenUnsatisfiable"])
	}
	ls := spread["labelSelector"].(map[string]any)
	if !reflect.DeepEqual(ls["matchLabels"], sel) {
		t.Errorf("labelSelector mismatch: %v vs %v", ls["matchLabels"], sel)
	}
}

func TestPlacementReportAtRisk(t *testing.T) {
	// 3 across 2 zones even-spread = (2,1), non-quorum contract:
	// losing either zone still leaves >= 1 replica → survives, no zone at risk.
	topo, _ := NewTopology("cn-north-1", []string{"a", "b"})
	p, _ := topo.Distribute(3) // (2,1)
	rep := p.Report(false)
	if !rep.Survives {
		t.Fatal("3-across-2 even spread should survive non-quorum AZ loss")
	}
	if len(rep.AtRiskZones) != 0 {
		t.Fatalf("AtRiskZones = %v, want none", rep.AtRiskZones)
	}
	// A placement holding everything in one zone flags that zone as at risk.
	solo := Placement{ByZone: map[string]int{"cn-north-1-a": 3, "cn-north-1-b": 0}, Total: 3}
	repSolo := solo.Report(false)
	if repSolo.Survives {
		t.Fatal("all-in-one-zone must not survive")
	}
	want := []string{"cn-north-1-a"}
	if !reflect.DeepEqual(repSolo.AtRiskZones, want) {
		t.Fatalf("AtRiskZones = %v, want %v", repSolo.AtRiskZones, want)
	}
}

func TestReportMGRAtRisk(t *testing.T) {
	// MGR: 3 nodes across 3 zones (1,1,1) survives; at-risk should be empty.
	topo, _ := NewTopology("cn-north-1", []string{"a", "b", "c"})
	p, _ := topo.Distribute(3)
	rep := p.Report(true)
	if !rep.SurvivesMGR {
		t.Fatal("MGR 3-across-3 should survive")
	}
	if len(rep.AtRiskZones) != 0 {
		t.Fatalf("expected no at-risk zones, got %v", rep.AtRiskZones)
	}
}

// Integration: the placement API is internally consistent — a placement that
// CanSatisfy reports satisfiable, when actually distributed, survives.
func TestCanSatisfyMatchesSurvival(t *testing.T) {
	triple, _ := NewTopology("cn-north-1", []string{"a", "b", "c"})
	for n := 3; n <= 9; n++ {
		want := triple.CanSatisfy(n, false)
		p, _ := triple.Distribute(n)
		got := p.SurvivesAZLoss()
		if want != got {
			t.Errorf("replicas=%d: CanSatisfy=%v but SurvivesAZLoss=%v (inconsistent)", n, want, got)
		}
	}
}

// Ensure the package does not re-define the AZ/region naming convention — it
// must delegate to identifier (single source of truth for names).
func TestNamingDelegatesToIdentifier(t *testing.T) {
	az := AZ{Region: "cn-east-1", Letter: "b"}
	if az.Name() != identifier.AZName("cn-east-1", "b") {
		t.Fatal("AZ.Name must delegate to identifier.AZName")
	}
}

// Stable ordering helper used by Report — sanity check that AtRiskZones is
// sorted so the report is deterministic across map-iteration order.
func TestAtRiskZonesSorted(t *testing.T) {
	topo, _ := NewTopology("cn-north-1", []string{"a", "b", "c"})
	p, _ := topo.Distribute(4) // (2,1,1): lose the 2 → 2 remain, 2<=2 → at risk
	rep := p.Report(false)
	if !sort.StringsAreSorted(rep.AtRiskZones) {
		t.Fatalf("AtRiskZones not sorted: %v", rep.AtRiskZones)
	}
}
