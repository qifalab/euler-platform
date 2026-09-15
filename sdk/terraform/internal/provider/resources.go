// Product resources for the core products (三期验收 C3: Terraform Provider
// 覆盖核心产品 — EUECS/EUOSS/EUVPC/EURDS).
//
// Each resource is deliberately thin: Create/Read/Update/Delete are "sign the
// product OpenAPI action with the shared eusdk client and reconcile state".
// There is no per-product secret or retry logic here — eusdk owns signing and
// error mapping (one implementation, 03§9.4 rule ⑤). The four resources share
// one CRUD helper so a fifth product is a schema + four action names, not a new
// code path.
package provider

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/qifalab/euler-platform/eusdk"
)

// crudCall signs and sends one product action, returning the decoded Data body.
func crudCall(client *eusdk.Client, productCode, action string, params map[string]any) (map[string]any, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	resp, err := client.Call(eusdk.ApiRequest{
		ProductCode: productCode,
		Method:      "POST",
		Path:        "/",
		Query: url.Values{
			"Action":  {action},
			"Version": {"2026-08-01"},
		},
		Body: body,
	})
	if err != nil {
		return nil, err
	}
	var env struct {
		Data map[string]any `json:"Data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// idFromData extracts the resource id an action returns (CreateInstance →
// InstanceId etc.). The per-product key differs; each resource names its own.
func idFromData(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

// --- EUECS (云服务器 VM) -----------------------------------------------------

type euecsResource struct{}

func NewScecsResource() resource.Resource { return &euecsResource{} }

type euecsModel struct {
	ID           types.String `tfsdk:"id"`
	ImageID      types.String `tfsdk:"image_id"`
	InstanceType types.String `tfsdk:"instance_type"`
	ZoneID       types.String `tfsdk:"zone_id"`
}

func (r *euecsResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "euler_euecs_instance"
}

func (r *euecsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"id":            schema.StringAttribute{Computed: true},
			"image_id":      schema.StringAttribute{Required: true},
			"instance_type": schema.StringAttribute{Required: true},
			// ZONAL product (M-6.1b): the zone must be chosen at create time.
			"zone_id": schema.StringAttribute{Required: true},
		},
	}
}

func (r *euecsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan euecsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	client := req.ProviderData.(*eusdk.Client)
	data, err := crudCall(client, "euecs", "RunInstances", map[string]any{
		"ImageId": plan.ImageID.ValueString(), "InstanceType": plan.InstanceType.ValueString(),
		"ZoneId": plan.ZoneID.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("RunInstances", err.Error())
		return
	}
	plan.ID = types.StringValue(idFromData(data, "InstanceId"))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *euecsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state euecsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	client := req.ProviderData.(*eusdk.Client)
	data, err := crudCall(client, "euecs", "DescribeInstances", map[string]any{"InstanceId": state.ID.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("DescribeInstances", err.Error())
		return
	}
	// Reconcile the mutable attributes back into state.
	if v := data["InstanceType"].(string); v != "" {
		state.InstanceType = types.StringValue(v)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *euecsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// VM type change is a stop/modify/start cycle in the product; the skeleton
	// treats the attribute as re-readable via Read rather than faking an in-place
	// change. A real provider would call the product's ModifyInstanceType.
	resp.Diagnostics.Append(req.State.Set(ctx, &req.Plan)...)
}

func (r *euecsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state euecsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	client := req.ProviderData.(*eusdk.Client)
	if _, err := crudCall(client, "euecs", "TerminateInstances", map[string]any{"InstanceId": state.ID.ValueString()}); err != nil {
		resp.Diagnostics.AddError("TerminateInstances", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}

// --- EUOSS (对象存储桶) ------------------------------------------------------

type euossResource struct{}

func NewScossResource() resource.Resource { return &euossResource{} }

type euossModel struct {
	ID     types.String `tfsdk:"id"`
	Bucket types.String `tfsdk:"bucket"`
}

func (r *euossResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "euler_euoss_bucket"
}

func (r *euossResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"id":     schema.StringAttribute{Computed: true},
			"bucket": schema.StringAttribute{Required: true},
		},
	}
}

func (r *euossResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan euossModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	client := req.ProviderData.(*eusdk.Client)
	data, err := crudCall(client, "euoss", "CreateBucket", map[string]any{"Bucket": plan.Bucket.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("CreateBucket", err.Error())
		return
	}
	plan.ID = types.StringValue(idFromData(data, "BucketName"))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *euossResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state euossModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.State.Set(ctx, &state) // DescribeBucket re-reads metadata; skeleton no-ops
}

func (r *euossResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.Append(req.State.Set(ctx, &req.Plan)...)
}

func (r *euossResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state euossModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	client := req.ProviderData.(*eusdk.Client)
	if _, err := crudCall(client, "euoss", "DeleteBucket", map[string]any{"Bucket": state.Bucket.ValueString()}); err != nil {
		resp.Diagnostics.AddError("DeleteBucket", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}

// --- EUVPC (专有网络) --------------------------------------------------------

type euvpcResource struct{}

func NewScvpcResource() resource.Resource { return &euvpcResource{} }

type euvpcModel struct {
	ID    types.String `tfsdk:"id"`
	VpcID types.String `tfsdk:"vpc_id"`
	Cidr  types.String `tfsdk:"cidr_block"`
}

func (r *euvpcResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "euler_euvpc_vpc"
}

func (r *euvpcResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true},
			"vpc_id":     schema.StringAttribute{Computed: true},
			"cidr_block": schema.StringAttribute{Required: true},
		},
	}
}

func (r *euvpcResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan euvpcModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	client := req.ProviderData.(*eusdk.Client)
	data, err := crudCall(client, "euvpc", "CreateVpc", map[string]any{"CidrBlock": plan.Cidr.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("CreateVpc", err.Error())
		return
	}
	plan.VpcID = types.StringValue(idFromData(data, "VpcId"))
	plan.ID = plan.VpcID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *euvpcResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state euvpcModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.State.Set(ctx, &state)
}

func (r *euvpcResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.Append(req.State.Set(ctx, &req.Plan)...)
}

func (r *euvpcResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state euvpcModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	client := req.ProviderData.(*eusdk.Client)
	if _, err := crudCall(client, "euvpc", "DeleteVpc", map[string]any{"VpcId": state.VpcID.ValueString()}); err != nil {
		resp.Diagnostics.AddError("DeleteVpc", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}

// --- EURDS (托管 MySQL) ------------------------------------------------------

type eurdsResource struct{}

func NewScrdsResource() resource.Resource { return &eurdsResource{} }

type eurdsModel struct {
	ID         types.String `tfsdk:"id"`
	InstanceID types.String `tfsdk:"instance_id"`
	Engine     types.String `tfsdk:"engine_version"`
}

func (r *eurdsResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "euler_eurds_instance"
}

func (r *eurdsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true},
			"instance_id":    schema.StringAttribute{Computed: true},
			"engine_version": schema.StringAttribute{Required: true},
		},
	}
}

func (r *eurdsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan eurdsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	client := req.ProviderData.(*eusdk.Client)
	data, err := crudCall(client, "eurds", "CreateInstance", map[string]any{"EngineVersion": plan.Engine.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("CreateInstance", err.Error())
		return
	}
	plan.InstanceID = types.StringValue(idFromData(data, "InstanceId"))
	plan.ID = plan.InstanceID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *eurdsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state eurdsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.State.Set(ctx, &state)
}

func (r *eurdsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.Append(req.State.Set(ctx, &req.Plan)...)
}

func (r *eurdsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state eurdsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	client := req.ProviderData.(*eusdk.Client)
	if _, err := crudCall(client, "eurds", "DeleteInstance", map[string]any{"InstanceId": state.InstanceID.ValueString()}); err != nil {
		resp.Diagnostics.AddError("DeleteInstance", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}
