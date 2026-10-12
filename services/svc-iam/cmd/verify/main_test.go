package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/accesskey"
	"github.com/qifalab/euler-platform/cps1"
	"github.com/qifalab/euler-platform/kms"
	"github.com/qifalab/euler-platform/verify"
)

// Regression (I1): the binary only knew -addr while the chart (and every other
// svc-iam binary) passes -http, so the verify container could not be started
// on its port.
func TestParseListenAddr(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, ":9100"},
		{[]string{"-http=:9200"}, ":9200"},
		{[]string{"-addr=:9300"}, ":9300"},                // deprecated alias still works
		{[]string{"-addr=:9300", "-http=:9200"}, ":9200"}, // -http wins
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("verify", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		got, err := parseListenAddr(fs, c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got != c.want {
			t.Errorf("%v: listen %q, want %q", c.args, got, c.want)
		}
	}
}

// strictPathEscape is the wire form an SDK sends (each segment strict RFC
// 3986 encoded, "/" kept) — what the gateway sees in ngx.var.request_uri.
func strictPathEscape(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		var b strings.Builder
		for j := 0; j < len(s); j++ {
			c := s[j]
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
				c == '-' || c == '.' || c == '_' || c == '~' {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
		segs[i] = b.String()
	}
	return strings.Join(segs, "/")
}

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Regression (I2): the gateway forwards the RAW (percent-encoded) path and the
// raw get_uri_args/get_headers shapes. Converting that payload must reproduce
// every shared golden vector — the same fixture the SDKs are pinned to — so
// the gateway path and the SDK can never disagree on the canonical request.
func TestGatewayPayloadVerifiesGoldenVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.FromSlash("../../../../proto-hub/testdata/cps1-golden-vectors.json"))
	if err != nil {
		t.Fatalf("golden fixture: %v", err)
	}
	var fx struct {
		Vectors []struct {
			Name                  string            `json:"name"`
			Method                string            `json:"method"`
			Host                  string            `json:"host"`
			Path                  string            `json:"path"`
			Query                 map[string]string `json:"query"`
			Headers               map[string]string `json:"headers"`
			Body                  string            `json:"body"`
			AK                    string            `json:"ak"`
			SK                    string            `json:"sk"`
			Region                string            `json:"region"`
			Service               string            `json:"service"`
			Date                  string            `json:"date"`
			ExpectedAuthorization string            `json:"expected_authorization"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range fx.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, v.Date)
			if err != nil {
				t.Fatal(err)
			}
			b64 := base64.StdEncoding.EncodeToString([]byte(v.Body))
			req := verifyRequest{
				Method:  v.Method,
				Host:    v.Host,
				Path:    strictPathEscape(v.Path),
				Query:   map[string]json.RawMessage{},
				Headers: map[string]json.RawMessage{"authorization": rawJSON(t, v.ExpectedAuthorization)},
				BodyB64: &b64,
				Region:  v.Region,
				Service: v.Service,
			}
			for k, val := range v.Query {
				req.Query[k] = rawJSON(t, val)
			}
			for k, val := range v.Headers {
				req.Headers[k] = rawJSON(t, val)
			}
			vr, err := req.toVerifyRequest()
			if err != nil {
				t.Fatal(err)
			}
			if vr.Path != v.Path {
				t.Fatalf("decoded path %q, want %q", vr.Path, v.Path)
			}
			err = cps1.Verify(cps1.Request{
				Method: vr.Method, Host: vr.Host, Path: vr.Path, Query: vr.Query,
				Headers: vr.Headers, Body: vr.Body,
			}, cps1.Credentials{AK: v.AK, SK: v.SK}, v.Region, v.Service, now)
			if err != nil {
				t.Fatalf("gateway-shaped payload does not verify: %v", err)
			}
		})
	}
}

func TestNgxValuesShapes(t *testing.T) {
	ok := []struct {
		raw       string
		allowBare bool
		want      []string
	}{
		{`"v"`, true, []string{"v"}},
		{`true`, true, []string{""}},
		{`["1","2"]`, true, []string{"1", "2"}},
		{`[true,"1"]`, true, []string{"", "1"}},
		{`["a","b"]`, false, []string{"a", "b"}},
	}
	for _, c := range ok {
		got, err := ngxValues(json.RawMessage(c.raw), c.allowBare)
		if err != nil || strings.Join(got, "|") != strings.Join(c.want, "|") || len(got) != len(c.want) {
			t.Errorf("ngxValues(%s) = %q, %v; want %q", c.raw, got, err, c.want)
		}
	}
	for _, raw := range []string{`null`, `false`, `1`, `{}`, `[]`, `[null]`, `[["x"]]`} {
		if _, err := ngxValues(json.RawMessage(raw), true); err == nil {
			t.Errorf("ngxValues(%s) accepted", raw)
		}
	}
	if _, err := ngxValues(json.RawMessage(`true`), false); err == nil {
		t.Error("a bare `true` header value was accepted")
	}
}

const (
	tHost    = "euecs.api.euler.emoera.com"
	tRegion  = "cn-north-1"
	tService = "ecs"
)

func newTestApp(t *testing.T) *app {
	t.Helper()
	t.Setenv("EULER_INTERNAL_TOKEN", "")
	k := kms.New()
	if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
		t.Fatal(err)
	}
	mgr := accesskey.NewManager(newMemAKStore(), k, time.Now)
	return &app{
		verifier: &verify.Verifier{AK: akResolver{mgr}, Nonces: newMemNonceStore(), Accounts: allAccountsUsable{}},
		akMgr:    mgr,
	}
}

// gatewayPayload signs (decoded path, url.Values, body) the way an SDK does
// and returns the JSON eu-auth.lua would forward: the encoded wire path,
// the ngx-shaped args (bare keys as true, repeats as arrays) and body_b64.
func gatewayPayload(t *testing.T, ak, sk, nonce, decodedPath, wirePath string, signed url.Values, ngxArgs map[string]any, body []byte) []byte {
	t.Helper()
	res, err := cps1.Sign(cps1.Request{
		Method: "POST", Host: tHost, Path: decodedPath, Query: signed,
		Headers: map[string]string{"host": tHost, cps1.NonceHeader: nonce},
		Body:    body,
	}, cps1.Credentials{AK: ak, SK: sk}, tRegion, tService, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]any{"authorization": res.Authorization, "content-type": "application/octet-stream"}
	for k, v := range res.Headers {
		headers[strings.ToLower(k)] = v
	}
	// A repeated unsigned header arrives as an array.
	headers["accept"] = []string{"application/json", "text/plain"}
	out, err := json.Marshal(map[string]any{
		"method": "POST", "host": tHost, "path": wirePath,
		"query": ngxArgs, "headers": headers,
		"body_b64": base64.StdEncoding.EncodeToString(body),
		"region":   tRegion, "service": tService,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func postVerify(t *testing.T, a *app, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	a.handleVerify(rr, httptest.NewRequest(http.MethodPost, "/internal/openapi/verify", bytes.NewReader(payload)))
	return rr
}

// Regression (I2): repeated query keys, bare keys, encoded (space / UTF-8)
// paths and binary bodies used to fail verification (or 400 at decode).
func TestHandleVerifyAcceptsGatewayShapes(t *testing.T) {
	a := newTestApp(t)
	created, err := a.akMgr.Create(100123, accesskey.OwnerRAMUser, 555)
	if err != nil {
		t.Fatal(err)
	}
	ak, sk := created.Record.AK, created.SKPlaintext
	binary := []byte{0x00, 0xff, 0xfe, 0x80, '"', '\\', 0x01}

	cases := []struct {
		name          string
		decoded, wire string
		signed        url.Values
		args          map[string]any
		body          []byte
	}{
		{"multi-value query", "/", "/",
			url.Values{"Action": {"Run"}, "Tag": {"b", "a"}},
			map[string]any{"Action": "Run", "Tag": []string{"b", "a"}}, nil},
		{"bare key", "/", "/",
			url.Values{"Action": {"Run"}, "dryrun": {""}},
			map[string]any{"Action": "Run", "dryrun": true}, nil},
		{"encoded path", "/buckets/my bucket/文件 1+2", "/buckets/my%20bucket/%E6%96%87%E4%BB%B6%201+2",
			url.Values{"Action": {"Get"}}, map[string]any{"Action": "Get"}, nil},
		{"binary body", "/", "/",
			url.Values{"Action": {"Put"}}, map[string]any{"Action": "Put"}, binary},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := gatewayPayload(t, ak, sk, fmt.Sprintf("n-%d", i), c.decoded, c.wire, c.signed, c.args, c.body)
			rr := postVerify(t, a, p)
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d body %s", rr.Code, rr.Body)
			}
			if got := rr.Header().Get("X-Euler-Account-Id"); got != "100123" {
				t.Fatalf("account id %q", got)
			}
			// A RAM-user key must never come back as the account root.
			if got := rr.Header().Get("X-Euler-Identity"); got != "user/id:555" {
				t.Fatalf("X-Euler-Identity %q, want user/id:555", got)
			}
		})
	}

	// The body is bound: a different body_b64 under the same signature fails.
	p := gatewayPayload(t, ak, sk, "n-tamper", "/", "/", url.Values{"Action": {"Put"}}, map[string]any{"Action": "Put"}, binary)
	var m map[string]any
	_ = json.Unmarshal(p, &m)
	m["body_b64"] = base64.StdEncoding.EncodeToString([]byte{0x00, 0xff})
	p, _ = json.Marshal(m)
	if rr := postVerify(t, a, p); rr.Code != http.StatusForbidden {
		t.Fatalf("tampered binary body: status %d", rr.Code)
	}
}

// A master-account key is the account root: no X-Euler-Identity.
func TestHandleVerifyMasterKeyHasNoPrincipal(t *testing.T) {
	a := newTestApp(t)
	created, err := a.akMgr.Create(100200, accesskey.OwnerMaster, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := gatewayPayload(t, created.Record.AK, created.SKPlaintext, "m-1", "/", "/",
		url.Values{"Action": {"Run"}}, map[string]any{"Action": "Run"}, []byte(`{}`))
	rr := postVerify(t, a, p)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body)
	}
	if got, ok := rr.Header()["X-Euler-Identity"]; ok {
		t.Fatalf("master key carried X-Euler-Identity %q", got)
	}
}

// F5: a key sealed before a master-key rotation still verifies once the new
// root is current and the old one is listed in EULER_KMS_MASTER_KEY_PREVIOUS;
// keys created afterwards are sealed under the new version.
func TestAKKeyringHonoursRotation(t *testing.T) {
	const (
		rootV1 = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
		rootV2 = "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWY="
	)
	t.Setenv(kms.MasterKeyEnv, rootV1)
	t.Setenv(kms.MasterKeyVersionEnv, "")
	t.Setenv(kms.MasterKeyPreviousEnv, "")
	before, err := akKeyring(true)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemAKStore() // stands in for account_db.access_key, shared across the "restart"
	old, err := accesskey.NewManager(store, before, time.Now).Create(100123, accesskey.OwnerMaster, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(kms.MasterKeyEnv, rootV2)
	t.Setenv(kms.MasterKeyVersionEnv, "2")
	t.Setenv(kms.MasterKeyPreviousEnv, "1:"+rootV1)
	after, err := akKeyring(true)
	if err != nil {
		t.Fatal(err)
	}
	mgr := accesskey.NewManager(store, after, time.Now)
	if sk, _, err := mgr.ResolveSK(old.Record.AK, time.Now()); err != nil || sk != old.SKPlaintext {
		t.Fatalf("key sealed under v1 after the rotation: %v", err)
	}
	fresh, err := mgr.Create(100123, accesskey.OwnerMaster, 0)
	if err != nil || fresh.Record.SKKeyVersion != 2 {
		t.Fatalf("new key sealed under version %d (%v), want 2", fresh.Record.SKKeyVersion, err)
	}

	// Without the previous list the old key no longer opens.
	t.Setenv(kms.MasterKeyPreviousEnv, "")
	bare, err := akKeyring(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := accesskey.NewManager(store, bare, time.Now).ResolveSK(old.Record.AK, time.Now()); err == nil {
		t.Fatal("a retired version resolved without being configured")
	}
	// A malformed previous list is a startup error.
	t.Setenv(kms.MasterKeyPreviousEnv, "2:"+rootV1)
	if _, err := akKeyring(true); err == nil {
		t.Fatal("a previous version equal to the current one was accepted")
	}
}
