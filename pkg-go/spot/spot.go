// Package spot implements the floating-price engine for preemptible (抢占式)
// instances — the third billing form, opened in phase 2 (decision D6,
// 09-roadmap M-4.2).
//
// A spot price floats minute-by-minute against market supply. Unlike 按量
// (postpaid) which bills at a frozen list price, a spot instance bills each
// metering cycle at the price in force at the start of that cycle. The customer
// accepts two trades for the discount: the price moves, and the platform may
// reclaim the instance with 5 minutes' notice when capacity pressure rises.
//
// # Price is a snapshot, billed as-of
//
// Each cycle's charge references the price snapshot at that cycle's start, not
// the live price at settlement — the same "bill must be reproducible" rule that
// governs the catalogue (01§12.3 rule 4). A bill issued today for last week's
// spot cycles must still reconcile against the price history, so the engine
// keeps an append-only price log indexed by (product, region, cycle).
//
// # Reclaim is gated by delivered notice
//
// A spot resource may only be reclaimed after the customer has been notified
// with a 5-minute window, and that notice must be provably delivered — the same
// trust-critical notification gate that protects resource release in the
// arrears path (pkg-go/notify). The reclaim window constant lives in
// pkg-go/provision (provision.ReclaimNoticeWindow) since the Preemptor contract
// is defined there; this package holds the price side.
package spot

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/provision"
)

// ReclaimNoticeWindow re-exports the provision constant so spot-package callers
// see a single name; the authority is provision.ReclaimNoticeWindow.
const ReclaimNoticeWindow = provision.ReclaimNoticeWindow

// Price is one floating price point in force over a time range.
//
// Append-only: a price change adds a new record with a new start, never
// rewrites an existing one — so historical bills remain explicable, mirroring
// the catalogue's append-only pricing_rule (01§12.3 rule 2).
type Price struct {
	ProductCode string
	RegionID    string
	// PricePerHour is the floating unit price for one billing-unit-hour.
	PricePerHour  pricing.Amount
	EffectiveFrom time.Time
	// EffectiveTo is the open end; zero means the price is current. The next
	// record's EffectiveFrom bounds it.
	EffectiveTo time.Time
}

// InForce reports whether the price covers time t.
func (p Price) InForce(t time.Time) bool {
	if !p.EffectiveFrom.IsZero() && t.Before(p.EffectiveFrom) {
		return false
	}
	if !p.EffectiveTo.IsZero() && !t.Before(p.EffectiveTo) {
		return false
	}
	return true
}

// Errors.
var (
	ErrNoPriceHistory = errors.New("spot: no price history for product/region")
	ErrFutureCycle    = errors.New("spot: billing cycle is in the future")
	ErrNegativePrice  = errors.New("spot: price must not be negative")
)

// Pricer answers "what was the spot price for this product/region at time t".
type Pricer interface {
	// PriceAt returns the price in force at t. A spot instance billed for a
	// cycle starting at t charges PricePerHour × quantity for that cycle.
	PriceAt(productCode, regionID string, t time.Time) (Price, error)
}

// Engine is the floating-price engine. It holds an append-only price log per
// (product, region) and answers historical and current price queries.
type Engine struct {
	mu   sync.RWMutex
	logs map[string][]Price // key = productCode + "|" + regionID
	now  func() time.Time
}

// NewEngine builds an Engine.
func NewEngine(now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{logs: make(map[string][]Price), now: now}
}

func key(productCode, regionID string) string { return productCode + "|" + regionID }

// Publish records a new floating price for a product/region, effective from
// now. Publishing closes the previous record's open end at the same instant,
// keeping the log contiguous and append-only. Returns the published Price.
//
// A price may not go below zero (a negative spot price is a market error, not
// a subsidy) — a negative input is rejected with ErrNegativePrice before it
// can enter the append-only log. Zero is permitted and means the instance is
// effectively free for the cycle — a floor the platform sets deliberately,
// never by accident.
func (e *Engine) Publish(productCode, regionID string, pricePerHour pricing.Amount) (Price, error) {
	if pricePerHour.IsNegative() {
		return Price{}, fmt.Errorf("%w: %s", ErrNegativePrice, pricePerHour)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	k := key(productCode, regionID)
	now := e.now()
	// Close the previous current record.
	hist := e.logs[k]
	if n := len(hist); n > 0 {
		cur := hist[n-1]
		if cur.EffectiveTo.IsZero() {
			cur.EffectiveTo = now
			hist[n-1] = cur
		}
	}
	p := Price{
		ProductCode:   productCode,
		RegionID:      regionID,
		PricePerHour:  pricePerHour,
		EffectiveFrom: now,
	}
	e.logs[k] = append(hist, p)
	return p, nil
}

// PriceAt implements Pricer.
func (e *Engine) PriceAt(productCode, regionID string, t time.Time) (Price, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	hist := e.logs[key(productCode, regionID)]
	if len(hist) == 0 {
		return Price{}, fmt.Errorf("%w: %s/%s", ErrNoPriceHistory, productCode, regionID)
	}
	if t.After(e.now()) {
		return Price{}, fmt.Errorf("%w: %s", ErrFutureCycle, t.Format(time.RFC3339))
	}
	// Find the newest record in force at t. The log is time-ordered; the last
	// record whose EffectiveFrom <= t and (open-ended or EffectiveTo > t).
	idx := sort.Search(len(hist), func(i int) bool {
		return hist[i].EffectiveFrom.After(t)
	})
	if idx == 0 {
		// No record yet in force at t — the product predated any published
		// price. Fall back to the earliest so a cycle just before first publish
		// still bills (against the first price), rather than erroring.
		return hist[0], nil
	}
	return hist[idx-1], nil
}

// Current returns the price in force now.
func (e *Engine) Current(productCode, regionID string) (Price, error) {
	return e.PriceAt(productCode, regionID, e.now())
}

// History returns the full append-only price log for a product/region, oldest
// first. This is the audit trail behind a disputed spot bill.
func (e *Engine) History(productCode, regionID string) ([]Price, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	hist := e.logs[key(productCode, regionID)]
	if len(hist) == 0 {
		return nil, fmt.Errorf("%w: %s/%s", ErrNoPriceHistory, productCode, regionID)
	}
	out := make([]Price, len(hist))
	copy(out, hist)
	return out, nil
}
