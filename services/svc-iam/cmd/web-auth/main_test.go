package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// These tests cover the M-7.6 RAM endpoints: roles list, policies list, and the
// 策略模拟器 (simulate). They use the Bearer access_token path the rest of
// web-auth uses — the seed account (100123) gets a real HS256 JWT, and the
// handlers authenticate via requireAuth. The simulator's Deny-first semantics
// are the real pkg-go/authz engine's, exercised end-to-end over HTTP.

// TestMain seeds the account + RAM roles/policies once for the whole package,
// matching main()'s seed. Tests share this process-global state (the handlers
// are in-memory and stdlib-HTTP, same as the rest of web-auth's tests).
func TestMain(m *testing.M) {
	seedAccount(100123, "admin@starcloud.cn", "种子管理员", "starcloud123")
	seedRAMRolesPolicies(100123)
	os.Exit(m.Run())
}

func newTestAuthMux(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ram/roles", handleListRAMRoles)
	mux.HandleFunc("POST /api/ram/roles", handleCreateRAMRole)
	mux.HandleFunc("GET /api/ram/policies", handleListRAMPolicies)
	mux.HandleFunc("POST /api/ram/policies", handleCreateRAMPolicy)
	mux.HandleFunc("POST /api/ram/simulate", handleSimulate)
	return requestIDMiddleware(mux)
}

func authToken(t *testing.T) string {
	t.Helper()
	mu.RLock()
	a := byID[100123]
	mu.RUnlock()
	if a.AccountID == 0 {
		t.Fatal("seed account 100123 missing")
	}
	return issueAccessToken(a, time.Now())
}

func doAuth(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+authToken(t))
	req.Header.Set("X-Sc-TraceId", "t")
	rr := httptest.NewRecorder()
	newTestAuthMux(t).ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func testData(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	if d, ok := out["Data"].(map[string]any); ok {
		return d
	}
	t.Fatalf("no Data in envelope: %v", out)
	return nil
}

func TestRAMRolesListSeeded(t *testing.T) {
	code, out := doAuth(t, "GET", "/api/ram/roles", nil)
	if code != 200 {
		t.Fatalf("roles list: code %d body %v", code, out)
	}
	d := testData(t, out)
	roles, _ := d["roles"].([]any)
	if len(roles) < 3 {
		t.Fatalf("expected ≥3 seeded roles, got %d", len(roles))
	}
	names := map[string]bool{}
	for _, r := range roles {
		if rm, ok := r.(map[string]any); ok {
			names[rm["name"].(string)] = true
		}
	}
	for _, want := range []string{"Admin", "Operator", "ReadOnly"} {
		if !names[want] {
			t.Errorf("seeded role %q missing", want)
		}
	}
}

func TestRAMPoliciesListSeeded(t *testing.T) {
	code, out := doAuth(t, "GET", "/api/ram/policies", nil)
	if code != 200 {
		t.Fatalf("policies list: code %d body %v", code, out)
	}
	d := testData(t, out)
	pols, _ := d["policies"].([]any)
	names := map[string]bool{}
	for _, p := range pols {
		if pm, ok := p.(map[string]any); ok {
			names[pm["name"].(string)] = true
		}
	}
	if !names["ScEcsFullAccess"] {
		t.Error("ScEcsFullAccess system policy missing")
	}
	if !names["ScReadOnlyAccess"] {
		t.Error("ScReadOnlyAccess system policy missing")
	}
}

func TestSimulateAllow(t *testing.T) {
	// ScEcsFullAccess allows scecs:* on *. The verdict must name the allow
	// statement and the policy.
	code, out := doAuth(t, "POST", "/api/ram/simulate", map[string]any{
		"principal": "ops-admin",
		"policies":  []string{"ScEcsFullAccess"},
		"action":    "scecs:StartInstance",
		"resource":  "sc:ecs:cn-north-1:100123:instance/i-1",
	})
	if code != 200 {
		t.Fatalf("simulate: code %d body %v", code, out)
	}
	d := testData(t, out)
	if d["allowed"] != true {
		t.Errorf("allowed = %v, want true", d["allowed"])
	}
	if d["reason"] != "allow" {
		t.Errorf("reason = %v, want allow", d["reason"])
	}
	if d["decidingPolicy"] != "ScEcsFullAccess" {
		t.Errorf("decidingPolicy = %v, want ScEcsFullAccess", d["decidingPolicy"])
	}
}

func TestSimulateExplicitDenyWins(t *testing.T) {
	// Create a custom policy that DENIES scecs:DeleteInstance, then simulate
	// with both it and ScEcsFullAccess: the explicit Deny must win, and the
	// verdict must name the DENY policy as the deciding one.
	denyDoc := `{"Version":"1","Statement":[{"Effect":"Deny","Action":"scecs:DeleteInstance","Resource":"*"}]}`
	if code, _ := doAuth(t, "POST", "/api/ram/policies", map[string]any{
		"name": "DenyDelete", "document": denyDoc,
	}); code != 201 {
		t.Fatalf("create deny policy: code %d", code)
	}
	code, out := doAuth(t, "POST", "/api/ram/simulate", map[string]any{
		"principal": "ops-admin",
		"policies":  []string{"ScEcsFullAccess", "DenyDelete"},
		"action":    "scecs:DeleteInstance",
		"resource":  "sc:ecs:cn-north-1:100123:instance/i-1",
	})
	if code != 200 {
		t.Fatalf("simulate: code %d body %v", code, out)
	}
	d := testData(t, out)
	if d["allowed"] != false {
		t.Errorf("allowed = %v, want false (deny wins)", d["allowed"])
	}
	if d["reason"] != "explicit_deny" {
		t.Errorf("reason = %v, want explicit_deny", d["reason"])
	}
	if d["decidingPolicy"] != "DenyDelete" {
		t.Errorf("decidingPolicy = %v, want DenyDelete", d["decidingPolicy"])
	}
}

func TestSimulateDefaultDeny(t *testing.T) {
	// ScReadOnlyAccess only allows Describe*/List*. StartInstance matches
	// nothing → default_deny, with no deciding statement/policy.
	code, out := doAuth(t, "POST", "/api/ram/simulate", map[string]any{
		"principal": "dev-ro",
		"policies":  []string{"ScReadOnlyAccess"},
		"action":    "scecs:StartInstance",
		"resource":  "sc:ecs:cn-north-1:100123:instance/i-1",
	})
	if code != 200 {
		t.Fatalf("simulate: code %d body %v", code, out)
	}
	d := testData(t, out)
	if d["allowed"] != false {
		t.Errorf("allowed = %v, want false", d["allowed"])
	}
	if d["reason"] != "default_deny" {
		t.Errorf("reason = %v, want default_deny", d["reason"])
	}
	if d["decidingStatement"] != float64(-1) {
		t.Errorf("decidingStatement = %v, want -1", d["decidingStatement"])
	}
	if d["decidingPolicy"] != "" {
		t.Errorf("decidingPolicy = %v, want empty", d["decidingPolicy"])
	}
}

func TestSimulateRejectsUnknownPolicy(t *testing.T) {
	// An unknown policy name must 404, not silently treat it as no-match.
	code, out := doAuth(t, "POST", "/api/ram/simulate", map[string]any{
		"principal": "x",
		"policies":  []string{"DoesNotExist"},
		"action":    "scecs:StartInstance",
		"resource":  "sc:ecs:cn-north-1:100123:i/1",
	})
	if code != 404 {
		t.Fatalf("unknown policy: code %d, want 404 (body %v)", code, out)
	}
	if out["Code"] != "Auth.PolicyNotFound" {
		t.Errorf("code = %v, want Auth.PolicyNotFound", out["Code"])
	}
}

func TestCreatePolicyRejectsMalformedDocument(t *testing.T) {
	// A document authz.ParsePolicy rejects must not persist.
	code, _ := doAuth(t, "POST", "/api/ram/policies", map[string]any{
		"name": "Bad", "document": `{"Version":"1","Statement":[{"Effect":"Maybe"}]}`,
	})
	if code != 400 {
		t.Fatalf("malformed policy: code %d, want 400", code)
	}
}
