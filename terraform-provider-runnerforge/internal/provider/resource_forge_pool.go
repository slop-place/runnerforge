package provider

import (
	"context"
	"errors"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type forgeModel struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Kind     types.String `tfsdk:"kind"`
	Enabled  types.Bool   `tfsdk:"enabled"`
	Settings types.Map    `tfsdk:"settings"`
}

type forgeResource struct{ client *Client }

// NewForgeResource returns the runnerforge_forge resource.
func NewForgeResource() resource.Resource { return &forgeResource{} }

func (r *forgeResource) Metadata(
	_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_forge"
}

func (r *forgeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A connection to GitHub, GitLab or Forgejo, scoped to a " +
			"repository, organisation or group. Runners are only ever created inside " +
			"that scope.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{Required: true},
			"kind": schema.StringAttribute{
				MarkdownDescription: "`github`, `gitlab` or `forgejo`. Changing it " +
					"replaces the connection, since the settings mean different things.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"enabled": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
			},
			"settings": schema.MapAttribute{
				MarkdownDescription: "Connection settings. Which keys apply depends on " +
					"the forge; the deployment's export button renders a working block. " +
					"Secrets are stored encrypted and never read back.",
				ElementType: types.StringType, Optional: true, Sensitive: true,
			},
		},
	}
}

func (r *forgeResource) Configure(
	_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse,
) {
	r.client = clientFrom(req, resp)
}

func (r *forgeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan forgeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in, ok := r.build(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	out, err := r.client.CreateForge(ctx, in)
	if err != nil {
		resp.Diagnostics.AddError("Could not create the forge connection", err.Error())
		return
	}
	plan.ID = types.StringValue(strconv.FormatInt(out.ID, 10))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *forgeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state forgeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	numeric, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.State.RemoveResource(ctx)
		return
	}
	out, err := r.client.GetForge(ctx, numeric)
	if errors.Is(err, ErrNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Could not read the forge connection", err.Error())
		return
	}
	state.Name = types.StringValue(out.Name)
	state.Kind = types.StringValue(out.Kind)
	if out.Enabled != nil {
		state.Enabled = types.BoolValue(*out.Enabled)
	}
	// Settings are not refreshed: secrets never come back, so writing what did
	// come back into state would propose erasing every credential on next plan.
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *forgeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan forgeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	numeric, err := strconv.ParseInt(plan.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Bad resource id", err.Error())
		return
	}
	in, ok := r.build(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	if _, err := r.client.UpdateForge(ctx, numeric, in); err != nil {
		resp.Diagnostics.AddError("Could not update the forge connection", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *forgeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state forgeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if numeric, err := strconv.ParseInt(state.ID.ValueString(), 10, 64); err == nil {
		if err := r.client.DeleteForge(ctx, numeric); err != nil && !errors.Is(err, ErrNotFound) {
			resp.Diagnostics.AddError("Could not delete the forge connection", err.Error())
		}
	}
}

// ---- pools ----

// Pool defaults, matching what the deployment's own forms offer.
const (
	defaultMaxInstances   = 5
	defaultJobTimeoutSec  = 3600
	defaultMaxLifetimeSec = 7200
)

type poolModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Enabled        types.Bool   `tfsdk:"enabled"`
	ForgeID        types.String `tfsdk:"forge_id"`
	CloudID        types.String `tfsdk:"cloud_id"`
	SizeID         types.String `tfsdk:"size_id"`
	ImageID        types.String `tfsdk:"image_id"`
	Labels         types.List   `tfsdk:"labels"`
	MinIdle        types.Int64  `tfsdk:"min_idle"`
	MaxInstances   types.Int64  `tfsdk:"max_instances"`
	JobTimeoutSec  types.Int64  `tfsdk:"job_timeout_sec"`
	MaxLifetimeSec types.Int64  `tfsdk:"max_lifetime_sec"`
	ContainerImage types.String `tfsdk:"container_image"`
	PublicIPv4     types.Bool   `tfsdk:"public_ipv4"`
	AllowSSHFrom   types.List   `tfsdk:"allow_ssh_from"`
}

type poolResource struct{ client *Client }

// NewPoolResource returns the runnerforge_pool resource.
func NewPoolResource() resource.Resource { return &poolResource{} }

func (r *poolResource) Metadata(
	_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_pool"
}

func (r *poolResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A set of interchangeable runners: one forge, one cloud, " +
			"one machine shape. Jobs whose labels this pool covers get a machine of " +
			"that shape.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{Required: true},
			"enabled": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
			},
			"forge_id": schema.StringAttribute{Required: true},
			"cloud_id": schema.StringAttribute{Required: true},
			"size_id":  schema.StringAttribute{Required: true},
			"image_id": schema.StringAttribute{Optional: true},
			"labels": schema.ListAttribute{
				MarkdownDescription: "A job is served by this pool when the pool offers " +
					"every label the job asks for.",
				ElementType: types.StringType, Required: true,
			},
			"min_idle": schema.Int64Attribute{
				MarkdownDescription: "Warm machines kept waiting. Costs money, buys latency.",
				Optional:            true, Computed: true, Default: int64default.StaticInt64(0),
			},
			"max_instances": schema.Int64Attribute{
				MarkdownDescription: "A safety ceiling as much as a scaling knob: keep it " +
					"at or below the cloud account's quota.",
				Optional: true, Computed: true, Default: int64default.StaticInt64(defaultMaxInstances),
			},
			"job_timeout_sec": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(defaultJobTimeoutSec),
			},
			"max_lifetime_sec": schema.Int64Attribute{
				MarkdownDescription: "The reaper's hard ceiling. Must exceed the job " +
					"timeout, or machines are destroyed mid-job.",
				Optional: true, Computed: true, Default: int64default.StaticInt64(defaultMaxLifetimeSec),
			},
			"container_image": schema.StringAttribute{Optional: true},
			"public_ipv4": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
			},
			"allow_ssh_from": schema.ListAttribute{
				MarkdownDescription: "CIDRs allowed to reach port 22. Empty means no " +
					"inbound SSH at all, which is right for every forge except the " +
					"GitLab docker-autoscaler path.",
				ElementType: types.StringType, Optional: true,
			},
		},
	}
}

func (r *poolResource) Configure(
	_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse,
) {
	r.client = clientFrom(req, resp)
}

func (r *poolResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan poolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in, ok := r.build(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	out, err := r.client.CreatePool(ctx, in)
	if err != nil {
		resp.Diagnostics.AddError("Could not create the pool", err.Error())
		return
	}
	plan.ID = types.StringValue(strconv.FormatInt(out.ID, 10))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *poolResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state poolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	numeric, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.State.RemoveResource(ctx)
		return
	}
	out, err := r.client.GetPool(ctx, numeric)
	if errors.Is(err, ErrNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Could not read the pool", err.Error())
		return
	}
	state.Name = types.StringValue(out.Name)
	if out.Enabled != nil {
		state.Enabled = types.BoolValue(*out.Enabled)
	}
	state.MinIdle = types.Int64Value(out.MinIdle)
	state.MaxInstances = types.Int64Value(out.MaxInstances)
	state.JobTimeoutSec = types.Int64Value(out.JobTimeoutSec)
	state.MaxLifetimeSec = types.Int64Value(out.MaxLifetimeSec)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *poolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan poolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	numeric, err := strconv.ParseInt(plan.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Bad resource id", err.Error())
		return
	}
	in, ok := r.build(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	if _, err := r.client.UpdatePool(ctx, numeric, in); err != nil {
		resp.Diagnostics.AddError("Could not update the pool", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *poolResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state poolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if numeric, err := strconv.ParseInt(state.ID.ValueString(), 10, 64); err == nil {
		if err := r.client.DeletePool(ctx, numeric); err != nil && !errors.Is(err, ErrNotFound) {
			resp.Diagnostics.AddError("Could not delete the pool", err.Error())
		}
	}
}

func (r *forgeResource) build(ctx context.Context, plan forgeModel, diags *diag.Diagnostics) (Forge, bool) {
	settings, d := mapToAny(ctx, plan.Settings)
	diags.Append(d...)
	if d.HasError() {
		return Forge{}, false
	}
	enabled := plan.Enabled.ValueBool()
	return Forge{
		Name: plan.Name.ValueString(), Kind: plan.Kind.ValueString(),
		Enabled: &enabled, Settings: settings,
	}, true
}

func (r *poolResource) build(ctx context.Context, plan poolModel, diags *diag.Diagnostics) (Pool, bool) {
	labels, d := listToStrings(ctx, plan.Labels)
	diags.Append(d...)
	ssh, d2 := listToStrings(ctx, plan.AllowSSHFrom)
	diags.Append(d2...)
	if diags.HasError() {
		return Pool{}, false
	}

	ref := func(name string, v types.String) (int64, bool) {
		n, err := strconv.ParseInt(v.ValueString(), 10, 64)
		if err != nil {
			diags.AddError("Bad "+name, err.Error())
			return 0, false
		}
		return n, true
	}
	forgeID, ok1 := ref("forge_id", plan.ForgeID)
	cloudID, ok2 := ref("cloud_id", plan.CloudID)
	sizeID, ok3 := ref("size_id", plan.SizeID)
	if !ok1 || !ok2 || !ok3 {
		return Pool{}, false
	}

	var imageID *int64
	if !plan.ImageID.IsNull() && plan.ImageID.ValueString() != "" {
		n, err := strconv.ParseInt(plan.ImageID.ValueString(), 10, 64)
		if err != nil {
			diags.AddError("Bad image_id", err.Error())
			return Pool{}, false
		}
		imageID = &n
	}

	enabled := plan.Enabled.ValueBool()
	public := plan.PublicIPv4.ValueBool()
	return Pool{
		Name: plan.Name.ValueString(), Enabled: &enabled,
		ForgeID: forgeID, CloudID: cloudID, SizeID: sizeID, ImageID: imageID,
		Labels: labels, MinIdle: plan.MinIdle.ValueInt64(),
		MaxInstances:   plan.MaxInstances.ValueInt64(),
		JobTimeoutSec:  plan.JobTimeoutSec.ValueInt64(),
		MaxLifetimeSec: plan.MaxLifetimeSec.ValueInt64(),
		ContainerImage: plan.ContainerImage.ValueString(),
		PublicIPv4:     &public, AllowSSHFrom: ssh,
	}, true
}
