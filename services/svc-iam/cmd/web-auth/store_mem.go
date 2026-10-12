package main

import (
	"context"
	"crypto/hmac"
	"sort"
	"sync"
	"time"
)

// memStore is the in-memory authStore (no EULER_DB_DSN). One mutex guards
// everything: every read-modify-write runs inside one critical section, which
// is the memory equivalent of the SQL store's row-locking transactions.
type memStore struct {
	mu         sync.RWMutex
	accounts   map[int64]account
	byName     map[string]int64
	sessions   map[string]refreshSession // token hash → session
	resetCodes map[int64]resetCode
	emailCodes map[string]emailCode // emailCodeKey(purpose, email hash) → code
	users      map[int64]ramUser    // by id, soft-deleted ones included
	roles      map[int64]ramRole
	policies   map[int64]storedPolicy // custom policies (system ones are built in)
	system     []storedPolicy

	accountSeq, userSeq, roleSeq, policySeq int64
	lastSweep                               time.Time
}

var _ authStore = (*memStore)(nil)

func newMemStore() *memStore {
	return &memStore{
		accounts:   map[int64]account{},
		byName:     map[string]int64{},
		sessions:   map[string]refreshSession{},
		resetCodes: map[int64]resetCode{},
		emailCodes: map[string]emailCode{},
		users:      map[int64]ramUser{},
		roles:      map[int64]ramRole{},
		policies:   map[int64]storedPolicy{},
		system:     systemPolicies(),
		// Phase-1 dev id ranges (prod allocates from account_db's id_sequence).
		accountSeq: 100123,
		policySeq:  firstCustomPolicyID - 1,
		lastSweep:  time.Now(),
	}
}

func cloneAccount(a account) account {
	a.MFASecretCipher = append([]byte(nil), a.MFASecretCipher...)
	if len(a.MFASecretCipher) == 0 {
		a.MFASecretCipher = nil
	}
	return a
}

// --- accounts ---

func (m *memStore) CreateAccount(_ context.Context, a account) (account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, taken := m.byName[a.AccountName]; taken {
		return account{}, errExists
	}
	if a.AccountID == 0 {
		for {
			m.accountSeq++
			if _, used := m.accounts[m.accountSeq]; !used {
				break
			}
		}
		a.AccountID = m.accountSeq
	} else if _, used := m.accounts[a.AccountID]; used {
		return account{}, errExists
	}
	a.CreatedAt = createdAt(a.CreatedAt)
	a = cloneAccount(a)
	m.accounts[a.AccountID] = a
	m.byName[a.AccountName] = a.AccountID
	return cloneAccount(a), nil
}

func (m *memStore) AccountByID(_ context.Context, id int64) (account, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.accounts[id]
	if !ok {
		return account{}, errNotFound
	}
	return cloneAccount(a), nil
}

func (m *memStore) AccountByName(_ context.Context, name string) (account, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byName[name]
	if !ok {
		return account{}, errNotFound
	}
	return cloneAccount(m.accounts[id]), nil
}

func (m *memStore) UpdateAccount(_ context.Context, id int64, fn func(*account) error) (account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.accounts[id]
	if !ok {
		return account{}, errNotFound
	}
	next := cloneAccount(cur)
	if err := fn(&next); err != nil {
		return account{}, err
	}
	next.AccountID, next.CreatedAt = cur.AccountID, cur.CreatedAt
	if next.AccountName != cur.AccountName {
		if _, taken := m.byName[next.AccountName]; taken {
			return account{}, errExists
		}
		delete(m.byName, cur.AccountName)
		m.byName[next.AccountName] = id
	}
	m.accounts[id] = cloneAccount(next)
	return cloneAccount(next), nil
}

// --- refresh sessions ---

func (m *memStore) CreateSession(_ context.Context, s refreshSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(time.Now())
	if _, dup := m.sessions[s.TokenHash]; dup {
		return errExists
	}
	m.sessions[s.TokenHash] = s
	return nil
}

// sweepLocked drops sessions and reset codes past their expiry, at most once
// a minute (an expired row carries no information: it cannot be exchanged,
// and reuse detection only matters while the chain could still be live).
func (m *memStore) sweepLocked(now time.Time) {
	if now.Sub(m.lastSweep) < time.Minute {
		return
	}
	m.lastSweep = now
	for h, s := range m.sessions {
		if !now.Before(s.ExpiresAt) {
			delete(m.sessions, h)
		}
	}
	for id, rc := range m.resetCodes {
		if !now.Before(rc.ExpiresAt) {
			delete(m.resetCodes, id)
		}
	}
	for k, ec := range m.emailCodes {
		if !now.Before(ec.ExpiresAt) {
			delete(m.emailCodes, k)
		}
	}
}

func (m *memStore) SessionByHash(_ context.Context, hash string) (refreshSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[hash]
	if !ok {
		return refreshSession{}, errNotFound
	}
	return s, nil
}

func (m *memStore) RotateSession(_ context.Context, oldHash string, next refreshSession, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.sessions[oldHash]
	if !ok || !old.live(now) {
		return errNotFound
	}
	if _, dup := m.sessions[next.TokenHash]; dup {
		return errExists
	}
	old.RevokedAt, old.RevokeReason, old.ReplacedBy = now, revokeRotated, next.TokenHash
	m.sessions[oldHash] = old
	m.sessions[next.TokenHash] = next
	return nil
}

func (m *memStore) RevokeSession(_ context.Context, hash, reason string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[hash]; ok && s.RevokedAt.IsZero() {
		s.RevokedAt, s.RevokeReason = now, reason
		m.sessions[hash] = s
	}
	return nil
}

func (m *memStore) RevokeSessionFamily(_ context.Context, familyID, reason string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, s := range m.sessions {
		if s.FamilyID == familyID && s.RevokedAt.IsZero() {
			s.RevokedAt, s.RevokeReason = now, reason
			m.sessions[h] = s
		}
	}
	return nil
}

func (m *memStore) RevokeAccountSessions(_ context.Context, accountID int64, reason string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokeAccountSessionsLocked(accountID, reason, now)
	return nil
}

func (m *memStore) revokeAccountSessionsLocked(accountID int64, reason string, now time.Time) {
	for h, s := range m.sessions {
		if s.AccountID == accountID && s.RevokedAt.IsZero() {
			s.RevokedAt, s.RevokeReason = now, reason
			m.sessions[h] = s
		}
	}
}

// --- password reset codes ---

func (m *memStore) PutResetCode(_ context.Context, rc resetCode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(time.Now())
	rc.Attempts = 0
	m.resetCodes[rc.AccountID] = rc
	return nil
}

func (m *memStore) ResetPassword(_ context.Context, accountID int64, codeHash, passwordHash string, now time.Time) (account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rc, ok := m.resetCodes[accountID]
	switch {
	case !ok:
		return account{}, errCodeInvalid
	case !now.Before(rc.ExpiresAt):
		delete(m.resetCodes, accountID)
		return account{}, errCodeInvalid
	case rc.Attempts >= maxResetAttempts:
		return account{}, errCodeAttempts
	case !hmac.Equal([]byte(rc.CodeHash), []byte(codeHash)):
		rc.Attempts++
		m.resetCodes[accountID] = rc
		return account{}, errCodeInvalid
	}
	a, ok := m.accounts[accountID]
	if !ok {
		return account{}, errCodeInvalid
	}
	a.PasswordHash = passwordHash
	a.TokenVersion++
	m.accounts[accountID] = a
	m.revokeAccountSessionsLocked(accountID, revokeReset, now)
	delete(m.resetCodes, accountID)
	return cloneAccount(a), nil
}

// --- email verification codes ---

// emailCodeKey is the memory key of (purpose, email hash), the SQL table's
// primary key.
func emailCodeKey(purpose, emailHash string) string { return purpose + "\x00" + emailHash }

func (m *memStore) PutEmailCode(_ context.Context, ec emailCode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(time.Now())
	ec.Attempts = 0
	m.emailCodes[emailCodeKey(ec.Purpose, ec.EmailHash)] = ec
	return nil
}

func (m *memStore) ConsumeEmailCode(_ context.Context, purpose, emailHash, codeHash string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := emailCodeKey(purpose, emailHash)
	ec, ok := m.emailCodes[key]
	switch {
	case !ok:
		return errCodeInvalid
	case !now.Before(ec.ExpiresAt):
		delete(m.emailCodes, key)
		return errCodeInvalid
	case ec.Attempts >= maxResetAttempts:
		return errCodeAttempts
	case !hmac.Equal([]byte(ec.CodeHash), []byte(codeHash)):
		ec.Attempts++
		m.emailCodes[key] = ec
		return errCodeInvalid
	}
	delete(m.emailCodes, key)
	return nil
}

// --- RAM users ---

func cloneUser(u ramUser) ramUser {
	u.Policies = append([]string{}, u.Policies...)
	return u
}

func (m *memStore) ListRAMUsers(_ context.Context, accountID int64) ([]ramUser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []ramUser{}
	for _, u := range m.users {
		if u.AccountID == accountID && u.Status != 3 {
			out = append(out, cloneUser(u))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) RAMUserByID(_ context.Context, accountID, id int64) (ramUser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok || u.AccountID != accountID {
		return ramUser{}, errNotFound
	}
	return cloneUser(u), nil
}

func (m *memStore) RAMUserByName(_ context.Context, accountID int64, username string) (ramUser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.users {
		if u.AccountID == accountID && u.Username == username && u.Status != 3 {
			return cloneUser(u), nil
		}
	}
	return ramUser{}, errNotFound
}

// resolvePoliciesLocked checks that every name is visible to the account.
func (m *memStore) resolvePoliciesLocked(accountID int64, names []string) error {
	visible := m.visiblePoliciesLocked(accountID)
	var missing []string
	for _, n := range names {
		if _, ok := visible[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return &policyNotFoundError{names: missing}
	}
	return nil
}

func (m *memStore) CreateRAMUser(_ context.Context, u ramUser) (ramUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u.Policies = dedupeNames(u.Policies)
	if err := m.resolvePoliciesLocked(u.AccountID, u.Policies); err != nil {
		return ramUser{}, err
	}
	for _, x := range m.users {
		if x.AccountID == u.AccountID && x.Username == u.Username && x.Status != 3 {
			return ramUser{}, errExists
		}
	}
	m.userSeq++
	u.ID = m.userSeq
	u.CreatedAt = createdAt(u.CreatedAt)
	m.users[u.ID] = cloneUser(u)
	return cloneUser(u), nil
}

func (m *memStore) DeleteRAMUser(_ context.Context, accountID, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok || u.AccountID != accountID {
		return errNotFound
	}
	if u.Status == 3 {
		return errAlreadyDeleted
	}
	u.Status = 3
	u.Username = deletedUsername(u.Username, u.ID)
	m.users[id] = u
	return nil
}

// --- RAM roles ---

func cloneRole(r ramRole) ramRole {
	r.Policies = append([]string{}, r.Policies...)
	return r
}

func (m *memStore) ListRAMRoles(_ context.Context, accountID int64) ([]ramRole, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []ramRole{}
	for _, r := range m.roles {
		if r.AccountID == accountID {
			out = append(out, cloneRole(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) RAMRoleByID(_ context.Context, accountID, id int64) (ramRole, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.roles[id]
	if !ok || r.AccountID != accountID {
		return ramRole{}, errNotFound
	}
	return cloneRole(r), nil
}

func (m *memStore) RAMRoleByName(_ context.Context, accountID int64, name string) (ramRole, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.roles {
		if r.AccountID == accountID && r.Name == name {
			return cloneRole(r), nil
		}
	}
	return ramRole{}, errNotFound
}

func (m *memStore) CreateRAMRole(_ context.Context, r ramRole) (ramRole, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.Policies = dedupeNames(r.Policies)
	if err := m.resolvePoliciesLocked(r.AccountID, r.Policies); err != nil {
		return ramRole{}, err
	}
	for _, x := range m.roles {
		if x.AccountID == r.AccountID && x.Name == r.Name {
			return ramRole{}, errExists
		}
	}
	m.roleSeq++
	r.ID = m.roleSeq
	if r.Status == 0 {
		r.Status = 1
	}
	r.CreatedAt = createdAt(r.CreatedAt)
	m.roles[r.ID] = cloneRole(r)
	return cloneRole(r), nil
}

// --- policies ---

func (m *memStore) visiblePoliciesLocked(accountID int64) map[string]storedPolicy {
	out := make(map[string]storedPolicy, len(m.system))
	for _, p := range m.system {
		out[p.Name] = p
	}
	for _, p := range m.policies {
		if p.AccountID == accountID {
			out[p.Name] = p
		}
	}
	return out
}

func (m *memStore) ListPolicies(_ context.Context, accountID int64) ([]storedPolicy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]storedPolicy{}, m.system...)
	for _, p := range m.policies {
		if p.AccountID == accountID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) PoliciesByName(_ context.Context, accountID int64, names []string) (map[string]storedPolicy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	visible := m.visiblePoliciesLocked(accountID)
	out := make(map[string]storedPolicy, len(names))
	for _, n := range names {
		if p, ok := visible[n]; ok {
			out[n] = p
		}
	}
	return out, nil
}

func (m *memStore) CreatePolicy(_ context.Context, p storedPolicy) (storedPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, taken := m.visiblePoliciesLocked(p.AccountID)[p.Name]; taken {
		return storedPolicy{}, errExists
	}
	m.policySeq++
	p.ID = m.policySeq
	p.Type = "custom"
	p.CreatedAt = createdAt(p.CreatedAt)
	m.policies[p.ID] = p
	return p, nil
}
