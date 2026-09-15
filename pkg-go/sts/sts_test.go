package sts

import (
	"testing"
	"time"
)

func TestAssumeRoleIssuesValidCredentials(t *testing.T) {
	i, err := NewIssuer(DefaultTTL, nil)
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
	i, _ := NewIssuer(time.Minute, func() time.Time { return time.Unix(1000, 0) })
	c, _ := i.AssumeRole("role", "acct")
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
		if _, err := NewIssuer(ttl, nil); err == nil {
			t.Errorf("TTL %v accepted, want rejection (temporary credentials must expire)", ttl)
		}
	}
}

func TestAssumeRoleRejectsEmptyRoleOrAccount(t *testing.T) {
	i, _ := NewIssuer(DefaultTTL, nil)
	if _, err := i.AssumeRole("", "acct"); err == nil {
		t.Fatal("empty role accepted")
	}
	if _, err := i.AssumeRole("role", ""); err == nil {
		t.Fatal("empty account accepted")
	}
}

func TestCredentialsUniquePerCall(t *testing.T) {
	i, _ := NewIssuer(DefaultTTL, nil)
	a, _ := i.AssumeRole("role", "acct")
	b, _ := i.AssumeRole("role", "acct")
	if a.AccessKey == b.AccessKey || a.SecurityToken == b.SecurityToken {
		t.Fatal("two AssumeRole calls must mint distinct credentials")
	}
}
