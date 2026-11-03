package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

type spaceRepositoryResource struct{ base }

func newSpaceRepositoryResource() resource.Resource { return &spaceRepositoryResource{} }

type spaceRepositoryModel struct {
	ID            types.String `tfsdk:"id"`
	SpaceID       types.String `tfsdk:"space_id"`
	Name          types.String `tfsdk:"name"`
	Path          types.String `tfsdk:"path"`
	Description   types.String `tfsdk:"description"`
	URL           types.String `tfsdk:"url"`
	DefaultBranch types.String `tfsdk:"default_branch"`
	Role          types.String `tfsdk:"role"`
	ClonePolicy   types.String `tfsdk:"clone_policy"`
}

func (r *spaceRepositoryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space_repository"
}

func (r *spaceRepositoryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A repository of a space (`/spaces/{id}/repositories`, ADR-0044): its folder in the space folder, clone URL, default " +
			"branch, role and clone policy. The space's node clones it (with an `auto` policy as soon as it holds the space's lease) using " +
			"its own Git authentication: the URL never carries credentials, and SpaceMan stores none. Writer worktrees, knowledge bases and " +
			"relocation use the configured repositories. The node's report (remote, heads, last fetch, changes, unpushed commits, worktrees) " +
			"and syncs are runtime state and are not managed. Destroying the resource removes the repository from the space's list only: its " +
			"folder on the node and the upstream repository stay; a repository a knowledge base uses cannot be removed.",
		Attributes: map[string]schema.Attribute{
			"id":       idAttr("The repository ID."),
			"space_id": requiredReplace("The space (not archived)."),
			"name":     requiredReplace("Unique in the space: 1-63 lowercase letters, digits, `.`, `_` or `-`. Changing it replaces the repository."),
			"path": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "Folder relative to the space folder (default the name), for example `repos/api`; not in `instances/`, " +
					"`worktrees/`, `work/` or `dreams/`. Changing it replaces the repository (the old folder stays on the node).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplaceIfConfigured()}},
			"description": optString("Description."),
			"url": optString("Clone URL without credentials: `https://…`, `ssh://…` or `git@host:org/repo.git` (`http://`, `git://`, `file://` and " +
				"local paths only in development mode). Required unless `clone_policy` is `manual`."),
			"default_branch": optString("The branch the node checks out and reports on (default `main`)."),
			"role":           optString("`code` (default) or `kb` (a knowledge base's repository).", stringvalidator.OneOf("code", "kb")),
			"clone_policy": optString("`auto` (default with a URL: cloned while the node holds the space's lease), `on_demand` (cloned when a run "+
				"needs it or on a sync) or `manual` (default without a URL: never cloned by SpaceMan).", stringvalidator.OneOf("auto", "on_demand", "manual")),
		},
	}
}

func (r *spaceRepositoryResource) toModel(s *client.SpaceRepository, m *spaceRepositoryModel) {
	m.ID = types.StringValue(s.ID)
	m.SpaceID = types.StringValue(s.SpaceID)
	m.Name = types.StringValue(s.Name)
	m.Path = types.StringValue(s.Path)
	m.Description = types.StringValue(s.Description)
	m.URL = keepTrimmed(m.URL, s.URL)
	m.DefaultBranch = keepTrimmed(m.DefaultBranch, s.DefaultBranch)
	m.Role = types.StringValue(s.Role)
	m.ClonePolicy = types.StringValue(s.ClonePolicy)
}

func (r *spaceRepositoryResource) input(m *spaceRepositoryModel, create bool) client.SpaceRepositoryInput {
	in := client.SpaceRepositoryInput{Description: strOrEmpty(m.Description), URL: strOrEmpty(m.URL), DefaultBranch: strOrEmpty(m.DefaultBranch),
		Role: strOrEmpty(m.Role), ClonePolicy: strOrEmpty(m.ClonePolicy)}
	if create {
		in.Name, in.Path = m.Name.ValueString(), strOrEmpty(m.Path)
	}
	return in
}

func (r *spaceRepositoryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m spaceRepositoryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.CreateSpaceRepository(ctx, m.SpaceID.ValueString(), r.input(&m, true))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the repository", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceRepositoryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m spaceRepositoryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.GetSpaceRepository(ctx, m.SpaceID.ValueString(), m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the repository", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceRepositoryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior spaceRepositoryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetSpaceRepository(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the repository", err)
			return
		}
		v = cur.Version
	}
	s, err := r.c.UpdateSpaceRepository(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString(), v, r.input(&m, false))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the repository", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceRepositoryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m spaceRepositoryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteSpaceRepository(ctx, m.SpaceID.ValueString(), m.ID.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not remove the repository", err)
	}
}

func (r *spaceRepositoryResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p := splitImportID(req.ID, 2, "<space_id>/<repository_id or name>", &resp.Diagnostics)
	if p == nil {
		return
	}
	s, err := r.c.GetSpaceRepository(ctx, p[0], p[1])
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the repository", err)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), s.SpaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), s.ID)...)
}
