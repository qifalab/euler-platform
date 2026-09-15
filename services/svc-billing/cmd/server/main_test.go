package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/billing"
	"github.com/qifalab/euler-platform/pricing"
)

func doReq(t *testing.T, h http.HandlerFunc, method, target string, acct int64, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	if acct != 0 {
		req.Header.Set("X-Euler-Account-Id", fmt.Sprintf("%d", acct))
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func settleBody(aggID string) map[string]any {
	hour := time.Now().UTC().Truncate(time.Hour)
	return map[string]any{
		"AggId":         aggID,
		"ResourceId":    "res-1",
		"MeteringItem":  "cpu_core_hour",
		"TotalQuantity": "2",
		"HourStart":     hour.Format(time.RFC3339),
		"CoveredRatio":  10000,
		"ProductCode":   "euecs",
		"UnitPrice":     "0.25",
		"SnapshotId":    "snap-1",
	}
}

// TestSettleIdempotent verifies a retried settlement replays the original
// result (200) and deducts the ledger and pools exactly once.
func TestSettleIdempotent(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(1001)
	if _, _, err := a.ledger.Recharge(acct, pricing.MustParseAmount("100"), "seed", "seed-1", "seed"); err != nil {
		t.Fatal(err)
	}
	a.pools.set(acct, []billing.Pool{{Source: billing.SourceBalance, Available: pricing.MustParseAmount("100")}})

	for i := 0; i < 3; i++ {
		rr := doReq(t, a.handleSettle, http.MethodPost, "/internal/settle", acct, settleBody("agg-idem"))
		if rr.Code != http.StatusOK {
			t.Fatalf("attempt %d: status %d body %s", i, rr.Code, rr.Body.String())
		}
	}
	bal, err := a.ledger.Balance(acct)
	if err != nil {
		t.Fatal(err)
	}
	// 2 core-hours * 0.25 = 0.50 debited exactly once.
	if want := pricing.MustParseAmount("99.5"); bal.Available != want {
		t.Fatalf("available = %s, want %s (single deduction)", bal.Available, want)
	}
}

// TestSettleConcurrentDuplicates hammers the same aggregate from many
// goroutines; run with -race. The ledger must be debited exactly once.
func TestSettleConcurrentDuplicates(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(1002)
	if _, _, err := a.ledger.Recharge(acct, pricing.MustParseAmount("100"), "seed", "seed-1", "seed"); err != nil {
		t.Fatal(err)
	}
	a.pools.set(acct, []billing.Pool{{Source: billing.SourceBalance, Available: pricing.MustParseAmount("100")}})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := doReq(t, a.handleSettle, http.MethodPost, "/internal/settle", acct, settleBody("agg-conc"))
			if rr.Code != http.StatusOK {
				t.Errorf("status %d body %s", rr.Code, rr.Body.String())
			}
		}()
	}
	wg.Wait()
	bal, err := a.ledger.Balance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if want := pricing.MustParseAmount("99.5"); bal.Available != want {
		t.Fatalf("available = %s, want %s (single deduction)", bal.Available, want)
	}
}

// TestPoolForAccountReturnsCopy verifies mutating the returned slice does not
// leak into the registry, and concurrent forAccount/commit is race-free.
func TestPoolForAccountReturnsCopy(t *testing.T) {
	p := newPoolRegistry()
	p.set(7, []billing.Pool{{Source: billing.SourceBalance, Available: pricing.MustParseAmount("10")}})
	got := p.forAccount(7, nil)
	got[0].Available = pricing.MustParseAmount("0")
	again := p.forAccount(7, nil)
	if again[0].Available != pricing.MustParseAmount("10") {
		t.Fatalf("registry mutated through returned slice: %s", again[0].Available)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = p.forAccount(7, nil)
				p.commit(7, nil, nil)
			}
		}()
	}
	wg.Wait()
}

// TestInvoiceIssueVoidIDOR verifies Issue/Void reject invoices owned by a
// different account with 404.
func TestInvoiceIssueVoidIDOR(t *testing.T) {
	a := newInMemoryApp()
	draft := map[string]any{
		"invoiceId":  "inv-a",
		"billPeriod": "2026-08",
		"title":      "Acme",
		"taxNo":      "T1",
		"items":      []map[string]any{{"description": "compute", "amount": "1.00"}},
	}
	if rr := doReq(t, a.handleInvoices, http.MethodPost, "/internal/invoices", 1, draft); rr.Code != http.StatusOK {
		t.Fatalf("draft: %d %s", rr.Code, rr.Body.String())
	}

	// Foreign account may not issue.
	if rr := doReq(t, a.handleInvoiceIssue, http.MethodPost, "/internal/invoices/issue", 2, map[string]any{"invoiceId": "inv-a"}); rr.Code != http.StatusNotFound {
		t.Fatalf("foreign issue: got %d, want 404", rr.Code)
	}
	// Owner may.
	if rr := doReq(t, a.handleInvoiceIssue, http.MethodPost, "/internal/invoices/issue", 1, map[string]any{"invoiceId": "inv-a"}); rr.Code != http.StatusOK {
		t.Fatalf("owner issue: %d %s", rr.Code, rr.Body.String())
	}
	// Foreign account may not void.
	if rr := doReq(t, a.handleInvoiceVoid, http.MethodPost, "/internal/invoices/void", 2, map[string]any{"originalId": "inv-a", "reversalId": "inv-a-r"}); rr.Code != http.StatusNotFound {
		t.Fatalf("foreign void: got %d, want 404", rr.Code)
	}
	if rr := doReq(t, a.handleInvoiceVoid, http.MethodPost, "/internal/invoices/void", 1, map[string]any{"originalId": "inv-a", "reversalId": "inv-a-r"}); rr.Code != http.StatusOK {
		t.Fatalf("owner void: %d %s", rr.Code, rr.Body.String())
	}
	// Unknown invoice is also 404 (no enumeration).
	if rr := doReq(t, a.handleInvoiceIssue, http.MethodPost, "/internal/invoices/issue", 1, map[string]any{"invoiceId": "nope"}); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown issue: got %d, want 404", rr.Code)
	}
}

// TestSeqClosuresConcurrent drafts invoices from many goroutines; run with
// -race to verify the seq closure locking.
func TestSeqClosuresConcurrent(t *testing.T) {
	a := newInMemoryApp()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := map[string]any{
				"billPeriod": "2026-08",
				"title":      "Acme",
				"items":      []map[string]any{{"description": "d", "amount": "1.00"}},
			}
			rr := doReq(t, a.handleInvoices, http.MethodPost, "/internal/invoices", int64(100+i), body)
			if rr.Code != http.StatusOK {
				t.Errorf("draft %d: %d %s", i, rr.Code, rr.Body.String())
			}
		}(i)
	}
	wg.Wait()
}

// TestBodyLimit verifies oversized request bodies are rejected, not decoded.
func TestBodyLimit(t *testing.T) {
	a := newInMemoryApp()
	big := bytes.Repeat([]byte("a"), maxBodyBytes+16)
	req := httptest.NewRequest(http.MethodPost, "/internal/settle", bytes.NewReader(big))
	req.Header.Set("X-Euler-Account-Id", "1")
	rr := httptest.NewRecorder()
	a.handleSettle(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: got %d, want 400", rr.Code)
	}
}
