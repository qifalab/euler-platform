package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/accesskey"
	"github.com/qifalab/euler-platform/cps1"
	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/kms"
	"github.com/qifalab/euler-platform/notify"
	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/storage/sqltest"
	"github.com/qifalab/euler-platform/totp"
	"github.com/qifalab/euler-platform/verify"
)

const (
	testRootV1 = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	testRootV2 = "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWY="
)

// F2 delivery: the request web-auth sends svc-notify is the
// POST /internal/notifications contract — VERIFICATION_CODE over EMAIL, the
// code as a secret parameter, the tenant in X-Euler-Account-Id, the internal
// token attached.
func TestNotifyMailerRequest(t *testing.T) {
	t.Setenv(httpmw.InternalTokenEnv, "svc-token")
	var got struct {
		Class, TemplateId, BizKey string
		Channels                  []string
		Params                    map[string]string
	}
	var account, token string
	reply := `{"Code":"OK","RequestId":"r"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/notifications" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		account, token = r.Header.Get("X-Euler-Account-Id"), r.Header.Get(httpmw.InternalTokenHeader)
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()

	m := notifyMailer{base: srv.URL, client: srv.Client()}
	if err := m.SendResetCode(context.Background(), 4242, "alice@example.com", "493817"); err != nil {
		t.Fatal(err)
	}
	if got.Class != string(notify.ClassVerificationCode) || got.TemplateId != notify.TemplatePasswordResetCode ||
		len(got.Channels) != 1 || got.Channels[0] != "EMAIL" || got.BizKey != "pwreset:4242" ||
		got.Params[notify.ParamRecipient] != "alice@example.com" || got.Params[notify.ParamCode] != "493817" || got.Params[notify.ParamTTLMinutes] != "10" {
		t.Fatalf("notification: %+v", got)
	}
	if account != "4242" || token != "svc-token" {
		t.Fatalf("headers: account=%q token=%q", account, token)
	}
	reply = `{"Code":"Notify.NotDelivered","Message":"message not delivered on any channel"}`
	if err := m.SendResetCode(context.Background(), 4242, "alice@example.com", "493817"); err == nil {
		t.Fatal("an undelivered notification was reported as sent")
	}
}

// fakeProvider is a real-name provider that checks the request signature.
func fakeProvider(t *testing.T, secret string, answer func(c realnameCheck) (int, string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if r.Header.Get(realnameSignatureHeader) != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Errorf("bad signature %q", r.Header.Get(realnameSignatureHeader))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var c realnameCheck
		_ = json.Unmarshal(body, &c)
		status, out := answer(c)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
}

func useProvider(t *testing.T, url, secret string) {
	t.Helper()
	t.Setenv(realnameProviderURLEnv, url)
	t.Setenv(realnameProviderSecretEnv, secret)
	p, err := realnameProviderFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	saved := realnameProvider
	realnameProvider = p
	t.Cleanup(func() { realnameProvider = saved })
}

// F6: the provider is asked (signed), its verdict decides, and its failure
// fails closed.
func TestRealnameProvider(t *testing.T) {
	useBackend(t, memoryBackend())
	srv := fakeProvider(t, "prov-secret", func(c realnameCheck) (int, string) {
		switch c.Name {
		case "张三":
			if c.IDNumber != "110101199001011237" || c.Type != "individual" {
				t.Errorf("provider asked %+v", c)
			}
			return 200, `{"match":true}`
		case "李四":
			return 200, `{"match":false}`
		case "王五":
			return 500, `oops`
		default:
			return 200, `{"unexpected":1}`
		}
	})
	defer srv.Close()
	useProvider(t, srv.URL, "prov-secret")

	c := newFlowClient(t)
	id := c.signup("provider@euler.emoera.com", "provider-pass")
	submit := func(name string) (int, map[string]any) {
		return c.do("POST", "/api/realname/verify", map[string]string{"type": "individual", "name": name, "idCardNo": "110101199001011237"})
	}
	for name, want := range map[string]struct {
		status int
		code   string
	}{
		"李四": {400, "Realname.Mismatch"},
		"王五": {503, "Realname.ProviderUnavailable"},
		"赵六": {503, "Realname.ProviderUnavailable"}, // malformed answer
	} {
		if status, out := submit(name); status != want.status || out["Code"] != want.code {
			t.Fatalf("%s: %d %v, want %d %s", name, status, out, want.status, want.code)
		}
		if a := mustAccount(t, id); a.RealNameStatus != 0 {
			t.Fatalf("%s: account verified anyway", name)
		}
	}
	if status, out := submit("张三"); status != 200 {
		t.Fatalf("match: %d %v", status, out)
	}
	if a := mustAccount(t, id); a.RealNameStatus != 1 || a.RealName != "张三" {
		t.Fatalf("after a match: %+v", a)
	}

	// A provider that hangs fails closed within the timeout.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	p := httpRealnameProvider{url: slow.URL, secret: []byte("s"), client: &http.Client{Timeout: 50 * time.Millisecond}}
	if _, err := p.Verify(context.Background(), realnameCheck{Name: "x", IDNumber: "y", Type: "individual"}); err == nil {
		t.Fatal("a hanging provider did not fail")
	}
	// Repeated mismatches are capped per account.
	c2 := newFlowClient(t)
	c2.signup("provider-cap@euler.emoera.com", "provider-pass")
	var last int
	for i := 0; i < 6; i++ {
		last, _ = c2.do("POST", "/api/realname/verify", map[string]string{"type": "individual", "name": "李四", "idCardNo": "110101199001011237"})
	}
	if last != 429 {
		t.Fatalf("6th mismatch: %d, want 429", last)
	}
}

func TestRealnameProviderConfig(t *testing.T) {
	for name, c := range map[string]struct {
		url, secret, env string
		ok               bool
	}{
		"unset":               {"", "", "", true},
		"secret without url":  {"", "s", "", false},
		"url without secret":  {"https://kyc.example.com/v1/check", "", "", false},
		"not a url":           {"kyc.example.com", "s", "", false},
		"http in production":  {"http://kyc.example.com/v1/check", "s", "prod", false},
		"http outside prod":   {"http://127.0.0.1:9/check", "s", "dev", true},
		"https in production": {"https://kyc.example.com/v1/check", "s", "prod", true},
		"unsupported scheme":  {"ftp://kyc.example.com/", "s", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(realnameProviderURLEnv, c.url)
			t.Setenv(realnameProviderSecretEnv, c.secret)
			t.Setenv("EULER_ENV", c.env)
			p, err := realnameProviderFromEnv()
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
			if c.ok && c.url == "" {
				if _, isChecksum := p.(checksumOnlyVerifier); !isChecksum {
					t.Fatalf("unset provider: %T", p)
				}
			}
		})
	}
}

// Production refuses to start without its settings (pkg-go/runmode list,
// storage, notify).
func TestProductionStartupRequirements(t *testing.T) {
	want := map[string]bool{httpmw.InternalTokenEnv: true, storage.DSNEnv: true, kms.MasterKeyEnv: true, identityPepperEnv: true, notifyURLEnv: true}
	for _, v := range productionSettings() {
		delete(want, v)
	}
	if len(want) != 0 {
		t.Fatalf("missing from the production list: %v", want)
	}
	t.Setenv(storage.DSNEnv, "")
	t.Setenv(kms.MasterKeyEnv, "")
	if _, err := backendFromEnv(context.Background(), true); err == nil {
		t.Fatal("production without a database must not start")
	}
	if _, err := loadKeyring(true); err == nil {
		t.Fatal("persistent / production mode without a KMS root must not start")
	}
	t.Setenv(kms.MasterKeyEnv, testRootV1)
	if _, err := loadKeyring(true); err != nil {
		t.Fatal(err)
	}

	saved := resetMailer
	t.Cleanup(func() { resetMailer = saved })
	t.Setenv(realnameProviderURLEnv, "")
	t.Setenv(realnameProviderSecretEnv, "")
	t.Setenv("EULER_ENV", "prod")
	t.Setenv(notifyURLEnv, "")
	if err := configureIntegrations(); err == nil {
		t.Fatal("production without svc-notify must not start")
	}
	t.Setenv(notifyURLEnv, "not a url")
	if err := configureIntegrations(); err == nil {
		t.Fatal("a malformed notify URL was accepted")
	}
	t.Setenv(notifyURLEnv, "http://svc-notify.euler-svc-notify.svc:8080/")
	if err := configureIntegrations(); err != nil {
		t.Fatal(err)
	}
	if m, ok := resetMailer.(notifyMailer); !ok || m.base != "http://svc-notify.euler-svc-notify.svc:8080" {
		t.Fatalf("mailer: %#v", resetMailer)
	}
	t.Setenv("EULER_ENV", "dev")
	t.Setenv(notifyURLEnv, "")
	if err := configureIntegrations(); err != nil {
		t.Fatal(err)
	}
	if _, ok := resetMailer.(logCodeMailer); !ok {
		t.Fatalf("dev without svc-notify: %T, want the logging stand-in", resetMailer)
	}
}

// F5 in web-auth: after a master-key rotation (new root current, old root in
// EULER_KMS_MASTER_KEY_PREVIOUS) MFA seeds and AccessKeys sealed before keep
// working, and new secrets are sealed under the new version.
func TestKMSRotationKeepsSealedSecrets(t *testing.T) {
	t.Setenv(kms.MasterKeyEnv, testRootV1)
	t.Setenv(kms.MasterKeyVersionEnv, "")
	t.Setenv(kms.MasterKeyPreviousEnv, "")
	keysV1, err := loadKeyring(true)
	if err != nil {
		t.Fatal(err)
	}
	b := memoryBackend()
	b.keys = keysV1
	useBackend(t, b)

	c := newFlowClient(t)
	id := c.signup("rotation@euler.emoera.com", "rotation-pass")
	_, out := c.do("POST", "/api/mfa/bind", nil)
	seed, err := totp.ParseSecretBase32(dataOf(t, out)["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if code, out := c.do("POST", "/api/mfa/verify", map[string]string{"code": totp.Code(seed, time.Now())}); code != 200 {
		t.Fatalf("activate: %d %v", code, out)
	}
	_, out = c.do("POST", "/api/ak", nil)
	oldAK, oldSK := dataOf(t, out)["akId"].(string), dataOf(t, out)["secretKey"].(string)

	// "Restart" with the rotated configuration over the same stores.
	t.Setenv(kms.MasterKeyEnv, testRootV2)
	t.Setenv(kms.MasterKeyVersionEnv, "2")
	t.Setenv(kms.MasterKeyPreviousEnv, "1:"+testRootV1)
	keysV2, err := loadKeyring(true)
	if err != nil {
		t.Fatal(err)
	}
	b.keys = keysV2
	installBackend(b)

	totpVerifier.Forget(mfaReplayKey(id))
	code, out := c.do("POST", "/api/auth/login", map[string]string{"email": "rotation@euler.emoera.com", "password": "rotation-pass"})
	if code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	if code, out := c.do("POST", "/api/auth/mfa/verify", map[string]string{"mfaToken": dataOf(t, out)["mfaToken"].(string), "code": totp.Code(seed, time.Now())}); code != 200 {
		t.Fatalf("MFA seed sealed under v1 after the rotation: %d %v", code, out)
	}
	if sk, _, err := akMgr.ResolveSK(oldAK, time.Now()); err != nil || sk != oldSK {
		t.Fatalf("AK sealed under v1 after the rotation: %v", err)
	}
	_, out = c.do("POST", "/api/ak", nil)
	rec, err := akStore.Get(dataOf(t, out)["akId"].(string))
	if err != nil || rec.SKKeyVersion != 2 {
		t.Fatalf("new AK sealed under version %d (%v), want 2", rec.SKKeyVersion, err)
	}
}

// F1 acceptance on MySQL: state survives a web-auth restart, and a RAM-user
// key created in the console verifies in the verify container — a separate
// process that only shares account_db and EULER_KMS_MASTER_KEY — as
// user/id:<n>, which /internal/authz/check then judges by the user's
// attached policies.
func TestSQLRestartAndConsoleKeyVerifies(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-iam"))
	ctx := context.Background()
	t.Setenv(kms.MasterKeyEnv, testRootV1)
	t.Setenv(kms.MasterKeyVersionEnv, "")
	t.Setenv(kms.MasterKeyPreviousEnv, "")
	boot := func() backend {
		t.Helper()
		keys, err := loadKeyring(true)
		if err != nil {
			t.Fatal(err)
		}
		s, err := newSQLStore(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		return backend{store: s, keys: keys, akStore: accesskey.NewSQLStore(ctx, db), locks: sqlLocker{db: db}, db: db}
	}
	useBackend(t, boot())
	c := newFlowClient(t)
	acct := c.signup("restart@euler.emoera.com", "restart-pass")
	_, out := c.do("POST", "/api/ram/users", map[string]any{"username": "ci-bot", "policies": []string{"EuReadOnlyAccess"}})
	uid := int64(dataOf(t, out)["id"].(float64))
	_, out = c.do("POST", "/api/ak", map[string]any{"ramUserId": uid})
	ak, sk := dataOf(t, out)["akId"].(string), dataOf(t, out)["secretKey"].(string)

	// Restart: new process, new keyring object from the same environment.
	installBackend(boot())
	c.login("restart@euler.emoera.com", "restart-pass")
	if _, out := c.do("GET", "/api/ram/users", nil); dataOf(t, out)["total"].(float64) != 1 {
		t.Fatalf("RAM users after restart: %v", out)
	}
	if _, out := c.do("GET", "/api/ak?ramUserId="+strconv.FormatInt(uid, 10), nil); dataOf(t, out)["total"].(float64) != 1 {
		t.Fatalf("AKs after restart: %v", out)
	}

	// The verify container: its own KMS from the environment (cmd/verify
	// akKeyring), the shared access_key table, the real CPS1 chain.
	vk := kms.New()
	if ok, err := vk.LoadMasterKeyFromEnv(kms.PurposeAK); !ok || err != nil {
		t.Fatalf("verify keyring: %v %v", ok, err)
	}
	mgr := accesskey.NewManager(accesskey.NewSQLStore(ctx, db), vk, time.Now)
	now := time.Now()
	query := url.Values{"Action": {"DescribeInstances"}}
	signed, err := cps1.Sign(cps1.Request{
		Method: "POST", Host: "euecs.api.euler.emoera.com", Path: "/", Query: query,
		Headers: map[string]string{"host": "euecs.api.euler.emoera.com", cps1.NonceHeader: "n-restart-1"},
		Body:    []byte(`{}`),
	}, cps1.Credentials{AK: ak, SK: sk}, "cn-north-1", "ecs", now)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"authorization": signed.Authorization}
	for k, v := range signed.Headers {
		headers[strings.ToLower(k)] = v
	}
	v := verify.Verifier{AK: resolverFunc(mgr.ResolveSK), Nonces: onceNonces{}, Accounts: usableAccounts{}}
	id, err := v.Verify(verify.Request{
		Method: "POST", Host: "euecs.api.euler.emoera.com", Path: "/", Query: query, Headers: headers,
		Body: []byte(`{}`), Region: "cn-north-1", Service: "ecs",
	}, now)
	if err != nil {
		t.Fatalf("console-created key rejected by the verifier: %v", err)
	}
	_, rec, _ := mgr.ResolveSK(ak, now)
	if id.AccountID != acct || id.OwnerType != accesskey.OwnerRAMUser || rec.OwnerID != uid {
		t.Fatalf("verified identity %+v owner %d, want account %d RAM user %d", id, rec.OwnerID, acct, uid)
	}
	principal := "user/id:" + strconv.FormatInt(uid, 10) // what cmd/verify emits as X-Euler-Identity
	for action, want := range map[string]bool{"euecs:DescribeInstances": true, "euecs:RunInstances": false} {
		if _, out := c.do("POST", "/internal/authz/check", map[string]any{"account_id": strconv.FormatInt(acct, 10), "identity": principal, "action": action}); out["allow"] != want {
			t.Fatalf("check %s for the RAM-user key: %v, want %v", action, out, want)
		}
	}
}

type resolverFunc func(ak string, now time.Time) (string, accesskey.Record, error)

func (f resolverFunc) ResolveSK(ak string, now time.Time) (string, accesskey.Record, error) {
	return f(ak, now)
}

type onceNonces struct{}

func (onceNonces) SetNX(string, time.Duration) (bool, error) { return true, nil }

type usableAccounts struct{}

func (usableAccounts) IsAccountUsable(int64) (bool, error) { return true, nil }

// The dev seed is idempotent on a persistent store (a restart must not
// duplicate the demo rows) and takes over the placeholder account row the
// verify container's dev seed writes for the same id.
func TestDevSeedIdempotentAndTakesOverPlaceholder(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		ctx := context.Background()
		if _, err := st.CreateAccount(ctx, account{AccountID: 100123, AccountName: "dev-seed-100123", PasswordHash: devSeedPlaceholderHash, Status: 1}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := seedDemoData(ctx); err != nil {
				t.Fatalf("seed run %d: %v", i, err)
			}
		}
		c := newFlowClient(t)
		c.login("admin@euler.emoera.com", "euler123")
		for path, want := range map[string]float64{"/api/ram/users": 2, "/api/ram/roles": 3, "/api/ak": 1} {
			if _, out := c.do("GET", path, nil); dataOf(t, out)["total"].(float64) != want {
				t.Fatalf("%s after two seed runs: %v, want %v rows", path, out, want)
			}
		}
	})
}
