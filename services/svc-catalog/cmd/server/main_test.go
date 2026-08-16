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
	return recoverMiddleware(requestIDMiddleware(mux))
}

func doQuote(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/v1/catalog/quote", bytes.NewReader(raw))
	req.Header.Set("X-Sc-Account-Id", "100123")
	req.Header.Set("X-Sc-TraceId", "t")
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func getPlacement(t *testing.T, productCode string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/catalog/placement?productCode="+productCode, nil)
	req.Header.Set("X-Sc-Account-Id", "100123")
	req.Header.Set("X-Sc-TraceId", "t")
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
	// scecs is seeded ZONAL with cross-AZ replicas.
	code, out := getPlacement(t, "scecs")
	if code != 200 {
		t.Fatalf("placement scecs: code %d body %v", code, out)
	}
	if out["regionScope"] != "ZONAL" {
		t.Errorf("scecs scope = %v, want ZONAL", out["regionScope"])
	}
	if out["zonal"] != true {
		t.Errorf("scecs zonal = %v, want true", out["zonal"])
	}
	if out["crossAz"] != true {
		t.Errorf("scecs crossAz = %v, want true (HA VM crosses AZs)", out["crossAz"])
	}
	if out["zoneRequired"] != true {
		t.Errorf("scecs zoneRequired = %v, want true", out["zoneRequired"])
	}
}

func TestPlacementEndpointRegional(t *testing.T) {
	// scoss is seeded REGIONAL (a bucket spreads; no zone picker).
	code, out := getPlacement(t, "scoss")
	if code != 200 {
		t.Fatalf("placement scoss: code %d body %v", code, out)
	}
	if out["regionScope"] != "REGIONAL" {
		t.Errorf("scoss scope = %v, want REGIONAL", out["regionScope"])
	}
	if out["zoneRequired"] != false {
		t.Errorf("scoss zoneRequired = %v, want false", out["zoneRequired"])
	}
}

func TestPlacementNotFound(t *testing.T) {
	code, out := getPlacement(t, "scnope")
	if code != 404 {
		t.Fatalf("unknown product: code %d body %v", code, out)
	}
}

func TestPlacementMissingProductCode(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/catalog/placement", nil)
	req.Header.Set("X-Sc-Account-Id", "100123")
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("missing productCode: code %d, want 400", rr.Code)
	}
}

func TestQuoteZonalRequiresZone(t *testing.T) {
	// scecs is ZONAL: a quote without zoneId must be rejected at 400, not
	// silently priced and handed to the order flow (which would then fail at
	// provisioning — a far more expensive place to discover a missing zone).
	code, out := doQuote(t, map[string]any{
		"productCode": "scecs",
		"specCode":    "scecs.s2.large.postpaid",
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
		"productCode": "scecs",
		"specCode":    "scecs.s2.large.postpaid",
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
		"productCode": "scecs",
		"specCode":    "scecs.s2.large.postpaid",
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
		"productCode": "scecs",
		"specCode":    "scecs.s2.large.postpaid",
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
	// scoss is REGIONAL: a quote with no zoneId should succeed (the product
	// spreads; zone is not its concern).
	code, out := doQuote(t, map[string]any{
		"productCode": "scoss",
		"specCode":    "scoss.standard.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
	})
	if code != 200 {
		t.Fatalf("regional quote without zone: code %d body %v", code, out)
	}
}

func TestQuoteRequiresAccount(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"productCode": "scecs", "specCode": "scecs.s2.large.postpaid",
		"chargeType": "POSTPAID", "regionId": "cn-north-1", "zoneId": "cn-north-1-a",
	})
	req := httptest.NewRequest("POST", "/api/v1/catalog/quote", bytes.NewReader(raw))
	// no X-Sc-Account-Id
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("missing account: code %d, want 403", rr.Code)
	}
}

// --- SCECI (M-7.1, 09 §4.3 M-7) — the phase-2 elastic container instance ---

// TestSceciPlacementZonal guards the M-7.1 catalogue registration: sceci is a
// ZONAL postpaid container instance, the cheapest compute form, the "弹性"
// partner to scecs's VM. The placement contract the console's AZ picker reads
// must report ZONAL + zoneRequired, and crossAz=false (a container pod is
// single-AZ; an HA form would be a different SKU, not this product).
func TestSceciPlacementZonal(t *testing.T) {
	code, out := getPlacement(t, "sceci")
	if code != 200 {
		t.Fatalf("placement sceci: code %d body %v", code, out)
	}
	if out["regionScope"] != "ZONAL" {
		t.Errorf("sceci scope = %v, want ZONAL", out["regionScope"])
	}
	if out["zoneRequired"] != true {
		t.Errorf("sceci zoneRequired = %v, want true (ZONAL product)", out["zoneRequired"])
	}
	if out["crossAz"] != false {
		t.Errorf("sceci crossAz = %v, want false (single-AZ pod)", out["crossAz"])
	}
}

// TestSceciQuoteIsPerSecond is the M-7.1 billing-granularity contract: sceci
// is billed per-SECOND (09 §4.2), the DurationUnit M-4.2 reserved for exactly
// this. The quote path must derive the SECOND unit from the matched rule, not
// hardcode HOUR — a new billing granularity is a catalogue row, not a code
// change. The payable is the per-second unit price (×3600 reconciles to the
// hourly rate, which a separate concern settles from metering).
func TestSceciQuoteIsPerSecond(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "sceci",
		"specCode":    "sceci.c2.large.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
		"zoneId":      "cn-north-1-a",
	})
	if code != 200 {
		t.Fatalf("sceci quote: code %d body %v", code, out)
	}
	// Data lives under the envelope (doQuote unwraps it for 2xx? No — it returns
	// the whole envelope. Fetch the nested Data.)
	data, ok := out["Data"].(map[string]any)
	if !ok {
		t.Fatalf("sceci quote: no Data in envelope: %v", out)
	}
	if data["durationUnit"] != "SECOND" {
		t.Errorf("sceci durationUnit = %v, want SECOND (09 §4.2 per-second)", data["durationUnit"])
	}
	// 0.00007 yuan/sec × 3600 = 0.252 yuan/hr — matches the scecs.2large hourly
	// band (0.25/hr), confirming the per-second price is the hourly rate ÷ 3600.
	if data["payableAmount"] != "0.00007" {
		t.Errorf("sceci payableAmount = %v, want 0.00007", data["payableAmount"])
	}
	// The rule that matched is the per-second one (ruleId 19 for large).
	if data["ruleId"] != float64(19) {
		t.Errorf("sceci ruleId = %v, want 19 (per-second rule)", data["ruleId"])
	}
}

// TestSceciQuoteEnforcesZone is the M-6 gate applied to the new product: a
// ZONAL product's quote without a zoneId is rejected at the cheapest
// checkpoint, before any order is created. The same gate scecs passes.
func TestSceciQuoteEnforcesZone(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "sceci",
		"specCode":    "sceci.c2.large.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
		// no zoneId
	})
	if code != 400 {
		t.Fatalf("sceci quote without zone: code %d, want 400 (body %v)", code, out)
	}
	if out["Code"] != "Catalog.ZoneRequired" {
		t.Errorf("sceci no-zone error code = %v, want Catalog.ZoneRequired", out["Code"])
	}
}

// TestSceciRejectsPrepaid guards the M-7.1 catalogue shape: sceci is postpaid
// only — a prepaid container would just be a VM (09 §3.2 D-03). There is no
// prepaid SKU, so a PREPAID quote must fail with no-pricing-rule (the engine
// finds no MONTH rule for the sku), not silently price it.
func TestSceciRejectsPrepaid(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "sceci",
		"specCode":    "sceci.c2.large.postpaid", // only a postpaid SKU exists
		"chargeType":  "PREPAID",
		"regionId":    "cn-north-1",
		"zoneId":      "cn-north-1-a",
		"duration":    1,
	})
	if code != 400 {
		t.Fatalf("sceci prepaid quote: code %d, want 400 (body %v)", code, out)
	}
}

// --- M-7.2 SCLB / M-7.3 SCAS / M-7.4 SCBACKUP — REGIONAL products -----------
//
// These three span AZs (REGIONAL), so the placement contract reports
// zoneRequired=false and the quote path does NOT demand a zoneId — unlike the
// ZONAL products above. This is the M-6 gate's other side: REGIONAL products
// must not be blocked by a zone they do not pin to.

func TestSclbPlacementRegional(t *testing.T) {
	code, out := getPlacement(t, "sclb")
	if code != 200 {
		t.Fatalf("placement sclb: code %d body %v", code, out)
	}
	if out["regionScope"] != "REGIONAL" {
		t.Errorf("sclb scope = %v, want REGIONAL (LB spans AZs)", out["regionScope"])
	}
	if out["zoneRequired"] != false {
		t.Errorf("sclb zoneRequired = %v, want false", out["zoneRequired"])
	}
	if out["crossAz"] != true {
		t.Errorf("sclb crossAz = %v, want true (cross-AZ entry point)", out["crossAz"])
	}
}

func TestScasPlacementRegional(t *testing.T) {
	code, out := getPlacement(t, "scas")
	if code != 200 {
		t.Fatalf("placement scas: code %d body %v", code, out)
	}
	if out["regionScope"] != "REGIONAL" {
		t.Errorf("scas scope = %v, want REGIONAL", out["regionScope"])
	}
	if out["zoneRequired"] != false {
		t.Errorf("scas zoneRequired = %v, want false", out["zoneRequired"])
	}
}

func TestScbackupPlacementRegional(t *testing.T) {
	code, out := getPlacement(t, "scbackup")
	if code != 200 {
		t.Fatalf("placement scbackup: code %d body %v", code, out)
	}
	if out["regionScope"] != "REGIONAL" {
		t.Errorf("scbackup scope = %v, want REGIONAL", out["regionScope"])
	}
	if out["zoneRequired"] != false {
		t.Errorf("scbackup zoneRequired = %v, want false", out["zoneRequired"])
	}
}

// A REGIONAL product's quote must succeed WITHOUT a zoneId (it spreads).
func TestSclbQuoteNoZoneRequired(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "sclb",
		"specCode":    "sclb.l1.small.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
		// no zoneId — REGIONAL products do not pin
	})
	if code != 200 {
		t.Fatalf("sclb quote without zone: code %d, want 200 (body %v)", code, out)
	}
}

// --- M-7.5 SCREDIS — ZONAL managed DB with cross-AZ HA replica ------------

func TestScredisPlacementZonalHA(t *testing.T) {
	code, out := getPlacement(t, "scredis")
	if code != 200 {
		t.Fatalf("placement scredis: code %d body %v", code, out)
	}
	if out["regionScope"] != "ZONAL" {
		t.Errorf("scredis scope = %v, want ZONAL (managed DB pinned to one AZ)", out["regionScope"])
	}
	if out["zoneRequired"] != true {
		t.Errorf("scredis zoneRequired = %v, want true", out["zoneRequired"])
	}
	if out["crossAz"] != true {
		t.Errorf("scredis crossAz = %v, want true (HA master+replica cross-AZ)", out["crossAz"])
	}
}

func TestScredisQuoteEnforcesZone(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "scredis",
		"specCode":    "scredis.redis.small.postpaid",
		"chargeType":  "POSTPAID",
		"regionId":    "cn-north-1",
	})
	if code != 400 {
		t.Fatalf("scredis quote without zone: code %d, want 400 (body %v)", code, out)
	}
	if out["Code"] != "Catalog.ZoneRequired" {
		t.Errorf("scredis no-zone error = %v, want Catalog.ZoneRequired", out["Code"])
	}
}

// scredis offers BOTH prepay and postpay (managed DB). A prepaid quote must
// succeed (unlike sceci which rejects prepaid).
func TestScredisPrepaidAccepted(t *testing.T) {
	code, out := doQuote(t, map[string]any{
		"productCode": "scredis",
		"specCode":    "scredis.redis.large.prepaid",
		"chargeType":  "PREPAID",
		"regionId":    "cn-north-1",
		"zoneId":      "cn-north-1-a",
		"duration":    1,
	})
	if code != 200 {
		t.Fatalf("scredis prepaid quote: code %d, want 200 (body %v)", code, out)
	}
}
