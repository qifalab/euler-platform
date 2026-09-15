// Package accesskey implements the AK/SK lifecycle (07-security.md §2.2).
//
// Lifecycle state machine:
//
//	[*] → 创建(CreateAccessKey, max 2 per identity)
//	    → 启用(ENABLED, SK shown exactly once)
//	    → 禁用(DisableAccessKey)
//	    → 待删除(soft delete, 7-day audit retention)
//	    → 物理删除
//
//	轮转: 启用 → 轮转中(new AK active → old AK ≤72h grace) → 启用
//
// Hard rules (07§2.2):
//  1. SK is returned ONCE at creation. The platform stores only the KMS
//     envelope ciphertext and can never re-serve the plaintext.
//  2. Max 2 AKs per identity — forces rotation rather than key sprawl.
//  3. Master-account AK is disabled by default; API access is steered to RAM
//     user AKs or role temporary credentials.
//  4. STS temporary credentials are not persisted at all.
package accesskey

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/starcloud/sc-platform/kms"
)

// MaxKeysPerIdentity caps AKs per identity (07§2.2 rule 2).
const MaxKeysPerIdentity = 2

// MaxRotationGrace is the longest the superseded AK stays usable after a
// rotation before it is auto-disabled (07§2.2 rule 2).
const MaxRotationGrace = 72 * time.Hour

// SoftDeleteRetention is how long a deleted AK remains restorable and
// auditable before physical deletion (07§2.2 rule 1).
const SoftDeleteRetention = 7 * 24 * time.Hour

// AKLength is the total character length of an access key id, including the
// "SC" prefix (07§2.5).
const AKLength = 32

// Prefix is the platform access-key prefix, replacing LTAI/CPSA (07§2.5).
const Prefix = "SC"

// Status is the AK lifecycle state.
type Status int

const (
	StatusEnabled  Status = 1
	StatusDisabled Status = 2
	StatusDeleted  Status = 3 // soft delete, restorable for 7 days
)

// OwnerType distinguishes which identity kind holds the key.
type OwnerType int

const (
	OwnerMaster  OwnerType = 1 // master account (disabled by default, rule 3)
	OwnerRAMUser OwnerType = 2
	OwnerRole    OwnerType = 3
)

// Errors.
var (
	ErrTooManyKeys        = fmt.Errorf("accesskey: identity already holds %d keys (max)", MaxKeysPerIdentity)
	ErrNotFound           = errors.New("accesskey: not found")
	ErrAlreadyDeleted     = errors.New("accesskey: already deleted")
	ErrDisabled           = errors.New("accesskey: key is disabled")
	ErrRotationInProgress = errors.New("accesskey: a rotation is already in progress for this identity")
)

// Record is the persisted AK row (maps to the access_key table). Note there is
// no SK plaintext field: only the envelope ciphertext and its key version.
type Record struct {
	AK           string
	AccountID    int64
	OwnerType    OwnerType
	OwnerID      int64
	SKCipher     []byte // KMS envelope ciphertext (mk-ak)
	SKKeyVersion int    // KMS master key version, for rotation-aware cache invalidation
	Status       Status
	LastUsedAt   time.Time
	LastUsedIP   string
	RotatedAt    time.Time
	GraceUntil   time.Time // rotation grace deadline; after this the key auto-disables
	DeletedAt    time.Time
	CreatedAt    time.Time
}

// Created is what CreateAccessKey returns to the caller. SKPlaintext is the
// only time the secret is ever visible — it is not stored anywhere.
type Created struct {
	Record      Record
	SKPlaintext string
}

// Store is the persistence port. svc-iam implements it over Vitess (vtgate)
// (account_id single-key sharding); tests use an in-memory fake.
type Store interface {
	Insert(Record) error
	Get(ak string) (Record, error)
	ListByOwner(accountID int64, ownerType OwnerType, ownerID int64) ([]Record, error)
	Update(Record) error
}

// Manager implements the AK/SK lifecycle against a Store and a KMS.
type Manager struct {
	store Store
	kms   *kms.KMS
	now   func() time.Time
}

// NewManager builds a Manager. now is injectable so tests control time.
func NewManager(store Store, k *kms.KMS, now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{store: store, kms: k, now: now}
}

// Create issues a new AK/SK pair. The SK is returned once and never again.
func (m *Manager) Create(accountID int64, ownerType OwnerType, ownerID int64) (Created, error) {
	existing, err := m.store.ListByOwner(accountID, ownerType, ownerID)
	if err != nil {
		return Created{}, err
	}
	live := 0
	for _, r := range existing {
		if r.Status != StatusDeleted {
			live++
		}
	}
	if live >= MaxKeysPerIdentity {
		return Created{}, ErrTooManyKeys
	}

	ak, err := generateAK()
	if err != nil {
		return Created{}, err
	}
	sk, err := generateSK()
	if err != nil {
		return Created{}, err
	}

	// Envelope-encrypt the SK. Reversible by design — verification needs the
	// real SK to recompute HMAC (adjudication S5).
	cipherBlob, version, err := m.kms.Encrypt(kms.PurposeAK, []byte(sk))
	if err != nil {
		return Created{}, fmt.Errorf("accesskey: encrypt SK: %w", err)
	}

	rec := Record{
		AK:           ak,
		AccountID:    accountID,
		OwnerType:    ownerType,
		OwnerID:      ownerID,
		SKCipher:     cipherBlob,
		SKKeyVersion: version,
		Status:       StatusEnabled,
		CreatedAt:    m.now(),
	}
	if err := m.store.Insert(rec); err != nil {
		return Created{}, err
	}
	return Created{Record: rec, SKPlaintext: sk}, nil
}

// ResolveSK decrypts the stored SK for signature verification. The gateway
// caches the result keyed by (ciphertext, version) with a 5-minute TTL, so a
// key rotation naturally invalidates the cache (07§2.5).
//
// It refuses disabled and deleted keys, and refuses a key whose rotation grace
// window has expired.
func (m *Manager) ResolveSK(ak string, now time.Time) (string, Record, error) {
	rec, err := m.store.Get(ak)
	if err != nil {
		return "", Record{}, err
	}
	switch rec.Status {
	case StatusDeleted:
		return "", rec, ErrAlreadyDeleted
	case StatusDisabled:
		return "", rec, ErrDisabled
	}
	// A key past its rotation grace deadline is treated as disabled even if a
	// sweeper has not yet flipped the row.
	if !rec.GraceUntil.IsZero() && now.After(rec.GraceUntil) {
		return "", rec, ErrDisabled
	}
	plain, err := m.kms.Decrypt(kms.PurposeAK, rec.SKCipher, rec.SKKeyVersion)
	if err != nil {
		return "", rec, fmt.Errorf("accesskey: decrypt SK: %w", err)
	}
	return string(plain), rec, nil
}

// SetStatus enables or disables a key.
func (m *Manager) SetStatus(ak string, status Status) error {
	rec, err := m.store.Get(ak)
	if err != nil {
		return err
	}
	if rec.Status == StatusDeleted {
		return ErrAlreadyDeleted
	}
	rec.Status = status
	return m.store.Update(rec)
}

// Delete soft-deletes a key. The row is retained for 7 days for audit and
// possible restore, then physically removed by a sweeper.
func (m *Manager) Delete(ak string) error {
	rec, err := m.store.Get(ak)
	if err != nil {
		return err
	}
	if rec.Status == StatusDeleted {
		return ErrAlreadyDeleted
	}
	rec.Status = StatusDeleted
	rec.DeletedAt = m.now()
	return m.store.Update(rec)
}

// Rotate issues a replacement key and puts the old one into its grace window.
// The old AK keeps working until GraceUntil so callers can roll over without
// downtime; after that a sweeper disables it.
//
// grace is clamped to MaxRotationGrace (72h).
func (m *Manager) Rotate(ak string, grace time.Duration) (Created, error) {
	old, err := m.store.Get(ak)
	if err != nil {
		return Created{}, err
	}
	if old.Status == StatusDeleted {
		return Created{}, ErrAlreadyDeleted
	}
	if grace > MaxRotationGrace {
		grace = MaxRotationGrace
	}
	if grace < 0 {
		grace = 0
	}

	// A rotation temporarily holds cap+1 live keys (the superseded key stays
	// usable through its grace window). Without a bound, rotating repeatedly —
	// especially rotating a key that is itself mid-grace — grows the live-key
	// set without limit, defeating the 2-key cap (07§2.2 rule 2).
	nowCheck := m.now()
	// Only a key that is currently usable may be rotated. A DISABLED key, or a
	// superseded key whose grace window has closed, is already dead to the
	// gateway (ResolveSK refuses both); rotating it would reset GraceUntil and
	// revive it — and because the live-key count below treats such a key as
	// already gone, the revived key would sit outside the cap entirely.
	if old.Status == StatusDisabled {
		return Created{}, ErrDisabled
	}
	if !old.GraceUntil.IsZero() {
		if nowCheck.Before(old.GraceUntil) {
			// The key being rotated is already the superseded half of a rotation.
			return Created{}, ErrRotationInProgress
		}
		// Past its grace window: effectively disabled (07§2.2). Rotating it
		// would resurrect it; the operator must enable or delete it explicitly.
		return Created{}, ErrDisabled
	}
	siblings, err := m.store.ListByOwner(old.AccountID, old.OwnerType, old.OwnerID)
	if err != nil {
		return Created{}, err
	}
	live := 0
	for _, r := range siblings {
		if r.Status == StatusDeleted {
			continue
		}
		if !r.GraceUntil.IsZero() && nowCheck.After(r.GraceUntil) {
			continue // effectively disabled; the sweeper just has not flipped it yet
		}
		live++
	}
	// After this rotation the identity holds live+1 keys; allow at most one
	// key beyond the cap (the single in-grace superseded key).
	if live >= MaxKeysPerIdentity+1 {
		return Created{}, ErrTooManyKeys
	}

	// The replacement is created first so a failure leaves the old key intact.
	// This temporarily exceeds the 2-key cap by exactly one (bounded above);
	// the sweeper restores the invariant when the old key leaves the grace
	// window.
	newAK, err := generateAK()
	if err != nil {
		return Created{}, err
	}
	newSK, err := generateSK()
	if err != nil {
		return Created{}, err
	}
	cipherBlob, version, err := m.kms.Encrypt(kms.PurposeAK, []byte(newSK))
	if err != nil {
		return Created{}, fmt.Errorf("accesskey: encrypt SK: %w", err)
	}
	now := m.now()
	rec := Record{
		AK:           newAK,
		AccountID:    old.AccountID,
		OwnerType:    old.OwnerType,
		OwnerID:      old.OwnerID,
		SKCipher:     cipherBlob,
		SKKeyVersion: version,
		Status:       StatusEnabled,
		CreatedAt:    now,
	}
	if err := m.store.Insert(rec); err != nil {
		return Created{}, err
	}

	old.RotatedAt = now
	old.GraceUntil = now.Add(grace)
	if err := m.store.Update(old); err != nil {
		return Created{}, err
	}
	return Created{Record: rec, SKPlaintext: newSK}, nil
}

// RecordUsage stamps last-used metadata, backing the phase-1 "last used time /
// source IP" leak-inspection report (07§2.2 rule 5).
func (m *Manager) RecordUsage(ak, sourceIP string, at time.Time) error {
	rec, err := m.store.Get(ak)
	if err != nil {
		return err
	}
	rec.LastUsedAt = at
	rec.LastUsedIP = sourceIP
	return m.store.Update(rec)
}

// akAlphabet is the character set for the random portion of an AK. Ambiguous
// characters are kept because the AK is machine-copied, not hand-transcribed,
// and cloud-vendor AKs are conventionally full alphanumerics.
const akAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// generateAK returns a 32-character access key id beginning with "SC".
func generateAK() (string, error) {
	body, err := randomString(AKLength - len(Prefix))
	if err != nil {
		return "", err
	}
	return Prefix + body, nil
}

// generateSK returns a 40-character secret key, matching the length
// convention of mainstream cloud providers.
func generateSK() (string, error) {
	return randomString(40)
}

// randomString draws n characters uniformly from akAlphabet using crypto/rand
// with rejection sampling, so the distribution has no modulo bias.
func randomString(n int) (string, error) {
	const max = 256 - (256 % len(akAlphabet)) // rejection threshold
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("accesskey: random: %w", err)
		}
		for _, b := range buf {
			if int(b) >= max {
				continue // reject to keep the distribution uniform
			}
			out = append(out, akAlphabet[int(b)%len(akAlphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}
