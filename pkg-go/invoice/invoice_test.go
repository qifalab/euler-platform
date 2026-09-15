package invoice

import (
	"errors"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/pricing"
)

var testNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func newBook() *Book {
	var seq int64
	return NewBook(func() time.Time { return testNow }, func() string { seq++; return "inv-auto" })
}

func yuan(s string) pricing.Amount { return pricing.MustParseAmount(s) }

func TestDraftSumsLineItems(t *testing.T) {
	b := newBook()
	inv, err := b.Draft("inv-1", 100123, "2026-08", "星辰科技", "91110MA01AB2CD", []LineItem{
		{Description: "云服务器", Amount: yuan("700")},
		{Description: "对象存储", Amount: yuan("300")},
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if inv.Amount != yuan("1000") {
		t.Fatalf("amount = %s, want 1000 (sum)", inv.Amount)
	}
	if inv.Status != StatusDraft {
		t.Fatalf("status = %s, want DRAFT", inv.Status)
	}
}

func TestDraftRejectsEmptyTitle(t *testing.T) {
	b := newBook()
	if _, err := b.Draft("inv-1", 100123, "2026-08", "", "tax", []LineItem{{Description: "x", Amount: yuan("1")}}); !errors.Is(err, ErrEmptyTitle) {
		t.Fatalf("err = %v, want ErrEmptyTitle", err)
	}
}

func TestDraftRejectsEmptyItems(t *testing.T) {
	b := newBook()
	if _, err := b.Draft("inv-1", 100123, "2026-08", "星辰科技", "tax", nil); !errors.Is(err, ErrEmptyItems) {
		t.Fatalf("err = %v, want ErrEmptyItems", err)
	}
}

func TestIssuePromotesToImmutable(t *testing.T) {
	b := newBook()
	if _, err := b.Draft("inv-1", 100123, "2026-08", "星辰科技", "tax", []LineItem{{Description: "x", Amount: yuan("1000")}}); err != nil {
		t.Fatalf("draft: %v", err)
	}
	inv, err := b.Issue("inv-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if inv.Status != StatusIssued {
		t.Fatalf("status = %s, want ISSUED", inv.Status)
	}
	if inv.IssuedAt.IsZero() {
		t.Fatal("issuedAt must be set")
	}
	// Re-issuing an issued invoice is forbidden — it is immutable.
	if _, err := b.Issue("inv-1"); !errors.Is(err, ErrInvoiceNotDraft) {
		t.Fatalf("re-issue: err = %v, want ErrInvoiceNotDraft", err)
	}
}

func TestVoidIssuesNegativeReversal(t *testing.T) {
	b := newBook()
	b.Draft("inv-1", 100123, "2026-08", "星辰科技", "tax", []LineItem{{Description: "云服务器", Amount: yuan("1000")}})
	b.Issue("inv-1")

	reversal, err := b.Void("inv-1", "rev-1")
	if err != nil {
		t.Fatalf("void: %v", err)
	}
	if reversal.Amount != yuan("-1000") {
		t.Fatalf("reversal amount = %s, want -1000 (full negative)", reversal.Amount)
	}
	if reversal.ReversesID != "inv-1" {
		t.Fatalf("reversal.ReversesID = %s, want inv-1", reversal.ReversesID)
	}
	if reversal.Status != StatusIssued {
		t.Fatalf("reversal status = %s, want ISSUED (a real tax doc)", reversal.Status)
	}
	// Original is voided and retained.
	orig, _ := b.Get("inv-1")
	if orig.Status != StatusVoided {
		t.Fatalf("original status = %s, want VOIDED", orig.Status)
	}
	if orig.VoidedByID != "rev-1" {
		t.Fatalf("original.VoidedByID = %s, want rev-1", orig.VoidedByID)
	}
	if orig.Amount != yuan("1000") {
		t.Fatalf("original amount changed after void: %s, want 1000 (immutable)", orig.Amount)
	}
}

func TestVoidIsIdempotent(t *testing.T) {
	b := newBook()
	b.Draft("inv-1", 100123, "2026-08", "星辰科技", "tax", []LineItem{{Description: "x", Amount: yuan("500")}})
	b.Issue("inv-1")
	rev1, err := b.Void("inv-1", "rev-1")
	if err != nil {
		t.Fatalf("void 1: %v", err)
	}
	// Voiding again returns the SAME reversal — does not create a second.
	rev2, err := b.Void("inv-1", "rev-1")
	if err != nil {
		t.Fatalf("void 2: %v", err)
	}
	if rev1.InvoiceID != rev2.InvoiceID {
		t.Fatalf("idempotent void returned different reversal: %s vs %s", rev1.InvoiceID, rev2.InvoiceID)
	}
}

func TestVoidRejectsDraft(t *testing.T) {
	b := newBook()
	b.Draft("inv-1", 100123, "2026-08", "星辰科技", "tax", []LineItem{{Description: "x", Amount: yuan("500")}})
	// A DRAFT invoice may not be voided — issue it first or delete the draft.
	if _, err := b.Void("inv-1", "rev-1"); !errors.Is(err, ErrInvoiceTerminal) {
		t.Fatalf("void draft: err = %v, want ErrInvoiceTerminal", err)
	}
}

func TestVoidedInvoiceCannotBeReVoided(t *testing.T) {
	b := newBook()
	b.Draft("inv-1", 100123, "2026-08", "星辰科技", "tax", []LineItem{{Description: "x", Amount: yuan("500")}})
	b.Issue("inv-1")
	b.Void("inv-1", "rev-1")
	// A second distinct reversal id against the already-voided original.
	if _, err := b.Void("inv-1", "rev-2"); !errors.Is(err, ErrInvoiceAlreadyVoid) {
		t.Fatalf("re-void: err = %v, want ErrInvoiceAlreadyVoid", err)
	}
}

// TestNetPositionIsZeroAfterVoid proves the financial invariant behind B6
// (发票红冲通过税务评审): an issued invoice + its 红冲 reversal net to zero, so
// the tax books show no revenue for a reversed period.
func TestNetPositionIsZeroAfterVoid(t *testing.T) {
	b := newBook()
	b.Draft("inv-1", 100123, "2026-08", "星辰科技", "tax", []LineItem{
		{Description: "云服务器", Amount: yuan("700")},
		{Description: "对象存储", Amount: yuan("300")},
	})
	b.Issue("inv-1")
	orig, _ := b.Get("inv-1")
	rev, _ := b.Void("inv-1", "rev-1")
	net := orig.Amount.Add(rev.Amount)
	if !net.IsZero() {
		t.Fatalf("issued + reversal = %s, want 0 (net zero after 红冲)", net)
	}
}
