package accesskey

import (
	"errors"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/kms"
)

// MemStore must behave like SQLStore (mysql_test.go runs the same lifecycle
// against the schema of record): duplicate ids are refused, updates of a
// missing row fail, owners are isolated and listing is oldest first.
func TestMemStoreMirrorsSQLStore(t *testing.T) {
	s := NewMemStore()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a := Record{AK: "EUb", AccountID: 7, OwnerType: OwnerMaster, SKCipher: []byte{1, 2}, SKKeyVersion: 1, Status: StatusEnabled, CreatedAt: t0.Add(time.Second)}
	b := Record{AK: "EUa", AccountID: 7, OwnerType: OwnerMaster, SKCipher: []byte{3}, SKKeyVersion: 1, Status: StatusEnabled, CreatedAt: t0}
	u := Record{AK: "EUu", AccountID: 7, OwnerType: OwnerRAMUser, OwnerID: 9, SKCipher: []byte{4}, SKKeyVersion: 1, Status: StatusEnabled, CreatedAt: t0}
	for _, r := range []Record{a, b, u} {
		if err := s.Insert(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Insert(a); err == nil {
		t.Fatal("duplicate ak overwritten")
	}
	got, err := s.ListByOwner(7, OwnerMaster, 0)
	if err != nil || len(got) != 2 || got[0].AK != "EUa" || got[1].AK != "EUb" {
		t.Fatalf("master keys oldest first: %+v %v", got, err)
	}
	if got, _ := s.ListByOwner(7, OwnerRAMUser, 9); len(got) != 1 || got[0].AK != "EUu" {
		t.Fatalf("ram-user keys: %+v", got)
	}
	got[0].SKCipher[0] = 0xff // must not reach into the store
	if r, _ := s.Get("EUu"); r.SKCipher[0] != 4 {
		t.Fatal("store shares its ciphertext with callers")
	}
	if err := s.Update(Record{AK: "EUmissing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing row: %v", err)
	}
	if _, err := s.Get("EUmissing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get of a missing row: %v", err)
	}

	// The Manager's lifecycle runs unchanged on top of it.
	k := kms.New()
	if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
		t.Fatal(err)
	}
	m := NewManager(NewMemStore(), k, func() time.Time { return t0 })
	c, err := m.Create(1, OwnerRAMUser, 2)
	if err != nil {
		t.Fatal(err)
	}
	sk, rec, err := m.ResolveSK(c.Record.AK, t0)
	if err != nil || sk != c.SKPlaintext || rec.OwnerID != 2 {
		t.Fatalf("resolve: %q %+v %v", sk, rec, err)
	}
}
