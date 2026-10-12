package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/qifalab/euler-platform/invoice"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/storage"
)

// invoiceStore keeps the invoice documents (pkg-go/invoice) — trade_db's
// invoice / invoice_item tables (svc-billing V3/V5) when persistent, an
// invoice.Book otherwise.
//
// # The spend cap is checked where it is written
//
// An account may only invoice what it actually paid: cash spent (CONSUME −
// REFUND in its ledger, supplied by the caller as spent) minus what its live
// invoices (DRAFT/ISSUED, positive amounts) already claim. That is a
// constraint over ALL of the account's invoices, so no row lock on one
// invoice can hold it. Draft and Issue therefore check it inside the store's
// per-account critical section — a SELECT ... FOR UPDATE on the account's
// invoice_lock row in SQL mode, a mutex in memory — and write in the same
// section: two concurrent drafts (on any replica) cannot each claim the same
// quota, and one draft cannot be issued twice (the ISSUE is also a conditional
// UPDATE ... WHERE status = DRAFT).
type invoiceStore interface {
	// Draft creates (empty id: minted "inv-{n}") or amends a DRAFT.
	// Errors: *exceedsSpendError, invoice.ErrInvoiceExists (the id is
	// another account's, or no longer a draft), the invoice.NewDraft
	// validation errors.
	Draft(acct int64, invoiceID, billPeriod, title, taxNo string, items []invoice.LineItem, spent pricing.Amount) (invoice.Invoice, error)
	// Issue promotes the account's DRAFT to ISSUED. Errors:
	// invoice.ErrInvoiceNotFound, invoice.ErrInvoiceNotDraft,
	// *exceedsSpendError (a refund since the draft shrank the quota).
	Issue(acct int64, invoiceID string, spent pricing.Amount) (invoice.Invoice, error)
	// Void issues the 红冲 reversal of the account's ISSUED invoice
	// (idempotent for the same reversal id) and returns it.
	Void(acct int64, originalID, reversalID string) (invoice.Invoice, error)
	// Get returns an invoice by id (any account; callers check ownership).
	Get(invoiceID string) (invoice.Invoice, bool, error)
	// List returns the account's invoices in invoice.SortForListing order.
	List(acct int64) ([]invoice.Invoice, error)
}

// exceedsSpendError: the amount would exceed what the account may still
// invoice.
type exceedsSpendError struct{ Avail pricing.Amount }

func (e *exceedsSpendError) Error() string {
	return "invoice: amount exceeds the invoiceable amount " + e.Avail.String()
}

// invoiceAvail is spent minus the live invoices' claim, excluding the draft
// being amended or issued (its own amount is not counted twice).
func invoiceAvail(spent pricing.Amount, invs []invoice.Invoice, excludeDraftID string) pricing.Amount {
	avail := spent
	for _, inv := range invs {
		if inv.InvoiceID == excludeDraftID && inv.Status == invoice.StatusDraft {
			continue
		}
		if (inv.Status == invoice.StatusDraft || inv.Status == invoice.StatusIssued) && inv.Amount > 0 {
			avail = avail.Sub(inv.Amount)
		}
	}
	if avail < 0 {
		avail = 0
	}
	return avail
}

// --- in-memory store -------------------------------------------------------------

// memInvoiceStore is the single-process twin: an invoice.Book plus the mutex
// that makes check-and-write one step.
type memInvoiceStore struct {
	mu   sync.Mutex
	book *invoice.Book
}

func newMemInvoiceStore(now func() time.Time) *memInvoiceStore {
	var (
		seqMu sync.Mutex
		seq   int64
	)
	return &memInvoiceStore{book: invoice.NewBook(now, func() string {
		seqMu.Lock()
		defer seqMu.Unlock()
		seq++
		return fmt.Sprintf("inv-%d", seq)
	})}
}

func (m *memInvoiceStore) Draft(acct int64, invoiceID, billPeriod, title, taxNo string, items []invoice.LineItem, spent pricing.Amount) (invoice.Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	draft, err := invoice.NewDraft(invoiceID, acct, billPeriod, title, taxNo, items)
	if err != nil {
		return invoice.Invoice{}, err
	}
	if avail := invoiceAvail(spent, m.book.ListByAccount(acct), invoiceID); draft.Amount > avail {
		return invoice.Invoice{}, &exceedsSpendError{Avail: avail}
	}
	return m.book.Draft(invoiceID, acct, billPeriod, title, taxNo, items)
}

func (m *memInvoiceStore) Issue(acct int64, invoiceID string, spent pricing.Amount) (invoice.Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.book.Get(invoiceID)
	if !ok || inv.AccountID != acct {
		return invoice.Invoice{}, invoice.ErrInvoiceNotFound
	}
	if inv.Status != invoice.StatusDraft {
		return inv, fmt.Errorf("%w: %s", invoice.ErrInvoiceNotDraft, inv.Status)
	}
	if avail := invoiceAvail(spent, m.book.ListByAccount(acct), invoiceID); inv.Amount > avail {
		return invoice.Invoice{}, &exceedsSpendError{Avail: avail}
	}
	return m.book.Issue(invoiceID)
}

func (m *memInvoiceStore) Void(acct int64, originalID, reversalID string) (invoice.Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if orig, ok := m.book.Get(originalID); !ok || orig.AccountID != acct {
		return invoice.Invoice{}, invoice.ErrInvoiceNotFound
	}
	return m.book.Void(originalID, reversalID)
}

func (m *memInvoiceStore) Get(invoiceID string) (invoice.Invoice, bool, error) {
	inv, ok := m.book.Get(invoiceID)
	return inv, ok, nil
}

func (m *memInvoiceStore) List(acct int64) ([]invoice.Invoice, error) {
	return m.book.ListByAccount(acct), nil
}

// --- SQL store ---------------------------------------------------------------------

// sqlInvoiceStore keeps the documents in trade_db's invoice / invoice_item.
type sqlInvoiceStore struct {
	db  *sql.DB
	ids *storage.Sequence // mints "inv-{n}" for drafts without an id
	now func() time.Time
}

const invoiceTimeout = 10 * time.Second

// invoice.status (svc-billing V3 DDL): 1DRAFT 2ISSUED 3VOIDED.
func invoiceStatusCode(s invoice.Status) int {
	switch s {
	case invoice.StatusIssued:
		return 2
	case invoice.StatusVoided:
		return 3
	default:
		return 1
	}
}

func invoiceStatusFrom(code int) invoice.Status {
	switch code {
	case 2:
		return invoice.StatusIssued
	case 3:
		return invoice.StatusVoided
	default:
		return invoice.StatusDraft
	}
}

// invoiceCents renders an amount for the DECIMAL(12,2) columns; a value with
// sub-分 digits would be silently rounded by the column, so it is refused.
func invoiceCents(a pricing.Amount) (string, error) {
	if int64(a)%10_000 != 0 {
		return "", fmt.Errorf("invoice: amount %s is not a whole number of 分", a)
	}
	return a.String(), nil
}

// sqlQuerier is a pool or a transaction.
type sqlQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const invoiceColumns = `invoice_id, account_id, bill_period, title, tax_no, amount, status,
	issued_at, voided_at, voided_by_id, reverses_id`

func scanInvoice(sc interface{ Scan(...any) error }) (invoice.Invoice, error) {
	var (
		inv            invoice.Invoice
		amount         string
		status         int
		issued, voided sql.NullTime
	)
	if err := sc.Scan(&inv.InvoiceID, &inv.AccountID, &inv.BillPeriod, &inv.Title, &inv.TaxNo, &amount, &status,
		&issued, &voided, &inv.VoidedByID, &inv.ReversesID); err != nil {
		return invoice.Invoice{}, err
	}
	var err error
	if inv.Amount, err = pricing.ParseAmount(amount); err != nil {
		return invoice.Invoice{}, fmt.Errorf("invoice %s amount: %w", inv.InvoiceID, err)
	}
	inv.Status = invoiceStatusFrom(status)
	inv.IssuedAt, inv.VoidedAt = issued.Time, voided.Time
	return inv, nil
}

// loadInvoices reads invoices matching where, with their items.
func loadInvoices(ctx context.Context, q sqlQuerier, where string, args ...any) ([]invoice.Invoice, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+invoiceColumns+` FROM invoice `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("invoice: query: %w", err)
	}
	var out []invoice.Invoice
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("invoice: scan: %w", err)
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		items, err := loadItems(ctx, q, out[i].AccountID, out[i].InvoiceID)
		if err != nil {
			return nil, err
		}
		out[i].Items = items
	}
	return out, nil
}

func loadItems(ctx context.Context, q sqlQuerier, acct int64, invoiceID string) ([]invoice.LineItem, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT description, amount FROM invoice_item WHERE account_id = ? AND invoice_id = ? ORDER BY seq`, acct, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("invoice: items of %s: %w", invoiceID, err)
	}
	defer rows.Close()
	var out []invoice.LineItem
	for rows.Next() {
		var (
			it  invoice.LineItem
			amt string
		)
		if err := rows.Scan(&it.Description, &amt); err != nil {
			return nil, fmt.Errorf("invoice: items scan: %w", err)
		}
		if it.Amount, err = pricing.ParseAmount(amt); err != nil {
			return nil, fmt.Errorf("invoice: item amount: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// lockAccount takes the account's invoice lock for the rest of tx.
func lockAccount(ctx context.Context, tx *sql.Tx, acct int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO invoice_lock (account_id) VALUES (?)`, acct); err != nil {
		return fmt.Errorf("invoice: create lock row: %w", err)
	}
	var held int64
	if err := tx.QueryRowContext(ctx, `SELECT account_id FROM invoice_lock WHERE account_id = ? FOR UPDATE`, acct).Scan(&held); err != nil {
		return fmt.Errorf("invoice: take lock: %w", err)
	}
	return nil
}

// insertInvoice writes a new document and its lines.
func insertInvoice(ctx context.Context, tx *sql.Tx, inv invoice.Invoice) error {
	amount, err := invoiceCents(inv.Amount)
	if err != nil {
		return err
	}
	var issued, voided any
	if !inv.IssuedAt.IsZero() {
		issued = inv.IssuedAt.UTC()
	}
	if !inv.VoidedAt.IsZero() {
		voided = inv.VoidedAt.UTC()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO invoice (invoice_id, account_id, bill_period, title, tax_no, amount, status,
		                      issued_at, voided_at, voided_by_id, reverses_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.InvoiceID, inv.AccountID, inv.BillPeriod, inv.Title, inv.TaxNo, amount, invoiceStatusCode(inv.Status),
		issued, voided, inv.VoidedByID, inv.ReversesID); err != nil {
		if storage.IsDuplicateKey(err) {
			return fmt.Errorf("%w: %s", invoice.ErrInvoiceExists, inv.InvoiceID)
		}
		return fmt.Errorf("invoice: insert %s: %w", inv.InvoiceID, err)
	}
	return insertItems(ctx, tx, inv)
}

func insertItems(ctx context.Context, tx *sql.Tx, inv invoice.Invoice) error {
	for i, it := range inv.Items {
		amt, err := invoiceCents(it.Amount)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO invoice_item (invoice_id, account_id, description, amount, seq) VALUES (?, ?, ?, ?, ?)`,
			inv.InvoiceID, inv.AccountID, it.Description, amt, i+1); err != nil {
			return fmt.Errorf("invoice: item %d of %s: %w", i+1, inv.InvoiceID, err)
		}
	}
	return nil
}

// lockedInvoice reads one invoice under FOR UPDATE (nil when absent).
func lockedInvoice(ctx context.Context, tx *sql.Tx, invoiceID string) (*invoice.Invoice, error) {
	inv, err := scanInvoice(tx.QueryRowContext(ctx,
		`SELECT `+invoiceColumns+` FROM invoice WHERE invoice_id = ? FOR UPDATE`, invoiceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("invoice: read %s: %w", invoiceID, err)
	}
	items, err := loadItems(ctx, tx, inv.AccountID, inv.InvoiceID)
	if err != nil {
		return nil, err
	}
	inv.Items = items
	return &inv, nil
}

// liveInvoices is the account's DRAFT/ISSUED invoices (amounts only), read
// under the account lock.
func liveInvoices(ctx context.Context, tx *sql.Tx, acct int64) ([]invoice.Invoice, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT invoice_id, amount, status FROM invoice WHERE account_id = ? AND status IN (1, 2)`, acct)
	if err != nil {
		return nil, fmt.Errorf("invoice: live invoices of %d: %w", acct, err)
	}
	defer rows.Close()
	var out []invoice.Invoice
	for rows.Next() {
		var (
			inv    invoice.Invoice
			amt    string
			status int
		)
		if err := rows.Scan(&inv.InvoiceID, &amt, &status); err != nil {
			return nil, err
		}
		if inv.Amount, err = pricing.ParseAmount(amt); err != nil {
			return nil, err
		}
		inv.Status = invoiceStatusFrom(status)
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *sqlInvoiceStore) Draft(acct int64, invoiceID, billPeriod, title, taxNo string, items []invoice.LineItem, spent pricing.Amount) (invoice.Invoice, error) {
	draft, err := invoice.NewDraft(invoiceID, acct, billPeriod, title, taxNo, items)
	if err != nil {
		return invoice.Invoice{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), invoiceTimeout)
	defer cancel()
	var out invoice.Invoice
	err = storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockAccount(ctx, tx, acct); err != nil {
			return err
		}
		inv := draft
		var existing *invoice.Invoice
		if inv.InvoiceID == "" {
			for {
				n, err := s.ids.Next(ctx)
				if err != nil {
					return err
				}
				inv.InvoiceID = "inv-" + strconv.FormatInt(n, 10)
				if existing, err = lockedInvoice(ctx, tx, inv.InvoiceID); err != nil {
					return err
				} else if existing == nil {
					break
				}
			}
		} else if existing, err = lockedInvoice(ctx, tx, inv.InvoiceID); err != nil {
			return err
		}
		live, err := liveInvoices(ctx, tx, acct)
		if err != nil {
			return err
		}
		if avail := invoiceAvail(spent, live, inv.InvoiceID); inv.Amount > avail {
			return &exceedsSpendError{Avail: avail}
		}
		if existing != nil {
			// Ids are global: another account's id, or a document that is no
			// longer a draft, is never overwritten.
			if existing.AccountID != acct {
				return fmt.Errorf("%w: %s", invoice.ErrInvoiceExists, inv.InvoiceID)
			}
			if existing.Status != invoice.StatusDraft {
				return fmt.Errorf("%w: %s is %s", invoice.ErrInvoiceExists, inv.InvoiceID, existing.Status)
			}
		}
		if existing == nil {
			if err := insertInvoice(ctx, tx, inv); err != nil {
				return err
			}
		} else {
			amount, err := invoiceCents(inv.Amount)
			if err != nil {
				return err
			}
			// The row is locked and known to be this account's DRAFT; an
			// amend that changes nothing affects zero rows and is still fine.
			if _, err := tx.ExecContext(ctx,
				`UPDATE invoice SET bill_period = ?, title = ?, tax_no = ?, amount = ?
				  WHERE invoice_id = ? AND account_id = ? AND status = 1`,
				inv.BillPeriod, inv.Title, inv.TaxNo, amount, inv.InvoiceID, acct); err != nil {
				return fmt.Errorf("invoice: amend %s: %w", inv.InvoiceID, err)
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM invoice_item WHERE account_id = ? AND invoice_id = ?`, acct, inv.InvoiceID); err != nil {
				return fmt.Errorf("invoice: amend items of %s: %w", inv.InvoiceID, err)
			}
			if err := insertItems(ctx, tx, inv); err != nil {
				return err
			}
		}
		out = inv
		return nil
	})
	if err != nil {
		return invoice.Invoice{}, err
	}
	return out, nil
}

func (s *sqlInvoiceStore) Issue(acct int64, invoiceID string, spent pricing.Amount) (invoice.Invoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), invoiceTimeout)
	defer cancel()
	var out invoice.Invoice
	err := storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockAccount(ctx, tx, acct); err != nil {
			return err
		}
		inv, err := lockedInvoice(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		if inv == nil || inv.AccountID != acct {
			return invoice.ErrInvoiceNotFound
		}
		if inv.Status != invoice.StatusDraft {
			return fmt.Errorf("%w: %s", invoice.ErrInvoiceNotDraft, inv.Status)
		}
		live, err := liveInvoices(ctx, tx, acct)
		if err != nil {
			return err
		}
		if avail := invoiceAvail(spent, live, invoiceID); inv.Amount > avail {
			return &exceedsSpendError{Avail: avail}
		}
		now := s.now().UTC().Truncate(time.Second) // issued_at is DATETIME
		if err := storage.Affected(tx.ExecContext(ctx,
			`UPDATE invoice SET status = 2, issued_at = ? WHERE invoice_id = ? AND account_id = ? AND status = 1`,
			now, invoiceID, acct)); err != nil {
			return fmt.Errorf("%w: %s", invoice.ErrInvoiceNotDraft, invoiceID)
		}
		inv.Status, inv.IssuedAt = invoice.StatusIssued, now
		out = *inv
		return nil
	})
	if err != nil {
		return invoice.Invoice{}, err
	}
	return out, nil
}

func (s *sqlInvoiceStore) Void(acct int64, originalID, reversalID string) (invoice.Invoice, error) {
	if reversalID == "" {
		return invoice.Invoice{}, invoice.ErrMissingReversalID
	}
	ctx, cancel := context.WithTimeout(context.Background(), invoiceTimeout)
	defer cancel()
	var out invoice.Invoice
	err := storage.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockAccount(ctx, tx, acct); err != nil {
			return err
		}
		orig, err := lockedInvoice(ctx, tx, originalID)
		if err != nil {
			return err
		}
		if orig == nil || orig.AccountID != acct {
			return invoice.ErrInvoiceNotFound
		}
		if orig.Status == invoice.StatusVoided {
			if orig.VoidedByID != reversalID {
				return invoice.ErrInvoiceAlreadyVoid
			}
			// The same reversal replayed: return the existing document.
			rev, err := lockedInvoice(ctx, tx, reversalID)
			if err != nil {
				return err
			}
			if rev == nil {
				return fmt.Errorf("invoice: %s is voided by %s, which is missing", originalID, reversalID)
			}
			out = *rev
			return nil
		}
		now := s.now().UTC().Truncate(time.Second)
		reversal, err := invoice.NewReversal(*orig, reversalID, now)
		if err != nil {
			return err
		}
		if taken, err := lockedInvoice(ctx, tx, reversalID); err != nil {
			return err
		} else if taken != nil {
			return fmt.Errorf("%w: %s", invoice.ErrReversalIDTaken, reversalID)
		}
		if err := insertInvoice(ctx, tx, reversal); err != nil {
			if errors.Is(err, invoice.ErrInvoiceExists) {
				return fmt.Errorf("%w: %s", invoice.ErrReversalIDTaken, reversalID)
			}
			return err
		}
		if err := storage.Affected(tx.ExecContext(ctx,
			`UPDATE invoice SET status = 3, voided_at = ?, voided_by_id = ?
			  WHERE invoice_id = ? AND account_id = ? AND status = 2`,
			now, reversalID, originalID, acct)); err != nil {
			return fmt.Errorf("invoice: void %s: %w", originalID, err)
		}
		out = reversal
		return nil
	})
	if err != nil {
		return invoice.Invoice{}, err
	}
	return out, nil
}

func (s *sqlInvoiceStore) Get(invoiceID string) (invoice.Invoice, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), invoiceTimeout)
	defer cancel()
	invs, err := loadInvoices(ctx, s.db, `WHERE invoice_id = ?`, invoiceID)
	if err != nil || len(invs) == 0 {
		return invoice.Invoice{}, false, err
	}
	return invs[0], true, nil
}

func (s *sqlInvoiceStore) List(acct int64) ([]invoice.Invoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), invoiceTimeout)
	defer cancel()
	invs, err := loadInvoices(ctx, s.db, `WHERE account_id = ?`, acct)
	if err != nil {
		return nil, err
	}
	invoice.SortForListing(invs)
	return invs, nil
}
