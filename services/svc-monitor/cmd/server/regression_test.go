package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// rulesTestServer wires the rule routes (main()'s mux) so the PUT path can be
// exercised over httptest.
func rulesTestServer() http.Handler {
	store := newRuleStore()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/monitor/rules", store.handleListRules)
	mux.HandleFunc("POST /api/v1/monitor/rules", store.handleCreateRule)
	mux.HandleFunc("PUT /api/v1/monitor/rules/{id}", store.handleUpdateRule)
	mux.HandleFunc("DELETE /api/v1/monitor/rules/{id}", store.handleDeleteRule)
	return recoverMiddleware(requestIDMiddleware(mux))
}

// The update path the service documents must exist, and it must be guarded by
// the rule's version: a console tab editing a stale copy gets 409 instead of
// silently overwriting a newer edit. Before this the route was documented but
// never registered, so a rule could not be updated at all.
func TestUpdateRuleRouteAndVersionGuard(t *testing.T) {
	h := rulesTestServer()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(accountIDHeader, "100123")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	created := do(http.MethodPost, "/api/v1/monitor/rules",
		`{"productCode":"scecs","resourceType":"instance","metric":"cpu_util","threshold":"80"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var env struct {
		Data AlertRule `json:"Data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, created.Body.String())
	}
	rule := env.Data
	path := "/api/v1/monitor/rules/" + strconv.FormatInt(rule.RuleID, 10)

	// A stale version is refused (409) and changes nothing.
	stale := do(http.MethodPut, path, `{"threshold":"95","version":`+strconv.Itoa(rule.Version+7)+`}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale update: %d %s, want 409", stale.Code, stale.Body.String())
	}
	// The current version updates, and bumps the version.
	ok := do(http.MethodPut, path, `{"threshold":"95","status":2,"version":`+strconv.Itoa(rule.Version)+`}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("update: %d %s", ok.Code, ok.Body.String())
	}
	var updated struct {
		Data AlertRule `json:"Data"`
	}
	if err := json.Unmarshal(ok.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated: %v", err)
	}
	if updated.Data.Threshold != "95" || updated.Data.Status != 2 {
		t.Fatalf("update not applied: %+v", updated.Data)
	}
	if updated.Data.Version != rule.Version+1 {
		t.Fatalf("version = %d, want %d", updated.Data.Version, rule.Version+1)
	}
	// An empty body is a client error, not a silent no-op.
	if empty := do(http.MethodPut, path, `{"version":`+strconv.Itoa(updated.Data.Version)+`}`); empty.Code != http.StatusBadRequest {
		t.Fatalf("empty update: %d, want 400", empty.Code)
	}
}
