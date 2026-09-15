package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qifalab/euler-platform/pricing"
)

// Buying a resource pack must collect its price. The endpoint is reachable
// from the console (console-bff POST /console/reservepacks) and used to mint
// quota straight from a client-supplied faceValue with no debit at all — and
// since packs are waterfall rank 0, that was free compute for the tenant.
func TestReservePackPurchaseIsPaidFor(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(2001)
	buy := func(orderKey string) *httptest.ResponseRecorder {
		t.Helper()
		return doReq(t, a.handleReservePacks, http.MethodPost, "/internal/reservepacks", acct, map[string]any{
			"packId": "pk-1", "productCode": "euecs", "skuCode": "euecs.pack",
			"faceValue": "50", "orderKey": orderKey,
		})
	}

	// Unfunded: refused, and no quota is credited.
	if rr := buy("ord-1"); rr.Code != http.StatusConflict {
		t.Fatalf("unfunded purchase: %d %s, want 409", rr.Code, rr.Body.String())
	}
	if _, ok, err := a.packStore.GetPack("pk-1"); err != nil || ok {
		t.Fatalf("unfunded purchase minted a pack: ok=%v err=%v", ok, err)
	}

	// Funded: the face value leaves the cash balance and the quota appears.
	if _, _, err := a.ledger.Recharge(acct, pricing.MustParseAmount("100"), "seed", "seed-1", "seed"); err != nil {
		t.Fatal(err)
	}
	if rr := buy("ord-1"); rr.Code != http.StatusOK {
		t.Fatalf("funded purchase: %d %s", rr.Code, rr.Body.String())
	}
	bal, err := a.ledger.Balance(acct)
	if err != nil {
		t.Fatal(err)
	}
	if want := pricing.MustParseAmount("50"); bal.Available != want {
		t.Fatalf("balance = %s, want %s (the pack price was not collected)", bal.Available, want)
	}
	pack, ok, err := a.packStore.GetPack("pk-1")
	if err != nil || !ok || pack.Remaining.String() != "50" {
		t.Fatalf("pack = %+v / %v / %v", pack, ok, err)
	}

	// A replay of the same order key is idempotent: no second debit.
	if rr := buy("ord-1"); rr.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", rr.Code, rr.Body.String())
	}
	if bal, _ = a.ledger.Balance(acct); bal.Available != pricing.MustParseAmount("50") {
		t.Fatalf("replay debited again: balance %s", bal.Available)
	}

	// A different order key against the same pack id is refused (re-purchasing
	// would silently reset a partially consumed pack to full).
	if rr := buy("ord-2"); rr.Code != http.StatusConflict {
		t.Fatalf("duplicate pack id: %d %s, want 409", rr.Code, rr.Body.String())
	}
}

// 红冲 needs its own document id. An empty one used to be written as
// invoices[""] and silently replaced by the next void, so the handler must
// reject it before the store is touched.
func TestInvoiceVoidRequiresReversalID(t *testing.T) {
	a := newInMemoryApp()
	const acct = int64(3001)
	draft := doReq(t, a.handleInvoices, http.MethodPost, "/internal/invoices", acct, map[string]any{
		"invoiceId": "inv-x", "billPeriod": "2026-08", "title": "t", "taxNo": "n",
		"items": []map[string]string{{"description": "compute", "amount": "10"}},
	})
	if draft.Code != http.StatusOK {
		t.Fatalf("draft: %d %s", draft.Code, draft.Body.String())
	}
	if rr := doReq(t, a.handleInvoiceIssue, http.MethodPost, "/internal/invoices/issue", acct,
		map[string]any{"invoiceId": "inv-x"}); rr.Code != http.StatusOK {
		t.Fatalf("issue: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(t, a.handleInvoiceVoid, http.MethodPost, "/internal/invoices/void", acct,
		map[string]any{"originalId": "inv-x", "reversalId": ""}); rr.Code != http.StatusBadRequest {
		t.Fatalf("void without reversalId: %d %s, want 400", rr.Code, rr.Body.String())
	}
	if rr := doReq(t, a.handleInvoiceVoid, http.MethodPost, "/internal/invoices/void", acct,
		map[string]any{"originalId": "inv-x", "reversalId": "inv-x-r"}); rr.Code != http.StatusOK {
		t.Fatalf("void: %d %s", rr.Code, rr.Body.String())
	}
}
