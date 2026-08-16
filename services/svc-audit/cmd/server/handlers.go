// Package main: svc-audit domain handlers and the in-memory audit store.
//
// The store keeps one audit.Chain per account (07§6.2 tenant-dimension hash
// chain) plus the list of recorded events, so the /verify endpoint can run
// audit.Verify over the persisted trail exactly as the production
// ClickHouse-backed sink will. Appends go through audit.Chain.Append, which
// redacts sensitive params and masks the access key at write time (07§6.1).
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/starcloud/sc-platform/audit"
)

// accountHeader is injected by the APISIX gateway for authenticated calls
// (04§3.1). It is the tenant attribution for every audit event.
const accountHeader = "X-Sc-Account-Id"

// auditStore is an in-memory implementation of the svc-audit persistence
// contract: per-account chain + event trail.
type auditStore struct {
	mu     sync.Mutex
	chains map[int64]*audit.Chain
	events map[int64][]audit.Event
}

func newAuditStore() *auditStore {
	return &auditStore{
		chains: make(map[int64]*audit.Chain),
		events: make(map[int64][]audit.Event),
	}
}

func (s *auditStore) chainFor(accountID int64) *audit.Chain {
	c, ok := s.chains[accountID]
	if !ok {
		c = audit.NewChain(accountID)
		s.chains[accountID] = c
	}
	return c
}

// handleAppend records one audit event onto the caller's chain. The body
// carries the event fields; account attribution comes from the gateway
// header so a caller cannot self-report a different tenant.
//
//	POST /internal/audit
func (s *auditStore) handleAppend(w http.ResponseWriter, r *http.Request) {
	accountID, ok := requireAccount(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body appendRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid JSON body: "+err.Error())
		return
	}

	now := time.Now().UTC()
	seq := s.chainFor(accountID).Sequence() + 1
	ev := audit.Event{
		EventID:        audit.EventID(now, seq),
		EventTime:      now,
		EventSource:    body.EventSource,
		EventName:      body.EventName,
		SourceIP:       body.SourceIP,
		UserAgent:      r.UserAgent(),
		Identity: audit.Identity{
			Type:       body.IdentityType,
			AccountID:  accountID,
			Principal:  body.Principal,
			AKID:       body.AKID,
			MFAPresent: body.MFAPresent,
		},
		Resources:      body.Resources,
		Decision:       body.Decision,
		DecisionNumber: body.DecisionNumber,
		RequestParams:  body.RequestParams,
		ResponseCode:   body.ResponseCode,
		TraceID:        r.Header.Get("X-Sc-TraceId"),
	}

	s.mu.Lock()
	recorded, err := s.chainFor(accountID).Append(ev)
	if err == nil {
		s.events[accountID] = append(s.events[accountID], recorded)
	}
	s.mu.Unlock()

	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, recorded)
}

// handleList returns all recorded events for an account, oldest first.
//
//	GET /internal/audit/{account}/events
func (s *auditStore) handleList(w http.ResponseWriter, r *http.Request) {
	accountID, err := accountFromPath(w, r)
	if err != nil {
		return
	}

	s.mu.Lock()
	events := append([]audit.Event(nil), s.events[accountID]...)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, events)
}

// handleVerify recomputes the account's chain and reports whether it is
// intact, mirroring the check an auditor runs.
//
//	GET /internal/audit/{account}/verify
func (s *auditStore) handleVerify(w http.ResponseWriter, r *http.Request) {
	accountID, err := accountFromPath(w, r)
	if err != nil {
		return
	}

	s.mu.Lock()
	events := append([]audit.Event(nil), s.events[accountID]...)
	s.mu.Unlock()

	res := audit.Verify(events)
	if res.AccountID == 0 {
		res.AccountID = accountID
	}
	writeJSON(w, http.StatusOK, res)
}

// --- request helpers -------------------------------------------------------

// requireAccount pulls account_id from the gateway-injected header and
// returns 403 when it is absent or malformed.
func requireAccount(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := parseAccountID(r.Header.Get(accountHeader))
	if err != nil {
		writeError(w, http.StatusForbidden, "AccessDenied", "missing or invalid "+accountHeader)
		return 0, false
	}
	return id, true
}

func accountFromPath(w http.ResponseWriter, r *http.Request) (int64, error) {
	raw := r.PathValue("account")
	id, err := parseAccountID(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "invalid account in path")
		return 0, err
	}
	return id, nil
}

func parseAccountID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid account id %q", raw)
	}
	return id, nil
}

// writeJSON renders a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{
		"Code":    code,
		"Message": message,
	})
}

// appendRequest is the JSON body of POST /internal/audit. Account
// attribution is intentionally absent — it comes from the gateway header.
type appendRequest struct {
	EventSource    string            `json:"event_source"`
	EventName      string            `json:"event_name"`
	SourceIP       string            `json:"source_ip"`
	IdentityType   string            `json:"identity_type"`
	Principal      string            `json:"principal"`
	AKID           string            `json:"ak_id"`
	MFAPresent     bool              `json:"mfa_present"`
	Resources      []string          `json:"resources"`
	Decision       audit.Decision    `json:"decision"`
	DecisionNumber string            `json:"decision_number"`
	RequestParams  map[string]string `json:"request_params"`
	ResponseCode   int               `json:"response_code"`
}
