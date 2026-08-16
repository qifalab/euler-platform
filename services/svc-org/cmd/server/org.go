// Package main: org domain handlers for svc-org.
//
// This file implements the org domain (03§4.1/§4.2) inline — there is no
// pkg-go/org package — backed by a thread-safe in-memory store so the stdlib
// HTTP server is runnable standalone in phase 1. The store is shaped like the
// pkg-go Store interfaces (e.g. pkg-go/quota.Store): the handler layer talks
// to an interface so a production MySQL/Redis implementation can be dropped in
// without touching the handlers.
//
// Project (03§4.1.2): {project_id, account_id, name, parent_id}.
// Tag:              (account_id, key, value).
//
// Tenancy comes from the X-Sc-Account-Id header injected by the gateway
// (04§3.1); a request without it is rejected with 403. Every read/write is
// scoped to that account.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const accountHeader = "X-Sc-Account-Id"

// project is the org unit a resource belongs to (03§4.1.2). parent_id = "" for
// a top-level project; otherwise it references another project_id of the same
// account.
type project struct {
	ProjectID string `json:"project_id"`
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	ParentID  string `json:"parent_id"`
}

// tag is an account-scoped (key, value) pair (03§4.1.2).
type tag struct {
	AccountID string `json:"account_id"`
	Key       string `json:"key"`
	Value     string `json:"value"`
}

// --- Store -------------------------------------------------------------------

// orgStore is the persistence boundary for the org domain. Phase-1 uses an
// in-memory implementation; production backs this with MySQL (sharded by
// account_id) without changing the handlers.
type orgStore interface {
	CreateProject(p project) error
	GetProject(accountID, projectID string) (project, error)
	ListProjects(accountID string) ([]project, error)
	UpdateProject(accountID, projectID string, name *string, parentID *string) error

	MoveResource(accountID, resourceID, fromProjectID, toProjectID string) error

	PutTag(t tag) error
	DeleteTag(accountID, key string) error
	ListTags(accountID string) ([]tag, error)
}

// memStore is the in-memory orgStore. All access is guarded by a mutex so the
// stdlib server is safe under concurrent requests.
type memStore struct {
	mu        sync.RWMutex
	projects  map[string]project // key: accountID + ":" + projectID
	tags      map[string]tag     // key: accountID + ":" + key
	resources map[string]string  // resourceID -> projectID (per account via resource key)
}

func newMemStore() *memStore {
	return &memStore{
		projects:  make(map[string]project),
		tags:      make(map[string]tag),
		resources: make(map[string]string),
	}
}

func projKey(accountID, projectID string) string { return accountID + ":" + projectID }
func tagKey(accountID, key string) string        { return accountID + ":" + key }
func resKey(accountID, resourceID string) string { return accountID + ":" + resourceID }

// --- Store errors ------------------------------------------------------------

var (
	errProjectNotFound = errors.New("org: project not found")
	errTagNotFound     = errors.New("org: tag not found")
	errProjectExists   = errors.New("org: project already exists")
)

// --- memStore: projects ------------------------------------------------------

func (s *memStore) CreateProject(p project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.projects[projKey(p.AccountID, p.ProjectID)]; ok {
		return errProjectExists
	}
	s.projects[projKey(p.AccountID, p.ProjectID)] = p
	return nil
}

func (s *memStore) GetProject(accountID, projectID string) (project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[projKey(accountID, projectID)]
	if !ok {
		return project{}, errProjectNotFound
	}
	return p, nil
}

func (s *memStore) ListProjects(accountID string) ([]project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []project
	for _, p := range s.projects {
		if p.AccountID == accountID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out, nil
}

func (s *memStore) UpdateProject(accountID, projectID string, name *string, parentID *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := projKey(accountID, projectID)
	p, ok := s.projects[key]
	if !ok {
		return errProjectNotFound
	}
	if name != nil {
		p.Name = *name
	}
	if parentID != nil {
		p.ParentID = *parentID
	}
	s.projects[key] = p
	return nil
}

// --- memStore: resources -----------------------------------------------------

func (s *memStore) MoveResource(accountID, resourceID, fromProjectID, toProjectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate both projects exist and belong to the account.
	if _, ok := s.projects[projKey(accountID, fromProjectID)]; !ok {
		return errProjectNotFound
	}
	if _, ok := s.projects[projKey(accountID, toProjectID)]; !ok {
		return errProjectNotFound
	}

	key := resKey(accountID, resourceID)
	current, ok := s.resources[key]
	if !ok {
		// First time this resource is seen by the org domain: treat the move
		// as also registering its membership in the destination project.
		s.resources[key] = toProjectID
		return nil
	}
	if current != fromProjectID {
		return fmt.Errorf("org: resource %s is in project %s, not %s", resourceID, current, fromProjectID)
	}
	s.resources[key] = toProjectID
	return nil
}

// --- memStore: tags ----------------------------------------------------------

func (s *memStore) PutTag(t tag) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tags[tagKey(t.AccountID, t.Key)] = t
	return nil
}

func (s *memStore) DeleteTag(accountID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tags[tagKey(accountID, key)]; !ok {
		return errTagNotFound
	}
	delete(s.tags, tagKey(accountID, key))
	return nil
}

func (s *memStore) ListTags(accountID string) ([]tag, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []tag
	for _, t := range s.tags {
		if t.AccountID == accountID {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// --- orgService --------------------------------------------------------------

// orgService wires the org domain into the HTTP layer. It is the "manager"
// equivalent of pkg-go/quota.Manager: it owns the store and exposes the
// use-cases the handlers call.
type orgService struct {
	store orgStore
}

func newOrgService() *orgService {
	return &orgService{store: newMemStore()}
}

// --- Handler helpers ---------------------------------------------------------

// accountID extracts the gateway-injected tenancy header. An empty account is
// 403 — the caller is not authenticated, so nothing can be served.
func accountID(r *http.Request) (string, error) {
	a := strings.TrimSpace(r.Header.Get(accountHeader))
	if a == "" {
		return "", errMissingAccount
	}
	return a, nil
}

var errMissingAccount = errors.New("org: missing X-Sc-Account-Id")

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"Code": code, "Message": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "invalid request body: "+err.Error())
		return false
	}
	return true
}

// --- Projects ----------------------------------------------------------------

type createProjectReq struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	ParentID  string `json:"parent_id"`
}

type updateProjectReq struct {
	Name     *string `json:"name"`
	ParentID *string `json:"parent_id"`
}

// handleProjects routes POST (create) and GET (list) on /internal/projects.
func (s *orgService) handleProjects(w http.ResponseWriter, r *http.Request) {
	acct, err := accountID(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, "Common.Unauthorized", "missing "+accountHeader)
		return
	}

	switch r.Method {
	case http.MethodPost:
		var req createProjectReq
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Name == "" {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "name is required")
			return
		}
		pid := req.ProjectID
		if pid == "" {
			pid = uuid.NewString()
		}
		p := project{ProjectID: pid, AccountID: acct, Name: req.Name, ParentID: req.ParentID}
		if err := s.store.CreateProject(p); err != nil {
			writeErr(w, http.StatusConflict, "Project.AlreadyExists", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p)

	case http.MethodGet:
		projects, err := s.store.ListProjects(acct)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Common.InternalError", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"projects": projects})

	default:
		writeErr(w, http.StatusMethodNotAllowed, "Common.MethodNotAllowed", "method not allowed")
	}
}

// handleProjectByID routes PATCH on /internal/projects/{id}.
func (s *orgService) handleProjectByID(w http.ResponseWriter, r *http.Request) {
	acct, err := accountID(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, "Common.Unauthorized", "missing "+accountHeader)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/internal/projects/")
	id = strings.Trim(id, "/")
	if id == "" {
		writeErr(w, http.StatusNotFound, "Project.NotFound", "project id required")
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var req updateProjectReq
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Name == nil && req.ParentID == nil {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "nothing to update")
			return
		}
		if err := s.store.UpdateProject(acct, id, req.Name, req.ParentID); err != nil {
			if errors.Is(err, errProjectNotFound) {
				writeErr(w, http.StatusNotFound, "Project.NotFound", err.Error())
				return
			}
			writeErr(w, http.StatusInternalServerError, "Common.InternalError", err.Error())
			return
		}
		p, _ := s.store.GetProject(acct, id)
		writeJSON(w, http.StatusOK, p)

	default:
		writeErr(w, http.StatusMethodNotAllowed, "Common.MethodNotAllowed", "method not allowed")
	}
}

// --- Resource move -----------------------------------------------------------

type moveResourceReq struct {
	ResourceID    string `json:"resource_id"`
	FromProjectID string `json:"from_project_id"`
	ToProjectID   string `json:"to_project_id"`
	ProjectID     string `json:"project_id"`     // destination, alternate field name
	DestinationID string `json:"destination_id"` // destination, alternate field name
}

// handleResourceMove moves a resource between projects on POST
// /internal/resources/move. project_id on the resource_instance ledger is the
// org domain's project membership (03§6.2); here it is stored in-process and
// would be written back to the resource_instance row by svc-orchestrator in
// production.
func (s *orgService) handleResourceMove(w http.ResponseWriter, r *http.Request) {
	acct, err := accountID(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, "Common.Unauthorized", "missing "+accountHeader)
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "Common.MethodNotAllowed", "method not allowed")
		return
	}

	var req moveResourceReq
	if !decodeBody(w, r, &req) {
		return
	}
	// Accept either "project_id"/"destination_id" or "to_project_id".
	to := req.ToProjectID
	if to == "" {
		to = req.ProjectID
	}
	if to == "" {
		to = req.DestinationID
	}
	if req.ResourceID == "" || to == "" {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "resource_id and destination project are required")
		return
	}

	if err := s.store.MoveResource(acct, req.ResourceID, req.FromProjectID, to); err != nil {
		switch {
		case errors.Is(err, errProjectNotFound):
			writeErr(w, http.StatusNotFound, "Project.NotFound", err.Error())
		default:
			writeErr(w, http.StatusConflict, "Resource.MoveConflict", err.Error())
		}
		return
	}

	// Emit the membership change for traceability/audit (03§6.2). In-process
	// the move is already applied above; production would write project_id back
	// to the resource_instance ledger.
	slog.Info("resource moved",
		"account_id", acct,
		"resource_id", req.ResourceID,
		"from_project", req.FromProjectID,
		"to_project", to,
	)
	writeJSON(w, http.StatusOK, map[string]string{
		"resource_id": req.ResourceID,
		"project_id":  to,
	})
}

// --- Tags --------------------------------------------------------------------

type putTagReq struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type delTagReq struct {
	Key string `json:"key"`
}

// handleTags routes POST (upsert), GET (list) and DELETE on /internal/tags.
func (s *orgService) handleTags(w http.ResponseWriter, r *http.Request) {
	acct, err := accountID(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, "Common.Unauthorized", "missing "+accountHeader)
		return
	}

	switch r.Method {
	case http.MethodPost:
		var req putTagReq
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Key == "" {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "key is required")
			return
		}
		t := tag{AccountID: acct, Key: req.Key, Value: req.Value}
		if err := s.store.PutTag(t); err != nil {
			writeErr(w, http.StatusInternalServerError, "Common.InternalError", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, t)

	case http.MethodGet:
		tags, err := s.store.ListTags(acct)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Common.InternalError", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tags": tags})

	case http.MethodDelete:
		key := strings.TrimSpace(r.URL.Query().Get("key"))
		if key == "" {
			var req delTagReq
			if !decodeBody(w, r, &req) {
				return
			}
			key = req.Key
		}
		if key == "" {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "key is required")
			return
		}
		if err := s.store.DeleteTag(acct, key); err != nil {
			if errors.Is(err, errTagNotFound) {
				writeErr(w, http.StatusNotFound, "Tag.NotFound", err.Error())
				return
			}
			writeErr(w, http.StatusInternalServerError, "Common.InternalError", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"deleted": key})

	default:
		writeErr(w, http.StatusMethodNotAllowed, "Common.MethodNotAllowed", "method not allowed")
	}
}
