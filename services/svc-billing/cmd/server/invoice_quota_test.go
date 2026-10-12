package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/pricing"
)

// spend records real cash consumption for acct: recharge then consume.
func spend(t *testing.T, a *app, acct int64, amount string) {
	t.Helper()
	amt := pricing.MustParseAmount(amount)
	if _, _, err := a.ledger.Recharge(acct, amt, "seed", "seed-"+amount+"-"+itoa(acct), "seed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.ledger.Consume(acct, amt, "order", "o-"+itoa(acct), "spend-"+amount+"-"+itoa(acct), "spend"); err != nil {
		t.Fatal(err)
	}
}

func itoa(v int64) string { return pricing.Amount(v * 1_000_000).String() }

func draftInvoice(t *testing.T, a *app, acct int64, id, amount string) int {
	t.Helper()
	body := map[string]any{"billPeriod": "2026-09", "title": "Acme", "taxNo": "T1",
		"items": []map[string]any{{"description": "compute", "amount": amount}}}
	if id != "" {
		body["invoiceId"] = id
	}
	rr := doReq(t, a.handleInvoices, http.MethodPost, "/internal/invoices", acct, body)
	return rr.Code
}

// Regression: invoices accepted any amount the caller typed, so a tenant could
// issue 发票 for money it never paid.
func TestInvoiceCannotExceedSpend(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(4001)
	if code := draftInvoice(t, a, acct, "", "1"); code != http.StatusConflict {
		t.Fatalf("draft with no spend: %d, want 409", code)
	}
	spend(t, a, acct, "100")
	if code := draftInvoice(t, a, acct, "inv-q1", "100.01"); code != http.StatusConflict {
		t.Fatalf("draft above spend: %d, want 409", code)
	}
	if code := draftInvoice(t, a, acct, "inv-q1", "60"); code != http.StatusOK {
		t.Fatalf("draft within spend: %d", code)
	}
	// Amending the same draft does not count its old amount twice.
	if code := draftInvoice(t, a, acct, "inv-q1", "100"); code != http.StatusOK {
		t.Fatalf("amend draft to the full quota: %d", code)
	}
	// The quota is now fully claimed.
	if code := draftInvoice(t, a, acct, "inv-q2", "0.01"); code != http.StatusConflict {
		t.Fatalf("second draft beyond quota: %d, want 409", code)
	}
	// Voiding returns the quota (the reversal cancels the original).
	if rr := doReq(t, a.handleInvoiceIssue, http.MethodPost, "/internal/invoices/issue", acct, map[string]any{"invoiceId": "inv-q1"}); rr.Code != http.StatusOK {
		t.Fatalf("issue: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(t, a.handleInvoiceVoid, http.MethodPost, "/internal/invoices/void", acct, map[string]any{"originalId": "inv-q1", "reversalId": "inv-q1-r"}); rr.Code != http.StatusOK {
		t.Fatalf("void: %d %s", rr.Code, rr.Body.String())
	}
	if code := draftInvoice(t, a, acct, "inv-q3", "100"); code != http.StatusOK {
		t.Fatalf("draft after void: %d", code)
	}
}

func TestInvoiceRejectsNonPositiveLines(t *testing.T) {
	a := newInMemoryApp()
	spend(t, a, 4002, "100")
	for _, amt := range []string{"0", "-5"} {
		if code := draftInvoice(t, a, 4002, "", amt); code != http.StatusBadRequest {
			t.Errorf("amount %s: %d, want 400", amt, code)
		}
	}
}

// Regression: invoice ids are global and Draft overwrote any DRAFT with that
// id — another tenant's included.
func TestInvoiceDraftCannotHijackForeignID(t *testing.T) {
	a := newInMemoryApp()
	spend(t, a, 4003, "100")
	spend(t, a, 4004, "100")
	if code := draftInvoice(t, a, 4003, "inv-victim", "50"); code != http.StatusOK {
		t.Fatalf("victim draft: %d", code)
	}
	if code := draftInvoice(t, a, 4004, "inv-victim", "50"); code != http.StatusConflict {
		t.Fatalf("foreign draft over victim's id: %d, want 409", code)
	}
	inv, _, _ := a.invoices.Get("inv-victim")
	if inv.AccountID != 4003 {
		t.Fatalf("victim's invoice now belongs to %d", inv.AccountID)
	}
}

func TestInvoiceEmptyIDIsMinted(t *testing.T) {
	a := newInMemoryApp()
	spend(t, a, 4005, "100")
	for i := 0; i < 3; i++ {
		if code := draftInvoice(t, a, 4005, "", "10"); code != http.StatusOK {
			t.Fatalf("draft %d: %d", i, code)
		}
	}
	invs, _ := a.invoices.List(4005)
	if len(invs) != 3 {
		t.Fatalf("want 3 distinct drafts, got %d", len(invs))
	}
	for _, inv := range invs {
		if inv.InvoiceID == "" || !strings.HasPrefix(inv.InvoiceID, "inv-") {
			t.Fatalf("minted id %q", inv.InvoiceID)
		}
	}
}
