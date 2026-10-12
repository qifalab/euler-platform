// Package runmode decides whether a process runs in a production-like
// environment, and enforces that security-critical configuration is present
// there.
//
// Every security control in this repo is configured through environment
// variables (signing keys, the internal shared secret, KMS master keys). Before
// this package existed, a missing variable silently meant "dev mode": an
// ephemeral JWT key, no internal-token check, a demo admin account with a
// published password. A deployment that forgot one variable therefore ran
// insecurely without anyone noticing — fail-open. Now:
//
//   - EULER_ENV=prod|production|staging marks a production-like environment,
//     and services refuse to start there unless their secrets are configured;
//   - demo data that carries credentials (seed accounts, seed AKs, the dev
//     marketplace operator) is created only with EULER_DEV_SEED=1, and never
//     in a production-like environment.
package runmode

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// EnvVar names the deployment environment.
const EnvVar = "EULER_ENV"

// DevSeedVar opts a NON-production process into credential-bearing demo data.
const DevSeedVar = "EULER_DEV_SEED"

// Env returns the normalised EULER_ENV value ("" when unset).
func Env() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv(EnvVar)))
}

// Production reports whether this is a production-like environment.
func Production() bool {
	switch Env() {
	case "prod", "production", "staging", "pre", "preprod":
		return true
	}
	return false
}

// DevSeedEnabled reports whether credential-bearing demo data may be created.
// It requires an explicit EULER_DEV_SEED=1 and is always false in production.
func DevSeedEnabled() bool {
	if os.Getenv(DevSeedVar) != "1" {
		return false
	}
	if Production() {
		slog.Warn("EULER_DEV_SEED ignored in a production environment", "env", Env())
		return false
	}
	return true
}

// MissingSecrets returns the names (of those given) that are unset or blank.
func MissingSecrets(names ...string) []string {
	var missing []string
	for _, n := range names {
		if strings.TrimSpace(os.Getenv(n)) == "" {
			missing = append(missing, n)
		}
	}
	return missing
}

// RequireSecrets returns an error naming every missing variable when running
// in production; outside production it only logs a warning per missing name.
func RequireSecrets(names ...string) error {
	missing := MissingSecrets(names...)
	if len(missing) == 0 {
		return nil
	}
	if Production() {
		return fmt.Errorf("runmode: %s=%s requires %s", EnvVar, Env(), strings.Join(missing, ", "))
	}
	for _, n := range missing {
		slog.Warn("security setting not configured (allowed outside production only)", "var", n)
	}
	return nil
}

// MustRequireSecrets is RequireSecrets for main(): it exits the process when a
// production environment is missing any of the names.
func MustRequireSecrets(names ...string) {
	if err := RequireSecrets(names...); err != nil {
		slog.Error("refusing to start with insecure configuration", "err", err)
		os.Exit(1)
	}
}
