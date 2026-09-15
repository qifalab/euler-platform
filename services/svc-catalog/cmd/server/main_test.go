package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests cover the M-6 placement contract: the catalogue now distinguishes
// ZONAL products (pinned to one AZ at create time) from REGIONAL ones (spread),
// the quote path enforces that a ZONAL product's quote carries a valid zoneId,
// and the placement endpoint exposes the contract to the console.

func newTestServer() http.Handler {
	store := newCatalogStore()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/catalog/quote", store.handleQuote)
	mux.HandleFunc("GET /api/v1/catalog/placement", store.handlePlacement)
	mux.HandleFunc("GET /api/v1/catalog/region-topology", store.handleRegionTopology)
	return recoverMiddleware(requestIDMiddleware(mux))
}

func doQuote(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/v1/catalog/quote", bytes.NewReader(raw))
	req.Header.Set("X-Euler-Account-Id", "100123")
	req.Header.Set("X-Euler-TraceId", "t")
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func getPlacement(t *testing.T, productCode string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/catalog/placement?productCode="+productCode, nil)
	req.Header.Set("X-Euler-Account-Id", "100123")
	req.Header.Set("X-Euler-TraceId", "t")
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, req)
	var env map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	// success body is {Code,Data,RequestId}; the fields live under Data.
	if data, ok := env["Data"].(map[string]any); ok {
		return rr.Code, data
	}
	return rr.Code, env // error envelope (top-level Code/Message)
}

func TestPlacementEndpointZonal(t *testing.T) {
	// euecs is seeded ZONAL with cross-AZ replicas.
	code, out := getPlacement(t, "euecs")
	if code != 200 {
		t.Fatalf("placement euecs: code %d body %v", code, out)
	}
	if out["regionScope"] != "ZONAL" {
		t.Errorf("euecs scope = %v, want ZONAL", out["regionScope"])
	}
	if out["zonal"] != true {
		t.Errorf("euecs zonal = %v, want true", out["zonal"])
	}
	if out["crossAz"] != true {
		t.Errorf("euecs crossAz = %v, want true (HA VM crosses AZs)", out["crossAz"])
	}
	if out["zoneRequired"] != true {
		t.Errorf("euecs zoneRequired = %v, want true", out["zoneRequired"])
	}
}

func TestPlacementEndpointRegional(t *testing.T) {
	// euoss is seeded REGIONAL (a bucket spreads; no zone picker).
	code, out := getPlacement(t, "euoss")
	if code != 200 {
		t.Fatalf("placement euoss: code %d body %v", code, out)
	}
	if out["regionScope"] != "REGIONAL" {
		t.Errorf("euoss scope = %v, want REGIONAL", out["regionScope"])
	}
	if out["zoneRequired"] != false {
		t.Errorf("euoss zoneRequired = %v, want false", out["zoneRequired"])
	}
}

func TestPlacementNotFound(t *testing.T) {
	code, out := getPlacement(t, "scnope")
	if code != 404 {
		t.Fatalf("unknown product: code %d body %v", code, out)
	}
}

// getRegionTopology fetches the M-8 topology view (anonymous-safe like the
// other public catalogue reads).
func getRegionTopology(t *testing.T) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/catalog/region-topology", nil)
	req.Header.Set("X-Euler-TraceId", "t")
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, req)
	var env map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if data, ok := env["Data"].(map[string]any); ok {
		return rr.Code, data
	}
	return rr.Code, env
}

func TestRegionTopologyPlan(t *testing.T) {
	code, out := getRegionTopology(t)
	if code != 200 {
		t.Fatalf("region-topology: code %d body %v", code, out)
	}
	plan, _ := out["plan"].(map[string]any)
	if plan == nil {
		t.Fatalf("region-topology: no plan in %v", out)
	}
	primary, _ := plan["primary"].(map[string]any)
	standby, _ := plan["standby"].(map[string]any)
	if primary["name"] != "cn-north-1" || primary["role"] != "PRIMARY" {
		t.Errorf("primary = %v, want cn-north-1/PRIMARY", primary)
	}
	// P3 boundary: only the PRIMARY writes — the standby is read-only/DR.
	if primary["writable"] != true {
		t.Errorf("primary writable = %v, want true", primary["writable"])
	}
	if standby["name"] != "cn-east-1" || standby["role"] != "STANDBY" {
		t.Errorf("standby = %v, want cn-east-1/STANDBY", standby)
	}
	if standby["writable"] != false {
		t.Errorf("standby writable = %v, want false (不承诺异地多活写)", standby["writable"])
	}
	if plan["maxRtoMs"] != float64(30*60*1000) {
		t.Errorf("maxRtoMs = %v, want 1800000 (RTO ≤ 30min)", plan["maxRtoMs"])
	}
}

func TestRegionTopologyChannelsMeetRPO(t *testing.T) {
	_, out := getRegionTopology(t)
	channels, _ := out["channels"].([]any)
	if len(channels) != 2 {
		t.Fatalf("channels = %v, want 2 (ledger-binlog + object-storage)", channels)
	}
	for _, raw := range channels {
		c, _ := raw.(map[string]any)
		if c["name"] == "ledger-binlog" {
			if c["rpoMs"] != float64(5000) {
				t.Errorf("ledger-binlog rpoMs = %v, want 5000", c["rpoMs"])
			}
			if c["rpoMet"] != true {
				t.Errorf("ledger-binlog rpoMet = %v, want true (2.1s lag vs 5s RPO)", c["rpoMet"])
			}
		}
		if c["name"] == "object-storage" {
			if c["rpoMet"] != true {
				t.Errorf("object-storage rpoMet = %v, want true (8min lag vs 1h RPO)", c["rpoMet"])
			}
		}
	}
}

func TestRegionTopologyClassification(t *testing.T) {
	_, out := getRegionTopology(t)
	scopes, _ := out["serviceScopes"].([]any)
	found := map[string]any{}
	for _, raw := range scopes {
		s, _ := raw.(map[string]any)
		found[s["service"].(string)] = s["scope"]
	}
	if found["svc-iam"] != "GLOBAL" || found["svc-billing"] != "GLOBAL" {
		t.Errorf("iam/billing scope = %v, want GLOBAL (09§5.2 M-8 IAM/计费全局单例)", found)
	}
	if found["svc-order"] != "REGIONAL" {
		t.Errorf("svc-order scope = %v, want REGIONAL", found["svc-order"])
	}
	states, _ := out["stateClasses"].([]any)
	classes := map[string]any{}
	for _, raw := range states {
		s, _ := raw.(map[string]any)
		classes[s["state"].(string)] = s["class"]
	}
	if classes["account"] != "SHARED" || classes["ledger"] != "REPLICATED" || classes["kafka-topic"] != "REBUILT" {
		t.Errorf("state classes = %v, want account=SHARED ledger=REPLICATED kafka-topic=REBUILT", classes)
	}
	steps, _ := out["failoverSteps"].([]any)
	if len(steps) != 2 {
		t.Errorf("failoverSteps = %v, want 2 (管控面冷转热 + DNS 切换)", steps)
	}
}

func TestPlacementMissingProductCode(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/catalog/placement", nil)
	req.Header.Set("X-Euler-Account-Id", "100123")
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("missing productCode: code %d, want 400", rr.Code)
	}
}

func TestQuoteZonalRequiresZone(t *testing.T) {
	// euecs is ZONAL: a quote without zoneId must be rejected at 400, not
	// silently priced and handed to the order flow (which would then fail at
	// provisioning — a far more expensive place to discover a missing zone).
	code, out := doQuote(t, map[string]any{
		"productCode": "euecs",
		"specCode":    "euecs.s2.large.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
	})
	if code != 400 {
		t.Fatalf("zonal quote without zone: code %d, want 400 (body %v)", code, out)
	}
	if out["Code"] != "Catalog.ZoneRequired" {
		t.Errorf("error code = %v, want Catalog.ZoneRequired", out["Code"])
	}
}

func TestQuoteZonalAcceptsValidZone(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "euecs",
		"specCode":    "euecs.s2.large.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
		"zoneId":      "cn-north-1-a",
	})
	if code != 200 {
		t.Fatalf("zonal quote with valid zone: code %d body %v", code, out)
	}
}

func TestQuoteZonalRejectsBadZoneShape(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "euecs",
		"specCode":    "euecs.s2.large.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
		"zoneId":      "cnnorth1a", // not the {region}-{letter} convention
	})
	if code != 400 {
		t.Fatalf("bad zone shape: code %d, want 400 (body %v)", code, out)
	}
	if out["Code"] != "Catalog.InvalidZone" {
		t.Errorf("error code = %v, want Catalog.InvalidZone", out["Code"])
	}
}

func TestQuoteZonalRejectsZoneRegionMismatch(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "euecs",
		"specCode":    "euecs.s2.large.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
		"zoneId":      "cn-east-1-a", // valid AZ name, wrong region
	})
	if code != 400 {
		t.Fatalf("zone/region mismatch: code %d, want 400 (body %v)", code, out)
	}
	if out["Code"] != "Catalog.ZoneRegionMismatch" {
		t.Errorf("error code = %v, want Catalog.ZoneRegionMismatch", out["Code"])
	}
}

func TestQuoteRegionalIgnoresZone(t *testing.T) {
	// euoss is REGIONAL: a quote with no zoneId should succeed (the product
	// spreads; zone is not its concern).
	code, out := doQuote(t, map[string]any{
		"productCode": "euoss",
		"specCode":    "euoss.standard.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
	})
	if code != 200 {
		t.Fatalf("regional quote without zone: code %d body %v", code, out)
	}
}

func TestQuoteRequiresAccount(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"productCode": "euecs", "specCode": "euecs.s2.large.postpaid",
		"chargeType": "POSTPAID", "regionId": "cn-north-1", "zoneId": "cn-north-1-a",
	})
	req := httptest.NewRequest("POST", "/api/v1/catalog/quote", bytes.NewReader(raw))
	// no X-Euler-Account-Id
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("missing account: code %d, want 403", rr.Code)
	}
}

// --- EUECI (M-7.1, 09 §4.3 M-7) — the phase-2 elastic container instance ---

// TestSceciPlacementZonal guards the M-7.1 catalogue registration: eueci is a
// ZONAL postpaid container instance, the cheapest compute form, the "弹性"
// partner to euecs's VM. The placement contract the console's AZ picker reads
// must report ZONAL + zoneRequired, and crossAz=false (a container pod is
// single-AZ; an HA form would be a different SKU, not this product).
func TestSceciPlacementZonal(t *testing.T) {
	code, out := getPlacement(t, "eueci")
	if code != 200 {
		t.Fatalf("placement eueci: code %d body %v", code, out)
	}
	if out["regionScope"] != "ZONAL" {
		t.Errorf("eueci scope = %v, want ZONAL", out["regionScope"])
	}
	if out["zoneRequired"] != true {
		t.Errorf("eueci zoneRequired = %v, want true (ZONAL product)", out["zoneRequired"])
	}
	if out["crossAz"] != false {
		t.Errorf("eueci crossAz = %v, want false (single-AZ pod)", out["crossAz"])
	}
}

// TestSceciQuoteIsPerSecond is the M-7.1 billing-granularity contract: eueci
// is billed per-SECOND (09 §4.2), the DurationUnit M-4.2 reserved for exactly
// this. The quote path must derive the SECOND unit from the matched rule, not
// hardcode HOUR — a new billing granularity is a catalogue row, not a code
// change. The payable is the per-second unit price (×3600 reconciles to the
// hourly rate, which a separate concern settles from metering).
func TestSceciQuoteIsPerSecond(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "eueci",
		"specCode":    "eueci.c2.large.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
		"zoneId":      "cn-north-1-a",
	})
	if code != 200 {
		t.Fatalf("eueci quote: code %d body %v", code, out)
	}
	// Data lives under the envelope (doQuote unwraps it for 2xx? No — it returns
	// the whole envelope. Fetch the nested Data.)
	data, ok := out["Data"].(map[string]any)
	if !ok {
		t.Fatalf("eueci quote: no Data in envelope: %v", out)
	}
	if data["durationUnit"] != "SECOND" {
		t.Errorf("eueci durationUnit = %v, want SECOND (09 §4.2 per-second)", data["durationUnit"])
	}
	// 0.00007 yuan/sec × 3600 = 0.252 yuan/hr — matches the euecs.2large hourly
	// band (0.25/hr), confirming the per-second price is the hourly rate ÷ 3600.
	if data["payableAmount"] != "0.00007" {
		t.Errorf("eueci payableAmount = %v, want 0.00007", data["payableAmount"])
	}
	// The rule that matched is the per-second one (ruleId 19 for large).
	if data["ruleId"] != float64(19) {
		t.Errorf("eueci ruleId = %v, want 19 (per-second rule)", data["ruleId"])
	}
}

// TestSceciQuoteEnforcesZone is the M-6 gate applied to the new product: a
// ZONAL product's quote without a zoneId is rejected at the cheapest
// checkpoint, before any order is created. The same gate euecs passes.
func TestSceciQuoteEnforcesZone(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "eueci",
		"specCode":    "eueci.c2.large.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
		// no zoneId
	})
	if code != 400 {
		t.Fatalf("eueci quote without zone: code %d, want 400 (body %v)", code, out)
	}
	if out["Code"] != "Catalog.ZoneRequired" {
		t.Errorf("eueci no-zone error code = %v, want Catalog.ZoneRequired", out["Code"])
	}
}

// TestSceciRejectsPrepaid guards the M-7.1 catalogue shape: eueci is postpaid
// only — a prepaid container would just be a VM (09 §3.2 D-03). There is no
// prepaid SKU, so a PREPAID quote must fail with no-pricing-rule (the engine
// finds no MONTH rule for the sku), not silently price it.
func TestSceciRejectsPrepaid(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "eueci",
		"specCode":    "eueci.c2.large.postpaid", // only a postpaid SKU exists
		"chargeType":  "PREPAID",
		"regionId":    "cn-north-1",
		"zoneId":      "cn-north-1-a",
		"duration":    1,
	})
	if code != 400 {
		t.Fatalf("eueci prepaid quote: code %d, want 400 (body %v)", code, out)
	}
}

// --- M-7.2 EULB / M-7.3 EUAS / M-7.4 EUBACKUP — REGIONAL products -----------
//
// These three span AZs (REGIONAL), so the placement contract reports
// zoneRequired=false and the quote path does NOT demand a zoneId — unlike the
// ZONAL products above. This is the M-6 gate's other side: REGIONAL products
// must not be blocked by a zone they do not pin to.

func TestSclbPlacementRegional(t *testing.T) {
	code, out := getPlacement(t, "eulb")
	if code != 200 {
		t.Fatalf("placement eulb: code %d body %v", code, out)
	}
	if out["regionScope"] != "REGIONAL" {
		t.Errorf("eulb scope = %v, want REGIONAL (LB spans AZs)", out["regionScope"])
	}
	if out["zoneRequired"] != false {
		t.Errorf("eulb zoneRequired = %v, want false", out["zoneRequired"])
	}
	if out["crossAz"] != true {
		t.Errorf("eulb crossAz = %v, want true (cross-AZ entry point)", out["crossAz"])
	}
}

func TestScasPlacementRegional(t *testing.T) {
	code, out := getPlacement(t, "euas")
	if code != 200 {
		t.Fatalf("placement euas: code %d body %v", code, out)
	}
	if out["regionScope"] != "REGIONAL" {
		t.Errorf("euas scope = %v, want REGIONAL", out["regionScope"])
	}
	if out["zoneRequired"] != false {
		t.Errorf("euas zoneRequired = %v, want false", out["zoneRequired"])
	}
}

func TestScbackupPlacementRegional(t *testing.T) {
	code, out := getPlacement(t, "eubackup")
	if code != 200 {
		t.Fatalf("placement eubackup: code %d body %v", code, out)
	}
	if out["regionScope"] != "REGIONAL" {
		t.Errorf("eubackup scope = %v, want REGIONAL", out["regionScope"])
	}
	if out["zoneRequired"] != false {
		t.Errorf("eubackup zoneRequired = %v, want false", out["zoneRequired"])
	}
}

// A REGIONAL product's quote must succeed WITHOUT a zoneId (it spreads).
func TestSclbQuoteNoZoneRequired(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "eulb",
		"specCode":    "eulb.l1.small.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
		// no zoneId — REGIONAL products do not pin
	})
	if code != 200 {
		t.Fatalf("eulb quote without zone: code %d, want 200 (body %v)", code, out)
	}
}

// --- M-7.5 EUREDIS — ZONAL managed DB with cross-AZ HA replica ------------

func TestScredisPlacementZonalHA(t *testing.T) {
	code, out := getPlacement(t, "euredis")
	if code != 200 {
		t.Fatalf("placement euredis: code %d body %v", code, out)
	}
	if out["regionScope"] != "ZONAL" {
		t.Errorf("euredis scope = %v, want ZONAL (managed DB pinned to one AZ)", out["regionScope"])
	}
	if out["zoneRequired"] != true {
		t.Errorf("euredis zoneRequired = %v, want true", out["zoneRequired"])
	}
	if out["crossAz"] != true {
		t.Errorf("euredis crossAz = %v, want true (HA master+replica cross-AZ)", out["crossAz"])
	}
}

func TestScredisQuoteEnforcesZone(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "euredis",
		"specCode":    "euredis.redis.small.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
	})
	if code != 400 {
		t.Fatalf("euredis quote without zone: code %d, want 400 (body %v)", code, out)
	}
	if out["Code"] != "Catalog.ZoneRequired" {
		t.Errorf("euredis no-zone error = %v, want Catalog.ZoneRequired", out["Code"])
	}
}

// euredis offers BOTH prepay and postpay (managed DB). A prepaid quote must
// succeed (unlike eueci which rejects prepaid).
func TestScredisPrepaidAccepted(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "euredis",
		"specCode":    "euredis.redis.large.prepaid",
		"chargeType":  "PREPAID",
		"regionId":    "cn-north-1",
		"zoneId":      "cn-north-1-a",
		"duration":    1,
	})
	if code != 200 {
		t.Fatalf("euredis prepaid quote: code %d, want 200 (body %v)", code, out)
	}
}
