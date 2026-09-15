// Package main is the HTTP server for svc-workflow, the platform's workflow
// execution service.
//
// It implements the platform workflow base per 03§4.3.3: 03 forbids
// introducing Temporal or Camunda, so svc-workflow runs the lightweight
// in-process saga engine from pkg-go/workflow. The engine sequences ordered
// steps, each with a forward action and a compensating action, and on failure
// runs the compensations of the completed steps in REVERSE order (03§8.4).
//
// In the target architecture this server persists flow_instance and
// step_instance rows in Vitess and is driven by a "scan next_fire_at +
// distributed lock" timer (03§4.3.3). This Go binary is the runnable reference
// implementation: it keeps the in-memory stores behind small ports so the
// production repositories can be swapped in without touching the handlers, and
// it uses the identical pkg-go/workflow engine the rest of the platform runs,
// so the contract cannot drift.
//
// # Internal routes (the gateway reaches these only on authenticated /internal
// paths and injects X-Sc-Account-Id upstream, 07§3.3):
//
//	POST /internal/flows/start  -> start a predefined flow for def_key + biz_key
//	GET  /internal/flows/{id}   -> the flow Result (status, compensation errors)
//
// Cross-cutting middleware (request-id/logging/recover) and graceful shutdown
// follow the _tmpl-go scaffold (03§2.3.4).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	httpAddr := flag.String("http", ":8080", "HTTP listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	app, err := newApp(context.Background())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", readyz)
	mux.HandleFunc("/metrics", metrics)
	mux.HandleFunc("/internal/flows/start", app.handleStart)
	mux.HandleFunc("/internal/flows/", app.handleGet)

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

	// Graceful shutdown: drain in-flight requests (08§6.2 preStop hook).
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
// logging, and recover (03§2.3.4).
func withMiddleware(h http.Handler) http.Handler {
	return recoverMiddleware(requestIDMiddleware(loggingMiddleware(h)))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyz(w http.ResponseWriter, _ *http.Request) {
	// TODO(svc-workflow): return 503 until the flow_instance store is reachable.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// TODO(svc-workflow): expose RED metrics via prometheus/client_golang
	// (05§7.2). Mandatory labels: service, instance, region, env. FORBIDDEN as
	// labels: account_id, resource_id (high cardinality).
	_, _ = w.Write([]byte("# HELP sc_service_dummy 0\n# TYPE sc_service_dummy counter\nsc_service_dummy 0\n"))
}

// --- HTTP handlers -----------------------------------------------------------

type startRequest struct {
	DefKey string `json:"def_key"`
	BizKey string `json:"biz_key"`
}

func (a *app) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	accountID, ok := accountIDFromHeader(r)
	if !ok {
		writeErr(w, "Workflow.MissingAccount", "missing or invalid X-Sc-Account-Id header", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", "malformed request body", http.StatusBadRequest)
		return
	}
	if req.DefKey == "" {
		writeErr(w, "Common.InvalidParameter", "def_key is required", http.StatusBadRequest)
		return
	}

	id, result, err := a.StartFlow(accountID, req.DefKey, req.BizKey)
	if err != nil {
		writeErr(w, "Workflow.UnknownDef", err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"status":  string(result.Status),
		"biz_key": req.BizKey,
	})
}

func (a *app) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	accountID, ok := accountIDFromHeader(r)
	if !ok {
		writeErr(w, "Workflow.MissingAccount", "missing or invalid X-Sc-Account-Id header", http.StatusForbidden)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/internal/flows/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", "invalid flow id", http.StatusBadRequest)
		return
	}

	rec, ok, err := a.store.get(id)
	if err != nil {
		slog.Error("flow lookup failed", "flowId", id, "err", err)
		writeErr(w, "Workflow.LookupFailed", "flow lookup failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		writeErr(w, "Workflow.NotFound", "flow not found", http.StatusNotFound)
		return
	}
	// A caller may only read flows that belong to its own account.
	if rec.AccountID != accountID {
		writeErr(w, "Workflow.Forbidden", "flow does not belong to this account", http.StatusForbidden)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// accountIDFromHeader reads the account id the gateway injects upstream
// (07§3.3). The gateway guarantees it; a missing/invalid value is a routing
// misconfiguration and must be rejected with 403, not guessed.
func accountIDFromHeader(r *http.Request) (int64, bool) {
	v := r.Header.Get("X-Sc-Account-Id")
	if v == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code, msg string, status int) {
	writeJSON(w, status, map[string]string{"Code": code, "Message": msg})
}
