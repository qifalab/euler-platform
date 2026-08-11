package kms

import (
	"bytes"
	"testing"
)

func newSeededKMS(t *testing.T) *KMS {
	t.Helper()
	k := New()
	for _, p := range []Purpose{PurposeUser, PurposeAK, PurposeDB} {
		if err := k.GenerateMasterKey(p, 1); err != nil {
			t.Fatalf("GenerateMasterKey(%s): %v", p, err)
		}
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	k := newSeededKMS(t)
	secret := []byte("this-is-a-secret-key-value-0123456789")

	blob, version, err := k.Encrypt(PurposeAK, secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}
	// The ciphertext must not contain the plaintext.
	if bytes.Contains(blob, secret) {
		t.Fatal("ciphertext leaks plaintext")
	}

	got, err := k.Decrypt(PurposeAK, blob, version)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, secret)
	}
}

func TestSKReversibleForHMACVerification(t *testing.T) {
	// The core reason SK storage is envelope encryption, not a hash
	// (adjudication S5): the gateway must recover the real SK to recompute
	// HMAC during signature verification.
	k := newSeededKMS(t)
	sk := []byte("SK-secret-material-for-hmac")

	skCipher, skKeyVersion, err := k.Encrypt(PurposeAK, sk)
	if err != nil {
		t.Fatalf("Encrypt SK: %v", err)
	}
	// Simulate the gateway verification path: load sk_cipher + sk_key_version
	// from access_key, decrypt, use for HMAC.
	recovered, err := k.Decrypt(PurposeAK, skCipher, skKeyVersion)
	if err != nil {
		t.Fatalf("Decrypt SK: %v", err)
	}
	if !bytes.Equal(recovered, sk) {
		t.Fatal("SK must round-trip exactly for HMAC recomputation")
	}
}

func TestTamperedCiphertextFails(t *testing.T) {
	k := newSeededKMS(t)
	blob, version, err := k.Encrypt(PurposeUser, []byte("13800138000"))
	if err != nil {
		t.Fatal(err)
	}
	// Flip a bit in the ciphertext body; GCM authentication must catch it.
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := k.Decrypt(PurposeUser, tampered, version); err == nil {
		t.Fatal("tampered ciphertext must fail to decrypt")
	}
}

func TestPurposeIsolation(t *testing.T) {
	// A blob encrypted under mk-ak must not decrypt under mk-user — this is
	// what bounds the blast radius of a single key compromise.
	k := newSeededKMS(t)
	blob, version, err := k.Encrypt(PurposeAK, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Decrypt(PurposeUser, blob, version); err == nil {
		t.Fatal("cross-purpose decryption must fail")
	}
}

func TestKeyRotationKeepsOldCiphertextReadable(t *testing.T) {
	// MasterKeys rotate annually; old versions are retained so existing
	// ciphertext stays decryptable while a batch job re-encrypts (07§5.3).
	k := newSeededKMS(t)

	oldBlob, oldVersion, err := k.Encrypt(PurposeAK, []byte("old-secret"))
	if err != nil {
		t.Fatal(err)
	}

	// Rotate: add version 2, which becomes current.
	if err := k.GenerateMasterKey(PurposeAK, 2); err != nil {
		t.Fatal(err)
	}
	newBlob, newVersion, err := k.Encrypt(PurposeAK, []byte("new-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if newVersion != 2 {
		t.Fatalf("new encryption should use version 2, got %d", newVersion)
	}

	// Old ciphertext still decrypts under its recorded version.
	got, err := k.Decrypt(PurposeAK, oldBlob, oldVersion)
	if err != nil {
		t.Fatalf("old ciphertext must remain readable after rotation: %v", err)
	}
	if string(got) != "old-secret" {
		t.Fatalf("old plaintext = %q", got)
	}
	// New ciphertext decrypts under the new version.
	got2, err := k.Decrypt(PurposeAK, newBlob, newVersion)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != "new-secret" {
		t.Fatalf("new plaintext = %q", got2)
	}
}

func TestUnknownVersionRejected(t *testing.T) {
	k := newSeededKMS(t)
	blob, _, err := k.Encrypt(PurposeAK, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Decrypt(PurposeAK, blob, 99); err != ErrUnknownVersion {
		t.Fatalf("expected ErrUnknownVersion, got %v", err)
	}
}

func TestUnknownPurposeRejected(t *testing.T) {
	k := New() // no master keys seeded
	if _, _, err := k.Encrypt(PurposeAK, []byte("x")); err != ErrUnknownPurpose {
		t.Fatalf("expected ErrUnknownPurpose, got %v", err)
	}
}

func TestGenerateDataKeyProducesDistinctDEKs(t *testing.T) {
	k := newSeededKMS(t)
	a, err := k.GenerateDataKey(PurposeDB)
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.GenerateDataKey(PurposeDB)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Plaintext, b.Plaintext) {
		t.Fatal("each object must get a distinct DEK")
	}
	// Both unwrap correctly.
	ua, err := k.DecryptDataKey(PurposeDB, a.Encrypted, a.Version)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ua, a.Plaintext) {
		t.Fatal("DEK unwrap mismatch")
	}
}

func TestMasterKeyLengthValidated(t *testing.T) {
	k := New()
	if err := k.AddMasterKey(PurposeAK, 1, []byte("too-short")); err == nil {
		t.Fatal("short master key must be rejected")
	}
}

func TestShortBlobRejected(t *testing.T) {
	k := newSeededKMS(t)
	if _, err := k.Decrypt(PurposeAK, []byte{0x00}, 1); err != ErrCiphertextShort {
		t.Fatalf("expected ErrCiphertextShort, got %v", err)
	}
}
