package ids

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Hint output is returned to every project reader (masked AI settings,
// enrollment token lists), so it must never disclose most of a secret.
func TestHintKeepsMostOfEverySecretHidden(t *testing.T) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	for n := 1; n <= 80; n++ {
		secret := strings.Repeat(alphabet, 3)[:n]
		hint := Hint(secret)
		if hint == "••••" {
			continue
		}
		prefix, suffix, ok := strings.Cut(hint, "…")
		if !ok || !strings.HasPrefix(secret, prefix) || !strings.HasSuffix(secret, suffix) {
			t.Fatalf("len=%d: hint %q is not a prefix/suffix hint", n, hint)
		}
		if hidden := n - len(prefix) - len(suffix); hidden < 16 {
			t.Fatalf("len=%d: hint %q leaves only %d characters hidden", n, hint, hidden)
		}
	}
	for _, short := range []string{"sk-12345678", "abcdefghijkl", "my-secret-key-123"} {
		if hint := Hint(short); hint != "••••" {
			t.Errorf("short secret %q disclosed as %q", short, hint)
		}
	}
}

func TestHintKeepsRecognisableLongTokensAndValidUTF8(t *testing.T) {
	token, err := Token("wse", 32)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Hint(token), token[:6]+"…"+token[len(token)-4:]; got != want {
		t.Fatalf("long token hint=%q want=%q", got, want)
	}
	multibyte := strings.Repeat("密钥", 20)
	if hint := Hint(multibyte); !utf8.ValidString(hint) || hint == "••••" {
		t.Fatalf("multibyte secret hint is not a valid prefix/suffix hint: %q", hint)
	}
}
