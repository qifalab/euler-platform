package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Regression tests for the quote path's trust boundary: the SKU's catalogue
// row (charge type, sale status, product row) is the authority, never the
// caller's claim, and a zone-only ZONAL quote prices in the zone's region.

// doQuoteOn runs one quote against a specific store (doQuote always builds a
// fresh seed store).
func doQuoteOn(t *testing.T, store *catalogStore, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/v1/catalog/quote", bytes.NewReader(raw))
	req.Header.Set("X-Euler-Account-Id", "100123")
	req.Header.Set("X-Euler-TraceId", "t")
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/catalog/quote", store.handleQuote)
	recoverMiddleware(requestIDMiddleware(mux)).ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

// K1: a PREPAID SKU quoted as POSTPAID used to be priced as POSTPAID (no
// duration factor), so a month of compute quoted far below its price.
func TestQuoteRejectsChargeTypeMismatch(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "euecs",
		"specCode":    "euecs.s2.large.prepaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
		"zoneId":      "cn-north-1-a",
		"duration":    12,
	})
	if code != 400 || out["Code"] != "Catalog.ChargeTypeMismatch" {
		t.Fatalf("prepaid SKU quoted as POSTPAID: code %d body %v, want 400 Catalog.ChargeTypeMismatch", code, out)
	}
	for _, ct := range []string{"RESOURCE_PACK", "SPOT"} {
		code, out = doQuote(t, map[string]any{
			"productCode": "euecs",
			"specCode":    "euecs.s2.large.prepaid",
			"chargeType":  ct,
			"regionId":    "cn-north-1",
			"zoneId":      "cn-north-1-a",
			"duration":    12,
		})
		if code != 400 || out["Code"] != "Catalog.ChargeTypeMismatch" {
			t.Fatalf("prepaid SKU quoted as %s: code %d body %v, want 400 Catalog.ChargeTypeMismatch", ct, code, out)
		}
	}

	// Omitted chargeType = the SKU's own; the duration factor applies.
	code, out = doQuote(t, map[string]any{
		"productCode": "euecs",
		"specCode":    "euecs.s2.large.prepaid",
		"regionId":    "cn-north-1",
		"zoneId":      "cn-north-1-a",
		"duration":    2,
	})
	if code != 200 {
		t.Fatalf("quote without chargeType: code %d body %v", code, out)
	}
	data, _ := out["Data"].(map[string]any)
	if data["chargeType"] != "PREPAID" || data["durationUnit"] != "MONTH" || data["listAmount"] != "360" {
		t.Fatalf("quote without chargeType = %v, want PREPAID/MONTH list 360 (180 × 2)", data)
	}
}

// K2: draft / delisted SKUs and not-on-sale products must not be quotable,
// even though the list endpoints still return them.
func TestQuoteRejectsNotOnSale(t *testing.T) {
	body := map[string]any{
		"productCode": "euoss",
		"specCode":    "euoss.standard.postpaid",
		"regionId":    "cn-north-1",
	}
	if code, out := doQuoteOn(t, newCatalogStore(), body); code != 200 {
		t.Fatalf("on-sale seed quote: code %d body %v", code, out)
	}
	for _, st := range []string{"0", "2"} {
		store := newCatalogStore()
		for i := range store.skus {
			if store.skus[i].SKUCode == "euoss.standard.postpaid" {
				store.skus[i].Status = st
			}
		}
		code, out := doQuoteOn(t, store, body)
		if code != 409 || out["Code"] != "Catalog.NotOnSale" {
			t.Fatalf("sku status %s: code %d body %v, want 409 Catalog.NotOnSale", st, code, out)
		}
	}
	for _, st := range []int{0, 1, 3} {
		store := newCatalogStore()
		for i := range store.products {
			if store.products[i].ProductCode == "euoss" {
				store.products[i].Status = st
			}
		}
		code, out := doQuoteOn(t, store, body)
		if code != 409 || out["Code"] != "Catalog.NotOnSale" {
			t.Fatalf("product status %d: code %d body %v, want 409 Catalog.NotOnSale", st, code, out)
		}
	}
}

// The code seed must stay on-sale (it mirrors sql/V2, which seeds product
// status 2 and SKU status 1) or every seeded quote would 409.
func TestSeedCatalogueIsOnSale(t *testing.T) {
	for _, p := range seedProducts() {
		if p.Status != productStatusOnSale {
			t.Errorf("seed product %s status %d, want %d", p.ProductCode, p.Status, productStatusOnSale)
		}
	}
	for _, sk := range seedSKUs() {
		if sk.Status != skuStatusOnSale {
			t.Errorf("seed sku %s status %q, want %q", sk.SKUCode, sk.Status, skuStatusOnSale)
		}
	}
}

// K3: a ZONAL quote carrying only zoneId must select the zone's region rule
// (rule 5, cn-east-1 = 200/月), not the wildcard (rule 2, 180/月).
func TestQuoteZoneOnlyUsesRegionRule(t *testing.T) {
	cases := []struct {
		name   string
		body   map[string]any
		ruleID float64
	}{
		{"zone only", map[string]any{"zoneId": "cn-east-1-a"}, 5},
		{"zone and region", map[string]any{"zoneId": "cn-east-1-a", "regionId": "cn-east-1"}, 5},
		{"zone only, no region rule", map[string]any{"zoneId": "cn-north-1-a"}, 2},
	}
	for _, tc := range cases {
		body := map[string]any{
			"productCode": "euecs",
			"specCode":    "euecs.s2.large.prepaid",
			"duration":    1,
		}
		for k, v := range tc.body {
			body[k] = v
		}
		code, out := doQuote(t, body)
		if code != 200 {
			t.Fatalf("%s: code %d body %v", tc.name, code, out)
		}
		data, _ := out["Data"].(map[string]any)
		if data["ruleId"] != tc.ruleID {
			t.Errorf("%s: ruleId = %v, want %v (data %v)", tc.name, data["ruleId"], tc.ruleID, data)
		}
	}
}

// K4: a SKU whose t_product row is missing used to dereference a nil product
// (panic → 500); it is a 404.
func TestQuoteMissingProductRow(t *testing.T) {
	store := newCatalogStoreWith(catalogData{
		SKUs:  seedSKUs(),
		Rules: seedRules(),
	})
	code, out := doQuoteOn(t, store, map[string]any{
		"productCode": "euoss",
		"specCode":    "euoss.standard.postpaid",
		"regionId":    "cn-north-1",
	})
	if code != 404 || out["Code"] != "Catalog.ProductNotFound" {
		t.Fatalf("sku without product row: code %d body %v, want 404 Catalog.ProductNotFound", code, out)
	}
}
