// Package trial implements the free-trial admission policy — the 限额防薅羊毛
// gate on 免费试用 (09-roadmap §4.4 B7, 01-product-catalog.md §10 trial
// parameters).
//
// A trial is a voucher claimed through the standard order flow (decision D7),
// which is exactly why the claim needs a gate: the voucher costs the platform
// real money and the marginal cost of a fresh account is an email address.
// Without admission control, 免费试用 becomes an unlimited subsidy payable to
// anyone willing to register again.
//
// # Deny-first, rule named
//
// The evaluation mirrors pkg-go/authz: every rule is checked in a fixed order,
// the first violation decides, and the verdict NAMES the deciding rule — an
// operator denied at the gate sees TRIAL.IDENTITY_LIMIT, not "claim rejected".
// Unset numeric limits are literal (zero admits nobody), so a misconfigured
// policy fails closed rather than opening the subsidy faucet.
//
// The engine is pure: the service supplies the account's trial history, the
// platform-wide active count, and the identity registry (实名 doc → claims).
// Recording a claim updates those counters transactionally in the service
// (t_trial_record, svc-catalog V3 DDL); this package only decides.
package trial

import "time"

// VoucherValidDays is the trial voucher's validity window after claim
// (01§10 试用参数: 领取后 30 天内使用) — the value t_coupon.expire_at is set to.
const VoucherValidDays = 30

// Rule identifies the deciding admission rule. The string is the stable wire
// code surfaced to the operator and the audit log.
type Rule string

const (
	RuleRealName     Rule = "TRIAL.REALNAME_REQUIRED"
	RuleActiveLimit  Rule = "TRIAL.ACTIVE_LIMIT"
	RuleLifetimeLimit Rule = "TRIAL.LIFETIME_LIMIT"
	RuleIdentityLimit Rule = "TRIAL.IDENTITY_LIMIT"
	RuleCooldown     Rule = "TRIAL.COOLDOWN"
	RuleGlobalBudget Rule = "TRIAL.GLOBAL_BUDGET"
)

// Policy is the anti-abuse configuration (B7). Zero numeric limits are
// literal — a zero limit admits nobody — so the zero-value Policy fails
// closed; DefaultPolicy is the platform baseline.
type Policy struct {
	// RequireRealName: a claim requires a verified 实名 first. The identity
	// dedup below is meaningless without it.
	RequireRealName bool
	// MaxActivePerAccount: concurrent ACTIVE trials per account. One is the
	// norm — a second concurrent trial is a re-claim while the first still
	// runs.
	MaxActivePerAccount int
	// MaxLifetimePerAccount: total trials the account may ever claim. One
	// per account, forever.
	MaxLifetimePerAccount int
	// MaxPerIdentity: total claims per 实名 identity ACROSS accounts — the
	// rule that stops "register N accounts, claim N vouchers" with one ID
	// card. Keyed by the caller's identity key (hash of the doc number), so
	// the engine never sees the raw document.
	MaxPerIdentity int
	// Cooldown is the minimum gap between claims on the same account. Zero
	// means no cooldown (time windows are the one limit where zero is a
	// natural "off").
	Cooldown time.Duration
	// MaxActiveGlobal caps platform-wide concurrent active trials — the
	// activity's budget expressed in live vouchers rather than yuan, so the
	// gate needs no money arithmetic to enforce it.
	MaxActiveGlobal int64
}

// DefaultPolicy is the platform baseline (01§10 trial parameters): real-name
// required, one active and one lifetime trial per account, one claim per
// identity, 30-day cooldown, 10 000 concurrent vouchers platform-wide.
func DefaultPolicy() Policy {
	return Policy{
		RequireRealName:      true,
		MaxActivePerAccount:  1,
		MaxLifetimePerAccount: 1,
		MaxPerIdentity:       1,
		Cooldown:             30 * 24 * time.Hour,
		MaxActiveGlobal:      10_000,
	}
}

// AccountState is the claiming account's trial history. The service reads it
// from t_trial_record (svc-catalog V3 DDL); the engine never mutates it.
type AccountState struct {
	// Active is the account's currently-active trial count (claimed, not yet
	// consumed/expired).
	Active int
	// Lifetime is the account's all-time claim count.
	Lifetime int
	// LastClaimedAt is the most recent claim instant; zero = never.
	LastClaimedAt time.Time
	// RealNameVerified mirrors account real-name status (0 未实名).
	RealNameVerified bool
	// IdentityKey is the caller's stable key for the account's 实名 identity
	// (hash of the doc/credit number). Empty when unverified.
	IdentityKey string
}

// State bundles everything Admit needs beyond the policy.
type State struct {
	Account AccountState
	// GlobalActive is the platform-wide count of active trials.
	GlobalActive int64
	// IdentityCounts maps identityKey → total claims under that identity,
	// across all accounts.
	IdentityCounts map[string]int64
}

// Verdict is the admission decision.
type Verdict struct {
	Allowed bool
	// Rule is the deciding rule on denial; empty when allowed.
	Rule Rule
	// Detail is the human-facing explanation (operator console, audit log).
	Detail string
}

// Admit evaluates a trial claim. Rules run in a fixed order — real-name first
// (an unverified claimant is rejected before any counter is even consulted),
// then the account-scoped limits, identity dedup, cooldown, and the global
// budget last. The first violation decides and names its rule.
func Admit(p Policy, s State, now time.Time) Verdict {
	if p.RequireRealName && !s.Account.RealNameVerified {
		return Verdict{Rule: RuleRealName,
			Detail: "免费试用要求先完成实名认证"}
	}
	if s.Account.Active >= p.MaxActivePerAccount {
		return Verdict{Rule: RuleActiveLimit,
			Detail: "账号存在未结束的试用,不可重复领取"}
	}
	if s.Account.Lifetime >= p.MaxLifetimePerAccount {
		return Verdict{Rule: RuleLifetimeLimit,
			Detail: "账号累计试用次数已达上限"}
	}
	// Identity dedup applies only when the account carries an identity key; a
	// verified account without one is a data bug the caller owns — the rule
	// stays silent rather than denying on a field it cannot judge.
	if s.Account.IdentityKey != "" {
		if s.IdentityCounts[s.Account.IdentityKey] >= int64(p.MaxPerIdentity) {
			return Verdict{Rule: RuleIdentityLimit,
				Detail: "该实名身份已领取过试用,不可跨账号重复领取"}
		}
	}
	if p.Cooldown > 0 && !s.Account.LastClaimedAt.IsZero() &&
		now.Sub(s.Account.LastClaimedAt) < p.Cooldown {
		return Verdict{Rule: RuleCooldown,
			Detail: "距上次领取未满冷静期,暂不可再次领取"}
	}
	if s.GlobalActive >= p.MaxActiveGlobal {
		return Verdict{Rule: RuleGlobalBudget,
			Detail: "试用名额已发放完毕"}
	}
	return Verdict{Allowed: true}
}
