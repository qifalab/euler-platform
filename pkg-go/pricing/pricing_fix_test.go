package pricing

import "testing"

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected overflow panic", name)
		}
	}()
	f()
}

// int64 multiplication must never wrap silently: a wrapped amount is a
// corrupted bill. Overflow panics (consistent with MustParseAmount's posture).
func TestMulOverflowPanics(t *testing.T) {
	big := Amount(1 << 62)
	mustPanic(t, "Mul", func() { _ = big.Mul(4) })
	mustPanic(t, "Mul negative", func() { _ = (-big).Mul(4) })
	// In-range products stay exact.
	if got := Amount(3_500_000).Mul(2); got != 7_000_000 {
		t.Fatalf("Mul = %d", got)
	}
	if got := Amount(-3).Mul(5); got != -15 {
		t.Fatalf("Mul = %d", got)
	}
}

func TestMulRateOverflowAndPrecision(t *testing.T) {
	big := Amount(1 << 62)
	// Intermediate 2^62 × 1e6 no longer wraps: the 128-bit product divided by
	// 10000 still exceeds int64, so it panics rather than returning garbage.
	mustPanic(t, "MulRate", func() { _ = big.MulRate(1_000_000) })
	// A huge intermediate whose RESULT fits must succeed (the old code wrapped).
	a := Amount(2_000_000_000_000_000_000) // 2e18; ×8500 wraps int64
	if got := a.MulRate(8500); got != 1_700_000_000_000_000_000 {
		t.Fatalf("MulRate large = %d", got)
	}
	// Rounding half away from zero preserved.
	if got := MustParseAmount("1").MulRate(8500); got.String() != "0.85" {
		t.Fatalf("MulRate 85%% = %s", got)
	}
	if got := Amount(-1).MulRate(5000); got != -1 {
		t.Fatalf("MulRate round away from zero = %d, want -1", got)
	}
}

func TestMulDivOverflowAndPrecision(t *testing.T) {
	big := Amount(1 << 62)
	mustPanic(t, "MulDiv", func() { _ = big.MulDiv(1_000_000, 3) })
	// Huge intermediate, in-range result (the reason for 128-bit math).
	a := Amount(6_000_000_000_000_000_000)
	if got := a.MulDiv(11, 12); got != 5_500_000_000_000_000_000 {
		t.Fatalf("MulDiv large = %d", got)
	}
	// Existing semantics preserved.
	if got := MustParseAmount("1200").MulDiv(11, 12); got.String() != "1100" {
		t.Fatalf("MulDiv 11/12 = %s", got)
	}
	if got := MustParseAmount("1").MulDiv(1, 3); got.String() != "0.333333" {
		t.Fatalf("MulDiv 1/3 = %s", got)
	}
	if got := Amount(10).MulDiv(1, 0); got != 0 {
		t.Fatalf("MulDiv zero den = %d, want 0", got)
	}
	if got := Amount(-100).MulDiv(1, 3); got != -33 {
		t.Fatalf("MulDiv negative = %d, want -33", got)
	}
	if got := Amount(100).MulDiv(-1, 3); got != -33 {
		t.Fatalf("MulDiv negative num = %d, want -33", got)
	}
	if got := Amount(100).MulDiv(1, -3); got != -33 {
		t.Fatalf("MulDiv negative den = %d, want -33", got)
	}
}

// units*scaleFactor used to wrap BEFORE the `total < 0` check, so some
// overflowing inputs wrapped back to positive and slipped through.
func TestParseAmountOverflowBeforeWrap(t *testing.T) {
	for _, s := range []string{
		"9223372036854775807", // ×1e6 wraps to a positive value
		"9223372036854.775808",
		"18446744073709.551616", // wraps all the way past zero
	} {
		if a, err := ParseAmount(s); err == nil {
			t.Errorf("ParseAmount(%q) = %v, want overflow error", s, a)
		}
	}
	if a, err := ParseAmount("9223372036854.775807"); err != nil || int64(a) != 1<<63-1 {
		t.Fatalf("max amount should parse: %v, %v", a, err)
	}
}
