package accesskey

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/kms"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

// Regression (I9): re-enabling a superseded key kept its expired GraceUntil,
// so the "enabled" key was still refused by ResolveSK.
func TestSetStatusEnableClearsRotationGrace(t *testing.T) {
	now := baseTime
	k := kms.New()
	if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	m := NewManager(store, k, func() time.Time { return now })

	a, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	rot, err := m.Rotate(a.Record.AK, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now = baseTime.Add(2 * time.Hour) // the old key's grace window closed
	if _, _, err := m.ResolveSK(a.Record.AK, now); !errors.Is(err, ErrDisabled) {
		t.Fatalf("old key past grace: %v, want ErrDisabled", err)
	}
	// Make room under the cap, then explicitly re-enable the old key.
	if err := m.SetStatus(rot.Record.AK, StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if err := m.SetStatus(a.Record.AK, StatusEnabled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ResolveSK(a.Record.AK, now); err != nil {
		t.Fatalf("re-enabled key still refused: %v", err)
	}
	rec := store.rows[a.Record.AK]
	if !rec.GraceUntil.IsZero() || !rec.RotatedAt.IsZero() {
		t.Fatalf("rotation state not cleared: grace=%v rotated=%v", rec.GraceUntil, rec.RotatedAt)
	}
}

// SetStatus must not be a back door around Delete (which stamps DeletedAt for
// the retention sweep) nor accept arbitrary status values.
func TestSetStatusRejectsDeletedAndUnknown(t *testing.T) {
	m, store := newTestManager(t, baseTime)
	a, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []Status{StatusDeleted, 0, 9} {
		if err := m.SetStatus(a.Record.AK, st); !errors.Is(err, ErrInvalidStatus) {
			t.Fatalf("SetStatus(%d) = %v, want ErrInvalidStatus", st, err)
		}
	}
	if store.rows[a.Record.AK].Status != StatusEnabled {
		t.Fatal("refused SetStatus changed the key")
	}
}

// Re-enabling clears the grace deadline, so the key no longer leaves on its
// own: it must fit under the live-key cap.
func TestSetStatusEnableRespectsCap(t *testing.T) {
	m, _ := newTestManager(t, baseTime)
	a, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(100123, OwnerRAMUser, 555); err != nil {
		t.Fatal(err)
	}
	// a (in grace) + replacement + b = three live keys during the rotation.
	if _, err := m.Rotate(a.Record.AK, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := m.SetStatus(a.Record.AK, StatusEnabled); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("enabling the superseded key over the cap: %v, want ErrTooManyKeys", err)
	}
}

// Regression (I8): MySQL reports 0 affected rows for an UPDATE that writes the
// values already stored; that is a successful no-op (as in the memory store),
// not ErrVersionConflict. A row that is really gone is ErrNotFound.
func TestSQLStoreNoOpUpdateSucceeds(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-iam"))
	ctx := context.Background()
	k := kms.New()
	if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(ctx, db)
	mgr := NewManager(store, k, func() time.Time { return testClock })

	created, err := mgr.Create(100123, OwnerRAMUser, 31)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := store.Get(created.Record.AK)
	if err != nil {
		t.Fatal(err)
	}
	// Identical write: 0 rows changed.
	if err := store.Update(rec); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	// Same-second usage stamp twice (the second write changes nothing).
	if err := mgr.RecordUsage(created.Record.AK, "10.0.0.1", testClock); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RecordUsage(created.Record.AK, "10.0.0.1", testClock); err != nil {
		t.Fatalf("repeated usage stamp: %v", err)
	}
	// Disabling twice is idempotent.
	if err := mgr.SetStatus(created.Record.AK, StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetStatus(created.Record.AK, StatusDisabled); err != nil {
		t.Fatalf("repeated disable: %v", err)
	}
	// A row that does not exist is ErrNotFound.
	missing := rec
	missing.AK = "EUmissing0000000000000000000000x"
	if err := store.Update(missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing row: %v, want ErrNotFound", err)
	}
}
