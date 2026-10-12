package runmode

import "testing"

func TestProduction(t *testing.T) {
	for env, want := range map[string]bool{"": false, "dev": false, "test": false, "prod": true, "Production": true, " staging ": true} {
		t.Setenv(EnvVar, env)
		if got := Production(); got != want {
			t.Errorf("EULER_ENV=%q: Production() = %v, want %v", env, got, want)
		}
	}
}

func TestDevSeedNeverInProduction(t *testing.T) {
	t.Setenv(DevSeedVar, "1")
	t.Setenv(EnvVar, "dev")
	if !DevSeedEnabled() {
		t.Fatal("dev + EULER_DEV_SEED=1 should enable seeds")
	}
	t.Setenv(EnvVar, "prod")
	if DevSeedEnabled() {
		t.Fatal("seeds must never be enabled in production")
	}
	t.Setenv(EnvVar, "")
	t.Setenv(DevSeedVar, "")
	if DevSeedEnabled() {
		t.Fatal("seeds are opt-in")
	}
}

func TestRequireSecrets(t *testing.T) {
	t.Setenv("RUNMODE_TEST_A", "x")
	t.Setenv("RUNMODE_TEST_B", "")
	t.Setenv(EnvVar, "")
	if err := RequireSecrets("RUNMODE_TEST_A", "RUNMODE_TEST_B"); err != nil {
		t.Fatalf("outside production a missing secret is a warning, got %v", err)
	}
	t.Setenv(EnvVar, "prod")
	if err := RequireSecrets("RUNMODE_TEST_A", "RUNMODE_TEST_B"); err == nil {
		t.Fatal("production must refuse a missing secret")
	}
	if err := RequireSecrets("RUNMODE_TEST_A"); err != nil {
		t.Fatalf("all present: %v", err)
	}
}
