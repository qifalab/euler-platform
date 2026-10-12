package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMainPathOrgService builds the service the way main() does, persistence off.
func newMainPathOrgService(t *testing.T) *orgService {
	t.Helper()
	t.Setenv("EULER_DB_DSN", "")
	s, err := newOrgService(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func orgCall(t *testing.T, h http.HandlerFunc, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(accountHeader, "100123")
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func createProjectViaAPI(t *testing.T, s *orgService, body string) string {
	t.Helper()
	rec, out := orgCall(t, s.handleProjects, http.MethodPost, "/internal/projects", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create %s: %d %s", body, rec.Code, rec.Body.String())
	}
	return out["project_id"].(string)
}

// Regression: every CreateProject error was a 409 Project.AlreadyExists (a bad
// parent included), and a rename clash on PATCH was a 500.
func TestProjectHandlersMapStoreErrors(t *testing.T) {
	s := newMainPathOrgService(t)
	a := createProjectViaAPI(t, s, `{"name":"a","project_id":"client-chosen"}`)
	if a == "client-chosen" {
		t.Fatal("the handler kept the client-supplied project_id")
	}
	b := createProjectViaAPI(t, s, `{"name":"b","parent_id":"`+a+`"}`)

	cases := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"duplicate name", http.MethodPost, "/internal/projects", `{"name":"a"}`, http.StatusConflict, "Project.AlreadyExists"},
		{"missing parent", http.MethodPost, "/internal/projects", `{"name":"c","parent_id":"nope"}`, http.StatusBadRequest, "Project.InvalidParent"},
		{"rename clash", http.MethodPatch, "/internal/projects/" + b, `{"name":"a"}`, http.StatusConflict, "Project.AlreadyExists"},
		{"self parent", http.MethodPatch, "/internal/projects/" + a, `{"parent_id":"` + a + `"}`, http.StatusBadRequest, "Project.InvalidParent"},
		{"cycle", http.MethodPatch, "/internal/projects/" + a, `{"parent_id":"` + b + `"}`, http.StatusBadRequest, "Project.InvalidParent"},
		{"unknown project", http.MethodPatch, "/internal/projects/nope", `{"name":"z"}`, http.StatusNotFound, "Project.NotFound"},
	}
	for _, c := range cases {
		h := s.handleProjects
		if c.method == http.MethodPatch {
			h = s.handleProjectByID
		}
		rec, out := orgCall(t, h, c.method, c.path, c.body)
		if rec.Code != c.status || out["Code"] != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, rec.Code, rec.Body.String(), c.status, c.code)
		}
	}
}

// brokenStore stands in for a database failure on every project write.
type brokenStore struct{ *memStore }

var errDriver = errors.New("Error 1146 (42S02): Table 'account_db.org_project' doesn't exist")

func (brokenStore) CreateProject(*project) error { return errDriver }
func (brokenStore) UpdateProject(string, string, *string, *string) error {
	return errDriver
}
func (brokenStore) MoveResource(string, string, string, string) error { return errDriver }
func (brokenStore) PutTag(tag) error                                  { return errDriver }
func (brokenStore) ListTags(string) ([]tag, error)                    { return nil, errDriver }
func (brokenStore) DeleteTag(string, string) error                    { return errDriver }

// A storage failure is a 500 with a generic message, not a 409 (create) or a
// 409 Resource.MoveConflict (move) carrying the driver's text.
func TestOrgHandlersStorageFailureIs500Generic(t *testing.T) {
	s := &orgService{store: brokenStore{newMemStore()}}
	for _, c := range []struct {
		h            http.HandlerFunc
		method, path string
		body         string
	}{
		{s.handleProjects, http.MethodPost, "/internal/projects", `{"name":"a"}`},
		{s.handleProjectByID, http.MethodPatch, "/internal/projects/1", `{"name":"a"}`},
		{s.handleResourceMove, http.MethodPost, "/internal/resources/move", `{"resource_id":"r","from_project_id":"1","to_project_id":"2"}`},
		// B6: the tag handlers returned the driver's text on 500.
		{s.handleTags, http.MethodPost, "/internal/tags", `{"key":"env","value":"prod"}`},
		{s.handleTags, http.MethodGet, "/internal/tags", ``},
		{s.handleTags, http.MethodDelete, "/internal/tags?key=env", ``},
	} {
		rec, out := orgCall(t, c.h, c.method, c.path, c.body)
		if rec.Code != http.StatusInternalServerError || out["Code"] != "Common.InternalError" {
			t.Errorf("%s %s: %d %s, want 500 Common.InternalError", c.method, c.path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "account_db") {
			t.Errorf("%s %s leaked the driver error: %s", c.method, c.path, rec.Body.String())
		}
	}
}

func TestResourceMoveHandlerErrors(t *testing.T) {
	s := newMainPathOrgService(t)
	a := createProjectViaAPI(t, s, `{"name":"a"}`)
	b := createProjectViaAPI(t, s, `{"name":"b"}`)
	move := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		return orgCall(t, s.handleResourceMove, http.MethodPost, "/internal/resources/move", body)
	}
	if rec, _ := move(`{"resource_id":"r1","from_project_id":"` + a + `","to_project_id":"` + b + `"}`); rec.Code != http.StatusOK {
		t.Fatalf("first move: %d %s", rec.Code, rec.Body.String())
	}
	if rec, out := move(`{"resource_id":"r1","from_project_id":"` + a + `","to_project_id":"` + b + `"}`); rec.Code != http.StatusConflict || out["Code"] != "Resource.MoveConflict" {
		t.Fatalf("stale from-project: %d %s, want 409 Resource.MoveConflict", rec.Code, rec.Body.String())
	}
	if rec, out := move(`{"resource_id":"r1","from_project_id":"nope","to_project_id":"` + b + `"}`); rec.Code != http.StatusNotFound || out["Code"] != "Project.NotFound" {
		t.Fatalf("unknown project: %d %s, want 404 Project.NotFound", rec.Code, rec.Body.String())
	}
}

// The tag sentinel keeps its own status through the shared mapping.
func TestTagHandlerNotFoundIs404(t *testing.T) {
	s := newMainPathOrgService(t)
	rec, out := orgCall(t, s.handleTags, http.MethodDelete, "/internal/tags?key=nope", ``)
	if rec.Code != http.StatusNotFound || out["Code"] != "Tag.NotFound" {
		t.Fatalf("delete of a missing tag: %d %s, want 404 Tag.NotFound", rec.Code, rec.Body.String())
	}
}
