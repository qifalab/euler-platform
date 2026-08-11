package authz

import (
	"encoding/json"
	"testing"
)

func mustPolicy(t *testing.T, doc string) Policy {
	t.Helper()
	p, err := ParsePolicy([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	return p
}

func TestDefaultDeny(t *testing.T) {
	// Rule 1: no statements → deny.
	d := Evaluate(nil, Request{Action: "scecs:StartInstance", Resource: "sc:ecs:cn-north-1:100:instance/x"})
	if d.Allow {
		t.Fatal("empty policy set must deny")
	}
	if d.DecisionNumber != "D-default-deny" {
		t.Fatalf("DecisionNumber = %q", d.DecisionNumber)
	}
}

func TestAllowMatches(t *testing.T) {
	p := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{"Effect":"Allow","Action":"scecs:StartInstance","Resource":"sc:ecs:cn-north-1:100:instance/i-1"}]
    }`)
	d := Evaluate([]Policy{p}, Request{
		Action:   "scecs:StartInstance",
		Resource: "sc:ecs:cn-north-1:100:instance/i-1",
	})
	if !d.Allow {
		t.Fatalf("expected allow, got %+v", d)
	}
}

func TestDenyWinsOverAllow(t *testing.T) {
	// Rule 2: explicit Deny beats a matching Allow regardless of order.
	allow := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{"Effect":"Allow","Action":"scecs:*","Resource":"*"}]
    }`)
	deny := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{"Effect":"Deny","Action":"scecs:DeleteInstance","Resource":"*"}]
    }`)
	req := Request{Action: "scecs:DeleteInstance", Resource: "sc:ecs:cn-north-1:100:instance/i-1"}

	if d := Evaluate([]Policy{allow, deny}, req); d.Allow {
		t.Fatal("Deny must win when listed after Allow")
	}
	if d := Evaluate([]Policy{deny, allow}, req); d.Allow {
		t.Fatal("Deny must win when listed before Allow")
	}
	// A different action is still allowed by the wildcard Allow.
	if d := Evaluate([]Policy{allow, deny}, Request{
		Action: "scecs:StartInstance", Resource: "sc:ecs:cn-north-1:100:instance/i-1",
	}); !d.Allow {
		t.Fatal("non-denied action should be allowed")
	}
}

func TestActionWildcards(t *testing.T) {
	p := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{"Effect":"Allow","Action":["scecs:Describe*","scoss:*"],"Resource":"*"}]
    }`)
	cases := []struct {
		action string
		want   bool
	}{
		{"scecs:DescribeInstances", true},
		{"scecs:DescribeImages", true},
		{"scecs:StartInstance", false},
		{"scoss:PutObject", true},
		{"scrds:DescribeDBInstances", false},
	}
	for _, c := range cases {
		got := Evaluate([]Policy{p}, Request{Action: c.action, Resource: "sc:ecs:cn-north-1:100:x"}).Allow
		if got != c.want {
			t.Errorf("action %q: allow=%v want %v", c.action, got, c.want)
		}
	}
}

func TestResourceARNWildcards(t *testing.T) {
	// The ARN form from 07§3.1: sc:iam:*:1001234567890:user/dev-*
	p := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{"Effect":"Allow","Action":"sciam:GetUser","Resource":"sc:iam:*:1001234567890:user/dev-*"}]
    }`)
	cases := []struct {
		resource string
		want     bool
	}{
		{"sc:iam:cn-north-1:1001234567890:user/dev-alice", true},
		{"sc:iam:cn-east-1:1001234567890:user/dev-bob", true},
		{"sc:iam:cn-north-1:1001234567890:user/prod-carol", false},
		{"sc:iam:cn-north-1:9999999999999:user/dev-alice", false}, // different account
		{"sc:ecs:cn-north-1:1001234567890:user/dev-alice", false}, // different service
	}
	for _, c := range cases {
		got := Evaluate([]Policy{p}, Request{Action: "sciam:GetUser", Resource: c.resource}).Allow
		if got != c.want {
			t.Errorf("resource %q: allow=%v want %v", c.resource, got, c.want)
		}
	}
}

func TestConditionIpAddress(t *testing.T) {
	p := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{
        "Effect":"Allow","Action":"scecs:*","Resource":"*",
        "Condition":{"IpAddress":{"sc:SourceIp":["10.0.0.0/8"]}}
      }]
    }`)
	in := Evaluate([]Policy{p}, Request{
		Action: "scecs:StartInstance", Resource: "sc:ecs:cn-north-1:100:i/1",
		Context: map[string]string{"sc:SourceIp": "10.1.2.3"},
	})
	if !in.Allow {
		t.Fatal("in-range IP should be allowed")
	}
	out := Evaluate([]Policy{p}, Request{
		Action: "scecs:StartInstance", Resource: "sc:ecs:cn-north-1:100:i/1",
		Context: map[string]string{"sc:SourceIp": "203.0.113.9"},
	})
	if out.Allow {
		t.Fatal("out-of-range IP must be denied")
	}
	// Missing context key → condition fails → default deny.
	missing := Evaluate([]Policy{p}, Request{
		Action: "scecs:StartInstance", Resource: "sc:ecs:cn-north-1:100:i/1",
	})
	if missing.Allow {
		t.Fatal("missing SourceIp must deny")
	}
}

func TestConditionNotIpAddress(t *testing.T) {
	p := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{
        "Effect":"Deny","Action":"*","Resource":"*",
        "Condition":{"NotIpAddress":{"sc:SourceIp":["203.0.113.0/24"]}}
      }]
    }`)
	// Outside the allowed range → NotIpAddress true → Deny applies.
	d := Evaluate([]Policy{p}, Request{
		Action: "scecs:StartInstance", Resource: "sc:ecs:cn-north-1:100:i/1",
		Context: map[string]string{"sc:SourceIp": "10.1.2.3"},
	})
	if d.Allow {
		t.Fatal("expected deny for IP outside the permitted range")
	}
}

func TestConditionDateLessThan(t *testing.T) {
	p := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{
        "Effect":"Allow","Action":"scecs:*","Resource":"*",
        "Condition":{"DateLessThan":{"sc:CurrentTime":"2027-01-01T00:00:00Z"}}
      }]
    }`)
	before := Evaluate([]Policy{p}, Request{
		Action: "scecs:StartInstance", Resource: "x",
		Context: map[string]string{"sc:CurrentTime": "2026-08-04T09:30:00Z"},
	})
	if !before.Allow {
		t.Fatal("time before the limit should be allowed")
	}
	after := Evaluate([]Policy{p}, Request{
		Action: "scecs:StartInstance", Resource: "x",
		Context: map[string]string{"sc:CurrentTime": "2027-06-01T00:00:00Z"},
	})
	if after.Allow {
		t.Fatal("time after the limit must be denied")
	}
}

func TestConditionBoolMFA(t *testing.T) {
	p := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{
        "Effect":"Allow","Action":"scecs:DeleteInstance","Resource":"*",
        "Condition":{"Bool":{"sc:MFAPresent":"true"}}
      }]
    }`)
	with := Evaluate([]Policy{p}, Request{
		Action: "scecs:DeleteInstance", Resource: "x",
		Context: map[string]string{"sc:MFAPresent": "true"},
	})
	if !with.Allow {
		t.Fatal("MFA present should allow")
	}
	without := Evaluate([]Policy{p}, Request{
		Action: "scecs:DeleteInstance", Resource: "x",
		Context: map[string]string{"sc:MFAPresent": "false"},
	})
	if without.Allow {
		t.Fatal("MFA absent must deny high-risk action")
	}
}

func TestUnknownOperatorFailsClosed(t *testing.T) {
	// A policy using an operator this engine does not implement must never
	// silently grant access.
	p := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{
        "Effect":"Allow","Action":"*","Resource":"*",
        "Condition":{"StringLike":{"sc:Whatever":"x"}}
      }]
    }`)
	d := Evaluate([]Policy{p}, Request{
		Action: "scecs:StartInstance", Resource: "x",
		Context: map[string]string{"sc:Whatever": "x"},
	})
	if d.Allow {
		t.Fatal("unknown condition operator must fail closed (deny)")
	}
}

func TestFlexListAcceptsStringOrArray(t *testing.T) {
	// Both the compact and array forms must parse.
	single := mustPolicy(t, `{"Version":"1","Statement":[{"Effect":"Allow","Action":"a:B","Resource":"*"}]}`)
	if len(single.Statement[0].Action) != 1 || single.Statement[0].Action[0] != "a:B" {
		t.Fatalf("single-string Action parsed wrong: %+v", single.Statement[0].Action)
	}
	multi := mustPolicy(t, `{"Version":"1","Statement":[{"Effect":"Allow","Action":["a:B","c:D"],"Resource":["*"]}]}`)
	if len(multi.Statement[0].Action) != 2 {
		t.Fatalf("array Action parsed wrong: %+v", multi.Statement[0].Action)
	}
	// Round-trip: one element marshals back to a bare string.
	out, err := json.Marshal(single.Statement[0].Action)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"a:B"` {
		t.Fatalf("single-element marshal = %s, want \"a:B\"", out)
	}
}

func TestParsePolicyRejectsBadEffect(t *testing.T) {
	if _, err := ParsePolicy([]byte(`{"Version":"1","Statement":[{"Effect":"Maybe","Action":"*","Resource":"*"}]}`)); err == nil {
		t.Fatal("invalid Effect must be rejected")
	}
	if _, err := ParsePolicy([]byte(`{"Statement":[]}`)); err == nil {
		t.Fatal("missing Version must be rejected")
	}
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "anything", true},
		{"scecs:*", "scecs:StartInstance", true},
		{"scecs:*", "scoss:PutObject", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"dev-*", "dev-alice", true},
		{"dev-*", "prod-alice", false},
		{"*-suffix", "any-suffix", true},
		{"", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := wildcardMatch(c.pattern, c.s); got != c.want {
			t.Errorf("wildcardMatch(%q,%q) = %v want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestSystemPolicyShape(t *testing.T) {
	// The platform auto-generates ScECSFullAccess / ScECSReadOnlyAccess from
	// each product's API registration (07§3.2). Verify the read-only shape
	// denies writes.
	readOnly := mustPolicy(t, `{
      "Version":"1",
      "Statement":[{"Effect":"Allow","Action":["scecs:Describe*","scecs:List*"],"Resource":"*"}]
    }`)
	if !Evaluate([]Policy{readOnly}, Request{Action: "scecs:DescribeInstances", Resource: "x"}).Allow {
		t.Fatal("read-only policy should allow Describe")
	}
	if Evaluate([]Policy{readOnly}, Request{Action: "scecs:DeleteInstance", Resource: "x"}).Allow {
		t.Fatal("read-only policy must not allow Delete")
	}
}
