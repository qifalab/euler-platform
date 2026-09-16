// Package httpmw is the cross-cutting HTTP middleware chain every Euler service
// applies on Day 1 (03§2.3.4 / 03§9.3):
//
//   - request-id injection: the id becomes the platform's trace_id, is echoed in
//     the X-Euler-TraceId response header, and aligns with the APISIX request-id
//     plugin (04§3.1 global plugins);
//   - structured access logging carrying the trace_id;
//   - panic recovery: a 500 response MUST always carry the request id so客服/排障
//     can find it (03§9.3);
//   - the optional internal shared-secret guard (EULER_INTERNAL_TOKEN), the
//     defense-in-depth behind the gateway-injected X-Euler-Account-Id trust.
//
// This chain used to be copied verbatim into every service (and into
// services/_tmpl-go); it lives here so a change lands once and the services
// cannot drift apart.
package httpmw

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

// Chain applies the standard chain — Recover(RequestID(Logging(h))) — matching
// the order the services used before this package existed: outermost panic
// recovery, then request-id injection, then access logging.
func Chain(h http.Handler) http.Handler {
	return Recover(RequestID(Logging(h)))
}

// RequestIDFromContext returns the request id (≡ trace_id) carried by ctx, or
// "" when the request never passed through RequestID.
func RequestIDFromContext(ctx context.Context) string {
	rid, _ := ctx.Value(requestIDKey).(string)
	return rid
}

// RequestID generates a request id (≡ trace_id for the platform), injects it
// into the context and the X-Euler-TraceId response header, and aligns with the
// APISIX request-id plugin (04§3.1 global plugins). An inbound X-Request-Id
// (set by the gateway) is preserved.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = uuid.NewString()
		}
		w.Header().Set("X-Euler-TraceId", rid)
		ctx := context.WithValue(r.Context(), requestIDKey, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Logging logs each request with trace_id; RED metrics (rate, errors,
// duration) are computed here in the real scaffold.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"trace_id", RequestIDFromContext(r.Context()),
		)
	})
}

// Recover catches panics, logs the stack, and returns 500 — a 500 response must
// always carry the request id so客服/排障 can find it (03§9.3).
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				rid := RequestIDFromContext(r.Context())
				slog.Error("panic recovered",
					"trace_id", rid,
					"recover", rec,
					"stack", string(debug.Stack()),
				)
				http.Error(w, `{"RequestId":"`+rid+`","Code":"Common.InternalError","Message":"internal error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// InternalToken optionally enforces an internal shared secret: when the
// EULER_INTERNAL_TOKEN env var is set, every request must carry a matching
// X-Euler-Internal-Token header (defense-in-depth for the gateway-injected
// X-Euler-Account-Id trust). Unset (dev default) = no check.
func InternalToken(next http.Handler) http.Handler {
	token := os.Getenv("EULER_INTERNAL_TOKEN")
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Euler-Internal-Token") != token {
			rid := RequestIDFromContext(r.Context())
			http.Error(w, `{"RequestId":"`+rid+`","Code":"Common.Forbidden","Message":"invalid internal token"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
