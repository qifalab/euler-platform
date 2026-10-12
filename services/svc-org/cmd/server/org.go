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
// Tenancy comes from the X-Euler-Account-Id header injected by the gateway
// (04§3.1); a request without it is rejected with 403. Every read/write is
// scoped to that account.
package main

import (
	"context"
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

const accountHeader = "X-Euler-Account-Id"

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
	// CreateProject takes a pointer because the store owns the project id: the
	// schema's key is a 号段-issued BIGINT (04§6.6), so the persistent store
	// assigns one and the caller must see it; the in-memory store assigns a
	// UUID. A caller-supplied id is never used — project_id is the table's
	// global primary key, so honouring it let one tenant squat ids another
	// tenant's creates would need. Names are unique per account
	// (errProjectExists); the parent must be a project of the same account
	// (errInvalidParent).
	CreateProject(p *project) error
	GetProject(accountID, projectID string) (project, error)
	ListProjects(accountID string) ([]project, error)
	// UpdateProject renames and/or reparents. A rename onto another project's
	// name is errProjectExists; a parent that is missing, another account's,
	// the project itself or one of its descendants is errInvalidParent.
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
	// errInvalidParent: parent_id is not a project of the caller's account, is
	// the project itself, or would close a cycle in the tree.
	errInvalidParent = errors.New("org: invalid parent project")
	// errResourceNotFound: the resource is not one the caller's account can
	// see (its membership row belongs to another account).
	errResourceNotFound = errors.New("org: resource not found")
	// errResourceConflict: the move's from-project is not where the resource is.
	errResourceConflict = errors.New("org: resource move conflict")
)

// maxProjectDepth bounds the ancestor walk that validates a parent link: the
// schema keeps the tree shallow (no arbitrary-depth cross-shard JOINs), and a
// bound keeps a corrupted chain from looping forever.
const maxProjectDepth = 32

// --- memStore: projects ------------------------------------------------------

func (s *memStore) CreateProject(p *project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nameTakenLocked(p.AccountID, p.Name, "") {
		return errProjectExists
	}
	id := uuid.NewString()
	if err := s.checkParentLocked(p.AccountID, id, p.ParentID); err != nil {
		return err
	}
	p.ProjectID = id
	s.projects[projKey(p.AccountID, p.ProjectID)] = *p
	return nil
}

// nameTakenLocked is the in-memory uk_acc_name: is name used by another of the
// account's projects (other than except)?
func (s *memStore) nameTakenLocked(accountID, name, except string) bool {
	for _, q := range s.projects {
		if q.AccountID == accountID && q.Name == name && q.ProjectID != except {
			return true
		}
	}
	return false
}

// checkParentLocked validates parentID as the parent of projectID: it must be
// a project of the same account, not the project itself, and not one of its
// descendants (walking up from the parent must not reach projectID).
func (s *memStore) checkParentLocked(accountID, projectID, parentID string) error {
	if parentID == "" {
		return nil
	}
	if parentID == projectID {
		return fmt.Errorf("%w: a project cannot be its own parent", errInvalidParent)
	}
	cur := parentID
	for depth := 0; depth < maxProjectDepth; depth++ {
		p, ok := s.projects[projKey(accountID, cur)]
		if !ok {
			if depth == 0 {
				return fmt.Errorf("%w: parent project not found", errInvalidParent)
			}
			return nil
		}
		if p.ParentID == "" {
			return nil
		}
		if p.ParentID == projectID {
			return fmt.Errorf("%w: the parent is a descendant of this project", errInvalidParent)
		}
		cur = p.ParentID
	}
	return fmt.Errorf("%w: project tree is deeper than %d levels", errInvalidParent, maxProjectDepth)
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
		if s.nameTakenLocked(accountID, *name, projectID) {
			return errProjectExists
		}
		p.Name = *name
	}
	if parentID != nil {
		if err := s.checkParentLocked(accountID, projectID, *parentID); err != nil {
			return err
		}
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
		return fmt.Errorf("%w: resource %s is in project %s, not %s", errResourceConflict, resourceID, current, fromProjectID)
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

// newOrgService wires the service to its storage. Persistence is opt-in
// (pkg-go/storage doc): EULER_DB_DSN set → account_db; unset → the in-memory store
// that keeps the demo and `go test` dependency-free.
func newOrgService(ctx context.Context) (*orgService, error) {
	store, err := newStore(ctx)
	if err != nil {
		return nil, err
	}
	slog.Info("svc-org store ready", "persistent", persistentStore(store))
	return &orgService{store: store}, nil
}

// newInMemoryOrgService builds the demo service. Handler tests construct it
// directly, so they never depend on whether the developer's shell has EULER_DB_DSN
// set — a test that wrote projects into account_db would be a data incident.
func newInMemoryOrgService() *orgService {
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

var errMissingAccount = errors.New("org: missing X-Euler-Account-Id")

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"Code": code, "Message": msg})
}

// writeStoreErr maps a store error onto the API. Only the sentinels are the
// caller's errors; anything else is a storage failure whose text (driver
// messages, internal ids) is logged, never returned.
func writeStoreErr(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, errProjectNotFound):
		writeErr(w, http.StatusNotFound, "Project.NotFound", "project not found")
	case errors.Is(err, errProjectExists):
		writeErr(w, http.StatusConflict, "Project.AlreadyExists", "a project with this name already exists")
	case errors.Is(err, errInvalidAccount):
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "account id must be numeric")
	case errors.Is(err, errInvalidParent):
		writeErr(w, http.StatusBadRequest, "Project.InvalidParent", err.Error())
	case errors.Is(err, errResourceNotFound):
		writeErr(w, http.StatusNotFound, "Resource.NotFound", "resource not found")
	case errors.Is(err, errResourceConflict):
		writeErr(w, http.StatusConflict, "Resource.MoveConflict", err.Error())
	case errors.Is(err, errTagNotFound):
		writeErr(w, http.StatusNotFound, "Tag.NotFound", "tag not found")
	default:
		slog.Error("org store failure", "op", op, "err", err)
		writeErr(w, http.StatusInternalServerError, "Common.InternalError", "internal error")
	}
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
	// ProjectID is accepted for compatibility and ignored: ids are always
	// issued by the store (see orgStore.CreateProject).
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
		p := project{AccountID: acct, Name: req.Name, ParentID: req.ParentID}
		if err := s.store.CreateProject(&p); err != nil {
			writeStoreErr(w, "create project", err)
			return
		}
		writeJSON(w, http.StatusOK, p)

	case http.MethodGet:
		projects, err := s.store.ListProjects(acct)
		if err != nil {
			writeStoreErr(w, "list projects", err)
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
		if req.Name != nil && *req.Name == "" {
			writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "name must not be empty")
			return
		}
		if err := s.store.UpdateProject(acct, id, req.Name, req.ParentID); err != nil {
			writeStoreErr(w, "update project", err)
			return
		}
		p, err := s.store.GetProject(acct, id)
		if err != nil {
			writeStoreErr(w, "get project", err)
			return
		}
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
		writeStoreErr(w, "move resource", err)
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
			writeStoreErr(w, "put tag", err)
			return
		}
		writeJSON(w, http.StatusOK, t)

	case http.MethodGet:
		tags, err := s.store.ListTags(acct)
		if err != nil {
			writeStoreErr(w, "list tags", err)
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
			writeStoreErr(w, "delete tag", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"deleted": key})

	default:
		writeErr(w, http.StatusMethodNotAllowed, "Common.MethodNotAllowed", "method not allowed")
	}
}
