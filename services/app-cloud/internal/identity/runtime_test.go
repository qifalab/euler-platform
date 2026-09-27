package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type outageTransport struct {
	base   http.RoundTripper
	outage atomic.Bool
}

func (t *outageTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.outage.Load() {
		return nil, errors.New("unreachable https://secret.example/?client_secret=do-not-leak")
	}
	return t.base.RoundTrip(r)
}
func waitRuntime(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runtime did not reach expected state")
}
func TestRuntimeRetriesInitialDiscoveryAndPreservesSessionsDuringOutage(t *testing.T) {
	i := newIssuer(t)
	store := newStore()
	cfg := issuerConfig(i)
	transport := &outageTransport{base: cfg.HTTPClient.Transport}
	transport.outage.Store(true)
	cfg.HTTPClient = &http.Client{Transport: transport}
	r, err := NewRuntime(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	r.retryMin = time.Millisecond
	r.retryMax = 5 * time.Millisecond
	r.refreshInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitRuntime(t, func() bool { return r.Status().ConsecutiveFailures > 0 })
	w := httptest.NewRecorder()
	r.Login(w, httptest.NewRequest("GET", "https://cloud.test/auth/login", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "do-not-leak") {
		t.Fatalf("unsafe outage response: %d %s", w.Code, w.Body.String())
	}
	transport.outage.Store(false)
	waitRuntime(t, r.LoginAvailable)
	r.mu.RLock()
	manager := r.active
	r.mu.RUnlock()
	callback, flow := startLogin(t, manager, i, "/")
	finished := httptest.NewRecorder()
	req := httptest.NewRequest("GET", callback, nil)
	req.AddCookie(flow)
	r.Callback(finished, req)
	if finished.Code != 303 {
		t.Fatalf("recovered callback failed: %d %s", finished.Code, finished.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range finished.Result().Cookies() {
		if c.Name == "__Host-euler_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("missing real login session")
	}
	transport.outage.Store(true)
	waitRuntime(t, func() bool { return !r.LoginAvailable() })
	sessionRequest := httptest.NewRequest("GET", "https://cloud.test/api/v1/session", nil)
	sessionRequest.AddCookie(cookie)
	principal, session, err := r.Authenticate(sessionRequest)
	if err != nil || principal.Subject != "123456" {
		t.Fatal("existing session depended on provider", err)
	}
	if !r.Status().SessionValidationAvailable {
		t.Fatal("session availability incorrectly disabled")
	}
	logout := httptest.NewRequest("POST", "https://cloud.test/auth/logout", nil)
	logout.AddCookie(cookie)
	logout.Header.Set("Origin", cfg.PublicOrigin)
	logout.Header.Set("X-CSRF-Token", session.CSRFToken)
	mux := http.NewServeMux()
	r.Register(mux)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, logout)
	if w.Code != 204 {
		t.Fatalf("logout failed during outage: %d", w.Code)
	}
	if _, _, err = r.Authenticate(sessionRequest); err == nil {
		t.Fatal("revoked session survived")
	}
	transport.outage.Store(false)
	waitRuntime(t, r.LoginAvailable)
	if r.Status().ConsecutiveFailures != 0 || r.Status().LastSuccessAt.IsZero() {
		t.Fatal("recovery state stale")
	}
}

func TestRuntimeConcurrentDiscoveryAndSessionRequests(t *testing.T) {
	i := newIssuer(t)
	store := newStore()
	r, err := NewRuntime(issuerConfig(i), store)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				if n%3 == 0 {
					r.refresh(context.Background())
				} else {
					r.Status()
					r.LoginAvailable()
					r.Authenticate(httptest.NewRequest("GET", "https://cloud.test/api/v1/session", nil))
					w := httptest.NewRecorder()
					r.Login(w, httptest.NewRequest("GET", "https://cloud.test/auth/login", nil))
				}
			}
		}(n)
	}
	wg.Wait()
	if !r.LoginAvailable() {
		t.Fatal("concurrent refresh lost available state")
	}
}

func TestRuntimeCancellationInterruptsDiscoveryAndRejectsInvalidConfig(t *testing.T) {
	if _, err := NewRuntime(Config{}, newStore()); err == nil {
		t.Fatal("invalid config accepted")
	}
	i := newIssuer(t)
	cfg := issuerConfig(i)
	started := make(chan struct{})
	cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	r, err := NewRuntime(cfg, newStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("discovery did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("discovery ignored cancellation")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
