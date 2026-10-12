package authz

import "testing"

// Regression (I5): wildcardMatch compared literal characters before handling
// "*", so a pattern "*" facing a literal "*" in the target was consumed as a
// plain character with no backtrack point recorded.
func TestWildcardMatchStarIsAlwaysWildcard(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"a*", "a*b", true},
		{"*", "**", true},
		{"*", "*", true},
		{"a*b", "a**b", true},
		{"secret/*", "secret/*x", true},
		{"*x", "*yx", true},
		{"?*", "**", true},
		{"a?c", "a*c", true},
		{"a*c", "a*b", false},
		{"abc", "a*c", false},
		{"", "", true},
		{"*", "", true},
		{"a", "", false},
	}
	for _, c := range cases {
		if got := wildcardMatch(c.pattern, c.s); got != c.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// An explicit Deny on ".../secret/*" must cover a resource whose name itself
// contains a literal "*" — before the fix the Allow won.
func TestDenyCoversResourceWithLiteralStar(t *testing.T) {
	p, err := ParsePolicy([]byte(`{
	  "Version":"1","Statement":[
	    {"Effect":"Allow","Action":"euoss:*","Resource":"*"},
	    {"Effect":"Deny","Action":"euoss:GetObject","Resource":"eu:oss:*:100:bucket/secret/*"}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	d := Evaluate([]Policy{p}, Request{Action: "euoss:GetObject", Resource: "eu:oss:cn-north-1:100:bucket/secret/*x"})
	if d.Allow {
		t.Fatal("Deny on bucket/secret/* did not cover bucket/secret/*x")
	}
}

// Regression (I4): with an unknown operator next to a known one that does not
// hold, the verdict depended on map iteration order. A Deny must match (deny
// wins on doubt) and an Allow must not, on every evaluation.
func TestUnknownConditionOperatorIsDeterministic(t *testing.T) {
	deny, err := ParsePolicy([]byte(`{
	  "Version":"1","Statement":[
	    {"Effect":"Allow","Action":"*","Resource":"*"},
	    {"Effect":"Deny","Action":"euecs:DeleteInstance","Resource":"*",
	     "Condition":{
	       "NumericLessThan":{"eu:RiskScore":["10"]},
	       "StringEquals":{"eu:ResourceGroupId":["rg-never"]},
	       "Bool":{"eu:MFAPresent":["true"]},
	       "IpAddress":{"eu:SourceIp":["192.0.2.0/24"]}
	     }}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	allow, err := ParsePolicy([]byte(`{
	  "Version":"1","Statement":[
	    {"Effect":"Allow","Action":"*","Resource":"*",
	     "Condition":{
	       "NumericLessThan":{"eu:RiskScore":["10"]},
	       "StringEquals":{"eu:ResourceGroupId":["rg-1"]},
	       "Bool":{"eu:MFAPresent":["true"]}
	     }}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Action:   "euecs:DeleteInstance",
		Resource: "eu:ecs:cn-north-1:100:instance/i-1",
		Context:  map[string]string{"eu:ResourceGroupId": "rg-1", "eu:MFAPresent": "true", "eu:SourceIp": "10.0.0.1"},
	}
	for i := 0; i < 500; i++ {
		if d := Evaluate([]Policy{deny}, req); d.Allow {
			t.Fatalf("iteration %d: Deny with an unknown operator was switched off", i)
		}
		if d := Evaluate([]Policy{allow}, req); d.Allow {
			t.Fatalf("iteration %d: Allow with an unknown operator granted", i)
		}
		if !matchConditions(deny.Statement[1].Condition, req.Context, EffectDeny) {
			t.Fatalf("iteration %d: matchConditions(Deny) = false", i)
		}
	}
}
