// Package main is the entry point for the console-bff service.
//
// console-bff is the control-plane aggregation layer (03-backend-services.md
// §4, console-bff node in the 03§3 architecture diagram). It faces the web
// console frontend and fans out to the internal services (svc-billing,
// svc-orchestrator, svc-order) behind it, aggregating their responses into the
// shapes the console needs. The fan-out is live (see store.go); downstream base
// URLs come from EULER_SVC_* env vars (dev defaults: orchestrator :9203, billing
// :9206, order :9204). In the target architecture the BFF also caches
// aggregated views in Redis (03§4); phase-1 is uncached fan-out.
//
// This is a stdlib-HTTP service (no Kratos/gRPC codegen — repo convention).
// Account identity is injected by the APISIX gateway via the X-Euler-Account-Id
// header; handlers reject requests that omit it with 403 (03§9.3).
//
// Responsibilities wired here (matching the shared scaffold, _tmpl-go):
//
//   - /healthz and /readyz for K8s probes
//   - /metrics (placeholder RED endpoint; real collection wired by scaffold)
//   - shared middleware chain (request-id → trace_id, access logging, recover)
//   - graceful shutdown on SIGTERM/SIGINT (08§6.2 preStop drain)
//
// Domain routes (console aggregation, 03§4.4):
//
//	GET /console/overview   -> account overview (balance/resource/order stubs)
//	GET /console/resources  -> resource list
//	GET /console/bills      -> bill list
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

	store := newConsoleStore()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/metrics", metrics)

	// Console aggregation endpoints (frontend-facing, gateway-authorized).
	mux.HandleFunc("GET /console/overview", store.handleOverview)
	mux.HandleFunc("GET /console/resources", store.handleResources)
	mux.HandleFunc("GET /console/bills", store.handleBills)

	// Phase-2 billing-form endpoints (M-4.4): resource packs, invoices, cost
	// analysis — all fan out to svc-billing, which holds the real ledger.
	mux.HandleFunc("GET /console/reservepacks", store.handleReservePacks)
	mux.HandleFunc("POST /console/reservepacks", store.handleReservePackPurchase)
	mux.HandleFunc("GET /console/invoices", store.handleInvoices)
	mux.HandleFunc("POST /console/invoices", store.handleInvoiceDraft)
	mux.HandleFunc("POST /console/invoices/issue", store.handleInvoiceIssue)
	mux.HandleFunc("POST /console/invoices/void", store.handleInvoiceVoid)
	mux.HandleFunc("GET /console/cost-analysis", store.handleCostAnalysis)

	// Global search for the console ⌘K palette: aggregates live resources
	// (svc-orchestrator) and product catalogue entries (svc-catalog).
	mux.HandleFunc("GET /console/search", store.handleSearch)

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
// logging, recover (03§2.3.4), plus this service's internal-token guard. The
// chain itself lives in pkg-go/httpmw so the services cannot drift apart.
func withMiddleware(h http.Handler) http.Handler {
	return httpmw.Chain(httpmw.InternalToken(h))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyz(w http.ResponseWriter, _ *http.Request) {
	// TODO(scaffold): return 503 until Nacos registration + Redis ping succeed.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// TODO(scaffold): expose RED metrics via prometheus/client_golang.
	// FORBIDDEN as labels: account_id, resource_id (high cardinality — 05§7.2).
	_, _ = w.Write([]byte("# HELP eu_service_dummy 0\n# TYPE eu_service_dummy counter\nsc_service_dummy 0\n"))
}
