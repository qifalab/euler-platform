package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// facadeActions is the Action table of services/svc-openapi
// (resources.go actionTable, monitor.go). svc-openapi is a separate module and
// is not imported here, so the list is kept by hand: a change to the facade's
// table must be mirrored here and in catalogue.go.
var facadeActions = map[string][]string{
	"euecs": {"RunInstances", "DescribeInstances", "StartInstance", "StopInstance", "RebootInstance",
		"ModifyInstanceSpec", "ModifyInstanceAttribute", "TerminateInstances"},
	"eurds": {"CreateInstance", "DescribeInstances", "StartInstance", "StopInstance", "RestartInstance",
		"ModifyInstanceSpec", "ModifyInstanceAttribute", "DeleteInstance"},
	"euvpc": {"CreateVpc", "DescribeVpcs", "ModifyVpcAttribute", "DeleteVpc"},
	"eubs":  {"CreateDisk", "DescribeDisks", "ModifyDiskAttribute", "DeleteDisk"},
	"eueip": {"AllocateEipAddress", "DescribeEipAddresses", "ReleaseEipAddress"},
	"euoss": {"CreateBucket", "ListBuckets", "DeleteBucket"},
	"eumon": {"DescribeAlarmRules", "CreateAlarmRule", "ModifyAlarmRule", "DeleteAlarmRule", "DescribeMetricData"},
}

// createActions are the paid purchases (svc-openapi createAction), with
// whether the product is zonal.
var createActions = map[string]bool{
	"euecs.RunInstances": true, "eurds.CreateInstance": true, "eubs.CreateDisk": true,
	"eueip.AllocateEipAddress": true, "euvpc.CreateVpc": false, "euoss.CreateBucket": false,
}

type testSchemaDoc struct {
	Type        string                    `json:"type"`
	Description string                    `json:"description"`
	Required    []string                  `json:"required"`
	Properties  map[string]*testSchemaDoc `json:"properties"`
	Items       *testSchemaDoc            `json:"items"`
	Enum        []string                  `json:"enum"`
}

func catalogueByID(t *testing.T) map[string]Action {
	t.Helper()
	out := map[string]Action{}
	for _, a := range facadeCatalogue() {
		if _, dup := out[a.ID]; dup {
			t.Fatalf("catalogue lists %s twice", a.ID)
		}
		out[a.ID] = a
	}
	return out
}

func decodeSchema(t *testing.T, id, which string, raw json.RawMessage) *testSchemaDoc {
	t.Helper()
	var s testSchemaDoc
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s %s: %v", id, which, err)
	}
	if s.Type != "object" {
		t.Fatalf("%s %s: type %q, want object", id, which, s.Type)
	}
	return &s
}

// Every Action the facade serves is in the catalogue, and nothing else is.
func TestCatalogueMatchesFacadeActionTable(t *testing.T) {
	got := catalogueByID(t)
	want := map[string]bool{}
	for product, actions := range facadeActions {
		for _, a := range actions {
			id := actionID(product, a)
			want[id] = true
			if _, ok := got[id]; !ok {
				t.Errorf("facade action %s is missing from the catalogue", id)
			}
		}
	}
	var extra []string
	for id := range got {
		if !want[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("catalogue lists actions the facade does not serve: %v", extra)
	}
}

func TestCatalogueSchemasAreWellFormed(t *testing.T) {
	for id, a := range catalogueByID(t) {
		if a.Version != catalogueVersion || a.ProductCode == "" || a.ActionName == "" || a.ID != actionID(a.ProductCode, a.ActionName) {
			t.Errorf("%s: identity %+v", id, a)
		}
		params := decodeSchema(t, id, "param_schema", a.ParamSchema)
		if params.Description == "" {
			t.Errorf("%s: no description", id)
		}
		for _, r := range params.Required {
			if params.Properties[r] == nil {
				t.Errorf("%s: required %s is not a property", id, r)
			}
		}
		for name, p := range params.Properties {
			if p.Type == "" || p.Description == "" {
				t.Errorf("%s: parameter %s lacks type or description", id, name)
			}
			if p.Type == "array" && (p.Items == nil || p.Items.Type == "") {
				t.Errorf("%s: array parameter %s has no item type", id, name)
			}
		}
		resp := decodeSchema(t, id, "response_schema", a.ResponseSchema)
		if len(resp.Properties) == 0 {
			t.Errorf("%s: response schema lists no fields", id)
		}
	}
}

// The error codes are the facade's OpenAPI vocabulary, not the old
// product-prefixed codes (euecs.InvalidInstanceType).
func TestCatalogueErrorCodesAreFacadeVocabulary(t *testing.T) {
	for id, a := range catalogueByID(t) {
		seen := map[string]bool{}
		for _, c := range a.ErrorCodes {
			if !errorCodePattern.MatchString(c) || strings.HasPrefix(c, "eu") {
				t.Errorf("%s: error code %q is not in the OpenAPI vocabulary", id, c)
			}
			if seen[c] {
				t.Errorf("%s: error code %q listed twice", id, c)
			}
			seen[c] = true
		}
		for _, c := range commonErrorCodes {
			if !seen[c] {
				t.Errorf("%s: missing dispatch code %s", id, c)
			}
		}
	}
	cat := catalogueByID(t)
	for id, want := range map[string][]string{
		"euecs.RunInstances":            {"InvalidInstanceType.NotFound", "QuotaExceeded", "InsufficientBalance", "IdempotentParameterMismatch", "InvalidZoneId.NotFound"},
		"euecs.StartInstance":           {"InvalidInstanceId.NotFound", "IncorrectInstanceStatus"},
		"euecs.ModifyInstanceSpec":      {"InvalidInstanceType.NotFound", "OperationDenied.UpgradeRequiresOrder"},
		"eurds.ModifyInstanceSpec":      {"InvalidInstanceClass.NotFound"},
		"euvpc.CreateVpc":               {"InvalidCidrBlock", "QuotaExceeded"},
		"euoss.CreateBucket":            {"BucketAlreadyExists", "InvalidBucketName"},
		"euoss.DeleteBucket":            {"InvalidBucketName.NotFound"},
		"eubs.DeleteDisk":               {"InvalidDiskId.NotFound", "IncorrectDiskStatus"},
		"eueip.ReleaseEipAddress":       {"InvalidAllocationId.NotFound"},
		"eumon.ModifyAlarmRule":         {"InvalidRuleId.NotFound", "ConcurrentModification"},
		"eumon.DescribeMetricData":      {"InvalidResourceId.NotFound"},
		"euvpc.ModifyVpcAttribute":      {"InvalidVpcId.NotFound"},
		"euecs.TerminateInstances":      {"InvalidInstanceId.NotFound"},
		"eurds.CreateInstance":          {"InvalidInstanceClass.NotOnSale", "ProvisionFailed"},
		"eueip.AllocateEipAddress":      {"InvalidRegionId.NotFound"},
		"euecs.DescribeInstances":       {"ServiceUnavailable"},
		"euoss.ListBuckets":             {"InvalidParameter"},
		"eumon.DescribeAlarmRules":      {"InvalidAction.NotFound"},
		"eubs.CreateDisk":               {"InvalidDiskCategory.NotFound", "MissingParameter"},
		"eurds.DeleteInstance":          {"IncorrectInstanceStatus"},
		"euecs.ModifyInstanceAttribute": {"InvalidInstanceId.NotFound"},
	} {
		codes := map[string]bool{}
		for _, c := range cat[id].ErrorCodes {
			codes[c] = true
		}
		for _, c := range want {
			if !codes[c] {
				t.Errorf("%s: error codes %v lack %s", id, cat[id].ErrorCodes, c)
			}
		}
	}
}

// Creates document the purchase parameters svc-openapi createParams parses.
func TestCatalogueCreatesDocumentPurchaseParameters(t *testing.T) {
	cat := catalogueByID(t)
	for id, zonal := range createActions {
		a, ok := cat[id]
		if !ok {
			t.Fatalf("%s missing", id)
		}
		params := decodeSchema(t, id, "param_schema", a.ParamSchema)
		for _, p := range []string{"ClientToken", "RegionId", "ChargeType", "Period", "PeriodUnit", "SkuCode"} {
			if params.Properties[p] == nil {
				t.Errorf("%s: no %s parameter", id, p)
			}
		}
		if (params.Properties["ZoneId"] != nil) != zonal {
			t.Errorf("%s: ZoneId documented = %v, want %v (zonal product)", id, params.Properties["ZoneId"] != nil, zonal)
		}
		if ct := params.Properties["ChargeType"]; ct != nil && strings.Join(ct.Enum, ",") != "Prepaid,Postpaid" {
			t.Errorf("%s: ChargeType enum %v", id, ct.Enum)
		}
		resp := decodeSchema(t, id, "response_schema", a.ResponseSchema)
		for _, f := range []string{"OrderId", "Status"} {
			if resp.Properties[f] == nil {
				t.Errorf("%s: response lacks %s", id, f)
			}
		}
	}
	for id, req := range map[string][]string{
		"euvpc.CreateVpc":       {"CidrBlock"},
		"eubs.CreateDisk":       {"Size"},
		"euoss.CreateBucket":    {"Bucket"},
		"eumon.CreateAlarmRule": {"ProductCode", "MetricName", "Threshold"},
		"euecs.StartInstance":   {"InstanceId"},
		"eumon.DeleteAlarmRule": {"RuleId"},
		// svc-monitor selects the series by resource: no ResourceId, no data.
		"eumon.DescribeMetricData": {"MetricName", "ResourceId"},
	} {
		got := decodeSchema(t, id, "param_schema", cat[id].ParamSchema).Required
		if strings.Join(got, ",") != strings.Join(req, ",") {
			t.Errorf("%s: required = %v, want %v", id, got, req)
		}
	}
}

// Describe responses carry the list key with the resource fields the facade
// renders (svc-openapi productSpec.view) plus the paging fields.
func TestCatalogueDescribeResponses(t *testing.T) {
	cat := catalogueByID(t)
	for id, c := range map[string]struct {
		listKey string
		fields  []string
	}{
		"euecs.DescribeInstances":    {"Instances", []string{"InstanceId", "InstanceType", "ZoneId", "RegionId", "Status", "ChargeType", "SkuCode", "ExpiredTime", "InstanceName", "CreationTime"}},
		"eurds.DescribeInstances":    {"Instances", []string{"InstanceId", "InstanceClass", "Engine", "EngineVersion", "ZoneId"}},
		"euvpc.DescribeVpcs":         {"Vpcs", []string{"VpcId", "CidrBlock", "VpcName", "Status"}},
		"eubs.DescribeDisks":         {"Disks", []string{"DiskId", "DiskCategory", "Size", "DiskName", "ZoneId"}},
		"eueip.DescribeEipAddresses": {"EipAddresses", []string{"AllocationId", "Bandwidth", "InternetChargeType", "Name"}},
		"euoss.ListBuckets":          {"Buckets", []string{"Bucket", "RegionId", "StorageClass", "Status", "CreationTime"}},
		"eumon.DescribeAlarmRules":   {"AlarmRules", []string{"RuleId", "MetricName", "Threshold", "ComparisonOperator", "RuleVersion"}},
	} {
		resp := decodeSchema(t, id, "response_schema", cat[id].ResponseSchema)
		list := resp.Properties[c.listKey]
		if list == nil || list.Type != "array" || list.Items == nil {
			t.Fatalf("%s: no %s array in the response", id, c.listKey)
		}
		for _, f := range c.fields {
			if list.Items.Properties[f] == nil {
				t.Errorf("%s: %s[] lacks %s", id, c.listKey, f)
			}
		}
		for _, f := range []string{"TotalCount", "PageNumber", "PageSize"} {
			if resp.Properties[f] == nil {
				t.Errorf("%s: response lacks %s", id, f)
			}
		}
		params := decodeSchema(t, id, "param_schema", cat[id].ParamSchema)
		if params.Properties["PageNumber"] == nil || params.Properties["PageSize"] == nil {
			t.Errorf("%s: no pagination parameters", id)
		}
	}
}

// DescribeMetricData documents svc-openapi's mapping of svc-monitor's metrics
// contract (monitor.go describeMetricData).
func TestCatalogueDescribeMetricData(t *testing.T) {
	a := catalogueByID(t)["eumon.DescribeMetricData"]
	params := decodeSchema(t, a.ID, "param_schema", a.ParamSchema)
	for _, p := range []string{"MetricName", "ResourceId", "StartTime", "EndTime", "Period"} {
		if params.Properties[p] == nil {
			t.Errorf("no %s parameter", p)
		}
	}
	if m := params.Properties["MetricName"]; m == nil || strings.Join(m.Enum, ",") != "cpu_utilization,memory_utilization,request_count" {
		t.Errorf("MetricName = %+v", m)
	}
	resp := decodeSchema(t, a.ID, "response_schema", a.ResponseSchema)
	for f, typ := range map[string]string{"MetricName": "string", "ResourceId": "string", "Period": "integer", "Datapoints": "array", "Source": "string"} {
		if resp.Properties[f] == nil || resp.Properties[f].Type != typ {
			t.Errorf("response %s = %+v, want a %s", f, resp.Properties[f], typ)
		}
	}
	if len(resp.Properties) != 5 {
		t.Errorf("response fields = %d, want 5", len(resp.Properties))
	}
	if dp := resp.Properties["Datapoints"]; dp != nil && (dp.Items == nil || dp.Items.Properties["Timestamp"] == nil || dp.Items.Properties["Value"] == nil) {
		t.Errorf("Datapoints[] = %+v", dp.Items)
	}
}

// The in-memory mode serves the catalogue through the list endpoint in the
// shape the Explorer reads, with response_schema as an additive field.
func TestSeededCatalogueServedInExplorerShape(t *testing.T) {
	s := newMetaStore()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/apimeta/explorer/actions?product=euecs", nil)
	req.Header.Set(accountIDHeader, "100123")
	rec := httptest.NewRecorder()
	s.handleListActions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []map[string]json.RawMessage `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data) != len(facadeActions["euecs"]) {
		t.Fatalf("euecs actions = %d, want %d", len(env.Data), len(facadeActions["euecs"]))
	}
	for _, a := range env.Data {
		for _, k := range []string{"id", "product_code", "action_name", "version", "param_schema", "error_codes", "response_schema", "updated_by"} {
			if _, ok := a[k]; !ok {
				t.Errorf("action %s lacks %q", a["id"], k)
			}
		}
	}
}

// Registration accepts the facade's codes (and this product's prefixed ones)
// and keeps rejecting another product's prefix or a malformed code.
func TestRegisterValidatesErrorCodeVocabulary(t *testing.T) {
	s := newMetaStoreWith(newMemActionRepo())
	for _, c := range []struct {
		codes  []string
		status int
	}{
		{[]string{"InvalidInstanceId.NotFound", "QuotaExceeded", "OperationDenied.UpgradeRequiresOrder"}, http.StatusOK},
		{[]string{"euecs.InvalidInstanceType"}, http.StatusOK},
		{[]string{"euoss.BucketAlreadyExists"}, http.StatusBadRequest},
		{[]string{"invalidInstanceId.NotFound"}, http.StatusBadRequest},
		{[]string{"InvalidInstanceId..NotFound"}, http.StatusBadRequest},
		{[]string{"Invalid Instance"}, http.StatusBadRequest},
	} {
		body, _ := json.Marshal(map[string]any{
			"product_code": "euecs", "action_name": "StartInstance", "error_codes": c.codes,
			"response_schema": map[string]any{"type": "object"},
		})
		req := httptest.NewRequest(http.MethodPost, "/internal/actions", strings.NewReader(string(body)))
		req.Header.Set(accountIDHeader, "100123")
		rec := httptest.NewRecorder()
		s.handleRegisterAction(rec, req)
		if rec.Code != c.status {
			t.Errorf("codes %v: status %d, want %d (%s)", c.codes, rec.Code, c.status, rec.Body.String())
		}
	}
	got, ok, err := s.get("euecs.StartInstance")
	if err != nil || !ok || string(got.ResponseSchema) != `{"type":"object"}` {
		t.Fatalf("registered action = %+v ok=%v err=%v", got, ok, err)
	}
}

// response_schema survives the V4 column; an Action without one reads back
// without it.
func TestSQLActionRepoResponseSchemaRoundTrip(t *testing.T) {
	repo := newSQLTestActionRepo(t)
	for _, a := range facadeCatalogue() {
		if err := repo.Upsert(&a); err != nil {
			t.Fatal(err)
		}
	}
	bare := testAction("euecs", "Legacy")
	if err := repo.Upsert(bare); err != nil {
		t.Fatal(err)
	}
	got, ok, err := repo.Get("euecs.RunInstances")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	resp := decodeSchema(t, got.ID, "response_schema", got.ResponseSchema)
	if resp.Properties["InstanceId"] == nil || len(got.ErrorCodes) == 0 {
		t.Fatalf("round trip lost the response schema: %s", got.ResponseSchema)
	}
	legacy, ok, err := repo.Get(bare.ID)
	if err != nil || !ok || legacy.ResponseSchema != nil {
		t.Fatalf("legacy action = %+v ok=%v err=%v", legacy, ok, err)
	}
	all, err := repo.List("")
	if err != nil || len(all) != len(facadeCatalogue())+1 {
		t.Fatalf("list = %d err=%v", len(all), err)
	}
}
