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
	seedAccount(100123, "admin@euler.emoera.com", "种子管理员", "euler123")
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
	req.Header.Set("X-Euler-TraceId", "t")
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
	if !names["EuEcsFullAccess"] {
		t.Error("EuEcsFullAccess system policy missing")
	}
	if !names["EuReadOnlyAccess"] {
		t.Error("EuReadOnlyAccess system policy missing")
	}
}

func TestSimulateAllow(t *testing.T) {
	// EuEcsFullAccess allows euecs:* on *. The verdict must name the allow
	// statement and the policy.
	code, out := doAuth(t, "POST", "/api/ram/simulate", map[string]any{
		"principal": "ops-admin",
		"policies":  []string{"EuEcsFullAccess"},
		"action":    "euecs:StartInstance",
		"resource":  "eu:ecs:cn-north-1:100123:instance/i-1",
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
	if d["decidingPolicy"] != "EuEcsFullAccess" {
		t.Errorf("decidingPolicy = %v, want EuEcsFullAccess", d["decidingPolicy"])
	}
}

func TestSimulateExplicitDenyWins(t *testing.T) {
	// Create a custom policy that DENIES euecs:DeleteInstance, then simulate
	// with both it and EuEcsFullAccess: the explicit Deny must win, and the
	// verdict must name the DENY policy as the deciding one.
	denyDoc := `{"Version":"1","Statement":[{"Effect":"Deny","Action":"euecs:DeleteInstance","Resource":"*"}]}`
	if code, _ := doAuth(t, "POST", "/api/ram/policies", map[string]any{
		"name": "DenyDelete", "document": denyDoc,
	}); code != 201 {
		t.Fatalf("create deny policy: code %d", code)
	}
	code, out := doAuth(t, "POST", "/api/ram/simulate", map[string]any{
		"principal": "ops-admin",
		"policies":  []string{"EuEcsFullAccess", "DenyDelete"},
		"action":    "euecs:DeleteInstance",
		"resource":  "eu:ecs:cn-north-1:100123:instance/i-1",
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
	// EuReadOnlyAccess only allows Describe*/List*. StartInstance matches
	// nothing → default_deny, with no deciding statement/policy.
	code, out := doAuth(t, "POST", "/api/ram/simulate", map[string]any{
		"principal": "dev-ro",
		"policies":  []string{"EuReadOnlyAccess"},
		"action":    "euecs:StartInstance",
		"resource":  "eu:ecs:cn-north-1:100123:instance/i-1",
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
		"action":    "euecs:StartInstance",
		"resource":  "eu:ecs:cn-north-1:100123:i/1",
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

// --- security-fix regression tests ---

// TestChangePasswordInvalidatesOldTokensAndSessions covers the tokenVersion
// bump + session revocation on password change, and the atomic read-modify-write.
func TestChangePasswordInvalidatesOldTokensAndSessions(t *testing.T) {
	seedAccount(200001, "pwtest@euler.emoera.com", "改密用户", "oldpass12345")
	mu.RLock()
	a := byID[200001]
	mu.RUnlock()
	oldToken := issueAccessToken(a, time.Now())

	// Plant a refresh session for the account; it must be revoked.
	mu.Lock()
	sessions["pwtest-refresh"] = &session{account: a, refreshToken: "pwtest-refresh", refreshExp: time.Now().Add(time.Hour)}
	mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/account/password", handleChangePassword)
	mux.HandleFunc("GET /api/account/profile", handleAccountProfile)
	h := requestIDMiddleware(mux)

	raw, _ := json.Marshal(map[string]string{"oldPassword": "oldpass12345", "newPassword": "newpass12345"})
	req := httptest.NewRequest("PUT", "/api/account/password", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+oldToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("change password: code %d body %s", rr.Code, rr.Body.String())
	}

	// Old token must now be rejected (TokenVersion mismatch).
	req2 := httptest.NewRequest("GET", "/api/account/profile", nil)
	req2.Header.Set("Authorization", "Bearer "+oldToken)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != 401 {
		t.Errorf("old token after password change: code %d, want 401", rr2.Code)
	}

	mu.RLock()
	_, sessAlive := sessions["pwtest-refresh"]
	cur := byID[200001]
	mu.RUnlock()
	if sessAlive {
		t.Error("refresh session survived password change")
	}
	if !verifyPassword(cur.PasswordHash, "newpass12345") {
		t.Error("new password not installed")
	}

	// A token minted from the fresh account state works.
	newToken := issueAccessToken(cur, time.Now())
	req3 := httptest.NewRequest("GET", "/api/account/profile", nil)
	req3.Header.Set("Authorization", "Bearer "+newToken)
	rr3 := httptest.NewRecorder()
	h.ServeHTTP(rr3, req3)
	if rr3.Code != 200 {
		t.Errorf("new token: code %d, want 200", rr3.Code)
	}
}

// TestRefreshRejectsDisabledAccount covers the live-status recheck on refresh.
func TestRefreshRejectsDisabledAccount(t *testing.T) {
	seedAccount(200002, "frozen@euler.emoera.com", "冻结用户", "somepass1234")
	mu.Lock()
	a := byID[200002]
	sessions["frozen-refresh"] = &session{account: a, refreshToken: "frozen-refresh", refreshExp: time.Now().Add(time.Hour)}
	a.Status = 2 // freeze after the session was issued
	accounts[a.AccountName] = a
	byID[a.AccountID] = a
	mu.Unlock()

	req := httptest.NewRequest("POST", "/api/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "eu_refresh", Value: "frozen-refresh"})
	rr := httptest.NewRecorder()
	requestIDMiddleware(http.HandlerFunc(handleRefresh)).ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Errorf("refresh for frozen account: code %d, want 401", rr.Code)
	}
	mu.RLock()
	_, alive := sessions["frozen-refresh"]
	mu.RUnlock()
	if alive {
		t.Error("session for frozen account not revoked on refresh")
	}
}

// TestRotateUsesEnabledCount covers the rotate limit switching to countEnabled:
// 2 AKs with one disabled must still allow rotation of the enabled one.
func TestRotateUsesEnabledCount(t *testing.T) {
	seedAccount(200003, "rotate@euler.emoera.com", "轮换用户", "somepass1234")
	mu.Lock()
	a := byID[200003]
	_, _, k1 := issueAccessKey()
	_, _, k2 := issueAccessKey()
	k2.Status = 2 // disabled
	accessKeys[200003] = []accessKey{k1, k2}
	mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/ak/{akId}/rotate", handleRotateAccessKey)
	req := httptest.NewRequest("POST", "/api/ak/"+k1.AKID+"/rotate", nil)
	req.Header.Set("Authorization", "Bearer "+issueAccessToken(a, time.Now()))
	rr := httptest.NewRecorder()
	requestIDMiddleware(mux).ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("rotate with 1 enabled + 1 disabled AK: code %d body %s, want 201", rr.Code, rr.Body.String())
	}
}

// TestOversizedBodyRejected covers the MaxBytesReader cap on JSON handlers.
func TestOversizedBodyRejected(t *testing.T) {
	big := bytes.Repeat([]byte("a"), maxBodyBytes+1024)
	body, _ := json.Marshal(map[string]string{"name": "Big", "document": string(big)})
	req := httptest.NewRequest("POST", "/api/ram/policies", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+authToken(t))
	rr := httptest.NewRecorder()
	newTestAuthMux(t).ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: code %d, want 413", rr.Code)
	}
}
