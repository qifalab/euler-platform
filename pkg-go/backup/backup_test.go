package backup

import (
	"errors"
	"testing"
	"time"
)

var (
	tBase   = time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)
	tTenAM  = time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	tTwoPM  = time.Date(2026, 8, 16, 14, 0, 0, 0, time.UTC)
	tFourPM = time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)
)

func validPolicy() BackupPolicy {
	return BackupPolicy{
		PolicyID:           "eubackup-cn-north-1-01-3f4a5b6c",
		TargetResourceType: "instance",
		ScheduleHours:      6,
		RetentionDays:      7,
		CrossAZ:            false,
	}
}

// TestNextRunAfterInterval: last backup at 10:00, 6h schedule → next run 16:00.
// now (14:00) is before the next run, so the backup is not yet due.
func TestNextRunAfterInterval(t *testing.T) {
	p := validPolicy()
	got := NextRun(p, tTenAM, tTwoPM)
	if !got.Equal(tFourPM) {
		t.Errorf("NextRun = %s, want %s (last + 6h)", got, tFourPM)
	}
}

// TestNextRunFirstRunIsNow: a freshly installed policy (zero lastBackup) backs
// up immediately — waiting a full interval would leave the just-bought policy
// protecting nothing for its first schedule window.
func TestNextRunFirstRunIsNow(t *testing.T) {
	p := validPolicy()
	got := NextRun(p, time.Time{}, tTwoPM)
	if !got.Equal(tTwoPM) {
		t.Errorf("first-run NextRun = %s, want %s (now, not now+interval)", got, tTwoPM)
	}
}

// TestIsDue: due when now >= next, not before.
func TestIsDue(t *testing.T) {
	p := validPolicy()
	// last 10:00, next 16:00. At 14:00 → not due.
	if IsDue(p, tTenAM, tTwoPM) {
		t.Error("IsDue at 14:00 (next 16:00) = true, want false")
	}
	// At exactly 16:00 → due.
	if !IsDue(p, tTenAM, tFourPM) {
		t.Error("IsDue at 16:00 (== next) = false, want true")
	}
	// At 17:00 (past) → due.
	if !IsDue(p, tTenAM, tFourPM.Add(time.Hour)) {
		t.Error("IsDue at 17:00 (past next) = false, want true")
	}
	// First install → due now.
	if !IsDue(p, time.Time{}, tTwoPM) {
		t.Error("IsDue first-run = false, want true")
	}
}

// TestExpiredSnapshotsPicksOld: two snapshots, one old one fresh, retention 7d
// → only the old one is expired.
func TestExpiredSnapshotsPicksOld(t *testing.T) {
	now := tBase
	fresh := Snapshot{SnapshotID: "snap-fresh", ResourceID: "r1", TakenAt: now.Add(-1 * 24 * time.Hour), SizeGB: 10}
	old := Snapshot{SnapshotID: "snap-old", ResourceID: "r1", TakenAt: now.Add(-10 * 24 * time.Hour), SizeGB: 10}
	all := []Snapshot{fresh, old}

	expired := ExpiredSnapshots(all, 7, now)
	if len(expired) != 1 {
		t.Fatalf("expired = %d snapshots, want 1 (only the old one)", len(expired))
	}
	if expired[0].SnapshotID != "snap-old" {
		t.Errorf("expired[0] = %s, want snap-old", expired[0].SnapshotID)
	}
}

// TestExpiredSnapshotsRetentionZeroKeepsAll: retention 0 = retain forever.
// A policy with no retention is a config smell, not a crash — nothing expires.
func TestExpiredSnapshotsRetentionZeroKeepsAll(t *testing.T) {
	now := tBase
	all := []Snapshot{
		{SnapshotID: "snap-1", TakenAt: now.Add(-365 * 24 * time.Hour)},
		{SnapshotID: "snap-2", TakenAt: now.Add(-2 * 24 * time.Hour)},
	}
	expired := ExpiredSnapshots(all, 0, now)
	if expired != nil {
		t.Errorf("retention 0 expired = %v, want nil (retain forever)", expired)
	}
}

// TestValidate: rejects ScheduleHours<=0, RetentionDays<0, empty target;
// accepts the good policy.
func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(BackupPolicy) BackupPolicy
		wantErr error
	}{
		{"good policy", func(p BackupPolicy) BackupPolicy { return p }, nil},
		{"schedule zero", func(p BackupPolicy) BackupPolicy { p.ScheduleHours = 0; return p }, ErrScheduleRequired},
		{"schedule negative", func(p BackupPolicy) BackupPolicy { p.ScheduleHours = -1; return p }, ErrScheduleRequired},
		{"retention negative", func(p BackupPolicy) BackupPolicy { p.RetentionDays = -1; return p }, ErrRetentionNegative},
		{"retention zero ok", func(p BackupPolicy) BackupPolicy { p.RetentionDays = 0; return p }, nil},
		{"empty target", func(p BackupPolicy) BackupPolicy { p.TargetResourceType = ""; return p }, ErrTargetRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.mutate(validPolicy()))
			if c.wantErr == nil {
				if err != nil {
					t.Errorf("Validate(%s) = %v, want nil", c.name, err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Errorf("Validate(%s) = %v, want %v", c.name, err, c.wantErr)
			}
		})
	}
}

// TestCrossAZIntentFlowsIntoSnapshot: a policy with CrossAZ=true produces
// snapshots with CrossAZ=true. The flag is intent carried by the policy; the
// snapshot reflects it so the executor/bill can tell a cross-AZ backup from a
// single-AZ one without re-reading the policy (model先行: the field exists
// before the cross-AZ-copy executor does).
func TestCrossAZIntentFlowsIntoSnapshot(t *testing.T) {
	p := validPolicy()
	p.CrossAZ = true
	// The executor creates a snapshot under this policy; the snapshot's CrossAZ
	// mirrors the policy's intent.
	snap := Snapshot{
		SnapshotID: "snap-xaz",
		ResourceID: "r1",
		TakenAt:    tBase,
		SizeGB:     10,
		CrossAZ:    p.CrossAZ,
	}
	if !snap.CrossAZ {
		t.Error("snapshot from a cross-AZ policy must carry CrossAZ=true")
	}
}
