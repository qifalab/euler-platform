// Package pricing implements the platform's price calculation engine
// (01-product-catalog.md §7, 03-backend-services.md §4.2.1).
//
// Calculation order is fixed by 01§12.3 and may not be overridden by any
// individual product:
//
//	① 目录价   list price, selected by (sku, region, duration tier, customer level)
//	② 促销折扣  best single promotion — promotions never stack
//	③ 代金券   voucher pre-deduction, shown at quote time
//
// Deduction order at payment time (01§12.3, §5.3):
//
//	资源包额度 → 代金券(按到期时间升序) → 现金余额 → 三方支付
//
// Phase-1 sells only 包年包月 (prepaid) and 按量 (postpaid); 资源包 and 抢占式
// are deferred to phase 2 (decision D6). The ChargeType enum carries the
// deferred values so the order model does not need a schema change later —
// "模型 Day1 预留、实现按需" (roadmap principle P3).
//
// # Money is never a float
//
// Every amount in this package is a decimal fixed-point value carried as an
// integer number of micro-units (1/1_000_000 of the currency unit). Binary
// floating point cannot represent 0.1 exactly, so a float pipeline drifts by
// fractions of a cent per operation; across millions of hourly billing rows
// that becomes a real reconciliation gap, and 09 A2 requires 无未解释差异.
// Storage is DECIMAL (03§6 约定); this package is the in-memory counterpart.
package pricing

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Scale is the number of decimal places carried internally. Six places matches
// the DECIMAL(18,6) quantity columns in metering_record and leaves room for
// per-second unit prices without rounding at intermediate steps.
const Scale = 6

// scaleFactor is 10^Scale.
const scaleFactor = 1_000_000

// Amount is a fixed-point monetary value in micro-units.
//
// The zero value is a valid zero amount. Amounts are always exact: addition,
// subtraction and comparison are exact by construction, and multiplication by
// a quantity rounds explicitly (see Mul) rather than silently.
type Amount int64

// ParseAmount converts a decimal string such as "180.50" into an Amount.
// It is deliberately strict: a malformed price in a pricing rule must fail
// loudly at load time rather than silently becoming zero and giving a product
// away for free.
func ParseAmount(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("pricing: empty amount")
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	} else if s[0] == '+' {
		s = s[1:]
	}
	if s == "" {
		return 0, errors.New("pricing: amount has sign but no digits")
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if intPart == "" && !hasFrac {
		return 0, fmt.Errorf("pricing: malformed amount %q", s)
	}
	if strings.Contains(fracPart, ".") {
		return 0, fmt.Errorf("pricing: malformed amount %q (multiple decimal points)", s)
	}
	if len(fracPart) > Scale {
		return 0, fmt.Errorf("pricing: amount %q exceeds %d decimal places", s, Scale)
	}

	var units int64
	for i := 0; i < len(intPart); i++ {
		c := intPart[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("pricing: malformed amount %q", s)
		}
		next := units*10 + int64(c-'0')
		if next < units {
			return 0, fmt.Errorf("pricing: amount %q overflows", s)
		}
		units = next
	}

	frac := int64(0)
	for i := 0; i < Scale; i++ {
		frac *= 10
		if i < len(fracPart) {
			c := fracPart[i]
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("pricing: malformed amount %q", s)
			}
			frac += int64(c - '0')
		}
	}

	total := units*scaleFactor + frac
	if total < 0 {
		return 0, fmt.Errorf("pricing: amount %q overflows", s)
	}
	if neg {
		total = -total
	}
	return Amount(total), nil
}

// MustParseAmount is ParseAmount for constants known to be well-formed.
func MustParseAmount(s string) Amount {
	a, err := ParseAmount(s)
	if err != nil {
		panic(err)
	}
	return a
}

// String renders the amount as a decimal string with trailing zeros trimmed,
// e.g. "180.5". This is the form written to price_snapshot.detail_json and
// returned by the 询价 API.
func (a Amount) String() string {
	neg := a < 0
	v := int64(a)
	if neg {
		v = -v
	}
	units := v / scaleFactor
	frac := v % scaleFactor

	out := fmt.Sprintf("%d", units)
	if frac != 0 {
		fracStr := fmt.Sprintf("%06d", frac)
		fracStr = strings.TrimRight(fracStr, "0")
		out += "." + fracStr
	}
	if neg {
		out = "-" + out
	}
	return out
}

// Add returns a + b.
func (a Amount) Add(b Amount) Amount { return a + b }

// Sub returns a - b.
func (a Amount) Sub(b Amount) Amount { return a - b }

// IsZero reports whether the amount is exactly zero.
func (a Amount) IsZero() bool { return a == 0 }

// IsNegative reports whether the amount is below zero.
func (a Amount) IsNegative() bool { return a < 0 }

// Mul multiplies by an integer quantity. Exact — no rounding needed.
func (a Amount) Mul(qty int64) Amount { return Amount(int64(a) * qty) }

// MulRate multiplies by a rate expressed in basis points (1/10000), rounding
// half-up at the last carried decimal place.
//
// Rounding is half-up rather than banker's rounding because that is what
// customers and finance teams expect on an invoice line, and consistency with
// the printed bill matters more here than statistical neutrality.
//
// Use MulDiv instead when the rate is a ratio of two integers (for example
// "11 of 12 months"): converting such a ratio to basis points first truncates
// it, and the lost precision is systematic rather than random.
func (a Amount) MulRate(basisPoints int64) Amount {
	product := int64(a) * basisPoints
	// Round half away from zero.
	if product >= 0 {
		return Amount((product + 5000) / 10000)
	}
	return Amount((product - 5000) / 10000)
}

// MulDiv returns a × num / den, rounding half away from zero, without an
// intermediate rate conversion.
//
// This exists because proration is a ratio of whole periods, and expressing
// such a ratio as basis points loses precision that no later rounding can
// recover: 11/12 becomes 9166 bp (truncated from 9166.67), so a 1200 refund
// pays 1099.92 instead of 1100. The error is small per transaction but always
// falls the same way — against whoever is receiving the money — so it is a
// systematic bias, not noise, and it accumulates across every refund.
//
// den must be non-zero; a zero denominator returns zero rather than panicking,
// since the caller's degenerate input should not take down a billing run.
func (a Amount) MulDiv(num, den int64) Amount {
	if den == 0 {
		return 0
	}
	product := int64(a) * num
	half := den / 2
	if (product < 0) != (den < 0) {
		// Result is negative: round away from zero.
		return Amount((product - half) / den)
	}
	return Amount((product + half) / den)
}

// Min returns the smaller of two amounts.
func Min(a, b Amount) Amount {
	if a < b {
		return a
	}
	return b
}

// ChargeType is the billing form (01§5.1, decision D6).
type ChargeType string

const (
	// ChargePrepaid is 包年包月 — phase-1 Day-1.
	ChargePrepaid ChargeType = "PREPAID"
	// ChargePostpaid is 按量付费, hourly settlement — phase-1 Day-1.
	ChargePostpaid ChargeType = "POSTPAID"
	// ChargeResourcePack is 资源包 — model reserved, sale deferred to phase 2.
	ChargeResourcePack ChargeType = "RESOURCE_PACK"
	// ChargeSpot is 抢占式 — model reserved, sale deferred to phase 2.
	ChargeSpot ChargeType = "SPOT"
)

// Sellable reports whether this charge type may be sold in the current phase.
// The order service and pricing engine call this so a not-yet-sold charge type
// cannot be ordered through an API that happens to accept the enum value.
//
// Phase 2 (09-roadmap M-4) opens 资源包 (M-4.1) and 抢占式 (M-4.2, once the spot
// price engine in pkg-go/spot and the reclaim path in pkg-go/provision land)
// for sale. All four billing forms are now sellable.
func (c ChargeType) Sellable() bool {
	return c == ChargePrepaid || c == ChargePostpaid || c == ChargeResourcePack || c == ChargeSpot
}

// SellableInPhase1 reports whether this charge type was sellable during phase 1
// (包年包月 + 按量 only, decision D6). Retained as the phase-1 historical record
// so the phase-1 gate semantics stay legible; new code uses Sellable.
func (c ChargeType) SellableInPhase1() bool {
	return c == ChargePrepaid || c == ChargePostpaid
}

// DurationUnit is the billing period unit for a pricing rule.
type DurationUnit string

const (
	DurationMonth  DurationUnit = "MONTH"
	DurationYear   DurationUnit = "YEAR"
	DurationHour   DurationUnit = "HOUR"
	DurationMinute DurationUnit = "MINUTE" // phase 2: spot/preemptible per-minute cycles
	DurationSecond DurationUnit = "SECOND" // phase 2: SCECI per-second metering (09 §4.2)
	DurationUsage  DurationUnit = "USAGE"  // metered, quantity supplied by 计量
)

// PricingRule is one row of t_pricing_rule. Rules are append-only: a price
// change adds a new row with a new effective range and never rewrites an
// existing one, so historical orders remain explicable (01§12.3 — 价格可追溯、
// 不可改写 是资损防控底线).
type PricingRule struct {
	RuleID        int64
	SKUCode       string
	RegionID      string // "*" matches any region
	DurationUnit  DurationUnit
	ListPrice     Amount
	CustomerLevel string // "NORMAL", or a tier name; "" means any
	EffectiveFrom time.Time
	EffectiveTo   time.Time // zero means open-ended
}

// active reports whether the rule is in force at t.
func (r PricingRule) active(t time.Time) bool {
	if !r.EffectiveFrom.IsZero() && t.Before(r.EffectiveFrom) {
		return false
	}
	if !r.EffectiveTo.IsZero() && !t.Before(r.EffectiveTo) {
		return false
	}
	return true
}

// Active is the exported form of active, so a service that needs to inspect
// rules outside the engine (svc-catalog deriving a quote's DurationUnit from
// the matched rule, rather than hardcoding it) can reuse the same selection
// logic the engine uses internally — one source of truth for "which rule
// matches this request".
func (r PricingRule) Active(t time.Time) bool { return r.active(t) }

// specificity scores how closely a rule targets the request. A region-specific
// rule beats a wildcard; a customer-level rule beats an unscoped one. This is
// what lets a single 目录价 coexist with regional and enterprise overrides
// without an explicit priority column.
func (r PricingRule) specificity() int {
	score := 0
	if r.RegionID != "*" && r.RegionID != "" {
		score += 2
	}
	if r.CustomerLevel != "" {
		score++
	}
	return score
}

// Specificity is the exported form of specificity, paired with Active so a
// service can find the most-specific active rule for a SKU without duplicating
// selectRule's internals.
func (r PricingRule) Specificity() int { return r.specificity() }

// PromoType is the promotion mechanism.
type PromoType string

const (
	// PromoDiscountRate applies a percentage discount, e.g. 8500 bp = 85折.
	PromoDiscountRate PromoType = "DISCOUNT_RATE"
	// PromoFixedPrice replaces the list price with a fixed 一口价.
	PromoFixedPrice PromoType = "FIXED_PRICE"
)

// Promotion is one row of t_promo_policy.
type Promotion struct {
	PromoID   string
	PromoType PromoType
	ScopeType string // "PRODUCT" | "SKU" | "ORDER"
	ScopeRef  string
	// Value is basis points for DISCOUNT_RATE, or an Amount for FIXED_PRICE.
	RateBasisPoints int64
	FixedPrice      Amount
	UserTag         string // "new", "enterprise", ...; "" matches everyone
	StartAt         time.Time
	EndAt           time.Time
}

func (p Promotion) active(t time.Time) bool {
	if !p.StartAt.IsZero() && t.Before(p.StartAt) {
		return false
	}
	if !p.EndAt.IsZero() && !t.Before(p.EndAt) {
		return false
	}
	return true
}

// applies reports whether the promotion targets this request.
func (p Promotion) applies(req Request) bool {
	if p.UserTag != "" && p.UserTag != req.UserTag {
		return false
	}
	switch p.ScopeType {
	case "SKU":
		return p.ScopeRef == req.SKUCode
	case "PRODUCT":
		return p.ScopeRef == req.ProductCode
	case "ORDER":
		return true
	default:
		return false
	}
}

// discountOn returns the amount this promotion takes off the given list total.
func (p Promotion) discountOn(list Amount) Amount {
	switch p.PromoType {
	case PromoDiscountRate:
		// RateBasisPoints is the price retained, e.g. 8500 = pay 85%.
		retained := list.MulRate(p.RateBasisPoints)
		d := list.Sub(retained)
		if d.IsNegative() {
			return 0
		}
		return d
	case PromoFixedPrice:
		d := list.Sub(p.FixedPrice)
		if d.IsNegative() {
			// A "promotion" that raises the price is a configuration error;
			// treat it as no discount rather than charging more than list.
			return 0
		}
		return d
	default:
		return 0
	}
}

// Coupon is one row of t_coupon — the carrier for 免费试用 (decision D7).
type Coupon struct {
	CouponID    string
	AccountID   int64
	FaceValue   Amount
	RemainValue Amount
	ExpireAt    time.Time
	// Scope restricts what the coupon may offset: product codes, charge types,
	// or SKUs. An empty slice means unrestricted on that dimension.
	ProductCodes []string
	ChargeTypes  []ChargeType
	Status       int // 0未用 1部分使用 2用尽 3过期
}

func (c Coupon) usable(t time.Time, req Request) bool {
	if c.RemainValue.IsZero() || c.RemainValue.IsNegative() {
		return false
	}
	if c.Status == 2 || c.Status == 3 {
		return false
	}
	if !c.ExpireAt.IsZero() && !t.Before(c.ExpireAt) {
		return false
	}
	if len(c.ProductCodes) > 0 && !contains(c.ProductCodes, req.ProductCode) {
		return false
	}
	if len(c.ChargeTypes) > 0 && !containsCharge(c.ChargeTypes, req.ChargeType) {
		return false
	}
	return true
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func containsCharge(list []ChargeType, v ChargeType) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// Request is a 询价 input.
type Request struct {
	AccountID     int64
	ProductCode   string
	SKUCode       string
	RegionID      string
	ChargeType    ChargeType
	Duration      int64 // number of DurationUnit periods; 0 for postpaid
	DurationUnit  DurationUnit
	Quantity      int64 // number of instances; defaults to 1
	CustomerLevel string
	UserTag       string // "new" for first-purchase promotions
	At            time.Time
}

// Result is the price breakdown returned by 询价 and frozen into the order's
// price snapshot.
type Result struct {
	ListAmount    Amount
	PromoAmount   Amount
	CouponAmount  Amount
	PayableAmount Amount

	// AppliedPromoID is empty when no promotion matched. Only one promotion
	// ever applies — promotions do not stack (01§12.3).
	AppliedPromoID string
	// AppliedCoupons records which coupons were consumed and by how much, in
	// consumption order. This is the audit trail for a disputed bill.
	AppliedCoupons []CouponUse
	// RuleID identifies the pricing rule used, so a historical order can be
	// explained even after the catalogue moves on.
	RuleID int64
}

// CouponUse records one coupon's contribution.
type CouponUse struct {
	CouponID string
	Amount   Amount
}

// Errors.
var (
	ErrNoPricingRule    = errors.New("pricing: no active pricing rule for request")
	ErrChargeTypeUnsold = errors.New("pricing: charge type not sellable in the current phase")
	ErrInvalidDuration  = errors.New("pricing: prepaid orders require a positive duration")
)

// Engine evaluates prices. It is stateless; the caller supplies the candidate
// rules, promotions and coupons, which in the service come from svc-catalog's
// tables via a repository.
type Engine struct{}

// Calculate runs the three-stage pricing pipeline and returns the breakdown.
//
// It never mutates the coupons it is given: the returned AppliedCoupons is the
// intent to consume, which the order service commits transactionally at
// payment time. Quoting must not spend a coupon — a user browsing prices would
// otherwise drain their own trial credit.
func (e Engine) Calculate(req Request, rules []PricingRule, promos []Promotion, coupons []Coupon) (Result, error) {
	if !req.ChargeType.Sellable() {
		return Result{}, fmt.Errorf("%w: %s", ErrChargeTypeUnsold, req.ChargeType)
	}
	if req.Quantity <= 0 {
		req.Quantity = 1
	}
	if req.ChargeType == ChargePrepaid && req.Duration <= 0 {
		return Result{}, ErrInvalidDuration
	}

	// ① 目录价.
	rule, ok := selectRule(rules, req)
	if !ok {
		return Result{}, ErrNoPricingRule
	}
	list := rule.ListPrice.Mul(req.Quantity)
	if req.ChargeType == ChargePrepaid {
		list = list.Mul(req.Duration)
	}

	// ② 促销折扣 — best single promotion, never stacked.
	promo, promoAmount := bestPromotion(promos, req, list)
	afterPromo := list.Sub(promoAmount)
	if afterPromo.IsNegative() {
		afterPromo = 0
		promoAmount = list
	}

	// ③ 代金券预抵扣 — consumed by expiry ascending so the credit that would
	// expire soonest is spent first, which is what a user expects and what
	// minimises silently wasted trial credit.
	couponUses, couponTotal := applyCoupons(coupons, req, afterPromo)

	payable := afterPromo.Sub(couponTotal)
	if payable.IsNegative() {
		payable = 0
	}

	return Result{
		ListAmount:     list,
		PromoAmount:    promoAmount,
		CouponAmount:   couponTotal,
		PayableAmount:  payable,
		AppliedPromoID: promo,
		AppliedCoupons: couponUses,
		RuleID:         rule.RuleID,
	}, nil
}

// selectRule picks the most specific active rule matching the request.
func selectRule(rules []PricingRule, req Request) (PricingRule, bool) {
	var best PricingRule
	found := false
	for _, r := range rules {
		if r.SKUCode != req.SKUCode {
			continue
		}
		if !r.active(req.At) {
			continue
		}
		if r.RegionID != "*" && r.RegionID != "" && r.RegionID != req.RegionID {
			continue
		}
		if r.CustomerLevel != "" && r.CustomerLevel != req.CustomerLevel {
			continue
		}
		if r.DurationUnit != "" && req.DurationUnit != "" && r.DurationUnit != req.DurationUnit {
			continue
		}
		if !found || r.specificity() > best.specificity() {
			best = r
			found = true
		}
	}
	return best, found
}

// bestPromotion returns the single most valuable applicable promotion.
// Promotions never stack (01§12.3): taking the best one is both the most
// generous interpretation for the customer and the only rule that keeps the
// final price independent of evaluation order.
func bestPromotion(promos []Promotion, req Request, list Amount) (string, Amount) {
	bestID := ""
	var bestDiscount Amount
	for _, p := range promos {
		if !p.active(req.At) || !p.applies(req) {
			continue
		}
		d := p.discountOn(list)
		if d > bestDiscount {
			bestDiscount = d
			bestID = p.PromoID
		}
	}
	return bestID, bestDiscount
}

// applyCoupons consumes coupons against the outstanding amount, expiry
// ascending.
func applyCoupons(coupons []Coupon, req Request, outstanding Amount) ([]CouponUse, Amount) {
	if outstanding.IsZero() || outstanding.IsNegative() {
		return nil, 0
	}

	usable := make([]Coupon, 0, len(coupons))
	for _, c := range coupons {
		if c.AccountID != req.AccountID {
			continue
		}
		if c.usable(req.At, req) {
			usable = append(usable, c)
		}
	}
	// Expiry ascending; coupons with no expiry sort last since they are never
	// at risk of being wasted.
	sort.SliceStable(usable, func(i, j int) bool {
		ei, ej := usable[i].ExpireAt, usable[j].ExpireAt
		switch {
		case ei.IsZero() && ej.IsZero():
			return usable[i].CouponID < usable[j].CouponID
		case ei.IsZero():
			return false
		case ej.IsZero():
			return true
		default:
			return ei.Before(ej)
		}
	})

	var uses []CouponUse
	var total Amount
	remaining := outstanding
	for _, c := range usable {
		if remaining.IsZero() {
			break
		}
		take := Min(c.RemainValue, remaining)
		if take.IsZero() || take.IsNegative() {
			continue
		}
		uses = append(uses, CouponUse{CouponID: c.CouponID, Amount: take})
		total = total.Add(take)
		remaining = remaining.Sub(take)
	}
	return uses, total
}
