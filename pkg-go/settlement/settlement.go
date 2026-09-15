// Package settlement implements the marketplace revenue split — 合作伙伴与
// 渠道分账 (09-roadmap §5.1 goal 3, §5.2 M-10 云市场交易闭环 上架审核/分账/结算).
//
// A marketplace order has one gross amount and two beneficiaries: the partner
// (ISV) whose product was sold, and the platform (the marketplace itself). The
// split is a ratio of the gross, expressed in basis points, and the two shares
// must sum back to the gross EXACTLY — the same 无未解释差异 discipline the
// billing waterfall enforces (03§4.2.4). A split that loses or invents a
// micro-unit is a reconciliation defect, not a rounding footnote.
//
// Money is `pricing.Amount` (fixed-point micro-units, never float — see the
// pricing package and the README invariant table). Rates are basis points,
// because that is the integer ratio the fixed-point engine divides cleanly.
package settlement

import (
	"fmt"

	"github.com/qifalab/euler-platform/pricing"
)

// RateBasisPointsFull is the rate representing 100% (the platform or a single
// partner taking the entire gross).
const RateBasisPointsFull = 10000

// Rate is a settlement rate in basis points (0..10000, where 10000 = 100%).
type Rate int64

// Valid reports whether r is in [0, 10000]. A negative rate or one above 100%
// is a config error that must fail at load, not a silent over-payout.
func (r Rate) Valid() bool { return r >= 0 && r <= RateBasisPointsFull }

// Split is a settled revenue split between a partner and the platform.
type Split struct {
	// Total is the gross order amount being split.
	Total pricing.Amount
	// PartnerRate is the partner's basis-point share.
	PartnerRate Rate
	// Partner is the partner's share: Total × PartnerRate / 10000.
	Partner pricing.Amount
	// Platform is the platform's share: Total − Partner.
	Platform pricing.Amount
}

// Settle computes the split of total between a partner (at partnerRate basis
// points) and the platform (the remainder). The invariant Partner + Platform ==
// Total holds exactly, because the platform share is derived by subtraction,
// never by a second independent rate computation — two rounded computations
// could disagree with each other, which is the exact defect this package exists
// to prevent.
func Settle(total pricing.Amount, partnerRate Rate) (Split, error) {
	if !partnerRate.Valid() {
		return Split{}, fmt.Errorf("settlement: partner rate %d out of range [0, %d]", partnerRate, RateBasisPointsFull)
	}
	if total.IsNegative() {
		return Split{}, fmt.Errorf("settlement: cannot split a negative total %s", total)
	}
	partner := total.MulRate(int64(partnerRate))
	return Split{
		Total:       total,
		PartnerRate: partnerRate,
		Partner:     partner,
		Platform:    total.Sub(partner),
	}, nil
}

// Valid reports whether a split reconciles: both shares are non-negative and
// together equal the total exactly. Used by the settlement audit path, where a
// stored split must never be trusted at face value.
func (s Split) Valid() bool {
	if !s.PartnerRate.Valid() {
		return false
	}
	if s.Partner.IsNegative() || s.Platform.IsNegative() {
		return false
	}
	return s.Partner.Add(s.Platform) == s.Total
}
