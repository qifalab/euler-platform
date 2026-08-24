package autoscaling

import (
	"testing"
	"time"
)

var fixNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// When currentReplicas has drifted outside [Min,Max], the decision must move
// toward the bounds — the reported Action reflects the real direction, never
// the rule's nominal one.
func TestOutOfBoundsCurrentIsClampedAndDirectionCorrected(t *testing.T) {
	policy := ScalingPolicy{
		MinReplicas: 1, MaxReplicas: 10, DesiredReplicas: 5,
		Rules: []ScalingRule{
			{MetricName: "cpu", Threshold: 0.8, Operator: OpGreaterThan, Action: ActionScaleUp, Step: 2},
		},
	}
	// current=100 > Max=10: a scale_up rule fires, but the only sane move is DOWN to 10.
	d, err := Evaluate(policy, 100, map[string]float64{"cpu": 0.9}, time.Time{}, fixNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionScaleDown || d.TargetReplicas != 10 {
		t.Fatalf("current above Max: got %+v, want scale_down to 10", d)
	}

	// current=0 < Min=1 with a scale_down rule: the move is UP to Min.
	down := ScalingPolicy{
		MinReplicas: 2, MaxReplicas: 10, DesiredReplicas: 5,
		Rules: []ScalingRule{
			{MetricName: "mem", Threshold: 0.5, Operator: OpLessThan, Action: ActionScaleDown, Step: 1},
		},
	}
	d2, err := Evaluate(down, 0, map[string]float64{"mem": 0.1}, time.Time{}, fixNow)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Action != ActionScaleUp || d2.TargetReplicas != 2 {
		// clamp 0 → 2, scale_down by 1 → 1, re-clamped to Min=2; 2 > current 0 → up.
		t.Fatalf("current below Min: got %+v, want scale_up to 2", d2)
	}
}

// An at_cap scale-up rule must not mask a later scale-down rule that can act —
// otherwise scale-down starves forever while the group sits at Max.
func TestAtCapDoesNotStarveLaterRules(t *testing.T) {
	policy := ScalingPolicy{
		MinReplicas: 1, MaxReplicas: 10, DesiredReplicas: 10,
		Rules: []ScalingRule{
			{MetricName: "cpu", Threshold: 0.8, Operator: OpGreaterThan, Action: ActionScaleUp, Step: 1},
			{MetricName: "mem", Threshold: 0.2, Operator: OpLessThan, Action: ActionScaleDown, Step: 3},
		},
	}
	// At Max, both rules fire: scale_up is capped, scale_down must still run.
	d, err := Evaluate(policy, 10, map[string]float64{"cpu": 0.95, "mem": 0.1}, time.Time{}, fixNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionScaleDown || d.TargetReplicas != 7 || d.RuleIndex != 1 {
		t.Fatalf("at_cap must not mask the scale-down rule: %+v", d)
	}

	// If ONLY the capped rule fires, the at_cap decision is still reported.
	d2, err := Evaluate(policy, 10, map[string]float64{"cpu": 0.95, "mem": 0.5}, time.Time{}, fixNow)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Action != ActionNone || d2.Reason != "at_cap" || d2.RuleIndex != 0 {
		t.Fatalf("capped-only evaluation should report at_cap: %+v", d2)
	}
}
