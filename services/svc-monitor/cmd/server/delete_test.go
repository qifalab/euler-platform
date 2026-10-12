package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// withRepos runs fn on the in-memory repo and on MySQL (skipped without
// EULER_DB_DSN), so the two backends are held to one contract.
func withRepos(t *testing.T, fn func(t *testing.T, repo ruleRepo)) {
	t.Run("memory", func(t *testing.T) { fn(t, newMemRuleRepo()) })
	t.Run("mysql", func(t *testing.T) { fn(t, newSQLTestRepo(t)) })
}

func TestRepoDeleteReportsMissingRule(t *testing.T) {
	withRepos(t, func(t *testing.T, repo ruleRepo) {
		rule := newRule(100123, "cpu_utilization")
		if err := repo.Create(rule); err != nil {
			t.Fatal(err)
		}
		if err := repo.Delete(100123, rule.RuleID+1000); !errors.Is(err, ErrRuleNotFound) {
			t.Fatalf("unknown id: %v, want ErrRuleNotFound", err)
		}
		if err := repo.Delete(200456, rule.RuleID); !errors.Is(err, ErrRuleNotFound) {
			t.Fatalf("other account: %v, want ErrRuleNotFound", err)
		}
		if err := repo.Delete(100123, rule.RuleID); err != nil {
			t.Fatalf("own rule: %v", err)
		}
		if err := repo.Delete(100123, rule.RuleID); !errors.Is(err, ErrRuleNotFound) {
			t.Fatalf("second delete: %v, want ErrRuleNotFound", err)
		}
	})
}

// Regression: DELETE answered 200 {"deleted":true} for an id that named no
// rule (or another account's), so a console could not tell a stale id from a
// successful delete.
func TestDeleteRuleRouteNotFound(t *testing.T) {
	withRepos(t, func(t *testing.T, repo ruleRepo) {
		store := newRuleStoreWith(repo)
		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/v1/monitor/rules", store.handleCreateRule)
		mux.HandleFunc("GET /api/v1/monitor/rules", store.handleListRules)
		mux.HandleFunc("DELETE /api/v1/monitor/rules/{id}", store.handleDeleteRule)
		h := recoverMiddleware(requestIDMiddleware(mux))
		do := func(acct, method, path, body string) (int, string, json.RawMessage) {
			t.Helper()
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set(accountIDHeader, acct)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			var env struct {
				Code string          `json:"Code"`
				Data json.RawMessage `json:"Data"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("%s %s: decode %q: %v", method, path, rr.Body.String(), err)
			}
			return rr.Code, env.Code, env.Data
		}

		status, code, data := do("100123", http.MethodPost, "/api/v1/monitor/rules",
			`{"productCode":"euecs","resourceType":"instance","metric":"cpu_utilization","threshold":"80"}`)
		if status != 200 || code != "OK" {
			t.Fatalf("create: %d %s", status, code)
		}
		var rule AlertRule
		if err := json.Unmarshal(data, &rule); err != nil {
			t.Fatal(err)
		}
		path := "/api/v1/monitor/rules/" + strconv.FormatInt(rule.RuleID, 10)

		for _, tc := range []struct {
			what, acct, path string
			status           int
			code             string
		}{
			{"other account", "200456", path, 404, "Monitor.RuleNotFound"},
			{"unknown id", "100123", "/api/v1/monitor/rules/987654321", 404, "Monitor.RuleNotFound"},
			{"malformed id", "100123", "/api/v1/monitor/rules/abc", 400, "Common.InvalidParameter"},
			{"own rule", "100123", path, 200, "OK"},
			{"already deleted", "100123", path, 404, "Monitor.RuleNotFound"},
		} {
			if status, code, _ := do(tc.acct, http.MethodDelete, tc.path, ""); status != tc.status || code != tc.code {
				t.Fatalf("%s: %d %s, want %d %s", tc.what, status, code, tc.status, tc.code)
			}
			if tc.what == "other account" {
				// The refused delete left the owner's rule in place.
				if got, err := repo.Get(100123, rule.RuleID); err != nil || got == nil {
					t.Fatalf("rule gone after another account's delete: %+v %v", got, err)
				}
			}
		}
		if got, err := repo.Get(100123, rule.RuleID); err != nil || got != nil {
			t.Fatalf("deleted rule still readable: %+v %v", got, err)
		}
	})
}
