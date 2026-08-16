package metering

import (
	"errors"
	"testing"
	"time"
)

// A zero-value Aggregator (ExpectedWindowSeconds == 0) must fall back to the
// 60s default instead of panicking with an integer divide by zero.
func TestAggregateZeroWindowSecondsDoesNotPanic(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 30, 0, 0, time.UTC)
	a := &Aggregator{Now: func() time.Time { return now }} // zero ExpectedWindowSeconds
	hour := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)
	rec := UsageRecord{
		ResourceID: "r-1", MeteringItem: "cpu",
		Quantity: MustParseQuantity("1"), WindowStart: hour,
	}
	h, err := a.Aggregate([]UsageRecord{rec}, hour)
	if err != nil {
		t.Fatal(err)
	}
	if h.WindowsExpected != 60 {
		t.Fatalf("WindowsExpected = %d, want 60 (default fallback)", h.WindowsExpected)
	}

	a.ExpectedWindowSeconds = -5
	if _, err := a.Aggregate([]UsageRecord{rec}, hour); err != nil {
		t.Fatalf("negative window seconds must also fall back, got %v", err)
	}
}

func TestParseQuantityRejectsDotAndSignOnly(t *testing.T) {
	for _, s := range []string{".", "-", "-."} {
		if q, err := ParseQuantity(s); err == nil {
			t.Errorf("ParseQuantity(%q) = %v, want error", s, q)
		}
	}
	// ".5" still parses (fractional-only form with digits).
	if q, err := ParseQuantity(".5"); err != nil || q.String() != "0.5" {
		t.Fatalf("ParseQuantity(.5) = %v, %v", q, err)
	}
}

func TestParseQuantityOverflow(t *testing.T) {
	for _, s := range []string{
		"99999999999999999999", // integer-digit overflow
		"9223372036854775807",  // ×1e6 overflow
		"9223372036854.775808", // one past max total
	} {
		if q, err := ParseQuantity(s); err == nil {
			t.Errorf("ParseQuantity(%q) = %v, want overflow error", s, q)
		}
	}
	// Max representable value parses.
	if _, err := ParseQuantity("9223372036854.775807"); err != nil {
		t.Fatalf("max quantity should parse: %v", err)
	}
}

// Records whose window falls outside the aggregation hour must be rejected,
// not silently summed into the wrong hour's total.
func TestAggregateRejectsWindowOutsideHour(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 30, 0, 0, time.UTC)
	a := NewAggregator(func() time.Time { return now })
	hour := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)

	in := UsageRecord{ResourceID: "r-1", MeteringItem: "cpu",
		Quantity: MustParseQuantity("1"), WindowStart: hour.Add(59 * time.Minute)}
	if _, err := a.Aggregate([]UsageRecord{in}, hour); err != nil {
		t.Fatalf("in-hour record must aggregate: %v", err)
	}

	for _, ws := range []time.Time{hour.Add(-time.Minute), hour.Add(time.Hour), hour.Add(2 * time.Hour)} {
		out := in
		out.WindowStart = ws
		_, err := a.Aggregate([]UsageRecord{in, out}, hour)
		if !errors.Is(err, ErrWindowOutsideHour) {
			t.Errorf("window %v: err = %v, want ErrWindowOutsideHour", ws, err)
		}
	}
}
