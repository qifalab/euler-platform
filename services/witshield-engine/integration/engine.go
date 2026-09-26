// Package integration embeds the complete Apache-2.0 WitShield engine in Euler.
// It deliberately does not expose the standalone administrator/session router.
package integration

import (
	"context"
	"errors"
	"fmt"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/httpapi"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/scheduler"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/secret"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/store"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Config struct {
	DataDir string
	Key     []byte
	Version string
	Enabled func(context.Context) bool
}
type Route = httpapi.ManagementRoute

func ManagementRoutes() []Route { return httpapi.ManagementRoutes() }

type Engine struct {
	api      *httpapi.Server
	db       *store.Store
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	once     sync.Once
	closeErr error
}

// New creates an isolated controller. Key must be unique to the installation
// and durable across platform restarts and backups.
func New(parent context.Context, cfg Config) (*Engine, error) {
	if cfg.DataDir == "" || !filepath.IsAbs(cfg.DataDir) {
		return nil, errors.New("absolute engine data directory required")
	}
	if cfg.Enabled == nil {
		return nil, errors.New("installation enabled gate required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.DataDir, 0700); err != nil {
		return nil, err
	}
	vault, err := secret.New(cfg.Key)
	if err != nil {
		return nil, err
	}
	filename := filepath.Join(cfg.DataDir, "witshield.db")
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Chmod(filename, 0600); err != nil {
		return nil, err
	}
	db, err := store.Open(parent, filename)
	if err != nil {
		return nil, err
	}
	api, err := httpapi.New(httpapi.Config{Store: db, Vault: vault, Version: cfg.Version, Enabled: cfg.Enabled})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	e := &Engine{api: api, db: db, cancel: cancel}
	sched := scheduler.New(db, slog.Default())
	sched.SetAllowed(cfg.Enabled)
	sched.SetObserver(func(err error) { api.MarkWorkerHealth("scheduler", err) })
	e.start(ctx, "scheduler", sched.Run)
	e.start(ctx, "notifications", api.RunNotificationWorker)
	e.start(ctx, "security_engineer", api.RunSecurityEngineerWorker)
	e.start(ctx, "maintenance", e.maintain)
	return e, nil
}
func (e *Engine) start(ctx context.Context, name string, run func(context.Context) error) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("Euler WitShield worker stopped", "worker", name, "error", fmt.Sprintf("%T", err))
		}
	}()
}
func (e *Engine) maintain(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	compact := time.NewTicker(6 * time.Hour)
	defer compact.Stop()
	if err := e.db.Compact(ctx, time.Now().UTC()); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("Euler WitShield compaction failed")
	}
	for {
		// Expiry, lease recovery and retention continue while disabled. This loop
		// dispatches no new device work; local Helper safety rollback remains active.
		e.api.MarkWorkerHealth("maintenance", e.db.Maintain(ctx, time.Now().UTC()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-compact.C:
			if err := e.db.Compact(ctx, time.Now().UTC()); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("Euler WitShield compaction failed")
			}
		}
	}
}
func (e *Engine) ServeManagement(w http.ResponseWriter, r *http.Request, actorID string) {
	e.api.EulerManagementHandler(actorID).ServeHTTP(w, r)
}
func (e *Engine) ServeAgent(w http.ResponseWriter, r *http.Request, originalURI string) {
	e.api.EulerAgentHandler(originalURI).ServeHTTP(w, r)
}
func (e *Engine) Close() error {
	e.once.Do(func() { e.cancel(); e.wg.Wait(); e.closeErr = e.db.Close() })
	return e.closeErr
}
