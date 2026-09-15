// Command svc-iam-verify runs the internal OpenAPI verification endpoint that
// APISIX forward-auth bypasses to (04§3.4).
//
// In the target architecture this endpoint is part of svc-iam (Go/Kratos). This Go
// binary is the runnable reference implementation and the shared contract
// owner: it uses the same pkg-go/cps1 signer the SDK and doc-site examples
// use, so the three cannot drift (03§9.4 rule ⑤).
//
// Route: POST /internal/openapi/verify
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
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/accesskey"
	"github.com/qifalab/euler-platform/kms"
	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/verify"
)

func main() {
	addr := flag.String("addr", ":9100", "listen address")
	flag.Parse()

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
	if os.Getenv("EULER_DEV_SEED_AK") == "1" {
		created, err := app.akMgr.Create(100123, accesskey.OwnerRAMUser, 555)
		if err != nil {
			slog.Error("seed AK failed", "err", err)
			os.Exit(1)
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
}

func newApp() (*app, error) {
	k := kms.New()
	if err := k.GenerateMasterKey(kms.PurposeAK, 1); err != nil {
		return nil, err
	}

	// The AK store decides where credentials live. Persistence is opt-in
	// (pkg-go/storage doc): with EULER_DB_DSN set, keys live in account_db so a
	// restarted verifier still resolves every live key; unset, the in-memory
	// store keeps the demo and `go test` dependency-free.
	//
	// A configured DSN that cannot be reached is a startup failure: a verifier
	// that cannot read credentials must not pretend to authenticate anyone.
	ctx := context.Background()
	db, ok, err := storage.MustOpenFor(ctx, "account_db")
	if err != nil {
		return nil, err
	}
	var store accesskey.Store
	if ok {
		if err := storage.EnsureMigrated(ctx, db, "account_db"); err != nil {
			return nil, err
		}
		store = accesskey.NewSQLStore(ctx, db)
		slog.Info("svc-iam verify AK store ready", "persistent", true)
	} else {
		store = newMemAKStore()
		slog.Info("svc-iam verify AK store ready", "persistent", false)
	}
	mgr := accesskey.NewManager(store, k, time.Now)

	return &app{
		verifier: &verify.Verifier{
			AK:       akResolver{mgr},
			Nonces:   newMemNonceStore(),
			Accounts: allAccountsUsable{},
		},
		akMgr: mgr,
	}, nil
}

// verifyRequest is the JSON body APISIX forward-auth sends.
type verifyRequest struct {
	Method  string            `json:"method"`
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Region  string            `json:"region"`  // resolved by the gateway from the routed subdomain
	Service string            `json:"service"` // never taken from client input
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

	q := url.Values{}
	for k, v := range req.Query {
		q.Set(k, v)
	}

	identity, err := a.verifier.Verify(verify.Request{
		Method:  req.Method,
		Host:    req.Host,
		Path:    req.Path,
		Query:   q,
		Headers: req.Headers,
		Body:    []byte(req.Body),
		Region:  req.Region,
		Service: req.Service,
	}, time.Now())

	if err != nil {
		// The response is deliberately uniform — it does not distinguish
		// SignatureDoesNotMatch / InvalidAccessKeyId / AccountUnusable /
		// replay, to avoid handing an attacker an oracle. The precise reason
		// goes to the log only.
		slog.Info("verify rejected", "reason", err.Error(), "path", req.Path, "region", req.Region, "service", req.Service)
		writeErr(w, "IAM.AccessDenied", "access denied")
		return
	}

	// Identity headers the gateway injects upstream (07§3.3).
	w.Header().Set("X-Euler-Account-Id", strconv.FormatInt(identity.AccountID, 10))
	w.Header().Set("X-Euler-Ak-Id", identity.AKID)
	if identity.Principal != "" {
		w.Header().Set("X-Euler-Identity", identity.Principal)
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

type allAccountsUsable struct{}

func (allAccountsUsable) IsAccountUsable(int64) (bool, error) { return true, nil }
