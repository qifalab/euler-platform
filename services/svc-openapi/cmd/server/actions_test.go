package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/httpmw"
)

func TestEnvelopeAndRouting(t *testing.T) {
	e := newTestEnv(t)

	t.Run("success envelope carries the gateway trace id", func(t *testing.T) {
		r := e.do(acctA, "euecs", "DescribeInstances", nil)
		if r.status != http.StatusOK || r.code != "OK" {
			t.Fatalf("got %d %v", r.status, r.env)
		}
		rid := str(t, r.env, "RequestId")
		if !strings.HasPrefix(rid, "trace-") || r.header.Get(traceIDHeader) != rid {
			t.Fatalf("RequestId %q / header %q do not carry the gateway trace id", rid, r.header.Get(traceIDHeader))
		}
		if !e.cloud.traces[rid] {
			t.Fatalf("trace id %s was not propagated downstream", rid)
		}
	})

	cases := []struct {
		name   string
		mutate func(r *http.Request)
		status int
		code   string
	}{
		{"unknown host", func(r *http.Request) { r.Host = "euxyz.api.euler.emoera.com" }, 400, "InvalidProduct.NotFound"},
		{"console host", func(r *http.Request) { r.Host = "console.euler.emoera.com" }, 400, "InvalidProduct.NotFound"},
		{"host with port", func(r *http.Request) { r.Host = "euecs.api.euler.emoera.com:443" }, 200, "OK"},
		{"missing Action", func(r *http.Request) { r.URL.RawQuery = "Version=2026-08-01" }, 400, "MissingParameter"},
		{"unknown Action", func(r *http.Request) { r.URL.RawQuery = "Action=CreateBucket&Version=2026-08-01" }, 400, "InvalidAction.NotFound"},
		{"malformed Version", func(r *http.Request) { r.URL.RawQuery = "Action=DescribeInstances&Version=v1" }, 400, "InvalidParameter"},
		{"duplicated Action", func(r *http.Request) {
			r.URL.RawQuery = "Action=DescribeInstances&Action=RunInstances"
		}, 400, "InvalidParameter"},
		{"other path", func(r *http.Request) { r.URL.Path = "/internal/orders" }, 404, "InvalidPath.NotFound"},
		{"missing account", func(r *http.Request) { r.Header.Del(accountIDHeader) }, 403, "Common.MissingAccountId"},
		{"missing internal token", func(r *http.Request) { r.Header.Del(httpmw.InternalTokenHeader) }, 403, "Common.Forbidden"},
		{"forged internal token", func(r *http.Request) { r.Header.Set(httpmw.InternalTokenHeader, "guess") }, 403, "Common.Forbidden"},
		{"malformed JSON body", func(r *http.Request) {
			*r = *httptest.NewRequest(http.MethodPost, r.URL.String(), strings.NewReader(`{"InstanceId":`)).WithContext(r.Context())
			r.Host = "euecs.api.euler.emoera.com"
			r.Header.Set(accountIDHeader, "1001")
			r.Header.Set(httpmw.InternalTokenHeader, testToken)
		}, 400, "InvalidParameter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := e.request(acctA, "euecs", "DescribeInstances", nil)
			tc.mutate(req)
			r := e.send(req)
			if r.status != tc.status || r.code != tc.code {
				t.Fatalf("got HTTP %d %q (%v), want HTTP %d %q", r.status, r.code, r.env, tc.status, tc.code)
			}
		})
	}
	if e.cloud.missingToken != 0 {
		t.Fatalf("%d downstream calls went out without the internal token", e.cloud.missingToken)
	}
}

func TestParameterEncodings(t *testing.T) {
	e := newTestEnv(t)
	id := str(t, e.ok(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a"}), "InstanceId")

	check := func(t *testing.T, req *http.Request) {
		t.Helper()
		r := e.send(req)
		if r.status != http.StatusOK {
			t.Fatalf("got %d %v", r.status, r.env)
		}
		if got := list(t, r.data, "Instances"); len(got) != 1 || got[0]["InstanceId"] != id {
			t.Fatalf("Instances = %v", got)
		}
	}
	t.Run("query string (GET)", func(t *testing.T) {
		req := e.request(acctA, "euecs", "DescribeInstances", nil)
		req.Method = http.MethodGet
		req.Body = http.NoBody
		req.URL.RawQuery += "&InstanceId=" + url.QueryEscape(id)
		check(t, req)
	})
	t.Run("form body", func(t *testing.T) {
		req := e.request(acctA, "euecs", "DescribeInstances", nil)
		*req = *httptest.NewRequest(http.MethodPost, req.URL.String(), strings.NewReader("InstanceIds.1="+url.QueryEscape(id))).WithContext(req.Context())
		req.Host = "euecs.api.euler.emoera.com"
		req.Header.Set(accountIDHeader, "1001")
		req.Header.Set(httpmw.InternalTokenHeader, testToken)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		check(t, req)
	})
	t.Run("JSON body under the form content type (sdk/python urllib default)", func(t *testing.T) {
		req := e.request(acctA, "euecs", "DescribeInstances", map[string]any{"InstanceIds": []string{id}})
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		check(t, req)
	})
	t.Run("Action in the body is ignored", func(t *testing.T) {
		r := e.do(acctA, "euecs", "DescribeInstances", map[string]any{"Action": "TerminateInstances", "InstanceId": id})
		if r.status != http.StatusOK || len(list(t, r.data, "Instances")) != 1 {
			t.Fatalf("got %d %v", r.status, r.env)
		}
	})
}

func TestECSLifecycle(t *testing.T) {
	e := newTestEnv(t)
	created := e.ok(acctA, "euecs", "RunInstances", map[string]any{
		"InstanceType": "s2.large", "ImageId": "img-ubuntu-2204", "InstanceName": "web-1",
		"RegionId": "cn-north-1", "ZoneId": "cn-north-1-b", "ChargeType": "PostPaid", "ClientToken": "tok-1",
	})
	id := str(t, created, "InstanceId")
	if str(t, created, "OrderId") == "" || created["Status"] != "Running" {
		t.Fatalf("RunInstances = %v", created)
	}

	desc := e.ok(acctA, "euecs", "DescribeInstances", nil)
	inst := list(t, desc, "Instances")
	if len(inst) != 1 || desc["TotalCount"] != float64(1) || desc["PageNumber"] != float64(1) || desc["PageSize"] != float64(10) {
		t.Fatalf("DescribeInstances = %v", desc)
	}
	want := map[string]any{"InstanceId": id, "InstanceType": "s2.large", "InstanceName": "web-1", "RegionId": "cn-north-1",
		"ZoneId": "cn-north-1-b", "Status": "Running", "ChargeType": "Postpaid", "SkuCode": "euecs.s2.large.postpaid",
		"ExpiredTime": ""}
	for k, v := range want {
		if inst[0][k] != v {
			t.Errorf("Instances[0].%s = %v, want %v", k, inst[0][k], v)
		}
	}
	if inst[0]["CreationTime"] == "" {
		t.Errorf("CreationTime missing: %v", inst[0])
	}

	steps := []struct {
		action string
		status string
	}{{"StopInstance", "Stopped"}, {"StartInstance", "Running"}, {"RebootInstance", "Running"}}
	for _, st := range steps {
		got := e.ok(acctA, "euecs", st.action, map[string]any{"InstanceId": id})
		if got["Status"] != st.status {
			t.Fatalf("%s = %v, want Status %s", st.action, got, st.status)
		}
	}
	e.fail(acctA, "euecs", "StartInstance", map[string]any{"InstanceId": id}, 409, "IncorrectInstanceStatus")

	mod := e.ok(acctA, "euecs", "ModifyInstanceSpec", map[string]any{"InstanceId": id, "InstanceType": "s2.xlarge"})
	if mod["SkuCode"] != "euecs.s2.xlarge.postpaid" {
		t.Fatalf("ModifyInstanceSpec = %v", mod)
	}
	e.fail(acctA, "euecs", "ModifyInstanceSpec", map[string]any{"InstanceId": id, "InstanceType": "s9.huge"}, 400, "InvalidInstanceType.NotFound")
	e.fail(acctA, "euecs", "ModifyInstanceSpec", map[string]any{"InstanceId": id, "InstanceType": "s2.legacy"}, 409, "InvalidInstanceType.NotOnSale")
	e.fail(acctA, "euecs", "ModifyInstanceSpec", map[string]any{"InstanceId": id}, 400, "MissingParameter")
	e.ok(acctA, "euecs", "ModifyInstanceAttribute", map[string]any{"InstanceId": id, "InstanceName": "web-renamed"})

	page := e.ok(acctA, "euecs", "DescribeInstances", map[string]any{"InstanceId": id})
	// One id is still a page: the instance is in Instances[], not repeated at
	// the top level of Data.
	if one := single(t, page, "Instances"); one["InstanceType"] != "s2.xlarge" || one["InstanceName"] != "web-renamed" {
		t.Fatalf("DescribeInstances(InstanceId) = %v", page)
	}
	if _, top := page["InstanceType"]; top || len(page) != 4 {
		t.Fatalf("DescribeInstances(InstanceId) Data = %v, want only Instances and the paging fields", page)
	}

	term := e.ok(acctA, "euecs", "TerminateInstances", map[string]any{"InstanceId": id})
	if term["InstanceId"] != id {
		t.Fatalf("TerminateInstances = %v", term)
	}
	if got := list(t, e.ok(acctA, "euecs", "DescribeInstances", nil), "Instances"); len(got) != 0 {
		t.Fatalf("terminated instance still listed: %v", got)
	}
	e.fail(acctA, "euecs", "TerminateInstances", map[string]any{"InstanceId": id}, 404, "InvalidInstanceId.NotFound")
	e.fail(acctA, "euecs", "TerminateInstances", nil, 400, "MissingParameter")
}

func TestPrepaidPurchase(t *testing.T) {
	e := newTestEnv(t)
	before := e.cloud.balance[acctA]
	got := e.ok(acctA, "euecs", "RunInstances", map[string]any{
		"InstanceType": "s2.large", "ZoneId": "cn-north-1-a", "ChargeType": "Prepaid", "Period": 2, "ClientToken": "pre-1"})
	if e.cloud.balance[acctA] != before-10000 {
		t.Fatalf("balance %d, want %d (2 months paid)", e.cloud.balance[acctA], before-10000)
	}
	inst := single(t, e.ok(acctA, "euecs", "DescribeInstances", map[string]any{"InstanceId": got["InstanceId"]}), "Instances")
	if inst["ChargeType"] != "Prepaid" || inst["ExpiredTime"] == "" {
		t.Fatalf("prepaid instance = %v", inst)
	}
	// A Prepaid spec change needs an upgrade order.
	e.fail(acctA, "euecs", "ModifyInstanceSpec", map[string]any{"InstanceId": got["InstanceId"], "InstanceType": "s2.small"},
		400, "InvalidInstanceType.NotFound") // s2.small is postpaid-only
	e.cloud.mu.Lock()
	e.cloud.skus = append(e.cloud.skus, catalogSKU{"euecs.s2.xlarge.prepaid", "euecs", chargePrepaid, "1"})
	e.cloud.mu.Unlock()
	e.fail(acctA, "euecs", "ModifyInstanceSpec", map[string]any{"InstanceId": got["InstanceId"], "InstanceType": "s2.xlarge"},
		409, "OperationDenied.UpgradeRequiresOrder")
}

func TestRunInstancesIdempotentRetry(t *testing.T) {
	e := newTestEnv(t)
	params := map[string]any{"InstanceType": "s2.large", "ZoneId": "cn-north-1-a", "ClientToken": "retry-token-1"}
	first := e.ok(acctA, "euecs", "RunInstances", params)
	second := e.ok(acctA, "euecs", "RunInstances", params)
	if first["InstanceId"] != second["InstanceId"] || first["OrderId"] != second["OrderId"] {
		t.Fatalf("retry with the same ClientToken bought another instance: %v vs %v", first, second)
	}
	if e.cloud.ordersMade != 1 || e.cloud.payments != 1 || len(e.cloud.resources) != 1 {
		t.Fatalf("orders %d payments %d resources %d, want 1/1/1", e.cloud.ordersMade, e.cloud.payments, len(e.cloud.resources))
	}

	// A retry after the first attempt timed out between pay and fulfil.
	e.cloud.down["svc-orchestrator"] = true
	e.fail(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a", "ClientToken": "retry-token-2"},
		503, "ServiceUnavailable")
	e.cloud.down["svc-orchestrator"] = false
	third := e.ok(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a", "ClientToken": "retry-token-2"})
	if e.cloud.ordersMade != 2 || len(e.cloud.resources) != 2 || third["InstanceId"] == first["InstanceId"] {
		t.Fatalf("orders %d resources %d", e.cloud.ordersMade, len(e.cloud.resources))
	}

	// The same token for a different purchase is refused, not answered with
	// the first purchase.
	e.fail(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.xlarge", "ZoneId": "cn-north-1-a", "ClientToken": "retry-token-1"},
		409, "IdempotentParameterMismatch")
	e.fail(acctA, "eubs", "CreateDisk", map[string]any{"Size": 20, "ZoneId": "cn-north-1-a", "ClientToken": "retry-token-1"},
		409, "IdempotentParameterMismatch")
	// Tokens are per account.
	other := e.ok(acctB, "euecs", "RunInstances", params)
	if other["InstanceId"] == first["InstanceId"] {
		t.Fatal("another account's ClientToken returned acctA's instance")
	}
}

func TestOwnershipIsolation(t *testing.T) {
	e := newTestEnv(t)
	ecs := str(t, e.ok(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a"}), "InstanceId")
	vpc := str(t, e.ok(acctA, "euvpc", "CreateVpc", map[string]any{"CidrBlock": "10.0.0.0/16"}), "VpcId")

	for _, action := range []string{"StartInstance", "StopInstance", "RebootInstance", "TerminateInstances", "ModifyInstanceAttribute"} {
		e.fail(acctB, "euecs", action, map[string]any{"InstanceId": ecs, "InstanceName": "x"}, 404, "InvalidInstanceId.NotFound")
	}
	e.fail(acctB, "euecs", "ModifyInstanceSpec", map[string]any{"InstanceId": ecs, "InstanceType": "s2.large"}, 404, "InvalidInstanceId.NotFound")
	e.fail(acctB, "euvpc", "DeleteVpc", map[string]any{"VpcId": vpc}, 404, "InvalidVpcId.NotFound")
	if got := list(t, e.ok(acctB, "euecs", "DescribeInstances", map[string]any{"InstanceId": ecs}), "Instances"); len(got) != 0 {
		t.Fatalf("acctB sees acctA's instance: %v", got)
	}
	// An id of another product of the same account is not this product's.
	e.fail(acctA, "euecs", "StopInstance", map[string]any{"InstanceId": vpc}, 404, "InvalidInstanceId.NotFound")
	e.fail(acctA, "eurds", "DeleteInstance", map[string]any{"InstanceId": ecs}, 404, "InvalidInstanceId.NotFound")
	// Ids that cannot name a resource never reach a downstream path.
	e.fail(acctA, "euecs", "StopInstance", map[string]any{"InstanceId": "../../orders/1/pay"}, 404, "InvalidInstanceId.NotFound")
	// Releasing several: one foreign id releases nothing.
	e.fail(acctB, "euecs", "TerminateInstances", map[string]any{"InstanceIds": []string{ecs}}, 404, "InvalidInstanceId.NotFound")
	if e.cloud.resources[ecs].State != "RUNNING" {
		t.Fatalf("instance state %s after a refused release", e.cloud.resources[ecs].State)
	}
}

func TestCreateErrorMapping(t *testing.T) {
	ecs := func(extra map[string]any) map[string]any {
		p := map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	cases := []struct {
		name    string
		acct    int64
		product string
		action  string
		params  map[string]any
		setup   func(f *fakeCloud)
		status  int
		code    string
	}{
		{"insufficient balance", acctPoor, "euecs", "RunInstances", ecs(map[string]any{"InstanceType": "s2.large", "ChargeType": "Prepaid"}), nil, 403, "InsufficientBalance"},
		{"quota exceeded", acctA, "euecs", "RunInstances", ecs(nil), func(f *fakeCloud) {
			f.fulfilErr = &fakeErr{409, "Quota.Exceeded", "配额不足,订单将退款: euecs instance 10/10"}
		}, 409, "QuotaExceeded"},
		{"provision failed", acctA, "euecs", "RunInstances", ecs(nil), func(f *fakeCloud) {
			f.fulfilErr = &fakeErr{409, "Provision.Failed", "开通失败,已进入退款"}
		}, 409, "ProvisionFailed"},
		{"provision compensation pending is retryable", acctA, "euecs", "RunInstances", ecs(nil), func(f *fakeCloud) {
			f.fulfilErr = &fakeErr{500, "Provision.Failed", "开通失败,资源回收中,请稍后重试"}
		}, 503, "ServiceUnavailable"},
		{"quota service down", acctA, "euecs", "RunInstances", ecs(nil), func(f *fakeCloud) {
			f.fulfilErr = &fakeErr{503, "Quota.Unavailable", "配额服务不可用,请稍后重试"}
		}, 503, "ServiceUnavailable"},
		{"order service down", acctA, "euecs", "RunInstances", ecs(nil), func(f *fakeCloud) { f.down["svc-order"] = true }, 503, "ServiceUnavailable"},
		{"catalog down", acctA, "euecs", "RunInstances", ecs(nil), func(f *fakeCloud) { f.down["svc-catalog"] = true }, 503, "ServiceUnavailable"},
		{"unknown instance type", acctA, "euecs", "RunInstances", ecs(map[string]any{"InstanceType": "s9.huge"}), nil, 400, "InvalidInstanceType.NotFound"},
		{"off-sale instance type", acctA, "euecs", "RunInstances", ecs(map[string]any{"InstanceType": "s2.legacy"}), nil, 409, "InvalidInstanceType.NotOnSale"},
		{"spec not sold prepaid", acctA, "euecs", "RunInstances", ecs(map[string]any{"ChargeType": "Prepaid"}), nil, 400, "InvalidParameter"},
		{"unknown SkuCode", acctA, "euecs", "RunInstances", map[string]any{"SkuCode": "euecs.zz.postpaid", "ZoneId": "cn-north-1-a"}, nil, 400, "InvalidSkuCode.NotFound"},
		{"SkuCode of another product", acctA, "euecs", "RunInstances", map[string]any{"SkuCode": "eubs.essd.postpaid", "ZoneId": "cn-north-1-a"}, nil, 400, "InvalidSkuCode.NotFound"},
		{"SkuCode with spec", acctA, "euecs", "RunInstances", ecs(map[string]any{"SkuCode": "euecs.s2.small.postpaid"}), nil, 400, "InvalidParameter"},
		{"no spec among several", acctA, "euecs", "RunInstances", map[string]any{"ZoneId": "cn-north-1-a"}, nil, 400, "MissingParameter"},
		{"bad charge type", acctA, "euecs", "RunInstances", ecs(map[string]any{"ChargeType": "Spot"}), nil, 400, "InvalidParameter"},
		{"bad period", acctA, "euecs", "RunInstances", ecs(map[string]any{"Period": 61}), nil, 400, "InvalidParameter"},
		{"period not a number", acctA, "euecs", "RunInstances", ecs(map[string]any{"Period": "one"}), nil, 400, "InvalidParameter"},
		{"zone outside region", acctA, "euecs", "RunInstances", ecs(map[string]any{"RegionId": "cn-east-1"}), nil, 400, "InvalidParameter"},
		{"unknown zone", acctA, "euecs", "RunInstances", ecs(map[string]any{"ZoneId": "cn-north-1-z"}), nil, 400, "InvalidZoneId.NotFound"},
		{"unknown region", acctA, "euvpc", "CreateVpc", map[string]any{"CidrBlock": "10.0.0.0/16", "RegionId": "cn-west-9"}, nil, 400, "InvalidRegionId.NotFound"},
		{"malformed region", acctA, "euvpc", "CreateVpc", map[string]any{"CidrBlock": "10.0.0.0/16", "RegionId": "north"}, nil, 400, "InvalidParameter"},
		{"oversized ClientToken", acctA, "euecs", "RunInstances", ecs(map[string]any{"ClientToken": strings.Repeat("t", 65)}), nil, 400, "InvalidParameter"},
		{"name too long", acctA, "euecs", "RunInstances", ecs(map[string]any{"InstanceName": strings.Repeat("n", 65)}), nil, 400, "InvalidParameter"},
		{"scalar given twice", acctA, "euecs", "RunInstances", ecs(map[string]any{"InstanceType": []string{"s2.small", "s2.large"}}), nil, 400, "InvalidParameter"},
		{"missing CidrBlock", acctA, "euvpc", "CreateVpc", map[string]any{"VpcName": "v"}, nil, 400, "MissingParameter"},
		{"invalid engine version", acctA, "eurds", "CreateInstance", map[string]any{"EngineVersion": "9.9"}, nil, 400, "InvalidParameter"},
		{"missing disk size", acctA, "eubs", "CreateDisk", map[string]any{"ZoneId": "cn-north-1-a"}, nil, 400, "MissingParameter"},
		{"bad bucket name", acctA, "euoss", "CreateBucket", map[string]any{"Bucket": "Bad_Name"}, nil, 400, "InvalidBucketName"},
		{"storage class not on sale", acctA, "euoss", "CreateBucket", map[string]any{"Bucket": "cold-1", "StorageClass": "Archive"}, nil, 400, "InvalidParameter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			if tc.setup != nil {
				tc.setup(e.cloud)
			}
			r := e.fail(tc.acct, tc.product, tc.action, tc.params, tc.status, tc.code)
			if r.message == "" || str(t, r.env, "RequestId") == "" {
				t.Fatalf("error envelope lacks Message/RequestId: %v", r.env)
			}
		})
	}
}

func TestProvisioningReturnsCreating(t *testing.T) {
	e := newTestEnv(t)
	e.cloud.provisionAsync = true
	got := e.ok(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a", "ClientToken": "async-1"})
	if got["Status"] != "Creating" || str(t, got, "InstanceId") == "" {
		t.Fatalf("RunInstances while provisioning = %v", got)
	}
	inst := single(t, e.ok(acctA, "euecs", "DescribeInstances", map[string]any{"InstanceId": got["InstanceId"]}), "Instances")
	if inst["Status"] != "Creating" {
		t.Fatalf("Describe = %v", inst)
	}
	// Still provisioning: the orchestrator refuses the release.
	e.fail(acctA, "euecs", "TerminateInstances", map[string]any{"InstanceId": got["InstanceId"]}, 409, "IncorrectInstanceStatus")
	// The retry returns the same, still provisioning, instance.
	again := e.ok(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a", "ClientToken": "async-1"})
	if again["InstanceId"] != got["InstanceId"] || again["Status"] != "Creating" {
		t.Fatalf("retry = %v", again)
	}
}

// TestTerraformShapes runs the exact parameter sets sdk/terraform sends.
func TestTerraformShapes(t *testing.T) {
	e := newTestEnv(t)

	ecs := e.ok(acctA, "euecs", "RunInstances", map[string]any{"ImageId": "img-001", "InstanceType": "s2.large", "ZoneId": "cn-north-1-a"})
	// The provider's Read looks the resource up in Instances[] by InstanceId.
	read := single(t, e.ok(acctA, "euecs", "DescribeInstances", map[string]any{"InstanceId": ecs["InstanceId"]}), "Instances")
	if read["InstanceId"] != ecs["InstanceId"] || read["InstanceType"] != "s2.large" {
		t.Fatalf("Terraform reads Instances[InstanceId]: %v", read)
	}
	e.ok(acctA, "euecs", "TerminateInstances", map[string]any{"InstanceId": ecs["InstanceId"]})

	// eurds: only EngineVersion — the sole Postpaid class and the region's
	// first zone are chosen.
	rds := e.ok(acctA, "eurds", "CreateInstance", map[string]any{"EngineVersion": "8.0"})
	id := str(t, rds, "InstanceId")
	inst := single(t, e.ok(acctA, "eurds", "DescribeInstances", map[string]any{"InstanceId": id}), "Instances")
	if inst["InstanceClass"] != "mysql8.small" || inst["EngineVersion"] != "8.0" || inst["ZoneId"] != "cn-north-1-a" ||
		inst["RegionId"] != "cn-north-1" || inst["Engine"] != "MySQL" {
		t.Fatalf("eurds instance = %v", inst)
	}
	for _, a := range []struct{ action, status string }{{"StopInstance", "Stopped"}, {"StartInstance", "Running"}, {"RestartInstance", "Running"}} {
		if got := e.ok(acctA, "eurds", a.action, map[string]any{"InstanceId": id}); got["Status"] != a.status {
			t.Fatalf("%s = %v", a.action, got)
		}
	}
	e.ok(acctA, "eurds", "DeleteInstance", map[string]any{"InstanceId": id})

	vpc := e.ok(acctA, "euvpc", "CreateVpc", map[string]any{"CidrBlock": "172.16.0.0/16"})
	e.ok(acctA, "euvpc", "DeleteVpc", map[string]any{"VpcId": str(t, vpc, "VpcId")})

	b := e.ok(acctA, "euoss", "CreateBucket", map[string]any{"Bucket": "tf-bucket-1"})
	if b["BucketName"] != "tf-bucket-1" || b["Bucket"] != "tf-bucket-1" {
		t.Fatalf("CreateBucket = %v", b)
	}
	e.ok(acctA, "euoss", "DeleteBucket", map[string]any{"Bucket": "tf-bucket-1"})
}

func TestVPC(t *testing.T) {
	e := newTestEnv(t)
	created := e.ok(acctA, "euvpc", "CreateVpc", map[string]any{"CidrBlock": "10.1.0.0/16", "VpcName": "prod", "ClientToken": "vpc-1"})
	id := str(t, created, "VpcId")
	if created["Status"] != "Available" {
		t.Fatalf("CreateVpc = %v", created)
	}
	vpcs := list(t, e.ok(acctA, "euvpc", "DescribeVpcs", nil), "Vpcs")
	if len(vpcs) != 1 || vpcs[0]["VpcId"] != id || vpcs[0]["VpcName"] != "prod" || vpcs[0]["CidrBlock"] != "10.1.0.0/16" ||
		vpcs[0]["RegionId"] != "cn-north-1" || vpcs[0]["Status"] != "Available" || vpcs[0]["CreationTime"] == "" {
		t.Fatalf("DescribeVpcs = %v", vpcs)
	}
	if _, zonal := vpcs[0]["ZoneId"]; zonal {
		t.Fatalf("a VPC is regional: %v", vpcs[0])
	}
	e.fail(acctA, "euvpc", "CreateVpc", map[string]any{"CidrBlock": "10.1.0.0/16"}, 409, "InvalidCidrBlock")
	e.ok(acctA, "euvpc", "ModifyVpcAttribute", map[string]any{"VpcId": id, "VpcName": "prod-2"})
	if got := list(t, e.ok(acctA, "euvpc", "DescribeVpcs", map[string]any{"VpcIds": []string{id}}), "Vpcs"); got[0]["VpcName"] != "prod-2" {
		t.Fatalf("renamed VPC = %v", got)
	}
	e.fail(acctA, "euvpc", "ModifyVpcAttribute", map[string]any{"VpcId": id}, 400, "MissingParameter")
	e.ok(acctA, "euvpc", "DeleteVpc", map[string]any{"VpcId": id})
	if got := list(t, e.ok(acctA, "euvpc", "DescribeVpcs", nil), "Vpcs"); len(got) != 0 {
		t.Fatalf("deleted VPC listed: %v", got)
	}
}

func TestDiskAndEIP(t *testing.T) {
	e := newTestEnv(t)
	disk := e.ok(acctA, "eubs", "CreateDisk", map[string]any{"Size": 40, "ZoneId": "cn-north-1-b", "DiskCategory": "essd", "DiskName": "data"})
	id := str(t, disk, "DiskId")
	disks := list(t, e.ok(acctA, "eubs", "DescribeDisks", map[string]any{"DiskId": id}), "Disks")
	if len(disks) != 1 || disks[0]["Size"] != float64(40) || disks[0]["DiskCategory"] != "essd" || disks[0]["ZoneId"] != "cn-north-1-b" ||
		disks[0]["DiskName"] != "data" || disks[0]["Status"] != "Available" {
		t.Fatalf("DescribeDisks = %v", disks)
	}
	e.ok(acctA, "eubs", "DeleteDisk", map[string]any{"DiskId": id})

	before := e.cloud.balance[acctA]
	eip := e.ok(acctA, "eueip", "AllocateEipAddress", map[string]any{"Bandwidth": 5, "ChargeType": "Prepaid", "ZoneId": "cn-north-1-a"})
	alloc := str(t, eip, "AllocationId")
	if e.cloud.balance[acctA] != before-5000 {
		t.Fatalf("prepaid EIP not paid: balance %d", e.cloud.balance[acctA])
	}
	eips := list(t, e.ok(acctA, "eueip", "DescribeEipAddresses", nil), "EipAddresses")
	if len(eips) != 1 || eips[0]["AllocationId"] != alloc || eips[0]["Bandwidth"] != float64(5) ||
		eips[0]["InternetChargeType"] != "PayByBandwidth" || eips[0]["ChargeType"] != "Prepaid" {
		t.Fatalf("DescribeEipAddresses = %v", eips)
	}
	postpaid := e.ok(acctA, "eueip", "AllocateEipAddress", map[string]any{})
	got := list(t, e.ok(acctA, "eueip", "DescribeEipAddresses", map[string]any{"AllocationId": postpaid["AllocationId"]}), "EipAddresses")
	if got[0]["InternetChargeType"] != "PayByTraffic" || got[0]["ZoneId"] != "cn-north-1-a" {
		t.Fatalf("postpaid EIP = %v", got)
	}
	e.ok(acctA, "eueip", "ReleaseEipAddress", map[string]any{"AllocationId": alloc})
	e.fail(acctA, "eueip", "ReleaseEipAddress", map[string]any{"AllocationId": alloc}, 404, "InvalidAllocationId.NotFound")
}

func TestOSSBuckets(t *testing.T) {
	e := newTestEnv(t)
	e.ok(acctA, "euoss", "CreateBucket", map[string]any{"Bucket": "logs-2026"})
	e.ok(acctA, "euoss", "CreateBucket", map[string]any{"BucketName": "assets-2026", "RegionId": "cn-east-1", "StorageClass": "Standard"})
	buckets := list(t, e.ok(acctA, "euoss", "ListBuckets", nil), "Buckets")
	if len(buckets) != 2 || buckets[0]["Bucket"] != "assets-2026" || buckets[0]["RegionId"] != "cn-east-1" ||
		buckets[0]["StorageClass"] != "Standard" || buckets[1]["Bucket"] != "logs-2026" {
		t.Fatalf("ListBuckets = %v", buckets)
	}
	if got := list(t, e.ok(acctA, "euoss", "ListBuckets", map[string]any{"Prefix": "logs"}), "Buckets"); len(got) != 1 {
		t.Fatalf("ListBuckets(Prefix) = %v", got)
	}
	e.fail(acctB, "euoss", "CreateBucket", map[string]any{"Bucket": "logs-2026"}, 409, "BucketAlreadyExists")
	e.fail(acctB, "euoss", "DeleteBucket", map[string]any{"Bucket": "logs-2026"}, 404, "InvalidBucketName.NotFound")
	e.ok(acctA, "euoss", "DeleteBucket", map[string]any{"Bucket": "logs-2026"})
	e.fail(acctA, "euoss", "DeleteBucket", map[string]any{"Bucket": "logs-2026"}, 404, "InvalidBucketName.NotFound")
	// The name is free again.
	e.ok(acctB, "euoss", "CreateBucket", map[string]any{"Bucket": "logs-2026"})
}

func TestMonitor(t *testing.T) {
	e := newTestEnv(t)
	created := e.ok(acctA, "eumon", "CreateAlarmRule", map[string]any{
		"ProductCode": "euecs", "ResourceType": "instance", "MetricName": "cpu_utilization",
		"Threshold": "80.5", "ComparisonOperator": "GreaterThanThreshold", "Period": 300, "EvaluationCount": 3,
		"NotificationChannels": []string{"IN_APP", "EMAIL"},
	})
	id := str(t, created, "RuleId")
	if created["RuleVersion"] != float64(1) {
		t.Fatalf("CreateAlarmRule = %v", created)
	}
	rules := list(t, e.ok(acctA, "eumon", "DescribeAlarmRules", nil), "AlarmRules")
	if len(rules) != 1 || rules[0]["RuleId"] != id || rules[0]["Threshold"] != "80.5" || rules[0]["ComparisonOperator"] != "GreaterThanThreshold" ||
		rules[0]["Period"] != float64(300) || rules[0]["EvaluationCount"] != float64(3) || rules[0]["Status"] != "Enabled" {
		t.Fatalf("DescribeAlarmRules = %v", rules)
	}
	mod := e.ok(acctA, "eumon", "ModifyAlarmRule", map[string]any{"RuleId": id, "Threshold": "90", "Status": "Disabled"})
	if mod["RuleVersion"] != float64(2) {
		t.Fatalf("ModifyAlarmRule = %v", mod)
	}
	e.fail(acctA, "eumon", "ModifyAlarmRule", map[string]any{"RuleId": id, "Threshold": "91", "RuleVersion": 1}, 409, "ConcurrentModification")
	e.fail(acctA, "eumon", "ModifyAlarmRule", map[string]any{"RuleId": id, "Threshold": "1.23456"}, 400, "InvalidParameter")
	e.fail(acctA, "eumon", "ModifyAlarmRule", map[string]any{"RuleId": id}, 400, "MissingParameter")
	e.fail(acctA, "eumon", "CreateAlarmRule", map[string]any{"ProductCode": "euecs", "MetricName": "cpu", "Threshold": "1.23456"}, 400, "InvalidParameter")
	e.fail(acctA, "eumon", "CreateAlarmRule", map[string]any{"ProductCode": "euecs", "MetricName": "cpu"}, 400, "MissingParameter")
	e.fail(acctA, "eumon", "CreateAlarmRule", map[string]any{"ProductCode": "euecs", "MetricName": "cpu", "Threshold": "1", "ComparisonOperator": "~"}, 400, "InvalidParameter")
	e.fail(acctB, "eumon", "ModifyAlarmRule", map[string]any{"RuleId": id, "Threshold": "1"}, 404, "InvalidRuleId.NotFound")
	e.fail(acctB, "eumon", "DeleteAlarmRule", map[string]any{"RuleId": id}, 404, "InvalidRuleId.NotFound")
	if len(e.cloud.rules) != 1 {
		t.Fatal("acctB deleted acctA's rule")
	}
	e.ok(acctA, "eumon", "DeleteAlarmRule", map[string]any{"RuleId": id})
	e.fail(acctA, "eumon", "DeleteAlarmRule", map[string]any{"RuleId": id}, 404, "InvalidRuleId.NotFound")

}

// DescribeMetricData maps onto svc-monitor's metrics contract:
// GET /api/v1/monitor/metrics?metric=&resourceId=&start=&end=&period= →
// {metric, resourceId, start, end, period, source, points:[{timestamp, value}]}.
func TestDescribeMetricData(t *testing.T) {
	e := newTestEnv(t)
	inst := str(t, e.ok(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a"}), "InstanceId")
	lastQuery := func() url.Values {
		t.Helper()
		e.cloud.mu.Lock()
		defer e.cloud.mu.Unlock()
		if len(e.cloud.metricsQueries) == 0 {
			t.Fatal("svc-monitor was not asked")
		}
		return e.cloud.metricsQueries[len(e.cloud.metricsQueries)-1]
	}
	queries := func() int {
		e.cloud.mu.Lock()
		defer e.cloud.mu.Unlock()
		return len(e.cloud.metricsQueries)
	}

	data := e.ok(acctA, "eumon", "DescribeMetricData", map[string]any{
		"MetricName": "cpu_utilization", "InstanceId": inst,
		"StartTime": "1791536400000", "EndTime": "2026-10-09T10:00:00Z", "Period": 300,
	})
	points := list(t, data, "Datapoints")
	if data["MetricName"] != "cpu_utilization" || data["ResourceId"] != inst || data["Period"] != float64(300) ||
		data["Source"] != "synthetic" || len(points) != 2 || points[1]["Value"] != 27.5 ||
		points[1]["Timestamp"] != "2026-10-09T10:01:00Z" || len(data) != 5 {
		t.Fatalf("DescribeMetricData = %v", data)
	}
	if q := lastQuery(); q.Get("metric") != "cpu_utilization" || q.Get("resourceId") != inst ||
		q.Get("start") != "1791536400000" || q.Get("end") != "2026-10-09T10:00:00Z" || q.Get("period") != "300" {
		t.Fatalf("svc-monitor query = %v", q)
	}
	// ResourceId is the canonical name; svc-monitor's defaults apply.
	data = e.ok(acctA, "eumon", "DescribeMetricData", map[string]any{"MetricName": "memory_utilization", "ResourceId": inst})
	if data["MetricName"] != "memory_utilization" || data["Period"] != float64(60) {
		t.Fatalf("DescribeMetricData(defaults) = %v", data)
	}
	if q := lastQuery(); q.Has("start") || q.Has("end") || q.Has("period") {
		t.Fatalf("unsupplied parameters reached svc-monitor: %v", q)
	}

	// Required parameters and local bounds never reach svc-monitor.
	before := queries()
	r := e.fail(acctA, "eumon", "DescribeMetricData", map[string]any{"MetricName": "cpu_utilization"}, 400, "MissingParameter")
	if !strings.Contains(r.message, "ResourceId") {
		t.Fatalf("MissingParameter message %q does not name ResourceId", r.message)
	}
	e.fail(acctA, "eumon", "DescribeMetricData", map[string]any{"InstanceId": inst}, 400, "MissingParameter")
	e.fail(acctA, "eumon", "DescribeMetricData", map[string]any{"MetricName": "cpu_utilization", "InstanceId": inst, "Period": 5}, 400, "InvalidParameter")
	e.fail(acctA, "eumon", "DescribeMetricData", map[string]any{"MetricName": "cpu_utilization", "InstanceId": inst, "Period": "1m"}, 400, "InvalidParameter")
	// Another account's resource, an unknown one and a released one read alike.
	e.fail(acctB, "eumon", "DescribeMetricData", map[string]any{"MetricName": "cpu_utilization", "InstanceId": inst}, 404, "InvalidResourceId.NotFound")
	e.fail(acctA, "eumon", "DescribeMetricData", map[string]any{"MetricName": "cpu_utilization", "ResourceId": "euecs-cn-north-1-01-ffff"}, 404, "InvalidResourceId.NotFound")
	if queries() != before {
		t.Fatal("a refused request reached svc-monitor")
	}

	// svc-monitor's verdicts in the API vocabulary.
	r = e.fail(acctA, "eumon", "DescribeMetricData", map[string]any{
		"MetricName": "cpu_utilization", "InstanceId": inst, "StartTime": "2026-10-09T11:00:00Z", "EndTime": "2026-10-09T10:00:00Z",
	}, 400, "InvalidParameter")
	if r.message != "StartTime must be before EndTime" {
		t.Fatalf("InvalidParameter message = %q", r.message)
	}
	r = e.fail(acctA, "eumon", "DescribeMetricData", map[string]any{"MetricName": "disk_iops", "InstanceId": inst}, 400, "InvalidParameter")
	if !strings.HasPrefix(r.message, "MetricName must be one of cpu_utilization") {
		t.Fatalf("InvalidParameter message = %q", r.message)
	}
	for _, c := range []struct {
		err    fakeErr
		status int
		code   string
	}{
		{fakeErr{404, "Monitor.ResourceNotFound", "资源不存在"}, 404, "InvalidResourceId.NotFound"},
		{fakeErr{503, "Monitor.MetricsUnavailable", "监控数据暂不可用,请稍后重试"}, 503, "ServiceUnavailable"},
		{fakeErr{503, "Monitor.OwnerLookupFailed", "资源归属暂无法校验,请稍后重试"}, 503, "ServiceUnavailable"},
	} {
		e.cloud.mu.Lock()
		e.cloud.metricsErr = &fakeErr{c.err.status, c.err.code, c.err.msg}
		e.cloud.mu.Unlock()
		e.fail(acctA, "eumon", "DescribeMetricData", map[string]any{"MetricName": "cpu_utilization", "InstanceId": inst}, c.status, c.code)
	}

	e.cloud.mu.Lock()
	e.cloud.resources[inst].State = stateReleased
	e.cloud.mu.Unlock()
	before = queries()
	e.fail(acctA, "eumon", "DescribeMetricData", map[string]any{"MetricName": "cpu_utilization", "InstanceId": inst}, 404, "InvalidResourceId.NotFound")
	if queries() != before {
		t.Fatal("a released resource's metrics were asked for")
	}
}

// A metrics answer for another series than the one asked for breaks
// svc-monitor's contract: an unavailability, never data for the caller.
func TestDescribeMetricDataContractBreak(t *testing.T) {
	e := newTestEnv(t)
	inst := str(t, e.ok(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a"}), "InstanceId")
	other := str(t, e.ok(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a"}), "InstanceId")
	mon := http.NewServeMux()
	mon.HandleFunc("GET /api/v1/monitor/metrics", func(w http.ResponseWriter, r *http.Request) {
		fakeWriteOK(w, http.StatusOK, map[string]any{"metric": "cpu_utilization", "resourceId": other, "period": 60,
			"source": "prometheus", "points": []map[string]any{}})
	})
	srv := e.cloud.serve("svc-monitor-misrouted", mon)
	t.Cleanup(srv.Close)
	cfg := config{CatalogURL: e.cloud.catalogSrv.URL, OrderURL: e.cloud.orderSrv.URL, OrchestratorURL: e.cloud.orchSrv.URL,
		MonitorURL: srv.URL, DefaultRegion: "cn-north-1"}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	e.h = newHandler(newServer(cfg))
	e.fail(acctA, "eumon", "DescribeMetricData", map[string]any{"MetricName": "cpu_utilization", "InstanceId": inst}, 503, "ServiceUnavailable")
	data := e.ok(acctA, "eumon", "DescribeMetricData", map[string]any{"MetricName": "cpu_utilization", "InstanceId": other})
	if data["Source"] != "prometheus" || len(list(t, data, "Datapoints")) != 0 {
		t.Fatalf("DescribeMetricData = %v", data)
	}
}

func TestDescribePagination(t *testing.T) {
	e := newTestEnv(t)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, str(t, e.ok(acctA, "euecs", "RunInstances", map[string]any{"InstanceType": "s2.small", "ZoneId": "cn-north-1-a"}), "InstanceId"))
	}
	// Another product of the same account is not listed.
	e.ok(acctA, "euvpc", "CreateVpc", map[string]any{"CidrBlock": "10.9.0.0/16"})

	p1 := e.ok(acctA, "euecs", "DescribeInstances", map[string]any{"PageSize": 2})
	p2 := e.ok(acctA, "euecs", "DescribeInstances", map[string]any{"PageSize": 2, "PageNumber": 2})
	got1, got2 := list(t, p1, "Instances"), list(t, p2, "Instances")
	if p1["TotalCount"] != float64(3) || len(got1) != 2 || len(got2) != 1 {
		t.Fatalf("pages = %v / %v", p1, p2)
	}
	// Newest first.
	if got1[0]["InstanceId"] != ids[2] || got2[0]["InstanceId"] != ids[0] {
		t.Fatalf("order = %v, %v", got1, got2)
	}
	if got := list(t, e.ok(acctA, "euecs", "DescribeInstances", map[string]any{"PageNumber": 9}), "Instances"); len(got) != 0 {
		t.Fatalf("page past the end = %v", got)
	}
	sub := e.ok(acctA, "euecs", "DescribeInstances", map[string]any{"InstanceIds": []string{ids[0], ids[1]}})
	if sub["TotalCount"] != float64(2) {
		t.Fatalf("filter by ids = %v", sub)
	}
	e.fail(acctA, "euecs", "DescribeInstances", map[string]any{"PageSize": 101}, 400, "InvalidParameter")
	e.fail(acctA, "euecs", "DescribeInstances", map[string]any{"PageNumber": 0}, 400, "InvalidParameter")
}
