package autoscaling

import (
	"testing"
	"time"
)

// A group outside [Min,Max] is pulled back into bounds even when no rule fires
// (no metrics at all, or quiet ones) and even inside the cooldown window —
// before, it was only corrected as a side effect of a firing rule.
func TestOutOfBoundsGroupIsCorrectedWithoutARule(t *testing.T) {
	policy := ScalingPolicy{
		MinReplicas: 2, MaxReplicas: 6, DesiredReplicas: 3, CooldownSeconds: 300,
		Rules: []ScalingRule{
			{MetricName: "cpu", Threshold: 0.8, Operator: OpGreaterThan, Action: ActionScaleUp, Step: 1},
		},
	}
	recent := fixNow.Add(-10 * time.Second) // cooldown open
	for _, c := range []struct {
		name       string
		current    int
		metrics    map[string]float64
		last       time.Time
		wantAction Action
		wantTarget int
	}{
		{"below min, no metrics", 0, nil, time.Time{}, ActionScaleUp, 2},
		{"below min, quiet metric", 1, map[string]float64{"cpu": 0.1}, time.Time{}, ActionScaleUp, 2},
		{"above max, no metrics", 9, nil, time.Time{}, ActionScaleDown, 6},
		{"above max, in cooldown", 9, map[string]float64{"cpu": 0.1}, recent, ActionScaleDown, 6},
		{"below min, rule says up", 1, map[string]float64{"cpu": 0.95}, time.Time{}, ActionScaleUp, 2},
	} {
		d, err := Evaluate(policy, c.current, c.metrics, c.last, fixNow)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != c.wantAction || d.TargetReplicas != c.wantTarget || d.RuleIndex != -1 || d.Reason != "bounds_correction" {
			t.Errorf("%s: got %+v, want %s to %d (bounds_correction)", c.name, d, c.wantAction, c.wantTarget)
		}
	}

	// In bounds, nothing changes: quiet metrics are still a no-op.
	d, err := Evaluate(policy, 3, map[string]float64{"cpu": 0.1}, time.Time{}, fixNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionNone || d.Reason != "no_rule_matched" {
		t.Fatalf("in-bounds quiet group: %+v", d)
	}
}
