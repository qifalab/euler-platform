package verify

import (
	"net/url"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/accesskey"
	"github.com/qifalab/euler-platform/cps1"
	"github.com/qifalab/euler-platform/kms"
)

var testNow = time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)

const (
	testRegion  = "cn-north-1"
	testService = "ecs"
	testHost    = "euecs.api.euler.emoera.com"
)

// --- test doubles ---

type memNonces struct{ seen map[string]bool }

func newMemNonces() *memNonces { return &memNonces{seen: make(map[string]bool)} }

func (m *memNonces) SetNX(key string, _ time.Duration) (bool, error) {
	if m.seen[key] {
		return false, nil
	}
	m.seen[key] = true
	return true, nil
}

// failingNonces simulates the replay store being down.
type failingNonces struct{}

func (failingNonces) SetNX(string, time.Duration) (bool, error) {
	return false, errRedisDown
}

var errRedisDown = &redisDownError{}

type redisDownError struct{}

func (*redisDownError) Error() string { return "redis unavailable" }

type memAccounts struct{ frozen map[int64]bool }

func (m *memAccounts) IsAccountUsable(accountID int64) (bool, error) {
	return !m.frozen[accountID], nil
}

type memSTS struct {
	token   string
	cred    STSCredential
	expired bool
}

func (m *memSTS) ValidateToken(token string, _ time.Time) (STSCredential, error) {
	if token != m.token || m.expired {
		return STSCredential{}, ErrSecurityTokenExpired
	}
	return m.cred, nil
}

// Temporary credentials sealed in the test token.
const (
	tempAK = "EUTEMP0000000000000000000000000A"
	tempSK = "temporary-secret-key"
)

// akStore adapts accesskey.Manager to the AKResolver port.
type akStore struct{ mgr *accesskey.Manager }

func (a akStore) ResolveSK(ak string, now time.Time) (string, accesskey.Record, error) {
	return a.mgr.ResolveSK(ak, now)
}

// memAKStore is the accesskey.Store backing the manager.
type memAKStore struct{ rows map[string]accesskey.Record }

func newMemAKStore() *memAKStore { return &memAKStore{rows: make(map[string]accesskey.Record)} }

func (m *memAKStore) Insert(r accesskey.Record) error { m.rows[r.AK] = r; return nil }
func (m *memAKStore) Get(ak string) (accesskey.Record, error) {
	r, ok := m.rows[ak]
	if !ok {
		return accesskey.Record{}, accesskey.ErrNotFound
	}
	return r, nil
}
func (m *memAKStore) ListByOwner(accountID int64, ot accesskey.OwnerType, oid int64) ([]accesskey.Record, error) {
	var out []accesskey.Record
	for _, r := range m.rows {
		if r.AccountID == accountID && r.OwnerType == ot && r.OwnerID == oid {
			out = append(out, r)
		}
	}
	return out, nil
}
func (m *memAKStore) Update(r accesskey.Record) error { m.rows[r.AK] = r; return nil }

// --- fixture ---

type fixture struct {
	verifier *Verifier
	ak       string
	sk       string
	accounts *memAccounts
	nonces   *memNonces
	mgr      *accesskey.Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	k := kms.New()
	if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
		t.Fatal(err)
	}
	store := newMemAKStore()
	mgr := accesskey.NewManager(store, k, func() time.Time { return testNow })
	created, err := mgr.Create(100123, accesskey.OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	accounts := &memAccounts{frozen: make(map[int64]bool)}
	nonces := newMemNonces()
	return &fixture{
		verifier: &Verifier{
			AK:       akStore{mgr},
			Nonces:   nonces,
			Accounts: accounts,
		},
		ak:       created.Record.AK,
		sk:       created.SKPlaintext,
		accounts: accounts,
		nonces:   nonces,
		mgr:      mgr,
	}
}

// signRequest produces a fully signed request the gateway would forward.
func signRequest(t *testing.T, ak, sk, nonce string, body []byte, at time.Time) Request {
	t.Helper()
	hdrs := map[string]string{
		"host":                 testHost,
		cps1.HeaderDate:        at.UTC().Format("20060102T150405Z"),
		cps1.HeaderContentSHA:  "",
		cps1.NonceHeader:       nonce,
	}
	query := url.Values{"Action": {"DescribeInstances"}, "Version": {"2026-08-01"}}
	req := cps1.Request{
		Method:  "POST",
		Host:    testHost,
		Path:    "/",
		Query:   query,
		Headers: hdrs,
		Body:    body,
	}
	signed, err := cps1.Sign(req, cps1.Credentials{AK: ak, SK: sk}, testRegion, testService, at)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	out := make(map[string]string, len(signed.Headers)+1)
	for k, v := range signed.Headers {
		out[k] = v
	}
	out["Authorization"] = signed.Authorization
	return Request{
		Method:  "POST",
		Host:    testHost,
		Path:    "/",
		Query:   query,
		Headers: out,
		Body:    body,
		Region:  testRegion,
		Service: testService,
	}
}

// --- tests ---

func TestVerifyHappyPath(t *testing.T) {
	f := newFixture(t)
	req := signRequest(t, f.ak, f.sk, "nonce-1", []byte(`{"x":1}`), testNow)

	id, err := f.verifier.Verify(req, testNow)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.AccountID != 100123 {
		t.Fatalf("AccountID = %d, want 100123", id.AccountID)
	}
	if id.AKID != f.ak {
		t.Fatalf("AKID = %q, want %q", id.AKID, f.ak)
	}
	if id.OwnerType != accesskey.OwnerRAMUser {
		t.Fatalf("OwnerType = %d", id.OwnerType)
	}
}

func TestVerifyRejectsReplayedNonce(t *testing.T) {
	f := newFixture(t)
	req := signRequest(t, f.ak, f.sk, "replay-me", nil, testNow)

	if _, err := f.verifier.Verify(req, testNow); err != nil {
		t.Fatalf("first request should pass: %v", err)
	}
	// Same nonce again → replay.
	if _, err := f.verifier.Verify(req, testNow); err != ErrSignatureNonceUsed {
		t.Fatalf("expected ErrSignatureNonceUsed, got %v", err)
	}
}

func TestVerifyRejectsSkewedClock(t *testing.T) {
	f := newFixture(t)
	// Signed 20 minutes in the past; server clock is testNow.
	past := testNow.Add(-20 * time.Minute)
	req := signRequest(t, f.ak, f.sk, "skew-nonce", nil, past)

	if _, err := f.verifier.Verify(req, testNow); err != ErrRequestTimeTooSkewed {
		t.Fatalf("expected ErrRequestTimeTooSkewed, got %v", err)
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	f := newFixture(t)
	req := signRequest(t, f.ak, f.sk, "tamper-nonce", []byte(`{"amount":1}`), testNow)
	// Attacker swaps the body after signing.
	req.Body = []byte(`{"amount":9999}`)

	if _, err := f.verifier.Verify(req, testNow); err != ErrSignatureDoesNotMatch {
		t.Fatalf("expected ErrSignatureDoesNotMatch, got %v", err)
	}
}

func TestVerifyRejectsUnknownAK(t *testing.T) {
	f := newFixture(t)
	// Sign with a well-formed but unregistered AK.
	req := signRequest(t, "SCunknownunknownunknownunknown12", "some-sk", "unknown-nonce", nil, testNow)

	if _, err := f.verifier.Verify(req, testNow); err != ErrInvalidAccessKeyID {
		t.Fatalf("expected ErrInvalidAccessKeyId, got %v", err)
	}
}

func TestVerifyRejectsDisabledKey(t *testing.T) {
	f := newFixture(t)
	if err := f.mgr.SetStatus(f.ak, accesskey.StatusDisabled); err != nil {
		t.Fatal(err)
	}
	req := signRequest(t, f.ak, f.sk, "disabled-nonce", nil, testNow)

	if _, err := f.verifier.Verify(req, testNow); err != ErrInvalidAccessKeyID {
		t.Fatalf("expected ErrInvalidAccessKeyId for disabled key, got %v", err)
	}
}

func TestVerifyRejectsFrozenAccount(t *testing.T) {
	f := newFixture(t)
	f.accounts.frozen[100123] = true
	req := signRequest(t, f.ak, f.sk, "frozen-nonce", nil, testNow)

	if _, err := f.verifier.Verify(req, testNow); err != ErrAccountUnusable {
		t.Fatalf("expected ErrAccountUnusable, got %v", err)
	}
}

func TestVerifyRejectsWrongScope(t *testing.T) {
	// A signature scoped to service "ecs" must not verify on the oss route.
	// This is why region/service come from the routed subdomain, never the client.
	f := newFixture(t)
	req := signRequest(t, f.ak, f.sk, "scope-nonce", nil, testNow)
	req.Service = "oss"

	if _, err := f.verifier.Verify(req, testNow); err != ErrSignatureDoesNotMatch {
		t.Fatalf("expected ErrSignatureDoesNotMatch for scope mismatch, got %v", err)
	}
}

func TestVerifyFailsClosedWhenNonceStoreDown(t *testing.T) {
	// If the replay store is unavailable we cannot prove the request is not a
	// replay, so verification must fail rather than wave it through.
	f := newFixture(t)
	f.verifier.Nonces = failingNonces{}
	req := signRequest(t, f.ak, f.sk, "down-nonce", nil, testNow)

	if _, err := f.verifier.Verify(req, testNow); err != ErrSignatureNonceUsed {
		t.Fatalf("expected fail-closed ErrSignatureNonceUsed, got %v", err)
	}
}

func stsRequest(t *testing.T, ak, sk, nonce string) Request {
	t.Helper()
	req := signRequest(t, ak, sk, nonce, nil, testNow)
	req.Headers[cps1.SecurityTokenHeader] = "sts-token-abc"
	// Re-sign because the security-token header participates in the signature.
	return resignWithHeaders(t, ak, sk, req, testNow)
}

func TestVerifySTSHappyPath(t *testing.T) {
	f := newFixture(t)
	f.verifier.STS = &memSTS{token: "sts-token-abc", cred: STSCredential{AccessKey: tempAK, SecretKey: tempSK, AccountID: 100123, Principal: "role/deployer"}}

	id, err := f.verifier.Verify(stsRequest(t, tempAK, tempSK, "sts-nonce"), testNow)
	if err != nil {
		t.Fatalf("Verify with STS: %v", err)
	}
	if id.Principal != "role/deployer" || id.AccountID != 100123 || id.AKID != tempAK {
		t.Fatalf("identity = %+v", id)
	}
}

func TestVerifySTSRejectsExpiredToken(t *testing.T) {
	f := newFixture(t)
	f.verifier.STS = &memSTS{token: "sts-token-abc", cred: STSCredential{AccessKey: tempAK, SecretKey: tempSK, AccountID: 100123}, expired: true}

	if _, err := f.verifier.Verify(stsRequest(t, tempAK, tempSK, "sts-exp-nonce"), testNow); err != ErrSecurityTokenExpired {
		t.Fatalf("expected ErrSecurityTokenExpired, got %v", err)
	}
}

// A token is bound to the temporary AK it sealed: it cannot ride on another
// key (a long-lived AK, or someone else's temporary one).
func TestVerifySTSRejectsTokenForAnotherAK(t *testing.T) {
	f := newFixture(t)
	f.verifier.STS = &memSTS{token: "sts-token-abc", cred: STSCredential{AccessKey: tempAK, SecretKey: tempSK, AccountID: 999999, Principal: "role/other"}}

	if _, err := f.verifier.Verify(stsRequest(t, f.ak, f.sk, "sts-cross-nonce"), testNow); err != ErrSecurityTokenExpired {
		t.Fatalf("expected ErrSecurityTokenExpired for a token naming another AK, got %v", err)
	}
}

// The signature must be made with the SEALED temporary secret.
func TestVerifySTSRejectsWrongSecret(t *testing.T) {
	f := newFixture(t)
	f.verifier.STS = &memSTS{token: "sts-token-abc", cred: STSCredential{AccessKey: tempAK, SecretKey: tempSK, AccountID: 100123}}

	if _, err := f.verifier.Verify(stsRequest(t, tempAK, "guessed-secret", "sts-wrong-sk"), testNow); err != ErrSignatureDoesNotMatch {
		t.Fatalf("expected ErrSignatureDoesNotMatch, got %v", err)
	}
}

// Freezing an account revokes its temporary credentials too.
func TestVerifySTSRejectsFrozenAccount(t *testing.T) {
	f := newFixture(t)
	f.verifier.STS = &memSTS{token: "sts-token-abc", cred: STSCredential{AccessKey: tempAK, SecretKey: tempSK, AccountID: 100123}}
	f.accounts.frozen[100123] = true

	if _, err := f.verifier.Verify(stsRequest(t, tempAK, tempSK, "sts-frozen"), testNow); err != ErrAccountUnusable {
		t.Fatalf("expected ErrAccountUnusable, got %v", err)
	}
}

func TestRotatedKeyStillVerifiesDuringGrace(t *testing.T) {
	// End-to-end: rotation must not break in-flight callers during the grace
	// window (07§2.2 rule 2).
	f := newFixture(t)
	if _, err := f.mgr.Rotate(f.ak, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	later := testNow.Add(12 * time.Hour)
	req := signRequest(t, f.ak, f.sk, "grace-nonce", nil, later)

	if _, err := f.verifier.Verify(req, later); err != nil {
		t.Fatalf("old key must verify during grace: %v", err)
	}

	// Past the grace window it is refused.
	afterGrace := testNow.Add(25 * time.Hour)
	req2 := signRequest(t, f.ak, f.sk, "grace-nonce-2", nil, afterGrace)
	if _, err := f.verifier.Verify(req2, afterGrace); err != ErrInvalidAccessKeyID {
		t.Fatalf("old key after grace: expected ErrInvalidAccessKeyId, got %v", err)
	}
}

func TestExtractAK(t *testing.T) {
	cases := []struct {
		auth string
		want string
		ok   bool
	}{
		{"CPS1-HMAC-SHA256 Credential=SCabc/20260804/cn-north-1/ecs/cps1_request, SignedHeaders=host, Signature=ff", "SCabc", true},
		{"CPS1-HMAC-SHA256 SignedHeaders=host, Signature=ff", "", false},
		{"", "", false},
		{"CPS1-HMAC-SHA256 Credential=/20260804/x/y/z", "", false},
	}
	for _, c := range cases {
		got, err := extractAK(map[string]string{"authorization": c.auth})
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("extractAK(%q) = (%q,%v), want (%q,nil)", c.auth, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("extractAK(%q) should fail", c.auth)
		}
	}
}

// resignWithHeaders re-signs a request after extra x-cps-* headers were added,
// since those headers participate in the canonical header block.
func resignWithHeaders(t *testing.T, ak, sk string, req Request, at time.Time) Request {
	t.Helper()
	hdrs := make(map[string]string, len(req.Headers))
	for k, v := range req.Headers {
		if k == "Authorization" {
			continue
		}
		hdrs[k] = v
	}
	sigReq := cps1.Request{
		Method:  req.Method,
		Host:    req.Host,
		Path:    req.Path,
		Query:   req.Query,
		Headers: hdrs,
		Body:    req.Body,
	}
	signed, err := cps1.Sign(sigReq, cps1.Credentials{AK: ak, SK: sk}, req.Region, req.Service, at)
	if err != nil {
		t.Fatalf("re-sign: %v", err)
	}
	out := make(map[string]string, len(signed.Headers)+1)
	for k, v := range signed.Headers {
		out[k] = v
	}
	out["Authorization"] = signed.Authorization
	req.Headers = out
	return req
}
