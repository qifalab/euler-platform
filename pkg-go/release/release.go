// Package release encodes the phase-3 release discipline — 变更三板斧:
// 可灰度 (canary), 可监控 (automated analysis gates), 可回滚 (rollback path)
// (09-roadmap §5.1 goal 2; 08§6). These are the non-negotiable gates a change
// must pass, modelled as code so they are enforced, not aspirational.
//
//   - Canary progression is fixed at 5% → 20% → 50% → 100% (08§6.3), and each
//     step must pass the automated analysis gate (success rate ≥ 0.995,
//     p99 latency ≤ 1.5s) before the next step opens. OpenAPI and billing
//     changes are hard-required to use this path (08§6.1).
//   - Database changes use expand-contract with an observation window of at
//     least 7 days, and rollback reverts code only, never schema (08§6.5).
//   - Rollback preference is git revert first (the auditable path); ArgoCD
//     rollback is the minute-level stopgap, not the record (08§4.5).
package release

import (
	"errors"
	"fmt"
	"time"
)

// CanarySteps is the fixed canary progression (08§6.3): 5% → 20% → 50% → 100%.
var CanarySteps = []int{5, 20, 50, 100}

// Canary gate thresholds (08§6.3 AnalysisTemplate).
const (
	// MinSuccessRate is the minimum success rate a canary step must hold
	// (failureLimit 1 on the AnalysisTemplate).
	MinSuccessRate = 0.995
	// MaxP99Latency is the maximum p99 latency a canary step may show (1.5s,
	// failureLimit 2).
	MaxP99Latency = 1500 * time.Millisecond
)

// PassCanaryGate reports whether a canary step's observed metrics clear the
// automated analysis gate (08§6.3). Both thresholds must hold; one metric
// alone never passes the step.
func PassCanaryGate(successRate float64, p99 time.Duration) bool {
	return successRate >= MinSuccessRate && p99 <= MaxP99Latency
}

// ValidateCanarySteps rejects a malformed progression. The steps are a
// contract (08§6.3), not a preference: they must start at 5, end at 100, and
// strictly increase — a progression that regresses (100 → 50) or skips a
// widening step is a mis-typed rollout that must fail at definition time.
func ValidateCanarySteps(steps []int) error {
	if len(steps) < 2 {
		return errors.New("release: canary progression needs at least two steps")
	}
	if steps[0] != 5 {
		return fmt.Errorf("release: canary progression must start at 5%%, got %d", steps[0])
	}
	if steps[len(steps)-1] != 100 {
		return fmt.Errorf("release: canary progression must end at 100%%, got %d", steps[len(steps)-1])
	}
	for i := 1; i < len(steps); i++ {
		if steps[i] <= steps[i-1] {
			return fmt.Errorf("release: canary progression must strictly increase, got %d after %d", steps[i], steps[i-1])
		}
	}
	return nil
}

// ExpandContractObservationWindow is the minimum time a database change stays
// in the expand phase before the contract step runs (08§6.5: 观察期 ≥ 7 天).
const ExpandContractObservationWindow = 7 * 24 * time.Hour

// PassExpandContractGate reports whether a database change has observed the
// expand phase long enough before the contract step may proceed (08§6.5).
func PassExpandContractGate(since time.Duration) bool {
	return since >= ExpandContractObservationWindow
}

// RollbackMode is the rollback path preference (08§4.5, §9.3).
type RollbackMode string

const (
	// RollbackRevert: git revert MR → ArgoCD syncs the old image. The auditable
	// path, and the one that leaves a record.
	RollbackRevert RollbackMode = "REVERT"
	// RollbackAppRollback: argocd app rollback — minute-level stopgap, to be
	// followed by a git revert. Never the record.
	RollbackAppRollback RollbackMode = "APP_ROLLBACK"
)

// PreferredRollback returns the first-choice rollback mode. A revert is always
// preferred to an ArgoCD rollback (08§4.5: 首选 git revert 版本 MR → ArgoCD
// 同步旧镜像; 次选 argocd app rollback, 事后补 git revert).
func PreferredRollback() RollbackMode { return RollbackRevert }
