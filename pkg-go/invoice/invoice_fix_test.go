package invoice

import (
	"errors"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/pricing"
)

func fixBook() *Book {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	n := 0
	return NewBook(func() time.Time { n++; return base.Add(time.Duration(n) * time.Minute) }, nil)
}

func li(desc, amt string) LineItem {
	return LineItem{Description: desc, Amount: pricing.MustParseAmount(amt)}
}

// Draft must not overwrite an ISSUED or VOIDED invoice back to DRAFT: an
// issued tax document is immutable.
func TestDraftCannotOverwriteIssuedOrVoided(t *testing.T) {
	b := fixBook()
	if _, err := b.Draft("inv-1", 100, "2026-08", "Acme", "TAX1", []LineItem{li("a", "10")}); err != nil {
		t.Fatal(err)
	}
	// Re-drafting a DRAFT (amend) is allowed.
	if _, err := b.Draft("inv-1", 100, "2026-08", "Acme", "TAX1", []LineItem{li("a", "12")}); err != nil {
		t.Fatalf("amending a draft must be allowed: %v", err)
	}
	if _, err := b.Issue("inv-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Draft("inv-1", 100, "2026-08", "Acme", "TAX1", []LineItem{li("x", "1")}); !errors.Is(err, ErrInvoiceExists) {
		t.Fatalf("drafting over ISSUED: err = %v, want ErrInvoiceExists", err)
	}
	if _, err := b.Void("inv-1", "inv-1r"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Draft("inv-1", 100, "2026-08", "Acme", "TAX1", []LineItem{li("x", "1")}); !errors.Is(err, ErrInvoiceExists) {
		t.Fatalf("drafting over VOIDED: err = %v, want ErrInvoiceExists", err)
	}
	got, _ := b.Get("inv-1")
	if got.Status != StatusVoided {
		t.Fatalf("original status = %s, want VOIDED", got.Status)
	}
}

// The caller's items slice must be copied: mutating it after Draft must not
// change what the ledger holds.
func TestDraftDeepCopiesItems(t *testing.T) {
	b := fixBook()
	items := []LineItem{li("compute", "10")}
	if _, err := b.Draft("inv-2", 100, "2026-08", "Acme", "TAX1", items); err != nil {
		t.Fatal(err)
	}
	items[0].Description = "tampered"
	items[0].Amount = pricing.MustParseAmount("999")
	got, _ := b.Get("inv-2")
	if got.Items[0].Description != "compute" || got.Items[0].Amount.String() != "10" {
		t.Fatalf("ledger items were mutated through the caller's slice: %+v", got.Items[0])
	}
}

// ListByAccount promises oldest issue first; drafts come last.
func TestListByAccountSortedOldestIssueFirst(t *testing.T) {
	b := fixBook()
	for _, id := range []string{"inv-c", "inv-a", "inv-b"} {
		if _, err := b.Draft(id, 100, "2026-08", "Acme", "TAX1", []LineItem{li("x", "1")}); err != nil {
			t.Fatal(err)
		}
	}
	// Issue in c, a order (b stays a draft).
	if _, err := b.Issue("inv-c"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Issue("inv-a"); err != nil {
		t.Fatal(err)
	}
	out := b.ListByAccount(100)
	if len(out) != 3 {
		t.Fatalf("len = %d", len(out))
	}
	gotOrder := []string{out[0].InvoiceID, out[1].InvoiceID, out[2].InvoiceID}
	want := []string{"inv-c", "inv-a", "inv-b"} // issue order, draft last
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Fatalf("order = %v, want %v", gotOrder, want)
		}
	}
}
