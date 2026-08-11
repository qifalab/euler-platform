// Package main is the entry point for the svc-api-meta service.
//
// svc-api-meta is the platform's API metadata center (03§4.5.1): it stores the
// per-Action parameter schema, error codes, and version for every OpenAPI
// Action. That metadata drives documentation generation (文档中心, 01§),
// SDK generation (03§9.4), and the OpenAPI Explorer debug console backend.
//
// Responsibilities wired here (matching the shared scaffold, _tmpl-go):
//
//   - /healthz and /readyz for K8s probes
//   - /metrics (placeholder RED endpoint; real collection wired by scaffold)
//   - shared middleware chain (request-id → trace_id, access logging, recover)
//   - graceful shutdown on SIGTERM/SIGINT (08§6.2 preStop drain)
//
// The service name follows the全局标识规范: svc-{domain} (svc-api-meta).
//
// This is a stdlib-HTTP service (no Kratos/gRPC codegen — repo convention).
// Account identity is injected by the APISIX gateway via the X-Sc-Account-Id
// header; handlers reject requests that omit it with 403 (03§9.3).
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		httpAddr = flag.String("http", ":8080", "HTTP listen address")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	store := newMetaStore()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/metrics", metrics)

	// Internal control-plane endpoints (APISIX/gateway intranet only).
	mux.HandleFunc("POST /internal/actions", store.handleRegisterAction)
	mux.HandleFunc("GET /internal/actions/{product}/{action}", store.handleGetAction)
	mux.HandleFunc("GET /internal/actions", store.handleListActions)

	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           withMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// TODO(scaffold): wire Nacos registration + config watch here (04§4.3).

	go func() {
		slog.Info("http server listening", "addr", *httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen error", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown: drain in-flight requests on SIGTERM (08§6.2).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutdown signal received, draining")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
	slog.Info("server exited")
}

// withMiddleware wraps the mux with the cross-cutting middleware chain every
// service must apply: request-id injection (→ trace_id), structured access
// logging, recover (03§2.3.4).
func withMiddleware(h http.Handler) http.Handler {
	return recoverMiddleware(requestIDMiddleware(loggingMiddleware(h)))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyz(w http.ResponseWriter, _ *http.Request) {
	// TODO(scaffold): return 503 until Nacos registration + DB ping succeed.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// TODO(scaffold): expose RED metrics via prometheus/client_golang.
	// FORBIDDEN as labels: account_id, resource_id (high cardinality — 05§7.2).
	_, _ = w.Write([]byte("# HELP sc_service_dummy 0\n# TYPE sc_service_dummy counter\nsc_service_dummy 0\n"))
}
