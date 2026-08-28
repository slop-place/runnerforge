package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Provider is the runnerforge Terraform provider.
type Provider struct {
	version string
}

// New returns a provider constructor for the given version.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &Provider{version: version} }
}

// Metadata names the provider and reports its version.
func (p *Provider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "runnerforge"
	resp.Version = p.version
}

// providerModel is the provider block's configuration.
type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

// Schema describes the provider block.
func (p *Provider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage a runnerforge deployment: the clouds it provisions " +
			"machines on, the forges it serves, and the pools that connect them.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				MarkdownDescription: "Base URL of the runnerforge deployment, for example " +
					"`https://runnerforge.example.com`. Falls back to `RUNNERFORGE_ENDPOINT`.",
				Optional: true,
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "An API token from the deployment's `api_tokens` config. " +
					"Falls back to `RUNNERFORGE_TOKEN`. Prefer the environment variable: a token " +
					"in a `.tf` file ends up in version control.",
				Optional:  true,
				Sensitive: true,
			},
		},
	}
}

// Configure builds the shared API client every resource uses.
func (p *Provider) Configure(
	ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse,
) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := cfg.Endpoint.ValueString()
	if endpoint == "" {
		endpoint = os.Getenv("RUNNERFORGE_ENDPOINT")
	}
	token := cfg.Token.ValueString()
	if token == "" {
		token = os.Getenv("RUNNERFORGE_TOKEN")
	}

	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(
			path("endpoint"), "Missing endpoint",
			"Set endpoint in the provider block, or RUNNERFORGE_ENDPOINT in the environment.")
	}
	if token == "" {
		resp.Diagnostics.AddAttributeError(
			path("token"), "Missing API token",
			"Set token in the provider block, or RUNNERFORGE_TOKEN in the environment. "+
				"Tokens come from the deployment's api_tokens config.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	client := NewClient(endpoint, token)
	resp.DataSourceData = client
	resp.ResourceData = client
}

// Resources lists the resource types this provider manages.
func (p *Provider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewCloudResource,
		NewSizeResource,
		NewImageResource,
		NewForgeResource,
		NewPoolResource,
	}
}

// DataSources lists the data sources. There are none yet: everything
// runnerforge exposes is something you manage rather than something you read.
func (p *Provider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}

// Functions lists provider-defined functions. There are none.
func (p *Provider) Functions(_ context.Context) []func() function.Function {
	return nil
}
