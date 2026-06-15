package provider

import (
	"context"
	"fmt"
	"reflect"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/spawnly/terraform-provider-spawnly/internal/client"
)

var (
	_ resource.Resource                = (*templateResource)(nil)
	_ resource.ResourceWithConfigure   = (*templateResource)(nil)
	_ resource.ResourceWithImportState = (*templateResource)(nil)
)

// NewTemplateResource is the resource factory registered with the provider.
func NewTemplateResource() resource.Resource {
	return &templateResource{}
}

type templateResource struct {
	client *client.Client
}

// templateModel mirrors the spawnly_agent_template HCL. Nested blocks are
// pointers/slices so an omitted block maps to null rather than an empty object.
type templateModel struct {
	AgentType      types.String     `tfsdk:"agent_type"`
	Version        types.String     `tfsdk:"version"`
	Status         types.String     `tfsdk:"status"`
	RequiresTenant types.Bool       `tfsdk:"requires_tenant"`
	OAuthScopes    []types.String   `tfsdk:"oauth_scopes"`
	Meta           *metaModel       `tfsdk:"meta"`
	RuntimeSpec    *runtimeModel    `tfsdk:"runtime_spec"`
	AuthZ          *authzModel      `tfsdk:"authz_template"`
	Delegation     *delegationModel `tfsdk:"delegation"`
}

type metaModel struct {
	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`
}

type runtimeModel struct {
	Image        types.String            `tfsdk:"image"`
	Lifecycle    types.String            `tfsdk:"lifecycle"`
	SupportsChat types.Bool              `tfsdk:"supports_chat"`
	EnvDefaults  map[string]types.String `tfsdk:"env_defaults"`
	Resources    *resourcesModel         `tfsdk:"resources"`
}

type resourcesModel struct {
	CPULimits    types.String `tfsdk:"cpu_limits"`
	MemoryLimits types.String `tfsdk:"memory_limits"`
}

type authzModel struct {
	Relations []relationModel `tfsdk:"spicedb_relation"`
}

type relationModel struct {
	Resource types.String `tfsdk:"resource"`
	Relation types.String `tfsdk:"relation"`
	Subject  types.String `tfsdk:"subject"`
}

type delegationModel struct {
	AllowedChildTypes []types.String              `tfsdk:"allowed_child_types"`
	GrantableScopes   []types.String              `tfsdk:"grantable_scopes"`
	MaxDepth          types.Int64                 `tfsdk:"max_depth"`
	ChildPolicies     map[string]childPolicyModel `tfsdk:"child_policies"`
}

type childPolicyModel struct {
	RequireUserConsent types.Bool   `tfsdk:"require_user_consent"`
	ConsentTTL         types.String `tfsdk:"consent_ttl"`
}

func (r *templateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent_template"
}

func (r *templateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("expected *client.Client, got %T", req.ProviderData))
		return
	}
	r.client = c
}

func (r *templateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An agent template in the Spawnly registry — the spawnable definition of an agent type.",
		Attributes: map[string]schema.Attribute{
			"agent_type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Unique agent type identifier. Changing it replaces the template.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"version": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Template version string (opaque to the registry).",
			},
			"status": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("active"),
				Validators:          []validator.String{stringvalidator.OneOf("active", "disabled")},
				MarkdownDescription: "Template status: `active` (spawnable) or `disabled` (hidden, unspawnable). Defaults to `active`.",
			},
			"requires_tenant": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "When true, the orchestrator rejects a tenant-less spawn of this type. Set on templates whose authz relations reference a tenant.",
			},
			"oauth_scopes": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "OAuth2 scopes this agent type requests at runtime (and the set a stored consent is matched against).",
			},
		},
		Blocks: map[string]schema.Block{
			"meta": schema.SingleNestedBlock{
				MarkdownDescription: "Human-facing metadata.",
				Attributes: map[string]schema.Attribute{
					"display_name": schema.StringAttribute{Optional: true, MarkdownDescription: "Display name shown in the dashboard."},
					"description":  schema.StringAttribute{Optional: true, MarkdownDescription: "Short description of the agent type."},
				},
			},
			"runtime_spec": schema.SingleNestedBlock{
				MarkdownDescription: "How the agent pod is built and run.",
				Attributes: map[string]schema.Attribute{
					"image":         schema.StringAttribute{Optional: true, MarkdownDescription: "Container image for the agent."},
					"lifecycle":     schema.StringAttribute{Optional: true, MarkdownDescription: "`short-lived` or `long-lived`."},
					"supports_chat": schema.BoolAttribute{Optional: true, MarkdownDescription: "Whether the agent serves the chat endpoint (long-lived only)."},
					"env_defaults": schema.MapAttribute{
						Optional:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "Default environment variables injected into the agent pod.",
					},
				},
				Blocks: map[string]schema.Block{
					"resources": schema.SingleNestedBlock{
						MarkdownDescription: "Pod resource limits.",
						Attributes: map[string]schema.Attribute{
							"cpu_limits":    schema.StringAttribute{Optional: true, MarkdownDescription: "CPU limit (e.g. `500m`)."},
							"memory_limits": schema.StringAttribute{Optional: true, MarkdownDescription: "Memory limit (e.g. `256Mi`)."},
						},
					},
				},
			},
			"authz_template": schema.SingleNestedBlock{
				MarkdownDescription: "SpiceDB relations written when an agent of this type registers. Use `{{tenant_id}}` / `{{agent_id}}` placeholders.",
				Blocks: map[string]schema.Block{
					"spicedb_relation": schema.ListNestedBlock{
						MarkdownDescription: "One SpiceDB relationship template.",
						NestedObject: schema.NestedBlockObject{
							Attributes: map[string]schema.Attribute{
								"resource": schema.StringAttribute{Required: true, MarkdownDescription: "Resource, e.g. `tenant:{{tenant_id}}`."},
								"relation": schema.StringAttribute{Required: true, MarkdownDescription: "Relation, e.g. `agent`."},
								"subject":  schema.StringAttribute{Required: true, MarkdownDescription: "Subject, e.g. `agent:{{agent_id}}`."},
							},
						},
					},
				},
			},
			"delegation": schema.SingleNestedBlock{
				MarkdownDescription: "What child types this type may spawn, and how those spawns are gated.",
				Attributes: map[string]schema.Attribute{
					"allowed_child_types": schema.ListAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Child agent types this type may spawn (deny-by-default)."},
					"grantable_scopes":    schema.ListAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Scopes this type may delegate to children."},
					"max_depth": schema.Int64Attribute{
						Optional:            true,
						Computed:            true,
						Default:             int64default.StaticInt64(0),
						MarkdownDescription: "Maximum spawn-chain depth below this type.",
					},
					"child_policies": schema.MapNestedAttribute{
						Optional:            true,
						MarkdownDescription: "Per-child-type spawn gating, keyed by child agent type. Only takes effect for a key also present in `allowed_child_types`.",
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"require_user_consent": schema.BoolAttribute{Optional: true, MarkdownDescription: "Gate this child spawn behind CIBA user consent."},
								"consent_ttl": schema.StringAttribute{
									Optional:            true,
									Validators:          []validator.String{goDurationValidator()},
									MarkdownDescription: "Go duration string (e.g. `720h`) a granted consent lasts. Empty means never expires.",
								},
							},
						},
					},
				},
			},
		},
	}
}

func (r *templateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan templateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Clobber guard: POST is an upsert, so creating a type that already exists
	// would silently overwrite a template Terraform doesn't track. Refuse, and
	// point the user at import instead.
	agentType := plan.AgentType.ValueString()
	if _, found, err := r.client.GetTemplate(ctx, agentType); err != nil {
		resp.Diagnostics.AddError("Failed to check for an existing template", err.Error())
		return
	} else if found {
		resp.Diagnostics.AddError(
			"Template already exists",
			fmt.Sprintf("A template with agent_type %q already exists in the registry. "+
				"Import it into Terraform state instead of recreating it:\n\n"+
				"  terraform import <resource address> %s", agentType, agentType),
		)
		return
	}
	if err := r.client.PutTemplate(ctx, toWire(plan)); err != nil {
		resp.Diagnostics.AddError("Failed to create template", err.Error())
		return
	}
	// State is the plan (with defaults already applied); no server read-back, so
	// server-side normalization can't introduce a spurious post-apply diff.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *templateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state templateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tmpl, found, err := r.client.GetTemplate(ctx, state.AgentType.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read template", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, fromWire(*tmpl))...)
}

func (r *templateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state templateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Status-only fast path: if the prior state and the plan differ in nothing
	// but status, PATCH the status instead of re-POSTing the whole document.
	// Comparing the wire forms (with status zeroed) is robust to null-vs-empty
	// representation noise that an in-model comparison would trip over.
	planWire, stateWire := toWire(plan), toWire(state)
	planWire.Status, stateWire.Status = "", ""
	if reflect.DeepEqual(planWire, stateWire) {
		if plan.Status.ValueString() != state.Status.ValueString() {
			if err := r.client.SetStatus(ctx, plan.AgentType.ValueString(), plan.Status.ValueString()); err != nil {
				resp.Diagnostics.AddError("Failed to update template status", err.Error())
				return
			}
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	// POST is a full-document upsert, so it carries both spec and status changes.
	if err := r.client.PutTemplate(ctx, toWire(plan)); err != nil {
		resp.Diagnostics.AddError("Failed to update template", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *templateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state templateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	agentType := state.AgentType.ValueString()
	// The registry refuses to delete a template that isn't disabled, so disable
	// first. SetStatus is idempotent on an already-disabled template.
	if err := r.client.SetStatus(ctx, agentType, "disabled"); err != nil {
		resp.Diagnostics.AddError("Failed to disable template before delete", err.Error())
		return
	}
	if err := r.client.DeleteTemplate(ctx, agentType); err != nil {
		resp.Diagnostics.AddError("Failed to delete template", err.Error())
		return
	}
}

func (r *templateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by agent_type; the subsequent Read hydrates the rest of state.
	resource.ImportStatePassthroughID(ctx, path.Root("agent_type"), req, resp)
}
