package provider

import (
	"context"
	"errors"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// cloudModel is the Terraform state for a cloud.
type cloudModel struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Driver   types.String `tfsdk:"driver"`
	Enabled  types.Bool   `tfsdk:"enabled"`
	Settings types.Map    `tfsdk:"settings"`
}

type cloudResource struct{ client *Client }

// NewCloudResource returns the runnerforge_cloud resource.
func NewCloudResource() resource.Resource { return &cloudResource{} }

func (r *cloudResource) Metadata(
	_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_cloud"
}

func (r *cloudResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A provider account runnerforge creates machines on.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "What pools refer to this account by.",
				Required:            true,
			},
			"driver": schema.StringAttribute{
				MarkdownDescription: "`openstack` for OVHcloud and any other OpenStack, " +
					"or `docker` for local containers. Changing it replaces the cloud, " +
					"because its settings mean something different to each driver.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "A disabled cloud provisions nothing, but is still " +
					"swept by the reaper — disabling it must not strand running machines.",
				Optional: true, Computed: true,
				Default: booldefault.StaticBool(true),
			},
			"settings": schema.MapAttribute{
				MarkdownDescription: "Driver settings. Which keys apply depends on the " +
					"driver; the deployment's UI lists them, and its export button " +
					"renders a working block. Values the driver declares as secret are " +
					"stored encrypted and never read back, so they will not appear in " +
					"plans once set.",
				ElementType: types.StringType,
				Optional:    true,
				Sensitive:   true,
			},
		},
	}
}

func (r *cloudResource) Configure(
	_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			"The provider did not supply a runnerforge client. This is a bug in the provider.")
		return
	}
	r.client = client
}

func (r *cloudResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan cloudModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	settings, diags := mapToAny(ctx, plan.Settings)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	enabled := plan.Enabled.ValueBool()
	out, err := r.client.CreateCloud(ctx, Cloud{
		Name: plan.Name.ValueString(), Driver: plan.Driver.ValueString(),
		Enabled: &enabled, Settings: settings,
	})
	if err != nil {
		resp.Diagnostics.AddError("Could not create the cloud", err.Error())
		return
	}
	plan.ID = types.StringValue(strconv.FormatInt(out.ID, 10))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *cloudResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state cloudModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	numeric, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.State.RemoveResource(ctx)
		return
	}
	out, err := r.client.GetCloud(ctx, numeric)
	if errors.Is(err, ErrNotFound) {
		// Deleted outside Terraform: drop it from state so the next plan
		// proposes recreating it, rather than failing forever.
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Could not read the cloud", err.Error())
		return
	}
	state.Name = types.StringValue(out.Name)
	state.Driver = types.StringValue(out.Driver)
	if out.Enabled != nil {
		state.Enabled = types.BoolValue(*out.Enabled)
	}
	// Settings are deliberately not refreshed from the API: secrets are never
	// returned, so overwriting state with what came back would show a
	// permanent diff proposing to erase every credential.
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *cloudResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan cloudModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	numeric, err := strconv.ParseInt(plan.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Bad resource id", err.Error())
		return
	}
	settings, diags := mapToAny(ctx, plan.Settings)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	enabled := plan.Enabled.ValueBool()
	if _, err := r.client.UpdateCloud(ctx, numeric, Cloud{
		Name: plan.Name.ValueString(), Driver: plan.Driver.ValueString(),
		Enabled: &enabled, Settings: settings,
	}); err != nil {
		resp.Diagnostics.AddError("Could not update the cloud", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *cloudResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state cloudModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	numeric, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		return
	}
	if err := r.client.DeleteCloud(ctx, numeric); err != nil && !errors.Is(err, ErrNotFound) {
		resp.Diagnostics.AddError("Could not delete the cloud", err.Error())
	}
}
