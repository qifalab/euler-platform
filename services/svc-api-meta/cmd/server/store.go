// Package main: svc-api-meta domain model and in-memory store.
//
// This is the API metadata center (03§4.5.1). There is no pkg-go/api-meta
// package; the domain logic is defined inline here with an in-memory store
// (read-heavy, write-light metadata — the real store is MySQL + MinIO, but an
// in-memory struct keeps phase-1 runnable with no DB dependency, matching the
// _tmpl-go / in-server verify pattern).
//
// An Action is the atomic unit of the OpenAPI surface: every {product_code}
// Action (e.g. euecs:CreateInstance) has a parameter schema (JSON Schema-ish),
// the set of error codes it can return (registered in this center — unregistered
// codes are blocked at CI time, 03§9.3), and a version. This metadata drives
// documentation generation and SDK generation (03§9.4).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/qifalab/euler-platform/errors"
	"github.com/qifalab/euler-platform/identifier"
)

// accountIDHeader is injected by the APISIX gateway on every internal call.
const accountIDHeader = "X-Euler-Account-Id"

// Action is the stored metadata for one OpenAPI Action.
type Action struct {
	ID          string          `json:"id"`           // {product}.{Action}, e.g. euecs.CreateInstance
	ProductCode string          `json:"product_code"` // euecs / euoss / ...
	ActionName  string          `json:"action_name"`  // CreateInstance
	Version     string          `json:"version"`      // e.g. "2026-08-01"
	ParamSchema json.RawMessage `json:"param_schema"` // parameter schema (JSON)
	// ResponseSchema describes the Data object of the success envelope
	// (JSON Schema, like ParamSchema). Optional and additive: omitted when the
	// registrant declared none.
	ResponseSchema json.RawMessage `json:"response_schema,omitempty"`
	ErrorCodes     []string        `json:"error_codes"` // error codes this Action may return
	UpdatedBy      string          `json:"updated_by"`  // account_id that last registered
	UpdatedAt      string          `json:"updated_at"`  // RFC3339
}

// actionStore wires the metadata handlers to the Action repository.
type actionStore struct {
	repo actionRepo
}

func newMetaStore() *actionStore {
	s, err := newMetaStoreFor(newMemActionRepo())
	if err != nil {
		panic(err) // the in-memory repo never fails an upsert
	}
	return s
}

// newMetaStoreFor is main()'s constructor: it wires whichever repo
// newActionRepo picked and upserts the built-in catalogue into it, in both
// modes. main() used to call newMetaStoreWith directly, so the in-memory mode
// came up with an empty catalogue; later the seed was skipped for a persistent
// openapi_meta, which left the Explorer and the OpenAPI reference empty
// whenever EULER_DB_DSN was set — the svc-openapi facade does not register its
// Actions itself, the catalogue is their registration.
//
// The seed is idempotent and safe to run from every replica at every
// startup: it upserts under the natural key (one row per product+Action, see
// sqlActionRepo.Upsert), so a built-in entry replaces a stale row of the same
// Action and never adds a second one, and it never deletes — Actions other
// products registered through POST /internal/actions are left as they are.
func newMetaStoreFor(repo actionRepo) (*actionStore, error) {
	s := newMetaStoreWith(repo)
	if err := s.seedActions(); err != nil {
		return nil, err
	}
	return s, nil
}

// newMetaStoreWith wires a repo without seeding. Tests use it to run the
// handlers against whichever backend they are exercising.
func newMetaStoreWith(repo actionRepo) *actionStore {
	return &actionStore{repo: repo}
}

// seedUpdatedBy is the updated_by of the built-in catalogue's rows.
const seedUpdatedBy = "system-seed"

// seedActions registers the OpenAPI surface the svc-openapi facade serves
// (catalogue.go) so the Explorer and docs describe the real Actions before
// anything registers its own (03§9.4 rule ①: a route is not mounted until its
// metadata is registered here).
func (s *actionStore) seedActions() error {
	for _, a := range facadeCatalogue() {
		a.UpdatedBy = seedUpdatedBy
		if err := s.repo.Upsert(&a); err != nil {
			return fmt.Errorf("register built-in action %s: %w", a.ID, err)
		}
	}
	return nil
}

// id builds the canonical Action id: {product}.{Action} (identifier convention).
func actionID(product, action string) string {
	return product + "." + action
}

// --- domain operations (delegated to the repo; MySQL when EULER_DB_DSN is set) --

func (s *actionStore) register(a Action) (Action, error) {
	if err := s.repo.Upsert(&a); err != nil {
		return Action{}, err
	}
	return a, nil
}

func (s *actionStore) get(id string) (*Action, bool, error) {
	return s.repo.Get(id)
}

func (s *actionStore) listByProduct(product string) ([]*Action, error) {
	return s.repo.List(product)
}

// --- HTTP handlers ------------------------------------------------------------

// accountIDFromRequest extracts the gateway-injected account id, or returns ""
// if the header is missing.
//
// TRUST NOTE: X-Euler-Account-Id is injected by the API gateway after
// authentication; this service relies on network isolation (and optionally
// internalTokenMiddleware / EULER_INTERNAL_TOKEN) rather than re-authenticating.
func accountIDFromRequest(r *http.Request) string {
	return r.Header.Get(accountIDHeader)
}

// requestID reads the trace id the httpmw.RequestID middleware set on the
// response header, so the envelope's RequestId matches X-Euler-TraceId. Get
// canonicalizes the key: Go stores it as "X-Euler-Traceid", so indexing the
// header map with "X-Euler-TraceId" never matched.
func requestID(w http.ResponseWriter) string {
	return w.Header().Get("X-Euler-TraceId")
}

// writeJSON renders a success response with the standard body
// { "RequestId": ..., "Code": "OK", "Data": ... }.
func writeJSON(w http.ResponseWriter, status int, data any) {
	body := map[string]any{
		"RequestId": requestID(w),
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
	body := map[string]any{
		"RequestId": requestID(w),
		"Code":      e.Code,
		"Message":   e.Message,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.HTTPStatus)
	_ = json.NewEncoder(w).Encode(body)
}

// registerRequest is the POST /internal/actions body.
type registerRequest struct {
	ProductCode    string          `json:"product_code"`
	ActionName     string          `json:"action_name"`
	Version        string          `json:"version"`
	ParamSchema    json.RawMessage `json:"param_schema"`
	ResponseSchema json.RawMessage `json:"response_schema"`
	ErrorCodes     []string        `json:"error_codes"`
}

// errorCodePattern is the OpenAPI error vocabulary: PascalCase segments joined
// by dots ({Reason}.{Detail}, e.g. InvalidInstanceId.NotFound, QuotaExceeded)
// — what the svc-openapi facade answers with.
var errorCodePattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*(\.[A-Z][A-Za-z0-9]*)*$`)

// validErrorCode accepts an OpenAPI code, or one prefixed with the registering
// product (euecs.InvalidInstanceType, the form earlier registrations used) —
// never another product's prefix.
func validErrorCode(product, code string) bool {
	rest, prefixed := strings.CutPrefix(code, product+".")
	if prefixed {
		code = rest
	}
	return errorCodePattern.MatchString(code)
}

func (s *actionStore) handleRegisterAction(w http.ResponseWriter, r *http.Request) {
	acct := accountIDFromRequest(r)
	if acct == "" {
		writeError(w, errorsx.New("Common.MissingAccountId", errorsx.StatusForbidden,
			"X-Euler-Account-Id header is required (injected by gateway)"))
		return
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errorsx.ErrInvalidParameter)
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
	// A registered action can only return registered error codes; check
	// their format (03§9.3 CI gate, enforced here for phase-1): the OpenAPI
	// vocabulary (InvalidInstanceId.NotFound), or a code prefixed with this
	// product — never another product's.
	for _, c := range req.ErrorCodes {
		if !validErrorCode(req.ProductCode, c) {
			writeError(w, errorsx.New("Common.InvalidParameter", errorsx.StatusBadRequest,
				fmt.Sprintf("error code %q must be an OpenAPI code such as InvalidInstanceId.NotFound (PascalCase segments joined by dots), optionally prefixed with %q",
					c, req.ProductCode+".")))
			return
		}
	}
	if req.ParamSchema == nil {
		req.ParamSchema = json.RawMessage(`{}`)
	}
	if string(req.ResponseSchema) == "null" {
		req.ResponseSchema = nil // explicit null = none declared
	}

	a := Action{
		ID:             actionID(req.ProductCode, req.ActionName),
		ProductCode:    req.ProductCode,
		ActionName:     req.ActionName,
		Version:        req.Version,
		ParamSchema:    req.ParamSchema,
		ResponseSchema: req.ResponseSchema,
		ErrorCodes:     req.ErrorCodes,
		UpdatedBy:      acct,
	}
	saved, err := s.register(a)
	if err != nil {
		writeError(w, errorsx.New("ApiMeta.RegisterFailed", errorsx.StatusInternalError,
			"action metadata persistence failed"))
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *actionStore) handleGetAction(w http.ResponseWriter, r *http.Request) {
	if accountIDFromRequest(r) == "" {
		writeError(w, errorsx.New("Common.MissingAccountId", errorsx.StatusForbidden,
			"X-Euler-Account-Id header is required (injected by gateway)"))
		return
	}
	product := r.PathValue("product")
	action := r.PathValue("action")
	a, ok, err := s.get(actionID(product, action))
	if err != nil {
		writeError(w, errorsx.New("ApiMeta.ReadFailed", errorsx.StatusInternalError,
			"action metadata lookup failed"))
		return
	}
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
			"X-Euler-Account-Id header is required (injected by gateway)"))
		return
	}
	product := r.URL.Query().Get("product")
	list, err := s.listByProduct(product)
	if err != nil {
		writeError(w, errorsx.New("ApiMeta.ReadFailed", errorsx.StatusInternalError,
			"action metadata lookup failed"))
		return
	}
	if list == nil {
		list = []*Action{}
	}
	writeJSON(w, http.StatusOK, list)
}
