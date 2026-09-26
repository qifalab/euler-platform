// Euler application cloud control plane. It runs independently of legacy IaaS.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/connectors"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
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
		slog.Error("cannot open application cloud database", "error", e)
		os.Exit(1)
	}
	defer store.Close()
	products, e := connectors.New(connectors.Config{})
	if e != nil {
		slog.Error("invalid product connector configuration", "error", e)
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
	modules := apps.New(store.ApplicationRuntime(dataDir, value("EULER_PUBLIC_URL", "http://localhost:8080")))
	for _, m := range modules {
		if e = m.Migrate(context.Background()); e != nil {
			slog.Error("application initialization failed", "application", m.ID(), "error", e)
			os.Exit(1)
		}
		if closer, ok := m.(interface{ Close() error }); ok {
			defer closer.Close()
		}
	}
	mux := http.NewServeMux()
	var auth platform.Authenticator
	cfg, configErr := identity.ConfigFromEnv()
	if configErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		manager, err := identity.New(ctx, cfg, store)
		cancel()
		if err == nil {
			manager.Register(mux)
			auth = manager
		} else {
			slog.Warn("identity provider unavailable; login remains disabled")
		}
	} else {
		slog.Warn("identity configuration incomplete; login remains disabled")
	}
	if auth == nil {
		mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(503)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "identity_unavailable", "message": "Identity provider is not configured or unavailable"}})
		})
	}
	control := platform.NewHandler(store, auth, products, platform.WithApplications(modules, administrators))
	mux.Handle("/api/", control)
	mux.Handle("/public/", control)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if e := store.Ping(ctx); e != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ready"))
	})
	srv := &http.Server{Addr: value("EULER_HTTP_ADDR", ":8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	slog.Info("application cloud listening", "address", srv.Addr, "loginAvailable", auth != nil)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server failed", "error", err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if e = srv.Shutdown(ctx); e != nil {
		slog.Error("HTTP shutdown incomplete")
	}
}
