// Package slo implements the platform's SLO (service-level objective) model —
// the error-budget math, multi-window burn-rate alerting, and budget policy
// that phase 3 (09-roadmap §5.2 M-9) turns into a management-visible dashboard
// (08§10, 08§9.6 值班"黄金三板斧" input).
//
// The three-layer discipline (08§10.1):
//
//   - SLI is the measurement (e.g. non-5xx ratio over a rolling window).
//   - SLO is the target over the window (e.g. ≥ 99.95% over 30 days).
//   - SLA is a contractual promise, and is only offered for a service that has
//     GA'd AND met its SLO for two consecutive quarters — never for an
//     unproven one. CanCommitSLA encodes that gate.
//
// Error budget = 1 − SLO. A 99.95% availability target over a 30-day window
// admits 21.6 minutes of unavailability (08§10.3); alerting is designed around
// the budget's burn rate, not around raw thresholds, so a single blip does not
// page and a slow leak does not go unnoticed (08§10.1 "先定 SLO 再定告警").
package slo

import (
	"errors"
	"fmt"
	"time"
)

// Objective is a service-level objective: a target over a rolling window.
type Objective struct {
	// Name is the SLI's canonical name (e.g. gateway-availability).
	Name string
	// Target is the SLO as a fraction in (0, 1): 0.9995 = 99.95%.
	Target float64
	// Window is the rolling observation window (typically 30 days).
	Window time.Duration
}

// Validate rejects malformed objectives. A target outside (0,1) or a
// non-positive window is a config error that must fail at load — a 100% target
// has a zero budget, which means every blip freezes the domain, and that is
// never the operator's intent.
func (o Objective) Validate() error {
	if o.Name == "" {
		return errors.New("slo: objective name required")
	}
	if o.Target <= 0 || o.Target >= 1 {
		return fmt.Errorf("slo: target %v must be in (0,1) — a target of 1 has a zero error budget", o.Target)
	}
	if o.Window <= 0 {
		return errors.New("slo: window must be positive")
	}
	return nil
}

// ErrorBudget returns the total tolerable "bad" time over the window:
// (1 − target) × window (08§10.3). For 99.95% over 30 days this is ~21.6 min.
func (o Objective) ErrorBudget() time.Duration {
	return time.Duration((1 - o.Target) * float64(o.Window))
}

// BurnRate returns how fast the budget is being consumed over an observation
// window: consumed ÷ the budget pro-rated to that window. A burn rate of 1×
// means "consuming the budget at the steady rate that exhausts it exactly at
// window end"; 14.4× means the full budget is gone in ~1/14.4 of the window.
//
// The pro-ration is done in float64: budget (nanoseconds) × (window ÷ window)
// would overflow int64 for a 30-day budget scaled to a 1-hour window, and the
// integer truncation would turn every short-window observation into 0.
func (o Objective) BurnRate(consumed time.Duration, window time.Duration) float64 {
	if window <= 0 || o.Window <= 0 {
		return 0
	}
	proRata := float64(o.ErrorBudget()) * float64(window) / float64(o.Window)
	if proRata <= 0 {
		return 0
	}
	return float64(consumed) / proRata
}

// Burn rate alert thresholds (08§10.3 多窗口多燃烧率):
//
//   - Fast burn: a 1h window at ≥14.4× pages immediately (phone-level) — the
//     budget is vanishing fast enough to matter this on-call cycle.
//   - Slow burn: a 3d window at ≥1× is ticket-level — a sustained leak handled
//     within the iteration, not at 3am.
const (
	FastBurnWindow    = time.Hour
	FastBurnThreshold = 14.4
	SlowBurnWindow    = 72 * time.Hour
	SlowBurnThreshold = 1.0
)

// AlertSeverity is the escalation level a burn rate produces.
type AlertSeverity string

const (
	// SeverityPage is phone-level: act now.
	SeverityPage AlertSeverity = "PAGE"
	// SeverityTicket is work-queue-level: act within the iteration.
	SeverityTicket AlertSeverity = "TICKET"
	// SeverityNone is below every threshold.
	SeverityNone AlertSeverity = "NONE"
)

// Alert returns the burn-rate alert for a consumption observation. It applies
// the fast-burn rule first (a fast burn that is also a slow burn pages, not
// tickets) — the more urgent window wins.
func (o Objective) Alert(consumed time.Duration, window time.Duration) AlertSeverity {
	if o.BurnRate(consumed, window) >= FastBurnThreshold && window <= FastBurnWindow {
		return SeverityPage
	}
	if o.BurnRate(consumed, window) >= SlowBurnThreshold && window >= SlowBurnWindow {
		return SeverityTicket
	}
	return SeverityNone
}

// BudgetPolicy is the release-policy consequence of remaining budget
// (08§10.3 预算政策).
type BudgetPolicy string

const (
	// PolicyNormal: publish as usual.
	PolicyNormal BudgetPolicy = "NORMAL"
	// PolicySlowDown: fewer than half the budget remains — release frequency
	// drops to weekly and the canary observation window doubles.
	PolicySlowDown BudgetPolicy = "SLOW_DOWN"
	// PolicyFreeze: budget exhausted — all non-reliability feature releases for
	// the domain are frozen until the budget recovers.
	PolicyFreeze BudgetPolicy = "FREEZE"
)

// BudgetPolicy maps remaining budget to the release policy. The thresholds are
// the 08§10.3 numbers: <50% remaining slows down; 0 (or less) freezes.
func (o Objective) BudgetPolicy(remaining time.Duration) BudgetPolicy {
	budget := o.ErrorBudget()
	if remaining <= 0 {
		return PolicyFreeze
	}
	if remaining < budget/2 {
		return PolicySlowDown
	}
	return PolicyNormal
}

// CanCommitSLA encodes the SLA gate (08§10.1): a service may only promise an
// SLA after it has met its SLO for two consecutive quarters.
func CanCommitSLA(consecutiveQuartersMet int) bool { return consecutiveQuartersMet >= 2 }

// PlatformObjectives is the first-year platform SLO seed table (08§10.2).
// These are targets, not SLAs: the SLA gate (CanCommitSLA) applies on top.
var PlatformObjectives = []Objective{
	{Name: "openapi-gateway-availability", Target: 0.9995, Window: 30 * 24 * time.Hour},
	{Name: "resource-control-plane-success", Target: 0.999, Window: 30 * 24 * time.Hour},
	{Name: "metering-no-loss", Target: 0.9999, Window: 30 * 24 * time.Hour},
	{Name: "billing-on-time", Target: 0.995, Window: 30 * 24 * time.Hour},
	{Name: "login-auth-success", Target: 0.9995, Window: 30 * 24 * time.Hour},
}
