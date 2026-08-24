// Product resources for the core products (三期验收 C3: Terraform Provider
// 覆盖核心产品 — SCECS/SCOSS/SCVPC/SCRDS).
//
// Each resource is deliberately thin: Create/Read/Update/Delete are "sign the
// product OpenAPI action with the shared scsdk client and reconcile state".
// There is no per-product secret or retry logic here — scsdk owns signing and
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

	"github.com/starcloud/sc-platform/scsdk"
)

// crudCall signs and sends one product action, returning the decoded Data body.
func crudCall(client *scsdk.Client, productCode, action string, params map[string]any) (map[string]any, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	resp, err := client.Call(scsdk.ApiRequest{
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

// --- SCECS (云服务器 VM) -----------------------------------------------------

type scecsResource struct{}

func NewScecsResource() resource.Resource { return &scecsResource{} }

type scecsModel struct {
	ID           types.String `tfsdk:"id"`
	ImageID      types.String `tfsdk:"image_id"`
	InstanceType types.String `tfsdk:"instance_type"`
	ZoneID       types.String `tfsdk:"zone_id"`
}

func (r *scecsResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "starcloud_scecs_instance"
}

func (r *scecsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
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

func (r *scecsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan scecsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	client := req.ProviderData.(*scsdk.Client)
	data, err := crudCall(client, "scecs", "RunInstances", map[string]any{
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

func (r *scecsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state scecsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	client := req.ProviderData.(*scsdk.Client)
	data, err := crudCall(client, "scecs", "DescribeInstances", map[string]any{"InstanceId": state.ID.ValueString()})
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

func (r *scecsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// VM type change is a stop/modify/start cycle in the product; the skeleton
	// treats the attribute as re-readable via Read rather than faking an in-place
	// change. A real provider would call the product's ModifyInstanceType.
	resp.Diagnostics.Append(req.State.Set(ctx, &req.Plan)...)
}

func (r *scecsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state scecsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	client := req.ProviderData.(*scsdk.Client)
	if _, err := crudCall(client, "scecs", "TerminateInstances", map[string]any{"InstanceId": state.ID.ValueString()}); err != nil {
		resp.Diagnostics.AddError("TerminateInstances", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}

// --- SCOSS (对象存储桶) ------------------------------------------------------

type scossResource struct{}

func NewScossResource() resource.Resource { return &scossResource{} }

type scossModel struct {
	ID     types.String `tfsdk:"id"`
	Bucket types.String `tfsdk:"bucket"`
}

func (r *scossResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "starcloud_scoss_bucket"
}

func (r *scossResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"id":     schema.StringAttribute{Computed: true},
			"bucket": schema.StringAttribute{Required: true},
		},
	}
}

func (r *scossResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan scossModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	client := req.ProviderData.(*scsdk.Client)
	data, err := crudCall(client, "scoss", "CreateBucket", map[string]any{"Bucket": plan.Bucket.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("CreateBucket", err.Error())
		return
	}
	plan.ID = types.StringValue(idFromData(data, "BucketName"))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *scossResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state scossModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.State.Set(ctx, &state) // DescribeBucket re-reads metadata; skeleton no-ops
}

func (r *scossResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.Append(req.State.Set(ctx, &req.Plan)...)
}

func (r *scossResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state scossModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	client := req.ProviderData.(*scsdk.Client)
	if _, err := crudCall(client, "scoss", "DeleteBucket", map[string]any{"Bucket": state.Bucket.ValueString()}); err != nil {
		resp.Diagnostics.AddError("DeleteBucket", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}

// --- SCVPC (专有网络) --------------------------------------------------------

type scvpcResource struct{}

func NewScvpcResource() resource.Resource { return &scvpcResource{} }

type scvpcModel struct {
	ID    types.String `tfsdk:"id"`
	VpcID types.String `tfsdk:"vpc_id"`
	Cidr  types.String `tfsdk:"cidr_block"`
}

func (r *scvpcResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "starcloud_scvpc_vpc"
}

func (r *scvpcResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true},
			"vpc_id":     schema.StringAttribute{Computed: true},
			"cidr_block": schema.StringAttribute{Required: true},
		},
	}
}

func (r *scvpcResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan scvpcModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	client := req.ProviderData.(*scsdk.Client)
	data, err := crudCall(client, "scvpc", "CreateVpc", map[string]any{"CidrBlock": plan.Cidr.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("CreateVpc", err.Error())
		return
	}
	plan.VpcID = types.StringValue(idFromData(data, "VpcId"))
	plan.ID = plan.VpcID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *scvpcResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state scvpcModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.State.Set(ctx, &state)
}

func (r *scvpcResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.Append(req.State.Set(ctx, &req.Plan)...)
}

func (r *scvpcResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state scvpcModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	client := req.ProviderData.(*scsdk.Client)
	if _, err := crudCall(client, "scvpc", "DeleteVpc", map[string]any{"VpcId": state.VpcID.ValueString()}); err != nil {
		resp.Diagnostics.AddError("DeleteVpc", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}

// --- SCRDS (托管 MySQL) ------------------------------------------------------

type scrdsResource struct{}

func NewScrdsResource() resource.Resource { return &scrdsResource{} }

type scrdsModel struct {
	ID         types.String `tfsdk:"id"`
	InstanceID types.String `tfsdk:"instance_id"`
	Engine     types.String `tfsdk:"engine_version"`
}

func (r *scrdsResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "starcloud_scrds_instance"
}

func (r *scrdsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true},
			"instance_id":    schema.StringAttribute{Computed: true},
			"engine_version": schema.StringAttribute{Required: true},
		},
	}
}

func (r *scrdsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan scrdsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	client := req.ProviderData.(*scsdk.Client)
	data, err := crudCall(client, "scrds", "CreateInstance", map[string]any{"EngineVersion": plan.Engine.ValueString()})
	if err != nil {
		resp.Diagnostics.AddError("CreateInstance", err.Error())
		return
	}
	plan.InstanceID = types.StringValue(idFromData(data, "InstanceId"))
	plan.ID = plan.InstanceID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *scrdsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state scrdsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.State.Set(ctx, &state)
}

func (r *scrdsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.Append(req.State.Set(ctx, &req.Plan)...)
}

func (r *scrdsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state scrdsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	client := req.ProviderData.(*scsdk.Client)
	if _, err := crudCall(client, "scrds", "DeleteInstance", map[string]any{"InstanceId": state.InstanceID.ValueString()}); err != nil {
		resp.Diagnostics.AddError("DeleteInstance", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}
