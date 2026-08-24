package release

import (
	"testing"
	"time"
)

func TestCanaryProgressionContract(t *testing.T) {
	if err := ValidateCanarySteps(CanarySteps); err != nil {
		t.Fatalf("canonical 5→20→50→100 rejected: %v", err)
	}
}

func TestValidateCanaryStepsRejectsRegression(t *testing.T) {
	for _, steps := range [][]int{
		{20, 5, 50, 100}, // first step not 5
		{5, 20, 50},      // doesn't end at 100
		{5, 20, 20, 100}, // non-increasing
		{5, 20, 100, 50}, // regresses
		{5},              // too short
	} {
		if err := ValidateCanarySteps(steps); err == nil {
			t.Errorf("malformed progression %v accepted, want rejection", steps)
		}
	}
}

func TestPassCanaryGateRequiresBoth(t *testing.T) {
	if !PassCanaryGate(0.996, 1400*time.Millisecond) {
		t.Error("passing metrics should pass the gate")
	}
	if PassCanaryGate(0.99, 100*time.Millisecond) {
		t.Error("success rate below 0.995 must fail the gate even with good latency")
	}
	if PassCanaryGate(1.0, 1600*time.Millisecond) {
		t.Error("p99 above 1.5s must fail the gate even with perfect success")
	}
}

func TestExpandContractObservationWindow(t *testing.T) {
	if PassExpandContractGate(6 * 24 * time.Hour) {
		t.Error("6-day observation must not clear the ≥7-day expand-contract gate")
	}
	if !PassExpandContractGate(7 * 24 * time.Hour) {
		t.Error("7-day observation must clear the expand-contract gate")
	}
}

func TestPreferredRollbackIsRevert(t *testing.T) {
	if PreferredRollback() != RollbackRevert {
		t.Fatal("first-choice rollback must be git revert (08§4.5)")
	}
}
