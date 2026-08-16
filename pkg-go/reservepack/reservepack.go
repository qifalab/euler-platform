// Package reservepack implements the resource-pack quota ledger — the
// second billing form enabled in phase 2 (decision D6, 09-roadmap M-4).
//
// A resource pack is a prepaid, fixed-quota balance that is consumed before
// cash in the deduction waterfall (01§12.3: 资源包额度 → 代金券 → 现金 → 三方).
// Phase 1 carried the waterfall tier (billing.SourceResourcePack, rank 0) but
// sold no packs; phase 2 adds the ledger that backs it.
//
// # The ledger is append-only, like cash
//
// Consumption and refunds are journal rows with an idempotency key, mirroring
// pkg-go/ledger: a repeated consume with the same key is a no-op. Remaining
// balance never goes negative and never exceeds the original face value.
//
// # Expiry reclaims residual quota
//
// When a pack reaches its expiry, residual quota is forfeit and the pack enters
// the EXPIRED terminal state. Expire is idempotent: expiring an already-expired
// pack is a no-op. The pool's Available drops to zero so a subsequent billing
// cycle cannot draw on it.
//
// # Why a separate package, not a Coupon
//
// A coupon (pricing.Coupon) is gifted and consumed at 询价 time against a single
// order. A resource pack is purchased (order.TypeNew + ChargeResourcePack),
// persists across billing cycles, and is consumed incrementally by the hourly
// 出账 engine via the billing waterfall. The ledger shape is the same; the
// consumption model and lifecycle differ, so the types are separate to keep
// each domain's invariants legible.
package reservepack

import (
	"errors"
	"time"

	"github.com/starcloud/sc-platform/pricing"
)

// Status is the lifecycle state of a resource pack.
type Status string

const (
	// StatusActive means the pack has remaining quota and has not expired.
	StatusActive Status = "ACTIVE"
	// StatusExhausted means all quota has been consumed (terminal).
	StatusExhausted Status = "EXHAUSTED"
	// StatusExpired means the pack reached expiry with residual quota
	// forfeit (terminal).
	StatusExpired Status = "EXPIRED"
)

// IsTerminal reports whether the state is one resources may not leave.
func (s Status) IsTerminal() bool { return s == StatusExhausted || s == StatusExpired }

// Pack is one purchased resource pack — a row of t_resource_pack plus its
// derived remaining balance.
type Pack struct {
	// PackID is the resource-pack identifier.
	PackID string
	// AccountID is the owning tenant (account_id ≡ uid ≡ tenant_id).
	AccountID int64
	// ProductCode restricts which product's charges this pack may offset.
	// Empty means universal; D6 packs are typically product-scoped.
	ProductCode string
	// SKUCode is the pack SKU purchased (e.g. "scecs.pack.1000cpu.hour").
	SKUCode string
	// FaceValue is the quota purchased. Fixed-point, never float.
	FaceValue pricing.Amount
	// Remaining is the unconsumed quota. Zero on an exhausted/expired pack.
	Remaining pricing.Amount
	// PurchasedAt is when the purchase order settled and quota was credited.
	PurchasedAt time.Time
	// ExpireAt is the quota forfeiture deadline. Zero means no expiry.
	ExpireAt time.Time
	// Status is the current lifecycle state.
	Status Status
	// Version is the optimistic-lock version; every mutation increments it
	// (mirrors ledger's expectedVersion and resource's optimistic lock).
	Version int
}

// Available reports how much quota the pack can still contribute at time t.
// An exhausted, expired, or past-deadline pack contributes nothing.
func (p Pack) Available(t time.Time) pricing.Amount {
	if p.Status.IsTerminal() {
		return 0
	}
	if !p.ExpireAt.IsZero() && !t.Before(p.ExpireAt) {
		return 0
	}
	if p.Remaining.IsZero() || p.Remaining.IsNegative() {
		return 0
	}
	return p.Remaining
}

// usable reports whether the pack may settle a charge for productCode at t.
func (p Pack) usable(productCode string, t time.Time) bool {
	if p.Available(t).IsZero() {
		return false
	}
	if p.ProductCode != "" && p.ProductCode != productCode {
		return false
	}
	return true
}

// EntryType classifies a pack journal entry. The set is closed.
type EntryType string

const (
	// EntryPurchase credits quota when the purchase order settles.
	EntryPurchase EntryType = "PURCHASE"
	// EntryConsume debits quota as the billing waterfall draws on it.
	EntryConsume EntryType = "CONSUME"
	// EntryRefund credits quota back for a reversed charge (e.g. a disputed
	// bill row corrected after issue).
	EntryRefund EntryType = "REFUND"
	// EntryExpire forfeits residual quota at the deadline.
	EntryExpire EntryType = "EXPIRE"
)

// Direction returns +1 for quota-crediting entries, -1 for debiting, 0 otherwise.
func (t EntryType) Direction() int {
	switch t {
	case EntryPurchase, EntryRefund:
		return 1
	case EntryConsume, EntryExpire:
		return -1
	default:
		return 0
	}
}

// Valid reports whether the entry type is known.
func (t EntryType) Valid() bool {
	switch t {
	case EntryPurchase, EntryConsume, EntryRefund, EntryExpire:
		return true
	}
	return false
}

// Entry is one immutable pack journal row.
type Entry struct {
	EntryID int64
	PackID  string
	Type    EntryType
	Amount  pricing.Amount
	Balance pricing.Amount // remaining after this entry
	// BizKey links the entry to its cause (the charge id, the order id).
	BizKey string
	// IdempotencyKey makes application exactly-once. A repeated consume with
	// the same key is a no-op, so a retried settlement cannot double-spend.
	IdempotencyKey string
	CreatedAt      time.Time
}

// Errors.
var (
	ErrPackNotFound       = errors.New("reservepack: pack not found")
	ErrPackExists         = errors.New("reservepack: pack already exists")
	ErrPackExhausted      = errors.New("reservepack: pack exhausted")
	ErrPackExpired        = errors.New("reservepack: pack expired")
	ErrPackTerminal       = errors.New("reservepack: pack is in a terminal state")
	ErrInsufficientQuota  = errors.New("reservepack: insufficient quota")
	ErrInvalidEntryType   = errors.New("reservepack: unknown entry type")
	ErrMissingIdempotency = errors.New("reservepack: idempotency key required")
	ErrDuplicateEntry     = errors.New("reservepack: entry already applied (idempotent no-op)")
	ErrRefundExceedsFace  = errors.New("reservepack: refund would exceed face value")
)
