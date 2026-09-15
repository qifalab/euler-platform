package accesskey

import (
	"errors"
	"testing"
	"time"
)

// Once a superseded key's grace window has closed it is dead to the gateway
// (ResolveSK refuses it). Rotating it used to reset GraceUntil and revive it —
// and because the live-key count treated such a key as already gone, the
// revived key sat outside the 2-key cap entirely.
func TestRotateRefusesKeyPastItsGraceWindow(t *testing.T) {
	m, store := newTestManager(t, baseTime)
	a, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rotate(a.Record.AK, time.Hour); err != nil {
		t.Fatalf("first rotation: %v", err)
	}

	later := baseTime.Add(2 * time.Hour) // past the one-hour grace window
	m2 := NewManager(store, m.kms, func() time.Time { return later })
	if _, err := m2.Rotate(a.Record.AK, time.Hour); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	// The refused call must not have revived the key.
	if _, _, err := m2.ResolveSK(a.Record.AK, later); !errors.Is(err, ErrDisabled) {
		t.Fatalf("key resolved after the refused rotation: %v", err)
	}
}

// Rotating an explicitly disabled key is refused for the same reason.
func TestRotateRefusesDisabledKey(t *testing.T) {
	m, _ := newTestManager(t, baseTime)
	a, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetStatus(a.Record.AK, StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rotate(a.Record.AK, time.Hour); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
}
