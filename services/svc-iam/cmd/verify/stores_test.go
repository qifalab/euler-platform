package main

import (
	"bytes"
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/cps1"
	"github.com/qifalab/euler-platform/storage/sqltest"
	"github.com/qifalab/euler-platform/sts"
	"github.com/qifalab/euler-platform/verify"
)

func TestSQLNonceStoreIsSharedAndExpires(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-iam"))
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a, b := newSQLNonceStore(db), newSQLNonceStore(db) // two replicas
	a.now = func() time.Time { return clock }
	b.now = func() time.Time { return clock }

	if fresh, err := a.SetNX("sec:nonce:AK:n1", verify.NonceTTL); err != nil || !fresh {
		t.Fatalf("first use: %v %v", fresh, err)
	}
	// Regression: the in-memory store let a replay succeed on another replica.
	if fresh, err := b.SetNX("sec:nonce:AK:n1", verify.NonceTTL); err != nil || fresh {
		t.Fatalf("replay on another replica: fresh=%v err=%v", fresh, err)
	}
	clock = clock.Add(verify.NonceTTL + time.Second)
	if fresh, err := b.SetNX("sec:nonce:AK:n1", verify.NonceTTL); err != nil || !fresh {
		t.Fatalf("after expiry the key is reusable: %v %v", fresh, err)
	}
	// Arbitrarily long client nonces are hashed to a fixed-width key.
	long := string(bytes.Repeat([]byte("x"), 5000))
	if fresh, err := a.SetNX("sec:nonce:AK:"+long, verify.NonceTTL); err != nil || !fresh {
		t.Fatalf("long nonce: %v %v", fresh, err)
	}
}

func TestSQLAccountStatusFailsClosed(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-iam"))
	ctx := context.Background()
	for id, status := range map[int64]int{1001: 1, 1002: 2, 1003: 3} {
		if _, err := db.ExecContext(ctx, `INSERT INTO account (account_id, account_name, password_hash, status) VALUES (?, ?, 'x', ?)`,
			id, "acct-"+strconv.FormatInt(id, 10), status); err != nil {
			t.Fatal(err)
		}
	}
	clock := time.Now()
	s := newSQLAccountStatus(db)
	s.now = func() time.Time { return clock }
	for id, want := range map[int64]bool{1001: true, 1002: false, 1003: false, 9999: false} {
		if got, err := s.IsAccountUsable(id); err != nil || got != want {
			t.Errorf("account %d: usable=%v err=%v, want %v", id, got, err, want)
		}
	}
	// Freezing takes effect once the short cache entry expires.
	if _, err := db.ExecContext(ctx, `UPDATE account SET status = 2 WHERE account_id = 1001`); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(accountCacheTTL + time.Second)
	if got, _ := s.IsAccountUsable(1001); got {
		t.Fatal("frozen account still usable after the cache TTL")
	}
	// The dev seed row is idempotent.
	for i := 0; i < 2; i++ {
		if err := devSeedAccountRow(ctx, db, 100123); err != nil {
			t.Fatal(err)
		}
	}
}

// End to end: credentials minted by the STS issuer (web-auth) are accepted by
// the verifier with the shared key, and a token from another key is not.
func TestSTSTemporaryCredentialsVerify(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	issuer, _ := sts.NewIssuer(time.Hour, nil, key)
	cred, err := issuer.AssumeRole("Deployer", "100123")
	if err != nil {
		t.Fatal(err)
	}
	v := &verify.Verifier{AK: akResolver{}, Nonces: newMemNonceStore(), Accounts: allAccountsUsable{}, STS: stsValidator{issuer}}

	sign := func(token string) verify.Request {
		now := time.Now().UTC()
		hdrs := map[string]string{"host": "euecs.api.euler.emoera.com", cps1.HeaderDate: now.Format("20060102T150405Z"),
			cps1.HeaderContentSHA: "", cps1.NonceHeader: now.String(), cps1.SecurityTokenHeader: token}
		q := url.Values{"Action": {"DescribeInstances"}}
		s, err := cps1.Sign(cps1.Request{Method: "GET", Host: "euecs.api.euler.emoera.com", Path: "/", Query: q, Headers: hdrs},
			cps1.Credentials{AK: cred.AccessKey, SK: cred.SecretKey, SecurityToken: token}, "cn-north-1", "ecs", now)
		if err != nil {
			t.Fatal(err)
		}
		h := map[string]string{"Authorization": s.Authorization}
		for k, val := range s.Headers {
			h[k] = val
		}
		return verify.Request{Method: "GET", Host: "euecs.api.euler.emoera.com", Path: "/", Query: q, Headers: h,
			Region: "cn-north-1", Service: "ecs"}
	}
	id, err := v.Verify(sign(cred.SecurityToken), time.Now())
	if err != nil || id.AccountID != 100123 || id.Principal != "role/Deployer" {
		t.Fatalf("STS verify: %+v %v", id, err)
	}
	other, _ := sts.NewIssuer(time.Hour, nil, bytes.Repeat([]byte{4}, 32))
	forged, _ := other.AssumeRole("Admin", "100123")
	forged.AccessKey, forged.SecretKey = cred.AccessKey, cred.SecretKey
	if _, err := v.Verify(sign(forged.SecurityToken), time.Now()); err == nil {
		t.Fatal("token sealed with another key accepted")
	}
}
