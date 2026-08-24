package resource

import (
	"testing"
	"time"
)

// The unlock event (LOCKED → RUNNING) must carry the time the resource HAD
// been locked: consumers (billing pause accounting, audit) need it, and the
// old code cleared the field before building the event, emitting a zero.
func TestUnlockEventCarriesOriginalLockedAt(t *testing.T) {
	m := NewMachine(func() time.Time { return base.Add(48 * time.Hour) })
	inst := newInstance(StateRunning)
	inst.BillingStart = base

	lockEvt, err := m.Transition(inst, StateLocked, 0, "arrears")
	if err != nil {
		t.Fatal(err)
	}
	lockedAt := lockEvt.LockedAt
	if lockedAt.IsZero() {
		t.Fatal("lock event must stamp LockedAt")
	}

	unlockEvt, err := m.Transition(inst, StateRunning, 1, "paid")
	if err != nil {
		t.Fatal(err)
	}
	if !unlockEvt.LockedAt.Equal(lockedAt) {
		t.Fatalf("unlock event LockedAt = %v, want the original %v", unlockEvt.LockedAt, lockedAt)
	}
	// The INSTANCE's marker is cleared after recovery.
	if !inst.LockedAt.IsZero() {
		t.Fatalf("instance LockedAt should be cleared after unlock, got %v", inst.LockedAt)
	}
}
