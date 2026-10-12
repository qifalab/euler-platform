package provider

import (
	"fmt"
	"os"
	"testing"

	"github.com/qifalab/euler-platform/eusdk"
)

// credentialPairs are the accepted spellings, in the SDKs' precedence order.
var credentialPairs = [][2]string{
	{"EULER_ACCESS_KEY_ID", "EULER_SECRET_ACCESS_KEY"},
	{"EULER_ACCESS_KEY", "EULER_SECRET_KEY"},
	{"EULER_AK", "EULER_SK"},
}

// L2: the provider preferred EULER_ACCESS_KEY over EULER_ACCESS_KEY_ID while
// eusdk and the Python SDK prefer EULER_ACCESS_KEY_ID, so a shell exporting
// both spellings signed with different keys in different tools. Every
// combination of exported spellings now resolves exactly as eusdk resolves it.
func TestCredentialsFromEnvMatchesEusdk(t *testing.T) {
	for mask := 0; mask < 1<<len(credentialPairs); mask++ {
		for i, p := range credentialPairs {
			ak, sk := "", ""
			if mask&(1<<i) != 0 {
				ak, sk = fmt.Sprintf("AK%d", i), fmt.Sprintf("SK%d", i)
			}
			t.Setenv(p[0], ak)
			t.Setenv(p[1], sk)
		}
		gotAK, gotSK := credentialsFromEnv(os.Getenv)
		wantAK, wantSK := eusdk.CredentialsFromEnv()
		if gotAK != wantAK || gotSK != wantSK {
			t.Fatalf("mask %03b: provider resolved %q/%q, eusdk %q/%q", mask, gotAK, gotSK, wantAK, wantSK)
		}
		if mask != 0 && gotAK == "" {
			t.Fatalf("mask %03b: an exported spelling was not accepted", mask)
		}
	}
}

func TestCredentialsFromEnvPrecedence(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	// Exported from the lowest precedence up: each newly exported, higher
	// precedence spelling takes over.
	for i := len(credentialPairs) - 1; i >= 0; i-- {
		p := credentialPairs[i]
		env[p[0]], env[p[1]] = p[0]+"-value", p[1]+"-value"
		if ak, sk := credentialsFromEnv(getenv); ak != p[0]+"-value" || sk != p[1]+"-value" {
			t.Fatalf("with %s exported: got %q/%q", p[0], ak, sk)
		}
	}
}
