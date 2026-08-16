package autoscaling

import (
	"errors"
	"testing"
	"time"
)

var (
	evalNow = time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)
)

// a sound policy used across the happy-path tests: scale up on high cpu,
// scale down on low mem, room to grow/shrink within [1,10].
func soundPolicy() ScalingPolicy {
	return ScalingPolicy{
		MinReplicas: 1, MaxReplicas: 10, DesiredReplicas: 3, CooldownSeconds: 60,
		Rules: []ScalingRule{
			{MetricName: "cpu", Threshold: 0.8, Operator: OpGreaterThan, Action: ActionScaleUp, Step: 1},
			{MetricName: "mem", Threshold: 0.2, Operator: OpLessThan, Action: ActionScaleDown, Step: 1},
		},
	}
}

func TestScaleUpWhenCpuAboveThreshold(t *testing.T) {
	// cpu=0.9 > 0.8 → scale_up +1, current 3, max 10 → target 4.
	d, err := Evaluate(soundPolicy(), 3, map[string]float64{"cpu": 0.9, "mem": 0.5}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionScaleUp {
		t.Fatalf("action = %s, want scale_up", d.Action)
	}
	if d.TargetReplicas != 4 {
		t.Fatalf("target = %d, want 4", d.TargetReplicas)
	}
	if d.RuleIndex != 0 {
		t.Fatalf("rule index = %d, want 0 (first rule)", d.RuleIndex)
	}
}

func TestScaleDownWhenMemBelowThreshold(t *testing.T) {
	// mem=0.1 < 0.2, cpu not high → scale_down -1, current 5, min 1 → target 4.
	d, err := Evaluate(soundPolicy(), 5, map[string]float64{"cpu": 0.3, "mem": 0.1}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionScaleDown {
		t.Fatalf("action = %s, want scale_down", d.Action)
	}
	if d.TargetReplicas != 4 {
		t.Fatalf("target = %d, want 4", d.TargetReplicas)
	}
}

func TestCooldownBlocksImmediateRescale(t *testing.T) {
	// Last action 30s ago, cooldown 60s → no-op "cooldown" even though the
	// rule would otherwise fire. This is the anti-flapping guarantee.
	lastAction := evalNow.Add(-30 * time.Second)
	d, err := Evaluate(soundPolicy(), 3, map[string]float64{"cpu": 0.95}, lastAction, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionNone {
		t.Fatalf("cooldown action = %s, want none", d.Action)
	}
	if d.Reason != "cooldown" {
		t.Fatalf("reason = %q, want \"cooldown\"", d.Reason)
	}
}

func TestCooldownExpiredAllowsAction(t *testing.T) {
	// 90s ago > 60s cooldown → the rule may fire again.
	lastAction := evalNow.Add(-90 * time.Second)
	d, err := Evaluate(soundPolicy(), 3, map[string]float64{"cpu": 0.95}, lastAction, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionScaleUp {
		t.Fatalf("action = %s, want scale_up (cooldown elapsed)", d.Action)
	}
}

func TestFirstEvaluationNotBlockedByZeroLastAction(t *testing.T) {
	// lastActionAt zero-value = never scaled → the very first evaluation is not
	// in cooldown, so a matching rule acts immediately.
	d, err := Evaluate(soundPolicy(), 3, map[string]float64{"cpu": 0.95}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionScaleUp {
		t.Fatalf("first-eval action = %s, want scale_up", d.Action)
	}
}

func TestRespectsMaxCap(t *testing.T) {
	// current 10, scale_up step 2, max 10 → clamped to 10 == current → at_cap.
	d, err := Evaluate(soundPolicy(), 10, map[string]float64{"cpu": 0.95}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionNone {
		t.Fatalf("action = %s, want none (at max)", d.Action)
	}
	if d.Reason != "at_cap" {
		t.Fatalf("reason = %q, want \"at_cap\"", d.Reason)
	}
	if d.TargetReplicas != 10 {
		t.Fatalf("target = %d, want 10 (clamped to max)", d.TargetReplicas)
	}
}

func TestRespectsMinFloor(t *testing.T) {
	// current 1, scale_down step 1, min 1 → clamped to 1 == current → at_cap.
	d, err := Evaluate(soundPolicy(), 1, map[string]float64{"mem": 0.05}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionNone {
		t.Fatalf("action = %s, want none (at min)", d.Action)
	}
	if d.Reason != "at_cap" {
		t.Fatalf("reason = %q, want \"at_cap\"", d.Reason)
	}
	if d.TargetReplicas != 1 {
		t.Fatalf("target = %d, want 1 (clamped to min)", d.TargetReplicas)
	}
}

func TestMissingMetricIsNoOpNoPanic(t *testing.T) {
	// The rule wants "cpu" but the snapshot carries only "mem" → rule does not
	// match, and the mem rule does not fire (mem not below threshold). No-op.
	d, err := Evaluate(soundPolicy(), 3, map[string]float64{"mem": 0.5}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionNone {
		t.Fatalf("action = %s, want none (missing metric)", d.Action)
	}
	if d.RuleIndex != -1 {
		t.Fatalf("rule index = %d, want -1 (no rule matched)", d.RuleIndex)
	}
}

func TestDefaultDenyEmptyRules(t *testing.T) {
	// No rules → nothing can match → no-op. Default-deny.
	p := ScalingPolicy{MinReplicas: 1, MaxReplicas: 10, DesiredReplicas: 3, CooldownSeconds: 60}
	d, err := Evaluate(p, 3, map[string]float64{"cpu": 0.99}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionNone {
		t.Fatalf("action = %s, want none (empty rules)", d.Action)
	}
	if d.Reason != "no_rule_matched" {
		t.Fatalf("reason = %q, want \"no_rule_matched\"", d.Reason)
	}
}

func TestFirstMatchingRuleWins(t *testing.T) {
	// Two rules both match (cpu>0.8 and a second cpu>0.1 scale_up) → the first
	// decides. Order independence of equal-effect rules is fine; the point is
	// we don't combine steps (no +2 from two rules).
	p := ScalingPolicy{
		MinReplicas: 1, MaxReplicas: 10, DesiredReplicas: 3, CooldownSeconds: 60,
		Rules: []ScalingRule{
			{MetricName: "cpu", Threshold: 0.8, Operator: OpGreaterThan, Action: ActionScaleUp, Step: 1},
			{MetricName: "cpu", Threshold: 0.1, Operator: OpGreaterThan, Action: ActionScaleUp, Step: 3},
		},
	}
	d, err := Evaluate(p, 3, map[string]float64{"cpu": 0.9}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionScaleUp {
		t.Fatalf("action = %s, want scale_up", d.Action)
	}
	if d.TargetReplicas != 4 {
		t.Fatalf("target = %d, want 4 (first rule step=1, not combined)", d.TargetReplicas)
	}
	if d.RuleIndex != 0 {
		t.Fatalf("rule index = %d, want 0 (first match wins)", d.RuleIndex)
	}
}

func TestInvalidPolicyRejected(t *testing.T) {
	cases := []struct {
		name  string
		mutate func(*ScalingPolicy)
	}{
		{"min>max", func(p *ScalingPolicy) { p.MinReplicas = 10; p.MaxReplicas = 5; p.DesiredReplicas = 5 }},
		{"desired below min", func(p *ScalingPolicy) { p.MinReplicas = 4; p.DesiredReplicas = 2; p.MaxReplicas = 10 }},
		{"desired above max", func(p *ScalingPolicy) { p.DesiredReplicas = 99; p.MinReplicas = 1; p.MaxReplicas = 10 }},
		{"step zero", func(p *ScalingPolicy) { p.Rules = []ScalingRule{{MetricName: "cpu", Threshold: 0.8, Operator: OpGreaterThan, Action: ActionScaleUp, Step: 0}} }},
		{"step negative", func(p *ScalingPolicy) { p.Rules = []ScalingRule{{MetricName: "cpu", Threshold: 0.8, Operator: OpGreaterThan, Action: ActionScaleUp, Step: -1}} }},
		{"empty metric name", func(p *ScalingPolicy) { p.Rules = []ScalingRule{{MetricName: "", Threshold: 0.8, Operator: OpGreaterThan, Action: ActionScaleUp, Step: 1}} }},
		{"bad action", func(p *ScalingPolicy) { p.Rules = []ScalingRule{{MetricName: "cpu", Threshold: 0.8, Operator: OpGreaterThan, Action: Action("bogus"), Step: 1}} }},
		{"bad operator", func(p *ScalingPolicy) { p.Rules = []ScalingRule{{MetricName: "cpu", Threshold: 0.8, Operator: Operator("="), Action: ActionScaleUp, Step: 1}} }},
		{"cooldown negative", func(p *ScalingPolicy) { p.CooldownSeconds = -1; p.Rules = nil }},
		{"min negative", func(p *ScalingPolicy) { p.MinReplicas = -1; p.DesiredReplicas = 0; p.Rules = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ScalingPolicy{MinReplicas: 1, MaxReplicas: 10, DesiredReplicas: 3, CooldownSeconds: 60}
			c.mutate(&p)
			_, err := Evaluate(p, 3, map[string]float64{"cpu": 0.9}, time.Time{}, evalNow)
			if err == nil {
				t.Fatalf("invalid policy %q accepted", c.name)
			}
			if !errors.Is(err, ErrInvalidPolicy) {
				t.Fatalf("error = %v, want wrapped ErrInvalidPolicy", err)
			}
		})
	}
	// A sound policy validates clean.
	if err := soundPolicy().Validate(); err != nil {
		t.Fatalf("sound policy rejected: %v", err)
	}
}

func TestRuleDoesNotMatchWhenComparisonFails(t *testing.T) {
	// cpu=0.5 is NOT > 0.8, and mem=0.5 is NOT < 0.2 → neither fires → no-op.
	d, err := Evaluate(soundPolicy(), 3, map[string]float64{"cpu": 0.5, "mem": 0.5}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionNone {
		t.Fatalf("action = %s, want none (no comparison holds)", d.Action)
	}
}

func TestScaleUpClampsToMaxWhenStepExceedsHeadroom(t *testing.T) {
	// current 9, scale_up step 5, max 10 → clamped to 10 (not 14).
	p := ScalingPolicy{
		MinReplicas: 1, MaxReplicas: 10, DesiredReplicas: 3, CooldownSeconds: 60,
		Rules: []ScalingRule{{MetricName: "cpu", Threshold: 0.8, Operator: OpGreaterThan, Action: ActionScaleUp, Step: 5}},
	}
	d, err := Evaluate(p, 9, map[string]float64{"cpu": 0.99}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionScaleUp {
		t.Fatalf("action = %s, want scale_up", d.Action)
	}
	if d.TargetReplicas != 10 {
		t.Fatalf("target = %d, want 10 (clamped to max)", d.TargetReplicas)
	}
}

func TestScaleDownClampsToMinWhenStepExceedsFloor(t *testing.T) {
	// current 2, scale_down step 5, min 1 → clamped to 1 (not -3).
	p := ScalingPolicy{
		MinReplicas: 1, MaxReplicas: 10, DesiredReplicas: 3, CooldownSeconds: 60,
		Rules: []ScalingRule{{MetricName: "mem", Threshold: 0.2, Operator: OpLessThan, Action: ActionScaleDown, Step: 5}},
	}
	d, err := Evaluate(p, 2, map[string]float64{"mem": 0.05}, time.Time{}, evalNow)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Action != ActionScaleDown {
		t.Fatalf("action = %s, want scale_down", d.Action)
	}
	if d.TargetReplicas != 1 {
		t.Fatalf("target = %d, want 1 (clamped to min)", d.TargetReplicas)
	}
}
