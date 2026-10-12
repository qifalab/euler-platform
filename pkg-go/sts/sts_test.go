package sts

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

var testKey = bytes.Repeat([]byte{7}, 32)

// The token is the sealed credential: Validate recovers exactly what was
// issued, and nothing else validates.
func TestValidateRoundTrip(t *testing.T) {
	i, _ := NewIssuer(time.Hour, func() time.Time { return time.Unix(1000, 0) }, testKey)
	c, err := i.AssumeRole("Deployer", "100123")
	if err != nil {
		t.Fatal(err)
	}
	got, err := i.Validate(c.SecurityToken, time.Unix(1001, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessKey != c.AccessKey || got.SecretKey != c.SecretKey || got.AccountID != 100123 || got.Role != "Deployer" {
		t.Fatalf("claims = %+v", got)
	}
	if _, err := i.Validate(c.SecurityToken, time.Unix(1000+3600, 0)); err != ErrTokenExpired {
		t.Fatalf("at expiry: %v, want ErrTokenExpired", err)
	}
}

// Regression: tokens were "STS.<role>.<account>.<random>" — anyone could write
// one naming any account.
func TestForgedOrTamperedTokensRejected(t *testing.T) {
	i, _ := NewIssuer(time.Hour, nil, testKey)
	other, _ := NewIssuer(time.Hour, nil, bytes.Repeat([]byte{9}, 32))
	c, _ := other.AssumeRole("Admin", "100123")
	good, _ := i.AssumeRole("Admin", "100123")
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(good.SecurityToken, "STS2."))
	raw[len(raw)-1] ^= 1
	for name, tok := range map[string]string{
		"legacy plain format": "STS.Admin.100123.deadbeef",
		"other key":           c.SecurityToken,
		"bit flip":            "STS2." + base64.RawURLEncoding.EncodeToString(raw),
		"garbage":             "STS2.!!!",
		"empty":               "",
	} {
		if _, err := i.Validate(tok, time.Now()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewIssuerRequires32ByteKey(t *testing.T) {
	if _, err := NewIssuer(time.Hour, nil, []byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestKeyFromEnv(t *testing.T) {
	t.Setenv(KeyEnv, "")
	if _, ok, _ := KeyFromEnv(); ok {
		t.Fatal("unset must report ok=false")
	}
	t.Setenv(KeyEnv, base64.StdEncoding.EncodeToString(testKey))
	if k, ok, err := KeyFromEnv(); !ok || err != nil || !bytes.Equal(k, testKey) {
		t.Fatalf("KeyFromEnv = %x %v %v", k, ok, err)
	}
	t.Setenv(KeyEnv, base64.StdEncoding.EncodeToString([]byte("short")))
	if _, _, err := KeyFromEnv(); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestAssumeRoleIssuesValidCredentials(t *testing.T) {
	i, err := NewIssuer(DefaultTTL, nil, testKey)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	now := time.Now()
	c, err := i.AssumeRole("EuEcsFullAccess", "100123")
	if err != nil {
		t.Fatalf("AssumeRole: %v", err)
	}
	if !c.Valid(now) {
		t.Fatalf("fresh credentials must be valid: %+v", c)
	}
	if len(c.AccessKey) != 32 || c.AccessKey[:2] != "EU" {
		t.Fatalf("AK format wrong: %q", c.AccessKey)
	}
}

func TestCredentialsExpire(t *testing.T) {
	i, _ := NewIssuer(time.Minute, func() time.Time { return time.Unix(1000, 0) }, testKey)
	c, _ := i.AssumeRole("role", "100123")
	if !c.Valid(time.Unix(1000+59, 0)) {
		t.Fatal("credentials must be valid 59s in, for a 1min TTL")
	}
	if c.Valid(time.Unix(1000+60, 0)) {
		t.Fatal("credentials must be invalid AT expiry (inclusive boundary)")
	}
	if c.Valid(time.Unix(1000+61, 0)) {
		t.Fatal("credentials must be invalid past expiry")
	}
}

func TestNewIssuerRejectsNonPositiveTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Minute} {
		if _, err := NewIssuer(ttl, nil, testKey); err == nil {
			t.Errorf("TTL %v accepted, want rejection (temporary credentials must expire)", ttl)
		}
	}
}

func TestAssumeRoleRejectsEmptyRoleOrAccount(t *testing.T) {
	i, _ := NewIssuer(DefaultTTL, nil, testKey)
	if _, err := i.AssumeRole("", "100123"); err == nil {
		t.Fatal("empty role accepted")
	}
	if _, err := i.AssumeRole("role", ""); err == nil {
		t.Fatal("empty account accepted")
	}
}

func TestCredentialsUniquePerCall(t *testing.T) {
	i, _ := NewIssuer(DefaultTTL, nil, testKey)
	a, _ := i.AssumeRole("role", "100123")
	b, _ := i.AssumeRole("role", "100123")
	if a.AccessKey == b.AccessKey || a.SecurityToken == b.SecurityToken {
		t.Fatal("two AssumeRole calls must mint distinct credentials")
	}
}
