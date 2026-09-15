// Package billing turns metered usage into charges (03-backend-services.md
// §4.2.4, 01-product-catalog.md §12.3).
//
// # The出账 chain
//
//	计量聚合 → 计费(按价格快照) → 抵扣 → 明细入账 → 小时汇总 → 月度账单
//
// # Deduction waterfall
//
// 01§12.3 fixes the order: 资源包额度 → 代金券(按到期时间升序) → 现金余额 →
// 三方支付. Phase 1 sells no resource packs (decision D6), so the effective
// order is voucher → cash, but the waterfall is written with the resource-pack
// tier present so enabling it in phase 2 is configuration rather than a
// rewrite of the charging path.
//
// Vouchers are spent expiry-ascending so credit that would otherwise be wasted
// goes first — the customer-favourable choice, and the one that avoids the
// support conversation about credit that silently expired while cash was
// charged instead.
//
// # Charging against the snapshot, never the live price
//
// Every charge references the price snapshot frozen at order time. The
// catalogue moves; a bill issued last quarter must still be explicable today
// (01§12.3 rule 4). Reading the current price at settlement time would make
// historical bills unreproducible, which is exactly the "unexplained
// difference" that 09 A2 forbids.
//
// # Arrears is a state, not a negative balance
//
// When the balance cannot cover a charge, the account enters arrears and the
// lifecycle state machine takes over (03§5.4). The ledger never goes negative:
// an overdraft would be credit that no policy authorised.
package billing

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/qifalab/euler-platform/metering"
	"github.com/qifalab/euler-platform/pricing"
)

// DeductionSource identifies which pool paid for a charge.
type DeductionSource string

const (
	// SourceResourcePack 资源包额度 — phase 2 (decision D6), tier present so
	// enabling it later does not rewrite the waterfall.
	SourceResourcePack DeductionSource = "RESOURCE_PACK"
	// SourceCoupon 代金券, spent expiry-ascending.
	SourceCoupon DeductionSource = "COUPON"
	// SourceBalance 现金余额.
	SourceBalance DeductionSource = "BALANCE"
)

// Deduction records one pool's contribution to settling a charge.
type Deduction struct {
	Source DeductionSource
	// Ref identifies the specific pool instance: a coupon id, a resource-pack
	// id, or empty for cash. Without it a disputed bill cannot be traced back
	// to which voucher was consumed.
	Ref    string
	Amount pricing.Amount
}

// Charge is one hour of billing for one resource and metering item — a
// bill_detail row.
type Charge struct {
	// ChargeID derives from the aggregate id, so re-running settlement for the
	// same hour produces the same id and the unique index makes it idempotent
	// (05§5.5.1: billing keys on agg_id).
	ChargeID     string
	AccountID    int64
	ResourceID   string
	ProductCode  string
	MeteringItem string
	BillPeriod   string // "2026-08"
	BillingCycle time.Time
	Quantity     metering.Quantity
	UnitPrice    pricing.Amount
	// PretaxAmount is the charge before any deduction — the list cost.
	PretaxAmount pricing.Amount
	Deductions   []Deduction
	// PayAmount is what came out of cash balance.
	PayAmount pricing.Amount
	// SnapshotID is the price snapshot this charge was computed against.
	SnapshotID string
	// Shortfall is the portion of PretaxAmount no pool could cover — the
	// arrears amount carried on the charge itself so the reconcile equation
	// (deductions + shortfall = pretax) closes even for an unpaid line.
	Shortfall pricing.Amount
	// CoveredRatio is carried forward from the aggregate so a bill line built
	// on incomplete data is visible on the bill itself, not only in the
	// pipeline's own logs.
	CoveredRatio int
	SettledAt    time.Time
}

// Incomplete reports whether this charge was computed from a partial hour.
func (c Charge) Incomplete() bool { return c.CoveredRatio < 100 }

// TotalDeducted returns the sum of all deductions.
func (c Charge) TotalDeducted() pricing.Amount {
	var total pricing.Amount
	for _, d := range c.Deductions {
		total = total.Add(d.Amount)
	}
	return total
}

// Reconciles reports whether the charge's components add up: every unit of
// pretax cost must be accounted for by some deduction OR by the recorded
// arrears shortfall. This is the per-row invariant behind 09 A2 (无未解释差异):
// an unpaid line is EXPLAINED (the account is in arrears), not unexplained,
// so it must not keep a bill unreconciled forever.
func (c Charge) Reconciles() bool {
	return c.TotalDeducted().Add(c.Shortfall) == c.PretaxAmount
}

// Pool is a deductible balance available to settle charges.
type Pool struct {
	Source    DeductionSource
	Ref       string
	Available pricing.Amount
	// ExpireAt orders coupon consumption; zero means no expiry.
	ExpireAt time.Time
	// ProductCodes restricts what the pool may pay for; empty means universal.
	ProductCodes []string
}

// applicable reports whether the pool may settle a charge for this product.
func (p Pool) applicable(productCode string) bool {
	if len(p.ProductCodes) == 0 {
		return true
	}
	for _, c := range p.ProductCodes {
		if c == productCode {
			return true
		}
	}
	return false
}

// Errors.
var (
	ErrNoUnitPrice    = errors.New("billing: no unit price for metering item")
	ErrFrozenPeriod   = errors.New("billing: billing period is frozen")
	ErrNegativeCharge = errors.New("billing: computed charge is negative")
)

// Settlement is the outcome of charging one aggregate.
type Settlement struct {
	Charge Charge
	// Shortfall is the amount that could not be covered by any pool. Non-zero
	// means the account falls into arrears and the lifecycle state machine
	// takes over (03§5.4) — the ledger is never driven negative.
	Shortfall pricing.Amount
	// InArrears reports whether this settlement pushed the account into arrears.
	InArrears bool
}

// Engine settles metered usage into charges.
type Engine struct {
	Now func() time.Time
}

// NewEngine builds an Engine.
func NewEngine(now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{Now: now}
}

// ChargeID derives the settlement idempotency key from the aggregate id.
func ChargeID(aggID string) string { return "chg-" + aggID }

// Settle computes the charge for one hourly aggregate and applies the
// deduction waterfall against the supplied pools.
//
// Pools are not mutated: the returned deductions are the INTENT to consume,
// which the caller commits transactionally alongside the ledger entry. A
// settlement that computed and spent in one step could not be retried safely
// after a partial failure.
func (e *Engine) Settle(
	usage metering.HourlyUsage,
	unitPrice pricing.Amount,
	productCode, snapshotID string,
	pools []Pool,
) (Settlement, error) {
	if unitPrice.IsNegative() {
		return Settlement{}, fmt.Errorf("%w: %s", ErrNoUnitPrice, unitPrice)
	}
	if metering.Frozen(usage.HourStart, e.Now()) {
		return Settlement{}, fmt.Errorf("%w: %s", ErrFrozenPeriod, usage.HourStart.Format("2006-01"))
	}

	// quantity × unit price, both fixed-point. The quantity carries 6 decimal
	// places, so the product is divided back down by the same scale.
	pretax := unitPrice.MulDiv(int64(usage.TotalQuantity), 1_000_000)
	if pretax.IsNegative() {
		return Settlement{}, ErrNegativeCharge
	}

	deductions, shortfall := applyWaterfall(pretax, productCode, pools)

	var cashPaid pricing.Amount
	for _, d := range deductions {
		if d.Source == SourceBalance {
			cashPaid = cashPaid.Add(d.Amount)
		}
	}

	// BillPeriod is derived in UTC, matching metering.FreezeBoundary/Frozen:
	// deriving it from the local zone would put an hour near month boundary
	// into one month's bill while the freeze check treats it as another's.
	hourUTC := usage.HourStart.UTC()
	charge := Charge{
		ChargeID:     ChargeID(usage.AggID),
		AccountID:    usage.AccountID,
		ResourceID:   usage.ResourceID,
		ProductCode:  productCode,
		MeteringItem: usage.MeteringItem,
		BillPeriod:   hourUTC.Format("2006-01"),
		BillingCycle: hourUTC,
		Quantity:     usage.TotalQuantity,
		UnitPrice:    unitPrice,
		PretaxAmount: pretax,
		Deductions:   deductions,
		PayAmount:    cashPaid,
		SnapshotID:   snapshotID,
		Shortfall:    shortfall,
		CoveredRatio: usage.CoveredRatio,
		SettledAt:    e.Now(),
	}

	return Settlement{
		Charge:    charge,
		Shortfall: shortfall,
		InArrears: !shortfall.IsZero(),
	}, nil
}

// applyWaterfall spends pools in the fixed order: resource pack → coupon
// (expiry ascending) → cash balance (01§12.3).
func applyWaterfall(amount pricing.Amount, productCode string, pools []Pool) ([]Deduction, pricing.Amount) {
	if amount.IsZero() {
		return nil, 0
	}

	// Order pools by tier, then by expiry within the coupon tier so credit
	// that would otherwise be wasted is spent first.
	ordered := make([]Pool, 0, len(pools))
	for _, p := range pools {
		if p.applicable(productCode) && !p.Available.IsZero() && !p.Available.IsNegative() {
			ordered = append(ordered, p)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, rj := tierRank(ordered[i].Source), tierRank(ordered[j].Source)
		if ri != rj {
			return ri < rj
		}
		// Within a tier, soonest expiry first; no-expiry pools sort last since
		// they are never at risk of being wasted.
		ei, ej := ordered[i].ExpireAt, ordered[j].ExpireAt
		switch {
		case ei.IsZero() && ej.IsZero():
			return ordered[i].Ref < ordered[j].Ref
		case ei.IsZero():
			return false
		case ej.IsZero():
			return true
		default:
			return ei.Before(ej)
		}
	})

	var deductions []Deduction
	remaining := amount
	for _, p := range ordered {
		if remaining.IsZero() {
			break
		}
		take := pricing.Min(p.Available, remaining)
		if take.IsZero() || take.IsNegative() {
			continue
		}
		deductions = append(deductions, Deduction{Source: p.Source, Ref: p.Ref, Amount: take})
		remaining = remaining.Sub(take)
	}
	return deductions, remaining
}

func tierRank(s DeductionSource) int {
	switch s {
	case SourceResourcePack:
		return 0
	case SourceCoupon:
		return 1
	case SourceBalance:
		return 2
	default:
		return 99
	}
}

// MonthlyBill aggregates a period's charges — a bill_main row plus its detail.
type MonthlyBill struct {
	AccountID   int64
	BillPeriod  string
	TotalAmount pricing.Amount
	PaidAmount  pricing.Amount
	ChargeCount int
	// IncompleteCharges counts lines built from partial metering data. A bill
	// containing any of these is provisional: it will change when backfill
	// completes, and issuing it as final would guarantee a dispute.
	IncompleteCharges int
	// UnreconciledCount is the number of lines whose components do not add up.
	// Commercial acceptance requires this to be zero (09 A2, adjudication S17).
	UnreconciledCount int
}

// Final reports whether the bill may be issued as settled: no incomplete data
// and no unreconciled lines.
func (b MonthlyBill) Final() bool {
	return b.IncompleteCharges == 0 && b.UnreconciledCount == 0
}

// Summarize folds charges into a monthly bill.
func Summarize(accountID int64, period string, charges []Charge) MonthlyBill {
	bill := MonthlyBill{AccountID: accountID, BillPeriod: period}
	for _, c := range charges {
		if c.AccountID != accountID || c.BillPeriod != period {
			continue
		}
		bill.TotalAmount = bill.TotalAmount.Add(c.PretaxAmount)
		bill.PaidAmount = bill.PaidAmount.Add(c.PayAmount)
		bill.ChargeCount++
		if c.Incomplete() {
			bill.IncompleteCharges++
		}
		if !c.Reconciles() {
			bill.UnreconciledCount++
		}
	}
	return bill
}

// CostReport breaks an account's charges down by product and tag, the
// FinOps view opened in phase 2 (09-roadmap M-4.3). It allocates each charge's
// pretax amount to its product code and to the resource tags carried on the
// charge (the four-tuple cloud.platform/{tenant,project,product,instance},
// projected to ResourceID here — full tag attribution arrives with the tag
// service).
//
// The report is a re-aggregation of the same charges behind Summarize, never a
// separate data path: a cost figure and a bill figure that disagree is exactly
// the 无未解释差异 the Gate review rejects.
type CostReport struct {
	AccountID  int64
	BillPeriod string
	// ByProduct is the pretax spend per product code.
	ByProduct map[string]pricing.Amount
	// ByResource is the pretax spend per resource id — the finest grain a
	// customer can attribute cost to.
	ByResource map[string]pricing.Amount
	// Total is the sum of all charges in the period.
	Total pricing.Amount
}

// CostAnalysis aggregates an account's charges for a period into a CostReport.
func CostAnalysis(accountID int64, period string, charges []Charge) CostReport {
	r := CostReport{
		AccountID:  accountID,
		BillPeriod: period,
		ByProduct:  make(map[string]pricing.Amount),
		ByResource: make(map[string]pricing.Amount),
	}
	for _, c := range charges {
		if c.AccountID != accountID || c.BillPeriod != period {
			continue
		}
		r.ByProduct[c.ProductCode] = r.ByProduct[c.ProductCode].Add(c.PretaxAmount)
		r.ByResource[c.ResourceID] = r.ByResource[c.ResourceID].Add(c.PretaxAmount)
		r.Total = r.Total.Add(c.PretaxAmount)
	}
	return r
}
