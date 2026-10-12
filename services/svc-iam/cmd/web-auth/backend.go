package main

// Storage and key wiring (F1). memoryBackend is the default (dev servers,
// `go test`); main switches to account_db with EULER_DB_DSN, exactly like
// every other service (pkg-go/storage: persistence is opt-in).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/accesskey"
	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/kms"
	"github.com/qifalab/euler-platform/storage"
	"github.com/qifalab/euler-platform/totp"
)

// The process-wide wiring the handlers use. installBackend sets all of it at
// once (tests swap backends the same way).
var (
	st authStore
	// keyring seals MFA seeds (kms.PurposeUser) and AK secrets
	// (kms.PurposeAK). With persistence it is derived from
	// EULER_KMS_MASTER_KEY (+ _PREVIOUS for rotation) — the same root the
	// verify container loads, which is what lets it open a console-created
	// key.
	keyring *kms.KMS
	// akStore / akMgr are THE AccessKey store and lifecycle cmd/verify reads
	// (pkg-go/accesskey over account_db.access_key, or memory).
	akStore accesskey.Store
	akMgr   *accesskey.Manager
	// akLocks serialises AK mutations per account (the 2-key cap is a
	// read-then-insert): process-local in memory, a MySQL named lock across
	// replicas.
	akLocks   keyedLocker
	backendDB *sql.DB // nil in memory mode

	// totpVerifier's replay memory is process-local — restarting reopens a
	// ≤90s replay window, the standard trade.
	totpVerifier = totp.NewVerifier()
)

func init() { installBackend(memoryBackend()) }

// backend bundles one coherent wiring.
type backend struct {
	store   authStore
	keys    *kms.KMS
	akStore accesskey.Store
	locks   keyedLocker
	db      *sql.DB
}

// installBackend makes b the process wiring and returns the previous one.
func installBackend(b backend) backend {
	prev := backend{store: st, keys: keyring, akStore: akStore, locks: akLocks, db: backendDB}
	st, keyring, akStore, akLocks, backendDB = b.store, b.keys, b.akStore, b.locks, b.db
	if b.akStore != nil {
		akMgr = accesskey.NewManager(b.akStore, b.keys, func() time.Time { return storeTime(time.Now()) })
	}
	return prev
}

// memoryBackend is the dependency-free wiring with ephemeral keys.
func memoryBackend() backend {
	k := kms.New()
	for _, p := range []kms.Purpose{kms.PurposeUser, kms.PurposeAK} {
		if err := k.GenerateMasterKey(p, 1); err != nil {
			panic("kms bootstrap: " + err.Error())
		}
	}
	return backend{store: newMemStore(), keys: k, akStore: accesskey.NewMemStore(), locks: &memLocker{}}
}

// loadKeyring loads both purposes from EULER_KMS_MASTER_KEY (+ _VERSION,
// _PREVIOUS). required makes an unset key an error; otherwise an unset key
// yields an ephemeral keyring (memory mode outside production only).
func loadKeyring(required bool) (*kms.KMS, error) {
	k := kms.New()
	for _, p := range []kms.Purpose{kms.PurposeUser, kms.PurposeAK} {
		configured, err := k.LoadMasterKeyFromEnv(p)
		if err != nil {
			return nil, err
		}
		if configured {
			continue
		}
		if required {
			return nil, fmt.Errorf("%s is required when web-auth persists sealed secrets (MFA seeds, AccessKey SKs) or runs in production", kms.MasterKeyEnv)
		}
		if err := k.GenerateMasterKey(p, 1); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// backendFromEnv builds main's wiring. With EULER_DB_DSN it opens account_db
// (an unreachable or unmigrated database is a startup failure); production
// without it is refused — a restart would forget every account.
func backendFromEnv(ctx context.Context, production bool) (backend, error) {
	db, persistent, err := storage.MustOpenFor(ctx, "account_db")
	if err != nil {
		return backend{}, err
	}
	if !persistent && production {
		return backend{}, fmt.Errorf("%s is required in production: the in-memory store forgets every account, session and AccessKey on restart", storage.DSNEnv)
	}
	keys, err := loadKeyring(persistent || production)
	if err != nil {
		return backend{}, err
	}
	if !persistent {
		slog.Warn("svc-iam web-auth running in-memory: accounts, sessions, RAM data and AccessKeys are lost on restart and invisible to the verify container (dev only)", "set", storage.DSNEnv)
		return memoryBackend().withKeys(keys), nil
	}
	if strings.TrimSpace(os.Getenv(identityPepperEnv)) == "" {
		return backend{}, fmt.Errorf("%s is required with persistence: identity fingerprints and reset-code hashes outlive the process", identityPepperEnv)
	}
	if err := storage.EnsureMigrated(ctx, db, "account_db"); err != nil {
		return backend{}, err
	}
	s, err := newSQLStore(ctx, db)
	if err != nil {
		return backend{}, err
	}
	slog.Info("svc-iam web-auth store ready", "persistent", true, "schema", storage.Schema())
	return backend{store: s, keys: keys, akStore: accesskey.NewSQLStore(ctx, db), locks: sqlLocker{db: db}, db: db}, nil
}

func (b backend) withKeys(k *kms.KMS) backend { b.keys = k; return b }

// productionSettings are the variables web-auth refuses to start without in
// production (pkg-go/runmode). The JWT signing key is checked separately (it
// can come from a file).
func productionSettings() []string {
	return []string{httpmw.InternalTokenEnv, storage.DSNEnv, kms.MasterKeyEnv, identityPepperEnv, notifyURLEnv}
}

// --- per-account locks ---------------------------------------------------------

// keyedLocker serialises a critical section per key.
type keyedLocker interface {
	Lock(ctx context.Context, key string) (unlock func(), err error)
}

// memLocker is one process-wide mutex: memory mode is a single process, and
// AK mutations are rare.
type memLocker struct{ mu sync.Mutex }

func (l *memLocker) Lock(context.Context, string) (func(), error) {
	l.mu.Lock()
	return l.mu.Unlock, nil
}

// sqlLocker takes a MySQL named lock (GET_LOCK) on a pinned connection, so
// the section is exclusive across every web-auth replica.
type sqlLocker struct{ db *sql.DB }

var errLockTimeout = errors.New("lock: timed out")

const sqlLockWait = 5 // seconds

func (l sqlLocker) Lock(ctx context.Context, key string) (func(), error) {
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	name := "euler-iam:" + key
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`, name, sqlLockWait).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !got.Valid || got.Int64 != 1 {
		_ = conn.Close()
		return nil, errLockTimeout
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), `DO RELEASE_LOCK(?)`, name)
		_ = conn.Close()
	}, nil
}

// akLockKey names the per-account AK lock.
func akLockKey(accountID int64) string { return "ak:" + strconv.FormatInt(accountID, 10) }
