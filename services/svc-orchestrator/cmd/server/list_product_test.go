package main

import (
	"encoding/json"
	"sort"
	"testing"
)

// listAs runs GET /api/v1/orchestrator/resources{query} as acct and returns
// the listed resource ids, sorted.
func listAs(t *testing.T, s *lifecycleStore, acct int64, query string) []string {
	t.Helper()
	rec := call(t, s.handleListResources, "GET", "/api/v1/orchestrator/resources"+query, acct, nil)
	if rec.Code != 200 {
		t.Fatalf("list%s: %d %s", query, rec.Code, rec.Body.String())
	}
	var items []view
	if err := json.Unmarshal(decodeEnv(t, rec).Data, &items); err != nil {
		t.Fatalf("decode list %q: %v", rec.Body.String(), err)
	}
	if items == nil {
		t.Fatalf("list%s: Data is not an array: %s", query, rec.Body.String())
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ResourceID)
	}
	sort.Strings(ids)
	return ids
}

// Regression: ?product= used to be ignored, so a product page listed every
// product's resources. Fresh accounts keep the memory ledger's seed rows out.
func TestListResourcesFiltersByProduct(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		ecs, disk, other := paidIn(501, "euecs", "cn-north-1"), paidIn(502, "eubs", "cn-north-1"), paidIn(503, "euecs", "cn-north-1")
		orders.add(700001, ecs)
		orders.add(700001, disk)
		orders.add(700002, other)
		fulfil := func(acct int64, o paidOrder) string {
			rec := fulfilAs(t, s, acct, fulfilBodyFor(o, nil))
			if rec.Code != 200 {
				t.Fatalf("fulfil %d: %d %s", o.OrderID, rec.Code, rec.Body.String())
			}
			return decodeFulfil(t, rec).Data.ResourceID
		}
		ecsID, diskID := fulfil(700001, ecs), fulfil(700001, disk)
		fulfil(700002, other)

		same := func(got []string, want ...string) bool {
			sort.Strings(want)
			if len(got) != len(want) {
				return false
			}
			for i := range got {
				if got[i] != want[i] {
					return false
				}
			}
			return true
		}
		for _, tc := range []struct {
			acct  int64
			query string
			want  []string
		}{
			{700001, "", []string{ecsID, diskID}},
			{700001, "?product=", []string{ecsID, diskID}},
			{700001, "?product=euecs", []string{ecsID}},
			{700001, "?product=EUECS", []string{ecsID}},
			{700001, "?product=%20eubs%20", []string{diskID}},
			{700001, "?product=eurds", nil},
			// The filter narrows the caller's rows only: another account's
			// euecs never leaks in, and it does not see 700001's disk.
			{700002, "?product=eubs", nil},
		} {
			if got := listAs(t, s, tc.acct, tc.query); !same(got, tc.want...) {
				t.Fatalf("account %d list%s = %v, want %v", tc.acct, tc.query, got, tc.want)
			}
		}
	})
}
