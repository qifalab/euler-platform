// Euler application cloud control plane. It runs independently of legacy IaaS.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/connectors"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/observability"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/platform"
)

func value(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	key, e := base64.StdEncoding.DecodeString(os.Getenv("EULER_ENCRYPTION_KEY"))
	if e != nil || len(key) != 32 {
		slog.Error("EULER_ENCRYPTION_KEY must be a base64 encoded 32-byte key")
		os.Exit(1)
	}
	databasePath := value("EULER_DATABASE_PATH", "data/app-cloud.db")
	store, e := platform.Open(databasePath, key)
	if e != nil {
		slog.Error("cannot open application cloud database; check storage permissions, integrity and encryption key")
		os.Exit(1)
	}
	defer store.Close()
	products, e := connectors.New(connectors.Config{})
	if e != nil {
		slog.Error("invalid product connector configuration")
		os.Exit(1)
	}
	var administrators []platform.Administrator
	if raw := os.Getenv("EULER_PLATFORM_ADMIN_IDENTITIES"); raw != "" {
		if e = json.Unmarshal([]byte(raw), &administrators); e != nil || len(administrators) > 100 {
			slog.Error("EULER_PLATFORM_ADMIN_IDENTITIES must be a JSON array of provider/subject objects")
			os.Exit(1)
		}
		for _, a := range administrators {
			if strings.TrimSpace(a.Provider) == "" || strings.TrimSpace(a.Subject) == "" {
				slog.Error("platform administrator identity must include provider and subject")
				os.Exit(1)
			}
		}
	}
	dataDir, e := filepath.Abs(value("EULER_APP_DATA_DIR", filepath.Join(filepath.Dir(databasePath), "apps")))
	if e != nil {
		slog.Error("invalid application data directory")
		os.Exit(1)
	}
	if e = os.MkdirAll(dataDir, 0700); e != nil {
		slog.Error("cannot create application data directory")
		os.Exit(1)
	}
	rt := store.ApplicationRuntime(dataDir, value("EULER_PUBLIC_URL", "http://localhost:8080"))
	modules := apps.New(rt)
	if e = apps.CheckVersions(context.Background(), rt.DB, modules); e != nil {
		slog.Error("application schema versions incompatible; verify release and restore plan")
		os.Exit(1)
	}
	for _, m := range modules {
		if e = m.Migrate(context.Background()); e != nil {
			slog.Error("application initialization failed", "application", m.ID())
			os.Exit(1)
		}
		if closer, ok := m.(interface{ Close() error }); ok {
			defer closer.Close()
		}
	}
	if e = apps.RecordVersions(context.Background(), rt.DB, modules); e != nil {
		slog.Error("cannot record application schema versions")
		os.Exit(1)
	}
	workflow := store.NewAutomation()
	if e = workflow.Migrate(context.Background()); e != nil {
		slog.Error("automation initialization failed")
		os.Exit(1)
	}
	// Long-lived workers share service cancellation. New platform workers should
	// start here after their migration and stop before the persistence is closed.
	runCtx, stopWorkers := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	startWorker := func(run func(context.Context)) {
		workers.Add(1)
		go func() { defer workers.Done(); run(runCtx) }()
	}
	defer func() { stopWorkers(); workers.Wait() }()
	startWorker(workflow.Run)
	for _, module := range modules {
		if maintenance, ok := module.(interface{ Maintain(context.Context) error }); ok {
			startWorker(func(ctx context.Context) { runMaintenance(ctx, time.Hour, module.ID(), maintenance.Maintain) })
		}
	}
	mux := http.NewServeMux()
	var auth platform.Authenticator
	var runtime *identity.Runtime
	cfg, configErr := identity.ConfigFromEnv()
	if configErr == nil {
		runtime, e = identity.NewRuntime(cfg, store)
		if e == nil {
			runtime.Register(mux)
			auth = runtime
			startWorker(runtime.Run)
		} else {
			slog.Warn("identity configuration invalid; login disabled")
		}
	} else {
		slog.Warn("identity configuration incomplete; login disabled")
	}
	if auth == nil {
		mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(503)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "identity_unavailable", "message": "Identity provider is not configured or unavailable"}})
		})
	}
	control := platform.NewHandler(store, auth, products, platform.WithApplications(modules, administrators), platform.WithAutomation(workflow))
	mux.Handle("/api/", control)
	mux.Handle("/public/", control)
	operationsToken := os.Getenv("EULER_OPERATIONS_TOKEN")
	if operationsToken != "" && (len(operationsToken) < 32 || len(operationsToken) > 4096 || strings.TrimSpace(operationsToken) != operationsToken) {
		slog.Error("EULER_OPERATIONS_TOKEN must contain 32 to 4096 bytes without surrounding whitespace")
		os.Exit(1)
	}
	checks := []observability.Check{}
	for _, module := range modules {
		probe, ok := module.(interface {
			DependencyStatus(context.Context) map[string]string
		})
		if !ok {
			continue
		}
		var names []string
		switch module.ID() {
		case "database":
			names = []string{"mysql", "postgresql"}
		case "storage":
			names = []string{"s3"}
		}
		for _, name := range names {
			checks = append(checks, observability.Check{Name: name, Probe: func(ctx context.Context) string { return probe.DependencyStatus(ctx)[name] }})
		}
	}
	monitor := observability.New(observability.Config{
		CoreCheck: store.Ping, Checks: checks,
		Authorize:      operationsAuthorizer(auth, administrators, operationsToken),
		LoginAvailable: func() bool { return runtime != nil && runtime.LoginAvailable() },
		IdentityStatus: func() any {
			if runtime != nil {
				return runtime.Status()
			}
			state := "not_configured"
			if strings.TrimSpace(os.Getenv("EULER_OIDC_ISSUER")) != "" {
				state = "invalid_configuration"
			}
			return map[string]any{"state": state, "loginAvailable": false, "sessionValidationAvailable": false}
		},
	})
	monitor.Register(mux)
	startWorker(monitor.Run)
	srv := &http.Server{Addr: value("EULER_HTTP_ADDR", ":8080"), Handler: monitor.Middleware(mux), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	slog.Info("application cloud listening", "address", srv.Addr, "identityConfigured", auth != nil)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	select {
	case <-stop:
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server failed; check listen address and port availability")
			os.Exit(1)
		}
	}
	monitor.Stop()
	stopWorkers()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if e = srv.Shutdown(ctx); e != nil {
		slog.Error("HTTP shutdown incomplete")
		_ = srv.Close()
	}
}

// Monitoring automation gets a separate read-only bearer; browser access checks
// the same exact provider/subject identities used by platform administration.
// This token never authorizes business APIs or application mutations.
func operationsAuthorizer(auth platform.Authenticator, admins []platform.Administrator, token string) func(*http.Request) int {
	want := sha256.Sum256([]byte(token))
	return func(r *http.Request) int {
		if values := r.Header.Values("Authorization"); len(values) > 0 {
			if len(values) != 1 || token == "" || !strings.HasPrefix(values[0], "Bearer ") {
				return http.StatusUnauthorized
			}
			got := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
			if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
				return 0
			}
			return http.StatusUnauthorized
		}
		if auth == nil {
			return http.StatusUnauthorized
		}
		principal, _, err := auth.Authenticate(r)
		if err != nil {
			return http.StatusUnauthorized
		}
		for _, admin := range admins {
			if principal.Provider == admin.Provider && principal.Subject == admin.Subject {
				return 0
			}
		}
		return http.StatusForbidden
	}
}

// Maintenance belongs to the module but scheduling and shutdown belong to the
// service. Bounded batches may continue on the next tick after a failure.
func runMaintenance(ctx context.Context, interval time.Duration, application string, maintain func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		work, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := maintain(work)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("application maintenance failed", "application", application)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
