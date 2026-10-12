// Package observability exposes bounded, credential-free operational signals.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Dependency struct {
	Name      string    `json:"name"`
	Required  bool      `json:"required"`
	State     string    `json:"state"`
	CheckedAt time.Time `json:"checkedAt"`
}
type Check struct {
	Name  string
	Probe func(context.Context) string
}
type Config struct {
	CoreCheck func(context.Context) error
	Checks    []Check
	// Authorize returns 0 when authorized, otherwise 401/403. It must not use
	// caller-supplied role headers. The main package binds exact admin identities.
	Authorize      func(*http.Request) int
	LoginAvailable func() bool
	IdentityStatus func() any
	Logger         *slog.Logger
}
type metricKey struct {
	method, route string
	status        int
}
type metricValue struct {
	count   uint64
	seconds float64
}
type Monitor struct {
	config       Config
	mu           sync.RWMutex
	dependencies []Dependency
	metrics      map[metricKey]metricValue
	inFlight     atomic.Int64
	stopping     atomic.Bool
}

func New(c Config) *Monitor {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	m := &Monitor{config: c, metrics: make(map[metricKey]metricValue)}
	for _, check := range c.Checks {
		m.dependencies = append(m.dependencies, Dependency{Name: check.Name, State: "checking"})
	}
	return m
}

func canonicalState(state string) string {
	switch state {
	case "up", "down", "not_configured", "configured_unverified", "checking":
		return state
	default:
		return "down"
	}
}

// Refresh never includes an error string, DSN, URL, path, or credential. Each
// probe gets a deadline and must respect it; the bundled SQL/S3 probes do so.
func (m *Monitor) Refresh(ctx context.Context) {
	deps := make([]Dependency, len(m.config.Checks))
	var wg sync.WaitGroup
	for i, check := range m.config.Checks {
		wg.Add(1)
		go func(i int, check Check) {
			defer wg.Done()
			probe, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			state := "not_configured"
			if check.Probe != nil {
				state = canonicalState(check.Probe(probe))
			}
			deps[i] = Dependency{Name: check.Name, State: state, CheckedAt: time.Now().UTC()}
		}(i, check)
	}
	wg.Wait()
	m.mu.Lock()
	m.dependencies = deps
	m.mu.Unlock()
}
func (m *Monitor) Run(ctx context.Context) {
	m.Refresh(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Refresh(ctx)
		}
	}
}
func (m *Monitor) Stop() { m.stopping.Store(true) }
func (m *Monitor) core(ctx context.Context) Dependency {
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	state := "up"
	if m.stopping.Load() || m.config.CoreCheck == nil || m.config.CoreCheck(probe) != nil {
		state = "down"
	}
	return Dependency{Name: "platform_database", Required: true, State: state, CheckedAt: time.Now().UTC()}
}
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func (m *Monitor) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "alive"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		status := 200
		state := "ready"
		if m.core(r.Context()).State != "up" {
			status = 503
			state = "not_ready"
		}
		writeJSON(w, status, map[string]string{"status": state})
	})
	mux.Handle("GET /ops/diagnostics", m.protected(http.HandlerFunc(m.diagnostics)))
	mux.Handle("GET /ops/metrics", m.protected(http.HandlerFunc(m.prometheus)))
}
func (m *Monitor) protected(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusUnauthorized
		if m.config.Authorize != nil {
			status = m.config.Authorize(r)
		}
		if status != 0 {
			if status != http.StatusForbidden {
				status = http.StatusUnauthorized
				w.Header().Set("WWW-Authenticate", "Bearer")
			}
			writeJSON(w, status, map[string]string{"error": "operations_access_required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (m *Monitor) diagnostics(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	deps := append([]Dependency(nil), m.dependencies...)
	m.mu.RUnlock()
	core := m.core(r.Context())
	deps = append([]Dependency{core}, deps...)
	body := map[string]any{"ready": core.State == "up", "dependencies": deps, "checkIntervalSeconds": 30}
	if m.config.IdentityStatus != nil {
		body["identity"] = m.config.IdentityStatus()
	}
	writeJSON(w, 200, body)
}
func (m *Monitor) prometheus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	coreUp := 0
	if m.core(r.Context()).State == "up" {
		coreUp = 1
	}
	login := 0
	if m.config.LoginAvailable != nil && m.config.LoginAvailable() {
		login = 1
	}
	fmt.Fprintf(w, "# TYPE euler_identity_login_available gauge\neuler_identity_login_available %d\n", login)
	fmt.Fprintf(w, "# TYPE euler_ready gauge\neuler_ready %d\n# TYPE euler_http_in_flight gauge\neuler_http_in_flight %d\n", coreUp, m.inFlight.Load())
	m.mu.RLock()
	deps := append([]Dependency(nil), m.dependencies...)
	stats := make(map[metricKey]metricValue, len(m.metrics))
	for k, v := range m.metrics {
		stats[k] = v
	}
	m.mu.RUnlock()
	fmt.Fprintln(w, "# TYPE euler_dependency_state gauge")
	for _, d := range deps {
		fmt.Fprintf(w, "euler_dependency_state{name=%q,state=%q} 1\n", d.Name, d.State)
	}
	fmt.Fprintln(w, "# TYPE euler_http_requests_total counter\n# TYPE euler_http_duration_seconds_sum counter")
	keys := make([]metricKey, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.status < b.status
	})
	for _, k := range keys {
		v := stats[k]
		fmt.Fprintf(w, "euler_http_requests_total{method=%q,route=%q,status=%q} %d\neuler_http_duration_seconds_sum{method=%q,route=%q,status=%q} %.6f\n", k.method, k.route, fmt.Sprint(k.status), v.count, k.method, k.route, fmt.Sprint(k.status), v.seconds)
	}
}

type requestIDKey struct{}

func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}
func routeClass(path string) string {
	// Never use a raw URL or path: public routes may contain invitation tokens,
	// storage keys, usernames and other sensitive identifiers in their path.
	for _, prefix := range []string{"/auth/", "/api/", "/public/", "/ops/"} {
		if strings.HasPrefix(path, prefix) {
			return strings.Trim(prefix, "/")
		}
	}
	if path == "/healthz" || path == "/readyz" {
		return "probe"
	}
	return "other"
}
func safeMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		return method
	default:
		return "OTHER"
	}
}

type response struct {
	http.ResponseWriter
	status int
}

func (w *response) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *response) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *response) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *response) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (m *Monitor) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			http.Error(w, "request unavailable", 503)
			return
		}
		id := hex.EncodeToString(bytes[:]) // Never trust a caller's correlation ID.
		w.Header().Set("X-Request-ID", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		rw := &response{ResponseWriter: w}
		start := time.Now()
		m.inFlight.Add(1)
		defer func() {
			if recovered := recover(); recovered != nil {
				if rw.status == 0 {
					writeJSON(rw, 500, map[string]string{"error": "internal_error", "requestId": id})
				}
				m.config.Logger.Error("request panic", "requestId", id)
			}
			m.inFlight.Add(-1)
			status := rw.status
			if status == 0 {
				status = 200
			}
			if status < 100 || status > 599 {
				status = 500
			}
			elapsed := time.Since(start)
			key := metricKey{safeMethod(r.Method), routeClass(r.URL.Path), status}
			m.mu.Lock()
			v := m.metrics[key]
			v.count++
			v.seconds += elapsed.Seconds()
			m.metrics[key] = v
			m.mu.Unlock()
			m.config.Logger.Info("http request", "requestId", id, "method", key.method, "route", key.route, "status", status, "durationMs", elapsed.Milliseconds())
		}()
		next.ServeHTTP(rw, r)
	})
}
