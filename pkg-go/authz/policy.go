// Package authz implements the RAM policy model and evaluation engine
// (07-security.md §3).
//
// Model: RBAC skeleton (user → group/role → system policy) + JSON Policy for
// fine-grained custom rules, aligned with Alibaba Cloud RAM. Principal is
// removed from the statement — platform-internal policies are always bound to
// an identity, so the binding carries the principal.
//
// Evaluation rules (07§3.3), in strict order:
//  1. Default Deny — no matching statement means deny.
//  2. Any explicit Deny wins, regardless of matching Allows.
//  3. A matching Allow with no matching Deny yields Allow.
//
// This engine is the shared authoritative implementation; svc-iam (Go/Kratos)
// implements the same semantics against the same policy
// JSON. The gateway's eu-authorize plugin calls svc-iam CheckAccess, which
// runs this evaluation — the engine is used both in-process (Go services doing
// owner checks) and behind the RPC.
package authz

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// Effect is Allow or Deny.
type Effect string

const (
	EffectAllow Effect = "Allow"
	EffectDeny  Effect = "Deny"
)

// Policy is a RAM policy document.
//
//	{"Version":"1","Statement":[{Effect,Action,Resource,Condition}]}
type Policy struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

// Statement is a single policy rule. Action and Resource accept either a JSON
// string or an array of strings, which is why they use flexList.
type Statement struct {
	Effect    Effect                         `json:"Effect"`
	Action    flexList                       `json:"Action"`
	Resource  flexList                       `json:"Resource"`
	Condition map[string]map[string]flexList `json:"Condition,omitempty"`
}

// flexList unmarshals both `"x"` and `["x","y"]` into a []string.
type flexList []string

func (f *flexList) UnmarshalJSON(data []byte) error {
	// Try a single string first.
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*f = []string{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("authz: Action/Resource must be string or []string: %w", err)
	}
	*f = many
	return nil
}

// MarshalJSON emits a bare string when there is exactly one element, matching
// the compact form operators write by hand.
func (f flexList) MarshalJSON() ([]byte, error) {
	if len(f) == 1 {
		return json.Marshal(f[0])
	}
	return json.Marshal([]string(f))
}

// ParsePolicy decodes a policy document.
func ParsePolicy(data []byte) (Policy, error) {
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return Policy{}, fmt.Errorf("authz: parse policy: %w", err)
	}
	if p.Version == "" {
		return Policy{}, fmt.Errorf("authz: policy missing Version")
	}
	for i, s := range p.Statement {
		if s.Effect != EffectAllow && s.Effect != EffectDeny {
			return Policy{}, fmt.Errorf("authz: statement %d has invalid Effect %q", i, s.Effect)
		}
	}
	return p, nil
}

// Request is an authorization question: may this identity perform this action
// on this resource, in this context?
type Request struct {
	// Action is the operation, formatted {productCode}:{Operation},
	// e.g. "euecs:StartInstance" (07§3.1, S3).
	Action string

	// Resource is the target ARN:
	// eu:{service}:{region}:{account_id}:{relative-resource}
	Resource string

	// Context carries the eu:-prefixed condition keys. Phase-1 supported keys:
	// SourceIp, CurrentTime, MFAPresent, ResourceGroupId (07§3.3).
	Context map[string]string
}

// Decision is the evaluation outcome. DecisionNumber is an opaque audit
// reference returned to the caller; the policy detail is never leaked to the
// client on a Deny (07§3.4).
type Decision struct {
	Allow          bool
	DecisionNumber string
	// MatchedStatement is the index of the statement that decided the outcome,
	// for internal audit logging only. -1 when the default-deny applied.
	MatchedStatement int
}

// Evaluate runs the policy set against the request and returns the decision.
//
// The policies slice is the full effective set for the identity: system
// policies + custom policies attached directly, via groups, and via the
// assumed role. Order does not matter — Deny always wins.
func Evaluate(policies []Policy, req Request) Decision {
	matchedAllowPolicy, matchedAllowStmt := -1, -1

	for pi, p := range policies {
		for si, st := range p.Statement {
			if !matchAction(st.Action, req.Action) {
				continue
			}
			if !matchResource(st.Resource, req.Resource) {
				continue
			}
			if !matchConditions(st.Condition, req.Context, st.Effect) {
				continue
			}
			if st.Effect == EffectDeny {
				// Rule 2: any explicit Deny wins immediately.
				return Decision{
					Allow:            false,
					DecisionNumber:   decisionNumber(pi, si, false),
					MatchedStatement: si,
				}
			}
			if matchedAllowStmt < 0 {
				matchedAllowPolicy, matchedAllowStmt = pi, si
			}
		}
	}

	if matchedAllowStmt >= 0 {
		// Rule 3: matching Allow, no Deny.
		return Decision{Allow: true, DecisionNumber: decisionNumber(matchedAllowPolicy, matchedAllowStmt, true), MatchedStatement: matchedAllowStmt}
	}
	// Rule 1: default Deny.
	return Decision{Allow: false, DecisionNumber: "D-default-deny", MatchedStatement: -1}
}

func decisionNumber(policyIdx, stmtIdx int, allow bool) string {
	verdict := "D"
	if allow {
		verdict = "A"
	}
	return fmt.Sprintf("%s-p%d-s%d", verdict, policyIdx, stmtIdx)
}

// matchAction reports whether any pattern in the list matches the action.
// Wildcards: "*" matches everything; "euecs:*" matches all euecs operations;
// "euecs:Describe*" matches by prefix. Matching is case-insensitive on the
// operation, matching cloud-vendor convention.
func matchAction(patterns flexList, action string) bool {
	for _, p := range patterns {
		if wildcardMatch(strings.ToLower(p), strings.ToLower(action)) {
			return true
		}
	}
	return false
}

// matchResource reports whether any ARN pattern matches the target resource.
// ARN segments are matched with wildcard support in each position, so
// "eu:iam:*:1001234567890:user/dev-*" matches
// "eu:iam:cn-north-1:1001234567890:user/dev-alice".
func matchResource(patterns flexList, resource string) bool {
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if arnMatch(p, resource) {
			return true
		}
	}
	return false
}

// arnMatch compares two ARNs segment by segment (5 segments:
// eu:{service}:{region}:{account_id}:{relative-resource}). The relative
// resource may itself contain "/" and wildcards; it is matched as a whole.
//
// A pattern or target with fewer than 5 segments does NOT match. Falling back
// to a whole-string wildcard compare here would let a truncated pattern such
// as "eu:iam:*" glob across the account-id segment — a cross-account grant the
// author never wrote.
func arnMatch(pattern, target string) bool {
	pParts := strings.SplitN(pattern, ":", 5)
	tParts := strings.SplitN(target, ":", 5)
	if len(pParts) != 5 || len(tParts) != 5 {
		return false
	}
	for i := 0; i < 5; i++ {
		if !wildcardMatch(pParts[i], tParts[i]) {
			return false
		}
	}
	return true
}

// wildcardMatch implements glob matching with "*" (any run, including empty)
// and "?" (exactly one character). It is iterative with backtracking, so it
// does not blow the stack on adversarial patterns.
func wildcardMatch(pattern, s string) bool {
	var (
		pi, si   int
		starIdx  = -1
		matchIdx int
	)
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			starIdx = pi
			matchIdx = si
			pi++
		case starIdx >= 0:
			pi = starIdx + 1
			matchIdx++
			si = matchIdx
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// matchConditions evaluates the Condition block. All operators in the block
// must pass (AND), and within an operator all keys must pass (AND); within a
// key, any listed value matching is enough (OR). This mirrors RAM semantics.
//
// Phase-1 operators (07§3.3): StringEquals, StringNotEquals, IpAddress,
// NotIpAddress, DateGreaterThan, DateLessThan, Bool.
//
// An operator this engine does not implement makes the condition UNDECIDABLE,
// and the two effects must fail in opposite directions: an Allow with an
// undecidable condition must not grant (fail closed), while a Deny with an
// undecidable condition must still deny — treating it as "not matched" would
// silently switch the Deny off, which is fail-open on the guard rail.
func matchConditions(cond map[string]map[string]flexList, ctx map[string]string, effect Effect) bool {
	if len(cond) == 0 {
		return true
	}
	for op, kv := range cond {
		if !knownConditionOp(op) {
			// Undecidable: Deny statements match (deny wins on doubt);
			// Allow statements do not (never grant on doubt).
			return effect == EffectDeny
		}
		for key, wantVals := range kv {
			gotVal, present := ctx[key]
			if !evalCondition(op, gotVal, present, wantVals) {
				return false
			}
		}
	}
	return true
}

// knownConditionOp reports whether the engine implements the operator.
func knownConditionOp(op string) bool {
	switch op {
	case "StringEquals", "StringNotEquals", "IpAddress", "NotIpAddress",
		"DateGreaterThan", "DateLessThan", "Bool":
		return true
	}
	return false
}

func evalCondition(op, got string, present bool, want flexList) bool {
	switch op {
	case "StringEquals":
		if !present {
			return false
		}
		for _, w := range want {
			if got == w {
				return true
			}
		}
		return false

	case "StringNotEquals":
		if !present {
			// Absent value cannot equal anything, so "not equals" holds.
			return true
		}
		for _, w := range want {
			if got == w {
				return false
			}
		}
		return true

	case "IpAddress":
		if !present {
			return false
		}
		return ipMatchesAny(got, want)

	case "NotIpAddress":
		if !present {
			return true
		}
		return !ipMatchesAny(got, want)

	case "DateGreaterThan":
		if !present {
			return false
		}
		return compareDate(got, want, func(a, b time.Time) bool { return a.After(b) })

	case "DateLessThan":
		if !present {
			return false
		}
		return compareDate(got, want, func(a, b time.Time) bool { return a.Before(b) })

	case "Bool":
		if !present {
			return false
		}
		for _, w := range want {
			if strings.EqualFold(got, w) {
				return true
			}
		}
		return false

	default:
		// Unreachable via matchConditions (unknown operators are handled
		// there); kept as a fail-closed backstop for direct callers.
		return false
	}
}

// ipMatchesAny reports whether ip falls within any of the CIDR ranges (or
// equals any plain address) in want.
func ipMatchesAny(ip string, want flexList) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, w := range want {
		if strings.Contains(w, "/") {
			_, network, err := net.ParseCIDR(w)
			if err != nil {
				continue
			}
			if network.Contains(parsed) {
				return true
			}
			continue
		}
		if other := net.ParseIP(w); other != nil && other.Equal(parsed) {
			return true
		}
	}
	return false
}

// compareDate parses got as RFC3339 and compares it against each want value.
func compareDate(got string, want flexList, cmp func(a, b time.Time) bool) bool {
	gotTime, err := time.Parse(time.RFC3339, got)
	if err != nil {
		return false
	}
	for _, w := range want {
		wantTime, err := time.Parse(time.RFC3339, w)
		if err != nil {
			continue
		}
		if cmp(gotTime, wantTime) {
			return true
		}
	}
	return false
}
