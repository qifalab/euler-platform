// Package main: svc-openapi — the OpenAPI facade of the phase-1 products
// (04§3.2 OpenAPI zone, 03§9.4 SDK contract).
//
// Every product subdomain {productCode}.api.euler.emoera.com is routed here by
// APISIX (platform/apisix/routes/openapi-zone.yaml) after the gateway chain has
// run: eu-auth verified the CPS1 signature and injected X-Euler-Account-Id,
// X-Euler-Ak-Id, X-Euler-Identity, X-Euler-TraceId and X-Euler-Internal-Token;
// eu-authorize checked {productCode}:{Action} against the caller's policies.
// The facade serves the RPC-style Action API that pkg-go/eusdk,
// sdk/python/cloudsdk and the Terraform provider (sdk/terraform) call, and
// translates each Action into calls on the internal REST services that own the
// state:
//
//	svc-catalog       SKU resolution, region/zone inventory
//	svc-order         purchase orders: create (idempotent on ClientToken), pay from balance
//	svc-orchestrator  resource lifecycle: fulfil, describe, start/stop/restart/upgrade, release
//	svc-monitor       alarm rules and metric data (eumon)
//
// The facade keeps no state of its own. Idempotency rides on svc-order's
// per-account ClientToken uniqueness and svc-orchestrator's one-resource-per-
// order ledger; ownership on the downstream services' account scoping (every
// downstream call carries the caller's X-Euler-Account-Id and the internal
// token) plus the facade's own product check on every resource-addressed
// Action.
//
// Wire contract (what the SDKs parse):
//
//	request  GET|POST /?Action={Action}&Version={YYYY-MM-DD}, parameters in the
//	         query string and/or a JSON-object or form body
//	success  200 {"RequestId": ..., "Code": "OK", "Data": {...}}  (the
//	         Terraform provider and SDK callers read Data)
//	error    4xx/5xx {"RequestId": ..., "Code": ..., "Message": ...}
//
// RequestId is the gateway trace id (X-Euler-TraceId), echoed in the
// X-Euler-TraceId response header and forwarded on every downstream call.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/identifier"
	"github.com/qifalab/euler-platform/runmode"
)

// Configuration (non-secret; the chart's extraEnv sets the in-cluster
// addresses http://<svc>.euler-<svc>.svc:<port>, the defaults are the local
// development ports of each service).
const (
	envCatalogURL      = "EULER_SVC_CATALOG_URL"
	envOrderURL        = "EULER_SVC_ORDER_URL"
	envOrchestratorURL = "EULER_SVC_ORCHESTRATOR_URL"
	envMonitorURL      = "EULER_SVC_MONITOR_URL"
	// envDefaultRegion is where a create lands when the caller names neither
	// RegionId nor ZoneId: the region this deployment's gateway routes are
	// bound to (the eu-auth signing region).
	envDefaultRegion = "EULER_OPENAPI_DEFAULT_REGION"
)

type config struct {
	CatalogURL      string
	OrderURL        string
	OrchestratorURL string
	MonitorURL      string
	DefaultRegion   string
}

func configFromEnv() config {
	return config{
		CatalogURL:      envOr(envCatalogURL, "http://localhost:9207"),
		OrderURL:        envOr(envOrderURL, "http://localhost:9204"),
		OrchestratorURL: envOr(envOrchestratorURL, "http://localhost:9203"),
		MonitorURL:      envOr(envMonitorURL, "http://localhost:9202"),
		DefaultRegion:   envOr(envDefaultRegion, "cn-north-1"),
	}
}

// validate refuses a configuration the facade cannot serve with: a base URL
// that is not absolute http(s), or a default region that is not a region name.
func (c config) validate() error {
	for name, raw := range map[string]string{
		envCatalogURL: c.CatalogURL, envOrderURL: c.OrderURL,
		envOrchestratorURL: c.OrchestratorURL, envMonitorURL: c.MonitorURL,
	} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s=%q is not an absolute http(s) URL", name, raw)
		}
	}
	if !identifier.IsValidRegion(c.DefaultRegion) {
		return fmt.Errorf("%s=%q is not a region name", envDefaultRegion, c.DefaultRegion)
	}
	return nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("http", ":9230", "HTTP listen address")
	flag.Parse()
	// Production refuses to start without the internal shared secret: the
	// X-Euler-Account-Id this service trusts must provably come from the gateway.
	runtimeRequireSecrets()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg := configFromEnv()
	if err := cfg.validate(); err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           newHandler(newServer(cfg)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		slog.Info("svc-openapi listening", "addr", *addr, "defaultRegion", cfg.DefaultRegion)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown: the chart's preStop sleep drains the endpoints first,
	// then in-flight actions get up to 30s to finish (08§6.2).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutdown signal received, draining")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
}

// newHandler mounts the probes and the Action endpoint behind the standard
// middleware chain.
func newHandler(api *server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("/", api)
	return withMiddleware(mux)
}

// withMiddleware is the template chain (httpmw: recover, request id, access
// log, internal-token guard) with the gateway's trace id adopted as the
// request id.
func withMiddleware(h http.Handler) http.Handler {
	return gatewayTraceID(httpmw.Chain(httpmw.InternalToken(h)))
}

// gatewayTraceID makes the gateway-injected X-Euler-TraceId the request id
// httpmw.RequestID adopts (it reads X-Request-Id), so the RequestId in the
// response body, the X-Euler-TraceId response header, the access log, the
// audit record (eu-audit) and every downstream service's log all carry one id.
func gatewayTraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := httpmw.SanitizeRequestID(r.Header.Get(traceIDHeader)); id != "" {
			r.Header.Set(requestIDHeader, id)
		}
		next.ServeHTTP(w, r)
	})
}

// runtimeRequireSecrets enforces the production secret set (pkg-go/runmode).
func runtimeRequireSecrets() {
	runmode.MustRequireSecrets(httpmw.InternalTokenEnv)
}
