package identity

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Runtime keeps session validation independent from provider discovery. All
// provider state is swapped atomically; concurrent callbacks keep their own
// immutable manager and never see partially updated endpoint configuration.
type Runtime struct {
	sessions                            *Manager
	mu                                  sync.RWMutex
	refreshMu                           sync.Mutex
	active                              *Manager
	status                              RuntimeStatus
	retryMin, retryMax, refreshInterval time.Duration
}

type RuntimeStatus struct {
	State                      string    `json:"state"`
	LoginAvailable             bool      `json:"loginAvailable"`
	SessionValidationAvailable bool      `json:"sessionValidationAvailable"`
	CheckedAt                  time.Time `json:"checkedAt,omitempty"`
	LastSuccessAt              time.Time `json:"lastSuccessAt,omitempty"`
	ConsecutiveFailures        int       `json:"consecutiveFailures"`
}

func NewRuntime(config Config, store SessionStore) (*Runtime, error) {
	m, err := newSessionManager(config, store)
	if err != nil {
		return nil, err
	}
	return &Runtime{sessions: m, status: RuntimeStatus{State: "checking", SessionValidationAvailable: true}, retryMin: time.Second, retryMax: time.Minute, refreshInterval: time.Minute}, nil
}

// Run immediately attempts discovery and keeps retrying with bounded backoff.
// Cancellation interrupts both outbound discovery and idle waits. Credentials
// and configuration are immutable: changing them requires a service restart.
func (r *Runtime) Run(ctx context.Context) {
	delay := r.retryMin
	for {
		if ctx.Err() != nil {
			return
		}
		ok := r.refresh(ctx)
		wait := delay
		if ok {
			wait = r.refreshInterval
			delay = r.retryMin
		} else {
			delay *= 2
			if delay > r.retryMax {
				delay = r.retryMax
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (r *Runtime) refresh(ctx context.Context) bool {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	check, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	m, err := New(check, r.sessions.config, r.sessions.store)
	if ctx.Err() != nil {
		return false
	}
	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.CheckedAt = now
	if err != nil {
		r.active = nil
		r.status.State = "unavailable"
		r.status.LoginAvailable = false
		r.status.ConsecutiveFailures++
		return false
	}
	r.active = m
	r.status.State = "available"
	if m.config.Mode == "oauth2_userinfo" {
		r.status.State = "configured_unverified"
	}
	r.status.LoginAvailable = true
	r.status.LastSuccessAt = now
	r.status.ConsecutiveFailures = 0
	return true
}

func (r *Runtime) Status() RuntimeStatus { r.mu.RLock(); defer r.mu.RUnlock(); return r.status }
func (r *Runtime) LoginAvailable() bool  { return r.Status().LoginAvailable }
func (r *Runtime) Authenticate(req *http.Request) (Principal, Session, error) {
	return r.sessions.Authenticate(req)
}
func (r *Runtime) ValidateCSRF(req *http.Request, session Session) error {
	return r.sessions.ValidateCSRF(req, session)
}

func (r *Runtime) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", r.Login)
	mux.HandleFunc("GET /auth/callback", r.Callback)
	// Logging out must remain possible during an identity provider outage.
	mux.HandleFunc("POST /auth/logout", r.sessions.Logout)
}
func (r *Runtime) loginManager(w http.ResponseWriter) *Manager {
	r.mu.RLock()
	manager := r.active
	r.mu.RUnlock()
	if manager == nil {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "identity_unavailable", "身份服务暂时不可用，系统正在自动重试；已有会话仍可使用")
	}
	return manager
}
func (r *Runtime) Login(w http.ResponseWriter, req *http.Request) {
	if m := r.loginManager(w); m != nil {
		m.Login(w, req)
	}
}
func (r *Runtime) Callback(w http.ResponseWriter, req *http.Request) {
	if m := r.loginManager(w); m != nil {
		m.Callback(w, req)
	}
}
