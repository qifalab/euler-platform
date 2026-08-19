package multiregion

import (
	"strings"
	"testing"
	"time"
)

// validPlan returns the canonical 两地三中心 plan: cn-north-1 primary with the
// P2 dual-AZ shape, cn-east-1 standby >300km away, both standard replication
// channels (00§4.5).
func validPlan() Plan {
	return Plan{
		Primary: Region{Name: "cn-north-1", Role: RolePrimary, DistanceKm: 0, ZoneLetters: []string{"a", "b"}},
		Standby: Region{Name: "cn-east-1", Role: RoleStandby, DistanceKm: 1200, ZoneLetters: []string{"a"}},
		Channels: []ReplicationChannel{
			CoreLedgerChannel(),
			ObjectStorageChannel(),
		},
	}
}

func TestValidateValidPlan(t *testing.T) {
	if err := validPlan().Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
}

func TestValidateRejectsStandbyTooClose(t *testing.T) {
	p := validPlan()
	p.Standby.DistanceKm = 250 // below the 300km remote-site floor
	err := p.Validate()
	if err == nil {
		t.Fatal("standby 250km away accepted, want rejection (>300km, 00§4.5)")
	}
	if !strings.Contains(err.Error(), "300") {
		t.Fatalf("error should name the distance floor, got: %v", err)
	}
}

func TestValidateRejectsStandbyAtFloor(t *testing.T) {
	p := validPlan()
	p.Standby.DistanceKm = StandbyDistanceFloor // exactly 300 is not >300
	if err := p.Validate(); err == nil {
		t.Fatal("standby exactly 300km accepted, want rejection (must exceed 300km)")
	}
}

func TestValidateRejectsTwoPrimaries(t *testing.T) {
	p := validPlan()
	p.Standby.Role = RolePrimary
	if err := p.Validate(); err == nil {
		t.Fatal("standby with PRIMARY role accepted, want rejection")
	}
}

func TestValidateRejectsSameRegion(t *testing.T) {
	p := validPlan()
	p.Standby.Name = p.Primary.Name
	p.Standby.DistanceKm = 1200
	if err := p.Validate(); err == nil {
		t.Fatal("primary == standby accepted, want rejection")
	}
}

func TestValidateRejectsInvalidRegionName(t *testing.T) {
	p := validPlan()
	p.Standby.Name = "east" // not cn-north-1 style
	if err := p.Validate(); err == nil {
		t.Fatal("invalid region name accepted, want rejection")
	}
}

func TestValidateRejectsPrimaryDistance(t *testing.T) {
	p := validPlan()
	p.Primary.DistanceKm = 10 // primary is the origin; a nonzero distance is a config error
	if err := p.Validate(); err == nil {
		t.Fatal("primary with nonzero distance accepted, want rejection")
	}
}

func TestValidateRejectsNegativeRPO(t *testing.T) {
	p := validPlan()
	p.Channels = append(p.Channels, ReplicationChannel{Name: "broken", Mode: "async", RPO: -time.Second})
	if err := p.Validate(); err == nil {
		t.Fatal("negative RPO accepted, want rejection")
	}
}

func TestClassifyGlobalVsRegional(t *testing.T) {
	cases := []struct {
		service string
		want    ServiceScope
	}{
		{"svc-iam", ScopeGlobal},    // 账号体系 00§4.1
		{"svc-billing", ScopeGlobal}, // 计费中枢 09§5.2 M-8
		{"svc-payment", ScopeRegional},
		{"svc-catalog", ScopeRegional},
		{"svc-order", ScopeRegional},
		{"svc-metering", ScopeRegional},
		{"console-bff", ScopeRegional},
		{"rc-eci", ScopeRegional},
	}
	for _, c := range cases {
		got, err := Classify(c.service)
		if err != nil {
			t.Fatalf("Classify(%q) error: %v", c.service, err)
		}
		if got != c.want {
			t.Errorf("Classify(%q) = %q, want %q", c.service, got, c.want)
		}
	}
}

func TestClassifyRejectsUnknown(t *testing.T) {
	if _, err := Classify("svc-billng"); err == nil { // typo of svc-billing
		t.Fatal("unknown/mistyped service classified without error, want rejection (fail closed, not silent REGIONAL)")
	}
}

func TestIsGlobal(t *testing.T) {
	if !IsGlobal("svc-iam") || !IsGlobal("svc-billing") {
		t.Error("iam/billing must be global")
	}
	if IsGlobal("svc-catalog") || IsGlobal("svc-billng") {
		t.Error("regional or unknown service must not be global")
	}
}

func TestClassifyState(t *testing.T) {
	cases := []struct {
		state string
		want  StateClass
	}{
		{"account", StateShared},        // 唯一全局域 00§4.1
		{"ledger", StateReplicated},     // 账务 binlog 准实时 00§4.5
		{"ledger-binlog", StateReplicated},
		{"object-storage", StateReplicated}, // MinIO Replication 00§4.5
		{"kafka-topic", StateRebuilt},       // 不做跨城镜像 00§4.5
	}
	for _, c := range cases {
		got, err := ClassifyState(c.state)
		if err != nil {
			t.Fatalf("ClassifyState(%q) error: %v", c.state, err)
		}
		if got != c.want {
			t.Errorf("ClassifyState(%q) = %q, want %q", c.state, got, c.want)
		}
	}
}

func TestClassifyStateRejectsUnknown(t *testing.T) {
	if _, err := ClassifyState("metrics"); err == nil {
		t.Fatal("undeclared state classified without error, want rejection")
	}
}

func TestRoleWritable(t *testing.T) {
	if !RolePrimary.Writable() {
		t.Error("PRIMARY must be writable")
	}
	if RoleStandby.Writable() {
		t.Error("STANDBY must be read-only — P3 不承诺异地多活写 (00§4.5)")
	}
}

func TestReplicationChannelMeetsRPO(t *testing.T) {
	c := CoreLedgerChannel() // RPO = 5s
	if !c.MeetsRPO(3 * time.Second) {
		t.Error("3s lag should satisfy a 5s RPO")
	}
	if c.MeetsRPO(6 * time.Second) {
		t.Error("6s lag should violate a 5s RPO")
	}
	if !c.MeetsRPO(-time.Second) {
		t.Error("negative lag (replica ahead) should be treated as satisfied")
	}
}

func TestCoreLedgerRPOIsNearRealtimeNotZero(t *testing.T) {
	c := CoreLedgerChannel()
	if c.RPO <= 0 {
		t.Fatal("core ledger RPO must be positive; ≈0 is a near-realtime target, not an arithmetic zero")
	}
	if c.RPO > CoreLedgerRPO {
		t.Fatalf("core ledger RPO %v exceeds the declared near-realtime bound %v", c.RPO, CoreLedgerRPO)
	}
}

func TestMeetsRTO(t *testing.T) {
	if !MeetsRTO(29 * time.Minute) {
		t.Error("29min failover should satisfy RTO ≤ 30min")
	}
	if !MeetsRTO(MaxRTO) {
		t.Error("exactly 30min should satisfy RTO ≤ 30min (inclusive bound)")
	}
	if MeetsRTO(31 * time.Minute) {
		t.Error("31min failover should violate RTO ≤ 30min")
	}
}

func TestFailoverSteps(t *testing.T) {
	steps := validPlan().FailoverSteps()
	if len(steps) != 2 {
		t.Fatalf("want 2 failover steps (冷转热 + DNS 切换), got %d", len(steps))
	}
	if steps[0].Name != "control-plane-cold-to-hot" || steps[1].Name != "dns-cutover" {
		t.Fatalf("unexpected step names: %v, %v", steps[0].Name, steps[1].Name)
	}
}
