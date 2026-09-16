// Package main is the entry point for the Go Kratos service template.
//
// This template is the standard scaffold every Go/Kratos service starts from
// (03§2.3.4 脚手架). It wires the cross-cutting concerns every service MUST
// carry on Day 1 (架构原则 9: 可观测性是 Day 1 特性, 未接观测的服务禁止上生产):
//
//   - OpenTelemetry tracing (SDK init, trace_id injected into logs + propagated)
//   - /healthz and /readyz endpoints for K8s probes (Kratos Health-equivalent)
//   - /metrics (Prometheus format, RED three indicators as delivery gate)
//   - Nacos registration + config watch (env = NACOS_NAMESPACE; group = service name)
//   - graceful shutdown (de-register from Nacos + drain on SIGTERM; 08§6.2)
//   - idempotency + audit middleware (the SDK hooks 08§6.2 references)
//
// The service name follows the全局标识规范: svc-{domain} for control-plane
// services, {scene}-bff for access-layer BFFs, rc-* for data-plane controllers.
// Replace _tmpl_ with the real domain on instantiation.
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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/metrics", metrics)

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
	//   - Red line: same process MUST NOT run SkyWalking agent + OTel exporter
	//     together (selection pit #1); CI baseline ships OTel agent only.

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
// logging, recover, and the idempotency/audit hooks (03§2.3.4). The chain
// itself lives in pkg-go/httpmw so new services start from the same code the
// running ones use.
func withMiddleware(h http.Handler) http.Handler {
	return httpmw.Chain(h)
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
	_, _ = w.Write([]byte("# HELP eu_service_dummy 0\n# TYPE eu_service_dummy counter\nsc_service_dummy 0\n"))
}
