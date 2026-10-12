package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/storage/sqltest"
)

// Regression: writeJSON/writeError looked the trace id up as
// w.Header()["X-Euler-TraceId"], but Go stores the header canonicalized
// ("X-Euler-Traceid"), so every envelope carried RequestId "".
func TestEnvelopeRequestIDMatchesTraceHeader(t *testing.T) {
	s := newMetaStore()
	cases := []struct {
		name   string
		h      http.HandlerFunc
		method string
		target string
		body   string
		status int
	}{
		{"success", s.handleListActions, http.MethodGet, "/internal/actions", "", http.StatusOK},
		{"not found", s.handleGetAction, http.MethodGet, "/internal/actions/euecs/Nope", "", http.StatusNotFound},
		{"bad body", s.handleRegisterAction, http.MethodPost, "/internal/actions", "{", http.StatusBadRequest},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
		req.Header.Set(accountIDHeader, "100123")
		rec := httptest.NewRecorder()
		httpmw.RequestID(c.h).ServeHTTP(rec, req)
		if rec.Code != c.status {
			t.Fatalf("%s: status = %d, want %d (%s)", c.name, rec.Code, c.status, rec.Body.String())
		}
		var env struct {
			RequestId string `json:"RequestId"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		trace := rec.Header().Get("X-Euler-TraceId")
		if trace == "" || env.RequestId != trace {
			t.Errorf("%s: envelope RequestId = %q, X-Euler-TraceId = %q; want equal and non-empty", c.name, env.RequestId, trace)
		}
	}
}

// Regression: main() built the store with newMetaStoreWith(repo), which never
// seeds, so the in-memory mode served an empty Action catalogue (the Explorer
// dropdown and docs had nothing). This goes through main's constructor path.
func TestMainConstructorSeedsInMemoryRepo(t *testing.T) {
	t.Setenv("EULER_DB_DSN", "")
	repo, err := newActionRepo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := newMetaStoreFor(repo)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.listByProduct("euecs")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("in-memory mode started with no seeded Actions")
	}
}

// Regression: the seed was skipped for a persistent openapi_meta, so with
// EULER_DB_DSN the Explorer and the OpenAPI reference had no Actions. Every
// startup now upserts the built-in catalogue: a restart keeps one row per
// Action, a stale row of a built-in Action is corrected, and an Action another
// product registered through /internal/actions survives.
func TestMainConstructorSeedsPersistentRepo(t *testing.T) {
	db := sqltest.Open(t, sqltest.Services("svc-api-meta"))
	ctx := context.Background()
	builtin := facadeCatalogue()

	s, err := newMetaStoreFor(newSQLActionRepo(ctx, db))
	if err != nil {
		t.Fatal(err)
	}
	if list, err := s.listByProduct(""); err != nil || len(list) != len(builtin) {
		t.Fatalf("persistent repo holds %d Actions after the first startup (err %v), want %d", len(list), err, len(builtin))
	}

	// Another product registers its own Action through the internal API...
	body := `{"product_code":"eueci","action_name":"DescribeContainerGroups","version":"2026-09-01",` +
		`"param_schema":{"type":"object"},"error_codes":["InvalidContainerGroupId.NotFound"]}`
	req := httptest.NewRequest(http.MethodPost, "/internal/actions", strings.NewReader(body))
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleRegisterAction(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	// ...and a built-in Action is re-registered with stale metadata.
	stale := builtin[0]
	stale.ErrorCodes, stale.ResponseSchema, stale.UpdatedBy = []string{"MissingParameter"}, nil, "100123"
	if _, err := s.register(stale); err != nil {
		t.Fatal(err)
	}

	// Restart (twice: every replica runs the seed).
	for i := 0; i < 2; i++ {
		if s, err = newMetaStoreFor(newSQLActionRepo(ctx, db)); err != nil {
			t.Fatalf("restart %d: %v", i+1, err)
		}
	}
	var rows, actions int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT product_code, action_name) FROM api_action`).Scan(&rows, &actions); err != nil {
		t.Fatal(err)
	}
	if rows != len(builtin)+1 || actions != rows {
		t.Fatalf("after restarts: %d rows for %d Actions, want one row per Action (%d)", rows, actions, len(builtin)+1)
	}
	got, ok, err := s.get(stale.ID)
	if err != nil || !ok {
		t.Fatalf("%s: ok=%v err=%v", stale.ID, ok, err)
	}
	if strings.Join(got.ErrorCodes, ",") != strings.Join(builtin[0].ErrorCodes, ",") || got.ResponseSchema == nil ||
		got.UpdatedBy != seedUpdatedBy || got.Version != catalogueVersion {
		t.Fatalf("stale built-in row was not corrected: %+v", got)
	}
	extra, ok, err := s.get("eueci.DescribeContainerGroups")
	if err != nil || !ok {
		t.Fatalf("registered Action lost on restart: ok=%v err=%v", ok, err)
	}
	if extra.UpdatedBy != "100123" || extra.Version != "2026-09-01" ||
		strings.Join(extra.ErrorCodes, ",") != "InvalidContainerGroupId.NotFound" {
		t.Fatalf("registered Action changed on restart: %+v", extra)
	}
}

// Regression: the backend registry lacked seven sub-apps the console shell's
// fallback lists; the shell prefers the backend registry, so those products
// were unreachable whenever svc-api-meta answered.
func TestRegistryCoversConsoleFallbackApps(t *testing.T) {
	want := map[string]struct {
		rule, product, entry string
	}{
		"console-eci":         {"/eueci", "eueci", "http://localhost:5183/"},
		"console-lb":          {"/eulb", "eulb", "http://localhost:5184/"},
		"console-autoscaling": {"/euas", "euas", "http://localhost:5185/"},
		"console-backup":      {"/eubackup", "eubackup", "http://localhost:5186/"},
		"console-redis":       {"/euredis", "euredis", "http://localhost:5187/"},
		"console-kafka":       {"/eukafka", "eukafka", "http://localhost:5188/"},
		"console-logservice":  {"/eulog", "eulog", "http://localhost:5189/"},
	}

	rec := httptest.NewRecorder()
	newRegistryStore().handleConsoleApps(rec, httptest.NewRequest(http.MethodGet, "/api/v1/meta/console/apps", nil))
	var env struct {
		Data Registry `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}

	rules := map[string]string{}
	got := map[string]RegistryApp{}
	for _, a := range env.Data.Apps {
		got[a.AppCode] = a
		for _, r := range a.ActiveRules {
			if prev, dup := rules[r]; dup {
				t.Errorf("activeRule %s claimed by both %s and %s", r, prev, a.AppCode)
			}
			rules[r] = a.AppCode
		}
	}
	for code, w := range want {
		a, ok := got[code]
		if !ok {
			t.Errorf("registry is missing %s", code)
			continue
		}
		if len(a.ActiveRules) != 1 || a.ActiveRules[0] != w.rule ||
			len(a.ProductCodes) != 1 || a.ProductCodes[0] != w.product ||
			a.EntryURL != w.entry || a.Status != "online" {
			t.Errorf("%s = %+v, want rule %s product %s entry %s", code, a, w.rule, w.product, w.entry)
		}
	}
}
