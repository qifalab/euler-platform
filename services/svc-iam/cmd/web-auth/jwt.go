package main

// RS256 access/MFA token primitives.
//
// The console zone gateway (eu-auth.lua) verifies access tokens with the
// PUBLIC half of this key (adjudication S13: RS256, kid rotation). This
// service used to sign HS256 with a shared secret while the gateway was
// configured for an RSA public key — a mismatch that, with lua-resty-jwt's
// header-driven algorithm choice, invites the classic alg-confusion forgery
// (an HS256 token "signed" with the public key). Tokens are now RS256 only,
// on both sides, and the verifier refuses any other header shape.

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

const (
	// jwtKeyFileEnv points at a PEM RSA private key (mounted Secret) — preferred.
	jwtKeyFileEnv = "EULER_JWT_PRIVATE_KEY_FILE"
	// jwtKeyEnv carries the PEM itself (for environments without file mounts).
	jwtKeyEnv = "EULER_JWT_PRIVATE_KEY"
	// minRSABits is the smallest signing key accepted.
	minRSABits = 2048
)

var errInvalidToken = errors.New("Auth.InvalidSignature")

// rsaSigner holds the signing key and its key id (published in the JWT
// header so the gateway can rotate by kid).
type rsaSigner struct {
	key       *rsa.PrivateKey
	kid       string
	ephemeral bool // generated at boot because no key was configured
}

// jwtSigner is the process signing key. main() refuses to run in production
// when it is ephemeral.
var jwtSigner = mustLoadJWTSigner()

func mustLoadJWTSigner() *rsaSigner {
	s, err := loadJWTSigner()
	if err != nil {
		panic("jwt signing key: " + err.Error())
	}
	return s
}

func loadJWTSigner() (*rsaSigner, error) {
	pemBytes := []byte(os.Getenv(jwtKeyEnv))
	if path := strings.TrimSpace(os.Getenv(jwtKeyFileEnv)); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", jwtKeyFileEnv, err)
		}
		pemBytes = b
	}
	if len(bytes.TrimSpace(pemBytes)) == 0 {
		key, err := rsa.GenerateKey(rand.Reader, minRSABits)
		if err != nil {
			return nil, err
		}
		slog.Warn("no JWT signing key configured — generated an ephemeral RS256 key (allowed outside production only; tokens die with the process)",
			"set", jwtKeyFileEnv)
		return newRSASigner(key, true)
	}
	key, err := parseRSAPrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	return newRSASigner(key, false)
}

func newRSASigner(key *rsa.PrivateKey, ephemeral bool) (*rsaSigner, error) {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(der)
	return &rsaSigner{key: key, kid: hex.EncodeToString(sum[:8]), ephemeral: ephemeral}, nil
}

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in JWT private key")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse JWT private key: %w", err)
		}
		rk, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("JWT private key is not RSA")
		}
		key = rk
	}
	if key.N.BitLen() < minRSABits {
		return nil, fmt.Errorf("JWT RSA key is %d bits; at least %d required", key.N.BitLen(), minRSABits)
	}
	return key, nil
}

// sign serialises claims with encoding/json (never string templating) and
// returns a compact RS256 JWT.
func (s *rsaSigner) sign(claims any) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		panic("jwt: marshal claims: " + err.Error()) // fixed struct types: cannot fail
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.kid})
	input := b64url(header) + "." + b64url(payload)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		panic("jwt: sign: " + err.Error())
	}
	return input + "." + b64url(sig)
}

// verify checks that the header is exactly an RS256 JWS for this key (no
// other alg, no JWE "enc", no "crit" extensions, matching kid) and that the
// signature is valid, then returns the raw payload.
func (s *rsaSigner) verify(token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("Auth.MalformedToken")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("Auth.MalformedToken")
	}
	var h map[string]any
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, errors.New("Auth.MalformedToken")
	}
	if alg, _ := h["alg"].(string); alg != "RS256" {
		return nil, errInvalidToken
	}
	if _, ok := h["enc"]; ok {
		return nil, errInvalidToken
	}
	if _, ok := h["crit"]; ok {
		return nil, errInvalidToken
	}
	if kid, ok := h["kid"]; ok && kid != s.kid {
		return nil, errInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errInvalidToken
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&s.key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("Auth.MalformedToken")
	}
	return payload, nil
}

// publicKeyPEM is what operators mount into the gateway (eu-jwt-public-key).
func (s *rsaSigner) publicKeyPEM() []byte {
	der, _ := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
