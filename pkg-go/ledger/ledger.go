// Package ledger implements the cash balance and its append-only journal
// (03-backend-services.md §4.2.4, adjudication S29, 04§6.3).
//
// # Where balance lives, and why not on the account
//
// Adjudication S29 removed `balance` from the account table: cash balance and
// its journal belong to the trade domain (trade_db ledger), keyed by
// account_id. The account table holds identity attributes only.
//
// The reason is ownership, not tidiness. Identity is read on every gateway
// request; balance is written on every charge. Sharing a row would put a
// financial write path behind the hottest read path on the platform, and would
// let the identity service silently become a participant in money mutations it
// has no business arbitrating.
//
// # The ledger is append-only
//
// Every balance change writes a journal entry; entries are never updated or
// deleted (00§3.3: 余额流水只增不改). The balance column is a materialised
// running total, and `SUM(amount)` over the journal must always reproduce it.
// That redundancy is deliberate: it is what makes the T+1 reconciliation able
// to prove the balance was not tampered with, rather than merely assert it.
//
// A correction is a new compensating entry, never an edit — an edited journal
// cannot be distinguished from a fraudulent one.
//
// # Concurrency
//
// Balance mutation uses an optimistic lock on version. Two concurrent charges
// against the same account cannot both succeed from the same starting version:
// the loser retries against the fresh balance and re-checks sufficiency. A
// last-write-wins update here would let a customer spend the same money twice.
package ledger

import (
	"errors"
	"fmt"
	"time"

	"github.com/qifalab/euler-platform/pricing"
)

// EntryType classifies a journal entry. The set is closed: an unclassified
// money movement is not something to invent at the call site.
type EntryType string

const (
	// EntryRecharge 充值 — cash in from a payment channel.
	EntryRecharge EntryType = "RECHARGE"
	// EntryConsume 消费 — cash out to pay an order or an hourly bill.
	EntryConsume EntryType = "CONSUME"
	// EntryRefund 退款 — cash back from a cancelled or downgraded order.
	EntryRefund EntryType = "REFUND"
	// EntryAdjustDebit 调整扣减 — operations correction reducing balance.
	EntryAdjustDebit EntryType = "ADJUST_DEBIT"
	// EntryAdjustCredit 调整增加 — operations correction increasing balance,
	// e.g. a service-credit compensation.
	EntryAdjustCredit EntryType = "ADJUST_CREDIT"
	// EntryFreeze 冻结 — reserve funds for an in-flight order.
	EntryFreeze EntryType = "FREEZE"
	// EntryUnfreeze 解冻 — release a reservation that was not consumed.
	EntryUnfreeze EntryType = "UNFREEZE"
)

// Direction reports whether the entry increases (+1) or decreases (-1) the
// available balance. Freeze/unfreeze move funds between available and frozen
// without changing the total, so they report 0.
func (t EntryType) Direction() int {
	switch t {
	case EntryRecharge, EntryRefund, EntryAdjustCredit:
		return 1
	case EntryConsume, EntryAdjustDebit:
		return -1
	default: // freeze / unfreeze
		return 0
	}
}

// Valid reports whether t is a known entry type.
func (t EntryType) Valid() bool {
	switch t {
	case EntryRecharge, EntryConsume, EntryRefund,
		EntryAdjustDebit, EntryAdjustCredit, EntryFreeze, EntryUnfreeze:
		return true
	}
	return false
}

// Balance is an account's cash position (trade_db ledger, sharded by
// account_id).
//
// Available is spendable; Frozen is reserved against in-flight orders. Their
// sum is the customer's total holding, which is what a statement shows.
type Balance struct {
	AccountID int64
	Available pricing.Amount
	Frozen    pricing.Amount
	Version   int
	UpdatedAt time.Time
}

// Total returns available + frozen.
func (b Balance) Total() pricing.Amount {
	return b.Available.Add(b.Frozen)
}

// Entry is one immutable journal row.
type Entry struct {
	EntryID   int64
	AccountID int64
	Type      EntryType
	// Amount is always positive; Type carries the direction. Storing signed
	// amounts invites a sign error to silently invert a charge into a credit.
	Amount pricing.Amount
	// BalanceAfter is the available balance once this entry was applied. It
	// makes the journal self-checking: reading forward, each entry's
	// BalanceAfter must equal the previous one plus this entry's signed
	// amount, so a missing or reordered row is detectable without recomputing
	// from the beginning of time.
	BalanceAfter pricing.Amount
	// BizType and BizKey link the entry to what caused it (order, bill,
	// refund, recharge), which is what makes a statement line explicable.
	BizType string
	BizKey  string
	// IdempotencyKey makes application exactly-once. A repeated charge with
	// the same key is a no-op rather than a double-debit.
	IdempotencyKey string
	Remark         string
	OccurredAt     time.Time
}

// Signed returns the entry amount with its direction applied, for summation.
func (e Entry) Signed() pricing.Amount {
	switch e.Type.Direction() {
	case 1:
		return e.Amount
	case -1:
		return pricing.Amount(0).Sub(e.Amount)
	default:
		return 0
	}
}

// Errors.
var (
	ErrInsufficientBalance = errors.New("ledger: insufficient balance")
	ErrInsufficientFrozen  = errors.New("ledger: insufficient frozen funds")
	ErrVersionConflict     = errors.New("ledger: version conflict (concurrent balance update)")
	ErrInvalidEntryType    = errors.New("ledger: unknown entry type")
	ErrNonPositiveAmount   = errors.New("ledger: amount must be positive")
	ErrMissingIdempotency  = errors.New("ledger: idempotency key required")
	ErrDuplicateEntry      = errors.New("ledger: entry already applied (idempotent no-op)")
)

// Store persists balances and journal entries. The production implementation
// writes both in ONE local transaction — the balance update and its journal
// entry must be atomic, or a crash between them leaves a balance no journal
// explains (03§8.2 keeps the same discipline for events).
type Store interface {
	// GetBalance returns the current balance, or a zero balance for an account
	// with no ledger history yet.
	GetBalance(accountID int64) (Balance, error)
	// Apply atomically writes the entry and the new balance, guarded by
	// expectedVersion. It returns ErrVersionConflict if the balance moved.
	Apply(entry Entry, newBalance Balance, expectedVersion int) error
	// FindByIdempotencyKey returns a previously applied entry, if any.
	FindByIdempotencyKey(accountID int64, key string) (Entry, bool, error)
	// ListEntries returns journal entries in occurrence order.
	ListEntries(accountID int64) ([]Entry, error)
}

// Ledger applies money movements against a Store.
type Ledger struct {
	store Store
	now   func() time.Time
	// nextEntryID mints journal ids; production uses the segment service
	// (04§6.6), which embeds the shard factor.
	nextEntryID func() int64
}

// New builds a Ledger.
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

// Balance returns the account's current position.
func (l *Ledger) Balance(accountID int64) (Balance, error) {
	return l.store.GetBalance(accountID)
}

// request bundles the common arguments for a money movement.
type request struct {
	accountID      int64
	entryType      EntryType
	amount         pricing.Amount
	bizType        string
	bizKey         string
	idempotencyKey string
	remark         string
}

func (r request) validate() error {
	if !r.entryType.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidEntryType, r.entryType)
	}
	if r.amount.IsNegative() || r.amount.IsZero() {
		return fmt.Errorf("%w: %s", ErrNonPositiveAmount, r.amount)
	}
	if r.idempotencyKey == "" {
		return ErrMissingIdempotency
	}
	return nil
}

// apply is the single path through which every balance change flows. Routing
// all movements through one function is what keeps the invariants — positive
// amounts, idempotency, atomic journal write, non-negative available balance —
// in one place rather than repeated at each call site where one can be missed.
func (l *Ledger) apply(r request) (Entry, Balance, error) {
	if err := r.validate(); err != nil {
		return Entry{}, Balance{}, err
	}

	// Idempotency: a repeated application returns the original entry and the
	// current balance, without moving money again.
	if prior, found, err := l.store.FindByIdempotencyKey(r.accountID, r.idempotencyKey); err != nil {
		return Entry{}, Balance{}, err
	} else if found {
		bal, err := l.store.GetBalance(r.accountID)
		if err != nil {
			return Entry{}, Balance{}, err
		}
		return prior, bal, ErrDuplicateEntry
	}

	bal, err := l.store.GetBalance(r.accountID)
	if err != nil {
		return Entry{}, Balance{}, err
	}

	newBal := bal
	switch r.entryType {
	case EntryFreeze:
		if bal.Available < r.amount {
			return Entry{}, bal, fmt.Errorf("%w: available %s, need %s",
				ErrInsufficientBalance, bal.Available, r.amount)
		}
		newBal.Available = bal.Available.Sub(r.amount)
		newBal.Frozen = bal.Frozen.Add(r.amount)

	case EntryUnfreeze:
		if bal.Frozen < r.amount {
			return Entry{}, bal, fmt.Errorf("%w: frozen %s, need %s",
				ErrInsufficientFrozen, bal.Frozen, r.amount)
		}
		newBal.Frozen = bal.Frozen.Sub(r.amount)
		newBal.Available = bal.Available.Add(r.amount)

	default:
		switch r.entryType.Direction() {
		case -1:
			if bal.Available < r.amount {
				return Entry{}, bal, fmt.Errorf("%w: available %s, need %s",
					ErrInsufficientBalance, bal.Available, r.amount)
			}
			newBal.Available = bal.Available.Sub(r.amount)
		case 1:
			newBal.Available = bal.Available.Add(r.amount)
		}
	}

	// Available balance may never go negative. Overdraft is expressed as an
	// arrears state on the account (03§5.4 欠费生命周期), not as a negative
	// number here — a negative balance would silently extend credit that no
	// policy authorised.
	if newBal.Available.IsNegative() {
		return Entry{}, bal, fmt.Errorf("%w: operation would overdraw", ErrInsufficientBalance)
	}

	now := l.now()
	newBal.Version = bal.Version + 1
	newBal.UpdatedAt = now
	newBal.AccountID = r.accountID

	entry := Entry{
		EntryID:        l.nextEntryID(),
		AccountID:      r.accountID,
		Type:           r.entryType,
		Amount:         r.amount,
		BalanceAfter:   newBal.Available,
		BizType:        r.bizType,
		BizKey:         r.bizKey,
		IdempotencyKey: r.idempotencyKey,
		Remark:         r.remark,
		OccurredAt:     now,
	}

	if err := l.store.Apply(entry, newBal, bal.Version); err != nil {
		return Entry{}, bal, err
	}
	return entry, newBal, nil
}

// Recharge credits the account after a channel payment settles.
func (l *Ledger) Recharge(accountID int64, amount pricing.Amount, bizKey, idempotencyKey, remark string) (Entry, Balance, error) {
	return l.apply(request{
		accountID: accountID, entryType: EntryRecharge, amount: amount,
		bizType: "recharge", bizKey: bizKey,
		idempotencyKey: idempotencyKey, remark: remark,
	})
}

// Consume debits the account to pay an order or an hourly bill.
func (l *Ledger) Consume(accountID int64, amount pricing.Amount, bizType, bizKey, idempotencyKey, remark string) (Entry, Balance, error) {
	return l.apply(request{
		accountID: accountID, entryType: EntryConsume, amount: amount,
		bizType: bizType, bizKey: bizKey,
		idempotencyKey: idempotencyKey, remark: remark,
	})
}

// Refund credits the account when an order is refunded.
func (l *Ledger) Refund(accountID int64, amount pricing.Amount, bizKey, idempotencyKey, remark string) (Entry, Balance, error) {
	return l.apply(request{
		accountID: accountID, entryType: EntryRefund, amount: amount,
		bizType: "refund", bizKey: bizKey,
		idempotencyKey: idempotencyKey, remark: remark,
	})
}

// Freeze reserves funds for an in-flight order.
func (l *Ledger) Freeze(accountID int64, amount pricing.Amount, bizKey, idempotencyKey string) (Entry, Balance, error) {
	return l.apply(request{
		accountID: accountID, entryType: EntryFreeze, amount: amount,
		bizType: "order", bizKey: bizKey, idempotencyKey: idempotencyKey,
		remark: "freeze for order " + bizKey,
	})
}

// Unfreeze releases a reservation that was not consumed.
func (l *Ledger) Unfreeze(accountID int64, amount pricing.Amount, bizKey, idempotencyKey string) (Entry, Balance, error) {
	return l.apply(request{
		accountID: accountID, entryType: EntryUnfreeze, amount: amount,
		bizType: "order", bizKey: bizKey, idempotencyKey: idempotencyKey,
		remark: "unfreeze for order " + bizKey,
	})
}

// Adjust applies an operations correction. credit=true increases the balance.
//
// Corrections are ordinary journal entries, not edits to existing ones: the
// original entry stays as written and the correction sits beside it, so the
// history shows both what was recorded and what was fixed.
func (l *Ledger) Adjust(accountID int64, amount pricing.Amount, credit bool, bizKey, idempotencyKey, remark string) (Entry, Balance, error) {
	t := EntryAdjustDebit
	if credit {
		t = EntryAdjustCredit
	}
	return l.apply(request{
		accountID: accountID, entryType: t, amount: amount,
		bizType: "adjust", bizKey: bizKey,
		idempotencyKey: idempotencyKey, remark: remark,
	})
}

// ReconcileResult reports whether the journal reproduces the stored balance.
type ReconcileResult struct {
	AccountID       int64
	StoredAvailable pricing.Amount
	JournalSum      pricing.Amount
	Difference      pricing.Amount
	EntryCount      int
	// ChainIntact reports whether each entry's BalanceAfter follows from the
	// previous one. A break means an entry was removed, reordered, or edited.
	ChainIntact bool
	// FirstBreakAt is the entry id where the chain first fails, 0 if intact.
	FirstBreakAt int64
}

// Balanced reports whether the ledger reconciles.
func (r ReconcileResult) Balanced() bool {
	return r.Difference.IsZero() && r.ChainIntact
}

// Reconcile recomputes the balance from the journal and checks the
// BalanceAfter chain (03§8.5 对账体系, T+1).
//
// This is the last line of defence: 09 A2 requires bills to have no
// unexplained differences, and a balance that the journal cannot reproduce is
// the most unexplainable difference there is. Running it is cheap; discovering
// months later that it was never run is not.
func (l *Ledger) Reconcile(accountID int64) (ReconcileResult, error) {
	entries, err := l.store.ListEntries(accountID)
	if err != nil {
		return ReconcileResult{}, err
	}
	bal, err := l.store.GetBalance(accountID)
	if err != nil {
		return ReconcileResult{}, err
	}

	var sum pricing.Amount
	chainIntact := true
	var firstBreak int64
	var running pricing.Amount

	for _, e := range entries {
		sum = sum.Add(e.Signed())
		// Freeze/unfreeze move funds between available and frozen, so they do
		// change available even though they net zero against the total.
		switch e.Type {
		case EntryFreeze:
			running = running.Sub(e.Amount)
		case EntryUnfreeze:
			running = running.Add(e.Amount)
		default:
			running = running.Add(e.Signed())
		}
		if chainIntact && running != e.BalanceAfter {
			chainIntact = false
			firstBreak = e.EntryID
		}
	}

	// The journal sum accounts for available plus whatever is currently
	// frozen, since freezes debited available without leaving the account.
	expected := bal.Available.Add(bal.Frozen)

	return ReconcileResult{
		AccountID:       accountID,
		StoredAvailable: bal.Available,
		JournalSum:      sum,
		Difference:      sum.Sub(expected),
		EntryCount:      len(entries),
		ChainIntact:     chainIntact,
		FirstBreakAt:    firstBreak,
	}, nil
}
