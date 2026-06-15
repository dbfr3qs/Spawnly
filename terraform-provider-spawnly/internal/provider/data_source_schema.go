package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/spawnly/terraform-provider-spawnly/internal/client"
)

var (
	_ datasource.DataSource              = (*schemaDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*schemaDataSource)(nil)
)

// NewSchemaDataSource is the data-source factory registered with the provider.
func NewSchemaDataSource() datasource.DataSource {
	return &schemaDataSource{}
}

type schemaDataSource struct {
	client *client.Client
}

// schemaModel mirrors the registry's active SpiceDB schema (GET /v1/schema). All
// fields are read-only.
type schemaModel struct {
	Schema  types.String `tfsdk:"schema"`
	Version types.String `tfsdk:"version"`
	Source  types.String `tfsdk:"source"`
}

func (d *schemaDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_schema"
}

func (d *schemaDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("expected *client.Client, got %T", req.ProviderData))
		return
	}
	d.client = c
}

func (d *schemaDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The registry's active SpiceDB authorization schema.",
		Attributes: map[string]schema.Attribute{
			"schema": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The full SpiceDB schema text the registry is currently serving.",
			},
			"version": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Version identifier of the active schema.",
			},
			"source": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Where the active schema was loaded from.",
			},
		},
	}
}

func (d *schemaDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	s, err := d.client.GetSchema(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read schema", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, schemaModel{
		Schema:  types.StringValue(s.Schema),
		Version: types.StringValue(s.Version),
		Source:  types.StringValue(s.Source),
	})...)
}
