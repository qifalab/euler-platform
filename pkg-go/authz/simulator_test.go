package authz

import "testing"

func simPolicy(t *testing.T, name, doc string) NamedPolicy {
	t.Helper()
	return NamedPolicy{Name: name, Policy: mustPolicy(t, doc)}
}

func TestSimulateNoPoliciesIsDefaultDeny(t *testing.T) {
	// No policy set at all → the simulator reports no_policy (distinct from
	// having policies that simply did not match, which is default_deny).
	v := Simulate(nil, "alice", "euecs:StartInstance", "eu:ecs:cn-north-1:100:instance/i-1")
	if v.Allowed {
		t.Fatal("empty policy set must deny")
	}
	if v.Reason != "no_policy" {
		t.Fatalf("reason = %q, want no_policy", v.Reason)
	}
	if v.DecidingStatement != -1 || v.DecidingPolicy != "" {
		t.Fatalf("default-deny verdict must not name a deciding statement: %+v", v)
	}
	if v.Principal != "alice" {
		t.Fatalf("principal not echoed: %q", v.Principal)
	}
}

func TestSimulateMatchingAllow(t *testing.T) {
	allow := simPolicy(t, "EuEcsFullAccess", `{
      "Version":"1",
      "Statement":[{"Effect":"Allow","Action":"euecs:*","Resource":"*"}]
    }`)
	v := Simulate([]NamedPolicy{allow}, "bob", "euecs:StartInstance", "eu:ecs:cn-north-1:100:instance/i-1")
	if !v.Allowed {
		t.Fatalf("matching allow must permit: %+v", v)
	}
	if v.Reason != "allow" {
		t.Fatalf("reason = %q, want allow", v.Reason)
	}
	if v.DecidingStatement != 0 {
		t.Fatalf("DecidingStatement = %d, want 0", v.DecidingStatement)
	}
	if v.DecidingPolicy != "EuEcsFullAccess" {
		t.Fatalf("DecidingPolicy = %q, want EuEcsFullAccess", v.DecidingPolicy)
	}
}

func TestSimulateExplicitDenyWinsOverAllow(t *testing.T) {
	// The Deny-first guarantee (07§3.3): even with a matching Allow, a
	// matching explicit Deny decides — and the verdict names the DENY
	// statement as the deciding one, not the allow.
	allow := simPolicy(t, "EuEcsFullAccess", `{
      "Version":"1","Statement":[{"Effect":"Allow","Action":"euecs:*","Resource":"*"}]
    }`)
	deny := simPolicy(t, "DenyDelete", `{
      "Version":"1","Statement":[{"Effect":"Deny","Action":"euecs:DeleteInstance","Resource":"*"}]
    }`)
	v := Simulate([]NamedPolicy{allow, deny}, "carol", "euecs:DeleteInstance", "eu:ecs:cn-north-1:100:instance/i-1")
	if v.Allowed {
		t.Fatal("explicit Deny must win over Allow")
	}
	if v.Reason != "explicit_deny" {
		t.Fatalf("reason = %q, want explicit_deny", v.Reason)
	}
	if v.DecidingPolicy != "DenyDelete" {
		t.Fatalf("DecidingPolicy = %q, want DenyDelete (the deny)", v.DecidingPolicy)
	}
	if v.DecidingStatement != 0 {
		t.Fatalf("DecidingStatement = %d, want 0", v.DecidingStatement)
	}
	// Deny wins regardless of policy order.
	v2 := Simulate([]NamedPolicy{deny, allow}, "carol", "euecs:DeleteInstance", "eu:ecs:cn-north-1:100:instance/i-1")
	if v2.Allowed || v2.Reason != "explicit_deny" {
		t.Fatalf("deny must win regardless of order: %+v", v2)
	}
	// A non-denied action still allows.
	v3 := Simulate([]NamedPolicy{allow, deny}, "carol", "euecs:StartInstance", "eu:ecs:cn-north-1:100:instance/i-1")
	if !v3.Allowed || v3.Reason != "allow" {
		t.Fatalf("non-denied action should allow: %+v", v3)
	}
}

func TestSimulateNonMatchingStatementIsDefaultDeny(t *testing.T) {
	// A statement that does not match the action neither allows nor denies.
	p := simPolicy(t, "ReadOnly", `{
      "Version":"1","Statement":[{"Effect":"Allow","Action":"euecs:Describe*","Resource":"*"}]
    }`)
	v := Simulate([]NamedPolicy{p}, "dan", "euecs:DeleteInstance", "eu:ecs:cn-north-1:100:instance/i-1")
	if v.Allowed {
		t.Fatal("non-matching statement must default-deny")
	}
	if v.Reason != "default_deny" {
		t.Fatalf("reason = %q, want default_deny", v.Reason)
	}
	if v.DecidingStatement != -1 || v.DecidingPolicy != "" {
		t.Fatalf("default-deny must not name a deciding statement: %+v", v)
	}
}

func TestSimulateWildcardActionMatchesAny(t *testing.T) {
	p := simPolicy(t, "Admin", `{
      "Version":"1","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]
    }`)
	for _, act := range []string{"euecs:StartInstance", "euoss:PutObject", "anything:Whatever"} {
		v := Simulate([]NamedPolicy{p}, "eve", act, "eu:ecs:cn-north-1:100:x")
		if !v.Allowed {
			t.Errorf("wildcard action * must allow %q: %+v", act, v)
		}
	}
}

func TestSimulateWildcardResourcePrefix(t *testing.T) {
	// ecs:* as a resource is NOT a 5-segment ARN. The engine deliberately does
	// not fall back to a whole-string prefix match for short patterns: a
	// truncated pattern would glob across the account-id segment and grant
	// cross-account access. Short patterns therefore match nothing.
	p := simPolicy(t, "EcsScoped", `{
      "Version":"1","Statement":[{"Effect":"Allow","Action":"euecs:*","Resource":"ecs:*"}]
    }`)
	v := Simulate([]NamedPolicy{p}, "frank", "euecs:StartInstance", "ecs:instance/i-123")
	if v.Allowed {
		t.Fatalf("truncated resource pattern ecs:* must not match anything: %+v", v)
	}
	v2 := Simulate([]NamedPolicy{p}, "frank", "euecs:StartInstance", "oss:bucket/x")
	if v2.Allowed {
		t.Fatal("resource ecs:* must not match oss:bucket/x")
	}
	// A full 5-segment pattern still wildcards within its own account.
	p2 := simPolicy(t, "ArnScoped", `{
      "Version":"1","Statement":[{"Effect":"Allow","Action":"euecs:*","Resource":"eu:ecs:*:100:instance/*"}]
    }`)
	v3 := Simulate([]NamedPolicy{p2}, "frank", "euecs:StartInstance", "eu:ecs:cn-north-1:100:instance/i-123")
	if !v3.Allowed {
		t.Fatalf("full ARN pattern should match same-account resource: %+v", v3)
	}
	v4 := Simulate([]NamedPolicy{p2}, "frank", "euecs:StartInstance", "eu:ecs:cn-north-1:200:instance/i-123")
	if v4.Allowed {
		t.Fatal("full ARN pattern must not match another account's resource")
	}
}

func TestSimulateDecidingStatementIndexCorrect(t *testing.T) {
	// A policy with several statements: the deciding one's index is reported.
	p := simPolicy(t, "Multi", `{
      "Version":"1","Statement":[
        {"Effect":"Allow","Action":"euecs:Describe*","Resource":"*"},
        {"Effect":"Allow","Action":"euecs:StartInstance","Resource":"*"},
        {"Effect":"Deny","Action":"euecs:DeleteInstance","Resource":"*"}
      ]
    }`)
	// StartInstance matches statement 1 (index 1).
	v := Simulate([]NamedPolicy{p}, "gina", "euecs:StartInstance", "eu:ecs:cn-north-1:100:i/1")
	if !v.Allowed || v.DecidingStatement != 1 || v.DecidingPolicy != "Multi" {
		t.Fatalf("StartInstance should be decided by statement 1: %+v", v)
	}
	// DeleteInstance matches the Deny at index 2.
	v2 := Simulate([]NamedPolicy{p}, "gina", "euecs:DeleteInstance", "eu:ecs:cn-north-1:100:i/1")
	if v2.DecidingStatement != 2 || v2.Reason != "explicit_deny" {
		t.Fatalf("DeleteInstance should be denied by statement 2: %+v", v2)
	}
}
