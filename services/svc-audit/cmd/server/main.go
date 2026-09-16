// Package main is the entry point for the svc-audit service.
//
// svc-audit is the operation-audit collection and query service
// (03§4.4.3). Its duties on the platform are:
//
//   - collect every OpenAPI call (gateway side-stream) plus every console
//     high-risk operation — including DENIED ones, because a rejected
//     privileged operation is precisely the event an investigator wants
//     (07§6.1)
//   - store the trail in a tamper-evident per-tenant hash chain
//     (pkg-go/audit, 07§6.2): removing, reordering or editing any record
//     breaks every link after it, and the break cannot be repaired without
//     recomputing the whole chain
//   - serve list and verify (Integrity) queries to the console and the
//     compliance export path
//
// The service is a stdlib HTTP server (no Kratos/grpc/prometheus — this
// repo has no codegen; the _tmpl-go and svc-iam verify pattern). It wires
// the shared middleware every service MUST carry on Day 1 (架构原则 9:
// 可观测性是 Day 1 特性): request-id injection (→ trace_id), structured
// access logging, and recover — plus /healthz, /readyz and /metrics probes
// for K8s, and graceful shutdown.
//
// In-memory per-account chains are used in place of the production
// Kafka → ClickHouse sink (03§4.4.3 实现). The store implements the same
// append/list/verify contract the real sink will, so the handlers do not
// change when the sink is swapped in.
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

	"github.com/qifalab/euler-platform/httpmw"
)

func main() {
	var (
		httpAddr = flag.String("http", ":8080", "HTTP listen address")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx := context.Background()
	repo, err := newCheckpointRepo(ctx)
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	store, err := newAuditStoreWith(ctx, repo)
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	// Log where the chain heads live: a restart that resets chains to genesis
	// would silently validate a truncated trail.
	slog.Info("svc-audit checkpoint repo ready", "persistent", repo != nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/metrics", metrics)
	mux.HandleFunc("POST /internal/audit", store.handleAppend)
	mux.HandleFunc("GET /internal/audit/{account}/events", store.handleList)
	mux.HandleFunc("GET /internal/audit/{account}/verify", store.handleVerify)

	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           withMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("http server listening", "addr", *httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen error", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown: drain on SIGTERM (08§6.2 preStop hook).
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
// logging, and recover (03§2.3.4). The chain itself lives in pkg-go/httpmw so
// the services cannot drift apart.
func withMiddleware(h http.Handler) http.Handler {
	return httpmw.Chain(h)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyz(w http.ResponseWriter, _ *http.Request) {
	// TODO(scaffold): return 503 until Kafka + ClickHouse connectivity is up.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// TODO(scaffold): expose RED metrics. Mandatory labels: service, instance,
	// region, env (05§7.2). FORBIDDEN as labels: account_id, resource_id (high
	// cardinality — 05§7.2).
	_, _ = w.Write([]byte("# HELP eu_service_dummy 0\n# TYPE eu_service_dummy counter\nsc_service_dummy 0\n"))
}
