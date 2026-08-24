// Package main: HTTP middleware for the svc-workflow server.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

// requestIDMiddleware generates a request id (≡ trace_id for the platform),
// injects it into the context and the X-Sc-TraceId response header, and
// aligns with the APISIX request-id plugin (04§3.1 global plugins).
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = uuid.NewString()
		}
		w.Header().Set("X-Sc-TraceId", rid)
		ctx := context.WithValue(r.Context(), requestIDKey, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// loggingMiddleware logs each request with trace_id; RED metrics (rate,
// errors, duration) are computed here in the real scaffold.
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

// recoverMiddleware catches panics, logs the stack, and returns 500 — a 500
// response must always carry the request id so客服/排障 can find it (03§9.3).
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
