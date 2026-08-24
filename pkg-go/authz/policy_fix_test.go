package authz

import "testing"

// A Deny whose condition uses an operator the engine does not implement must
// still DENY — treating the condition as unmatched would silently switch the
// guard rail off (fail-open). An Allow in the same situation must NOT grant.
func TestUnknownConditionOperatorFailsSafePerEffect(t *testing.T) {
	deny, err := ParsePolicy([]byte(`{
	  "Version":"1","Statement":[
	    {"Effect":"Allow","Action":"*","Resource":"*"},
	    {"Effect":"Deny","Action":"scecs:DeleteInstance","Resource":"*",
	     "Condition":{"NumericLessThan":{"sc:RiskScore":["10"]}}}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Action: "scecs:DeleteInstance", Resource: "sc:ecs:cn-north-1:100:instance/i-1"}
	d := Evaluate([]Policy{deny}, req)
	if d.Allow {
		t.Fatal("Deny with an unknown condition operator must still deny")
	}

	allow, err := ParsePolicy([]byte(`{
	  "Version":"1","Statement":[
	    {"Effect":"Allow","Action":"*","Resource":"*",
	     "Condition":{"NumericLessThan":{"sc:RiskScore":["10"]}}}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	d2 := Evaluate([]Policy{allow}, req)
	if d2.Allow {
		t.Fatal("Allow with an unknown condition operator must not grant (fail closed)")
	}
}

// A pattern with fewer than 5 ARN segments must not fall back to a raw
// wildcard compare: "sc:iam:*" would otherwise glob across the account id.
func TestShortARNPatternNeverMatches(t *testing.T) {
	p, err := ParsePolicy([]byte(`{
	  "Version":"1","Statement":[
	    {"Effect":"Allow","Action":"*","Resource":"sc:iam:*"}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	d := Evaluate([]Policy{p}, Request{
		Action:   "iam:GetUser",
		Resource: "sc:iam:cn-north-1:9999999999:user/victim",
	})
	if d.Allow {
		t.Fatal("truncated ARN pattern must not match a cross-account resource")
	}
	// "*" alone still matches everything (explicit universal grant).
	star, _ := ParsePolicy([]byte(`{"Version":"1","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`))
	if !Evaluate([]Policy{star}, Request{Action: "iam:GetUser", Resource: "sc:iam:r:1:user/x"}).Allow {
		t.Fatal("bare * resource must still match")
	}
}

// The decision number must name the REAL policy and statement that decided,
// not a hard-coded policy index 0.
func TestDecisionNumberCarriesRealIndices(t *testing.T) {
	noMatch, _ := ParsePolicy([]byte(`{"Version":"1","Statement":[{"Effect":"Allow","Action":"scoss:*","Resource":"*"}]}`))
	match, _ := ParsePolicy([]byte(`{"Version":"1","Statement":[
	  {"Effect":"Allow","Action":"scoss:*","Resource":"*"},
	  {"Effect":"Allow","Action":"scecs:*","Resource":"*"}
	]}`))
	d := Evaluate([]Policy{noMatch, match}, Request{Action: "scecs:StartInstance", Resource: "sc:ecs:r:1:instance/i-1"})
	if !d.Allow {
		t.Fatal("expected allow")
	}
	if d.DecisionNumber != "A-p1-s1" {
		t.Fatalf("DecisionNumber = %q, want A-p1-s1 (second policy, second statement)", d.DecisionNumber)
	}
}
