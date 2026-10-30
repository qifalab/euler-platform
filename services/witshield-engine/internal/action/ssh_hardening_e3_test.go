// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package action

import (
	"strings"
	"testing"
)

// sshd also accepts "Keyword=value". A Match-block override written that way
// must be removed too, otherwise password login stays enabled for matching
// clients while Verify (sshd -T without -C) only sees the global "no".
func TestHardenSSHConfigRemovesKeywordEqualsForm(t *testing.T) {
	input := []byte("Port 22\nMatch Address 192.0.2.0/24\n    PasswordAuthentication=yes\n    passwordauthentication = yes\n")
	hardened, changed := hardenSSHConfig(input)
	lower := strings.ToLower(string(hardened))
	if strings.Count(lower, "passwordauthentication") != 1 || !strings.HasPrefix(string(hardened), sshManagedMarker+"\nPasswordAuthentication no\n") {
		t.Fatalf("password override survived hardening:\n%s", hardened)
	}
	if changed != 3 {
		t.Fatalf("changedLines=%d, want 3 (marker and two overrides)", changed)
	}
	again, changedAgain := hardenSSHConfig(hardened)
	if string(again) != string(hardened) || changedAgain != 0 {
		t.Fatalf("hardening is not idempotent: changed=%d\n%s", changedAgain, again)
	}
}
