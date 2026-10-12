package accesskey

import (
	"fmt"
	"sort"
	"sync"
)

// MemStore is the in-memory Store: the development / test half of the port,
// for a process that runs without EULER_DB_DSN. It mirrors SQLStore's
// observable behaviour so a caller cannot tell the two apart — a duplicate AK
// is refused rather than overwritten, Update of a missing row is ErrNotFound,
// and ListByOwner returns the owner's keys oldest first.
//
// Nothing it holds survives the process, which is exactly why production
// runs on SQLStore: a credential store that forgets a key has deleted it.
type MemStore struct {
	mu   sync.RWMutex
	rows map[string]Record
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{rows: make(map[string]Record)} }

func (m *MemStore) Insert(r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.rows[r.AK]; dup {
		return fmt.Errorf("accesskey: insert %s: duplicate ak", r.AK)
	}
	m.rows[r.AK] = cloneRecord(r)
	return nil
}

func (m *MemStore) Get(ak string) (Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.rows[ak]
	if !ok {
		return Record{}, ErrNotFound
	}
	return cloneRecord(r), nil
}

func (m *MemStore) ListByOwner(accountID int64, ownerType OwnerType, ownerID int64) ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Record, 0, 2)
	for _, r := range m.rows {
		if r.AccountID == accountID && r.OwnerType == ownerType && r.OwnerID == ownerID {
			out = append(out, cloneRecord(r))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].AK < out[j].AK
	})
	return out, nil
}

func (m *MemStore) Update(r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[r.AK]; !ok {
		return ErrNotFound
	}
	m.rows[r.AK] = cloneRecord(r)
	return nil
}

// cloneRecord detaches the ciphertext slice so a caller mutating its copy
// cannot reach into the store.
func cloneRecord(r Record) Record {
	r.SKCipher = append([]byte(nil), r.SKCipher...)
	return r
}
