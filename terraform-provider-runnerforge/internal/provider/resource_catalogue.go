package provider

import (
	"context"
	"errors"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// A cloud's size and image catalogues are what make a pool portable: a pool
// asks for "large" and each cloud says what that means. They are separate
// resources rather than nested blocks so they can be added to a cloud that
// something else manages.

type sizeModel struct {
	ID        types.String  `tfsdk:"id"`
	CloudID   types.String  `tfsdk:"cloud_id"`
	Name      types.String  `tfsdk:"name"`
	Spec      types.Map     `tfsdk:"spec"`
	VCPUs     types.Int64   `tfsdk:"vcpus"`
	MemoryMB  types.Int64   `tfsdk:"memory_mb"`
	DiskGB    types.Int64   `tfsdk:"disk_gb"`
	HourlyUSD types.Float64 `tfsdk:"hourly_usd"`
}

type sizeResource struct{ client *Client }

// NewSizeResource returns the runnerforge_size resource.
func NewSizeResource() resource.Resource { return &sizeResource{} }

func (r *sizeResource) Metadata(
	_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_size"
}

func (r *sizeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One entry in a cloud's instance-size catalogue. Pools ask " +
			"for a size by name, so `large` can be a `c3-8` on one provider and a " +
			"`cpx41` on another without the pool changing.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"cloud_id": schema.StringAttribute{
				MarkdownDescription: "The cloud this size belongs to.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "What pools ask for: `small`, `large`, `gpu`.",
				Required:            true,
			},
			"spec": schema.MapAttribute{
				MarkdownDescription: "What the name means to this driver, for example " +
					"`{ flavor = \"c3-8\" }` on OpenStack.",
				ElementType: types.StringType,
				Optional:    true,
			},
			"vcpus":      schema.Int64Attribute{MarkdownDescription: "Shown in the UI.", Optional: true},
			"memory_mb":  schema.Int64Attribute{MarkdownDescription: "Shown in the UI.", Optional: true},
			"disk_gb":    schema.Int64Attribute{MarkdownDescription: "Shown in the UI.", Optional: true},
			"hourly_usd": schema.Float64Attribute{MarkdownDescription: "Used for cost figures.", Optional: true},
		},
	}
}

func (r *sizeResource) Configure(
	_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse,
) {
	r.client = clientFrom(req, resp)
}

func (r *sizeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sizeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in, ok := r.build(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	out, err := r.client.CreateSize(ctx, in)
	if err != nil {
		resp.Diagnostics.AddError("Could not create the size", err.Error())
		return
	}
	plan.ID = types.StringValue(strconv.FormatInt(out.ID, 10))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *sizeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sizeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	numeric, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.State.RemoveResource(ctx)
		return
	}
	out, err := r.client.GetSize(ctx, numeric)
	if errors.Is(err, ErrNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Could not read the size", err.Error())
		return
	}
	state.Name = types.StringValue(out.Name)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *sizeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan sizeModel
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
	if _, err := r.client.UpdateSize(ctx, numeric, in); err != nil {
		resp.Diagnostics.AddError("Could not update the size", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *sizeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sizeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if numeric, err := strconv.ParseInt(state.ID.ValueString(), 10, 64); err == nil {
		if err := r.client.DeleteSize(ctx, numeric); err != nil && !errors.Is(err, ErrNotFound) {
			resp.Diagnostics.AddError("Could not delete the size", err.Error())
		}
	}
}

// ---- images ----

type imageModel struct {
	ID                 types.String `tfsdk:"id"`
	CloudID            types.String `tfsdk:"cloud_id"`
	Name               types.String `tfsdk:"name"`
	Spec               types.Map    `tfsdk:"spec"`
	Username           types.String `tfsdk:"username"`
	PreinstalledDocker types.Bool   `tfsdk:"preinstalled_docker"`
	Notes              types.String `tfsdk:"notes"`
}

type imageResource struct{ client *Client }

// NewImageResource returns the runnerforge_image resource.
func NewImageResource() resource.Resource { return &imageResource{} }

func (r *imageResource) Metadata(
	_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_image"
}

func (r *imageResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One entry in a cloud's image catalogue.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"cloud_id": schema.StringAttribute{
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "What pools ask for, such as `ci-base`.",
				Required:            true,
			},
			"spec": schema.MapAttribute{
				MarkdownDescription: "What the name means to this driver, for example " +
					"`{ id = \"22ef146b-...\" }` on OpenStack.",
				ElementType: types.StringType, Optional: true,
			},
			"username": schema.StringAttribute{
				MarkdownDescription: "The account cloud-init creates. Only the SSH path needs it.",
				Optional:            true,
			},
			"preinstalled_docker": schema.BoolAttribute{
				MarkdownDescription: "Lets the bootstrap skip a slow install.",
				Optional:            true,
			},
			"notes": schema.StringAttribute{Optional: true},
		},
	}
}

func (r *imageResource) Configure(
	_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse,
) {
	r.client = clientFrom(req, resp)
}

func (r *imageResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan imageModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in, ok := r.build(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	out, err := r.client.CreateImage(ctx, in)
	if err != nil {
		resp.Diagnostics.AddError("Could not create the image", err.Error())
		return
	}
	plan.ID = types.StringValue(strconv.FormatInt(out.ID, 10))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *imageResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state imageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	numeric, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.State.RemoveResource(ctx)
		return
	}
	out, err := r.client.GetImage(ctx, numeric)
	if errors.Is(err, ErrNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Could not read the image", err.Error())
		return
	}
	state.Name = types.StringValue(out.Name)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *imageResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan imageModel
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
	if _, err := r.client.UpdateImage(ctx, numeric, in); err != nil {
		resp.Diagnostics.AddError("Could not update the image", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *imageResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state imageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if numeric, err := strconv.ParseInt(state.ID.ValueString(), 10, 64); err == nil {
		if err := r.client.DeleteImage(ctx, numeric); err != nil && !errors.Is(err, ErrNotFound) {
			resp.Diagnostics.AddError("Could not delete the image", err.Error())
		}
	}
}

func (r *sizeResource) build(ctx context.Context, plan sizeModel, diags *diag.Diagnostics) (Size, bool) {
	spec, d := mapToAny(ctx, plan.Spec)
	diags.Append(d...)
	if d.HasError() {
		return Size{}, false
	}
	cloudID, err := strconv.ParseInt(plan.CloudID.ValueString(), 10, 64)
	if err != nil {
		diags.AddError("Bad cloud_id", err.Error())
		return Size{}, false
	}
	return Size{
		CloudID: cloudID, Name: plan.Name.ValueString(), Spec: spec,
		VCPUs: plan.VCPUs.ValueInt64(), MemoryMB: plan.MemoryMB.ValueInt64(),
		DiskGB: plan.DiskGB.ValueInt64(), HourlyUSD: plan.HourlyUSD.ValueFloat64(),
	}, true
}

func (r *imageResource) build(ctx context.Context, plan imageModel, diags *diag.Diagnostics) (Image, bool) {
	spec, d := mapToAny(ctx, plan.Spec)
	diags.Append(d...)
	if d.HasError() {
		return Image{}, false
	}
	cloudID, err := strconv.ParseInt(plan.CloudID.ValueString(), 10, 64)
	if err != nil {
		diags.AddError("Bad cloud_id", err.Error())
		return Image{}, false
	}
	return Image{
		CloudID: cloudID, Name: plan.Name.ValueString(), Spec: spec,
		Username:           plan.Username.ValueString(),
		PreinstalledDocker: plan.PreinstalledDocker.ValueBool(),
		Notes:              plan.Notes.ValueString(),
	}, true
}
