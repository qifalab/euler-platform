package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/qifalab/euler-platform/eusdk"
)

func TestMain(m *testing.M) {
	pollInterval = time.Millisecond
	retryBackoff = time.Millisecond
	os.Exit(m.Run())
}

var allResources = []func() resource.Resource{
	NewScecsResource, NewScrdsResource, NewScvpcResource, NewScbsResource, NewSceipResource, NewScossResource,
}

func str(s string) types.String { return types.StringValue(s) }

// --- plan/state helpers (what Terraform hands the provider) -------------------

func testSchema(t *testing.T, r *euResource) schema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("%s schema: %v", r.spec.typeName, resp.Diagnostics)
	}
	return resp.Schema
}

func objectOf(t *testing.T, r *euResource, v values) types.Object {
	t.Helper()
	obj, diags := r.object(v)
	if diags.HasError() {
		t.Fatalf("object: %v", diags)
	}
	return obj
}

func nullOf(a *attrSpec, unknown bool) attr.Value {
	switch {
	case a.kind == kindInt64 && unknown:
		return types.Int64Unknown()
	case a.kind == kindInt64:
		return types.Int64Null()
	case unknown:
		return types.StringUnknown()
	}
	return types.StringNull()
}

// planFor builds the plan Terraform proposes for a configuration: configured
// values as given; an unconfigured computed attribute unknown, or (on an
// update) its prior value when UseStateForUnknown keeps it.
func planFor(t *testing.T, r *euResource, cfg map[string]attr.Value, prior values) tfsdk.Plan {
	t.Helper()
	v := values{}
	for i := range r.spec.attrs {
		a := &r.spec.attrs[i]
		switch c, ok := cfg[a.name]; {
		case ok:
			v[a.name] = c
		case !a.computed:
			v[a.name] = nullOf(a, false)
		case prior != nil && a.stable && !prior[a.name].IsNull():
			v[a.name] = prior[a.name]
		default:
			v[a.name] = nullOf(a, true)
		}
	}
	p := tfsdk.Plan{Schema: testSchema(t, r)}
	if diags := p.Set(context.Background(), objectOf(t, r, v)); diags.HasError() {
		t.Fatalf("plan: %v", diags)
	}
	return p
}

func emptyState(t *testing.T, r *euResource) tfsdk.State {
	s := testSchema(t, r)
	return tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}
}

func stateValues(t *testing.T, r *euResource, st tfsdk.State) values {
	t.Helper()
	if st.Raw.IsNull() {
		return nil
	}
	var obj types.Object
	if diags := st.Get(context.Background(), &obj); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	v, diags := r.decode(obj, "state")
	if diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	return v
}

func doCreate(t *testing.T, r *euResource, cfg map[string]attr.Value) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	resp := resource.CreateResponse{State: emptyState(t, r)}
	r.Create(context.Background(), resource.CreateRequest{Plan: planFor(t, r, cfg, nil)}, &resp)
	return resp.State, resp.Diagnostics
}

func doRead(t *testing.T, r *euResource, st tfsdk.State) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	resp := resource.ReadResponse{State: tfsdk.State{Schema: st.Schema, Raw: st.Raw.Copy()}}
	r.Read(context.Background(), resource.ReadRequest{State: st}, &resp)
	return resp.State, resp.Diagnostics
}

func doUpdate(t *testing.T, r *euResource, st tfsdk.State, cfg map[string]attr.Value) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	resp := resource.UpdateResponse{State: tfsdk.State{Schema: st.Schema, Raw: st.Raw.Copy()}}
	r.Update(context.Background(), resource.UpdateRequest{Plan: planFor(t, r, cfg, stateValues(t, r, st)), State: st}, &resp)
	return resp.State, resp.Diagnostics
}

func doDelete(t *testing.T, r *euResource, st tfsdk.State) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	resp := resource.DeleteResponse{State: tfsdk.State{Schema: st.Schema, Raw: st.Raw.Copy()}}
	r.Delete(context.Background(), resource.DeleteRequest{State: st}, &resp)
	return resp.State, resp.Diagnostics
}

func mustOK(t *testing.T, what string, diags diag.Diagnostics) {
	t.Helper()
	if diags.HasError() {
		t.Fatalf("%s: %v", what, diags)
	}
}

// --- configuration ------------------------------------------------------------

func TestResourcesTakeClientFromConfigure(t *testing.T) {
	ctx := context.Background()
	client := newAPIClient(eusdk.Config{AK: "EUAK", SK: "sk", Region: testRegion})
	for _, newRes := range allResources {
		res := newRes().(*euResource)
		var early resource.ConfigureResponse
		res.Configure(ctx, resource.ConfigureRequest{}, &early)
		if early.Diagnostics.HasError() || res.client != nil {
			t.Fatalf("%s: unconfigured provider: %v", res.spec.typeName, early.Diagnostics)
		}
		var resp resource.ConfigureResponse
		res.Configure(ctx, resource.ConfigureRequest{ProviderData: client}, &resp)
		if resp.Diagnostics.HasError() || res.client != client {
			t.Fatalf("%s: client not taken from Configure: %v", res.spec.typeName, resp.Diagnostics)
		}
		var bad resource.ConfigureResponse
		res.Configure(ctx, resource.ConfigureRequest{ProviderData: eusdk.New(eusdk.Config{})}, &bad)
		if !bad.Diagnostics.HasError() {
			t.Fatalf("%s: wrong provider data type accepted silently", res.spec.typeName)
		}
	}
}

func TestCallWithoutClientIsAnError(t *testing.T) {
	var c *apiClient
	if _, err := c.call(context.Background(), "euecs", "RunInstances", map[string]any{}); !errors.Is(err, errNotConfigured) {
		t.Fatalf("call without a client = %v, want errNotConfigured", err)
	}
}

// Attributes that cannot change in place force replacement; ids and other
// stable computed values keep their state across plans; every resource type
// is distinct and every resource declares the shared lifecycle attributes.
func TestSchemaPlanModifiers(t *testing.T) {
	ctx := context.Background()
	forceNew := map[string][]string{
		"euler_euecs_instance": {"image_id", "zone_id", "region", "charge_type", "period"},
		"euler_eurds_instance": {"engine_version", "zone_id", "region", "charge_type", "period"},
		"euler_euvpc_vpc":      {"cidr_block", "region"},
		"euler_eubs_disk":      {"size", "disk_category", "zone_id", "region", "charge_type", "period"},
		"euler_eueip_address":  {"bandwidth", "name", "zone_id", "region", "charge_type", "period"},
		"euler_euoss_bucket":   {"bucket", "storage_class", "region"},
	}
	inPlace := map[string][]string{
		"euler_euecs_instance": {"instance_type", "instance_name"},
		"euler_eurds_instance": {"instance_class", "instance_name"},
		"euler_euvpc_vpc":      {"vpc_name"},
		"euler_eubs_disk":      {"disk_name"},
	}
	type described interface {
		Description(context.Context) string
	}
	descs := func(a schema.Attribute) string {
		var out []string
		switch t := a.(type) {
		case schema.StringAttribute:
			for _, m := range t.PlanModifiers {
				out = append(out, m.(described).Description(ctx))
			}
		case schema.Int64Attribute:
			for _, m := range t.PlanModifiers {
				out = append(out, m.(described).Description(ctx))
			}
		}
		return strings.Join(out, " | ")
	}
	seen := map[string]bool{}
	for _, newRes := range allResources {
		r := newRes().(*euResource)
		s := testSchema(t, r)
		if seen[r.spec.typeName] {
			t.Fatalf("duplicate resource type %s", r.spec.typeName)
		}
		seen[r.spec.typeName] = true
		for _, name := range append(forceNew[r.spec.typeName], inPlace[r.spec.typeName]...) {
			if _, ok := s.Attributes[name]; !ok {
				t.Fatalf("%s has no attribute %s", r.spec.typeName, name)
			}
		}
		for name, a := range s.Attributes {
			d := descs(a)
			replaces := strings.Contains(d, "destroy and recreate")
			want := false
			for _, f := range forceNew[r.spec.typeName] {
				want = want || f == name
			}
			if replaces != want {
				t.Errorf("%s.%s: RequiresReplace = %v, want %v (%s)", r.spec.typeName, name, replaces, want, d)
			}
			for _, f := range inPlace[r.spec.typeName] {
				if f == name && r.spec.attr(name).modify == "" {
					t.Errorf("%s.%s has no modify Action", r.spec.typeName, name)
				}
			}
		}
		for _, name := range []string{"id", "client_token", "order_id", "creation_time"} {
			if !strings.Contains(descs(s.Attributes[name]), "will not change") {
				t.Errorf("%s.%s lacks UseStateForUnknown", r.spec.typeName, name)
			}
		}
		if _, ok := s.Attributes["status"]; !ok {
			t.Errorf("%s has no status", r.spec.typeName)
		}
	}
}

// --- lifecycle against the fake facade ----------------------------------------

// Every resource runs Create → Read → (Update) → Delete → Read against the
// facade's Actions and envelope.
func TestLifecycleAgainstFacade(t *testing.T) {
	cases := []struct {
		newRes           func() resource.Resource
		cfg              map[string]attr.Value
		wantParams       map[string]string // create parameters
		readBack         map[string]string // state after create
		update           map[string]attr.Value
		modifyAction     string
		modifyParams     map[string]string
		updatedAttribute [2]string
	}{
		{
			newRes:     NewScecsResource,
			cfg:        map[string]attr.Value{"instance_type": str("s2.large"), "image_id": str("img-001"), "instance_name": str("web")},
			wantParams: map[string]string{"InstanceType": "s2.large", "ImageId": "img-001", "InstanceName": "web", "RegionId": testRegion},
			readBack: map[string]string{"status": "Running", "zone_id": "cn-north-1-a", "region": testRegion, "charge_type": "Postpaid",
				"sku_code": "euecs.s2.large.postpaid", "creation_time": "2026-10-09T10:00:00Z", "instance_name": "web"},
			update:           map[string]attr.Value{"instance_type": str("s2.xlarge"), "image_id": str("img-001"), "instance_name": str("web")},
			modifyAction:     "ModifyInstanceSpec",
			modifyParams:     map[string]string{"InstanceType": "s2.xlarge"},
			updatedAttribute: [2]string{"sku_code", "euecs.s2.xlarge.postpaid"},
		},
		{
			newRes:     NewScrdsResource,
			cfg:        map[string]attr.Value{"engine_version": str("8.0"), "zone_id": str("cn-north-1-b")},
			wantParams: map[string]string{"EngineVersion": "8.0", "ZoneId": "cn-north-1-b"},
			readBack: map[string]string{"status": "Running", "engine": "MySQL", "instance_class": "mysql8.small", "zone_id": "cn-north-1-b",
				"sku_code": "eurds.mysql8.small.postpaid"},
			update:           map[string]attr.Value{"engine_version": str("8.0"), "zone_id": str("cn-north-1-b"), "instance_name": str("orders")},
			modifyAction:     "ModifyInstanceAttribute",
			modifyParams:     map[string]string{"InstanceName": "orders"},
			updatedAttribute: [2]string{"instance_name", "orders"},
		},
		{
			newRes:           NewScvpcResource,
			cfg:              map[string]attr.Value{"cidr_block": str("10.0.0.0/16"), "vpc_name": str("core")},
			wantParams:       map[string]string{"CidrBlock": "10.0.0.0/16", "VpcName": "core", "RegionId": testRegion},
			readBack:         map[string]string{"status": "Available", "vpc_name": "core", "region": testRegion},
			update:           map[string]attr.Value{"cidr_block": str("10.0.0.0/16"), "vpc_name": str("edge")},
			modifyAction:     "ModifyVpcAttribute",
			modifyParams:     map[string]string{"VpcName": "edge"},
			updatedAttribute: [2]string{"vpc_name", "edge"},
		},
		{
			newRes:           NewScbsResource,
			cfg:              map[string]attr.Value{"size": types.Int64Value(40), "charge_type": str("PrePaid"), "period": types.Int64Value(3)},
			wantParams:       map[string]string{"Size": "40", "ChargeType": "PrePaid", "Period": "3"},
			readBack:         map[string]string{"status": "Available", "disk_category": "essd", "charge_type": "PrePaid", "sku_code": "eubs.essd.prepaid"},
			update:           map[string]attr.Value{"size": types.Int64Value(40), "charge_type": str("PrePaid"), "period": types.Int64Value(3), "disk_name": str("data")},
			modifyAction:     "ModifyDiskAttribute",
			modifyParams:     map[string]string{"DiskName": "data"},
			updatedAttribute: [2]string{"disk_name", "data"},
		},
		{
			newRes:     NewSceipResource,
			cfg:        map[string]attr.Value{"name": str("egress")},
			wantParams: map[string]string{"Name": "egress"},
			readBack:   map[string]string{"status": "Available", "internet_charge_type": "PayByTraffic", "name": "egress"},
		},
		{
			newRes:     NewScossResource,
			cfg:        map[string]attr.Value{"bucket": str("logs-2026")},
			wantParams: map[string]string{"Bucket": "logs-2026"},
			readBack:   map[string]string{"id": "logs-2026", "status": "Available", "storage_class": "Standard", "region": testRegion},
		},
	}
	for _, tc := range cases {
		r := tc.newRes().(*euResource)
		t.Run(r.spec.typeName, func(t *testing.T) {
			f, client := newFakeFacade(t)
			r.client = client
			st, diags := doCreate(t, r, tc.cfg)
			mustOK(t, "create", diags)

			creates := f.callsOf(r.spec.create)
			if len(creates) != 1 {
				t.Fatalf("%d %s calls, want 1", len(creates), r.spec.create)
			}
			for k, want := range tc.wantParams {
				if got := str2(creates[0].params[k]); got != want {
					t.Errorf("create parameter %s = %q, want %q", k, got, want)
				}
			}
			if tok := str2(creates[0].params["ClientToken"]); !strings.HasPrefix(tok, "tf-") || len(tok) > 64 {
				t.Errorf("ClientToken = %q", tok)
			}

			v := stateValues(t, r, st)
			if !v["status"].IsNull() && v.str("status") == "Creating" {
				t.Errorf("create did not wait for the resource")
			}
			for k, want := range tc.readBack {
				if got := attrString(v[k]); got != want {
					t.Errorf("state %s = %q, want %q", k, got, want)
				}
			}
			id := v.str("id")
			if id == "" || v.str("client_token") != str2(creates[0].params["ClientToken"]) {
				t.Fatalf("state id %q / client_token %q", id, v.str("client_token"))
			}
			if r.spec.idAlias != "" && v.str(r.spec.idAlias) != id {
				t.Errorf("%s = %q, want %q", r.spec.idAlias, v.str(r.spec.idAlias), id)
			}
			if r.spec.product != "euoss" && v.str("order_id") != "o-"+id {
				t.Errorf("order_id = %q", v.str("order_id"))
			}
			for name, val := range v {
				if val.IsUnknown() {
					t.Errorf("state %s is unknown after create", name)
				}
			}

			st, diags = doRead(t, r, st)
			mustOK(t, "read", diags)
			if got := stateValues(t, r, st); !got.equal(v) {
				t.Errorf("read changed an unchanged resource:\n got %v\nwant %v", got, v)
			}

			if tc.update != nil {
				st, diags = doUpdate(t, r, st, tc.update)
				mustOK(t, "update", diags)
				calls := f.callsOf(tc.modifyAction)
				if len(calls) != 1 {
					t.Fatalf("%d %s calls, want 1", len(calls), tc.modifyAction)
				}
				if str2(calls[0].params[r.spec.idParam]) != id {
					t.Errorf("%s addressed %v, want %s", tc.modifyAction, calls[0].params[r.spec.idParam], id)
				}
				for k, want := range tc.modifyParams {
					if got := str2(calls[0].params[k]); got != want {
						t.Errorf("%s %s = %q, want %q", tc.modifyAction, k, got, want)
					}
				}
				u := stateValues(t, r, st)
				if got := attrString(u[tc.updatedAttribute[0]]); got != tc.updatedAttribute[1] {
					t.Errorf("after update %s = %q, want %q", tc.updatedAttribute[0], got, tc.updatedAttribute[1])
				}
				if u.str("id") != id || u.str("client_token") != v.str("client_token") {
					t.Errorf("update changed the identity: %v", u)
				}
			}

			st, diags = doDelete(t, r, st)
			mustOK(t, "delete", diags)
			if !st.Raw.IsNull() {
				t.Fatalf("state after delete is not removed")
			}
			if d := f.callsOf(r.spec.delete); len(d) != 1 || str2(d[0].params[r.spec.idParam]) != id {
				t.Fatalf("delete calls = %+v", d)
			}
			if f.count(r.spec.product) != 0 {
				t.Fatalf("the resource still exists in the facade")
			}
		})
	}
}

func (v values) equal(o values) bool {
	if len(v) != len(o) {
		return false
	}
	for k, x := range v {
		if !x.Equal(o[k]) {
			return false
		}
	}
	return true
}

// str2 renders a recorded request parameter (string or json.Number).
func str2(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func attrString(v attr.Value) string {
	switch t := v.(type) {
	case types.String:
		return t.ValueString()
	case types.Int64:
		return strconv.FormatInt(t.ValueInt64(), 10)
	}
	return ""
}

// A create whose answer is lost (the facade made the resource, then the
// caller saw 503) is retried with the same ClientToken and lands on the same
// resource.
func TestCreateRetriesWithSameClientToken(t *testing.T) {
	f, client := newFakeFacade(t)
	f.dropCreates = 2
	r := &euResource{spec: vpcSpec, client: client}
	st, diags := doCreate(t, r, map[string]attr.Value{"cidr_block": str("10.1.0.0/16")})
	mustOK(t, "create", diags)
	calls := f.callsOf("CreateVpc")
	if len(calls) != 3 {
		t.Fatalf("%d CreateVpc calls, want 3 (two lost answers, one retry that lands)", len(calls))
	}
	token := str2(calls[0].params["ClientToken"])
	for _, c := range calls[1:] {
		if str2(c.params["ClientToken"]) != token {
			t.Fatalf("retry used ClientToken %v, first attempt %s", c.params["ClientToken"], token)
		}
	}
	if f.count("euvpc") != 1 {
		t.Fatalf("%d VPCs exist, want 1", f.count("euvpc"))
	}
	if v := stateValues(t, r, st); v.str("id") != "vpc-1" || v.str("client_token") != token {
		t.Fatalf("state = %v", v)
	}
	// A second resource gets a token of its own.
	if _, diags := doCreate(t, r, map[string]attr.Value{"cidr_block": str("10.2.0.0/16")}); diags.HasError() {
		t.Fatal(diags)
	}
	if calls := f.callsOf("CreateVpc"); str2(calls[len(calls)-1].params["ClientToken"]) == token {
		t.Fatal("two resources were created with the same ClientToken")
	}
}

func TestCreateWithoutIDFails(t *testing.T) {
	f, client := newFakeFacade(t)
	f.hook = func(product, action string, _ map[string]any) (int, string, bool) {
		if action == "RunInstances" {
			return http.StatusOK, `{"RequestId":"r1","Code":"OK","Data":{"OrderId":"9","Status":"Creating"}}`, true
		}
		return 0, "", false
	}
	r := &euResource{spec: ecsSpec, client: client}
	st, diags := doCreate(t, r, map[string]attr.Value{"instance_type": str("s2.large"), "image_id": str("img-1")})
	if !diags.HasError() || !strings.Contains(diags.Errors()[0].Summary(), "returned no InstanceId") {
		t.Fatalf("diagnostics = %v, want a missing-id error", diags)
	}
	if !st.Raw.IsNull() {
		t.Fatal("state was written without an id")
	}
}

// A business error is not retried and carries code, message and RequestId.
func TestCreateErrorSurfacesCodeAndRequestID(t *testing.T) {
	f, client := newFakeFacade(t)
	f.hook = func(product, action string, _ map[string]any) (int, string, bool) {
		return http.StatusBadRequest, `{"RequestId":"req-42","Code":"InvalidInstanceType.NotFound","Message":"The specified InstanceType is not offered for euecs."}`, true
	}
	r := &euResource{spec: ecsSpec, client: client}
	_, diags := doCreate(t, r, map[string]attr.Value{"instance_type": str("zz"), "image_id": str("img-1")})
	if !diags.HasError() {
		t.Fatal("no error")
	}
	detail := diags.Errors()[0].Detail()
	for _, want := range []string{"InvalidInstanceType.NotFound", "not offered", "req-42", "ClientToken tf-"} {
		if !strings.Contains(detail, want) {
			t.Errorf("error detail %q lacks %q", detail, want)
		}
	}
	if n := len(f.callsOf("RunInstances")); n != 1 {
		t.Fatalf("%d attempts, want 1 (not retryable)", n)
	}
}

// A resource that ends in CreateFailed is kept in state (so Terraform taints
// and replaces it) with an error.
func TestCreateFailedStatusIsAnErrorWithState(t *testing.T) {
	f, client := newFakeFacade(t)
	f.createStatus = "CreateFailed"
	r := &euResource{spec: ecsSpec, client: client}
	st, diags := doCreate(t, r, map[string]attr.Value{"instance_type": str("s2.large"), "image_id": str("img-1")})
	if !diags.HasError() || !strings.Contains(diags.Errors()[0].Detail(), "CreateFailed") {
		t.Fatalf("diagnostics = %v", diags)
	}
	if v := stateValues(t, r, st); v == nil || v.str("id") != "i-1" || v.str("status") != "CreateFailed" {
		t.Fatalf("state = %v, want the failed instance tracked", v)
	}
}

// Read drops a resource that is gone — an empty list, or the product's
// NotFound code — and reports any other failure.
func TestReadRemovesGoneResources(t *testing.T) {
	for _, newRes := range allResources {
		r := newRes().(*euResource)
		t.Run(r.spec.typeName, func(t *testing.T) {
			f, client := newFakeFacade(t)
			r.client = client
			cfg := map[string]attr.Value{}
			for i := range r.spec.attrs {
				if a := &r.spec.attrs[i]; a.required && a.kind == kindInt64 {
					cfg[a.name] = types.Int64Value(20)
				} else if a.required {
					cfg[a.name] = str("x-" + a.name)
				}
			}
			if r.spec.product == "euecs" {
				cfg["instance_type"] = str("s2.large")
			}
			st, diags := doCreate(t, r, cfg)
			mustOK(t, "create", diags)
			id := stateValues(t, r, st).str("id")

			f.mu.Lock()
			delete(f.items[r.spec.product], id)
			f.mu.Unlock()
			gone, diags := doRead(t, r, st)
			mustOK(t, "read", diags)
			if !gone.Raw.IsNull() {
				t.Fatal("a resource missing from the list stayed in state")
			}

			f.hook = func(product, action string, _ map[string]any) (int, string, bool) {
				return http.StatusNotFound, `{"RequestId":"r","Code":"` + r.spec.notFound + `","Message":"gone"}`, true
			}
			gone, diags = doRead(t, r, st)
			mustOK(t, "read (NotFound)", diags)
			if !gone.Raw.IsNull() {
				t.Fatal("a NotFound resource stayed in state")
			}

			f.hook = func(product, action string, _ map[string]any) (int, string, bool) {
				return http.StatusForbidden, `{"RequestId":"r","Code":"IAM.NoPermission","Message":"denied"}`, true
			}
			kept, diags := doRead(t, r, st)
			if !diags.HasError() || kept.Raw.IsNull() {
				t.Fatalf("a refused Describe: diagnostics %v, state removed %v", diags, kept.Raw.IsNull())
			}
		})
	}
}

// Read refreshes drift from Describe — but keeps a configured spelling the
// API only normalised, and never lets an empty answer erase a value.
func TestReadRefreshesDrift(t *testing.T) {
	f, client := newFakeFacade(t)
	r := &euResource{spec: ecsSpec, client: client}
	st, diags := doCreate(t, r, map[string]attr.Value{"instance_type": str("S2.Large"), "image_id": str("img-1"),
		"charge_type": str("PrePaid")})
	mustOK(t, "create", diags)
	if v := stateValues(t, r, st); v.str("instance_type") != "S2.Large" || v.str("charge_type") != "PrePaid" {
		t.Fatalf("create did not keep the planned values: %v", v)
	}
	st, diags = doRead(t, r, st)
	mustOK(t, "read", diags)
	if v := stateValues(t, r, st); v.str("instance_type") != "S2.Large" || v.str("charge_type") != "PrePaid" {
		t.Fatalf("a case-only difference was reported as drift: %v", v)
	}

	item := f.item("euecs", "i-1")
	f.mu.Lock()
	item["InstanceType"] = "s2.xlarge"
	item["InstanceName"] = "renamed"
	item["ZoneId"] = ""
	item["Status"] = "Stopped"
	f.mu.Unlock()
	st, diags = doRead(t, r, st)
	mustOK(t, "read", diags)
	v := stateValues(t, r, st)
	for k, want := range map[string]string{"instance_type": "s2.xlarge", "instance_name": "renamed", "status": "Stopped",
		"zone_id": "cn-north-1-a", "image_id": "img-1"} {
		if v.str(k) != want {
			t.Errorf("after drift %s = %q, want %q", k, v.str(k), want)
		}
	}
}

// When one modify Action fails, the state records what the earlier ones
// already changed.
func TestUpdateFailureKeepsEarlierChanges(t *testing.T) {
	f, client := newFakeFacade(t)
	r := &euResource{spec: ecsSpec, client: client}
	st, diags := doCreate(t, r, map[string]attr.Value{"instance_type": str("s2.large"), "image_id": str("img-1"), "instance_name": str("a")})
	mustOK(t, "create", diags)
	f.hook = func(product, action string, _ map[string]any) (int, string, bool) {
		if action == "ModifyInstanceSpec" {
			return http.StatusConflict, `{"RequestId":"r9","Code":"OperationDenied.UpgradeRequiresOrder","Message":"prepaid"}`, true
		}
		return 0, "", false
	}
	st, diags = doUpdate(t, r, st, map[string]attr.Value{"instance_type": str("s2.xlarge"), "image_id": str("img-1"), "instance_name": str("b")})
	if !diags.HasError() || !strings.Contains(diags.Errors()[0].Detail(), "OperationDenied.UpgradeRequiresOrder") {
		t.Fatalf("diagnostics = %v", diags)
	}
	v := stateValues(t, r, st)
	if v.str("instance_name") != "b" || v.str("instance_type") != "s2.large" {
		t.Fatalf("state after a partial update = %v", v)
	}
}

// Buckets are found by exact name in ListBuckets (Prefix=name), across pages;
// a bucket that only shares the prefix is not a match.
func TestBucketLookupPaginates(t *testing.T) {
	f, client := newFakeFacade(t)
	r := &euResource{spec: bucketSpec, client: client}
	f.hook = func(product, action string, p map[string]any) (int, string, bool) {
		if action != "ListBuckets" || p["Prefix"] != "logs" || str2(p["PageSize"]) != "100" {
			return http.StatusBadRequest, `{"RequestId":"r","Code":"InvalidParameter","Message":"unexpected"}`, true
		}
		if str2(p["PageNumber"]) == "1" {
			return http.StatusOK, `{"RequestId":"r","Code":"OK","Data":{"Buckets":[{"Bucket":"logs-a"},{"Bucket":"logs-b"}],"TotalCount":101,"PageNumber":1,"PageSize":100}}`, true
		}
		return http.StatusOK, `{"RequestId":"r","Code":"OK","Data":{"Buckets":[{"Bucket":"logs","Status":"Available"}],"TotalCount":101,"PageNumber":2,"PageSize":100}}`, true
	}
	item, err := r.lookup(context.Background(), "logs")
	if err != nil || item == nil || item["Bucket"] != "logs" {
		t.Fatalf("lookup on page 2 = %v, %v", item, err)
	}
	if calls := f.callsOf("ListBuckets"); len(calls) != 2 || str2(calls[1].params["PageNumber"]) != "2" {
		t.Fatalf("ListBuckets calls = %+v", calls)
	}

	f.hook = func(product, action string, p map[string]any) (int, string, bool) {
		return http.StatusOK, `{"RequestId":"r","Code":"OK","Data":{"Buckets":[{"Bucket":"logs-a"}],"TotalCount":1,"PageNumber":1,"PageSize":100}}`, true
	}
	if item, err := r.lookup(context.Background(), "logs"); err != nil || item != nil {
		t.Fatalf("prefix-only match = %v, %v; want not found", item, err)
	}
}

// Create/Read/Update/Delete stop at the diagnostics when the plan or state
// does not match the schema, without calling the API.
func TestCrudStopsWhenPlanOrStateCannotBeRead(t *testing.T) {
	ctx := context.Background()
	f, client := newFakeFacade(t)
	alien := schema.Schema{Attributes: map[string]schema.Attribute{"other": schema.StringAttribute{Optional: true}}}
	type alienModel struct {
		Other types.String `tfsdk:"other"`
	}
	badPlan := tfsdk.Plan{Schema: alien}
	if diags := badPlan.Set(ctx, &alienModel{Other: str("x")}); diags.HasError() {
		t.Fatal(diags)
	}
	badState := tfsdk.State{Schema: alien, Raw: badPlan.Raw.Copy()}
	for _, newRes := range allResources {
		r := newRes().(*euResource)
		r.client = client
		var cresp resource.CreateResponse
		cresp.State = emptyState(t, r)
		r.Create(ctx, resource.CreateRequest{Plan: badPlan}, &cresp)
		rresp := resource.ReadResponse{State: tfsdk.State{Schema: alien, Raw: badState.Raw.Copy()}}
		r.Read(ctx, resource.ReadRequest{State: badState}, &rresp)
		uresp := resource.UpdateResponse{State: tfsdk.State{Schema: alien, Raw: badState.Raw.Copy()}}
		r.Update(ctx, resource.UpdateRequest{Plan: badPlan, State: badState}, &uresp)
		dresp := resource.DeleteResponse{State: tfsdk.State{Schema: alien, Raw: badState.Raw.Copy()}}
		r.Delete(ctx, resource.DeleteRequest{State: badState}, &dresp)
		for name, d := range map[string]bool{
			"create": cresp.Diagnostics.HasError(), "read": rresp.Diagnostics.HasError(),
			"update": uresp.Diagnostics.HasError(), "delete": dresp.Diagnostics.HasError(),
		} {
			if !d {
				t.Fatalf("%s %s: undecodable plan/state produced no error", r.spec.typeName, name)
			}
		}
	}
	if n := len(f.calls); n != 0 {
		t.Fatalf("%d API call(s) made after the plan/state could not be read", n)
	}
}

// Transport failures and 5xx/429 answers are retried; business errors and
// unconfigured clients are not.
func TestRetryable(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&transportError{err: errors.New("connection reset")}, true},
		{&apiError{Code: "ServiceUnavailable", Status: 503}, true},
		{&apiError{Code: "Common.InternalError", Status: 504}, true},
		{&apiError{Code: "Common.Throttling", Status: 429}, true},
		{&apiError{Code: "InvalidParameter", Status: 400}, false},
		{&apiError{Code: "IdempotentParameterMismatch", Status: 409}, false},
		{errNotConfigured, false},
	} {
		if got := retryable(c.err); got != c.want {
			t.Errorf("retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// A 2xx whose envelope carries an error code is an error; Data null is an
// empty object.
func TestDecodeEnvelope(t *testing.T) {
	if _, err := decodeEnvelope("X", 200, []byte(`{"RequestId":"r","Code":"InvalidParameter","Message":"bad"}`)); errorCode(err) != "InvalidParameter" {
		t.Fatalf("error envelope under 200 = %v", err)
	}
	if d, err := decodeEnvelope("X", 200, []byte(`{"RequestId":"r","Code":"OK","Data":null}`)); err != nil || len(d) != 0 {
		t.Fatalf("null Data = %v, %v", d, err)
	}
	if _, err := decodeEnvelope("X", 200, []byte(`{"Code":"OK","Data":[1]}`)); err == nil {
		t.Fatal("a non-object Data was accepted")
	}
	if _, err := decodeEnvelope("X", 200, []byte(`<html>`)); err == nil {
		t.Fatal("a non-envelope body was accepted")
	}
}
