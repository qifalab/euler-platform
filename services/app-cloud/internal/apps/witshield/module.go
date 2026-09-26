// Package witshield is Euler's native, project-isolated host security module.
package witshield

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	engine "github.com/qifalab/euler-platform/services/witshield-engine/integration"
)

type instance struct{ tenant, project, installation string }

func (i instance) purpose() string {
	return "witshield/v1/" + i.tenant + "/" + i.project + "/" + i.installation
}
func (i instance) path() string {
	d := sha256.Sum256([]byte(i.purpose()))
	return hex.EncodeToString(d[:])
}

type Module struct {
	rt      *appkit.Runtime
	mu      sync.Mutex
	engines map[string]*engine.Engine
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
}

func New(rt *appkit.Runtime) *Module {
	ctx, cancel := context.WithCancel(context.Background())
	return &Module{rt: rt, engines: map[string]*engine.Engine{}, ctx: ctx, cancel: cancel}
}
func (*Module) ID() string { return "witshield" }
func (m *Module) Migrate(ctx context.Context) error {
	if m.rt == nil || m.rt.DB == nil || m.rt.DeriveKey == nil || m.rt.ApplicationEnabled == nil || !filepath.IsAbs(m.rt.DataDir) {
		return errors.New("WitShield requires platform persistence, key derivation and enabled gate")
	}
	rows, err := m.rt.DB.QueryContext(ctx, `SELECT tenant_id,project_id,id FROM installations WHERE application_id='witshield' AND status='enabled'`)
	if err != nil {
		return err
	}
	var instances []instance
	for rows.Next() {
		var i instance
		if err = rows.Scan(&i.tenant, &i.project, &i.installation); err != nil {
			rows.Close()
			return err
		}
		instances = append(instances, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Restore enabled controllers at startup so resident scans, notifications and
	// investigation do not depend on someone opening a browser tab.
	for _, i := range instances {
		if _, err = m.engine(ctx, i); err != nil {
			return err
		}
	}
	return nil
}
func validID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func (m *Module) enabled(ctx context.Context, i instance) bool {
	if !validID(i.tenant) || !validID(i.project) || !validID(i.installation) {
		return false
	}
	ok, err := m.rt.ApplicationEnabled(ctx, i.tenant, i.project, "witshield")
	if err != nil || !ok {
		return false
	}
	var n int
	err = m.rt.DB.QueryRowContext(ctx, `SELECT count(*) FROM installations WHERE id=? AND tenant_id=? AND project_id=? AND application_id='witshield' AND status='enabled'`, i.installation, i.tenant, i.project).Scan(&n)
	return err == nil && n == 1
}
func (m *Module) engine(ctx context.Context, i instance) (*engine.Engine, error) {
	if !m.enabled(ctx, i) {
		return nil, appkit.NotFound()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, appkit.Unavailable("安全引擎正在关闭")
	}
	if e := m.engines[i.path()]; e != nil {
		return e, nil
	}
	e, err := engine.New(m.ctx, engine.Config{DataDir: filepath.Join(m.rt.DataDir, "witshield", i.path()), Key: m.rt.DeriveKey(i.purpose()), Version: "euler-native", Enabled: func(ctx context.Context) bool { return m.enabled(ctx, i) }})
	if err != nil {
		return nil, err
	}
	m.engines[i.path()] = e
	return e, nil
}
func (m *Module) scoped(ctx context.Context, s appkit.Scope) (*engine.Engine, error) {
	if s.ApplicationID != "witshield" {
		return nil, appkit.NotFound()
	}
	return m.engine(ctx, instance{s.TenantID, s.ProjectID, s.InstallationID})
}
func (m *Module) agentURL(i instance) string {
	return strings.TrimRight(m.rt.PublicURL, "/") + "/public/witshield/" + url.PathEscape(i.tenant) + "/" + url.PathEscape(i.project) + "/" + url.PathEscape(i.installation)
}
func (m *Module) Handler() http.Handler {
	mux := http.NewServeMux()
	appkit.Handle(mux, "GET /instance", "read", func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
		if _, err := m.scoped(r.Context(), s); err != nil {
			return err
		}
		appkit.JSON(w, 200, map[string]any{"mode": "euler", "installationId": s.InstallationID, "publicAgentURL": m.agentURL(instance{s.TenantID, s.ProjectID, s.InstallationID}), "managementRoutes": engine.ManagementRoutes(), "agentRouteCount": 8, "upstreamRevision": "17dd1f5f1a743fa8428668f15f2398df11d83e9d"})
		return nil
	})
	for _, route := range engine.ManagementRoutes() {
		pattern := strings.Replace(route.Pattern, "/api/v1", "", 1)
		appkit.Handle(mux, pattern, route.Permission, func(w http.ResponseWriter, r *http.Request, s appkit.Scope) error {
			e, err := m.scoped(r.Context(), s)
			if err != nil {
				return err
			}
			mutates := r.Method != http.MethodGet && r.Method != http.MethodHead
			if mutates {
				if err = m.rt.Audit(r.Context(), nil, s, "operation.requested", s.InstallationID, "安全引擎操作请求："+route.Pattern); err != nil {
					return err
				}
			}
			clone := r.Clone(r.Context())
			u := *r.URL
			u.Path = "/api/v1" + r.URL.Path
			u.RawPath = ""
			clone.URL = &u
			if !mutates {
				e.ServeManagement(w, clone, s.ActorID)
				return nil
			}
			// Capture the bounded response until the completion audit is durable. A
			// failed audit must never turn an unknown external outcome into success.
			recorder := newResponse()
			e.ServeManagement(recorder, clone, s.ActorID)
			if mutates {
				if err = m.rt.Audit(r.Context(), nil, s, "operation.completed", s.InstallationID, fmt.Sprintf("安全引擎操作结果：%s，HTTP %d", route.Pattern, recorder.statusCode())); err != nil {
					return appkit.Unavailable("操作结果审计暂不可用，请查询当前状态后再决定是否重试")
				}
			}
			recorder.writeTo(w)
			return nil
		})
	}
	return mux
}
func (m *Module) PublicHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The platform strips only /public/witshield from URL.Path and leaves
		// RequestURI intact. Original URI is passed as typed context, never a header.
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 4)
		if len(parts) != 4 || !strings.HasPrefix(parts[3], "agent/v1/") {
			http.NotFound(w, r)
			return
		}
		i := instance{parts[0], parts[1], parts[2]}
		e, err := m.engine(r.Context(), i)
		if err != nil {
			appkit.RespondError(w, err)
			return
		}
		expectedPrefix := "/public/witshield/" + i.tenant + "/" + i.project + "/" + i.installation + "/"
		if !strings.HasPrefix(r.RequestURI, expectedPrefix) {
			http.NotFound(w, r)
			return
		}
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Path = "/" + parts[3]
		u.RawPath = ""
		clone.URL = &u
		e.ServeAgent(w, clone, r.RequestURI)
	})
}
func (m *Module) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	instances := make([]*engine.Engine, 0, len(m.engines))
	for _, e := range m.engines {
		instances = append(instances, e)
	}
	m.mu.Unlock()
	var errs []error
	for _, e := range instances {
		errs = append(errs, e.Close())
	}
	return errors.Join(errs...)
}

// Business API responses are bounded by the engine's list and body limits.
// Keeping this local also preserves response status for the platform audit.
type response struct {
	header http.Header
	status int
	body   []byte
}

func newResponse() *response            { return &response{header: make(http.Header)} }
func (r *response) Header() http.Header { return r.header }
func (r *response) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *response) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	r.body = append(r.body, b...)
	return len(b), nil
}
func (r *response) statusCode() int {
	if r.status == 0 {
		return 200
	}
	return r.status
}
func (r *response) writeTo(w http.ResponseWriter) {
	for key, values := range r.header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(r.statusCode())
	_, _ = w.Write(r.body)
}
