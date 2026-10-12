package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/workflow"
)

// registerCountingFlow installs a one-step flow under defKey whose forward
// action counts its runs, and removes it when the test ends.
func registerCountingFlow(t *testing.T, defKey string) *atomic.Int64 {
	t.Helper()
	var runs atomic.Int64
	registeredFlows[defKey] = []workflow.Step{{
		Name: "occupy_quota",
		Do:   func(c *workflow.Context) error { runs.Add(1); return nil },
		Undo: func(c *workflow.Context) error { return nil },
	}}
	t.Cleanup(func() { delete(registeredFlows, defKey) })
	return &runs
}

// newMainPathApp builds the app the way main() does, with persistence off.
func newMainPathApp(t *testing.T) *app {
	t.Helper()
	t.Setenv("EULER_DB_DSN", "")
	a, err := newApp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func doStart(t *testing.T, a *app, account, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/flows/start", strings.NewReader(body))
	req.Header.Set("X-Euler-Account-Id", account)
	rec := httptest.NewRecorder()
	a.handleStart(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// Regression: a retried start ran the whole saga again (the in-memory store had
// no uniqueness at all) and every StartFlow error was a 400 Workflow.UnknownDef.
func TestStartFlowDuplicateBizKeyIs409WithoutRunning(t *testing.T) {
	a := newMainPathApp(t)
	runs := registerCountingFlow(t, "test_count")

	rec, _ := doStart(t, a, "100123", `{"def_key":"test_count","biz_key":"ord-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first start: %d %s", rec.Code, rec.Body.String())
	}
	rec, out := doStart(t, a, "100123", `{"def_key":"test_count","biz_key":"ord-1"}`)
	if rec.Code != http.StatusConflict || out["Code"] != "Workflow.AlreadyStarted" {
		t.Fatalf("retried start: %d %s, want 409 Workflow.AlreadyStarted", rec.Code, rec.Body.String())
	}
	if runs.Load() != 1 {
		t.Fatalf("the saga ran %d times, want 1", runs.Load())
	}
}

func TestStartFlowUnknownDefIs400(t *testing.T) {
	a := newMainPathApp(t)
	rec, out := doStart(t, a, "100123", `{"def_key":"no_such_flow","biz_key":"ord-1"}`)
	if rec.Code != http.StatusBadRequest || out["Code"] != "Workflow.UnknownDef" {
		t.Fatalf("unknown def: %d %s", rec.Code, rec.Body.String())
	}
}

func TestStartFlowRejectsOverlongBizKey(t *testing.T) {
	a := newMainPathApp(t)
	registerCountingFlow(t, "test_count")
	rec, _ := doStart(t, a, "100123", `{"def_key":"test_count","biz_key":"`+strings.Repeat("k", 65)+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("65-char biz_key: %d %s, want 400", rec.Code, rec.Body.String())
	}
}

func TestStartFlowKeylessStartsGetDistinctKeys(t *testing.T) {
	a := newMainPathApp(t)
	registerCountingFlow(t, "test_count")
	_, first := doStart(t, a, "100123", `{"def_key":"test_count"}`)
	rec, second := doStart(t, a, "100123", `{"def_key":"test_count"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second keyless start: %d %s", rec.Code, rec.Body.String())
	}
	if first["biz_key"] == "" || first["biz_key"] == second["biz_key"] {
		t.Fatalf("keyless starts returned biz keys %v and %v", first["biz_key"], second["biz_key"])
	}
}

// failingStore stands in for a database that is down.
type failingStore struct{}

func (failingStore) reserve(flowRecord) error {
	return errors.New("dial tcp 10.0.3.7:3306: connect: connection refused")
}
func (failingStore) finish(flowRecord) error             { return nil }
func (failingStore) release(int64) error                 { return nil }
func (failingStore) get(int64) (flowRecord, bool, error) { return flowRecord{}, false, nil }

// A storage failure is a 500 with a generic message — not a 400 that echoes
// the driver's text (internal addresses included) to the caller.
func TestStartFlowStoreFailureIs500Generic(t *testing.T) {
	a := &app{engine: workflow.NewEngine(time.Now), store: failingStore{}, nextID: func() int64 { return 1 }}
	runs := registerCountingFlow(t, "test_count")
	rec, out := doStart(t, a, "100123", `{"def_key":"test_count","biz_key":"ord-1"}`)
	if rec.Code != http.StatusInternalServerError || out["Code"] != "Workflow.StartFailed" {
		t.Fatalf("store failure: %d %s, want 500 Workflow.StartFailed", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "10.0.3.7") {
		t.Fatalf("response leaked the driver error: %s", rec.Body.String())
	}
	if runs.Load() != 0 {
		t.Fatal("the saga ran although its instance could not be recorded")
	}
}

// Regression: another account's flow answered 403 while a missing id answered
// 404, so a caller could enumerate which flow ids exist.
func TestGetFlowOfAnotherAccountIs404(t *testing.T) {
	a := newMainPathApp(t)
	registerCountingFlow(t, "test_count")
	_, out := doStart(t, a, "100123", `{"def_key":"test_count","biz_key":"ord-1"}`)
	id := int64(out["id"].(float64))

	get := func(account, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Euler-Account-Id", account)
		rec := httptest.NewRecorder()
		a.handleGet(rec, req)
		return rec
	}
	if rec := get("100123", "/internal/flows/"+jsonNum(id)); rec.Code != http.StatusOK {
		t.Fatalf("owner get: %d %s", rec.Code, rec.Body.String())
	}
	other := get("999", "/internal/flows/"+jsonNum(id))
	missing := get("999", "/internal/flows/987654321")
	if other.Code != http.StatusNotFound || other.Body.String() != missing.Body.String() {
		t.Fatalf("another account's flow: %d %s; missing flow: %d %s — want identical 404s",
			other.Code, other.Body.String(), missing.Code, missing.Body.String())
	}
}

func jsonNum(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// finishFailingStore loses the first n outcome writes (a database blip after
// the saga ran).
type finishFailingStore struct {
	flowStore
	n int
}

func (s *finishFailingStore) finish(r flowRecord) error {
	if s.n > 0 {
		s.n--
		return errors.New("Error 2013: Lost connection to MySQL server during query")
	}
	return s.flowStore.finish(r)
}

// assertFinishFailureReleases: a failed finish used to leave the instance
// RUNNING, so every retry over the business key was a 409 forever. The retry
// now starts the flow again.
func assertFinishFailureReleases(t *testing.T, a *app) {
	t.Helper()
	runs := registerCountingFlow(t, "test_count")
	a.store = &finishFailingStore{flowStore: a.store, n: 1}

	rec, _ := doStart(t, a, "100123", `{"def_key":"test_count","biz_key":"ord-finish"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("start whose outcome was lost: %d %s, want 500", rec.Code, rec.Body.String())
	}
	rec, out := doStart(t, a, "100123", `{"def_key":"test_count","biz_key":"ord-finish"}`)
	if rec.Code != http.StatusOK || out["status"] != string(workflow.FlowCompleted) {
		t.Fatalf("retry: %d %s, want 200 COMPLETED (not 409 against a stuck RUNNING row)", rec.Code, rec.Body.String())
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("saga ran %d times, want 2", n)
	}
	// The finished retry keeps its key: a third start is refused.
	if rec, _ := doStart(t, a, "100123", `{"def_key":"test_count","biz_key":"ord-finish"}`); rec.Code != http.StatusConflict {
		t.Fatalf("start after a recorded outcome: %d, want 409", rec.Code)
	}
}

func TestStartFlowFinishFailureReleasesReservation(t *testing.T) {
	assertFinishFailureReleases(t, newMainPathApp(t))
}

func TestSQLStartFlowFinishFailureReleasesReservation(t *testing.T) {
	assertFinishFailureReleases(t, newSQLTestApp(t))
}

// release never drops an instance that has its outcome.
func TestReleaseKeepsFinishedFlow(t *testing.T) {
	for name, a := range map[string]func(*testing.T) *app{
		"memory": newMainPathApp,
		"mysql":  newSQLTestApp,
	} {
		t.Run(name, func(t *testing.T) {
			a := a(t)
			registerCountingFlow(t, "test_count")
			id, _, err := a.StartFlow(100123, "test_count", "ord-kept")
			if err != nil {
				t.Fatal(err)
			}
			if err := a.store.release(id); err != nil {
				t.Fatal(err)
			}
			if rec, ok, err := a.store.get(id); err != nil || !ok || rec.Status != workflow.FlowCompleted {
				t.Fatalf("finished flow after release = %+v %v %v", rec, ok, err)
			}
			if _, _, err := a.StartFlow(100123, "test_count", "ord-kept"); !errors.Is(err, errFlowExists) {
				t.Fatalf("restart over a finished flow = %v, want errFlowExists", err)
			}
		})
	}
}
