package main

import (
	"os"
	"testing"
)

// The handler tests act as the seeded dev operator; production code only
// grants that role with EULER_DEV_SEED=1 outside production.
func init() { operatorAccounts[devOperatorAccount] = true }

func TestNoDefaultOperatorWithoutDevSeed(t *testing.T) {
	t.Setenv("EULER_DEV_SEED", "")
	if got := parseOperators(""); len(got) != 0 {
		t.Fatalf("no operators configured must mean nobody, got %v", got)
	}
	t.Setenv("EULER_DEV_SEED", "1")
	t.Setenv("EULER_ENV", "prod")
	if got := parseOperators(""); len(got) != 0 {
		t.Fatalf("dev seed must not apply in production, got %v", got)
	}
	t.Setenv("EULER_ENV", "")
	if got := parseOperators(""); !got[devOperatorAccount] {
		t.Fatalf("dev seed outside production should grant the demo operator, got %v", got)
	}
	if got := parseOperators("200001, 200002"); len(got) != 2 || got[devOperatorAccount] {
		t.Fatalf("explicit list = %v", got)
	}
	_ = os.Getenv
}
