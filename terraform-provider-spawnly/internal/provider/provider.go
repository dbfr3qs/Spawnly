package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/spawnly/terraform-provider-spawnly/internal/client"
)

// Ensure the provider satisfies the framework interface.
var _ provider.Provider = (*spawnlyProvider)(nil)

type spawnlyProvider struct {
	version string
}

// New returns the provider constructor the server entry point serves.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &spawnlyProvider{version: version}
	}
}

// providerModel maps the provider configuration block.
type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

func (p *spawnlyProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "spawnly"
	resp.Version = p.version
}

func (p *spawnlyProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage Spawnly agent templates in the registry control plane.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Base URL of the registry control-plane API. May also be set via the `SPAWNLY_ENDPOINT` environment variable.",
			},
			"token": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Shared-secret bearer token for the control-plane API. May also be set via the `SPAWNLY_TOKEN` environment variable. Leave unset against a registry running open (`CONTROL_PLANE_AUTH=none`).",
			},
		},
	}
}

func (p *spawnlyProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Config value wins; otherwise fall back to the environment.
	endpoint := os.Getenv("SPAWNLY_ENDPOINT")
	if !cfg.Endpoint.IsNull() {
		endpoint = cfg.Endpoint.ValueString()
	}
	token := os.Getenv("SPAWNLY_TOKEN")
	if !cfg.Token.IsNull() {
		token = cfg.Token.ValueString()
	}

	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("endpoint"),
			"Missing registry endpoint",
			"Set the provider `endpoint` attribute or the SPAWNLY_ENDPOINT environment variable to the registry control-plane base URL.",
		)
		return
	}

	c := client.New(endpoint, token)
	resp.ResourceData = c
	resp.DataSourceData = c
}

func (p *spawnlyProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewTemplateResource,
	}
}

func (p *spawnlyProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	// Data sources are added in a later phase.
	return nil
}
