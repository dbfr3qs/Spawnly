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
	_ datasource.DataSource              = (*templatesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*templatesDataSource)(nil)
)

// NewTemplatesDataSource is the data-source factory registered with the provider.
func NewTemplatesDataSource() datasource.DataSource {
	return &templatesDataSource{}
}

type templatesDataSource struct {
	client *client.Client
}

// templatesModel holds the catalog of spawnable agent type names.
type templatesModel struct {
	AgentTypes []types.String `tfsdk:"agent_types"`
}

func (d *templatesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent_templates"
}

func (d *templatesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *templatesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The catalog of spawnable agent type names registered in the Spawnly registry.",
		Attributes: map[string]schema.Attribute{
			"agent_types": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Names of the **active** (spawnable) agent types. The registry excludes disabled templates from this list, so it is a catalog view rather than a full inventory.",
			},
		},
	}
}

func (d *templatesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	agentTypes, err := d.client.ListTemplateTypes(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to list templates", err.Error())
		return
	}
	// Build a non-nil slice so an empty catalog serializes as [] rather than
	// null — a consumer iterating agent_types should get an empty list, not a
	// null that breaks length()/for_each.
	out := make([]types.String, 0, len(agentTypes))
	for _, t := range agentTypes {
		out = append(out, types.StringValue(t))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, templatesModel{AgentTypes: out})...)
}
