package slo

import (
	"math"
	"testing"
	"time"
)

// gateway99_95 is the 08§10.2 example objective.
func gateway99_95() Objective {
	return Objective{Name: "openapi-gateway-availability", Target: 0.9995, Window: 30 * 24 * time.Hour}
}

func TestErrorBudgetMatchesBookExample(t *testing.T) {
	// 08§10.3: 99.95% availability over 30 days ≈ 21.6 minutes of unavailability.
	budget := gateway99_95().ErrorBudget()
	expected := time.Duration((1 - 0.9995) * float64(30*24*time.Hour))
	diff := budget - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Second {
		t.Fatalf("budget = %v, want ~%v (21.6 min)", budget, expected)
	}
	if budget != 21*time.Minute+36*time.Second {
		t.Logf("budget = %v (21m36s = 21.6 min)", budget)
	}
}

func TestValidateRejectsPerfectTarget(t *testing.T) {
	o := Objective{Name: "x", Target: 1.0, Window: time.Hour} // zero budget
	if err := o.Validate(); err == nil {
		t.Fatal("100% target accepted, want rejection (zero error budget)")
	}
}

func TestValidateRejectsZeroTarget(t *testing.T) {
	o := Objective{Name: "x", Target: 0, Window: time.Hour}
	if err := o.Validate(); err == nil {
		t.Fatal("0% target accepted, want rejection")
	}
}

func TestValidateRejectsNegativeWindow(t *testing.T) {
	o := Objective{Name: "x", Target: 0.999, Window: -time.Hour}
	if err := o.Validate(); err == nil {
		t.Fatal("negative window accepted, want rejection")
	}
}

func TestBurnRateOneExhaustsBudgetExactly(t *testing.T) {
	o := gateway99_95()
	// Consuming the pro-rata budget over a 1h window is a burn rate of exactly 1.
	proRata := time.Duration(float64(o.ErrorBudget()) * float64(time.Hour) / float64(o.Window))
	if got := o.BurnRate(proRata, time.Hour); math.Abs(got-1.0) > 1e-6 {
		t.Fatalf("burn rate = %v, want 1.0", got)
	}
}

func TestFastBurnPages(t *testing.T) {
	o := gateway99_95()
	proRata := time.Duration(float64(o.ErrorBudget()) * float64(FastBurnWindow) / float64(o.Window))
	// 15× is clearly above the 14.4× threshold. (Testing exactly AT the threshold
	// is brittle: 0.9995 is not exactly representable in float64, so the
	// pro-rata budget truncates by a few nanoseconds and the burn rate lands a
	// hair under 14.4.)
	consumed := time.Duration(float64(proRata) * 15)
	if got := o.Alert(consumed, FastBurnWindow); got != SeverityPage {
		t.Fatalf("fast burn (15x over 1h) = %v, want PAGE", got)
	}
}

func TestSlowBurnTickets(t *testing.T) {
	o := gateway99_95()
	proRata := time.Duration(float64(o.ErrorBudget()) * float64(SlowBurnWindow) / float64(o.Window))
	consumed := time.Duration(float64(proRata) * 2) // 2×, clearly above the 1× threshold
	if got := o.Alert(consumed, SlowBurnWindow); got != SeverityTicket {
		t.Fatalf("slow burn (2x over 3d) = %v, want TICKET", got)
	}
}

func TestSubThresholdIsNone(t *testing.T) {
	o := gateway99_95()
	if got := o.Alert(0, FastBurnWindow); got != SeverityNone {
		t.Fatalf("zero consumption = %v, want NONE", got)
	}
}

func TestBudgetPolicyThresholds(t *testing.T) {
	o := gateway99_95()
	budget := o.ErrorBudget()
	cases := []struct {
		remaining time.Duration
		want      BudgetPolicy
	}{
		{budget, PolicyNormal},
		{budget / 2, PolicyNormal},     // exactly 50% is NOT <50% (08§10.3)
		{budget/2 - 1, PolicySlowDown}, // just under half
		{1, PolicySlowDown},
		{0, PolicyFreeze},
		{-time.Minute, PolicyFreeze},
	}
	for _, c := range cases {
		if got := o.BudgetPolicy(c.remaining); got != c.want {
			t.Errorf("BudgetPolicy(%v) = %v, want %v", c.remaining, got, c.want)
		}
	}
}

func TestCanCommitSLA(t *testing.T) {
	if CanCommitSLA(1) {
		t.Error("one quarter of SLO compliance must not permit an SLA promise")
	}
	if !CanCommitSLA(2) {
		t.Error("two consecutive quarters must permit an SLA promise")
	}
}

func TestPlatformObjectivesValidate(t *testing.T) {
	if len(PlatformObjectives) != 5 {
		t.Fatalf("want 5 platform objectives (08§10.2), got %d", len(PlatformObjectives))
	}
	for _, o := range PlatformObjectives {
		if err := o.Validate(); err != nil {
			t.Errorf("platform objective %q invalid: %v", o.Name, err)
		}
	}
}
