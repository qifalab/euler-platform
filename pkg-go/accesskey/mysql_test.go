package accesskey

import (
	"context"
	"testing"
	"time"

	"github.com/starcloud/sc-platform/kms"
	"github.com/starcloud/sc-platform/storage/sqltest"
)

func newSQLTestStore(t *testing.T) *SQLStore {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-iam"))
	return NewSQLStore(context.Background(), db)
}

var testClock = time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

// TestSQLStoreLifecycle runs the full credential lifecycle through the Manager
// on a real schema: create (SK shown once), resolve, rotate (grace window),
// disable, soft delete — then verifies the stored row survives a fresh store,
// which is the whole point: a restarted verifier must still resolve live keys.
func TestSQLStoreLifecycle(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-iam"))
	ctx := context.Background()

	k := kms.New()
	if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(NewSQLStore(ctx, db), k, func() time.Time { return testClock })

	created, err := mgr.Create(100123, OwnerRAMUser, 7)
	if err != nil {
		t.Fatal(err)
	}
	if created.SKPlaintext == "" {
		t.Fatal("SK plaintext not returned at creation")
	}

	// Resolve before and after rotation; the old key stays usable inside the
	// grace window and the replacement works immediately.
	if _, _, err := mgr.ResolveSK(created.Record.AK, testClock); err != nil {
		t.Fatalf("resolve new key: %v", err)
	}
	rot, err := mgr.Rotate(created.Record.AK, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.ResolveSK(created.Record.AK, testClock); err != nil {
		t.Fatalf("old key inside grace window: %v", err)
	}
	if _, _, err := mgr.ResolveSK(rot.Record.AK, testClock); err != nil {
		t.Fatalf("replacement key: %v", err)
	}
	// Past the grace deadline the old key is dead to the gateway.
	if _, _, err := mgr.ResolveSK(created.Record.AK, testClock.Add(2*time.Hour)); err == nil {
		t.Fatal("old key resolved past its grace window")
	}

	// Disable + soft delete on the replacement.
	if err := mgr.SetStatus(rot.Record.AK, StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.ResolveSK(rot.Record.AK, testClock); err == nil {
		t.Fatal("disabled key resolved")
	}
	if err := mgr.Delete(rot.Record.AK); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.ResolveSK(rot.Record.AK, testClock); err == nil {
		t.Fatal("deleted key resolved")
	}

	// A fresh store (restarted process) still sees the live key with its grace
	// deadline intact.
	restarted := NewSQLStore(ctx, db)
	old, err := restarted.Get(created.Record.AK)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != StatusEnabled || old.GraceUntil.IsZero() {
		t.Fatalf("restarted row = status %d grace %v", old.Status, old.GraceUntil)
	}
	if len(old.SKCipher) == 0 {
		t.Fatal("SK cipher did not survive the round trip")
	}
}

// TestSQLStoreMaxKeysPerIdentity pins the cap against the real unique keys:
// the third Create for one identity must be refused even though every insert
// would succeed row-wise.
func TestSQLStoreMaxKeysPerIdentity(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-iam"))
	ctx := context.Background()
	k := kms.New()
	if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(NewSQLStore(ctx, db), k, func() time.Time { return testClock })

	for i := 0; i < MaxKeysPerIdentity; i++ {
		if _, err := mgr.Create(100123, OwnerRAMUser, 8); err != nil {
			t.Fatalf("create #%d: %v", i+1, err)
		}
	}
	if _, err := mgr.Create(100123, OwnerRAMUser, 8); err != ErrTooManyKeys {
		t.Fatalf("third key: %v, want ErrTooManyKeys", err)
	}
	// Another identity is unaffected.
	if _, err := mgr.Create(100123, OwnerRAMUser, 9); err != nil {
		t.Fatalf("other identity: %v", err)
	}
}

func TestSQLStoreNotFoundAndMasterNullOwner(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-iam"))
	ctx := context.Background()
	k := kms.New()
	if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(NewSQLStore(ctx, db), k, func() time.Time { return testClock })

	if _, err := NewSQLStore(ctx, db).Get("SCnonexistent00000000000000000000"); err != ErrNotFound {
		t.Fatalf("get unknown ak: %v, want ErrNotFound", err)
	}

	// A master-account key stores owner_id NULL; ListByOwner with ownerID 0
	// must find it (the mem store matches OwnerID==0, the schema stores NULL).
	if _, err := mgr.Create(100123, OwnerMaster, 0); err != nil {
		t.Fatal(err)
	}
	keys, err := NewSQLStore(ctx, db).ListByOwner(100123, OwnerMaster, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("master keys = %d, want 1", len(keys))
	}
}
