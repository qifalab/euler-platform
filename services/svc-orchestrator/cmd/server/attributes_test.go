package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/resource"
)

// --- shared helpers for the O1–O8 tests ------------------------------------------

// acctOrders is a multi-account svc-order fake: account → order id → order.
type acctOrders map[int64]map[int64]paidOrder

func (a acctOrders) Order(_ context.Context, accountID, orderID int64) (paidOrder, error) {
	o, ok := a[accountID][orderID]
	if !ok {
		return paidOrder{}, errOrderNotFound
	}
	return o, nil
}

func (a acctOrders) add(acct int64, o paidOrder) {
	if a[acct] == nil {
		a[acct] = map[int64]paidOrder{}
	}
	a[acct][o.OrderID] = o
}

// newMultiStore is a test store over svc-order fake orders of several accounts.
func newMultiStore() (*lifecycleStore, acctOrders) {
	s := newStore()
	orders := acctOrders{}
	s.orderSrc = orders
	s.orderSink = &fakeReports{}
	return s, orders
}

// withLedgers runs fn once on the in-memory ledger and once on MySQL (skipped
// without EULER_DB_DSN), each with a fresh store whose svc-order fake knows
// orders.
func withLedgers(t *testing.T, fn func(t *testing.T, s *lifecycleStore, orders acctOrders)) {
	t.Run("memory", func(t *testing.T) {
		s, orders := newMultiStore()
		fn(t, s, orders)
	})
	t.Run("mysql", func(t *testing.T) {
		ledger := newSQLTestLedger(t)
		s, orders := newMultiStore()
		s.ledger = ledger
		fn(t, s, orders)
	})
}

// call runs one handler request as acct and returns the recorder.
func call(t *testing.T, h http.HandlerFunc, method, path string, acct int64, body any, pathValues ...string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	for i := 0; i+1 < len(pathValues); i += 2 {
		req.SetPathValue(pathValues[i], pathValues[i+1])
	}
	req.Header.Set(accountIDHeader, strconv.FormatInt(acct, 10))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func fulfilAs(t *testing.T, s *lifecycleStore, acct int64, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, s.handleFulfill, "POST", "/api/v1/orchestrator/fulfill", acct, body)
}

func detailAs(t *testing.T, s *lifecycleStore, acct int64, resID string) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, s.handleResourceDetail, "GET", "/api/v1/orchestrator/resources/"+resID, acct, nil, "id", resID)
}

func patchAs(t *testing.T, s *lifecycleStore, acct int64, resID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, s.handlePatchAttributes, "PATCH", "/api/v1/orchestrator/resources/"+resID+"/attributes", acct, body, "id", resID)
}

func releaseAs(t *testing.T, s *lifecycleStore, acct int64, resID string) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, s.handleRelease, "POST", "/api/v1/orchestrator/resources/"+resID+"/release", acct, nil, "id", resID)
}

// envelope decodes {Code, Message, Data}.
type envelope struct {
	Code    string          `json:"Code"`
	Message string          `json:"Message"`
	Data    json.RawMessage `json:"Data"`
}

func decodeEnv(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return e
}

// view is a resource detail/list record.
type view struct {
	ResourceID  string            `json:"resourceId"`
	ProductCode string            `json:"productCode"`
	Region      string            `json:"region"`
	Zone        string            `json:"zone"`
	State       string            `json:"state"`
	Attributes  map[string]string `json:"attributes"`
}

func decodeView(t *testing.T, rec *httptest.ResponseRecorder) view {
	t.Helper()
	var v view
	if err := json.Unmarshal(decodeEnv(t, rec).Data, &v); err != nil {
		t.Fatalf("decode view %q: %v", rec.Body.String(), err)
	}
	return v
}

// wantCode fails unless rec answered status with envelope code.
func wantCode(t *testing.T, what string, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status || decodeEnv(t, rec).Code != code {
		t.Fatalf("%s: %d %s, want %d %s", what, rec.Code, rec.Body.String(), status, code)
	}
}

func paidIn(id int64, product, region string) paidOrder {
	o := paid(id, product, product+".std")
	o.RegionID = region
	o.ChargeType = "POSTPAID"
	return o
}

func fulfilBodyFor(o paidOrder, params map[string]any) map[string]any {
	b := map[string]any{"orderId": o.OrderID, "productCode": o.ProductCode}
	if params != nil {
		b["params"] = params
	}
	return b
}

// --- O1: validation rules ----------------------------------------------------------

func TestValidateCreateParams(t *testing.T) {
	ok := []struct {
		product string
		params  attrValues
		want    map[string]string
	}{
		{"euecs", attrValues{"name": "  web-1 "}, map[string]string{"name": "web-1"}},
		{"euecs", nil, map[string]string{}},
		{"euvpc", attrValues{"cidrBlock": "10.0.0.0/16"}, map[string]string{"cidrBlock": "10.0.0.0/16"}},
		{"euvpc", attrValues{"cidrBlock": "192.168.10.0/28", "name": "边缘网络"}, map[string]string{"cidrBlock": "192.168.10.0/28", "name": "边缘网络"}},
		{"eurds", attrValues{"engineVersion": "8.0"}, map[string]string{"engineVersion": "8.0"}},
		{"euoss", attrValues{"bucket": "my.logs-2026"}, map[string]string{"bucket": "my.logs-2026"}},
		{"eubs", attrValues{"sizeGb": "020"}, map[string]string{"sizeGb": "20"}},
		{"eubs", attrValues{"sizeGb": "32768"}, map[string]string{"sizeGb": "32768"}},
		{"eueip", attrValues{"bandwidthMbps": "1000"}, map[string]string{"bandwidthMbps": "1000"}},
		{"euas", attrValues{"minSize": "1", "maxSize": "5", "desiredCapacity": "2", "cooldownSeconds": "300"},
			map[string]string{"minSize": "1", "maxSize": "5", "desiredCapacity": "2", "cooldownSeconds": "300"}},
		{"euas", attrValues{"maxSize": "3"}, map[string]string{"maxSize": "3"}},
		{"eubackup", attrValues{"retentionDays": "7", "schedule": "02:30"}, map[string]string{"retentionDays": "7", "schedule": "daily 02:30"}},
		{"eubackup", attrValues{"schedule": "Every 12h"}, map[string]string{"schedule": "every 12h"}},
		{"eulog", attrValues{"storageGb": "200", "retentionDays": "3650"}, map[string]string{"storageGb": "200", "retentionDays": "3650"}},
	}
	for _, tc := range ok {
		got, err := validateCreateParams(tc.product, tc.params)
		if err != nil {
			t.Fatalf("%s %v: %v", tc.product, tc.params, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s %v = %v, want %v", tc.product, tc.params, got, tc.want)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Fatalf("%s %v = %v, want %v", tc.product, tc.params, got, tc.want)
			}
		}
	}

	bad := []struct {
		product string
		params  attrValues
	}{
		{"euecs", attrValues{"name": ""}},
		{"euecs", attrValues{"name": strings.Repeat("名", 65)}},
		{"euecs", attrValues{"name": "a\x00b"}},
		{"euecs", attrValues{"sizeGb": "100"}},                // not an euecs key
		{"euecs", attrValues{"sys.quotaReservation": "qt-1"}}, // system keys are never accepted
		{"euvpc", nil}, // cidrBlock is required
		{"euvpc", attrValues{"cidrBlock": "8.8.0.0/16"}},  // public space
		{"euvpc", attrValues{"cidrBlock": "10.0.0.0/8"}},  // prefix too short
		{"euvpc", attrValues{"cidrBlock": "10.0.0.0/29"}}, // prefix too long
		{"euvpc", attrValues{"cidrBlock": "10.0.1.5/24"}}, // host bits
		{"euvpc", attrValues{"cidrBlock": "fd00::/64"}},
		{"eurds", attrValues{"engineVersion": "5.6"}},
		{"euoss", attrValues{"name": "x"}}, // bucket is required
		{"euoss", attrValues{"bucket": "My-Bucket"}},
		{"euoss", attrValues{"bucket": "ab"}},
		{"euoss", attrValues{"bucket": "a..b"}},
		{"euoss", attrValues{"bucket": "192.168.1.1"}},
		{"euoss", attrValues{"bucket": "xn--abc"}},
		{"euoss", attrValues{"bucket": "-abc"}},
		{"euoss", attrValues{"bucket": "abc.-def"}},
		{"euoss", attrValues{"bucket": "data-s3alias"}},
		{"eubs", attrValues{"sizeGb": "19"}},
		{"eubs", attrValues{"sizeGb": "32769"}},
		{"eubs", attrValues{"sizeGb": "+40"}},
		{"eubs", attrValues{"sizeGb": "40.5"}},
		{"eueip", attrValues{"bandwidthMbps": "0"}},
		{"eueip", attrValues{"bandwidthMbps": "1001"}},
		{"euas", attrValues{"minSize": "5", "maxSize": "3"}},
		{"euas", attrValues{"desiredCapacity": "10", "maxSize": "5"}},
		{"euas", attrValues{"minSize": "2", "desiredCapacity": "1"}},
		{"euas", attrValues{"cooldownSeconds": "-1"}},
		{"eubackup", attrValues{"retentionDays": "0"}},
		{"eubackup", attrValues{"retentionDays": "3651"}},
		{"eubackup", attrValues{"schedule": "0 2 * * *"}},
		{"eulog", attrValues{"storageGb": "0"}},
		{"eulog", attrValues{"retentionDays": "0"}},
	}
	for _, tc := range bad {
		if _, err := validateCreateParams(tc.product, tc.params); err == nil ||
			err.Code != "Resource.InvalidAttribute" || err.Status != 400 {
			t.Fatalf("%s %v: %v, want 400 Resource.InvalidAttribute", tc.product, tc.params, err)
		}
	}
}

func TestValidatePatchRules(t *testing.T) {
	for _, tc := range []struct {
		product string
		set     attrValues
		code    string
	}{
		{"euecs", attrValues{"name": "ok"}, ""},
		{"euas", attrValues{"maxSize": "9", "cooldownSeconds": "60"}, ""},
		{"eubackup", attrValues{"retentionDays": "14", "schedule": "every 6h"}, ""},
		{"eulog", attrValues{"storageGb": "500", "retentionDays": "90"}, ""},
		{"euoss", attrValues{"bucket": "other"}, "Resource.AttributeImmutable"},
		{"euvpc", attrValues{"cidrBlock": "10.9.0.0/16"}, "Resource.AttributeImmutable"},
		{"eubs", attrValues{"sizeGb": "100"}, "Resource.AttributeImmutable"},
		{"eurds", attrValues{"engineVersion": "8.0"}, "Resource.AttributeImmutable"},
		{"euecs", attrValues{"color": "blue"}, "Resource.AttributeImmutable"},
		{"euecs", attrValues{"sys.compensating": "x"}, "Resource.AttributeImmutable"},
		{"euecs", attrValues{"name": ""}, "Resource.InvalidAttribute"},
		{"eulog", attrValues{"storageGb": "zero"}, "Resource.InvalidAttribute"},
	} {
		_, err := validatePatch(tc.product, tc.set)
		got := ""
		if err != nil {
			got = err.Code
		}
		if got != tc.code {
			t.Fatalf("%s %v: %q, want %q", tc.product, tc.set, got, tc.code)
		}
	}
}

func TestAttrValuesAcceptsNumbers(t *testing.T) {
	var v attrValues
	if err := json.Unmarshal([]byte(`{"sizeGb": 100, "name": "x", "ratio": 1.5}`), &v); err != nil {
		t.Fatal(err)
	}
	if v["sizeGb"] != "100" || v["name"] != "x" || v["ratio"] != "1.5" {
		t.Fatalf("decoded %v", v)
	}
	if err := json.Unmarshal([]byte(`{"x": {"y": 1}}`), &v); err == nil {
		t.Fatal("an object value was accepted")
	}
	if err := json.Unmarshal([]byte(`{"x": true}`), &v); err == nil {
		t.Fatal("a boolean value was accepted")
	}
}

// --- O1: fulfil params become attributes ------------------------------------------

func TestFulfilStoresParamsAsAttributes(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		o := paidIn(301, "eubs", "cn-north-1")
		o.ChargeType, o.Duration, o.DurationUnit = "PREPAID", 1, "MONTH"
		orders.add(100123, o)
		rec := fulfilAs(t, s, 100123, fulfilBodyFor(o, map[string]any{"name": "data-disk", "sizeGb": 100}))
		if rec.Code != 200 {
			t.Fatalf("fulfil: %d %s", rec.Code, rec.Body.String())
		}
		resID := decodeFulfil(t, rec).Data.ResourceID
		v := decodeView(t, detailAs(t, s, 100123, resID))
		if v.Attributes["name"] != "data-disk" || v.Attributes["sizeGb"] != "100" || len(v.Attributes) != 2 {
			t.Fatalf("detail attributes = %v (system keys must not show)", v.Attributes)
		}
		// The term and nothing else rides along in the system keys.
		attrs, err := s.ledger.Attrs(100123, resID)
		if err != nil || attrs[attrTermDuration] != "1" || attrs[attrTermUnit] != "MONTH" {
			t.Fatalf("stored attributes = %v, %v", attrs, err)
		}
		// The list carries them too, and an attribute-less resource gets {}.
		list := call(t, s.handleListResources, "GET", "/api/v1/orchestrator/resources", 100123, nil)
		var items []view
		if err := json.Unmarshal(decodeEnv(t, list).Data, &items); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, it := range items {
			if it.Attributes == nil {
				t.Fatalf("list item %s has no attributes object", it.ResourceID)
			}
			if it.ResourceID == resID {
				found = it.Attributes["name"] == "data-disk" && len(it.Attributes) == 2
			}
		}
		if !found {
			t.Fatalf("list = %+v", items)
		}
		// The driver got the parameters as the spec's params, and the spec was
		// recorded for a restarted driver (AppliedSpec).
		if spec, ok, err := s.ledger.AppliedSpec(resID); err != nil || !ok || spec.Params["sizeGb"] != "100" ||
			spec.Params["name"] != "data-disk" || spec.Params[attrTermUnit] != "" {
			t.Fatalf("applied spec = %+v %v %v", spec, ok, err)
		}
	})
}

func TestFulfilRejectsInvalidParamsAndKeepsOrderRetryable(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		o := paidIn(302, "euvpc", "cn-north-1")
		orders.add(100123, o)
		before := len(mustList(t, s, 100123))
		for _, params := range []map[string]any{
			nil,                                    // cidrBlock missing
			{"cidrBlock": "8.8.8.0/24"},            // public
			{"cidrBlock": "10.0.0.0/16", "x": "y"}, // unknown key
		} {
			rec := fulfilAs(t, s, 100123, fulfilBodyFor(o, params))
			wantCode(t, "invalid params", rec, 400, "Resource.InvalidAttribute")
		}
		if n := len(mustList(t, s, 100123)); n != before {
			t.Fatalf("a rejected fulfil created a resource (%d → %d)", before, n)
		}
		if got := reportsOf(s).calls(); len(got) != 0 {
			t.Fatalf("a parameter rejection reported %+v (the order must stay PAID)", got)
		}
		// The same order then fulfils with valid parameters.
		rec := fulfilAs(t, s, 100123, fulfilBodyFor(o, map[string]any{"cidrBlock": "10.0.0.0/16"}))
		if rec.Code != 200 {
			t.Fatalf("retry with valid params: %d %s", rec.Code, rec.Body.String())
		}
		// A malformed params value is a malformed body.
		rec = fulfilAs(t, s, 100123, map[string]any{"orderId": 302, "productCode": "euvpc", "params": map[string]any{"cidrBlock": []int{1}}})
		wantCode(t, "malformed params", rec, 400, "Common.InvalidParameter")
	})
}

func TestFulfilReplayIgnoresParams(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		o := paidIn(303, "euecs", "cn-north-1")
		orders.add(100123, o)
		first := decodeFulfil(t, fulfilAs(t, s, 100123, fulfilBodyFor(o, map[string]any{"name": "first"})))
		rec := fulfilAs(t, s, 100123, fulfilBodyFor(o, map[string]any{"name": "", "bogus": "1"}))
		if rec.Code != 200 || decodeFulfil(t, rec).Data.ResourceID != first.Data.ResourceID {
			t.Fatalf("replay with other params: %d %s", rec.Code, rec.Body.String())
		}
		if v := decodeView(t, detailAs(t, s, 100123, first.Data.ResourceID)); v.Attributes["name"] != "first" {
			t.Fatalf("replay changed attributes: %v", v.Attributes)
		}
	})
}

func mustList(t *testing.T, s *lifecycleStore, acct int64) []*resource.Instance {
	t.Helper()
	l, err := s.ledger.List(acct)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestFulfilBucketUniquePerRegionAcrossAccounts(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		a, b, c := paidIn(311, "euoss", "cn-north-1"), paidIn(312, "euoss", "cn-north-1"), paidIn(313, "euoss", "cn-east-1")
		orders.add(100123, a)
		orders.add(200456, b)
		orders.add(200456, c)
		bucket := map[string]any{"bucket": "shared-name"}

		recA := fulfilAs(t, s, 100123, fulfilBodyFor(a, bucket))
		if recA.Code != 200 {
			t.Fatalf("first bucket: %d %s", recA.Code, recA.Body.String())
		}
		resA := decodeFulfil(t, recA).Data.ResourceID
		wantCode(t, "same bucket, same region, other account", fulfilAs(t, s, 200456, fulfilBodyFor(b, bucket)), 409, "Resource.BucketExists")
		if n := len(mustList(t, s, 200456)); n != 0 {
			t.Fatalf("the losing account got %d resources", n)
		}
		// Another region is another namespace.
		if rec := fulfilAs(t, s, 200456, fulfilBodyFor(c, bucket)); rec.Code != 200 {
			t.Fatalf("same bucket in another region: %d %s", rec.Code, rec.Body.String())
		}
		// Releasing the bucket frees the name; the refused order (still PAID)
		// now gets it.
		if rec := releaseAs(t, s, 100123, resA); rec.Code != 200 {
			t.Fatalf("release: %d %s", rec.Code, rec.Body.String())
		}
		if rec := fulfilAs(t, s, 200456, fulfilBodyFor(b, bucket)); rec.Code != 200 {
			t.Fatalf("bucket after release: %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestFulfilVPCBlockMustNotOverlap(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		for i, region := range []string{"cn-north-1", "cn-north-1", "cn-north-1", "cn-east-1"} {
			orders.add(100123, paidIn(int64(321+i), "euvpc", region))
		}
		orders.add(200456, paidIn(325, "euvpc", "cn-north-1"))
		block := func(id int64, acct int64, cidr string) *httptest.ResponseRecorder {
			return fulfilAs(t, s, acct, fulfilBodyFor(orders[acct][id], map[string]any{"cidrBlock": cidr}))
		}
		first := block(321, 100123, "10.0.0.0/16")
		if first.Code != 200 {
			t.Fatalf("first VPC: %d %s", first.Code, first.Body.String())
		}
		rec := block(322, 100123, "10.0.128.0/17")
		wantCode(t, "overlapping VPC", rec, 409, "Resource.InvalidAttribute")
		if !strings.Contains(decodeEnv(t, rec).Message, decodeFulfil(t, first).Data.ResourceID) {
			t.Fatalf("the refusal should name the VPC it overlaps: %s", rec.Body.String())
		}
		if rec := block(323, 100123, "10.1.0.0/16"); rec.Code != 200 {
			t.Fatalf("adjacent block: %d %s", rec.Code, rec.Body.String())
		}
		if rec := block(324, 100123, "10.0.0.0/16"); rec.Code != 200 {
			t.Fatalf("same block in another region: %d %s", rec.Code, rec.Body.String())
		}
		if rec := block(325, 200456, "10.0.0.0/16"); rec.Code != 200 {
			t.Fatalf("same block in another account: %d %s", rec.Code, rec.Body.String())
		}
		// Once the first VPC is released its range is free again; the refused
		// order (still PAID) can be retried with it.
		if rec := releaseAs(t, s, 100123, decodeFulfil(t, first).Data.ResourceID); rec.Code != 200 {
			t.Fatalf("release: %d %s", rec.Code, rec.Body.String())
		}
		if rec := block(322, 100123, "10.0.128.0/17"); rec.Code != 200 {
			t.Fatalf("block after the overlapping VPC was released: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// --- O2: PATCH attributes ------------------------------------------------------------

func TestPatchAttributes(t *testing.T) {
	withLedgers(t, func(t *testing.T, s *lifecycleStore, orders acctOrders) {
		as, bk := paidIn(331, "euas", "cn-north-1"), paidIn(332, "euoss", "cn-north-1")
		orders.add(100123, as)
		orders.add(100123, bk)
		asID := decodeFulfil(t, fulfilAs(t, s, 100123, fulfilBodyFor(as, map[string]any{"minSize": "1", "maxSize": "4"}))).Data.ResourceID
		bkID := decodeFulfil(t, fulfilAs(t, s, 100123, fulfilBodyFor(bk, map[string]any{"bucket": "patch-me"}))).Data.ResourceID

		rec := patchAs(t, s, 100123, asID, map[string]any{"attributes": map[string]any{"name": "web-asg", "desiredCapacity": 3}})
		if rec.Code != 200 {
			t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
		}
		v := decodeView(t, rec)
		if v.ResourceID != asID || v.Attributes["name"] != "web-asg" || v.Attributes["desiredCapacity"] != "3" || v.Attributes["minSize"] != "1" {
			t.Fatalf("patched detail = %+v", v)
		}
		// The policy is validated merged with what is stored: desired 3 with
		// the stored maxSize 4 is fine, maxSize 2 under desired 3 is not.
		wantCode(t, "policy violation", patchAs(t, s, 100123, asID, map[string]any{"attributes": map[string]any{"maxSize": "2"}}), 400, "Resource.InvalidAttribute")
		if v := decodeView(t, detailAs(t, s, 100123, asID)); v.Attributes["maxSize"] != "4" {
			t.Fatalf("a refused patch changed maxSize to %s", v.Attributes["maxSize"])
		}
		wantCode(t, "immutable key", patchAs(t, s, 100123, bkID, map[string]any{"attributes": map[string]any{"bucket": "renamed"}}), 400, "Resource.AttributeImmutable")
		wantCode(t, "unknown key", patchAs(t, s, 100123, bkID, map[string]any{"attributes": map[string]any{"tier": "gold"}}), 400, "Resource.AttributeImmutable")
		wantCode(t, "invalid name", patchAs(t, s, 100123, bkID, map[string]any{"attributes": map[string]any{"name": ""}}), 400, "Resource.InvalidAttribute")
		wantCode(t, "empty body", patchAs(t, s, 100123, bkID, map[string]any{"attributes": map[string]any{}}), 400, "Common.InvalidParameter")
		wantCode(t, "malformed body", patchAs(t, s, 100123, bkID, "{"), 400, "Common.InvalidParameter")
		wantCode(t, "another account", patchAs(t, s, 200456, bkID, map[string]any{"attributes": map[string]any{"name": "x"}}), 404, "Resource.NotFound")
		wantCode(t, "missing resource", patchAs(t, s, 100123, "euoss-cn-north-1-01-ffffffff", map[string]any{"attributes": map[string]any{"name": "x"}}), 404, "Resource.NotFound")

		// Not once the resource is released.
		if rec := releaseAs(t, s, 100123, bkID); rec.Code != 200 {
			t.Fatalf("release: %d %s", rec.Code, rec.Body.String())
		}
		wantCode(t, "released resource", patchAs(t, s, 100123, bkID, map[string]any{"attributes": map[string]any{"name": "late"}}), 409, "Resource.InvalidState")

		// Every accepted patch is journalled (SQL ledger).
		if sl, ok := s.ledger.(*sqlLedger); ok {
			var n int
			if err := sl.db.QueryRow(`SELECT COUNT(*) FROM resource_state_log
			  WHERE resource_id = ? AND from_status = to_status AND reason LIKE 'attributes updated:%'`, asID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("journal rows for the patch = %d, want 1", n)
			}
		}
	})
}

func TestPatchAttributesStateGate(t *testing.T) {
	for st, ok := range map[resource.State]bool{
		resource.StateCreating: true, resource.StateRunning: true, resource.StateStopped: true,
		resource.StateLocked: true, resource.StateExpired: true, resource.StateUpgrading: true,
		resource.StateReleasing: false, resource.StateReleased: false, resource.StateCreateFailed: false,
	} {
		if attributesWritable(st) != ok {
			t.Fatalf("attributesWritable(%s) = %v", st, !ok)
		}
	}
}
