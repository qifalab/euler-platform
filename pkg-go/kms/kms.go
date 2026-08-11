// Package kms implements the platform's envelope-encryption scheme
// (07-security.md §5.3), the storage substrate for every sensitive field:
// mobile numbers, ID documents, bank cards, real-name materials, TOTP seeds,
// and — critically — AK secret keys.
//
// Key hierarchy:
//
//	RootKey    (never exported; dedicated encrypted volume + boot-passphrase sharding)
//	  └─ MasterKey  (per purpose: mk-user, mk-ak, mk-db)
//	       └─ DEK   (DataKey, random per business object; plaintext only in memory)
//	            └─ ciphertext
//
// Business code never sees a MasterKey. It calls GenerateDataKey to get a
// (plaintext DEK, encrypted DEK) pair, encrypts with the plaintext DEK, stores
// only the encrypted DEK alongside the ciphertext, and discards the plaintext.
//
// Why SK storage is reversible (裁决 S5, overturning 03§6.1's sk_hash):
// signature verification must recompute HMAC-SHA256 with the real SK, so a
// one-way hash cannot work. SK is therefore stored as an envelope ciphertext
// (sk_cipher + sk_key_version) and decrypted into a short-lived gateway cache
// keyed by ciphertext+version, invalidated on key rotation (07§2.2/§2.5).
//
// Phase-1 decision D7: self-built software KMS (svc-kms), no hardware HSM.
// Change condition: 等保三级 assessor requirement or a financial customer —
// then the RootKey migrates to an HSM with the KMS interface unchanged.
package kms

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Purpose identifies a MasterKey by its use. Separating purposes limits the
// blast radius of any single key compromise (07§5.3).
type Purpose string

const (
	// PurposeUser protects user-sensitive fields: mobile, ID, bank card,
	// real-name materials, TOTP seeds.
	PurposeUser Purpose = "mk-user"
	// PurposeAK protects AK secret keys.
	PurposeAK Purpose = "mk-ak"
	// PurposeDB protects storage-level encryption keys.
	PurposeDB Purpose = "mk-db"
)

// Errors.
var (
	ErrUnknownPurpose   = errors.New("kms: unknown master key purpose")
	ErrUnknownVersion   = errors.New("kms: unknown master key version")
	ErrCiphertextShort  = errors.New("kms: ciphertext too short")
	ErrDecryptFailed    = errors.New("kms: decrypt failed (wrong key or tampered ciphertext)")
)

// DataKey is the result of GenerateDataKey. Plaintext must be zeroed by the
// caller as soon as the encryption operation completes; only Encrypted is
// persisted.
type DataKey struct {
	Plaintext []byte // 32 bytes (AES-256); caller must not persist this
	Encrypted []byte // envelope: master-key-encrypted DEK; safe to persist
	Version   int    // master key version used, stored alongside as sk_key_version
}

// masterKey is one version of a purpose's master key.
type masterKey struct {
	version int
	key     []byte // 32 bytes
}

// KMS is the software key-management service. In production this is svc-kms
// (Go/Kratos, per the service registry) exposing Encrypt/Decrypt/GenerateDataKey
// over gRPC; this Go implementation is the shared reference used by Go-side
// services and by tests.
//
// MasterKeys rotate annually; old versions are retained so legacy ciphertext
// stays decryptable, and re-encryption happens via a background batch job.
type KMS struct {
	mu sync.RWMutex
	// masters maps purpose → all versions, newest last.
	masters map[Purpose][]masterKey
}

// New creates an empty KMS. Callers seed it with master keys; in production
// the RootKey unwraps them at boot from the encrypted volume.
func New() *KMS {
	return &KMS{masters: make(map[Purpose][]masterKey)}
}

// AddMasterKey registers a master key version for a purpose. The highest
// version is the "current" key used for new encryptions; older versions remain
// available for decryption of existing ciphertext.
func (k *KMS) AddMasterKey(p Purpose, version int, key []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("kms: master key must be 32 bytes, got %d", len(key))
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.masters[p] = append(k.masters[p], masterKey{version: version, key: append([]byte(nil), key...)})
	return nil
}

// GenerateMasterKey creates and registers a random master key version. Used at
// bootstrap and on annual rotation.
func (k *KMS) GenerateMasterKey(p Purpose, version int) error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("kms: generate master key: %w", err)
	}
	return k.AddMasterKey(p, version, key)
}

// currentMaster returns the newest master key for a purpose.
func (k *KMS) currentMaster(p Purpose) (masterKey, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	versions, ok := k.masters[p]
	if !ok || len(versions) == 0 {
		return masterKey{}, ErrUnknownPurpose
	}
	newest := versions[0]
	for _, v := range versions[1:] {
		if v.version > newest.version {
			newest = v
		}
	}
	return newest, nil
}

// masterByVersion looks up a specific master key version.
func (k *KMS) masterByVersion(p Purpose, version int) (masterKey, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	for _, v := range k.masters[p] {
		if v.version == version {
			return v, nil
		}
	}
	return masterKey{}, ErrUnknownVersion
}

// GenerateDataKey returns a fresh random DEK together with its master-key
// encrypted form. This is the primitive business code uses: encrypt with
// Plaintext, persist Encrypted + Version, discard Plaintext.
func (k *KMS) GenerateDataKey(p Purpose) (DataKey, error) {
	master, err := k.currentMaster(p)
	if err != nil {
		return DataKey{}, err
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return DataKey{}, fmt.Errorf("kms: generate DEK: %w", err)
	}
	encrypted, err := aesGCMEncrypt(master.key, dek)
	if err != nil {
		return DataKey{}, err
	}
	return DataKey{Plaintext: dek, Encrypted: encrypted, Version: master.version}, nil
}

// DecryptDataKey unwraps an encrypted DEK using the master key version it was
// wrapped with. svc-kms caches the plaintext DEK for at most 60s (07§5.3).
func (k *KMS) DecryptDataKey(p Purpose, encrypted []byte, version int) ([]byte, error) {
	master, err := k.masterByVersion(p, version)
	if err != nil {
		return nil, err
	}
	return aesGCMDecrypt(master.key, encrypted)
}

// Encrypt performs a full envelope encryption of plaintext in one call: it
// generates a DEK, encrypts the data, and returns a self-contained blob
// holding both the wrapped DEK and the ciphertext, plus the master key
// version to persist as sk_key_version.
//
// Blob layout: [2-byte DEK-blob length][wrapped DEK][ciphertext]
func (k *KMS) Encrypt(p Purpose, plaintext []byte) (blob []byte, version int, err error) {
	dk, err := k.GenerateDataKey(p)
	if err != nil {
		return nil, 0, err
	}
	defer zero(dk.Plaintext)

	ciphertext, err := aesGCMEncrypt(dk.Plaintext, plaintext)
	if err != nil {
		return nil, 0, err
	}
	if len(dk.Encrypted) > 0xFFFF {
		return nil, 0, errors.New("kms: wrapped DEK too large")
	}
	out := make([]byte, 2+len(dk.Encrypted)+len(ciphertext))
	out[0] = byte(len(dk.Encrypted) >> 8)
	out[1] = byte(len(dk.Encrypted))
	copy(out[2:], dk.Encrypted)
	copy(out[2+len(dk.Encrypted):], ciphertext)
	return out, dk.Version, nil
}

// Decrypt reverses Encrypt using the recorded master key version.
func (k *KMS) Decrypt(p Purpose, blob []byte, version int) ([]byte, error) {
	if len(blob) < 2 {
		return nil, ErrCiphertextShort
	}
	dekLen := int(blob[0])<<8 | int(blob[1])
	if len(blob) < 2+dekLen {
		return nil, ErrCiphertextShort
	}
	wrappedDEK := blob[2 : 2+dekLen]
	ciphertext := blob[2+dekLen:]

	dek, err := k.DecryptDataKey(p, wrappedDEK, version)
	if err != nil {
		return nil, err
	}
	defer zero(dek)

	return aesGCMDecrypt(dek, ciphertext)
}

// aesGCMEncrypt encrypts with AES-256-GCM, prefixing the random nonce.
func aesGCMEncrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("kms: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("kms: new GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("kms: nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// aesGCMDecrypt reverses aesGCMEncrypt. GCM authenticates, so any tampering
// surfaces as a decrypt failure rather than silent corruption.
func aesGCMDecrypt(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("kms: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("kms: new GCM: %w", err)
	}
	if len(data) < gcm.NonceSize() {
		return nil, ErrCiphertextShort
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	out, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	return out, nil
}

// zero overwrites a key buffer. Go's GC may still hold copies, so this is
// best-effort defence, not a guarantee — but it shortens the window in which
// plaintext key material sits in reusable memory.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
