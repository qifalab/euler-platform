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
		TenantID: 100123, Product: "euecs", Metric: "cpu_util",
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
		TenantID: 100123, Product: "euecs", Metric: "cpu_util",
		Severity: SeverityWarning, Labels: map[string]string{"resource_id": "r-1"},
		OccurredAt: at,
	}
	if out := Converge([]Alert{a, a}, nil); len(out) != 1 {
		t.Fatalf("%d alerts, want 1", len(out))
	}
}

// The rate limit is applied to Converge's output in order, so the output must
// lead with the most severe groups — not with the oldest.
func TestConvergeOrdersBySeverityFirst(t *testing.T) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	batch := []Alert{
		{TenantID: 1, Product: "old-info", Metric: "m", Severity: SeverityInfo, OccurredAt: at},
		{TenantID: 1, Product: "mid-warn", Metric: "m", Severity: SeverityWarning, OccurredAt: at.Add(time.Second)},
		{TenantID: 1, Product: "new-crit", Metric: "m", Severity: SeverityCritical, OccurredAt: at.Add(2 * time.Second)},
		{TenantID: 1, Product: "newer-crit", Metric: "m", Severity: SeverityCritical, OccurredAt: at.Add(3 * time.Second)},
	}
	out := Converge(batch, nil)
	want := []string{"new-crit", "newer-crit", "mid-warn", "old-info"}
	for i, a := range out {
		if a.Product != want[i] {
			t.Fatalf("order[%d] = %s, want %v", i, a.Product, want)
		}
	}
}

func TestParseSeverity(t *testing.T) {
	for in, want := range map[string]Severity{
		"critical": SeverityCritical, " Warning ": SeverityWarning, "INFO": SeverityInfo,
	} {
		if got, ok := ParseSeverity(in); !ok || got != want {
			t.Errorf("ParseSeverity(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "urgent", "crit"} {
		if _, ok := ParseSeverity(in); ok {
			t.Errorf("ParseSeverity(%q) accepted an unknown severity", in)
		}
	}
}
