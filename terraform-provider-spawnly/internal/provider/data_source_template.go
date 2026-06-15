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
	_ datasource.DataSource              = (*templateDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*templateDataSource)(nil)
)

// NewTemplateDataSource is the data-source factory registered with the provider.
func NewTemplateDataSource() datasource.DataSource {
	return &templateDataSource{}
}

type templateDataSource struct {
	client *client.Client
}

func (d *templateDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent_template"
}

func (d *templateDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *templateDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a single agent template in the Spawnly registry by its agent type.",
		Attributes: map[string]schema.Attribute{
			"agent_type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Unique agent type identifier to look up.",
			},
			"version": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Template version string (opaque to the registry).",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Template status: `active` (spawnable) or `disabled` (hidden, unspawnable).",
			},
			"requires_tenant": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "When true, the orchestrator rejects a tenant-less spawn of this type.",
			},
			"oauth_scopes": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "OAuth2 scopes this agent type requests at runtime.",
			},
		},
		Blocks: map[string]schema.Block{
			"meta": schema.SingleNestedBlock{
				MarkdownDescription: "Human-facing metadata.",
				Attributes: map[string]schema.Attribute{
					"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Display name shown in the dashboard."},
					"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Short description of the agent type."},
				},
			},
			"runtime_spec": schema.SingleNestedBlock{
				MarkdownDescription: "How the agent pod is built and run.",
				Attributes: map[string]schema.Attribute{
					"image":         schema.StringAttribute{Computed: true, MarkdownDescription: "Container image for the agent."},
					"lifecycle":     schema.StringAttribute{Computed: true, MarkdownDescription: "`short-lived` or `long-lived`."},
					"supports_chat": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the agent serves the chat endpoint (long-lived only)."},
					"env_defaults": schema.MapAttribute{
						Computed:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "Default environment variables injected into the agent pod.",
					},
				},
				Blocks: map[string]schema.Block{
					"resources": schema.SingleNestedBlock{
						MarkdownDescription: "Pod resource limits.",
						Attributes: map[string]schema.Attribute{
							"cpu_limits":    schema.StringAttribute{Computed: true, MarkdownDescription: "CPU limit (e.g. `500m`)."},
							"memory_limits": schema.StringAttribute{Computed: true, MarkdownDescription: "Memory limit (e.g. `256Mi`)."},
						},
					},
				},
			},
			"authz_template": schema.SingleNestedBlock{
				MarkdownDescription: "SpiceDB relations written when an agent of this type registers.",
				Blocks: map[string]schema.Block{
					"spicedb_relation": schema.ListNestedBlock{
						MarkdownDescription: "One SpiceDB relationship template.",
						NestedObject: schema.NestedBlockObject{
							Attributes: map[string]schema.Attribute{
								"resource": schema.StringAttribute{Computed: true, MarkdownDescription: "Resource, e.g. `tenant:{{tenant_id}}`."},
								"relation": schema.StringAttribute{Computed: true, MarkdownDescription: "Relation, e.g. `agent`."},
								"subject":  schema.StringAttribute{Computed: true, MarkdownDescription: "Subject, e.g. `agent:{{agent_id}}`."},
							},
						},
					},
				},
			},
			"delegation": schema.SingleNestedBlock{
				MarkdownDescription: "What child types this type may spawn, and how those spawns are gated.",
				Attributes: map[string]schema.Attribute{
					"allowed_child_types": schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Child agent types this type may spawn."},
					"grantable_scopes":    schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Scopes this type may delegate to children."},
					"max_depth":           schema.Int64Attribute{Computed: true, MarkdownDescription: "Maximum spawn-chain depth below this type."},
					"child_policies": schema.MapNestedAttribute{
						Computed:            true,
						MarkdownDescription: "Per-child-type spawn gating, keyed by child agent type.",
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"require_user_consent": schema.BoolAttribute{Computed: true, MarkdownDescription: "Gate this child spawn behind CIBA user consent."},
								"consent_ttl":          schema.StringAttribute{Computed: true, MarkdownDescription: "Go duration string a granted consent lasts."},
							},
						},
					},
				},
			},
		},
	}
}

func (d *templateDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg templateModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	agentType := cfg.AgentType.ValueString()
	tmpl, found, err := d.client.GetTemplate(ctx, agentType)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read template", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddError("Template not found",
			fmt.Sprintf("no agent template with type %q exists in the registry", agentType))
		return
	}
	// fromWire shares the templateModel + tfsdk tags with the resource, so it maps
	// the fetched template straight into the data-source state. agent_type comes
	// back from the wire payload, matching the required input.
	resp.Diagnostics.Append(resp.State.Set(ctx, fromWire(*tmpl))...)
}
