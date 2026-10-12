// Product resources (三期验收 C3: Terraform Provider 覆盖核心产品) against the
// OpenAPI facade (services/svc-openapi).
//
// Every resource is one resourceSpec — its attributes and the Actions that
// create, describe, modify and delete it — run by one implementation
// (euResource), so a further product is a spec, not a new code path:
//
//   - Create sends the create Action with a fresh ClientToken (kept in state),
//     retries retryable failures with the same token (so a retry lands on the
//     resource the first attempt made), fails when the answer carries no id,
//     then waits until the resource leaves Creating and fills every computed
//     attribute from the Describe action.
//   - Read calls the Describe/List action and reads the resource out of its
//     list (Instances[...], Vpcs[...], Buckets[...]); a resource that is gone
//     (an empty list, or the product's NotFound code) leaves the state.
//   - Update calls the modify Action of each changed attribute (and only then
//     sets state); attributes that cannot change in place force replacement.
//   - Delete calls the delete Action (NotFound counts as done) and waits until
//     the resource is gone.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Waits for asynchronous provisioning; variables so the tests can shorten them.
var (
	pollInterval  = 5 * time.Second
	createTimeout = 30 * time.Minute
	updateTimeout = 30 * time.Minute
	deleteTimeout = 20 * time.Minute
)

// pendingStatuses are transitional: Create and Update wait them out.
var pendingStatuses = map[string]bool{"Creating": true, "Modifying": true}

type attrKind int

const (
	kindString attrKind = iota
	kindInt64
)

// attrSpec maps one Terraform attribute onto the API.
type attrSpec struct {
	name string
	desc string
	kind attrKind
	// required / optional+computed / computed; a plain optional attribute
	// (neither required nor computed) is an input only the configuration
	// knows.
	required, optional, computed bool
	// param is the create parameter ("" = not sent); field the Describe
	// field it is read back from ("" = not readable: the configured value is
	// kept).
	param, field string
	// forceNew: no Action changes it in place (RequiresReplace). stable: once
	// known, an update never changes it (UseStateForUnknown).
	forceNew, stable bool
	// modify is the Action that changes it in place, modifyParam its
	// parameter (default param).
	modify, modifyParam string
	// foldCase: the API normalises the spelling (s2.Large → s2.large,
	// PrePaid → Prepaid); a refresh keeps the configured spelling of an
	// equal value instead of planning a change.
	foldCase bool
}

func (a *attrSpec) attrType() attr.Type {
	if a.kind == kindInt64 {
		return types.Int64Type
	}
	return types.StringType
}

// resourceSpec describes one product resource.
type resourceSpec struct {
	typeName, product, label string
	// idParam addresses one resource in the Actions (InstanceId, VpcId, ...;
	// Bucket for euoss, whose resources are addressed by name); createIDField
	// is the create answer's id key (default idParam).
	idParam, createIDField string
	// idAlias is an extra computed attribute mirroring id (vpc_id), "" none.
	idAlias                  string
	create, describe, delete string
	// listKey is the Describe answer's array; byName lists by Prefix and
	// matches idParam exactly (euoss ListBuckets has no id filter).
	listKey string
	byName  bool
	// notFound is the product's code for a resource that does not exist.
	notFound string
	attrs    []attrSpec
}

func (s *resourceSpec) attr(name string) *attrSpec {
	for i := range s.attrs {
		if s.attrs[i].name == name {
			return &s.attrs[i]
		}
	}
	return nil
}

func (s *resourceSpec) attrTypes() map[string]attr.Type {
	m := make(map[string]attr.Type, len(s.attrs))
	for i := range s.attrs {
		m[s.attrs[i].name] = s.attrs[i].attrType()
	}
	return m
}

// --- shared attributes --------------------------------------------------------

func idAttr(desc string) attrSpec {
	return attrSpec{name: "id", desc: desc, computed: true, stable: true}
}

func regionAttr() attrSpec {
	return attrSpec{name: "region", desc: "Region, e.g. cn-north-1. Defaults to the zone's region, else the provider's region. Changing it replaces the resource.",
		optional: true, computed: true, param: "RegionId", field: "RegionId", forceNew: true, stable: true}
}

func zoneAttr() attrSpec {
	return attrSpec{name: "zone_id", desc: "Availability zone, e.g. cn-north-1-a. Defaults to the region's first zone. Changing it replaces the resource.",
		optional: true, computed: true, param: "ZoneId", field: "ZoneId", forceNew: true, stable: true}
}

func purchaseAttrs() []attrSpec {
	return []attrSpec{
		{name: "charge_type", desc: "Billing method: Prepaid or Postpaid (default). Changing it replaces the resource.",
			optional: true, computed: true, param: "ChargeType", field: "ChargeType", forceNew: true, stable: true, foldCase: true},
		{name: "period", desc: "Prepaid term in months (1-60); used at creation only. Changing it replaces the resource.",
			kind: kindInt64, optional: true, param: "Period", forceNew: true},
		{name: "sku_code", desc: "The SKU in effect, e.g. euecs.s2.large.postpaid.", computed: true, field: "SkuCode"},
		{name: "expired_time", desc: "Expiry time of a Prepaid resource (RFC 3339); empty for Postpaid.", computed: true, field: "ExpiredTime", stable: true},
	}
}

func lifecycleAttrs() []attrSpec {
	return []attrSpec{
		{name: "status", desc: "Lifecycle status (Running/Available, Stopped, ...).", computed: true, field: "Status"},
		{name: "creation_time", desc: "Creation time (RFC 3339).", computed: true, field: "CreationTime", stable: true},
		{name: "client_token", desc: "The ClientToken the resource was created with (idempotency key of the purchase).", computed: true, stable: true},
		{name: "order_id", desc: "The purchase order of the resource.", computed: true, stable: true},
	}
}

func join(groups ...[]attrSpec) []attrSpec {
	var out []attrSpec
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// --- the resources ------------------------------------------------------------

var ecsSpec = &resourceSpec{
	typeName: "euler_euecs_instance", product: "euecs", label: "ECS instance",
	idParam: "InstanceId", create: "RunInstances", describe: "DescribeInstances", delete: "TerminateInstances",
	listKey: "Instances", notFound: "InvalidInstanceId.NotFound",
	attrs: join([]attrSpec{
		idAttr("Instance id."),
		{name: "instance_type", desc: "Instance type, e.g. s2.large. Changed in place (ModifyInstanceSpec; Postpaid only).",
			required: true, param: "InstanceType", field: "InstanceType", modify: "ModifyInstanceSpec", foldCase: true},
		{name: "image_id", desc: "Image id. Changing it replaces the instance.", required: true, param: "ImageId", forceNew: true},
		{name: "instance_name", desc: "Display name (at most 64 characters). Changed in place (ModifyInstanceAttribute).",
			optional: true, computed: true, param: "InstanceName", field: "InstanceName", modify: "ModifyInstanceAttribute", stable: true},
		zoneAttr(), regionAttr(),
	}, purchaseAttrs(), lifecycleAttrs()),
}

var rdsSpec = &resourceSpec{
	typeName: "euler_eurds_instance", product: "eurds", label: "RDS instance",
	idParam: "InstanceId", idAlias: "instance_id", create: "CreateInstance", describe: "DescribeInstances", delete: "DeleteInstance",
	listKey: "Instances", notFound: "InvalidInstanceId.NotFound",
	attrs: join([]attrSpec{
		idAttr("Instance id."),
		{name: "instance_id", desc: "Instance id (same as id).", computed: true, stable: true},
		{name: "engine_version", desc: "MySQL version, e.g. 8.0. Changing it replaces the instance.",
			required: true, param: "EngineVersion", field: "EngineVersion", forceNew: true},
		{name: "engine", desc: "Database engine (MySQL).", computed: true, field: "Engine", stable: true},
		{name: "instance_class", desc: "Instance class, e.g. mysql8.small; defaults to the class on sale. Changed in place (ModifyInstanceSpec; Postpaid only).",
			optional: true, computed: true, param: "InstanceClass", field: "InstanceClass", modify: "ModifyInstanceSpec", stable: true, foldCase: true},
		{name: "instance_name", desc: "Display name (at most 64 characters). Changed in place (ModifyInstanceAttribute).",
			optional: true, computed: true, param: "InstanceName", field: "InstanceName", modify: "ModifyInstanceAttribute", stable: true},
		zoneAttr(), regionAttr(),
	}, purchaseAttrs(), lifecycleAttrs()),
}

var vpcSpec = &resourceSpec{
	typeName: "euler_euvpc_vpc", product: "euvpc", label: "VPC",
	idParam: "VpcId", idAlias: "vpc_id", create: "CreateVpc", describe: "DescribeVpcs", delete: "DeleteVpc",
	listKey: "Vpcs", notFound: "InvalidVpcId.NotFound",
	attrs: join([]attrSpec{
		idAttr("VPC id."),
		{name: "vpc_id", desc: "VPC id (same as id).", computed: true, stable: true},
		{name: "cidr_block", desc: "IPv4 CIDR block, e.g. 192.168.0.0/16. Changing it replaces the VPC.",
			required: true, param: "CidrBlock", field: "CidrBlock", forceNew: true},
		{name: "vpc_name", desc: "Display name (at most 64 characters). Changed in place (ModifyVpcAttribute).",
			optional: true, computed: true, param: "VpcName", field: "VpcName", modify: "ModifyVpcAttribute", stable: true},
		regionAttr(),
	}, lifecycleAttrs()),
}

var diskSpec = &resourceSpec{
	typeName: "euler_eubs_disk", product: "eubs", label: "disk",
	idParam: "DiskId", create: "CreateDisk", describe: "DescribeDisks", delete: "DeleteDisk",
	listKey: "Disks", notFound: "InvalidDiskId.NotFound",
	attrs: join([]attrSpec{
		idAttr("Disk id."),
		{name: "size", desc: "Size in GiB. Changing it replaces the disk (no resize Action yet).",
			kind: kindInt64, required: true, param: "Size", field: "Size", forceNew: true},
		{name: "disk_category", desc: "Disk category, e.g. essd; defaults to the category on sale. Changing it replaces the disk.",
			optional: true, computed: true, param: "DiskCategory", field: "DiskCategory", forceNew: true, stable: true, foldCase: true},
		{name: "disk_name", desc: "Display name (at most 64 characters). Changed in place (ModifyDiskAttribute).",
			optional: true, computed: true, param: "DiskName", field: "DiskName", modify: "ModifyDiskAttribute", stable: true},
		zoneAttr(), regionAttr(),
	}, purchaseAttrs(), lifecycleAttrs()),
}

var eipSpec = &resourceSpec{
	typeName: "euler_eueip_address", product: "eueip", label: "EIP",
	idParam: "AllocationId", create: "AllocateEipAddress", describe: "DescribeEipAddresses", delete: "ReleaseEipAddress",
	listKey: "EipAddresses", notFound: "InvalidAllocationId.NotFound",
	attrs: join([]attrSpec{
		idAttr("Allocation id."),
		{name: "bandwidth", desc: "Peak bandwidth in Mbps. Changing it replaces the EIP.",
			kind: kindInt64, optional: true, computed: true, param: "Bandwidth", field: "Bandwidth", forceNew: true, stable: true},
		{name: "name", desc: "Display name (at most 64 characters). Changing it replaces the EIP (no rename Action).",
			optional: true, computed: true, param: "Name", field: "Name", forceNew: true, stable: true},
		{name: "internet_charge_type", desc: "PayByBandwidth or PayByTraffic (follows from the SKU).", computed: true, field: "InternetChargeType", stable: true},
		zoneAttr(), regionAttr(),
	}, purchaseAttrs(), lifecycleAttrs()),
}

// The sku_code of a disk or an EIP never changes in place.
func init() {
	for _, s := range []*resourceSpec{diskSpec, eipSpec} {
		s.attr("sku_code").stable = true
	}
}

var bucketSpec = &resourceSpec{
	typeName: "euler_euoss_bucket", product: "euoss", label: "bucket",
	idParam: "Bucket", createIDField: "BucketName", create: "CreateBucket", describe: "ListBuckets", delete: "DeleteBucket",
	listKey: "Buckets", byName: true, notFound: "InvalidBucketName.NotFound",
	attrs: join([]attrSpec{
		idAttr("Bucket name (buckets are addressed by name)."),
		{name: "bucket", desc: "Bucket name: 3-63 lowercase letters, digits, dots and hyphens. Changing it replaces the bucket.",
			required: true, param: "Bucket", field: "Bucket", forceNew: true},
		{name: "storage_class", desc: "Storage class (Standard). Changing it replaces the bucket.",
			optional: true, computed: true, param: "StorageClass", field: "StorageClass", forceNew: true, stable: true, foldCase: true},
		regionAttr(),
	}, lifecycleAttrs()),
}

// Constructors (provider.Resources).
func NewScecsResource() resource.Resource { return &euResource{spec: ecsSpec} }
func NewScrdsResource() resource.Resource { return &euResource{spec: rdsSpec} }
func NewScvpcResource() resource.Resource { return &euResource{spec: vpcSpec} }
func NewScbsResource() resource.Resource  { return &euResource{spec: diskSpec} }
func NewSceipResource() resource.Resource { return &euResource{spec: eipSpec} }
func NewScossResource() resource.Resource { return &euResource{spec: bucketSpec} }

// --- the implementation -------------------------------------------------------

// euResource runs one resourceSpec.
type euResource struct {
	spec   *resourceSpec
	client *apiClient
}

var _ resource.ResourceWithConfigure = (*euResource)(nil)

func (r *euResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = r.spec.typeName
}

func (r *euResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := make(map[string]schema.Attribute, len(r.spec.attrs))
	for i := range r.spec.attrs {
		a := &r.spec.attrs[i]
		switch a.kind {
		case kindInt64:
			var mods []planmodifier.Int64
			if a.stable {
				mods = append(mods, int64planmodifier.UseStateForUnknown())
			}
			if a.forceNew {
				mods = append(mods, int64planmodifier.RequiresReplace())
			}
			attrs[a.name] = schema.Int64Attribute{Description: a.desc, Required: a.required,
				Optional: a.optional, Computed: a.computed, PlanModifiers: mods}
		default:
			var mods []planmodifier.String
			if a.stable {
				mods = append(mods, stringplanmodifier.UseStateForUnknown())
			}
			if a.forceNew {
				mods = append(mods, stringplanmodifier.RequiresReplace())
			}
			attrs[a.name] = schema.StringAttribute{Description: a.desc, Required: a.required,
				Optional: a.optional, Computed: a.computed, PlanModifiers: mods}
		}
	}
	resp.Schema = schema.Schema{
		Description: fmt.Sprintf("A %s (%s, via %s / %s / %s).", r.spec.label, r.spec.product, r.spec.create, r.spec.describe, r.spec.delete),
		Attributes:  attrs,
	}
}

// Configure takes the client the provider configured. ProviderData is nil
// until the provider itself is configured (Terraform also configures
// resources before that, e.g. to validate), which is not an error.
func (r *euResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*apiClient)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("expected *apiClient, got %T; this is a provider bug", req.ProviderData))
		return
	}
	r.client = c
}

// values is a plan or state as attribute values, checked against the spec.
type values map[string]attr.Value

func (r *euResource) decode(obj types.Object, what string) (values, diag.Diagnostics) {
	var diags diag.Diagnostics
	got := obj.Attributes()
	out := make(values, len(r.spec.attrs))
	for i := range r.spec.attrs {
		a := &r.spec.attrs[i]
		v, ok := got[a.name]
		if !ok || !v.Type(context.Background()).Equal(a.attrType()) {
			diags.AddError("Invalid "+what, fmt.Sprintf("The %s does not match the %s schema (attribute %q); this is a provider bug.", what, r.spec.typeName, a.name))
			return nil, diags
		}
		out[a.name] = v
	}
	return out, diags
}

func (r *euResource) object(v values) (types.Object, diag.Diagnostics) {
	return types.ObjectValue(r.spec.attrTypes(), v)
}

func (v values) str(name string) string {
	if s, ok := v[name].(types.String); ok && !s.IsNull() && !s.IsUnknown() {
		return s.ValueString()
	}
	return ""
}

// apiValue converts an answer field to the attribute's type. ok is false for
// an absent or null field.
func apiValue(a *attrSpec, raw any) (attr.Value, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	if a.kind == kindInt64 {
		var s string
		switch t := raw.(type) {
		case json.Number:
			s = t.String()
		case string:
			s = t
		default:
			return nil, false, fmt.Errorf("%s: expected a number, got %T", a.field, raw)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %q is not an integer", a.field, s)
		}
		return types.Int64Value(n), true, nil
	}
	switch t := raw.(type) {
	case string:
		return types.StringValue(t), true, nil
	case json.Number:
		return types.StringValue(t.String()), true, nil
	}
	return nil, false, fmt.Errorf("%s: expected a string, got %T", a.field, raw)
}

func isEmpty(v attr.Value) bool {
	if v == nil || v.IsNull() || v.IsUnknown() {
		return true
	}
	s, ok := v.(types.String)
	return ok && s.ValueString() == ""
}

// refresh folds a Describe item into prior values (Read): every readable
// attribute takes the API's value, except that an empty answer never erases
// a known value and a case-insensitive product spelling keeps the configured
// one.
func (r *euResource) refresh(prior values, item map[string]any) (values, error) {
	out := make(values, len(prior))
	for k, v := range prior {
		out[k] = v
	}
	for i := range r.spec.attrs {
		a := &r.spec.attrs[i]
		if a.field == "" {
			continue
		}
		v, ok, err := apiValue(a, item[a.field])
		if err != nil {
			return nil, err
		}
		if !ok || (isEmpty(v) && !isEmpty(prior[a.name])) {
			continue
		}
		if a.foldCase {
			if p, isStr := prior[a.name].(types.String); isStr && !isEmpty(p) &&
				strings.EqualFold(p.ValueString(), v.(types.String).ValueString()) {
				continue
			}
		}
		out[a.name] = v
	}
	return out, nil
}

// settle fills what a plan left unknown from a Describe item (after Create
// and Update). Known planned values are kept as planned — Terraform requires
// the applied state to match them — and anything still unknown becomes null.
func (r *euResource) settle(planned values, item map[string]any) (values, error) {
	out := make(values, len(planned))
	for i := range r.spec.attrs {
		a := &r.spec.attrs[i]
		v := planned[a.name]
		if v.IsUnknown() {
			v = nil
			if a.field != "" && item != nil {
				got, ok, err := apiValue(a, item[a.field])
				if err != nil {
					return nil, err
				}
				if ok {
					v = got
				}
			}
			if v == nil {
				if a.kind == kindInt64 {
					v = types.Int64Null()
				} else {
					v = types.StringNull()
				}
			}
		}
		out[a.name] = v
	}
	return out, nil
}

func (r *euResource) setID(v values, id string) {
	v["id"] = types.StringValue(id)
	if r.spec.idAlias != "" {
		v[r.spec.idAlias] = types.StringValue(id)
	}
}

// lookup finds one resource through the Describe/List action; nil when it no
// longer exists.
func (r *euResource) lookup(ctx context.Context, id string) (map[string]any, error) {
	s := r.spec
	for page := int64(1); ; page++ {
		params := map[string]any{s.idParam: id}
		if s.byName {
			params = map[string]any{"Prefix": id, "PageNumber": page, "PageSize": 100}
		}
		data, err := r.client.call(ctx, s.product, s.describe, params)
		if err != nil {
			if errorCode(err) == s.notFound {
				return nil, nil
			}
			return nil, err
		}
		raw, present := data[s.listKey]
		list, isList := raw.([]any)
		if present && raw != nil && !isList {
			return nil, fmt.Errorf("%s: %s is not a list", s.describe, s.listKey)
		}
		for _, e := range list {
			if item, ok := e.(map[string]any); ok && item[s.idParam] == id {
				return item, nil
			}
		}
		if !s.byName || len(list) == 0 {
			return nil, nil
		}
		total, err := strconv.ParseInt(fmt.Sprint(data["TotalCount"]), 10, 64)
		if err != nil || page*100 >= total {
			return nil, nil
		}
	}
}

// wait polls the resource until it leaves the pending statuses (or is gone:
// nil item). It returns the last item seen when the timeout or the context
// ends first, with the error.
func (r *euResource) wait(ctx context.Context, id string, timeout time.Duration, done func(item map[string]any) bool) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	for {
		item, err := r.lookup(ctx, id)
		if err != nil {
			return nil, err
		}
		if done(item) {
			return item, nil
		}
		if time.Now().After(deadline) {
			return item, fmt.Errorf("%s %s is still %v after %s", r.spec.label, id, item["Status"], timeout)
		}
		select {
		case <-ctx.Done():
			return item, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func settled(item map[string]any) bool {
	if item == nil {
		return true
	}
	status, _ := item["Status"].(string)
	return !pendingStatuses[status]
}

func gone(item map[string]any) bool {
	if item == nil {
		return true
	}
	status, _ := item["Status"].(string)
	return status == "Deleted"
}

func (r *euResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var obj types.Object
	resp.Diagnostics.Append(req.Plan.Get(ctx, &obj)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan, diags := r.decode(obj, "plan")
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	s := r.spec
	params := map[string]any{}
	for i := range s.attrs {
		a := &s.attrs[i]
		if a.param == "" {
			continue
		}
		switch v := plan[a.name].(type) {
		case types.String:
			if !v.IsNull() && !v.IsUnknown() {
				params[a.param] = v.ValueString()
			}
		case types.Int64:
			if !v.IsNull() && !v.IsUnknown() {
				params[a.param] = v.ValueInt64()
			}
		}
	}
	// Without a region (or a zone, which names its region) the resource goes
	// to the provider's region rather than the deployment's default.
	_, hasRegion := params["RegionId"]
	_, hasZone := params["ZoneId"]
	if s.attr("region") != nil && !hasRegion && !hasZone && r.client != nil && r.client.region != "" {
		params["RegionId"] = r.client.region
	}
	token := newClientToken()
	params["ClientToken"] = token

	data, err := r.client.call(ctx, s.product, s.create, params)
	if err != nil {
		resp.Diagnostics.AddError(s.create+" failed",
			fmt.Sprintf("Creating the %s failed: %v.\n\nThe request used ClientToken %s; if it timed out, the %s may still have been created.",
				s.label, err, token, s.label))
		return
	}
	idKey := s.createIDField
	if idKey == "" {
		idKey = s.idParam
	}
	id, _ := data[idKey].(string)
	if id == "" {
		resp.Diagnostics.AddError(s.create+" returned no "+idKey,
			fmt.Sprintf("The %s answer carries no %s (ClientToken %s); the %s cannot be tracked.", s.create, idKey, token, s.label))
		return
	}
	r.setID(plan, id)
	plan["client_token"] = types.StringValue(token)
	if orderID, ok := data["OrderId"].(string); ok {
		plan["order_id"] = types.StringValue(orderID)
	}

	item, waitErr := r.wait(ctx, id, createTimeout, settled)
	if waitErr == nil && item == nil {
		waitErr = fmt.Errorf("the %s %s disappeared right after %s", s.label, id, s.create)
	}
	state, err := r.settle(plan, item)
	if err != nil {
		resp.Diagnostics.AddError(s.describe+" returned an invalid value", err.Error())
		state, _ = r.settle(plan, nil)
	}
	obj, diags = r.object(state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The resource exists from here on: state is saved even when it did not
	// come up, so Terraform taints it (and replaces it on the next apply)
	// instead of losing track of it.
	resp.Diagnostics.Append(resp.State.Set(ctx, obj)...)
	switch {
	case waitErr != nil:
		resp.Diagnostics.AddError("Waiting for the "+s.label, waitErr.Error())
	case item["Status"] == "CreateFailed":
		resp.Diagnostics.AddError(s.label+" creation failed",
			fmt.Sprintf("The %s %s reached status CreateFailed; it will be replaced on the next apply.", s.label, id))
	}
}

func (r *euResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var obj types.Object
	resp.Diagnostics.Append(req.State.Get(ctx, &obj)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior, diags := r.decode(obj, "state")
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := prior.str("id")
	item, err := r.lookup(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError(r.spec.describe+" failed", err.Error())
		return
	}
	if item == nil || gone(item) {
		resp.State.RemoveResource(ctx)
		return
	}
	next, err := r.refresh(prior, item)
	if err != nil {
		resp.Diagnostics.AddError(r.spec.describe+" returned an invalid value", err.Error())
		return
	}
	obj, diags = r.object(next)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, obj)...)
}

func (r *euResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var planObj, stateObj types.Object
	resp.Diagnostics.Append(req.Plan.Get(ctx, &planObj)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &stateObj)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan, diags := r.decode(planObj, "plan")
	resp.Diagnostics.Append(diags...)
	prior, diags := r.decode(stateObj, "state")
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	s := r.spec
	id := prior.str("id")

	// One call per modify Action, carrying every changed attribute it owns.
	changes := map[string]map[string]any{}
	for i := range s.attrs {
		a := &s.attrs[i]
		v := plan[a.name]
		if a.modify == "" || v.IsUnknown() || v.IsNull() || v.Equal(prior[a.name]) {
			continue
		}
		param := a.modifyParam
		if param == "" {
			param = a.param
		}
		if changes[a.modify] == nil {
			changes[a.modify] = map[string]any{s.idParam: id}
		}
		switch t := v.(type) {
		case types.String:
			changes[a.modify][param] = t.ValueString()
		case types.Int64:
			changes[a.modify][param] = t.ValueInt64()
		}
	}
	actions := make([]string, 0, len(changes))
	for action := range changes {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	for _, action := range actions {
		if _, err := r.client.call(ctx, s.product, action, changes[action]); err != nil {
			resp.Diagnostics.AddError(action+" failed", fmt.Sprintf("Changing the %s %s failed: %v.", s.label, id, err))
			// Record what the earlier Actions already changed.
			if item, lerr := r.lookup(ctx, id); lerr == nil && item != nil {
				if next, rerr := r.refresh(prior, item); rerr == nil {
					if obj, d := r.object(next); !d.HasError() {
						resp.Diagnostics.Append(resp.State.Set(ctx, obj)...)
					}
				}
			}
			return
		}
	}

	item, waitErr := r.wait(ctx, id, updateTimeout, settled)
	if waitErr == nil && item == nil {
		waitErr = fmt.Errorf("the %s %s no longer exists", s.label, id)
	}
	if waitErr != nil {
		resp.Diagnostics.AddError("Waiting for the "+s.label, waitErr.Error())
		return
	}
	state, err := r.settle(plan, item)
	if err != nil {
		resp.Diagnostics.AddError(s.describe+" returned an invalid value", err.Error())
		return
	}
	obj, diags := r.object(state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, obj)...)
}

func (r *euResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var obj types.Object
	resp.Diagnostics.Append(req.State.Get(ctx, &obj)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior, diags := r.decode(obj, "state")
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	s := r.spec
	id := prior.str("id")
	if _, err := r.client.call(ctx, s.product, s.delete, map[string]any{s.idParam: id}); err != nil && errorCode(err) != s.notFound {
		resp.Diagnostics.AddError(s.delete+" failed", fmt.Sprintf("Deleting the %s %s failed: %v.", s.label, id, err))
		return
	}
	if item, err := r.wait(ctx, id, deleteTimeout, gone); err != nil {
		// The release was accepted; the resource only has not finished
		// going away yet.
		resp.Diagnostics.AddWarning("The "+s.label+" is still being deleted",
			fmt.Sprintf("%s accepted the deletion of %s, but it is not gone yet (status %v): %v.", s.delete, id, item["Status"], err))
	}
	resp.State.RemoveResource(ctx)
}
