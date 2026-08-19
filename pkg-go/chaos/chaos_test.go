package chaos

import (
	"strings"
	"testing"
	"time"
)

func TestMandatoryDrillsAreSix(t *testing.T) {
	if len(MandatoryDrills) != 6 {
		t.Fatalf("want 6 mandatory drills (08§9.5), got %d", len(MandatoryDrills))
	}
	seen := make(map[DrillKind]bool, 6)
	for _, k := range MandatoryDrills {
		if seen[k] {
			t.Fatalf("duplicate drill kind %q", k)
		}
		seen[k] = true
	}
}

func TestMandatoryDrillsAllValid(t *testing.T) {
	for _, k := range MandatoryDrills {
		if !k.Valid() {
			t.Errorf("mandatory drill %q reports invalid", k)
		}
	}
}

func TestValidRejectsUnknownKind(t *testing.T) {
	if DrillKind("format-c-drive").Valid() {
		t.Fatal("free-form drill kind accepted, want rejection")
	}
}

func TestStagingDrillNeedsNoAbortDeadline(t *testing.T) {
	d := DrillPlan{Kind: DrillKafkaBrokerLoss, Stage: StageStaging, AbortDeadline: 0}
	if err := d.Validate(); err != nil {
		t.Fatalf("staging drill without abort deadline rejected: %v", err)
	}
}

func TestProdDrillRequiresAbortDeadline(t *testing.T) {
	d := DrillPlan{Kind: DrillKafkaBrokerLoss, Stage: StageProd, BlastRadius: "one broker"}
	if err := d.Validate(); err == nil {
		t.Fatal("prod drill with no abort deadline accepted, want rejection")
	}
}

func TestProdDrillAbortDeadlineBounded(t *testing.T) {
	d := DrillPlan{Kind: DrillKafkaBrokerLoss, Stage: StageProd, BlastRadius: "one broker", AbortDeadline: 11 * time.Minute}
	if err := d.Validate(); err == nil {
		t.Fatal("prod drill with 11min abort accepted, want rejection (≤10min, 08§9.5)")
	}
}

func TestProdDrillRequiresBlastRadius(t *testing.T) {
	d := DrillPlan{Kind: DrillKafkaBrokerLoss, Stage: StageProd, AbortDeadline: 5 * time.Minute}
	if err := d.Validate(); err == nil {
		t.Fatal("prod drill with no blast radius accepted, want rejection")
	}
}

func TestProdDrillValid(t *testing.T) {
	d := DrillPlan{Kind: DrillSingleAZLoss, Stage: StageProd, BlastRadius: "the lighter MySQL AZ", AbortDeadline: 5 * time.Minute}
	if err := d.Validate(); err != nil {
		t.Fatalf("valid prod drill rejected: %v", err)
	}
}

func TestValidateRejectsUnknownStage(t *testing.T) {
	d := DrillPlan{Kind: DrillRedisFailover, Stage: Stage("qa")}
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "stage") {
		t.Fatalf("unknown stage not rejected clearly: %v", err)
	}
}

func TestNeedsRemediationBoundary(t *testing.T) {
	r := DrillResult{Kind: DrillMySQLFailover, Expected: 10 * time.Minute}
	r.Actual = 15 * time.Minute // exactly 150% — NOT >50% deviation
	if r.NeedsRemediation() {
		t.Error("exactly 150% must not trip remediation (threshold is >50%, strict)")
	}
	r.Actual = 15*time.Minute + time.Second
	if !r.NeedsRemediation() {
		t.Error(">150% must trip remediation")
	}
	r.Actual = 9 * time.Minute // faster than expected
	if r.NeedsRemediation() {
		t.Error("faster-than-expected must not trip remediation")
	}
}
