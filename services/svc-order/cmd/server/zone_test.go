package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/qifalab/euler-platform/pricing"
)

func zonalStore(t *testing.T) (*orderStore, *fakeQuoter) {
	t.Helper()
	s, q, _ := newTestStore()
	q.prices["euecs.zonal"] = quoteResult{Payable: pricing.MustParseAmount("10"), ChargeType: pricing.ChargePrepaid,
		DurationUnit: pricing.DurationMonth, RegionScope: "ZONAL"}
	q.prices["euoss.regional"] = quoteResult{Payable: pricing.MustParseAmount("10"), ChargeType: pricing.ChargePrepaid,
		DurationUnit: pricing.DurationMonth, RegionScope: "REGIONAL"}
	return s, q
}

func errCodeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Code string `json:"Code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return env.Code
}

// The order records the zone it was priced for and returns it as zoneId; the
// detail read returns the same.
func TestCreateRecordsZone(t *testing.T) {
	s, q := zonalStore(t)
	w := doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.zonal","regionId":"cn-north-1","zoneId":"cn-north-1-b","clientToken":"z-1"}`)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	d := dataOf(t, w)
	if d["zoneId"] != "cn-north-1-b" || d["regionId"] != "cn-north-1" {
		t.Fatalf("order = %v, want zoneId cn-north-1-b in cn-north-1", d)
	}
	if last := q.calls[len(q.calls)-1]; last.ZoneID != "cn-north-1-b" || last.RegionID != "cn-north-1" {
		t.Fatalf("quote input = %+v, want the zone forwarded", last)
	}
	id := strconv.FormatInt(int64(d["orderId"].(float64)), 10)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/orders/"+id, nil)
	r.SetPathValue("id", id)
	r.Header.Set(accountIDHeader, "100123")
	rr := httptest.NewRecorder()
	s.handleDetail(rr, r)
	if got := dataOf(t, rr); got["zoneId"] != "cn-north-1-b" {
		t.Fatalf("detail = %v, want zoneId", got)
	}

	// A zone-only order takes its region from the zone, as the catalogue
	// prices it.
	d = dataOf(t, doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.zonal","zoneId":"cn-east-1-a","clientToken":"z-2"}`))
	if d["regionId"] != "cn-east-1" || d["zoneId"] != "cn-east-1-a" {
		t.Fatalf("zone-only order = %v, want region cn-east-1", d)
	}
	// No zone, scope not reported (today's catalogue): nothing recorded.
	d = dataOf(t, doCreate(t, s, `{"productCode":"euecs","skuCode":"euecs.c1","regionId":"cn-north-1","clientToken":"z-3"}`))
	if d["zoneId"] != "" {
		t.Fatalf("zoneless order = %v, want zoneId \"\"", d)
	}
}

func TestCreateValidatesZone(t *testing.T) {
	s, q := zonalStore(t)
	for name, tc := range map[string]struct{ body, code string }{
		"shape":       {`{"productCode":"euecs","skuCode":"euecs.zonal","regionId":"cn-north-1","zoneId":"cn-north-1b"}`, "Order.InvalidZone"},
		"letter":      {`{"productCode":"euecs","skuCode":"euecs.zonal","regionId":"cn-north-1","zoneId":"cn-north-1-1"}`, "Order.InvalidZone"},
		"region":      {`{"productCode":"euecs","skuCode":"euecs.zonal","regionId":"cn-north-1","zoneId":"cn-east-1-a"}`, "Order.ZoneRegionMismatch"},
		"too long":    {`{"productCode":"euecs","skuCode":"euecs.zonal","zoneId":"cn-north-1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "Common.InvalidParameter"},
		"zonal, none": {`{"productCode":"euecs","skuCode":"euecs.zonal","regionId":"cn-north-1"}`, "Order.ZoneRequired"},
	} {
		before := len(q.calls)
		w := doCreate(t, s, tc.body)
		if w.Code != 400 || errCodeOf(t, w) != tc.code {
			t.Fatalf("%s: %d %s, want 400 %s", name, w.Code, w.Body.String(), tc.code)
		}
		if name != "zonal, none" && len(q.calls) != before {
			t.Fatalf("%s: an invalid zone reached the pricing engine", name)
		}
	}
}

// A REGIONAL product spreads across zones: a zone sent with it is not recorded.
func TestCreateDropsZoneForRegionalProduct(t *testing.T) {
	s, _ := zonalStore(t)
	d := dataOf(t, doCreate(t, s, `{"productCode":"euoss","skuCode":"euoss.regional","regionId":"cn-north-1","zoneId":"cn-north-1-a","clientToken":"r-1"}`))
	if d["zoneId"] != "" || d["regionId"] != "cn-north-1" {
		t.Fatalf("regional order = %v, want no zone", d)
	}
}

// The quoter reads the catalogue's placement scope when the quote carries it.
func TestCatalogQuoterReadsRegionScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": map[string]any{
			"payableAmount": "1", "chargeType": "PREPAID", "durationUnit": "MONTH", "regionScope": "ZONAL"}})
	}))
	defer srv.Close()
	q := catalogQuoter{newInternalClient(srv.URL)}
	res, err := q.Quote(t.Context(), 100123, quoteInput{ProductCode: "euecs", SKUCode: "euecs.c1", ZoneID: "cn-north-1-a"})
	if err != nil || res.RegionScope != "ZONAL" {
		t.Fatalf("quote = %+v, %v; want regionScope ZONAL", res, err)
	}
}

// order_item.zone_id round-trips through both repos: the zone of a ZONAL
// order, and "" (NULL) for one without.
func TestRepoPersistsZone(t *testing.T) {
	for name, mk := range map[string]func() orderRepo{
		"memory": func() orderRepo { return newMemOrderRepo() },
		"mysql":  func() orderRepo { return newSQLTestRepo(t) },
	} {
		t.Run(name, func(t *testing.T) {
			repo := mk()
			zonal, evt := testOrder(repo.NewOrderID(), "tok-zone")
			zonal.ZoneID = "cn-north-1-b"
			if err := repo.Create(zonal, evt, nil); err != nil {
				t.Fatal(err)
			}
			plain, evt2 := testOrder(repo.NewOrderID(), "tok-nozone")
			plain.OrderNo = "SO20260915000002"
			evt2.OrderNo = plain.OrderNo
			if err := repo.Create(plain, evt2, nil); err != nil {
				t.Fatal(err)
			}
			got, err := repo.Get(100123, zonal.OrderID)
			if err != nil || got == nil || got.ZoneID != "cn-north-1-b" {
				t.Fatalf("zonal order read back = %+v, %v", got, err)
			}
			got, err = repo.Get(100123, plain.OrderID)
			if err != nil || got == nil || got.ZoneID != "" {
				t.Fatalf("zoneless order read back = %+v, %v", got, err)
			}
			list, err := repo.List(100123)
			if err != nil || len(list) != 2 {
				t.Fatalf("list = %d orders, %v", len(list), err)
			}
		})
	}
}
