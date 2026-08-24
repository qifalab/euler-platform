package accesskey

import (
	"errors"
	"testing"
	"time"
)

// Rotating a key that is itself mid-grace (the superseded half of a rotation)
// must be rejected: each such rotation would mint another live key, growing
// the live-key set without bound.
func TestRotateRejectsKeyAlreadyInGrace(t *testing.T) {
	m, _ := newTestManager(t, baseTime)
	created, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rotate(created.Record.AK, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	// The OLD key is now in its grace window; rotating it again must fail.
	if _, err := m.Rotate(created.Record.AK, 24*time.Hour); !errors.Is(err, ErrRotationInProgress) {
		t.Fatalf("err = %v, want ErrRotationInProgress", err)
	}
}

// The identity may hold at most cap+1 live keys during a rotation. With two
// live keys plus one in grace, a further rotation must be refused until the
// grace window closes.
func TestRotateEnforcesLiveKeyCeiling(t *testing.T) {
	m, _ := newTestManager(t, baseTime)
	a, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	// Rotate a: now a(grace) + a'(new) + b = 3 live keys — at the ceiling.
	if _, err := m.Rotate(a.Record.AK, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	// Rotating b would mint a 4th live key; must be refused.
	if _, err := m.Rotate(b.Record.AK, 24*time.Hour); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("err = %v, want ErrTooManyKeys", err)
	}
}

// The grace-expiry path re-enables rotation once the superseded key drops out
// of the live count.
func TestRotateAllowedAfterGraceExpires(t *testing.T) {
	m, store := newTestManager(t, baseTime)
	a, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rotate(a.Record.AK, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Move the clock past a's grace deadline with a manager over the same store.
	m2 := NewManager(store, m.kms, func() time.Time { return baseTime.Add(2 * time.Hour) })
	if _, err := m2.Rotate(b.Record.AK, time.Hour); err != nil {
		t.Fatalf("rotation after grace expiry should succeed: %v", err)
	}
}
