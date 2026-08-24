package verify

import (
	"errors"
	"testing"
)

// When a nonce store is configured, the nonce header is mandatory: a replayer
// could otherwise bypass dedup entirely by stripping the header.
func TestMissingNonceRejectedWhenStoreConfigured(t *testing.T) {
	f := newFixture(t)
	req := signRequest(t, f.ak, f.sk, "", nil, testNow) // no nonce header value
	if _, err := f.verifier.Verify(req, testNow); !errors.Is(err, ErrSignatureNonceMissing) {
		t.Fatalf("err = %v, want ErrSignatureNonceMissing", err)
	}
}

// Without a nonce store (e.g. an internal test verifier), requests still pass.
func TestNoNonceStoreSkipsNonceCheck(t *testing.T) {
	f := newFixture(t)
	f.verifier.Nonces = nil
	req := signRequest(t, f.ak, f.sk, "", nil, testNow)
	if _, err := f.verifier.Verify(req, testNow); err != nil {
		t.Fatalf("verify without nonce store: %v", err)
	}
}
