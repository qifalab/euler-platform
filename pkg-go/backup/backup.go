// Package backup is the policy layer for scheduled snapshots and cross-AZ
// backup retention (06-kubernetes-productization.md §4.1).
//
// # Layering
//
// backup is the POLICY layer — it decides WHEN to back up and WHAT to expire.
// The actual snapshot creation and the cross-AZ copy are the EXECUTOR's job,
// running inside the resource reconcile loop (06§4.1: "backup runs inside the
// reconcile loop"). This package never reaches into a volume or a storage
// backend; it carries intent (the schedule, the retention window, the cross-AZ
// flag) and derives decisions from it.
//
// This is the model先行 principle: the policy and its semantics are defined
// and testable now, before the executor exists, so the catalogue can register
// eubackup (M-7.4) against a frozen contract rather than against an executor
// that is still being built.
//
// # Retention is enforced, never unbounded
//
// A policy without retention enforcement is storage that grows until the
// cluster runs out of space — the failure mode the 06§4.1 note calls out.
// ExpiredSnapshots is the gate: every reconcile tick, the executor asks which
// snapshots are past their retention window and deletes them. A retention of
// 0 means "retain forever" (a config smell surfaced elsewhere, not a crash
// here), but a positive retention MUST be honoured or the bill climbs without
// bound.
package backup

import (
	"errors"
	"fmt"
	"time"
)

// BackupPolicy is the desired-state policy a customer buys (06§4.1, M-7.4).
//
// ScheduleHours is the interval BETWEEN backups, not a cron expression: a
// simpler contract that covers the common case (back up every N hours) and
// avoids the cron-parser dependency. RetentionDays is how long snapshots are
// kept before ExpiredSnapshots marks them for deletion. CrossAZ is the
// cross-AZ-copy intent flag — the executor copies each snapshot to a second
// AZ when true (the actual copy is the executor's job; the policy carries the
// intent, model先行).
type BackupPolicy struct {
	PolicyID           string
	TargetResourceType string // e.g. "instance", "dbinstance" — what the policy backs up
	ScheduleHours      int
	RetentionDays      int
	CrossAZ            bool
}

// Snapshot is one backup taken under a policy.
//
// CrossAZ mirrors the policy's intent at taken-time: a policy whose cross-AZ
// flag was on when the snapshot ran produces a CrossAZ=true snapshot. The
// field flows through here so the executor (and the bill) can tell a
// single-AZ snapshot from a cross-AZ one without re-reading the policy.
type Snapshot struct {
	SnapshotID string
	ResourceID string
	TakenAt    time.Time
	SizeGB     int
	CrossAZ    bool
}

// Errors.
var (
	ErrScheduleRequired  = errors.New("backup: ScheduleHours must be positive")
	ErrRetentionNegative = errors.New("backup: RetentionDays must not be negative")
	ErrTargetRequired    = errors.New("backup: TargetResourceType required")
)

// Validate checks the policy before it can back anything up.
//
// ScheduleHours must be positive (a zero/negative interval means "never back
// up", which is a policy that does nothing — rejected at the cheapest
// checkpoint). RetentionDays may be 0 (retain forever) but never negative.
// TargetResourceType is required: a policy with no target backs up nothing.
// CrossAZ is a plain intent flag and needs no validation here.
func Validate(p BackupPolicy) error {
	if p.ScheduleHours <= 0 {
		return ErrScheduleRequired
	}
	if p.RetentionDays < 0 {
		return ErrRetentionNegative
	}
	if p.TargetResourceType == "" {
		return ErrTargetRequired
	}
	return nil
}

// NextRun returns the instant the next backup should run, given the last
// backup time and the policy's schedule.
//
// If lastBackup is zero (never backed up — a freshly installed policy), the
// next run is now: a brand-new policy should take its first backup right
// away, not wait a full interval. Waiting would mean a policy created at
// T0 with a 24h schedule does nothing until T0+24h — the very window the
// customer bought the policy to protect.
func NextRun(p BackupPolicy, lastBackup time.Time, now time.Time) time.Time {
	if lastBackup.IsZero() {
		return now
	}
	return lastBackup.Add(time.Duration(p.ScheduleHours) * time.Hour)
}

// IsDue reports whether a backup is due now under the policy.
//
// now >= NextRun: the scheduled instant has arrived (or passed). The executor
// should take a backup and update lastBackup; the next IsDue call then keys
// off the new lastBackup.
func IsDue(p BackupPolicy, lastBackup time.Time, now time.Time) bool {
	return !now.Before(NextRun(p, lastBackup, now))
}

// ExpiredSnapshots returns the snapshots whose TakenAt is older than the
// retention window, i.e. past their keep-by date.
//
// Deprecated: the bare retentionDays parameter invites passing a number that
// disagrees with the policy the snapshots were taken under. Use
// BackupPolicy.ExpiredSnapshots, which reads the retention from the policy
// itself. This function remains as the shared implementation.
//
// retentionDays == 0 means "retain forever" — nothing is expired. A policy
// with no retention is a configuration smell (storage grows unbounded) but is
// not a crash; the executor surfaces it elsewhere. With a positive retention,
// a snapshot older than retentionDays is past its keep-by and MUST be deleted
// — this is the gate that keeps backup storage bounded (06§4.1).
//
// now is taken as a parameter (not time.Now()) so the decision is deterministic
// and testable: the executor passes the reconcile tick's clock, and a test
// passes a fixed instant.
func ExpiredSnapshots(snapshots []Snapshot, retentionDays int, now time.Time) []Snapshot {
	if retentionDays <= 0 {
		return nil
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	var expired []Snapshot
	for _, s := range snapshots {
		if s.TakenAt.Before(cutoff) {
			expired = append(expired, s)
		}
	}
	return expired
}

// ExpiredSnapshots returns the snapshots past the POLICY's retention window.
// The retention comes from the policy itself — the single source of truth —
// so an executor cannot accidentally expire snapshots against a different
// number than the customer configured.
func (p BackupPolicy) ExpiredSnapshots(snapshots []Snapshot, now time.Time) []Snapshot {
	return ExpiredSnapshots(snapshots, p.RetentionDays, now)
}

// String renders a policy for diagnostics/logging (never parsed by consumers).
func (p BackupPolicy) String() string {
	return fmt.Sprintf("BackupPolicy{%s target=%s every=%dh keep=%dd crossAZ=%v}",
		p.PolicyID, p.TargetResourceType, p.ScheduleHours, p.RetentionDays, p.CrossAZ)
}
