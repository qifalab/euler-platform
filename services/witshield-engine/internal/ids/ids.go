// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package ids

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

func New(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return prefix + "_" + strings.TrimRight(base64.RawURLEncoding.EncodeToString(b), "=")
}

func Token(prefix string, bytes int) (string, error) {
	if bytes < 16 {
		return "", fmt.Errorf("token entropy must be at least 128 bits")
	}
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// Hint labels a secret for display. Every project reader can see it (masked AI
// settings, enrollment token lists), so the first six and last four characters
// are shown only while at least 16 stay hidden; shorter secrets are fully
// masked. Runes are counted so a multibyte secret never yields invalid UTF-8.
func Hint(token string) string {
	runes := []rune(token)
	if len(runes) < 26 {
		return "••••"
	}
	return string(runes[:6]) + "…" + string(runes[len(runes)-4:])
}
