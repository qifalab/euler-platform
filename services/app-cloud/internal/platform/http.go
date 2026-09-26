package platform

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/connectors"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/identity"
)

type Authenticator interface {
	Authenticate(*http.Request) (identity.Principal, identity.Session, error)
	ValidateCSRF(*http.Request, identity.Session) error
}
type Handler struct {
	store          *Store
	auth           Authenticator
	connectors     *connectors.Manager
	mux            *http.ServeMux
	operations     [64]sync.Mutex
	modules        map[string]appkit.Module
	administrators []Administrator
}

func NewHandler(store *Store, auth Authenticator, products *connectors.Manager, options ...HandlerOption) *Handler {
	h := &Handler{store: store, auth: auth, connectors: products, mux: http.NewServeMux()}
	for _, option := range options {
		option(h)
	}
	h.routes()
	h.nativeRoutes()
	return h
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.mux.ServeHTTP(w, r)
}

type endpoint func(http.ResponseWriter, *http.Request, identity.Principal) error

func (h *Handler) route(pattern string, fn endpoint) {
	h.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if h.auth == nil {
			writeError(w, "unauthenticated", "Sign in is required", 401)
			return
		}
		p, session, e := h.auth.Authenticate(r)
		if e != nil {
			writeError(w, "unauthenticated", "Sign in is required", 401)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if e = h.auth.ValidateCSRF(r, session); e != nil {
				writeError(w, "csrf_invalid", "The request could not be verified", 403)
				return
			}
		}
		// Native deployments never expose legacy upstream credentials or mutations.
		if len(h.modules) > 0 && r.PathValue("id") != "" && strings.Contains(r.URL.Path, "/installations/") {
			for _, suffix := range []string{"/connection", "/summary", "/resources", "/operations"} {
				if strings.Contains(r.URL.Path, suffix) {
					writeError(w, "native_application", "请使用欧拉应用工作台", 410)
					return
				}
			}
		}
		// Connection changes cannot race a resource call and attach its result to
		// another account. The lock spans authorization, upstream I/O and recording.
		if id := r.PathValue("id"); id != "" {
			hash := fnv.New64a()
			_, _ = hash.Write([]byte(id))
			mu := &h.operations[hash.Sum64()%uint64(len(h.operations))]
			mu.Lock()
			defer mu.Unlock()
		}
		if e = fn(w, r, p); e != nil {
			respondError(w, e)
		}
	})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return ErrInvalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrInvalid
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func items(w http.ResponseWriter, v any) { writeJSON(w, 200, map[string]any{"items": v}) }
func writeError(w http.ResponseWriter, code, msg string, status int) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
func respondError(w http.ResponseWriter, e error) {
	var ce *connectors.Error
	var ae *appkit.Error
	switch {
	case errors.As(e, &ae):
		appkit.RespondError(w, e)
	case errors.Is(e, ErrInvalid):
		writeError(w, "invalid_request", "The request contains invalid fields", 400)
	case errors.Is(e, ErrNotFound):
		writeError(w, "not_found", "Resource not found", 404)
	case errors.Is(e, ErrForbidden):
		writeError(w, "forbidden", "You do not have permission for this operation", 403)
	case errors.Is(e, ErrConflict):
		writeError(w, "conflict", e.Error(), 409)
	case errors.As(e, &ce):
		writeError(w, ce.Code, ce.Message, ce.StatusCode)
	default:
		writeError(w, "internal_error", "The operation could not be completed", 500)
	}
}
func scope(r *http.Request) (string, string, string) {
	return r.PathValue("tenantID"), r.PathValue("projectID"), r.PathValue("id")
}

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		if h.auth == nil {
			writeJSON(w, 200, map[string]any{"authenticated": false, "loginAvailable": false, "loginError": "Identity provider is not configured or unavailable"})
			return
		}
		p, s, e := h.auth.Authenticate(r)
		if e != nil {
			writeJSON(w, 200, map[string]any{"authenticated": false, "loginAvailable": true})
			return
		}
		writeJSON(w, 200, map[string]any{"authenticated": true, "loginAvailable": true, "platformAdmin": h.isPlatformAdmin(p), "csrfToken": s.CSRFToken, "user": map[string]string{"id": p.ID, "displayName": p.Name, "provider": p.Provider, "subject": p.Subject}})
	})
	h.mux.HandleFunc("GET /api/v1/catalog", func(w http.ResponseWriter, r *http.Request) { items(w, h.catalog()) })
	h.route("GET /api/v1/tenants", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		v, e := h.store.Tenants(r.Context(), p.ID)
		if e == nil {
			items(w, v)
		}
		return e
	})
	h.route("POST /api/v1/tenants", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			Name string `json:"name"`
		}
		if e := decode(w, r, &b); e != nil {
			return e
		}
		v, e := h.store.CreateTenant(r.Context(), p.ID, b.Name)
		if e == nil {
			writeJSON(w, 201, v)
		}
		return e
	})
	h.route("GET /api/v1/tenants/{tenantID}/members", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, _, _ := scope(r)
		v, e := h.store.Members(r.Context(), p.ID, t)
		if e == nil {
			items(w, v)
		}
		return e
	})
	h.route("PATCH /api/v1/tenants/{tenantID}/members/{userID}", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			Role string `json:"role"`
		}
		if e := decode(w, r, &b); e != nil {
			return e
		}
		t, _, _ := scope(r)
		e := h.store.ChangeMember(r.Context(), p.ID, t, r.PathValue("userID"), b.Role, false)
		if e == nil {
			w.WriteHeader(204)
		}
		return e
	})
	h.route("DELETE /api/v1/tenants/{tenantID}/members/{userID}", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, _, _ := scope(r)
		e := h.store.ChangeMember(r.Context(), p.ID, t, r.PathValue("userID"), "", true)
		if e == nil {
			w.WriteHeader(204)
		}
		return e
	})
	h.route("POST /api/v1/tenants/{tenantID}/invitations", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			Role string `json:"role"`
		}
		if e := decode(w, r, &b); e != nil {
			return e
		}
		t, _, _ := scope(r)
		v, e := h.store.CreateInvitation(r.Context(), p.ID, t, b.Role)
		if e == nil {
			writeJSON(w, 201, v)
		}
		return e
	})
	h.route("GET /api/v1/tenants/{tenantID}/invitations", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, _, _ := scope(r)
		v, e := h.store.Invitations(r.Context(), p.ID, t)
		if e == nil {
			items(w, v)
		}
		return e
	})
	h.route("DELETE /api/v1/tenants/{tenantID}/invitations/{id}", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, _, id := scope(r)
		e := h.store.RevokeInvitation(r.Context(), p.ID, t, id)
		if e == nil {
			w.WriteHeader(204)
		}
		return e
	})
	h.route("POST /api/v1/invitations/accept", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			Token string `json:"token"`
		}
		if e := decode(w, r, &b); e != nil {
			return e
		}
		v, e := h.store.AcceptInvitation(r.Context(), p.ID, b.Token)
		if e == nil {
			writeJSON(w, 200, v)
		}
		return e
	})
	h.route("GET /api/v1/tenants/{tenantID}/projects", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, _, _ := scope(r)
		v, e := h.store.Projects(r.Context(), p.ID, t)
		if e == nil {
			items(w, v)
		}
		return e
	})
	h.route("POST /api/v1/tenants/{tenantID}/projects", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			Name string `json:"name"`
		}
		if e := decode(w, r, &b); e != nil {
			return e
		}
		t, _, _ := scope(r)
		v, e := h.store.CreateProject(r.Context(), p.ID, t, b.Name)
		if e == nil {
			writeJSON(w, 201, v)
		}
		return e
	})
	h.route("GET /api/v1/tenants/{tenantID}/projects/{projectID}/members", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, pr, _ := scope(r)
		v, e := h.store.ProjectMembers(r.Context(), p.ID, t, pr)
		if e == nil {
			items(w, v)
		}
		return e
	})
	h.route("POST /api/v1/tenants/{tenantID}/projects/{projectID}/members", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			UserID string `json:"userId"`
			Role   string `json:"role"`
		}
		if e := decode(w, r, &b); e != nil {
			return e
		}
		t, pr, _ := scope(r)
		e := h.store.SetProjectMember(r.Context(), p.ID, t, pr, b.UserID, b.Role, false)
		if e == nil {
			w.WriteHeader(204)
		}
		return e
	})
	h.route("DELETE /api/v1/tenants/{tenantID}/projects/{projectID}/members/{userID}", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, pr, _ := scope(r)
		e := h.store.SetProjectMember(r.Context(), p.ID, t, pr, r.PathValue("userID"), "", true)
		if e == nil {
			w.WriteHeader(204)
		}
		return e
	})
	h.route("GET /api/v1/tenants/{tenantID}/audit", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, _, _ := scope(r)
		limit, e := strconv.Atoi(r.URL.Query().Get("limit"))
		if e != nil {
			limit = 50
		}
		v, e := h.store.Audit(r.Context(), p.ID, t, r.URL.Query().Get("projectId"), limit, r.URL.Query().Get("before"))
		if e == nil {
			items(w, v)
		}
		return e
	})
	base := "/api/v1/tenants/{tenantID}/projects/{projectID}/installations"
	h.route("GET "+base, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, pr, _ := scope(r)
		v, e := h.store.Installations(r.Context(), p.ID, t, pr)
		if e == nil {
			items(w, v)
		}
		return e
	})
	h.route("POST "+base, func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			ApplicationID string `json:"applicationId"`
		}
		if e := decode(w, r, &b); e != nil {
			return e
		}
		found := false
		for _, a := range h.connectors.Catalog() {
			if a.ID == b.ApplicationID {
				found = true
			}
		}
		if !found {
			return ErrInvalid
		}
		t, pr, _ := scope(r)
		v, e := h.store.EnableApplication(r.Context(), p.ID, t, pr, b.ApplicationID)
		if e == nil {
			writeJSON(w, 201, v)
		}
		return e
	})
	h.route("PATCH "+base+"/{id}", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		var b struct {
			Status string `json:"status"`
		}
		if e := decode(w, r, &b); e != nil {
			return e
		}
		t, pr, id := scope(r)
		if e := h.store.SetInstallationStatus(r.Context(), p.ID, t, pr, id, b.Status); e != nil {
			return e
		}
		v, e := h.store.Installation(r.Context(), p.ID, t, pr, id, "read")
		if e == nil {
			writeJSON(w, 200, v)
		}
		return e
	})
	h.route("PUT "+base+"/{id}/connection", h.putConnection)
	h.route("DELETE "+base+"/{id}/connection", func(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
		t, pr, id := scope(r)
		e := h.store.ClearConnection(r.Context(), p.ID, t, pr, id)
		if e == nil {
			w.WriteHeader(204)
		}
		return e
	})
	h.route("GET "+base+"/{id}/summary", h.summary)
	h.route("GET "+base+"/{id}/resources", h.resources)
	h.route("POST "+base+"/{id}/resources", h.createResource)
	h.route("DELETE "+base+"/{id}/resources/{resourceID}", h.deleteResource)
}
func connectorConnection(c Connection) connectors.Connection {
	return connectors.Connection{BaseURL: c.BaseURL, Credential: c.Credential, ExternalAccountID: c.ExternalAccountID}
}
func (h *Handler) putConnection(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
	var b struct {
		BaseURL           string `json:"baseUrl"`
		Credential        string `json:"credential"`
		ExternalAccountID string `json:"externalAccountId"`
	}
	if e := decode(w, r, &b); e != nil {
		return e
	}
	t, pr, id := scope(r)
	v, old, e := h.store.ConnectionFor(r.Context(), p.ID, t, pr, id, "admin")
	if e != nil {
		return e
	}
	if b.Credential == "" {
		if old.Credential != "" && strings.TrimRight(b.BaseURL, "/") != strings.TrimRight(old.BaseURL, "/") {
			return ErrInvalid
		}
		b.Credential = old.Credential
	}
	// externalAccountId is accepted for compatibility, but only the connector's
	// independently verified upstream identity is persisted.
	c, e := h.connectors.ValidateConnectionContext(r.Context(), v.ApplicationID, connectors.Connection{BaseURL: b.BaseURL, Credential: b.Credential})
	if e != nil {
		return e
	}
	if e = h.store.SaveConnection(r.Context(), p.ID, t, pr, id, Connection{BaseURL: c.BaseURL, Credential: c.Credential, ExternalAccountID: c.ExternalAccountID}); e != nil {
		return e
	}
	v, e = h.store.Installation(r.Context(), p.ID, t, pr, id, "read")
	if e == nil {
		writeJSON(w, 200, v)
	}
	return e
}
func (h *Handler) summary(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
	t, pr, id := scope(r)
	v, c, e := h.store.ConnectionFor(r.Context(), p.ID, t, pr, id, "read")
	if e != nil {
		return e
	}
	if v.Status != "enabled" {
		writeJSON(w, 200, connectors.Summary{State: "unavailable", Message: "Application is disabled in this project", CheckedAt: time.Now().UTC()})
		return nil
	}
	out := h.connectors.Summary(r.Context(), v.ApplicationID, connectorConnection(c), connectors.Subject{Provider: p.Provider, ID: p.Subject})
	writeJSON(w, 200, out)
	return nil
}
func converted(r connectors.Resource) Resource {
	return Resource{ID: r.ID, Name: r.Name, Type: r.Type, Status: r.Status, URL: r.URL}
}
func (h *Handler) capability(app, capability string) error {
	for _, a := range h.connectors.Catalog() {
		if a.ID == app {
			for _, c := range a.Capabilities {
				if c == capability {
					return nil
				}
			}
		}
	}
	return &connectors.Error{Code: "unsupported", Message: "This application does not support the requested resource operation", StatusCode: 501}
}
func (h *Handler) resources(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
	t, pr, id := scope(r)
	v, c, e := h.store.ConnectionFor(r.Context(), p.ID, t, pr, id, "read")
	if e != nil {
		return e
	}
	if v.Status != "enabled" {
		return ErrConflict
	}
	rs, e := h.connectors.Resources(r.Context(), v.ApplicationID, connectorConnection(c))
	if e != nil {
		return e
	}
	out := make([]Resource, 0, len(rs))
	for _, r := range rs {
		out = append(out, converted(r))
	}
	if e = h.store.TrackResources(r.Context(), p.ID, t, pr, id, out); e != nil {
		return e
	}
	items(w, out)
	return nil
}
func (h *Handler) createResource(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
	var b connectors.CreateResource
	if e := decode(w, r, &b); e != nil {
		return e
	}
	t, pr, id := scope(r)
	v, c, e := h.store.ConnectionFor(r.Context(), p.ID, t, pr, id, "write")
	if e != nil {
		return e
	}
	if v.Status != "enabled" {
		return ErrConflict
	}
	if e = h.capability(v.ApplicationID, "resources:create"); e != nil {
		return e
	}
	op, e := h.store.BeginResourceOperation(r.Context(), p.ID, t, pr, id, "create", "")
	if e != nil {
		return e
	}
	w.Header().Set("X-Euler-Operation-Id", op)
	out, e := h.connectors.Create(r.Context(), v.ApplicationID, connectorConnection(c), b)
	if e != nil {
		h.recordUncertain(op)
		return e
	}
	resource := converted(out)
	complete, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e = h.store.CompleteResourceOperation(complete, op, &resource); e != nil {
		return e
	}
	writeJSON(w, 201, resource)
	return nil
}
func (h *Handler) deleteResource(w http.ResponseWriter, r *http.Request, p identity.Principal) error {
	t, pr, id := scope(r)
	v, c, e := h.store.ConnectionFor(r.Context(), p.ID, t, pr, id, "write")
	if e != nil {
		return e
	}
	if v.Status != "enabled" {
		return ErrConflict
	}
	if e = h.capability(v.ApplicationID, "resources:delete"); e != nil {
		return e
	}
	resource := r.PathValue("resourceID")
	if e = h.store.RequireResource(r.Context(), p.ID, t, pr, id, resource); e != nil {
		return e
	}
	op, e := h.store.BeginResourceOperation(r.Context(), p.ID, t, pr, id, "delete", resource)
	if e != nil {
		return e
	}
	w.Header().Set("X-Euler-Operation-Id", op)
	if e = h.connectors.Delete(r.Context(), v.ApplicationID, connectorConnection(c), resource); e != nil {
		h.recordUncertain(op)
		return e
	}
	complete, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e = h.store.CompleteResourceOperation(complete, op, nil); e != nil {
		return e
	}
	w.WriteHeader(204)
	return nil
}
func (h *Handler) recordUncertain(op string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.store.MarkOperationUncertain(ctx, op)
}
