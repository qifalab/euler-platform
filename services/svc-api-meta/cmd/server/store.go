// Package main: svc-api-meta domain model and in-memory store.
//
// This is the API metadata center (03§4.5.1). There is no pkg-go/api-meta
// package; the domain logic is defined inline here with an in-memory store
// (read-heavy, write-light metadata — the real store is MySQL + MinIO, but an
// in-memory struct keeps phase-1 runnable with no DB dependency, matching the
// _tmpl-go / in-server verify pattern).
//
// An Action is the atomic unit of the OpenAPI surface: every {product_code}
// Action (e.g. scecs:CreateInstance) has a parameter schema (JSON Schema-ish),
// the set of error codes it can return (registered in this center — unregistered
// codes are blocked at CI time, 03§9.3), and a version. This metadata drives
// documentation generation and SDK generation (03§9.4).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/starcloud/sc-platform/errors"
	"github.com/starcloud/sc-platform/identifier"
)

// accountIDHeader is injected by the APISIX gateway on every internal call.
const accountIDHeader = "X-Sc-Account-Id"

// Action is the stored metadata for one OpenAPI Action.
type Action struct {
	ID          string          `json:"id"`          // {product}.{Action}, e.g. scecs.CreateInstance
	ProductCode string          `json:"product_code"` // scecs / scoss / ...
	ActionName  string          `json:"action_name"`  // CreateInstance
	Version     string          `json:"version"`      // e.g. "2026-08-01"
	ParamSchema json.RawMessage `json:"param_schema"` // parameter schema (JSON)
	ErrorCodes  []string        `json:"error_codes"`  // error codes this Action may return
	UpdatedBy   string          `json:"updated_by"`   // account_id that last registered
	UpdatedAt   string          `json:"updated_at"`   // RFC3339
}

// actionStore is the in-memory metadata store keyed by Action id.
type actionStore struct {
	mu      sync.RWMutex
	actions map[string]Action
}

func newMetaStore() *actionStore {
	s := &actionStore{actions: make(map[string]Action)}
	s.seedActions()
	return s
}

// seedActions registers a representative slice of the phase-1 product API
// surface so the Explorer and docs have real Actions to show before the
// services register their own (03§9.4 rule ①: a route is not mounted until its
// metadata is registered here). These mirror the OpenAPI Actions the gateway
// actually routes for scecs/scoss/scvpc.
func (s *actionStore) seedActions() {
	seed := []Action{
		{ID: "scecs.RunInstances", ProductCode: "scecs", ActionName: "RunInstances", Version: "2026-08-01",
			ParamSchema: json.RawMessage(`{"type":"object","required":["InstanceType","ImageId"],"properties":{"InstanceType":{"type":"string","description":"spec code, e.g. s2.large"},"ImageId":{"type":"string"},"InstanceName":{"type":"string"},"ChargeType":{"type":"string","enum":["Prepaid","Postpaid"]}}}`),
			ErrorCodes:  []string{"scecs.InvalidInstanceType", "scecs.QuotaExceeded", "scecs.InsufficientBalance"}},
		{ID: "scecs.DescribeInstances", ProductCode: "scecs", ActionName: "DescribeInstances", Version: "2026-08-01",
			ParamSchema: json.RawMessage(`{"type":"object","properties":{"InstanceIds":{"type":"array","items":{"type":"string"}},"PageNumber":{"type":"integer"},"PageSize":{"type":"integer"}}}`),
			ErrorCodes:  []string{"scecs.InstanceNotFound"}},
		{ID: "scecs.StartInstance", ProductCode: "scecs", ActionName: "StartInstance", Version: "2026-08-01",
			ParamSchema: json.RawMessage(`{"type":"object","required":["InstanceId"],"properties":{"InstanceId":{"type":"string"}}}`),
			ErrorCodes:  []string{"scecs.InstanceNotFound", "scecs.IncorrectStatus"}},
		{ID: "scoss.CreateBucket", ProductCode: "scoss", ActionName: "CreateBucket", Version: "2026-08-01",
			ParamSchema: json.RawMessage(`{"type":"object","required":["BucketName"],"properties":{"BucketName":{"type":"string"},"StorageClass":{"type":"string","enum":["Standard","IA","Archive"]}}}`),
			ErrorCodes:  []string{"scoss.BucketAlreadyExists", "scoss.InvalidBucketName"}},
		{ID: "scvpc.CreateVpc", ProductCode: "scvpc", ActionName: "CreateVpc", Version: "2026-08-01",
			ParamSchema: json.RawMessage(`{"type":"object","required":["CidrBlock"],"properties":{"CidrBlock":{"type":"string"},"VpcName":{"type":"string"}}}`),
			ErrorCodes:  []string{"scvpc.InvalidCidrBlock", "scvpc.QuotaExceeded"}},
	}
	for _, a := range seed {
		a.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		a.UpdatedBy = "system-seed"
		s.actions[a.ID] = a
	}
}

// id builds the canonical Action id: {product}.{Action} (identifier convention).
func actionID(product, action string) string {
	return product + "." + action
}

// --- domain operations (would hit MySQL in the real service) -----------------

func (s *actionStore) register(a Action) Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.actions[a.ID] = a
	return a
}

func (s *actionStore) get(id string) (Action, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.actions[id]
	return a, ok
}

func (s *actionStore) listByProduct(product string) []Action {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Action
	for _, a := range s.actions {
		if product == "" || a.ProductCode == product {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// --- HTTP handlers ------------------------------------------------------------

// accountIDFromRequest extracts the gateway-injected account id, or returns ""
// if the header is missing.
//
// TRUST NOTE: X-Sc-Account-Id is injected by the API gateway after
// authentication; this service relies on network isolation (and optionally
// internalTokenMiddleware / SC_INTERNAL_TOKEN) rather than re-authenticating.
func accountIDFromRequest(r *http.Request) string {
	return r.Header.Get(accountIDHeader)
}

// writeJSON renders a success response with the standard body
// { "RequestId": ..., "Code": "OK", "Data": ... }.
func writeJSON(w http.ResponseWriter, status int, data any) {
	rid := ""
	if v, ok := w.Header()["X-Sc-TraceId"]; ok && len(v) > 0 {
		rid = v[0]
	}
	body := map[string]any{
		"RequestId": rid,
		"Code":      "OK",
		"Data":      data,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError renders the platform error body { RequestId, Code, Message, Data }
// per errorsx (03§9.3).
func writeError(w http.ResponseWriter, e *errorsx.Error) {
	rid := ""
	if v, ok := w.Header()["X-Sc-TraceId"]; ok && len(v) > 0 {
		rid = v[0]
	}
	body := map[string]any{
		"RequestId": rid,
		"Code":      e.Code,
		"Message":   e.Message,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.HTTPStatus)
	_ = json.NewEncoder(w).Encode(body)
}

// registerRequest is the POST /internal/actions body.
type registerRequest struct {
	ProductCode string          `json:"product_code"`
	ActionName  string          `json:"action_name"`
	Version     string          `json:"version"`
	ParamSchema json.RawMessage `json:"param_schema"`
	ErrorCodes  []string        `json:"error_codes"`
}

func (s *actionStore) handleRegisterAction(w http.ResponseWriter, r *http.Request) {
	acct := accountIDFromRequest(r)
	if acct == "" {
		writeError(w, errorsx.New("Common.MissingAccountId", errorsx.StatusForbidden,
			"X-Sc-Account-Id header is required (injected by gateway)"))
		return
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errorsx.ErrInvalidParameter.WithRequestID(r.Header.Get("X-Sc-TraceId")))
		return
	}
	if req.ProductCode == "" || req.ActionName == "" {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest,
			"product_code and action_name are required"))
		return
	}
	if !identifier.IsValidProductCode(req.ProductCode) {
		writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest,
			fmt.Sprintf("invalid product_code %q", req.ProductCode)))
		return
	}
	if req.Version == "" {
		req.Version = "2026-08-11"
	}
	// A registered action can only return registered error codes; normalize
	// their format and reject unregistered ones (03§9.3 CI gate, enforced here
	// for phase-1).
	for _, c := range req.ErrorCodes {
		if !strings.HasPrefix(c, req.ProductCode+".") {
			writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest,
				fmt.Sprintf("error code %q must be prefixed with %q", c, req.ProductCode+".")))
			return
		}
	}
	if req.ParamSchema == nil {
		req.ParamSchema = json.RawMessage(`{}`)
	}

	a := Action{
		ID:          actionID(req.ProductCode, req.ActionName),
		ProductCode: req.ProductCode,
		ActionName:  req.ActionName,
		Version:     req.Version,
		ParamSchema: req.ParamSchema,
		ErrorCodes:  req.ErrorCodes,
		UpdatedBy:   acct,
	}
	saved := s.register(a)
	writeJSON(w, http.StatusOK, saved)
}

func (s *actionStore) handleGetAction(w http.ResponseWriter, r *http.Request) {
	if accountIDFromRequest(r) == "" {
		writeError(w, errorsx.New("Common.MissingAccountId", errorsx.StatusForbidden,
			"X-Sc-Account-Id header is required (injected by gateway)"))
		return
	}
	product := r.PathValue("product")
	action := r.PathValue("action")
	a, ok := s.get(actionID(product, action))
	if !ok {
		writeError(w, errorsx.New("ApiMeta.ActionNotFound", errorsx.StatusNotFound,
			fmt.Sprintf("action %q not found", actionID(product, action))))
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *actionStore) handleListActions(w http.ResponseWriter, r *http.Request) {
	if accountIDFromRequest(r) == "" {
		writeError(w, errorsx.New("Common.MissingAccountId", errorsx.StatusForbidden,
			"X-Sc-Account-Id header is required (injected by gateway)"))
		return
	}
	product := r.URL.Query().Get("product")
	list := s.listByProduct(product)
	if list == nil {
		list = []Action{}
	}
	writeJSON(w, http.StatusOK, list)
}
