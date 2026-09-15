package alertcenter

import (
	"testing"
	"time"
)

// A re-fire that raises the severity is the same alert getting worse, not a
// duplicate: dedup must keep the escalation, and the outcome must not depend
// on the order the batch arrives in.
func TestConvergeKeepsSeverityEscalation(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	warn := Alert{
		TenantID: 100123, Product: "scecs", Metric: "cpu_util",
		Severity: SeverityWarning, Labels: map[string]string{"resource_id": "r-1"},
		OccurredAt: at,
	}
	crit := warn
	crit.Severity = SeverityCritical

	cases := []struct {
		name  string
		batch []Alert
	}{
		{"warning then critical", []Alert{warn, crit}},
		{"critical then warning", []Alert{crit, warn}},
	}
	for _, tc := range cases {
		out := Converge(tc.batch, nil)
		if len(out) != 1 {
			t.Fatalf("%s: %d alerts, want 1", tc.name, len(out))
		}
		if out[0].Severity != SeverityCritical {
			t.Fatalf("%s: severity = %s, want CRITICAL", tc.name, out[0].Severity)
		}
	}
}

// Identical re-fires still collapse to one: the escalation fix must not turn
// dedup off.
func TestConvergeStillDedupsIdenticalFires(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	a := Alert{
		TenantID: 100123, Product: "scecs", Metric: "cpu_util",
		Severity: SeverityWarning, Labels: map[string]string{"resource_id": "r-1"},
		OccurredAt: at,
	}
	if out := Converge([]Alert{a, a}, nil); len(out) != 1 {
		t.Fatalf("%d alerts, want 1", len(out))
	}
}
