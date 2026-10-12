package invoice

import (
	"errors"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/pricing"
)

// NewDraft / NewReversal / SortForListing are the Book's rules exported for
// persistent stores; they must agree with the Book exactly.

func TestNewDraftAndReversalMatchBook(t *testing.T) {
	items := []LineItem{{"compute", pricing.MustParseAmount("10.5")}, {"disk", pricing.MustParseAmount("2")}}
	d, err := NewDraft("inv-1", 7, "2026-09", "Acme", "T1", items)
	if err != nil || d.Amount != pricing.MustParseAmount("12.5") || d.Status != StatusDraft {
		t.Fatalf("NewDraft = %+v, %v", d, err)
	}
	items[0].Amount = 999 // the draft owns its items
	if d.Items[0].Amount != pricing.MustParseAmount("10.5") {
		t.Fatal("NewDraft aliases the caller's items")
	}
	for _, bad := range [][]LineItem{nil, {{"x", 0}}} {
		if _, err := NewDraft("i", 7, "", "Acme", "", bad); err == nil {
			t.Fatalf("items %v accepted", bad)
		}
	}
	if _, err := NewDraft("i", 7, "", "", "", []LineItem{{"x", 1}}); !errors.Is(err, ErrEmptyTitle) {
		t.Fatalf("empty title: %v", err)
	}

	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := NewReversal(d, "inv-1-r", now); !errors.Is(err, ErrInvoiceTerminal) {
		t.Fatalf("reversing a draft: %v", err)
	}
	d.Status, d.IssuedAt = StatusIssued, now
	if _, err := NewReversal(d, "inv-1", now); !errors.Is(err, ErrReversalIDTaken) {
		t.Fatalf("self reversal: %v", err)
	}
	r, err := NewReversal(d, "inv-1-r", now)
	if err != nil || r.Amount != -d.Amount || r.ReversesID != "inv-1" || r.Status != StatusIssued ||
		r.Items[1].Description != "红冲: disk" || r.Items[1].Amount != pricing.MustParseAmount("-2") {
		t.Fatalf("reversal = %+v, %v", r, err)
	}

	b := NewBook(func() time.Time { return now }, nil)
	if _, err := b.Draft("inv-1", 7, "2026-09", "Acme", "T1", []LineItem{{"compute", pricing.MustParseAmount("10.5")}, {"disk", pricing.MustParseAmount("2")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Issue("inv-1"); err != nil {
		t.Fatal(err)
	}
	br, err := b.Void("inv-1", "inv-1-r")
	if err != nil || br.Amount != r.Amount || len(br.Items) != len(r.Items) || br.Items[0] != r.Items[0] {
		t.Fatalf("Book reversal %+v differs from NewReversal %+v (%v)", br, r, err)
	}
}

func TestSortForListing(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	invs := []Invoice{{InvoiceID: "d2"}, {InvoiceID: "b", IssuedAt: t0.Add(time.Hour)}, {InvoiceID: "d1"},
		{InvoiceID: "a2", IssuedAt: t0}, {InvoiceID: "a1", IssuedAt: t0}}
	SortForListing(invs)
	got := ""
	for _, inv := range invs {
		got += inv.InvoiceID + " "
	}
	if got != "a1 a2 b d1 d2 " {
		t.Fatalf("order = %s", got)
	}
}
