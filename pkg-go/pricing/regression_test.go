package pricing

import (
	"strings"
	"testing"
)

// A price field that is just a decimal point carries no digits. Parsing it as
// zero (the pre-fix behaviour) silently turned a malformed catalogue row into a
// free product; the strict parser must reject it.
func TestParseAmountRejectsDigitslessDecimalPoint(t *testing.T) {
	for _, s := range []string{".", "-.", "+."} {
		if got, err := ParseAmount(s); err == nil {
			t.Fatalf("ParseAmount(%q) = %s with no error, want a parse error", s, got)
		}
	}
}

// Ordinary decimals (including a leading point) still parse.
func TestParseAmountStillAcceptsNormalDecimals(t *testing.T) {
	cases := map[string]string{"0": "0", "180.50": "180.5", ".5": "0.5", "-12.34": "-12.34"}
	for in, want := range cases {
		got, err := ParseAmount(in)
		if err != nil {
			t.Fatalf("ParseAmount(%q): %v", in, err)
		}
		if got.String() != want {
			t.Fatalf("ParseAmount(%q) = %s, want %s", in, got, want)
		}
	}
}

// String() must render math.MinInt64 exactly: negating it in int64 wraps back
// to itself, which used to produce a garbled "--9223372036854.-775808".
func TestAmountStringHandlesMinInt64(t *testing.T) {
	got := Amount(-1 << 63).String()
	if want := "-9223372036854.775808"; got != want {
		t.Fatalf("String(MinInt64) = %q, want %q", got, want)
	}
	if strings.Count(got, "-") != 1 {
		t.Fatalf("String(MinInt64) = %q, want exactly one minus sign", got)
	}
}
