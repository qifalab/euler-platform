// Package authz simulator — the 策略模拟器 (07-security.md §3).
//
// The simulator lets an operator ask "would principal X be allowed to perform
// action Y on resource Z, and which statement decides it?" BEFORE committing a
// policy. The design goal is to catch a mis-scoped policy at design time — in
// the simulator's verdict — rather than at the first customer-data incident it
// would have caused in production.
//
// Deny-first is the guarantee (07§3.3), and the simulator's default is the same
// as production's default: no matching statement → deny; any matching explicit
// Deny wins over any matching Allow. The simulator reuses the engine's own
// matchAction / matchResource / matchConditions so it cannot drift from how
// Evaluate actually decides a request in production.
//
// Naming: a Policy document has no Name field (the JSON shape 07§3.2 is
// {Version,Statement}), so the simulator carries policy names through a
// NamedPolicy wrapper. DecidingPolicy in the verdict is the wrapper's Name,
// letting the console point the operator at the exact policy+statement that
// decided the result.
package authz

// NamedPolicy pairs a policy document with the name/ID it is attached under, so
// a simulator verdict can name the deciding policy (07§3.2 — the policy
// document itself carries no name).
type NamedPolicy struct {
	Name   string
	Policy Policy
}

// Verdict is the result of evaluating a request against a policy set — the
// policy simulator's output. It names the statement (and the policy) that
// decided the outcome, so the console can show the operator exactly which rule
// granted or denied the request.
type Verdict struct {
	Allowed           bool
	DecidingStatement int    // index into the deciding policy's Statements, -1 if default-deny
	DecidingPolicy    string // deciding NamedPolicy.Name, "" if default-deny
	Reason            string // "explicit_deny" | "allow" | "default_deny" | "no_policy"
	Principal         string // echoed for audit/display
}

// Simulate evaluates a single request against a set of named policies,
// Deny-first (07§3.3): an explicit Deny always wins; absent any matching
// statement the default is Deny. principal is carried on the verdict for
// audit/display only — platform-internal policies are always bound to an
// identity, so the principal is not part of statement matching (see policy.go
// doc: "Principal is removed from the statement").
//
// The matching logic reuses matchAction/matchResource/matchConditions — the
// same primitives Evaluate uses — so the simulator and the engine can never
// disagree about what a statement matches.
func Simulate(policies []NamedPolicy, principal string, action, resource string) Verdict {
	return SimulateCtx(policies, principal, Request{Action: action, Resource: resource})
}

// SimulateCtx is Simulate with a condition context (sc:SourceIp, sc:MFAPresent,
// etc). Conditions are part of the match — a statement whose Condition block
// does not pass does not match, so it neither allows nor denies.
func SimulateCtx(policies []NamedPolicy, principal string, req Request) Verdict {
	v := Verdict{Allowed: false, DecidingStatement: -1, DecidingPolicy: "", Reason: "no_policy", Principal: principal}
	if len(policies) == 0 {
		return v
	}
	v.Reason = "default_deny"

	matchedAllow := -1
	matchedAllowName := ""

	for _, np := range policies {
		for si, st := range np.Policy.Statement {
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
				return Verdict{
					Allowed:           false,
					DecidingStatement: si,
					DecidingPolicy:    np.Name,
					Reason:            "explicit_deny",
					Principal:         principal,
				}
			}
			if matchedAllow < 0 {
				matchedAllow = si
				matchedAllowName = np.Name
			}
		}
	}

	if matchedAllow >= 0 {
		// Rule 3: matching Allow, no Deny.
		v.Allowed = true
		v.DecidingStatement = matchedAllow
		v.DecidingPolicy = matchedAllowName
		v.Reason = "allow"
		return v
	}
	// Rule 1: default Deny (policies existed but nothing matched).
	return v
}
