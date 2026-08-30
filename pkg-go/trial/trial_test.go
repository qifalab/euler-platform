package trial

import (
	"testing"
	"time"
)

var now = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func verifiedAccount() AccountState {
	return AccountState{
		RealNameVerified: true,
		IdentityKey:      "idn-8f3a",
	}
}

func cleanState() State {
	return State{
		Account:        verifiedAccount(),
		GlobalActive:   100,
		IdentityCounts: map[string]int64{"idn-8f3a": 0},
	}
}

// --- Happy path ---

func TestAdmitAllowsCleanVerifiedAccount(t *testing.T) {
	v := Admit(DefaultPolicy(), cleanState(), now)
	if !v.Allowed {
		t.Fatalf("clean verified account denied: %+v", v)
	}
	if v.Rule != "" || v.Detail != "" {
		t.Errorf("allowed verdict should carry no rule/detail, got %q / %q", v.Rule, v.Detail)
	}
}

// --- Each rule fires and names itself ---

func TestRealNameRequired(t *testing.T) {
	s := cleanState()
	s.Account.RealNameVerified = false
	s.Account.IdentityKey = ""
	v := Admit(DefaultPolicy(), s, now)
	if v.Allowed || v.Rule != RuleRealName {
		t.Fatalf("want REALNAME denial, got %+v", v)
	}
	if v.Detail == "" {
		t.Error("denial must carry an operator-facing detail")
	}
}

func TestRealNameNotRequiredWhenPolicyDisablesIt(t *testing.T) {
	p := DefaultPolicy()
	p.RequireRealName = false
	s := cleanState()
	s.Account.RealNameVerified = false
	s.Account.IdentityKey = ""
	v := Admit(p, s, now)
	if !v.Allowed {
		t.Fatalf("policy without real-name requirement denied: %+v", v)
	}
}

func TestActiveLimitBlocksReclaim(t *testing.T) {
	s := cleanState()
	s.Account.Active = 1 // the one allowed concurrent trial is already running
	v := Admit(DefaultPolicy(), s, now)
	if v.Allowed || v.Rule != RuleActiveLimit {
		t.Fatalf("want ACTIVE_LIMIT denial, got %+v", v)
	}
}

func TestLifetimeLimitBlocksForeverAfterOneClaim(t *testing.T) {
	s := cleanState()
	s.Account.Lifetime = 1 // claimed once, consumed — still lifetime-capped
	v := Admit(DefaultPolicy(), s, now)
	if v.Allowed || v.Rule != RuleLifetimeLimit {
		t.Fatalf("want LIFETIME_LIMIT denial, got %+v", v)
	}
}

func TestIdentityLimitBlocksCrossAccountReclaim(t *testing.T) {
	// The core anti-薅 rule: a fresh account, clean on every account-scoped
	// counter, but the same 实名 identity already claimed through another
	// account. Only the identity registry can stop this.
	s := cleanState()
	s.IdentityCounts["idn-8f3a"] = 1
	v := Admit(DefaultPolicy(), s, now)
	if v.Allowed || v.Rule != RuleIdentityLimit {
		t.Fatalf("want IDENTITY_LIMIT denial, got %+v", v)
	}
}

func TestIdentityLimitIgnoresEmptyIdentityKey(t *testing.T) {
	// A verified account with no identity key is a caller data bug; the rule
	// stays silent rather than denying on a field it cannot judge.
	s := cleanState()
	s.Account.IdentityKey = ""
	v := Admit(DefaultPolicy(), s, now)
	if !v.Allowed {
		t.Fatalf("empty identity key should not deny: %+v", v)
	}
}

func TestCooldownBlocksTooSoonReclaim(t *testing.T) {
	s := cleanState()
	s.Account.LastClaimedAt = now.Add(-29 * 24 * time.Hour) // 30-day cooldown, 1 day short
	v := Admit(DefaultPolicy(), s, now)
	if v.Allowed || v.Rule != RuleCooldown {
		t.Fatalf("want COOLDOWN denial, got %+v", v)
	}
}

func TestCooldownPassesAtExactBoundary(t *testing.T) {
	s := cleanState()
	s.Account.LastClaimedAt = now.Add(-30 * 24 * time.Hour) // exactly the cooldown
	v := Admit(DefaultPolicy(), s, now)
	if !v.Allowed {
		t.Fatalf("claim at the exact cooldown boundary should pass: %+v", v)
	}
}

func TestGlobalBudgetExhausted(t *testing.T) {
	s := cleanState()
	s.GlobalActive = 10_000
	v := Admit(DefaultPolicy(), s, now)
	if v.Allowed || v.Rule != RuleGlobalBudget {
		t.Fatalf("want GLOBAL_BUDGET denial, got %+v", v)
	}
}

// --- Fixed rule order: the first violation decides ---

func TestRuleOrderFirstViolationDecides(t *testing.T) {
	// Account trips real-name AND active AND lifetime at once; real-name runs
	// first, so the operator sees the actionable fix (完成实名认证), not the
	// counter state that real-name would unlock anyway.
	s := State{
		Account: AccountState{
			Active:           2,
			Lifetime:         5,
			RealNameVerified: false,
		},
		GlobalActive: 20_000,
	}
	v := Admit(DefaultPolicy(), s, now)
	if v.Rule != RuleRealName {
		t.Fatalf("real-name must run first, got %q", v.Rule)
	}

	// Verified but tripping active + identity + cooldown + budget: active wins.
	s.Account = AccountState{
		Active:           1,
		Lifetime:         0,
		RealNameVerified: true,
		IdentityKey:      "idn-8f3a",
	}
	s.IdentityCounts = map[string]int64{"idn-8f3a": 1}
	s.Account.LastClaimedAt = now.Add(-time.Hour)
	s.GlobalActive = 20_000
	v = Admit(DefaultPolicy(), s, now)
	if v.Rule != RuleActiveLimit {
		t.Fatalf("active limit must run before identity/cooldown/budget, got %q", v.Rule)
	}

	// Identity before cooldown before budget.
	s.Account.Active = 0
	v = Admit(DefaultPolicy(), s, now)
	if v.Rule != RuleIdentityLimit {
		t.Fatalf("identity limit must run before cooldown/budget, got %q", v.Rule)
	}

	s.IdentityCounts["idn-8f3a"] = 0
	v = Admit(DefaultPolicy(), s, now)
	if v.Rule != RuleCooldown {
		t.Fatalf("cooldown must run before budget, got %q", v.Rule)
	}

	s.Account.LastClaimedAt = time.Time{}
	v = Admit(DefaultPolicy(), s, now)
	if v.Rule != RuleGlobalBudget {
		t.Fatalf("global budget is the last rule, got %q", v.Rule)
	}
}

// --- Fail closed ---

func TestZeroPolicyAdmitsNobody(t *testing.T) {
	// The zero-value Policy has zero numeric limits, which are literal: nobody
	// passes. A misconfigured policy fails closed, not open.
	v := Admit(Policy{}, cleanState(), now)
	if v.Allowed {
		t.Fatal("zero-value policy must fail closed")
	}
	// The first literal zero the engine hits is MaxActivePerAccount.
	if v.Rule != RuleActiveLimit {
		t.Errorf("zero policy should deny at ACTIVE_LIMIT, got %q", v.Rule)
	}
}

func TestZeroCooldownIsOffNotLiteral(t *testing.T) {
	// Cooldown is the one limit where zero naturally means "no cooldown".
	p := DefaultPolicy()
	p.Cooldown = 0
	s := cleanState()
	s.Account.LastClaimedAt = now.Add(-time.Minute)
	v := Admit(p, s, now)
	if !v.Allowed {
		t.Fatalf("zero cooldown should disable the rule: %+v", v)
	}
}

// --- Domain constants ---

func TestVoucherValidDaysMatchesCatalog(t *testing.T) {
	// 01§10: 领取后 30 天内使用. The service sets t_coupon.expire_at to
	// claimTime + VoucherValidDays; the constant must stay in sync with the doc.
	if VoucherValidDays != 30 {
		t.Errorf("VoucherValidDays = %d, want 30 (01§10)", VoucherValidDays)
	}
}
