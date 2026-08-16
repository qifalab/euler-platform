package backup

import (
	"reflect"
	"testing"
	"time"
)

// The policy-anchored ExpiredSnapshots must use the policy's own RetentionDays
// so an executor cannot expire against a number the customer never configured.
func TestPolicyExpiredSnapshotsUsesPolicyRetention(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	p := BackupPolicy{PolicyID: "bp-1", TargetResourceType: "instance", ScheduleHours: 24, RetentionDays: 7}
	snaps := []Snapshot{
		{SnapshotID: "old", TakenAt: now.Add(-8 * 24 * time.Hour)},
		{SnapshotID: "fresh", TakenAt: now.Add(-2 * 24 * time.Hour)},
	}
	got := p.ExpiredSnapshots(snaps, now)
	want := []Snapshot{snaps[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expired = %v, want %v", got, want)
	}
	// Matches the shared implementation exactly.
	if !reflect.DeepEqual(got, ExpiredSnapshots(snaps, p.RetentionDays, now)) {
		t.Fatal("policy method must delegate to the shared implementation")
	}
	// Retain-forever policy expires nothing.
	forever := p
	forever.RetentionDays = 0
	if out := forever.ExpiredSnapshots(snaps, now); out != nil {
		t.Fatalf("retention 0 must expire nothing, got %v", out)
	}
}
