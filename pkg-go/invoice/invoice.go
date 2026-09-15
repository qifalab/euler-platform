// Package invoice implements the invoice document model and the 红冲
// (red-credit) reversal — the financial-compliance layer opened in phase 2
// (09-roadmap M-4.3, B6).
//
// An invoice is a tax document issued against a settled bill period. Unlike a
// bill (which the billing engine generates from metered usage), an invoice is a
// document the customer requests and the tax authority requires — it carries a
// title (发票抬头) and tax number, and once issued it is immutable.
//
// # 红冲 is a reversal, not a delete
//
// Chinese tax law forbids deleting an issued invoice. To reverse one, a 红字
// 发票 (red-credit invoice) is issued for the full negative amount; the
// original invoice moves to VOIDED but its row is retained — exactly as the
// audit chain retains its history (pkg-go/audit). Terminal states are
// irreversible: a VOIDED invoice cannot be re-issued or re-voided.
//
// # Money is fixed-point
//
// Invoice amounts reuse pricing.Amount (micro-units, never float), so an
// invoice and the bill it derives from can be reconciled to the cent across
// millions of rows — the same 无未解释差异 rule (09 A2) that governs billing.
package invoice

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/pricing"
)

// Status is the lifecycle state of an invoice document.
type Status string

const (
	// StatusDraft means the invoice is not yet issued (edit window).
	StatusDraft Status = "DRAFT"
	// StatusIssued means the invoice is a live tax document; immutable.
	StatusIssued Status = "ISSUED"
	// StatusVoided means a 红冲 invoice reversed this one; retained for audit.
	StatusVoided Status = "VOIDED"
)

// IsTerminal reports whether the state may not transition further.
func (s Status) IsTerminal() bool { return s == StatusIssued || s == StatusVoided }

// LineItem is one row on an invoice.
type LineItem struct {
	Description string
	Amount      pricing.Amount
}

// Invoice is one tax document.
type Invoice struct {
	InvoiceID  string
	AccountID  int64
	BillPeriod string // "2026-08"
	Title      string // 发票抬头
	TaxNo      string // 纳税人识别号
	// Amount is the total (sum of items), always non-negative. A 红冲 invoice
	// carries the negative of the original as its amount.
	Amount   pricing.Amount
	Items    []LineItem
	Status   Status
	IssuedAt time.Time
	VoidedAt time.Time
	// VoidedByID links to the 红冲 invoice that reversed this one; empty on a
	// live or un-voided invoice.
	VoidedByID string
	// ReversesID is set on a 红冲 invoice: the original it reverses. Empty on
	// a normal invoice.
	ReversesID string
}

// Errors.
var (
	ErrInvoiceNotFound    = errors.New("invoice: invoice not found")
	ErrInvoiceNotDraft    = errors.New("invoice: only a DRAFT invoice may be issued")
	ErrInvoiceExists      = errors.New("invoice: invoice id already used by a non-draft invoice")
	ErrInvoiceTerminal    = errors.New("invoice: invoice is in a terminal state")
	ErrInvoiceAlreadyVoid = errors.New("invoice: invoice already voided")
	ErrMissingReversalID  = errors.New("invoice: 红冲 requires a reversal invoice id")
	ErrReversalIDTaken    = errors.New("invoice: reversal id is already used by another invoice")
	ErrVoidAmountMismatch = errors.New("invoice: 红冲 amount must equal the original")
	ErrEmptyTitle         = errors.New("invoice: title (发票抬头) required")
	ErrEmptyItems         = errors.New("invoice: invoice must have at least one line item")
)

// Book is the invoice ledger. It issues and voids invoices, enforcing the
// terminal-irreversibility and 红冲-equality invariants.
type Book struct {
	mu       sync.Mutex
	invoices map[string]Invoice
	now      func() time.Time
	nextSeq  func() string
}

// NewBook builds a Book. nextSeq mints invoice ids.
func NewBook(now func() time.Time, nextSeq func() string) *Book {
	if now == nil {
		now = time.Now
	}
	if nextSeq == nil {
		var n int64
		nextSeq = func() string { n++; return fmt.Sprintf("inv-%d", n) }
	}
	return &Book{invoices: make(map[string]Invoice), now: now, nextSeq: nextSeq}
}

// Draft creates a DRAFT invoice from a bill period's settled charges. The
// amount is summed from items; draft may be amended before Issue — but an id
// already held by an ISSUED or VOIDED invoice may not be reused: overwriting
// it would silently pull an immutable tax document back to an editable draft.
func (b *Book) Draft(invoiceID string, accountID int64, billPeriod, title, taxNo string, items []LineItem) (Invoice, error) {
	if title == "" {
		return Invoice{}, ErrEmptyTitle
	}
	if len(items) == 0 {
		return Invoice{}, ErrEmptyItems
	}
	// Deep-copy the items so a caller mutating its slice after Draft cannot
	// change what the ledger holds.
	own := make([]LineItem, len(items))
	copy(own, items)
	var total pricing.Amount
	for _, it := range own {
		total = total.Add(it.Amount)
	}
	inv := Invoice{
		InvoiceID:  invoiceID,
		AccountID:  accountID,
		BillPeriod: billPeriod,
		Title:      title,
		TaxNo:      taxNo,
		Amount:     total,
		Items:      own,
		Status:     StatusDraft,
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if existing, ok := b.invoices[invoiceID]; ok && existing.Status != StatusDraft {
		return Invoice{}, fmt.Errorf("%w: %s is %s", ErrInvoiceExists, invoiceID, existing.Status)
	}
	b.invoices[invoiceID] = inv
	return inv, nil
}

// Issue promotes a DRAFT to ISSUED. Once issued, the invoice is immutable. An
// already-issued or voided invoice cannot be re-issued.
func (b *Book) Issue(invoiceID string) (Invoice, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	inv, ok := b.invoices[invoiceID]
	if !ok {
		return Invoice{}, ErrInvoiceNotFound
	}
	if inv.Status != StatusDraft {
		return inv, fmt.Errorf("%w: %s", ErrInvoiceNotDraft, inv.Status)
	}
	inv.Status = StatusIssued
	inv.IssuedAt = b.now()
	b.invoices[invoiceID] = inv
	return inv, nil
}

// Void issues a 红字发票 reversing a previously-issued invoice. The original
// moves to VOIDED (retained for audit); the 红冲 invoice is a separate, issued
// invoice with a negative amount. Returns the void reversal invoice.
//
// Void is idempotent: voiding an already-voided invoice returns the existing
// reversal rather than creating a second one.
func (b *Book) Void(originalID, reversalID string) (Invoice, error) {
	// The reversal is a NEW tax document and needs its own id. An empty or
	// self-referencing id would collide with the original (or with a later
	// document) and silently replace a record in the book.
	if reversalID == "" {
		return Invoice{}, ErrMissingReversalID
	}
	if reversalID == originalID {
		return Invoice{}, fmt.Errorf("%w: %s is the original", ErrReversalIDTaken, reversalID)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	orig, ok := b.invoices[originalID]
	if !ok {
		return Invoice{}, ErrInvoiceNotFound
	}
	if orig.Status == StatusVoided {
		if orig.VoidedByID == reversalID {
			// Idempotent: the same reversal replayed returns the existing one.
			return b.invoices[orig.VoidedByID], nil
		}
		// A second, distinct reversal against an already-voided original is
		// forbidden — an invoice is reversed exactly once.
		return Invoice{}, ErrInvoiceAlreadyVoid
	}
	if orig.Status != StatusIssued {
		return Invoice{}, fmt.Errorf("%w: only an ISSUED invoice may be voided, got %s", ErrInvoiceTerminal, orig.Status)
	}
	// Never overwrite an existing document: 红冲 adds a document, it does not
	// replace one. A taken id is an operator error, not a reversal.
	if _, taken := b.invoices[reversalID]; taken {
		return Invoice{}, fmt.Errorf("%w: %s", ErrReversalIDTaken, reversalID)
	}

	// Build the 红冲 invoice: same title/tax/period, negative amount.
	reversalItems := make([]LineItem, len(orig.Items))
	for i, it := range orig.Items {
		reversalItems[i] = LineItem{
			Description: "红冲: " + it.Description,
			Amount:      pricing.Amount(0).Sub(it.Amount),
		}
	}
	reversal := Invoice{
		InvoiceID:  reversalID,
		AccountID:  orig.AccountID,
		BillPeriod: orig.BillPeriod,
		Title:      orig.Title,
		TaxNo:      orig.TaxNo,
		Amount:     pricing.Amount(0).Sub(orig.Amount), // negative
		Items:      reversalItems,
		Status:     StatusIssued,
		IssuedAt:   b.now(),
		ReversesID: originalID,
	}

	orig.Status = StatusVoided
	orig.VoidedAt = b.now()
	orig.VoidedByID = reversalID
	b.invoices[originalID] = orig
	b.invoices[reversalID] = reversal
	return reversal, nil
}

// Get returns an invoice by id.
func (b *Book) Get(invoiceID string) (Invoice, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	inv, ok := b.invoices[invoiceID]
	return inv, ok
}

// ListByAccount returns all invoices for an account, oldest issue first.
// Drafts (not yet issued) sort after every issued invoice; ties break on
// invoice id so the order is deterministic.
func (b *Book) ListByAccount(accountID int64) []Invoice {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Invoice
	for _, inv := range b.invoices {
		if inv.AccountID == accountID {
			out = append(out, inv)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].IssuedAt, out[j].IssuedAt
		switch {
		case ti.IsZero() && tj.IsZero():
			return out[i].InvoiceID < out[j].InvoiceID
		case ti.IsZero():
			return false
		case tj.IsZero():
			return true
		case ti.Equal(tj):
			return out[i].InvoiceID < out[j].InvoiceID
		default:
			return ti.Before(tj)
		}
	})
	return out
}
