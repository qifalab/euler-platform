package invoice

import (
	"errors"
	"testing"
	"time"

	"github.com/starcloud/sc-platform/pricing"
)

// 红冲 adds a NEW document; it must never overwrite one. An empty id used to
// write invoices[""] (and be replaced by the next void), and an id already in
// use silently replaced that document.
func TestVoidRequiresAUsableReversalID(t *testing.T) {
	b := NewBook(func() time.Time { return time.Unix(0, 0) }, func() string { return "seq" })
	items := []LineItem{{Description: "compute", Amount: pricing.MustParseAmount("100")}}
	mustDraftAndIssue := func(id string) {
		t.Helper()
		if _, err := b.Draft(id, 100123, "2026-08", "title", "taxno", items); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Issue(id); err != nil {
			t.Fatal(err)
		}
	}
	mustDraftAndIssue("inv-1")

	if _, err := b.Void("inv-1", ""); !errors.Is(err, ErrMissingReversalID) {
		t.Fatalf("empty reversal id: err = %v, want ErrMissingReversalID", err)
	}
	if _, err := b.Void("inv-1", "inv-1"); !errors.Is(err, ErrReversalIDTaken) {
		t.Fatalf("self-referencing reversal id: err = %v, want ErrReversalIDTaken", err)
	}
	mustDraftAndIssue("inv-2")
	if _, err := b.Void("inv-1", "inv-2"); !errors.Is(err, ErrReversalIDTaken) {
		t.Fatalf("taken reversal id: err = %v, want ErrReversalIDTaken", err)
	}

	// The happy path still reverses exactly once, and a replay is idempotent.
	rev, err := b.Void("inv-1", "rev-1")
	if err != nil {
		t.Fatalf("Void: %v", err)
	}
	if !rev.Amount.IsNegative() {
		t.Fatalf("reversal amount = %s, want negative", rev.Amount)
	}
	again, err := b.Void("inv-1", "rev-1")
	if err != nil || again.InvoiceID != "rev-1" {
		t.Fatalf("idempotent replay = %q / %v, want rev-1", again.InvoiceID, err)
	}
	// The original stays as a VOIDED record, and the other document is intact.
	if orig, ok := b.Get("inv-1"); !ok || orig.Status != StatusVoided {
		t.Fatalf("original = %+v / %v, want a retained VOIDED record", orig, ok)
	}
	if other, ok := b.Get("inv-2"); !ok || other.Status != StatusIssued {
		t.Fatalf("bystander document was modified: %+v / %v", other, ok)
	}
}
