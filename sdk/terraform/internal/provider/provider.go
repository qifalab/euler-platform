// Package provider wires the platform OpenAPI surface into Terraform.
//
// The provider is thin by design: its only job is to take provider config
// (endpoint + region, AK/SK from env) into a signed client (pkg-go/eusdk), and
// to declare the product resources. All product logic is "call the signed
// OpenAPI Action" against the svc-openapi facade — the same shape the console
// BFF and the SDKs use. There is deliberately no provider secret handling.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/qifalab/euler-platform/eusdk"
)

// euProvider is the provider implementation.
type euProvider struct{}

// New returns the provider factory (providerserver.Serve entry).
func New() provider.Provider { return &euProvider{} }

// euProviderModel is the provider configuration surface.
type euProviderModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Region   types.String `tfsdk:"region"`
	// AK/SK are NOT configurable in HCL — they come from the environment
	// (EULER_ACCESS_KEY_ID / EULER_SECRET_ACCESS_KEY, or EULER_ACCESS_KEY /
	// EULER_SECRET_KEY, or EULER_AK / EULER_SK; see credentialsFromEnv).
	// Secrets in HCL state would leak them; env vars keep them out of the plan
	// file.
}

// Credential environment variables, in precedence order. The platform's tools
// grew three spellings (the SDKs' EULER_ACCESS_KEY_ID, this provider's
// EULER_ACCESS_KEY, the short form used by svc-iam's e2e tool), so each is
// accepted and a user can export one pair for all of them. The names and their
// order are the ones eusdk and the Python SDK (cloudsdk ENV_*_ALIASES) use, so
// a shell exporting more than one spelling resolves to the same key everywhere.
var (
	envAccessKeyNames = []string{"EULER_ACCESS_KEY_ID", "EULER_ACCESS_KEY", "EULER_AK"}
	envSecretKeyNames = []string{"EULER_SECRET_ACCESS_KEY", "EULER_SECRET_KEY", "EULER_SK"}
)

// credentialsFromEnv returns the first non-empty AK and SK from the accepted
// environment variable names.
func credentialsFromEnv(getenv func(string) string) (ak, sk string) {
	first := func(names []string) string {
		for _, name := range names {
			if v := getenv(name); v != "" {
				return v
			}
		}
		return ""
	}
	return first(envAccessKeyNames), first(envSecretKeyNames)
}

// Metadata names the provider.
func (p *euProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "euler"
}

// Schema declares the provider's configurable attributes.
func (p *euProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				Description: "OpenAPI endpoint override (scheme://host[:port][/prefix]) used for every product. " +
					"Defaults to the product API hosts {product}.api.euler.emoera.com.",
			},
			"region": schema.StringAttribute{
				Optional:    true,
				Description: "Signing region and the default region of resources (cn-north-1).",
			},
		},
	}
}

// Configure builds the signed client from the provider config + env credentials.
func (p *euProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg euProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ak, sk := credentialsFromEnv(os.Getenv)
	if ak == "" || sk == "" {
		resp.Diagnostics.AddError(
			"Missing credentials",
			"EULER_ACCESS_KEY_ID and EULER_SECRET_ACCESS_KEY (or EULER_ACCESS_KEY/EULER_SECRET_KEY, or EULER_AK/EULER_SK) must be set in the environment (the provider never stores secrets in state).",
		)
		return
	}

	region := "cn-north-1"
	if !cfg.Region.IsNull() && cfg.Region.ValueString() != "" {
		region = cfg.Region.ValueString()
	}

	client := newAPIClient(eusdk.Config{
		AK:       ak,
		SK:       sk,
		Endpoint: cfg.Endpoint.ValueString(),
		Region:   region,
	})
	resp.DataSourceData = client
	resp.ResourceData = client
}

// DataSources declares the provider's data sources (none yet — the acceptance
// criterion is resource coverage; data sources are additive).
func (p *euProvider) DataSources(_ context.Context) []func() datasource.DataSource { return nil }

// Resources declares the product resources (C3: 覆盖核心产品).
func (p *euProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewScecsResource,
		NewScossResource,
		NewScvpcResource,
		NewScrdsResource,
		NewScbsResource,
		NewSceipResource,
	}
}
