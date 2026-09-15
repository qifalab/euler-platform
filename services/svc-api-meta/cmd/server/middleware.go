// Package main: HTTP middleware for svc-api-meta.
//
// Same chain as the shared scaffold (_tmpl-go): request-id injection aligned
// with the APISIX request-id plugin (04§3.1), structured access logging, and
// panic recovery that always returns a 500 carrying the request id (03§9.3).
package main

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

// internalTokenMiddleware optionally enforces an internal shared secret: when
// the EULER_INTERNAL_TOKEN env var is set, every request must carry a matching
// X-Euler-Internal-Token header (defense-in-depth for the gateway-injected
// X-Euler-Account-Id trust). Unset (dev default) = no check.
func internalTokenMiddleware(next http.Handler) http.Handler {
	token := os.Getenv("EULER_INTERNAL_TOKEN")
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Euler-Internal-Token") != token {
			rid, _ := r.Context().Value(requestIDKey).(string)
			http.Error(w, `{"RequestId":"`+rid+`","Code":"Common.Forbidden","Message":"invalid internal token"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestIDMiddleware generates a request id (≡ trace_id for the platform),
// injects it into the context and the X-Euler-TraceId response header.
func requestIDMiddleware(next http.Handler) http.Handler {
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

// loggingMiddleware logs each request with trace_id.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		rid, _ := r.Context().Value(requestIDKey).(string)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"trace_id", rid,
		)
	})
}

// recoverMiddleware catches panics and returns 500 with the request id (03§9.3).
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered",
					"trace_id", r.Context().Value(requestIDKey),
					"recover", rec,
					"stack", string(debug.Stack()),
				)
				rid, _ := r.Context().Value(requestIDKey).(string)
				http.Error(w, `{"RequestId":"`+rid+`","Code":"Common.InternalError","Message":"internal error"}`, http.StatusInternalServerError)
			}
		}()
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
