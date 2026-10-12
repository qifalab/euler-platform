package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/storage"
)

func ruleRoutes(store *ruleStore) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/monitor/rules", store.handleListRules)
	mux.HandleFunc("POST /api/v1/monitor/rules", store.handleCreateRule)
	mux.HandleFunc("PUT /api/v1/monitor/rules/{id}", store.handleUpdateRule)
	return recoverMiddleware(requestIDMiddleware(mux))
}

func doRule(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(accountIDHeader, "100123")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// E23: create accepted any threshold string and negative period/evalPeriods,
// which the update path rejects.
func TestCreateRuleValidatesFields(t *testing.T) {
	h := ruleRoutes(newRuleStore())
	for name, body := range map[string]string{
		"non-numeric threshold": `{"productCode":"euecs","metric":"m1","threshold":"eighty"}`,
		"NaN threshold":         `{"productCode":"euecs","metric":"m2","threshold":"NaN"}`,
		"too many decimals":     `{"productCode":"euecs","metric":"m3","threshold":"1.23456"}`,
		"negative period":       `{"productCode":"euecs","metric":"m4","threshold":"80","period":-60}`,
		"negative evalPeriods":  `{"productCode":"euecs","metric":"m5","threshold":"80","evalPeriods":-1}`,
	} {
		if rr := doRule(t, h, http.MethodPost, "/api/v1/monitor/rules", body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rr.Code, rr.Body.String())
		}
	}
	rr := doRule(t, h, http.MethodPost, "/api/v1/monitor/rules", `{"productCode":"euecs","metric":"ok","threshold":"80"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid create: %d %s", rr.Code, rr.Body.String())
	}
	var env struct{ Data AlertRule }
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if env.Data.Period != 60 || env.Data.EvalPeriods != 1 {
		t.Fatalf("zero period/evalPeriods still default: %+v", env.Data)
	}
	path := "/api/v1/monitor/rules/" + strconv.FormatInt(env.Data.RuleID, 10)
	if rr := doRule(t, h, http.MethodPut, path, `{"threshold":"high","version":1}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("update with a non-numeric threshold: %d, want 400", rr.Code)
	}
}

func TestNormalizeThreshold(t *testing.T) {
	for in, want := range map[string]string{
		"80": "80.0000", "80.5": "80.5000", "0080.25": "80.2500", ".5": "0.5000",
		"-3": "-3.0000", "+7.1": "7.1000", "-0": "0.0000", "1000.0000": "1000.0000",
	} {
		if got, err := normalizeThreshold(in); err != nil || got != want {
			t.Errorf("normalizeThreshold(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".", "-", "1e3", "Inf", "0x10", "1.00001", "123456789012345"} {
		if got, err := normalizeThreshold(in); err == nil {
			t.Errorf("normalizeThreshold(%q) = %q, want an error", in, got)
		}
	}
}

// E24: main() built the store with newRuleStoreWith, so the in-memory mode
// never got its demo rules. Go through main()'s constructor path.
func TestMainConstructorSeedsInMemoryMode(t *testing.T) {
	t.Setenv(storage.DSNEnv, "")
	repo, err := newRuleRepo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := newRuleStoreFor(repo)
	rules, err := s.repo.List(100123)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("in-memory mode has %d rules, want the 3 demo rules", len(rules))
	}
}

// E24: the in-memory repo let two rules share a natural key; the SQL repo's
// uk_acc_rule refuses that.
func TestMemRuleRepoEnforcesNaturalKey(t *testing.T) {
	repo := newMemRuleRepo()
	if err := repo.Create(newRule(100123, "cpu")); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(newRule(100123, "cpu")); !errors.Is(err, ErrRuleConflict) {
		t.Fatalf("duplicate natural key: err = %v, want ErrRuleConflict", err)
	}
	if err := repo.Create(newRule(100999, "cpu")); err != nil {
		t.Fatalf("same metric for another account must be allowed: %v", err)
	}
}

// E24: a threshold created as "80" read back as "80" from memory but
// "80.0000" from MySQL. Both backends now return the canonical DECIMAL form,
// in the create response and on every later read.
func TestThresholdRoundTripsIdenticallyInBothBackends(t *testing.T) {
	run := func(repo ruleRepo) (created, listed string) {
		t.Helper()
		h := ruleRoutes(newRuleStoreWith(repo))
		rr := doRule(t, h, http.MethodPost, "/api/v1/monitor/rules", `{"productCode":"euecs","resourceType":"instance","metric":"disk","threshold":"80"}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
		}
		var one struct{ Data AlertRule }
		_ = json.Unmarshal(rr.Body.Bytes(), &one)
		rr = doRule(t, h, http.MethodGet, "/api/v1/monitor/rules", "")
		var all struct{ Data []AlertRule }
		_ = json.Unmarshal(rr.Body.Bytes(), &all)
		if len(all.Data) != 1 {
			t.Fatalf("list: %s", rr.Body.String())
		}
		return one.Data.Threshold, all.Data[0].Threshold
	}
	check := func(name string, repo ruleRepo) {
		t.Helper()
		created, listed := run(repo)
		if created != "80.0000" || listed != "80.0000" {
			t.Errorf("%s: created %q, listed %q, want 80.0000 both", name, created, listed)
		}
	}
	check("memory", newMemRuleRepo())
	check("mysql", newSQLTestRepo(t)) // skips without EULER_DB_DSN
}
