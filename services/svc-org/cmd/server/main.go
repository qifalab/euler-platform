// Package main is the entry point for the svc-org HTTP service.
//
// svc-org implements the org domain (03-backend-services.md §4.1/§4.2): project
// CRUD, resource-to-project movement, and account-level tag management. The
// control-plane service is wired exactly like the shared scaffold
// (03§2.3.4 脚手架): the same middleware chain (request-id/logging/recover), the
// same /healthz, /readyz and /metrics endpoints, and the same graceful
// shutdown path. Phase-1 stores everything in-process (a thread-safe in-memory
// store) so the stdlib HTTP server is runnable standalone; production swaps the
// store for the MySQL/Redis implementation behind the same interface.
//
// Tenancy is carried by the X-Sc-Account-Id header, which the gateway (APISIX)
// injects on every authenticated request (04§3.1 global plugins). A request
// without it is rejected with 403 — an unauthenticated caller must not be able
// to read or write another account's projects or tags.
//
// This mirrors 03§4.1.2: project is {project_id, account_id, name, parent_id};
// tags are (account_id, key, value).
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

	org := newOrgService()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/metrics", metrics)

	// Project CRUD.
	mux.HandleFunc("/internal/projects", org.handleProjects)
	mux.HandleFunc("/internal/projects/", org.handleProjectByID)
	// Resource movement between projects.
	mux.HandleFunc("/internal/resources/move", org.handleResourceMove)
	// Tag management.
	mux.HandleFunc("/internal/tags", org.handleTags)

	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           withMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// TODO(scaffold): wire Nacos registration + config watch here.
	//   - Register under Group = service name (04§4.3); subscribe to the prod
	//     namespace only (prod gateway订阅 boundary, pit #2).
	//   - dataId = {service}-{profile}.yaml; env差异禁止写进代码 (08§7.2).
	// TODO(scaffold): wire OTel tracer provider + Meter provider here.

	go func() {
		slog.Info("http server listening", "addr", *httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen error", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown: de-register from Nacos + drain (08§6.2 preStop hook).
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
// logging, recover, and the idempotency/audit hooks (03§2.3.4).
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
	// Mandatory labels: service, instance, region, env (05§7.2).
	// FORBIDDEN as labels: account_id, resource_id (high cardinality — 05§7.2).
	_, _ = w.Write([]byte("# HELP sc_service_dummy 0\n# TYPE sc_service_dummy counter\nsc_service_dummy 0\n"))
}
