// Package autoscaling implements the scaling-policy decision engine for the
// 弹性伸缩 product (scas, 09-roadmap §4.3 M-7.3, 06-kubernetes-productization.md
// §2.6).
//
// # Policy layer, not the execution surface
//
// 06§2.6 splits autoscaling into two halves:
//   - the EXECUTION surface — HPA / VPA / Cluster-Autoscaler — which actually
//     adds/removes pods or nodes;
//   - the POLICY layer — which decides WHAT to do (scale up? down? by how
//     much? now or wait?).
//
// This package is the policy layer. It takes a ScalingPolicy, the current
// replica count, a metrics snapshot and the time of the last scaling action,
// and returns a Decision. It does not touch the cluster — that is the
// executor's job, wired through the rc-autoscaling controller. Keeping the
// decision pure (no I/O) is what makes it unit-testable without a cluster and
// is what makes the 06§6.3 MockDriver 验收门槛 runnable.
//
// # Deny-first
//
// The default Decision is a no-op (Action="none"). A scaling action happens
// only when a rule explicitly matches: a metric present in the snapshot AND
// its comparison holds. A missing metric never matches — scaling on unknown
// state is how a sensor gap turns into a runaway cluster. This is the same
// fail-safe posture as pkg-go/authz's Deny-first policy.
//
// # Cooldown prevents flapping
//
// A metric spike must not trigger five scales in a minute. Every action carries
// a cooldown window; while it is open, Evaluate returns a no-op "cooldown"
// decision regardless of what the metrics say. The cooldown is per-evaluation,
// not per-rule: any recent action suppresses all new actions, because the
// point is to let the system settle before re-evaluating.
package autoscaling

import (
	"errors"
	"fmt"
	"time"
)

// Action is what Evaluate decided to do.
type Action string

const (
	ActionScaleUp   Action = "scale_up"
	ActionScaleDown Action = "scale_down"
	ActionNone      Action = "none"
)

// Operator is the comparison a rule applies between the metric value and its
// threshold.
type Operator string

const (
	OpGreaterThan Operator = ">"
	OpLessThan    Operator = "<"
)

// ScalingRule is one trigger in a policy. A rule fires when its metric is
// present in the evaluation's metrics map AND the comparison holds; the first
// matching rule wins (rules do not combine).
type ScalingRule struct {
	MetricName string  // "cpu", "mem", ...; must be present in metrics to match
	Threshold  float64 // the value to compare against
	Operator   Operator
	Action     Action // scale_up | scale_down
	Step       int    // replicas to add (scale_up) or remove (scale_down)
}

// ScalingPolicy is the declarative desired state of a scaling group: the
// replica bounds, the rules, and the cooldown that suppresses flapping.
type ScalingPolicy struct {
	MinReplicas     int
	MaxReplicas     int
	DesiredReplicas int
	Rules           []ScalingRule
	CooldownSeconds int
}

// Decision is the outcome of one Evaluate call. It is the only thing the
// policy layer emits; the executor turns a non-"none" Decision into a cluster
// mutation (and updates lastActionAt).
type Decision struct {
	Action         Action
	TargetReplicas int  // the replica count the decision aims for; 0 for "none"
	RuleIndex      int  // which rule fired; -1 for "none"
	Reason         string
}

// Errors. Validate returns a wrapped ErrInvalidPolicy so callers can branch on
// policy shape versus runtime state.
var (
	ErrInvalidPolicy = errors.New("autoscaling: invalid scaling policy")
	ErrUnknownMetric  = errors.New("autoscaling: rule references an unknown metric")
)

// Validate checks the policy shape. Rejecting here keeps a malformed policy
// from ever reaching Evaluate, so Evaluate can assume a sound policy and stay
// focused on the decision.
func (p ScalingPolicy) Validate() error {
	if p.MinReplicas < 0 {
		return fmt.Errorf("%w: MinReplicas %d must be >= 0", ErrInvalidPolicy, p.MinReplicas)
	}
	if p.MaxReplicas < p.MinReplicas {
		return fmt.Errorf("%w: MaxReplicas %d < MinReplicas %d", ErrInvalidPolicy, p.MaxReplicas, p.MinReplicas)
	}
	if p.DesiredReplicas < p.MinReplicas || p.DesiredReplicas > p.MaxReplicas {
		return fmt.Errorf("%w: DesiredReplicas %d outside [%d,%d]", ErrInvalidPolicy, p.DesiredReplicas, p.MinReplicas, p.MaxReplicas)
	}
	if p.CooldownSeconds < 0 {
		return fmt.Errorf("%w: CooldownSeconds %d must be >= 0", ErrInvalidPolicy, p.CooldownSeconds)
	}
	for i, r := range p.Rules {
		if r.Step <= 0 {
			return fmt.Errorf("%w: rule[%d] Step %d must be > 0", ErrInvalidPolicy, i, r.Step)
		}
		if r.MetricName == "" {
			return fmt.Errorf("%w: rule[%d] MetricName is required", ErrInvalidPolicy, i)
		}
		if r.Action != ActionScaleUp && r.Action != ActionScaleDown {
			return fmt.Errorf("%w: rule[%d] Action %q must be scale_up or scale_down", ErrInvalidPolicy, i, r.Action)
		}
		if r.Operator != OpGreaterThan && r.Operator != OpLessThan {
			return fmt.Errorf("%w: rule[%d] Operator %q must be < or >", ErrInvalidPolicy, i, r.Operator)
		}
	}
	return nil
}

// matches reports whether the rule's comparison holds for the given value.
func (r ScalingRule) matches(value float64) bool {
	switch r.Operator {
	case OpGreaterThan:
		return value > r.Threshold
	case OpLessThan:
		return value < r.Threshold
	}
	return false
}

// Evaluate decides what to do given the policy, current replicas, a metrics
// snapshot and the time of the last scaling action. It is pure (no I/O), so it
// is unit-testable and re-runnable.
//
// The decision order is:
//  1. Cooldown: any action within CooldownSeconds of lastActionAt is suppressed.
//  2. First matching rule wins; a rule matches only if its metric is present.
//  3. The target is clamped to [Min,Max]; a clamped-equal target is "at_cap".
//  4. Nothing matches → no-op (default-deny).
func Evaluate(policy ScalingPolicy, currentReplicas int, metrics map[string]float64, lastActionAt, now time.Time) (Decision, error) {
	if err := policy.Validate(); err != nil {
		return Decision{}, err
	}

	// Cooldown suppresses any action while open. lastActionAt zero-value means
	// "never scaled", which is not a cooldown — the very first evaluation may
	// act. This is what lets a freshly-provisioned group react immediately.
	if !lastActionAt.IsZero() && policy.CooldownSeconds > 0 {
		elapsed := now.Sub(lastActionAt)
		if elapsed < time.Duration(policy.CooldownSeconds)*time.Second {
			return Decision{Action: ActionNone, RuleIndex: -1, Reason: "cooldown"}, nil
		}
	}

	for i, rule := range policy.Rules {
		value, present := metrics[rule.MetricName]
		if !present {
			continue // fail safe: never scale on an unknown metric
		}
		if !rule.matches(value) {
			continue
		}

		// First matching rule wins.
		var target int
		switch rule.Action {
		case ActionScaleUp:
			target = currentReplicas + rule.Step
			if target > policy.MaxReplicas {
				target = policy.MaxReplicas
			}
		case ActionScaleDown:
			target = currentReplicas - rule.Step
			if target < policy.MinReplicas {
				target = policy.MinReplicas
			}
		}

		// If the rule fired but the target is already at the bound, there is
		// nothing to do — reporting "at_cap" keeps the executor from no-oping
		// repeatedly and lets monitoring distinguish "scaled" from "capped".
		if target == currentReplicas {
			return Decision{Action: ActionNone, TargetReplicas: target, RuleIndex: i, Reason: "at_cap"}, nil
		}
		return Decision{Action: rule.Action, TargetReplicas: target, RuleIndex: i, Reason: fmt.Sprintf("rule[%d] %s%s %v", i, rule.MetricName, rule.Operator, rule.Threshold)}, nil
	}

	// Default-deny: no rule matched, so do nothing.
	return Decision{Action: ActionNone, RuleIndex: -1, Reason: "no_rule_matched"}, nil
}
