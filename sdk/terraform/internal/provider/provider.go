// Package provider wires the platform OpenAPI surface into Terraform.
//
// The provider is thin by design: its only job is to take provider config
// (endpoint + AK/SK from env) into an *scsdk.Client, and to declare the product
// resources. All product logic is "call the signed OpenAPI action" — the same
// shape the console BFF and the SDKs use. There is deliberately no provider
// secret-handling or retry logic beyond what scsdk already provides.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/starcloud/sc-platform/scsdk"
)

// scProvider is the provider implementation.
type scProvider struct{}

// New returns the provider factory (providerserver.Serve entry).
func New() provider.Provider { return &scProvider{} }

// scProviderModel is the provider configuration surface.
type scProviderModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Region   types.String `tfsdk:"region"`
	// AK/SK are NOT configurable in HCL — they come from the environment
	// (SC_ACCESS_KEY / SC_SECRET_KEY), matching the SDK convention. Secrets in
	// HCL state would leak them; env vars keep them out of the plan file.
}

// Metadata names the provider.
func (p *scProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "starcloud"
}

// Schema declares the provider's configurable attributes.
func (p *scProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:    true,
				Description: "OpenAPI endpoint. Defaults to the regional endpoint derived from region.",
			},
			"region": schema.StringAttribute{
				Optional:    true,
				Description: "Default region for resources (cn-north-1).",
			},
		},
	}
}

// Configure builds the signed client from the provider config + env credentials.
func (p *scProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg scProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ak := os.Getenv("SC_ACCESS_KEY")
	sk := os.Getenv("SC_SECRET_KEY")
	if ak == "" || sk == "" {
		resp.Diagnostics.AddError(
			"Missing credentials",
			"SC_ACCESS_KEY and SC_SECRET_KEY must be set in the environment (the provider never stores secrets in state).",
		)
		return
	}

	region := "cn-north-1"
	if !cfg.Region.IsNull() && cfg.Region.ValueString() != "" {
		region = cfg.Region.ValueString()
	}

	client := scsdk.New(scsdk.Config{
		AK:       ak,
		SK:       sk,
		Endpoint: cfg.Endpoint.ValueString(),
		Region:   region,
	})
	resp.DataSourceData = client
	resp.ResourceData = client
}

// DataSources declares the provider's data sources (phase-3 MVP: none yet —
// the acceptance criterion is resource coverage; data sources are additive).
func (p *scProvider) DataSources(_ context.Context) []func() datasource.DataSource { return nil }

// Resources declares the product resources (C3: 覆盖核心产品).
func (p *scProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewScecsResource,
		NewScossResource,
		NewScvpcResource,
		NewScrdsResource,
	}
}
