// Command svc-iam-verify runs the internal OpenAPI verification endpoint that
// APISIX forward-auth bypasses to (04§3.4).
//
// In the target architecture this endpoint is part of svc-iam (Go/Kratos). This Go
// binary is the runnable reference implementation and the shared contract
// owner: it uses the same pkg-go/cps1 signer the SDK and doc-site examples
// use, so the three cannot drift (03§9.4 rule ⑤).
//
// Route: POST /internal/openapi/verify — listens on -http (default :9100), the
// `verify` container of the svc-iam pod; web-auth is the other container on
// :9101. The deprecated -addr flag is still accepted as an alias.
// Budget: P99 < 10ms — local signature computation plus cached AK metadata.
// It must never touch the database synchronously on this path.
//
// On success it returns 200 with the identity headers the gateway injects
// upstream (07§3.3): X-Euler-Account-Id, X-Euler-Identity, X-Euler-TraceId, plus
// X-Euler-Ak-Id and X-Euler-Quota-Qps for per-AK rate limiting.
// On failure it returns 403 with the error code from 03§9.3.
package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/accesskey"
	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/kms"
	"github.com/qifalab/euler-platform/runmode"
	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/sts"
	"github.com/qifalab/euler-platform/verify"
)

// defaultListenAddr is the verify container's port (chart: svc-iam Service
// port "verify"); web-auth owns :9101 in the same pod.
const defaultListenAddr = ":9100"

// parseListenAddr reads the listen address from -http — the flag every
// svc-iam binary and the chart use — keeping -addr as a deprecated alias so an
// old invocation still starts. -http wins when both are given.
func parseListenAddr(fs *flag.FlagSet, args []string) (string, error) {
	httpAddr := fs.String("http", defaultListenAddr, "HTTP listen address")
	legacy := fs.String("addr", "", "deprecated alias for -http")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["addr"] && !set["http"] {
		slog.Warn("-addr is deprecated; use -http", "addr", *legacy)
		return *legacy, nil
	}
	return *httpAddr, nil
}

func main() {
	listen, err := parseListenAddr(flag.CommandLine, os.Args[1:])
	if err != nil {
		os.Exit(2)
	}
	addr := &listen
	// Production refuses to start without the internal shared secret: the
	// X-Euler-Account-Id this service trusts must provably come from the gateway.
	runtimeRequireSecrets()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// TODO(svc-iam): replace the in-memory stores with Vitess (vtgate)-backed
	// repositories (account_id single-key sharding) and the Redis L2 caches
	// described in 07§2.5 / §3.5. The verification logic itself is unchanged —
	// that is the point of keeping it behind the ports.
	app, err := newApp()
	if err != nil {
		slog.Error("bootstrap failed", "err", err)
		os.Exit(1)
	}

	// Dev-only: provision a demo AK/SK so the e2e client has credentials to
	// sign with. A real svc-iam never mints keys at boot and never logs an SK —
	// the SK is shown exactly once, to the user, at CreateAccessKey time.
	if runmode.DevSeedEnabled() || (os.Getenv("EULER_DEV_SEED_AK") == "1" && !runmode.Production()) {
		created, err := app.akMgr.Create(100123, accesskey.OwnerRAMUser, 555)
		if err != nil {
			slog.Error("seed AK failed", "err", err)
			os.Exit(1)
		}
		if app.db != nil {
			if err := devSeedAccountRow(context.Background(), app.db, 100123); err != nil {
				slog.Error("seed account row failed", "err", err)
				os.Exit(1)
			}
		}
		slog.Warn("DEV SEED — not for production",
			"ak", created.Record.AK,
			"sk", created.SKPlaintext,
			"account_id", created.Record.AccountID)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/internal/openapi/verify", app.handleVerify)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	slog.Info("svc-iam verify endpoint listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listen failed", "err", err)
		os.Exit(1)
	}
}

type app struct {
	verifier *verify.Verifier
	akMgr    *accesskey.Manager
	db       *sql.DB // nil in memory mode
}

func newApp() (*app, error) {
	ctx := context.Background()
	// The AK store decides where credentials live. Persistence is opt-in
	// (pkg-go/storage doc): with EULER_DB_DSN set, keys live in account_db so a
	// restarted verifier still resolves every live key; unset, the in-memory
	// store keeps the demo and `go test` dependency-free.
	//
	// A configured DSN that cannot be reached is a startup failure: a verifier
	// that cannot read credentials must not pretend to authenticate anyone.
	db, persistent, err := storage.MustOpenFor(ctx, "account_db")
	if err != nil {
		return nil, err
	}
	if !persistent && runmode.Production() {
		return nil, fmt.Errorf("%s is required in production: the in-memory stores cannot freeze accounts or share replay state across replicas", storage.DSNEnv)
	}

	k, err := akKeyring(persistent)
	if err != nil {
		return nil, err
	}

	var store accesskey.Store
	verifier := &verify.Verifier{}
	if persistent {
		if err := storage.EnsureMigrated(ctx, db, "account_db"); err != nil {
			return nil, err
		}
		store = accesskey.NewSQLStore(ctx, db)
		// Replay protection and account status come from account_db, shared
		// by every replica. (They used to be a per-process map and an
		// "every account is usable" stub.)
		verifier.Nonces = newSQLNonceStore(db)
		verifier.Accounts = newSQLAccountStatus(db)
		slog.Info("svc-iam verify AK store ready", "persistent", true)
	} else {
		store = newMemAKStore()
		verifier.Nonces = newMemNonceStore()
		verifier.Accounts = allAccountsUsable{}
		slog.Warn("svc-iam verify running in-memory: no account freeze, per-replica replay state (dev only)")
		db = nil
	}
	mgr := accesskey.NewManager(store, k, time.Now)
	verifier.AK = akResolver{mgr}

	// STS: temporary credentials are sealed with EULER_STS_KEY (shared with
	// web-auth, the issuer). Without it, requests carrying a security token
	// are rejected — never accepted on trust.
	if key, ok, err := sts.KeyFromEnv(); err != nil {
		return nil, err
	} else if ok {
		issuer, err := sts.NewIssuer(sts.DefaultTTL, nil, key)
		if err != nil {
			return nil, err
		}
		verifier.STS = stsValidator{issuer}
	}

	return &app{verifier: verifier, akMgr: mgr, db: db}, nil
}

// akKeyring loads the AK master key. It must be stable: SKs are stored sealed
// with it. A per-process random key (the old behaviour) made every persisted
// SK undecryptable after a restart and differed between replicas. During a
// master-key rotation EULER_KMS_MASTER_KEY_PREVIOUS keeps the retired
// versions available, so keys sealed before the rotation still verify — the
// same root web-auth seals console-created keys with.
func akKeyring(persistent bool) (*kms.KMS, error) {
	k := kms.New()
	configured, err := k.LoadMasterKeyFromEnv(kms.PurposeAK)
	if err != nil {
		return nil, err
	}
	if !configured {
		if persistent || runmode.Production() {
			return nil, fmt.Errorf("%s is required when AK secrets are persisted (or in production)", kms.MasterKeyEnv)
		}
		if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
			return nil, err
		}
		slog.Warn("no KMS master key configured — using an ephemeral one (in-memory dev mode only)", "set", kms.MasterKeyEnv)
	}
	return k, nil
}

// verifyRequest is the JSON body APISIX forward-auth (eu-auth.lua) sends.
//
// The values are what OpenResty hands the plugin, not a normalised form:
//
//   - Path is the request path exactly as received on the wire
//     (ngx.var.request_uri up to "?"), still percent-encoded. It is decoded
//     exactly once, here: cps1 canonicalises the DECODED path (golden vector
//     "path-with-encoding" signs "/buckets/my bucket/objects"), so passing the
//     encoded form through double-encoded every escaped octet.
//   - Query values come from ngx.req.get_uri_args(): a string, `true` for a
//     bare key ("?flag" — signed as "flag="), or an array for a repeated key.
//   - Header values come from ngx.req.get_headers(): a string, or an array for
//     a repeated header (joined with "," per RFC 9110 field combination).
//   - BodyB64 is the exact body bytes, base64 (binary bodies do not survive a
//     JSON string). Body is the legacy plain-string form, used only when
//     body_b64 is absent.
type verifyRequest struct {
	Method  string                     `json:"method"`
	Host    string                     `json:"host"`
	Path    string                     `json:"path"`
	Query   map[string]json.RawMessage `json:"query"`
	Headers map[string]json.RawMessage `json:"headers"`
	Body    string                     `json:"body"`
	BodyB64 *string                    `json:"body_b64"`
	Region  string                     `json:"region"`  // resolved by the gateway from the routed subdomain
	Service string                     `json:"service"` // never taken from client input
}

// toVerifyRequest converts the gateway payload into the verifier's input.
func (req verifyRequest) toVerifyRequest() (verify.Request, error) {
	path, err := url.PathUnescape(req.Path)
	if err != nil {
		return verify.Request{}, fmt.Errorf("path: %w", err)
	}
	q := url.Values{}
	for k, raw := range req.Query {
		vals, err := ngxValues(raw, true)
		if err != nil {
			return verify.Request{}, fmt.Errorf("query %q: %w", k, err)
		}
		q[k] = vals
	}
	hdrs := make(map[string]string, len(req.Headers))
	for k, raw := range req.Headers {
		vals, err := ngxValues(raw, false)
		if err != nil {
			return verify.Request{}, fmt.Errorf("header %q: %w", k, err)
		}
		lk := strings.ToLower(k)
		if _, dup := hdrs[lk]; dup {
			// Two spellings of one header name: which one wins would depend
			// on map order, so refuse rather than guess.
			return verify.Request{}, fmt.Errorf("header %q given twice", lk)
		}
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		hdrs[lk] = strings.Join(vals, ",")
	}
	body := []byte(req.Body)
	if req.BodyB64 != nil {
		body, err = base64.StdEncoding.DecodeString(*req.BodyB64)
		if err != nil {
			return verify.Request{}, fmt.Errorf("body_b64: %w", err)
		}
	}
	return verify.Request{
		Method:  req.Method,
		Host:    req.Host,
		Path:    path,
		Query:   q,
		Headers: hdrs,
		Body:    body,
		Region:  req.Region,
		Service: req.Service,
	}, nil
}

// ngxValues decodes one OpenResty arg/header value: a string, an array of
// strings (repeated key), or — for query args only — `true` for a bare key,
// which is signed as an empty value. Anything else (null, false, numbers,
// objects, empty arrays) is refused: it cannot come from OpenResty, and
// guessing a meaning would make the canonical request ambiguous.
func ngxValues(raw json.RawMessage, allowBare bool) ([]string, error) {
	one := func(r json.RawMessage) (string, error) {
		switch t := strings.TrimSpace(string(r)); {
		case t == "true" && allowBare:
			return "", nil
		case strings.HasPrefix(t, `"`):
			var s string
			if err := json.Unmarshal(r, &s); err != nil {
				return "", err
			}
			return s, nil
		default:
			return "", fmt.Errorf("unsupported value %s", t)
		}
	}
	if t := strings.TrimSpace(string(raw)); strings.HasPrefix(t, "[") {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, err
		}
		if len(arr) == 0 {
			return nil, errors.New("empty value list")
		}
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			s, err := one(e)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	}
	s, err := one(raw)
	if err != nil {
		return nil, err
	}
	return []string{s}, nil
}

// keyCapture records the AK row the verifier resolved, so the response can
// name the key's owner. pkg-go/verify's Identity carries the owner TYPE but
// not the owner id; one capture per request (the Verifier is copied).
type keyCapture struct {
	inner verify.AKResolver
	rec   accesskey.Record
}

func (c *keyCapture) ResolveSK(ak string, now time.Time) (string, accesskey.Record, error) {
	sk, rec, err := c.inner.ResolveSK(ak, now)
	c.rec = rec
	return sk, rec, err
}

// keyPrincipal names the identity a long-lived key acts as. A master-account
// key is the account root (""). A RAM-user or role key MUST carry a
// principal: an empty X-Euler-Identity is what eu-authorize and
// /internal/authz/check treat as the account root, so leaving it empty handed
// every sub-user key root permissions. "id:" marks a numeric owner id
// (iam_user.id / iam_role.id) as opposed to a name.
func keyPrincipal(ot accesskey.OwnerType, ownerID int64) (string, bool) {
	switch ot {
	case accesskey.OwnerMaster:
		return "", true
	case accesskey.OwnerRAMUser:
		return "user/id:" + strconv.FormatInt(ownerID, 10), true
	case accesskey.OwnerRole:
		return "role/id:" + strconv.FormatInt(ownerID, 10), true
	}
	return "", false
}

type verifyResponse struct {
	Code    string `json:"Code,omitempty"`
	Message string `json:"Message,omitempty"`
}

func (a *app) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// TRUST BOUNDARY: this is an internal endpoint reached only via APISIX
	// forward-auth; the network layer must not expose it publicly. The
	// identity headers it returns (X-Euler-Account-Id, ...) are injected by the
	// gateway upstream and trusted by backend services solely because the
	// gateway strips any client-supplied copies. Optionally, setting
	// EULER_INTERNAL_TOKEN requires the gateway to present the shared secret in
	// X-Euler-Internal-Token (dev default: disabled).
	if want := os.Getenv("EULER_INTERNAL_TOKEN"); want != "" {
		got := r.Header.Get("X-Euler-Internal-Token")
		if subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
			slog.Warn("internal token mismatch on /internal/openapi/verify", "remote", r.RemoteAddr)
			writeErr(w, "IAM.AccessDenied", "access denied")
			return
		}
	}
	// Cap the body (defense-in-depth; the gateway already limits bodies).
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req verifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Info("verify request decode failed", "err", err)
		writeErr(w, "Common.InvalidParameter", "malformed verify request")
		return
	}

	vreq, err := req.toVerifyRequest()
	if err != nil {
		slog.Info("verify request malformed", "err", err)
		writeErr(w, "Common.InvalidParameter", "malformed verify request")
		return
	}

	v := *a.verifier
	capture := &keyCapture{inner: v.AK}
	v.AK = capture
	identity, err := v.Verify(vreq, time.Now())

	if err != nil {
		// The response is deliberately uniform — it does not distinguish
		// SignatureDoesNotMatch / InvalidAccessKeyId / AccountUnusable /
		// replay, to avoid handing an attacker an oracle. The precise reason
		// goes to the log only.
		slog.Info("verify rejected", "reason", err.Error(), "path", req.Path, "region", req.Region, "service", req.Service)
		writeErr(w, "IAM.AccessDenied", "access denied")
		return
	}

	principal := identity.Principal // STS: "role/<RoleName>"
	if principal == "" {
		p, ok := keyPrincipal(identity.OwnerType, capture.rec.OwnerID)
		if !ok {
			slog.Error("verify: access key has an unknown owner type", "ak", identity.AKID, "owner_type", identity.OwnerType)
			writeErr(w, "IAM.AccessDenied", "access denied")
			return
		}
		principal = p
	}

	// Identity headers the gateway injects upstream (07§3.3).
	w.Header().Set("X-Euler-Account-Id", strconv.FormatInt(identity.AccountID, 10))
	w.Header().Set("X-Euler-Ak-Id", identity.AKID)
	if principal != "" {
		w.Header().Set("X-Euler-Identity", principal)
	}
	w.WriteHeader(http.StatusOK)
}

func writeErr(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(verifyResponse{Code: code, Message: msg})
}

// --- in-memory ports (replaced by Vitess (vtgate)/Redis in svc-iam) ---

type akResolver struct{ mgr *accesskey.Manager }

func (a akResolver) ResolveSK(ak string, now time.Time) (string, accesskey.Record, error) {
	return a.mgr.ResolveSK(ak, now)
}

type memAKStore struct {
	mu   sync.RWMutex
	rows map[string]accesskey.Record
}

func newMemAKStore() *memAKStore { return &memAKStore{rows: make(map[string]accesskey.Record)} }

func (m *memAKStore) Insert(r accesskey.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[r.AK] = r
	return nil
}

func (m *memAKStore) Get(ak string) (accesskey.Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.rows[ak]
	if !ok {
		return accesskey.Record{}, accesskey.ErrNotFound
	}
	return r, nil
}

func (m *memAKStore) ListByOwner(accountID int64, ot accesskey.OwnerType, oid int64) ([]accesskey.Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []accesskey.Record
	for _, r := range m.rows {
		if r.AccountID == accountID && r.OwnerType == ot && r.OwnerID == oid {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memAKStore) Update(r accesskey.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[r.AK] = r
	return nil
}

// memNonceStore stands in for Redis SET NX with TTL. Production uses Redis
// Cluster; entries expire after verify.NonceTTL (16 min). Expired entries are
// lazily evicted (a full sweep at most once per minute) so the map does not
// grow without bound.
type memNonceStore struct {
	mu        sync.Mutex
	seen      map[string]time.Time
	lastSweep time.Time
}

func newMemNonceStore() *memNonceStore {
	return &memNonceStore{seen: make(map[string]time.Time), lastSweep: time.Now()}
}

func (m *memNonceStore) SetNX(key string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if now.Sub(m.lastSweep) > time.Minute {
		for k, exp := range m.seen {
			if now.After(exp) {
				delete(m.seen, k)
			}
		}
		m.lastSweep = now
	}
	if exp, ok := m.seen[key]; ok && now.Before(exp) {
		return false, nil
	}
	m.seen[key] = now.Add(ttl)
	return true, nil
}

// allAccountsUsable is the in-memory dev stand-in (there is no account store
// without a database). newApp refuses to use it in production.
type allAccountsUsable struct{}

func (allAccountsUsable) IsAccountUsable(int64) (bool, error) { return true, nil }

// runtimeRequireSecrets enforces the production secret set (pkg-go/runmode).
func runtimeRequireSecrets() {
	runmode.MustRequireSecrets(httpmw.InternalTokenEnv)
}
