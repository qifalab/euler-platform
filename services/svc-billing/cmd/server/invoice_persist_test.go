package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/invoice"
	"github.com/qifalab/euler-platform/pricing"
)

// invoiceStoreScenario walks the document lifecycle and every refusal on one
// store and returns the account's final invoices.
func invoiceStoreScenario(t *testing.T, s invoiceStore) []invoice.Invoice {
	t.Helper()
	const acct, other = int64(5001), int64(5002)
	spent := pricing.MustParseAmount("100")
	line := func(amt string) []invoice.LineItem {
		return []invoice.LineItem{{Description: "compute", Amount: pricing.MustParseAmount(amt)}}
	}
	if _, err := s.Draft(acct, "p-1", "2026-09", "Acme", "T1", line("150"), spent); !errorAs[*exceedsSpendError](err) {
		t.Fatalf("draft above spend: %v", err)
	}
	if _, err := s.Draft(acct, "p-1", "2026-09", "Acme", "T1", line("60"), spent); err != nil {
		t.Fatal(err)
	}
	// Amending does not count the draft's own amount twice.
	d, err := s.Draft(acct, "p-1", "2026-09", "Acme Ltd", "T1",
		[]invoice.LineItem{{Description: "compute", Amount: pricing.MustParseAmount("70")}, {Description: "disk", Amount: pricing.MustParseAmount("10.5")}}, spent)
	if err != nil || d.Amount != pricing.MustParseAmount("80.5") || len(d.Items) != 2 {
		t.Fatalf("amend = %+v, %v", d, err)
	}
	if _, err := s.Draft(acct, "p-2", "2026-09", "Acme", "T1", line("19.51"), spent); !errorAs[*exceedsSpendError](err) {
		t.Fatalf("second draft beyond the rest of the quota: %v", err)
	}
	minted, err := s.Draft(acct, "", "2026-09", "Acme", "T1", line("19.5"), spent)
	if err != nil || minted.InvoiceID == "" {
		t.Fatalf("minted draft = %+v, %v", minted, err)
	}
	if _, err := s.Draft(other, "p-1", "2026-09", "Evil", "", line("1"), spent); !errors.Is(err, invoice.ErrInvoiceExists) {
		t.Fatalf("foreign id hijack: %v", err)
	}
	if _, err := s.Draft(acct, "p-x", "", "", "", line("1"), spent); !errors.Is(err, invoice.ErrEmptyTitle) {
		t.Fatalf("empty title: %v", err)
	}

	if _, err := s.Issue(other, "p-1", spent); !errors.Is(err, invoice.ErrInvoiceNotFound) {
		t.Fatalf("foreign issue: %v", err)
	}
	// A refund shrank the spend below the draft: the issue is refused.
	if _, err := s.Issue(acct, "p-1", pricing.MustParseAmount("50")); !errorAs[*exceedsSpendError](err) {
		t.Fatalf("issue above shrunk spend: %v", err)
	}
	iss, err := s.Issue(acct, "p-1", spent)
	if err != nil || iss.Status != invoice.StatusIssued || iss.IssuedAt.IsZero() {
		t.Fatalf("issue = %+v, %v", iss, err)
	}
	if _, err := s.Issue(acct, "p-1", spent); !errors.Is(err, invoice.ErrInvoiceNotDraft) {
		t.Fatalf("second issue: %v", err)
	}
	if _, err := s.Draft(acct, "p-1", "2026-09", "Acme", "T1", line("1"), pricing.MustParseAmount("1000")); !errors.Is(err, invoice.ErrInvoiceExists) {
		t.Fatalf("re-drafting an issued invoice: %v", err)
	}

	if _, err := s.Void(other, "p-1", "p-1-r"); !errors.Is(err, invoice.ErrInvoiceNotFound) {
		t.Fatalf("foreign void: %v", err)
	}
	if _, err := s.Void(acct, minted.InvoiceID, "p-m-r"); !errors.Is(err, invoice.ErrInvoiceTerminal) {
		t.Fatalf("voiding a draft: %v", err)
	}
	if _, err := s.Void(acct, "p-1", minted.InvoiceID); !errors.Is(err, invoice.ErrReversalIDTaken) {
		t.Fatalf("reversal onto a taken id: %v", err)
	}
	rev, err := s.Void(acct, "p-1", "p-1-r")
	if err != nil || rev.Amount != pricing.MustParseAmount("-80.5") || rev.ReversesID != "p-1" ||
		len(rev.Items) != 2 || rev.Items[1].Description != "红冲: disk" {
		t.Fatalf("reversal = %+v, %v", rev, err)
	}
	if again, err := s.Void(acct, "p-1", "p-1-r"); err != nil || again.InvoiceID != "p-1-r" {
		t.Fatalf("replayed void = %+v, %v", again, err)
	}
	if _, err := s.Void(acct, "p-1", "p-1-r2"); !errors.Is(err, invoice.ErrInvoiceAlreadyVoid) {
		t.Fatalf("second reversal: %v", err)
	}
	// The void returned the quota.
	if _, err := s.Draft(acct, "p-3", "2026-10", "Acme", "T1", line("80.5"), spent); err != nil {
		t.Fatalf("draft after void: %v", err)
	}
	orig, ok, err := s.Get("p-1")
	if err != nil || !ok || orig.Status != invoice.StatusVoided || orig.VoidedByID != "p-1-r" || orig.VoidedAt.IsZero() {
		t.Fatalf("original after void = %+v (%v %v)", orig, ok, err)
	}
	out, err := s.List(acct)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func errorAs[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

func TestInvoiceStoreParity(t *testing.T) {
	mem := invoiceStoreScenario(t, newMemInvoiceStore(time.Now))
	d := newSQLTestDB(t)
	sqlOut := invoiceStoreScenario(t, d.app().invoices)
	if len(mem) != len(sqlOut) {
		t.Fatalf("memory %d invoices, sql %d", len(mem), len(sqlOut))
	}
	for i := range mem {
		m, s := mem[i], sqlOut[i]
		if m.InvoiceID != s.InvoiceID && !(m.ReversesID == "" && s.ReversesID == "" && m.Status == invoice.StatusDraft && m.BillPeriod == "2026-09") {
			t.Fatalf("invoice %d: memory %s, sql %s", i, m.InvoiceID, s.InvoiceID)
		}
		if m.Amount != s.Amount || m.Status != s.Status || m.Title != s.Title || m.BillPeriod != s.BillPeriod ||
			m.VoidedByID != s.VoidedByID || m.ReversesID != s.ReversesID || len(m.Items) != len(s.Items) ||
			m.IssuedAt.IsZero() != s.IssuedAt.IsZero() || m.VoidedAt.IsZero() != s.VoidedAt.IsZero() {
			t.Fatalf("invoice %d: memory %+v, sql %+v", i, m, s)
		}
		for j := range m.Items {
			if m.Items[j] != s.Items[j] {
				t.Fatalf("invoice %s line %d: memory %+v, sql %+v", m.InvoiceID, j, m.Items[j], s.Items[j])
			}
		}
	}
}

// Invoices survive a restart: what one app drafted and issued, the next one
// (same database) lists, refuses to over-draft, and voids.
func TestInvoicesSurviveRestart(t *testing.T) {
	d := newSQLTestDB(t)
	const acct = int64(5101)
	first := d.app()
	spend(t, first, acct, "100")
	if code := draftInvoice(t, first, acct, "r-1", "100"); code != http.StatusOK {
		t.Fatalf("draft: %d", code)
	}
	if rr := doReq(t, first.handleInvoiceIssue, http.MethodPost, "/internal/invoices/issue", acct, map[string]any{"invoiceId": "r-1"}); rr.Code != http.StatusOK {
		t.Fatalf("issue: %d %s", rr.Code, rr.Body.String())
	}

	second := d.app()
	invs, err := second.invoices.List(acct)
	if err != nil || len(invs) != 1 || invs[0].Status != invoice.StatusIssued || invs[0].Amount != pricing.MustParseAmount("100") {
		t.Fatalf("after restart = %+v (%v)", invs, err)
	}
	if code := draftInvoice(t, second, acct, "r-2", "0.01"); code != http.StatusConflict {
		t.Fatalf("quota forgotten across restart: %d", code)
	}
	if rr := doReq(t, second.handleInvoiceVoid, http.MethodPost, "/internal/invoices/void", acct, map[string]any{"originalId": "r-1", "reversalId": "r-1-r"}); rr.Code != http.StatusOK {
		t.Fatalf("void: %d %s", rr.Code, rr.Body.String())
	}
	rr := doReq(t, first.handleInvoices, http.MethodGet, "/internal/invoices", acct, nil)
	if rr.Code != http.StatusOK || !containsAll(rr.Body.String(), `"r-1-r"`, `"VOIDED"`, `"-100"`) {
		t.Fatalf("list on the other replica: %d %s", rr.Code, rr.Body.String())
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// Concurrent drafts on two replicas cannot overspend one quota, and one draft
// cannot be issued twice.
func TestInvoiceSpendCapHoldsAcrossReplicas(t *testing.T) {
	d := newSQLTestDB(t)
	const acct = int64(5201)
	apps := []*app{d.app(), d.app()}
	spend(t, apps[0], acct, "100")

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = draftInvoice(t, apps[i%2], acct, fmt.Sprintf("c-%d", i), "30")
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected draft status %d (%v)", c, codes)
		}
	}
	if ok != 3 {
		t.Fatalf("%d drafts of 30 accepted against a spend of 100 (%v)", ok, codes)
	}

	var draftID string
	invs, _ := apps[0].invoices.List(acct)
	for _, inv := range invs {
		if inv.Status == invoice.StatusDraft {
			draftID = inv.InvoiceID
			break
		}
	}
	issued := make([]int, 2)
	for i := range issued {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := doReq(t, apps[i].handleInvoiceIssue, http.MethodPost, "/internal/invoices/issue", acct, map[string]any{"invoiceId": draftID})
			issued[i] = rr.Code
		}(i)
	}
	wg.Wait()
	if !(issued[0] == http.StatusOK && issued[1] == http.StatusBadRequest) && !(issued[1] == http.StatusOK && issued[0] == http.StatusBadRequest) {
		t.Fatalf("concurrent issue of one draft: %v", issued)
	}
}

func TestInvoiceRejectsSubCentAndOversizedFields(t *testing.T) {
	a := newInMemoryApp()
	spend(t, a, 5301, "100")
	if code := draftInvoice(t, a, 5301, "", "1.005"); code != http.StatusBadRequest {
		t.Fatalf("sub-分 amount: %d", code)
	}
	for name, body := range map[string]map[string]any{
		"period": {"billPeriod": "2026/09", "title": "Acme", "items": []map[string]any{{"description": "x", "amount": "1"}}},
		"id":     {"invoiceId": string(make([]byte, 65)), "title": "Acme", "items": []map[string]any{{"description": "x", "amount": "1"}}},
	} {
		if rr := doReq(t, a.handleInvoices, http.MethodPost, "/internal/invoices", 5301, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
}
