package main

// Gateway CheckAccess (07§3.4): POST /internal/authz/check.
//
// eu-authorize.lua calls this for every authorized route. The contract is the
// plugin's (call_check_access), not a public API:
//
//	request  {"account_id": "100123", "identity": "" | "user/<name>" | "user/id:<n>" |
//	          "role/<name>" | "role/id:<n>", "action": "euecs:RunInstances",
//	          "resource": "<arn>" (absent for action-level checks),
//	          "context": {"eu:SourceIp": "...", "eu:CurrentTime": "..."}}
//	response 200 {"allow": bool, "decision_number": "..."}
//
// Any non-200 is treated by the plugin as "backend unavailable" and fails
// closed (503), so malformed input is answered 400.
//
// Evaluation (pkg-go/authz, Deny-first, default deny):
//   - empty identity = the account root (master account, console JWT, or a
//     master-account AK): allowed, unless the account is known to be frozen;
//   - a RAM user or an assumed role: the policies attached to it, from this
//     account's policy set;
//   - anything else (unknown kind, missing / disabled user or role): deny.
//
// The decision number is an opaque audit reference; the policy detail never
// leaves svc-iam (07§3.4).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/qifalab/euler-platform/authz"
	"github.com/qifalab/euler-platform/httpmw"
	"github.com/qifalab/euler-platform/runmode"
)

type authzCheckRequest struct {
	AccountID json.RawMessage            `json:"account_id"` // string (header value) or number
	Identity  string                     `json:"identity"`
	Action    string                     `json:"action"`
	Resource  string                     `json:"resource"`
	Context   map[string]json.RawMessage `json:"context"`
}

type authzCheckResponse struct {
	Allow          bool   `json:"allow"`
	DecisionNumber string `json:"decision_number"`
}

// maxAuthzCheckBody bounds the internal request (it carries a handful of
// short strings).
const maxAuthzCheckBody = 64 << 10

// internalOnly guards a gateway → svc-iam endpoint with the shared internal
// token (httpmw.InternalToken). In production the token is mandatory: without
// it the endpoint refuses every call instead of answering anyone who can reach
// the port (the gateway then fails closed).
func internalOnly(h http.Handler) http.Handler {
	if os.Getenv(httpmw.InternalTokenEnv) == "" && runmode.Production() {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusServiceUnavailable, "Common.ServiceUnavailable", "internal token not configured")
		})
	}
	return httpmw.InternalToken(h)
}

func handleAuthzCheck(w http.ResponseWriter, r *http.Request) {
	var req authzCheckRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthzCheckBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed body")
		return
	}
	accountID, err := parseAccountIDField(req.AccountID)
	if err != nil || req.Action == "" {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "account_id and action are required")
		return
	}
	ctx, err := conditionContext(req.Context)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Common.InvalidParameter", "malformed context")
		return
	}
	// Action-level checks (no resource_arn_template on the route) evaluate
	// against "*": only statements granting the action on every resource
	// apply. Resource-scoped grants are decided by the owning service.
	resource := req.Resource
	if resource == "" {
		resource = "*"
	}
	d, err := checkAccess(r.Context(), accountID, req.Identity, authz.Request{Action: req.Action, Resource: resource, Context: ctx})
	if err != nil {
		// Not a decision: the plugin treats any non-200 as "backend
		// unavailable" and fails closed.
		internalError(w, r, "authz check", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(authzCheckResponse{Allow: d.Allow, DecisionNumber: d.DecisionNumber})
}

// checkAccess evaluates one request for the identity, from the store (with
// persistence: account_db, so a RAM-user key verified by the verify container
// is judged by the policies attached in the console). An error is an
// infrastructure failure, never a decision.
func checkAccess(ctx context.Context, accountID int64, identity string, req authz.Request) (authz.Decision, error) {
	// A frozen / closed account gets nothing, whoever in it is asking. An
	// account web-auth does not hold (e.g. one provisioned straight into
	// account_db) is not judged here: the verifier already checked it.
	a, err := st.AccountByID(ctx, accountID)
	switch {
	case err == nil && a.Status != 1:
		return authz.Decision{Allow: false, DecisionNumber: "D-account-unusable", MatchedStatement: -1}, nil
	case err != nil && !errors.Is(err, errNotFound):
		return authz.Decision{}, err
	}
	if identity == "" {
		return authz.Decision{Allow: true, DecisionNumber: "A-root", MatchedStatement: -1}, nil
	}
	names, ok, err := principalPolicyNames(ctx, accountID, identity)
	if err != nil {
		return authz.Decision{}, err
	}
	if !ok {
		return authz.Decision{Allow: false, DecisionNumber: "D-unknown-principal", MatchedStatement: -1}, nil
	}
	found, err := st.PoliciesByName(ctx, accountID, names)
	if err != nil {
		return authz.Decision{}, err
	}
	policies := make([]authz.Policy, 0, len(names))
	for _, n := range names {
		if sp, ok := found[n]; ok {
			policies = append(policies, sp.Parsed)
		}
	}
	return authz.Evaluate(policies, req), nil
}

// principalPolicyNames resolves a non-root principal to its attached policy
// names. "<kind>/id:<n>" names the user/role by numeric id (the form the
// verifier emits for a RAM-user / role access key); "<kind>/<name>" names it
// by user name / role name (STS emits "role/<RoleName>"). Only enabled users
// and roles resolve.
func principalPolicyNames(ctx context.Context, accountID int64, identity string) ([]string, bool, error) {
	kind, ref, found := strings.Cut(identity, "/")
	if !found || ref == "" {
		return nil, false, nil
	}
	idRef, byIDRef := strings.CutPrefix(ref, "id:")
	var wantID int64
	if byIDRef {
		n, err := strconv.ParseInt(idRef, 10, 64)
		if err != nil || n <= 0 {
			return nil, false, nil
		}
		wantID = n
	}
	switch kind {
	case "user":
		var u ramUser
		var err error
		if byIDRef {
			u, err = st.RAMUserByID(ctx, accountID, wantID)
		} else {
			u, err = st.RAMUserByName(ctx, accountID, ref)
		}
		if errors.Is(err, errNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if u.Status != 1 {
			return nil, false, nil
		}
		return u.Policies, true, nil
	case "role":
		var role ramRole
		var err error
		if byIDRef {
			role, err = st.RAMRoleByID(ctx, accountID, wantID)
		} else {
			role, err = st.RAMRoleByName(ctx, accountID, ref)
		}
		if errors.Is(err, errNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if role.Status != 1 {
			return nil, false, nil
		}
		return role.Policies, true, nil
	}
	return nil, false, nil
}

// parseAccountIDField accepts the account id as a JSON string (the gateway
// forwards the X-Euler-Account-Id header value) or a number.
func parseAccountIDField(raw json.RawMessage) (int64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return 0, err
		}
		s = n.String()
	}
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid account_id")
	}
	return id, nil
}

// conditionContext flattens the eu:* condition keys into the engine's string
// map. Scalars only; anything structured is refused.
func conditionContext(raw map[string]json.RawMessage) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
			continue
		}
		var b bool
		if err := json.Unmarshal(v, &b); err == nil {
			out[k] = strconv.FormatBool(b)
			continue
		}
		var n json.Number
		if err := json.Unmarshal(v, &n); err == nil {
			out[k] = n.String()
			continue
		}
		return nil, errors.New("unsupported context value for " + k)
	}
	return out, nil
}
