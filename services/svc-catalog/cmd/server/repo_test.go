package main

import (
	"context"
	"testing"

	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/storage/sqltest"
)

func newSQLTestCatalogRepo(t *testing.T) *sqlCatalogRepo {
	t.Helper()
	db := sqltest.Open(t, sqltest.Services("svc-catalog"))
	return newSQLCatalogRepo(context.Background(), db)
}

// TestSQLCatalogLoaderLoadsSeed loads the catalogue from the schema of record
// and checks the things the quote path silently depends on: non-empty sets,
// DECIMAL prices that parse exactly, and — after V4 — placement columns that
// match the code's model (scecs is ZONAL with cross-AZ HA; the V2 seed wrote
// everything REGIONAL, which would have disarmed the M-6 placement gate).
func TestSQLCatalogLoaderLoadsSeed(t *testing.T) {
	repo := newSQLTestCatalogRepo(t)
	data, err := repo.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCatalog(data); err != nil {
		t.Fatal(err)
	}

	byCode := make(map[string]product, len(data.Products))
	for _, p := range data.Products {
		byCode[p.ProductCode] = p
	}
	scecs, ok := byCode["scecs"]
	if !ok {
		t.Fatal("scecs missing from t_product")
	}
	if scecs.RegionScope != "ZONAL" || !scecs.CrossAZ {
		t.Fatalf("scecs placement = %s cross_az=%v, want ZONAL+true (V4 did not reconcile the seed)", scecs.RegionScope, scecs.CrossAZ)
	}
	if vpc := byCode["scvpc"]; vpc.RegionScope != "REGIONAL" || vpc.CrossAZ {
		t.Fatalf("scvpc placement = %s cross_az=%v, want REGIONAL+false", vpc.RegionScope, vpc.CrossAZ)
	}

	var found bool
	for _, r := range data.Rules {
		if r.SKUCode == "scecs.s2.large.prepaid" && r.RegionID == "*" && r.CustomerLevel == "" {
			found = true
			if r.ListPrice.String() != pricing.MustParseAmount("180").String() {
				t.Fatalf("scecs.s2.large.prepaid list price = %s, want 180", r.ListPrice.String())
			}
			if r.DurationUnit != pricing.DurationMonth {
				t.Fatalf("duration unit = %s, want MONTH", r.DurationUnit)
			}
		}
	}
	if !found {
		t.Fatal("wildcard rule for scecs.s2.large.prepaid missing")
	}
	if len(data.Promos) == 0 {
		t.Fatal("seed promotions did not load")
	}
}

// TestSQLCatalogLoaderRejectsEmpty pins the startup guard: an empty catalogue
// (seed never ran) must fail validation loudly — every quote would otherwise
// answer "规格不存在", which reads like an outage instead of the misconfiguration
// it is.
func TestSQLCatalogLoaderRejectsEmpty(t *testing.T) {
	if err := validateCatalog(catalogData{}); err == nil {
		t.Fatal("empty catalogue passed validation")
	}

	db := sqltest.Open(t, sqltest.Services("svc-catalog"))
	if _, err := db.Exec(`DELETE FROM t_pricing_rule`); err != nil {
		t.Fatal(err)
	}
	repo := newSQLCatalogRepo(context.Background(), db)
	data, err := repo.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCatalog(data); err == nil {
		t.Fatal("a catalogue without pricing rules passed validation")
	}
}

// TestSQLCatalogMatchesMemoryStore compares the DB-loaded catalogue with the
// in-memory seed: identical placement per product and identical rule identity
// (SKU, region, level, unit, price). The seed file and the code seed drifted
// once already — this test is the tripwire.
func TestSQLCatalogMatchesMemoryStore(t *testing.T) {
	repo := newSQLTestCatalogRepo(t)
	dbData, err := repo.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	memData, err := newMemCatalogRepo().Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	memPlacement := make(map[string]product, len(memData.Products))
	for _, p := range memData.Products {
		memPlacement[p.ProductCode] = p
	}
	for code, want := range memPlacement {
		got, ok := func() (product, bool) {
			for _, p := range dbData.Products {
				if p.ProductCode == code {
					return p, true
				}
			}
			return product{}, false
		}()
		if !ok {
			t.Fatalf("product %s in code seed but not in DB", code)
		}
		if got.RegionScope != want.RegionScope || got.CrossAZ != want.CrossAZ {
			t.Fatalf("product %s placement: DB %s/%v, seed %s/%v", code,
				got.RegionScope, got.CrossAZ, want.RegionScope, want.CrossAZ)
		}
	}

	memRules := make(map[string]string, len(memData.Rules))
	for _, r := range memData.Rules {
		memRules[r.SKUCode+"|"+r.RegionID+"|"+r.CustomerLevel] = string(r.DurationUnit) + "|" + r.ListPrice.String()
	}
	for _, r := range dbData.Rules {
		key := r.SKUCode + "|" + r.RegionID + "|" + r.CustomerLevel
		want, ok := memRules[key]
		if !ok {
			continue // DB-only rows are legitimate ops data
		}
		got := string(r.DurationUnit) + "|" + r.ListPrice.String()
		if got != want {
			t.Fatalf("rule %s: DB %q, seed %q", key, got, want)
		}
	}
}
