package totp

import (
	"strings"
	"testing"
	"time"
)

// rfcSeed is the RFC 4226 §5.3 / RFC 6238 Appendix B seed: the ASCII string
// "12345678901234567890". Both RFCs publish expected outputs for it, which is
// the only way to prove an implementation against the spec rather than against
// itself.
var rfcSeed = []byte("12345678901234567890")

// TestRFC4226Vectors pins the HMAC + dynamic-truncation core against RFC 4226
// Appendix D. If these fail, everything above hotp() is meaningless.
func TestRFC4226Vectors(t *testing.T) {
	vectors := []struct {
		counter uint64
		want    string
	}{
		{0, "755224"}, {1, "287082"}, {2, "359152"}, {3, "969429"},
		{4, "338314"}, {5, "254676"}, {6, "287922"}, {7, "162583"},
		{8, "399871"}, {9, "520489"},
	}
	for _, v := range vectors {
		if got := hotp(rfcSeed, v.counter, 6); got != v.want {
			t.Errorf("hotp(counter=%d) = %s, want %s", v.counter, got, v.want)
		}
	}
}

// TestRFC6238Vectors pins the time-step mapping against RFC 6238 Appendix B
// (SHA-1, T0=0, 30s step, 8 digits).
func TestRFC6238Vectors(t *testing.T) {
	vectors := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, v := range vectors {
		counter := uint64(v.unix) / 30
		if got := hotp(rfcSeed, counter, 8); got != v.want {
			t.Errorf("hotp(unix=%d → counter=%d) = %s, want %s", v.unix, counter, got, v.want)
		}
	}
}

// TestCodeStepsWithWallClock checks the exported Code(): two moments inside one
// 30-second step agree, the next step does not.
func TestCodeStepsWithWallClock(t *testing.T) {
	inStep := time.Unix(1111111080, 0) // 1111111080/30 = 37037036 exactly
	nextStep := inStep.Add(Step)

	c1 := Code(rfcSeed, inStep)
	c2 := Code(rfcSeed, inStep.Add(29*time.Second))
	c3 := Code(rfcSeed, nextStep)
	if c1 != c2 {
		t.Errorf("same step must yield same code: %s vs %s", c1, c2)
	}
	if c1 == c3 {
		t.Errorf("adjacent steps must differ: both %s", c1)
	}
	if len(c1) != Digits {
		t.Errorf("code length = %d, want %d", len(c1), Digits)
	}
}

// --- window acceptance ---

func TestVerifyAcceptsCurrentStep(t *testing.T) {
	v := NewVerifier()
	now := time.Unix(1111111100, 0)
	code := Code(rfcSeed, now)
	if err := v.Verify("k1", rfcSeed, code, now); err != nil {
		t.Fatalf("current-step code rejected: %v", err)
	}
}

func TestVerifyAcceptsSkewWindow(t *testing.T) {
	// A code generated at T-1 and T+1 steps must still verify at T: clock
	// drift and human typing time are normal, not attacks.
	v := NewVerifier()
	now := time.Unix(1111111100, 0)
	for _, off := range []time.Duration{-Step, +Step} {
		code := Code(rfcSeed, now.Add(off))
		if err := v.Verify("skew", rfcSeed, code, now); err != nil {
			t.Errorf("code from %v rejected: %v", off, err)
		}
	}
}

func TestVerifyRejectsBeyondWindow(t *testing.T) {
	v := NewVerifier()
	now := time.Unix(1111111100, 0)
	// 2 steps away = 60s late/early: outside ±1 window, must fail.
	code := Code(rfcSeed, now.Add(2*Step))
	if err := v.Verify("k2", rfcSeed, code, now); err != ErrBadCode {
		t.Fatalf("want ErrBadCode for 2-step-old code, got %v", err)
	}
}

func TestVerifyRejectsWrongAndMalformedCode(t *testing.T) {
	v := NewVerifier()
	now := time.Unix(1111111100, 0)
	if err := v.Verify("k3", rfcSeed, "000000", now); err != ErrBadCode {
		// "000000" colliding with the real code would be a 10^-6 fluke;
		// regenerate deterministically if it ever trips.
		if Code(rfcSeed, now) == "000000" {
			t.Skip("code happened to be 000000")
		}
		t.Fatalf("want ErrBadCode, got %v", err)
	}
	for _, bad := range []string{"", "12345", "1234567", "12345a", "abcdef"} {
		if err := v.Verify("k3", rfcSeed, bad, now); err != ErrMalformedCode {
			t.Errorf("Verify(%q): want ErrMalformedCode, got %v", bad, err)
		}
	}
}

// --- replay protection ---

func TestVerifyRefusesReplayOfSameCode(t *testing.T) {
	// The attack this kills: attacker reads the victim's code over their
	// shoulder, then replays it within the 90s validity window.
	v := NewVerifier()
	now := time.Unix(1111111100, 0)
	code := Code(rfcSeed, now)
	if err := v.Verify("k4", rfcSeed, code, now); err != nil {
		t.Fatalf("first use rejected: %v", err)
	}
	if err := v.Verify("k4", rfcSeed, code, now); err != ErrReplayed {
		t.Fatalf("replay must return ErrReplayed, got %v", err)
	}
	// ... even one step later, while the code is still inside the window.
	if err := v.Verify("k4", rfcSeed, code, now.Add(Step)); err != ErrReplayed {
		t.Fatalf("delayed replay must return ErrReplayed, got %v", err)
	}
}

func TestVerifyRefusesTimeGoingBackwards(t *testing.T) {
	// Consume the T+1 code first; the T code (an older sibling still in the
	// window) must then be refused — otherwise an attacker could replay the
	// previous step's code after watching a login succeed.
	v := NewVerifier()
	now := time.Unix(1111111100, 0)
	future := Code(rfcSeed, now.Add(Step))
	current := Code(rfcSeed, now)
	if err := v.Verify("k5", rfcSeed, future, now); err != nil {
		t.Fatalf("future code rejected: %v", err)
	}
	if err := v.Verify("k5", rfcSeed, current, now); err != ErrReplayed {
		t.Fatalf("older sibling code must be refused, got %v", err)
	}
	// The next genuinely-new step works again.
	if err := v.Verify("k5", rfcSeed, Code(rfcSeed, now.Add(2*Step)), now.Add(2*Step)); err != nil {
		t.Fatalf("fresh code after replay block rejected: %v", err)
	}
}

func TestVerifierForgetResetsReplayState(t *testing.T) {
	v := NewVerifier()
	now := time.Unix(1111111100, 0)
	code := Code(rfcSeed, now)
	if err := v.Verify("k6", rfcSeed, code, now); err != nil {
		t.Fatalf("first use rejected: %v", err)
	}
	v.Forget("k6")
	// After unbind/rebind the same timestep is acceptable again — a fresh
	// seed has no history.
	if err := v.Verify("k6", rfcSeed, code, now); err != nil {
		t.Fatalf("post-Forget reuse rejected: %v", err)
	}
}

func TestReplayStateIsScopedPerKey(t *testing.T) {
	v := NewVerifier()
	now := time.Unix(1111111100, 0)
	code := Code(rfcSeed, now)
	if err := v.Verify("user-a", rfcSeed, code, now); err != nil {
		t.Fatalf("user-a rejected: %v", err)
	}
	// Same code, different user, same instant: not a replay — distinct keys
	// keep independent replay memory.
	if err := v.Verify("user-b", rfcSeed, code, now); err != nil {
		t.Fatalf("user-b should not be affected by user-a's use: %v", err)
	}
}

func TestVerifyRejectsEmptySecret(t *testing.T) {
	v := NewVerifier()
	if err := v.Verify("k7", nil, "123456", time.Now()); err != ErrBadSecret {
		t.Fatalf("want ErrBadSecret, got %v", err)
	}
}

// --- secret handling ---

func TestSecretBase32RoundTrip(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(secret) != 20 {
		t.Fatalf("secret length = %d, want 20 (160 bits)", len(secret))
	}
	s32 := SecretBase32(secret)
	if strings.Contains(s32, "=") {
		t.Errorf("base32 secret must be unpadded: %s", s32)
	}
	back, err := ParseSecretBase32(s32)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if string(back) != string(secret) {
		t.Fatalf("round trip mismatch")
	}
	// Padding tolerated on copy-paste.
	if back2, err := ParseSecretBase32(s32 + "===="); err != nil || string(back2) != string(secret) {
		t.Errorf("padded form should parse: %v", err)
	}
	if _, err := ParseSecretBase32("  "); err == nil {
		t.Error("blank secret should fail")
	}
}

func TestGeneratedSecretsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s, _ := GenerateSecret()
		key := string(s)
		if seen[key] {
			t.Fatal("duplicate secret generated")
		}
		seen[key] = true
	}
}

// --- provisioning URI ---

func TestProvisioningURI(t *testing.T) {
	uri := ProvisioningURI(rfcSeed, "admin@starcloud.cn", "StarCloud")
	// '@' is legal in a URI path segment, so PathEscape keeps the email intact
	// — matching how every authenticator app renders the label.
	if !strings.HasPrefix(uri, "otpauth://totp/StarCloud:admin@starcloud.cn?") {
		t.Fatalf("unexpected URI shape: %s", uri)
	}
	if !strings.Contains(uri, "secret="+SecretBase32(rfcSeed)) {
		t.Errorf("URI missing secret parameter: %s", uri)
	}
	if !strings.Contains(uri, "issuer=StarCloud") {
		t.Errorf("URI missing issuer: %s", uri)
	}
}
