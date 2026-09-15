package reservepack

import (
	"errors"
	"fmt"
	"time"

	"github.com/qifalab/euler-platform/pricing"
)

// commitAttempts bounds the optimistic-lock retry loop. A conflict means
// another writer (a concurrent settlement, a refund) moved the pack between
// our read and our write; the loop re-reads, recomputes against the fresh
// balance and tries again. Three attempts is ample for a single pack's
// contention profile, and the fresh state (possibly exhausted, possibly
// expired) is what the retry acts on.
const commitAttempts = 3

// Store is the persistence boundary for packs and their journals, mirroring
// ledger.Store. Production backs this with t_resource_pack /
// t_resource_pack_journal (account_id shard key); tests use MemoryStore.
type Store interface {
	// GetPack returns the pack, or (_, false, nil) if absent.
	GetPack(packID string) (Pack, bool, error)
	// ListActiveByAccount returns packs that may still be drawn on at t:
	// active, not expired, with remaining quota.
	ListActiveByAccount(accountID int64, t time.Time) ([]Pack, error)
	// Apply mutates the pack and appends a journal row in one transaction.
	// expectedVersion is the optimistic-lock guard; a mismatch means another
	// writer moved first and the caller must retry.
	Apply(entry Entry, newPack Pack, expectedVersion int) error
	// FindByIdempotencyKey returns a previously applied entry, if any — the
	// exactly-once lookup.
	FindByIdempotencyKey(packID, key string) (Entry, bool, error)
	// ListEntries returns the journal for a pack, oldest first.
	ListEntries(packID string) ([]Entry, error)
}

// Ledger is the quota account. It is the single writer for pack mutations.
type Ledger struct {
	store       Store
	now         func() time.Time
	nextEntryID func() int64
}

// New builds a Ledger. nextEntryID mints journal ids; production uses the
// segment service (00 附录A), tests use a counter.
func New(store Store, now func() time.Time, nextEntryID func() int64) *Ledger {
	if now == nil {
		now = time.Now
	}
	if nextEntryID == nil {
		var seq int64
		nextEntryID = func() int64 { seq++; return seq }
	}
	return &Ledger{store: store, now: now, nextEntryID: nextEntryID}
}

// Purchase credits quota for a freshly settled pack order. Idempotent: a
// replay of the same idempotency key is a no-op. Remaining may not exceed
// FaceValue (refunds aside, a pack is credited exactly once) — which is also
// why an ALREADY-EXISTING packID under a different order key is rejected:
// re-purchasing would silently reset a partially consumed pack to full.
func (l *Ledger) Purchase(packID string, accountID int64, productCode, skuCode string, faceValue pricing.Amount, expireAt time.Time, orderKey string) (Pack, Entry, error) {
	if orderKey == "" {
		return Pack{}, Entry{}, ErrMissingIdempotency
	}
	if existing, ok, err := l.store.FindByIdempotencyKey(packID, orderKey); err != nil {
		return Pack{}, Entry{}, err
	} else if ok {
		// Idempotent replay: return the pack as it stands.
		p, _, gerr := l.store.GetPack(packID)
		return p, existing, gerr
	}
	if _, ok, err := l.store.GetPack(packID); err != nil {
		return Pack{}, Entry{}, err
	} else if ok {
		return Pack{}, Entry{}, fmt.Errorf("%w: %s", ErrPackExists, packID)
	}

	now := l.now()
	pack := Pack{
		PackID:      packID,
		AccountID:   accountID,
		ProductCode: productCode,
		SKUCode:     skuCode,
		FaceValue:   faceValue,
		Remaining:   faceValue,
		PurchasedAt: now,
		ExpireAt:    expireAt,
		Status:      StatusActive,
		Version:     1,
	}
	entry := Entry{
		EntryID:        l.nextEntryID(),
		PackID:         packID,
		Type:           EntryPurchase,
		Amount:         faceValue,
		Balance:        faceValue,
		BizKey:         orderKey,
		IdempotencyKey: orderKey,
		CreatedAt:      now,
	}
	if err := l.store.Apply(entry, pack, 0); err != nil {
		return Pack{}, Entry{}, err
	}
	return pack, entry, nil
}

// Consume draws quota from the pack to settle a charge. Returns the amount
// actually taken (which may be less than requested if the pack runs out) and
// the remaining shortfall. Idempotent on consumeKey.
//
// Consume never drives the pack negative: if the requested amount exceeds the
// available quota, only the available amount is taken and the rest is returned
// as shortfall for the next waterfall tier (the coupon, then cash).
func (l *Ledger) Consume(packID, productCode string, requested pricing.Amount, bizKey, consumeKey string) (Pack, Entry, pricing.Amount, error) {
	if consumeKey == "" {
		return Pack{}, Entry{}, 0, ErrMissingIdempotency
	}
	if requested.IsZero() || requested.IsNegative() {
		// A negative "consume" would flow through Min() as a negative take and
		// credit the pack; the movement amount is a magnitude, never a sign.
		return Pack{}, Entry{}, 0, fmt.Errorf("%w: %s", ErrInvalidAmount, requested)
	}
	if existing, ok, err := l.store.FindByIdempotencyKey(packID, consumeKey); err != nil {
		return Pack{}, Entry{}, 0, err
	} else if ok {
		return l.replayConsume(packID, existing, requested)
	}

	var lastErr error
	for attempt := 0; attempt < commitAttempts; attempt++ {
		pack, ok, err := l.store.GetPack(packID)
		if err != nil {
			return Pack{}, Entry{}, 0, err
		}
		if !ok {
			return Pack{}, Entry{}, 0, ErrPackNotFound
		}
		now := l.now()
		if !pack.usable(productCode, now) {
			if pack.Status.IsTerminal() {
				return pack, Entry{}, requested, ErrPackTerminal
			}
			if !pack.ExpireAt.IsZero() && !now.Before(pack.ExpireAt) {
				return pack, Entry{}, requested, ErrPackExpired
			}
			return pack, Entry{}, requested, ErrInsufficientQuota
		}

		take := pricing.Min(pack.Available(now), requested)
		if take.IsZero() {
			return pack, Entry{}, requested, ErrInsufficientQuota
		}
		shortfall := requested.Sub(take)
		newRemaining := pack.Remaining.Sub(take)
		status := pack.Status
		if newRemaining.IsZero() {
			status = StatusExhausted
		}
		next := pack
		next.Remaining = newRemaining
		next.Status = status
		next.Version = pack.Version + 1

		entry := Entry{
			EntryID:        l.nextEntryID(),
			PackID:         packID,
			Type:           EntryConsume,
			Amount:         take,
			Balance:        newRemaining,
			BizKey:         bizKey,
			IdempotencyKey: consumeKey,
			CreatedAt:      now,
		}
		switch err := l.store.Apply(entry, next, pack.Version); {
		case err == nil:
			return next, entry, shortfall, nil
		case errors.Is(err, ErrVersionConflict):
			// Another writer moved first: re-read and recompute. The fresh
			// state decides — it may now be exhausted or expired.
			lastErr = err
			continue
		case errors.Is(err, ErrDuplicateEntry):
			// A concurrent caller applied this very consumeKey first; its
			// result is the one that stands (exactly-once, not twice).
			if prior, ok, ferr := l.store.FindByIdempotencyKey(packID, consumeKey); ferr == nil && ok {
				return l.replayConsume(packID, prior, requested)
			}
			return Pack{}, Entry{}, 0, err
		default:
			return Pack{}, Entry{}, 0, err
		}
	}
	return Pack{}, Entry{}, 0, lastErr
}

// replayConsume reproduces the original consume result on an idempotent
// replay, including the shortfall the first attempt reported. Returning zero
// would tell a retrying settlement that the pack covered the whole charge and
// let it skip the next waterfall tier — the platform would silently under-bill.
func (l *Ledger) replayConsume(packID string, existing Entry, requested pricing.Amount) (Pack, Entry, pricing.Amount, error) {
	p, _, err := l.store.GetPack(packID)
	if err != nil {
		return Pack{}, Entry{}, 0, err
	}
	shortfall := requested.Sub(existing.Amount)
	if shortfall.IsNegative() {
		shortfall = 0
	}
	return p, existing, shortfall, nil
}

// Refund credits quota back for a reversed charge. Refund is bounded by FaceValue:
// the pack may not hold more than was purchased. Idempotent on refundKey.
func (l *Ledger) Refund(packID string, amount pricing.Amount, bizKey, refundKey string) (Pack, Entry, error) {
	if refundKey == "" {
		return Pack{}, Entry{}, ErrMissingIdempotency
	}
	if amount.IsZero() || amount.IsNegative() {
		return Pack{}, Entry{}, fmt.Errorf("%w: %s", ErrInvalidAmount, amount)
	}
	if existing, ok, err := l.store.FindByIdempotencyKey(packID, refundKey); err != nil {
		return Pack{}, Entry{}, err
	} else if ok {
		p, _, gerr := l.store.GetPack(packID)
		return p, existing, gerr
	}

	var lastErr error
	for attempt := 0; attempt < commitAttempts; attempt++ {
		pack, ok, err := l.store.GetPack(packID)
		if err != nil {
			return Pack{}, Entry{}, err
		}
		if !ok {
			return Pack{}, Entry{}, ErrPackNotFound
		}
		// An expired pack does not accept refunds: its quota was forfeit at
		// expiry and the bill that drew on it cannot be reversed through quota.
		if pack.Status == StatusExpired {
			return pack, Entry{}, ErrPackExpired
		}
		// The deadline is authoritative, not the status: a pack past its ExpireAt
		// is dead even before the sweep flips the row, and crediting quota back
		// to it would report a refund the customer cannot spend.
		if !pack.ExpireAt.IsZero() && !l.now().Before(pack.ExpireAt) {
			return pack, Entry{}, ErrPackExpired
		}
		newRemaining := pack.Remaining.Add(amount)
		if newRemaining > pack.FaceValue {
			return pack, Entry{}, fmt.Errorf("%w: %s > %s", ErrRefundExceedsFace, newRemaining, pack.FaceValue)
		}
		next := pack
		next.Remaining = newRemaining
		// A refund can revive an exhausted pack back to active.
		if next.Status == StatusExhausted && !newRemaining.IsZero() {
			next.Status = StatusActive
		}
		next.Version = pack.Version + 1

		entry := Entry{
			EntryID:        l.nextEntryID(),
			PackID:         packID,
			Type:           EntryRefund,
			Amount:         amount,
			Balance:        newRemaining,
			BizKey:         bizKey,
			IdempotencyKey: refundKey,
			CreatedAt:      l.now(),
		}
		switch err := l.store.Apply(entry, next, pack.Version); {
		case err == nil:
			return next, entry, nil
		case errors.Is(err, ErrVersionConflict):
			lastErr = err
			continue
		case errors.Is(err, ErrDuplicateEntry):
			if prior, ok, ferr := l.store.FindByIdempotencyKey(packID, refundKey); ferr == nil && ok {
				p, _, gerr := l.store.GetPack(packID)
				return p, prior, gerr
			}
			return Pack{}, Entry{}, err
		default:
			return Pack{}, Entry{}, err
		}
	}
	return Pack{}, Entry{}, lastErr
}

// Expire forfeits any residual quota at the deadline. Idempotent. A pack whose
// expiry has not yet been reached is not expired.
func (l *Ledger) Expire(packID string, expireKey string) (Pack, Entry, error) {
	if expireKey == "" {
		return Pack{}, Entry{}, ErrMissingIdempotency
	}
	if existing, ok, err := l.store.FindByIdempotencyKey(packID, expireKey); err != nil {
		return Pack{}, Entry{}, err
	} else if ok {
		p, _, gerr := l.store.GetPack(packID)
		return p, existing, gerr
	}

	var lastErr error
	for attempt := 0; attempt < commitAttempts; attempt++ {
		pack, ok, err := l.store.GetPack(packID)
		if err != nil {
			return Pack{}, Entry{}, err
		}
		if !ok {
			return Pack{}, Entry{}, ErrPackNotFound
		}
		now := l.now()
		if !pack.ExpireAt.IsZero() && now.Before(pack.ExpireAt) {
			return pack, Entry{}, fmt.Errorf("%w: expires at %s", ErrPackExpired, pack.ExpireAt.Format(time.RFC3339))
		}
		if pack.Status.IsTerminal() {
			// Already exhausted or expired — idempotent no-op.
			return pack, Entry{}, nil
		}
		forfeited := pack.Remaining
		next := pack
		next.Remaining = 0
		next.Status = StatusExpired
		next.Version = pack.Version + 1

		entry := Entry{
			EntryID:        l.nextEntryID(),
			PackID:         packID,
			Type:           EntryExpire,
			Amount:         forfeited,
			Balance:        0,
			BizKey:         expireKey,
			IdempotencyKey: expireKey,
			CreatedAt:      now,
		}
		switch err := l.store.Apply(entry, next, pack.Version); {
		case err == nil:
			return next, entry, nil
		case errors.Is(err, ErrVersionConflict):
			lastErr = err
			continue
		case errors.Is(err, ErrDuplicateEntry):
			if prior, ok, ferr := l.store.FindByIdempotencyKey(packID, expireKey); ferr == nil && ok {
				p, _, gerr := l.store.GetPack(packID)
				return p, prior, gerr
			}
			return Pack{}, Entry{}, err
		default:
			return Pack{}, Entry{}, err
		}
	}
	return Pack{}, Entry{}, lastErr
}

// SweepExpired expires every active pack whose deadline has passed, returning
// the pack ids that transitioned to expired. This is the background scanner
// (svc-billing /internal/reservepack/expire-sweep).
func (l *Ledger) SweepExpired(accountID int64) ([]string, error) {
	now := l.now()
	active, err := l.store.ListActiveByAccount(accountID, now)
	if err != nil {
		return nil, err
	}
	var expired []string
	for _, p := range active {
		if p.ExpireAt.IsZero() || now.Before(p.ExpireAt) {
			continue
		}
		// One idempotency key per expiry sweep per pack per day keeps replays safe.
		key := fmt.Sprintf("expire:%s:%s", p.PackID, now.Format("2006-01-02"))
		if _, _, err := l.Expire(p.PackID, key); err != nil {
			// A concurrent consume may have exhausted the pack first; that is fine.
			if err == ErrPackExpired || err == ErrPackTerminal {
				continue
			}
			return expired, err
		}
		expired = append(expired, p.PackID)
	}
	return expired, nil
}
