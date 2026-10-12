package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// fakeOrchestratorList serves /api/v1/orchestrator/resources with rows and
// records the query string it was asked with.
func fakeOrchestratorList(t *testing.T, rows []map[string]any, gotQuery *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/orchestrator/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(accountIDHeader) != "100123" {
			t.Errorf("account header = %q, want the caller's", r.Header.Get(accountIDHeader))
		}
		if gotQuery != nil {
			*gotQuery = r.URL.RawQuery
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": rows})
	})
	mux.HandleFunc("/api/v1/catalog/products", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": "OK", "Data": []map[string]any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var orchRows = []map[string]any{
	{
		"resourceId": "euecs-cn-north-1-01-a1", "productCode": "euecs", "region": "cn-north-1", "zone": "cn-north-1a",
		"chargeType": "POSTPAID", "state": "RUNNING", "specCode": "euecs.s2.large", "billingStart": "2026-10-01T00:00:00Z",
		"createdAt": "2026-10-01T00:00:00Z", "expiredAt": "", "version": 3,
		"attributes": map[string]any{"name": "web-01", "description": "frontend"},
	},
	// An orchestrator row without attributes/zone (older orchestrator, or a
	// resource that has none) still yields an attributes object.
	{"resourceId": "eubs-cn-north-1-01-b2", "productCode": "eubs", "region": "cn-north-1", "state": "RUNNING"},
}

func getAs100123(t *testing.T, h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
	}
	return rec
}

func TestResourcesPassesZoneAndAttributesThrough(t *testing.T) {
	var query string
	srv := fakeOrchestratorList(t, orchRows, &query)
	s := newConsoleStore()
	s.orchestratorURL = srv.URL

	rec := getAs100123(t, s.handleResources, "/console/resources")
	if query != "" {
		t.Fatalf("unfiltered list forwarded query %q", query)
	}
	var env struct {
		Data []map[string]any `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data) != 2 {
		t.Fatalf("Data = %v", env.Data)
	}
	first, second := env.Data[0], env.Data[1]
	// The existing PascalCase fields are unchanged.
	for k, want := range map[string]any{
		"ResourceId": "euecs-cn-north-1-01-a1", "ProductCode": "euecs", "Region": "cn-north-1",
		"ChargeType": "POSTPAID", "State": "RUNNING", "SpecCode": "euecs.s2.large",
		"BillingStart": "2026-10-01T00:00:00Z", "CreatedAt": "2026-10-01T00:00:00Z", "ExpiredAt": "",
		"Zone": "cn-north-1a",
	} {
		if first[k] != want {
			t.Errorf("%s = %#v, want %#v", k, first[k], want)
		}
	}
	if want := map[string]any{"name": "web-01", "description": "frontend"}; !reflect.DeepEqual(first["Attributes"], want) {
		t.Errorf("Attributes = %#v, want %#v", first["Attributes"], want)
	}
	if attrs, ok := second["Attributes"].(map[string]any); !ok || len(attrs) != 0 {
		t.Errorf("attribute-less row: Attributes = %#v, want {}", second["Attributes"])
	}
	if second["Zone"] != "" {
		t.Errorf("zone-less row: Zone = %#v, want \"\"", second["Zone"])
	}

	// ?product= is forwarded (escaped) to the orchestrator, which filters.
	getAs100123(t, s.handleResources, "/console/resources?product=%20euecs%20")
	if query != "product=euecs" {
		t.Fatalf("forwarded query = %q, want product=euecs", query)
	}
}

func TestSearchResourceHitsCarryZoneAndAttributes(t *testing.T) {
	srv := fakeOrchestratorList(t, orchRows, nil)
	s := newConsoleStore()
	s.orchestratorURL, s.catalogURL = srv.URL, srv.URL

	// The name attribute is searchable: "WEB-0" matches only web-01.
	rec := getAs100123(t, s.handleSearch, "/console/search?q=WEB-0")
	var env struct {
		Data struct {
			Resources []map[string]any `json:"resources"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Resources) != 1 {
		t.Fatalf("resources = %v, want the one named web-01", env.Data.Resources)
	}
	hit := env.Data.Resources[0]
	if hit["resourceId"] != "euecs-cn-north-1-01-a1" || hit["zone"] != "cn-north-1a" || hit["state"] != "RUNNING" {
		t.Fatalf("hit = %v", hit)
	}
	if attrs, _ := hit["attributes"].(map[string]any); attrs["name"] != "web-01" {
		t.Fatalf("hit attributes = %#v", hit["attributes"])
	}

	// An empty query lists everything, attribute-less rows with {}.
	rec = getAs100123(t, s.handleSearch, "/console/search")
	env.Data.Resources = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Resources) != 2 {
		t.Fatalf("resources = %v", env.Data.Resources)
	}
	if attrs, ok := env.Data.Resources[1]["attributes"].(map[string]any); !ok || len(attrs) != 0 {
		t.Fatalf("attribute-less hit: attributes = %#v, want {}", env.Data.Resources[1]["attributes"])
	}
}
