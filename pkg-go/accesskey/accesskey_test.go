package accesskey

import (
	"strings"
	"testing"
	"time"

	"github.com/starcloud/sc-platform/kms"
)

// memStore is an in-memory Store for tests. Production uses Vitess (vtgate)
// with account_id single-key sharding.
type memStore struct {
	rows map[string]Record
}

func newMemStore() *memStore { return &memStore{rows: make(map[string]Record)} }

func (m *memStore) Insert(r Record) error {
	m.rows[r.AK] = r
	return nil
}

func (m *memStore) Get(ak string) (Record, error) {
	r, ok := m.rows[ak]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}

func (m *memStore) ListByOwner(accountID int64, ownerType OwnerType, ownerID int64) ([]Record, error) {
	var out []Record
	for _, r := range m.rows {
		if r.AccountID == accountID && r.OwnerType == ownerType && r.OwnerID == ownerID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) Update(r Record) error {
	if _, ok := m.rows[r.AK]; !ok {
		return ErrNotFound
	}
	m.rows[r.AK] = r
	return nil
}

func newTestManager(t *testing.T, now time.Time) (*Manager, *memStore) {
	t.Helper()
	k := kms.New()
	if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
		t.Fatalf("seed KMS: %v", err)
	}
	store := newMemStore()
	return NewManager(store, k, func() time.Time { return now }), store
}

var baseTime = time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)

func TestCreateReturnsSKOnceAndStoresCiphertext(t *testing.T) {
	m, store := newTestManager(t, baseTime)

	created, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// AK format: 32 chars, SC prefix (07§2.5).
	if len(created.Record.AK) != AKLength {
		t.Fatalf("AK length = %d, want %d", len(created.Record.AK), AKLength)
	}
	if !strings.HasPrefix(created.Record.AK, Prefix) {
		t.Fatalf("AK %q must start with %q", created.Record.AK, Prefix)
	}
	if created.SKPlaintext == "" {
		t.Fatal("SK must be returned at creation")
	}
	// The stored row must NOT contain the SK plaintext anywhere.
	stored, err := store.Get(created.Record.AK)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored.SKCipher), created.SKPlaintext) {
		t.Fatal("stored ciphertext leaks the SK plaintext")
	}
	if stored.SKKeyVersion != 1 {
		t.Fatalf("SKKeyVersion = %d, want 1", stored.SKKeyVersion)
	}
	if stored.Status != StatusEnabled {
		t.Fatalf("new key status = %d, want enabled", stored.Status)
	}
}

func TestResolveSKRecoversPlaintextForHMAC(t *testing.T) {
	// The gateway verification path: load the row, decrypt sk_cipher, and use
	// the real SK to recompute the CPS1 HMAC (adjudication S5).
	m, _ := newTestManager(t, baseTime)
	created, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := m.ResolveSK(created.Record.AK, baseTime)
	if err != nil {
		t.Fatalf("ResolveSK: %v", err)
	}
	if got != created.SKPlaintext {
		t.Fatal("resolved SK must equal the SK handed out at creation")
	}
}

func TestMaxTwoKeysPerIdentity(t *testing.T) {
	m, _ := newTestManager(t, baseTime)
	if _, err := m.Create(100123, OwnerRAMUser, 555); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(100123, OwnerRAMUser, 555); err != nil {
		t.Fatal(err)
	}
	// Third key must be rejected (07§2.2 rule 2).
	if _, err := m.Create(100123, OwnerRAMUser, 555); err != ErrTooManyKeys {
		t.Fatalf("expected ErrTooManyKeys, got %v", err)
	}
	// A different identity is unaffected.
	if _, err := m.Create(100123, OwnerRAMUser, 666); err != nil {
		t.Fatalf("different owner should be allowed: %v", err)
	}
}

func TestDeletedKeyFreesASlot(t *testing.T) {
	m, _ := newTestManager(t, baseTime)
	a, _ := m.Create(100123, OwnerRAMUser, 555)
	if _, err := m.Create(100123, OwnerRAMUser, 555); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(a.Record.AK); err != nil {
		t.Fatal(err)
	}
	// With one soft-deleted, a new key fits again.
	if _, err := m.Create(100123, OwnerRAMUser, 555); err != nil {
		t.Fatalf("slot should be free after delete: %v", err)
	}
}

func TestResolveSKRefusesDisabledAndDeleted(t *testing.T) {
	m, _ := newTestManager(t, baseTime)
	created, _ := m.Create(100123, OwnerRAMUser, 555)

	if err := m.SetStatus(created.Record.AK, StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ResolveSK(created.Record.AK, baseTime); err != ErrDisabled {
		t.Fatalf("disabled key: expected ErrDisabled, got %v", err)
	}

	if err := m.SetStatus(created.Record.AK, StatusEnabled); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(created.Record.AK); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ResolveSK(created.Record.AK, baseTime); err != ErrAlreadyDeleted {
		t.Fatalf("deleted key: expected ErrAlreadyDeleted, got %v", err)
	}
}

func TestRotateKeepsOldKeyUsableDuringGrace(t *testing.T) {
	m, _ := newTestManager(t, baseTime)
	old, err := m.Create(100123, OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}

	fresh, err := m.Rotate(old.Record.AK, 24*time.Hour)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if fresh.Record.AK == old.Record.AK {
		t.Fatal("rotation must issue a new AK")
	}
	if fresh.SKPlaintext == old.SKPlaintext {
		t.Fatal("rotation must issue a new SK")
	}

	// Inside the grace window the old key still verifies.
	if _, _, err := m.ResolveSK(old.Record.AK, baseTime.Add(12*time.Hour)); err != nil {
		t.Fatalf("old key must work during grace: %v", err)
	}
	// After the grace window it is refused.
	if _, _, err := m.ResolveSK(old.Record.AK, baseTime.Add(25*time.Hour)); err != ErrDisabled {
		t.Fatalf("old key after grace: expected ErrDisabled, got %v", err)
	}
	// The new key works throughout.
	if _, _, err := m.ResolveSK(fresh.Record.AK, baseTime.Add(25*time.Hour)); err != nil {
		t.Fatalf("new key must work: %v", err)
	}
}

func TestRotateClampsGraceTo72h(t *testing.T) {
	m, store := newTestManager(t, baseTime)
	old, _ := m.Create(100123, OwnerRAMUser, 555)

	if _, err := m.Rotate(old.Record.AK, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Get(old.Record.AK)
	if err != nil {
		t.Fatal(err)
	}
	want := baseTime.Add(MaxRotationGrace)
	if !rec.GraceUntil.Equal(want) {
		t.Fatalf("GraceUntil = %v, want clamped to %v", rec.GraceUntil, want)
	}
}

func TestRecordUsageStampsLeakInspectionFields(t *testing.T) {
	m, store := newTestManager(t, baseTime)
	created, _ := m.Create(100123, OwnerRAMUser, 555)

	useTime := baseTime.Add(time.Hour)
	if err := m.RecordUsage(created.Record.AK, "203.0.113.9", useTime); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.Get(created.Record.AK)
	if !rec.LastUsedAt.Equal(useTime) || rec.LastUsedIP != "203.0.113.9" {
		t.Fatalf("usage not recorded: %+v", rec)
	}
}

func TestAKsAreUnique(t *testing.T) {
	m, _ := newTestManager(t, baseTime)
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		// Fresh owner each time to stay under the 2-key cap.
		c, err := m.Create(100123, OwnerRAMUser, int64(i))
		if err != nil {
			t.Fatal(err)
		}
		if seen[c.Record.AK] {
			t.Fatalf("duplicate AK generated: %s", c.Record.AK)
		}
		seen[c.Record.AK] = true
	}
}

func TestNotFound(t *testing.T) {
	m, _ := newTestManager(t, baseTime)
	if _, _, err := m.ResolveSK("SCnonexistent", baseTime); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
