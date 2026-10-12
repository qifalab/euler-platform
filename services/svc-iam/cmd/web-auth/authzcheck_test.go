package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/authz"
	"github.com/qifalab/euler-platform/httpmw"
)

const authzAccount = 300001

// seedAuthzFixture provisions an account with the system policies, a custom
// explicit-Deny policy and RAM users with different attachments.
func seedAuthzFixture(t *testing.T) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	mustSeed(t, authzAccount, "authz-check@euler.emoera.com", "", "authzpass123")
	if err := seedRAMRolesPolicies(ctx, authzAccount); err != nil {
		t.Fatal(err)
	}
	deny := `{"Version":"1","Statement":[{"Effect":"Deny","Action":"euecs:DeleteInstance","Resource":"*"}]}`
	p, err := authz.ParsePolicy([]byte(deny))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePolicy(ctx, storedPolicy{AccountID: authzAccount, Name: "DenyDelete", Type: "custom", Document: deny, Parsed: p, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, spec := range []struct {
		name     string
		status   int
		policies []string
	}{
		{"ops", 1, []string{"EuEcsFullAccess"}},
		{"ro", 1, []string{"EuReadOnlyAccess"}},
		{"guarded", 1, []string{"EuEcsFullAccess", "DenyDelete"}},
		{"disabled", 2, []string{"EuEcsFullAccess"}},
		{"nothing", 1, nil},
	} {
		u, err := st.CreateRAMUser(ctx, ramUser{AccountID: authzAccount, Username: spec.name, Status: spec.status, Policies: spec.policies, CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		ids[spec.name] = u.ID
	}
	return ids
}

func postCheck(t *testing.T, h http.Handler, token string, body any) (int, authzCheckResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/internal/authz/check", bytes.NewReader(raw))
	if token != "" {
		req.Header.Set(httpmw.InternalTokenHeader, token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out authzCheckResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

// Regression (I3): /internal/authz/check did not exist, so every route with
// eu-authorize answered 503. The contract is eu-authorize.lua's
// call_check_access: {account_id, identity, action, resource?, context} →
// {allow, decision_number}.
func TestAuthzCheckDecisions(t *testing.T) {
	forEachBackend(t, testAuthzCheckDecisions)
}

func testAuthzCheckDecisions(t *testing.T) {
	ids := seedAuthzFixture(t)
	t.Setenv(httpmw.InternalTokenEnv, "internal-s3cret")
	h := internalOnly(http.HandlerFunc(handleAuthzCheck))
	ctx := map[string]any{"eu:SourceIp": "10.1.2.3", "eu:CurrentTime": "2026-10-01T10:00:00Z"}

	cases := []struct {
		name, identity, action string
		resource               string
		want                   bool
	}{
		{"root may do anything", "", "euecs:DeleteInstance", "", true},
		{"full-access user", "user/ops", "euecs:RunInstances", "", true},
		{"full-access user by id", "user/id:" + itoa(ids["ops"]), "euecs:RunInstances", "", true},
		{"read-only user may read", "user/ro", "euecs:DescribeInstances", "", true},
		{"read-only user may not write", "user/ro", "euecs:RunInstances", "", false},
		{"no policy attached → default deny", "user/nothing", "euecs:DescribeInstances", "", false},
		{"explicit deny wins over allow", "user/guarded", "euecs:DeleteInstance", "", false},
		{"explicit deny is scoped to its action", "user/guarded", "euecs:StopInstances", "", true},
		{"explicit deny with a resource ARN", "user/guarded", "euecs:DeleteInstance", "eu:ecs:cn-north-1:300001:instance/i-1", false},
		{"other product not granted", "user/ops", "euoss:GetObject", "", false},
		{"disabled user", "user/disabled", "euecs:RunInstances", "", false},
		{"unknown user", "user/ghost", "euecs:DescribeInstances", "", false},
		{"unknown id", "user/id:999999", "euecs:DescribeInstances", "", false},
		{"unknown principal kind", "group/ops", "euecs:DescribeInstances", "", false},
		{"bare kind", "user/", "euecs:DescribeInstances", "", false},
		{"assumed role", "role/ReadOnly", "euecs:DescribeInstances", "", true},
		{"assumed role cannot write", "role/ReadOnly", "euecs:RunInstances", "", false},
		{"unknown role", "role/Ghost", "euecs:DescribeInstances", "", false},
	}
	for _, c := range cases {
		body := map[string]any{"account_id": "300001", "identity": c.identity, "action": c.action, "context": ctx}
		if c.resource != "" {
			body["resource"] = c.resource
		}
		code, out := postCheck(t, h, "internal-s3cret", body)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", c.name, code)
		}
		if out.Allow != c.want {
			t.Errorf("%s: allow=%v want %v (decision %s)", c.name, out.Allow, c.want, out.DecisionNumber)
		}
		if out.DecisionNumber == "" {
			t.Errorf("%s: empty decision_number", c.name)
		}
	}

	// A numeric account_id is accepted too.
	if code, out := postCheck(t, h, "internal-s3cret", map[string]any{"account_id": 300001, "identity": "user/ops", "action": "euecs:RunInstances"}); code != 200 || !out.Allow {
		t.Fatalf("numeric account_id: %d %+v", code, out)
	}
	// Malformed input is a 400 (the plugin fails closed on any non-200).
	if code, _ := postCheck(t, h, "internal-s3cret", map[string]any{"identity": "", "action": "euecs:RunInstances"}); code != http.StatusBadRequest {
		t.Fatalf("missing account_id: %d", code)
	}
	if code, _ := postCheck(t, h, "internal-s3cret", map[string]any{"account_id": "300001", "identity": ""}); code != http.StatusBadRequest {
		t.Fatalf("missing action: %d", code)
	}
}

// The endpoint answers only the gateway: without (or with a wrong) internal
// token it is 403, and in production without a configured token it refuses.
func TestAuthzCheckRequiresInternalToken(t *testing.T) {
	useBackend(t, memoryBackend())
	seedAuthzFixture(t)
	body := map[string]any{"account_id": "300001", "identity": "", "action": "euecs:RunInstances"}

	t.Setenv(httpmw.InternalTokenEnv, "internal-s3cret")
	h := internalOnly(http.HandlerFunc(handleAuthzCheck))
	for _, tok := range []string{"", "guess"} {
		if code, _ := postCheck(t, h, tok, body); code != http.StatusForbidden {
			t.Fatalf("token %q: status %d, want 403", tok, code)
		}
	}

	t.Setenv(httpmw.InternalTokenEnv, "")
	t.Setenv("EULER_ENV", "prod")
	h = internalOnly(http.HandlerFunc(handleAuthzCheck))
	if code, _ := postCheck(t, h, "", body); code != http.StatusServiceUnavailable {
		t.Fatalf("production without a token: status %d, want 503", code)
	}
}

// A frozen account is denied even for the root identity.
func TestAuthzCheckFrozenAccountDenied(t *testing.T) {
	forEachBackend(t, func(t *testing.T) {
		mustSeed(t, 300002, "authz-frozen@euler.emoera.com", "", "authzpass123")
		setAccountStatus(t, 300002, 2)
		d, err := checkAccess(context.Background(), 300002, "", authz.Request{Action: "euecs:RunInstances", Resource: "*"})
		if err != nil {
			t.Fatal(err)
		}
		if d.Allow {
			t.Fatal("frozen account allowed")
		}
	})
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
