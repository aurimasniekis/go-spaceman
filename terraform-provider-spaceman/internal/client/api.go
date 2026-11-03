package client

import (
	"context"
	"net/http"
)

type items[T any] struct {
	Items []T `json:"items"`
}

func notFound(op, what string) error {
	return &APIError{Status: http.StatusNotFound, Code: "not_found", Title: "Not Found", Detail: what + " not found", Operation: op}
}

// ---- nodes ----

// ListNodes is GET /nodes.
func (c *Client) ListNodes(ctx context.Context) ([]*Node, error) {
	var out items[*Node]
	return out.Items, c.get(ctx, "/nodes", &out)
}

// GetNode is GET /nodes/{id}.
func (c *Client) GetNode(ctx context.Context, id string) (*Node, error) {
	var out Node
	return &out, c.get(ctx, "/nodes/"+esc(id), &out)
}

// CreateNode is POST /nodes. The provider does not manage nodes (enrollment is a one-time secret); tests use it.
func (c *Client) CreateNode(ctx context.Context, name string) (*NodeEnrollment, error) {
	var out NodeEnrollment
	return &out, c.create(ctx, "/nodes", map[string]string{"name": name}, &out)
}

// ---- spaces ----

// ListSpaces is GET /spaces (the spaces the caller can see).
func (c *Client) ListSpaces(ctx context.Context) ([]*Space, error) {
	var out items[*Space]
	return out.Items, c.get(ctx, "/spaces", &out)
}

// GetSpace is GET /spaces/{id}.
func (c *Client) GetSpace(ctx context.Context, id string) (*Space, error) {
	var out Space
	return &out, c.get(ctx, "/spaces/"+esc(id), &out)
}

// CreateSpace is POST /spaces.
func (c *Client) CreateSpace(ctx context.Context, in CreateSpaceInput) (*Space, error) {
	var out Space
	return &out, c.create(ctx, "/spaces", in, &out)
}

// UpdateSpace is PUT /spaces/{id}.
func (c *Client) UpdateSpace(ctx context.Context, id string, version int64, in UpdateSpaceInput) (*Space, error) {
	var out Space
	return &out, c.put(ctx, "/spaces/"+esc(id), version, in, &out)
}

// SetSpaceState is POST /spaces/{id}/state.
func (c *Client) SetSpaceState(ctx context.Context, id string, version int64, state, mode string) (*Space, error) {
	var out Space
	body := map[string]string{"state": state}
	if mode != "" {
		body["mode"] = mode
	}
	return &out, c.post(ctx, "/spaces/"+esc(id)+"/state", version, body, &out)
}

// PurgeSpace is DELETE /spaces/{id} (archived spaces only).
func (c *Client) PurgeSpace(ctx context.Context, id string) error {
	return c.del(ctx, "/spaces/"+esc(id))
}

// ---- accounts ----

// GetAccount is GET /accounts/{id}.
func (c *Client) GetAccount(ctx context.Context, id string) (*Account, error) {
	var out Account
	return &out, c.get(ctx, "/accounts/"+esc(id), &out)
}

// CreateAccount is POST /accounts.
func (c *Client) CreateAccount(ctx context.Context, in AccountInput) (*Account, error) {
	var out Account
	return &out, c.create(ctx, "/accounts", in, &out)
}

// UpdateAccount is PUT /accounts/{id}.
func (c *Client) UpdateAccount(ctx context.Context, id string, version int64, in AccountInput) (*Account, error) {
	var out Account
	return &out, c.put(ctx, "/accounts/"+esc(id), version, in, &out)
}

// DeleteAccount is DELETE /accounts/{id}.
func (c *Client) DeleteAccount(ctx context.Context, id string) error {
	return c.del(ctx, "/accounts/"+esc(id))
}

// SpaceAccounts is GET /spaces/{id}/accounts.
func (c *Client) SpaceAccounts(ctx context.Context, spaceID string) ([]AccountBinding, error) {
	var out items[AccountBinding]
	return out.Items, c.get(ctx, "/spaces/"+esc(spaceID)+"/accounts", &out)
}

// SetSpaceAccounts is PUT /spaces/{id}/accounts (replaces every binding).
func (c *Client) SetSpaceAccounts(ctx context.Context, spaceID string, bs []AccountBinding) ([]AccountBinding, error) {
	in := items[map[string]any]{Items: []map[string]any{}}
	for _, b := range bs {
		in.Items = append(in.Items, map[string]any{"account_id": b.AccountID, "default": b.Default})
	}
	var out items[AccountBinding]
	return out.Items, c.put(ctx, "/spaces/"+esc(spaceID)+"/accounts", 0, in, &out)
}

// ---- harness profiles, role templates, space roles ----

// ListProfiles is GET /harness-profiles.
func (c *Client) ListProfiles(ctx context.Context) ([]*Profile, error) {
	var out items[*Profile]
	return out.Items, c.get(ctx, "/harness-profiles", &out)
}

// GetProfile finds a profile by ID (the API has no single-profile GET).
func (c *Client) GetProfile(ctx context.Context, id string) (*Profile, error) {
	l, err := c.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range l {
		if p.ID == id || p.Slug == id {
			return p, nil
		}
	}
	return nil, notFound("GET /harness-profiles", "harness profile "+id)
}

// SaveProfile is PUT /harness-profiles/{id} (id "" creates).
func (c *Client) SaveProfile(ctx context.Context, id string, version int64, in ProfileInput) (*Profile, error) {
	if id == "" {
		id, version = "new", 0
	}
	var out Profile
	return &out, c.put(ctx, "/harness-profiles/"+esc(id), version, in, &out)
}

// ListRoleTemplates is GET /role-templates.
func (c *Client) ListRoleTemplates(ctx context.Context) ([]*RoleTemplate, error) {
	var out items[*RoleTemplate]
	return out.Items, c.get(ctx, "/role-templates", &out)
}

// GetRoleTemplate finds a template by ID or slug.
func (c *Client) GetRoleTemplate(ctx context.Context, id string) (*RoleTemplate, error) {
	l, err := c.ListRoleTemplates(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range l {
		if t.ID == id || t.Slug == id {
			return t, nil
		}
	}
	return nil, notFound("GET /role-templates", "role template "+id)
}

// SaveRoleTemplate is PUT /role-templates/{id} (id "" creates).
func (c *Client) SaveRoleTemplate(ctx context.Context, id string, version int64, in RoleTemplateInput) (*RoleTemplate, error) {
	if id == "" {
		id, version = "new", 0
	}
	var out RoleTemplate
	return &out, c.put(ctx, "/role-templates/"+esc(id), version, in, &out)
}

// GetSpaceRole finds a space's role by name.
func (c *Client) GetSpaceRole(ctx context.Context, spaceID, name string) (*SpaceRole, error) {
	var out items[*SpaceRole]
	if err := c.get(ctx, "/spaces/"+esc(spaceID)+"/roles", &out); err != nil {
		return nil, err
	}
	for _, r := range out.Items {
		if r.Name == name {
			return r, nil
		}
	}
	return nil, notFound("GET /spaces/{space_id}/roles", "space role "+name)
}

// SaveSpaceRole is PUT /spaces/{id}/roles/{name}.
func (c *Client) SaveSpaceRole(ctx context.Context, spaceID, name string, in SpaceRoleInput) (*SpaceRole, error) {
	var out SpaceRole
	return &out, c.put(ctx, "/spaces/"+esc(spaceID)+"/roles/"+esc(name), 0, in, &out)
}

// DeleteSpaceRole is DELETE /spaces/{id}/roles/{name}.
func (c *Client) DeleteSpaceRole(ctx context.Context, spaceID, name string) error {
	return c.del(ctx, "/spaces/"+esc(spaceID)+"/roles/"+esc(name))
}

// ---- prompts ----

// GetPrompt is GET /prompts/{id} (ID or slug).
func (c *Client) GetPrompt(ctx context.Context, id string) (*Prompt, error) {
	var out Prompt
	return &out, c.get(ctx, "/prompts/"+esc(id), &out)
}

// CreatePrompt is POST /prompts.
func (c *Client) CreatePrompt(ctx context.Context, in CreatePromptInput) (*Prompt, error) {
	var out Prompt
	return &out, c.do2(ctx, http.MethodPost, "/prompts", 0, in, &out)
}

// SavePromptDraft is PUT /prompts/{id}/draft: version 0 starts a new draft, otherwise it updates the draft at version.
func (c *Client) SavePromptDraft(ctx context.Context, id string, version int64, in PromptDraftInput) (*PromptVersion, error) {
	var out PromptVersion
	return &out, c.put(ctx, "/prompts/"+esc(id)+"/draft", version, in, &out)
}

// PublishPrompt is POST /prompts/{id}/publish with the draft's version.
func (c *Client) PublishPrompt(ctx context.Context, id string, draftVersion int64) (*PromptVersion, error) {
	var out PromptVersion
	return &out, c.post(ctx, "/prompts/"+esc(id)+"/publish", draftVersion, nil, &out)
}

// ---- skills ----

// GetSkill is GET /skills/{id} (ID or slug).
func (c *Client) GetSkill(ctx context.Context, id string) (*Skill, error) {
	var out Skill
	return &out, c.get(ctx, "/skills/"+esc(id), &out)
}

// CreateSkill is POST /skills.
func (c *Client) CreateSkill(ctx context.Context, in CreateSkillInput) (*Skill, error) {
	var out Skill
	return &out, c.do2(ctx, http.MethodPost, "/skills", 0, in, &out)
}

// SaveSkillDraft is PUT /skills/{id}/draft.
func (c *Client) SaveSkillDraft(ctx context.Context, id string, version int64, in SkillDraftInput) (*Skill, error) {
	var out Skill
	return &out, c.put(ctx, "/skills/"+esc(id)+"/draft", version, in, &out)
}

// PublishSkill is POST /skills/{id}/publish.
func (c *Client) PublishSkill(ctx context.Context, id string, draftVersion int64) (*Skill, error) {
	var out Skill
	return &out, c.post(ctx, "/skills/"+esc(id)+"/publish", draftVersion, nil, &out)
}

// ---- MCP gateway ----

// GetMCPServer is GET /mcp-servers/{id}.
func (c *Client) GetMCPServer(ctx context.Context, id string) (*MCPServer, error) {
	var out MCPServer
	return &out, c.get(ctx, "/mcp-servers/"+esc(id), &out)
}

// CreateMCPServer is POST /mcp-servers.
func (c *Client) CreateMCPServer(ctx context.Context, in MCPServerInput) (*MCPServer, error) {
	var out MCPServer
	return &out, c.create(ctx, "/mcp-servers", in, &out)
}

// UpdateMCPServer is PUT /mcp-servers/{id}.
func (c *Client) UpdateMCPServer(ctx context.Context, id string, version int64, in MCPServerInput) (*MCPServer, error) {
	var out MCPServer
	return &out, c.put(ctx, "/mcp-servers/"+esc(id), version, in, &out)
}

// DeleteMCPServer is DELETE /mcp-servers/{id} (its grants go too).
func (c *Client) DeleteMCPServer(ctx context.Context, id string) error {
	return c.del(ctx, "/mcp-servers/"+esc(id))
}

// GetMCPGrant finds a grant of a server.
func (c *Client) GetMCPGrant(ctx context.Context, serverID, grantID string) (*MCPGrant, error) {
	var out items[*MCPGrant]
	if err := c.get(ctx, "/mcp-servers/"+esc(serverID)+"/grants", &out); err != nil {
		return nil, err
	}
	for _, g := range out.Items {
		if g.ID == grantID {
			return g, nil
		}
	}
	return nil, notFound("GET /mcp-servers/{server_id}/grants", "grant "+grantID)
}

// AddMCPGrant is POST /mcp-servers/{id}/grants.
func (c *Client) AddMCPGrant(ctx context.Context, serverID string, in MCPGrantInput) (*MCPGrant, error) {
	var out MCPGrant
	return &out, c.do2(ctx, http.MethodPost, "/mcp-servers/"+esc(serverID)+"/grants", 0, in, &out)
}

// RevokeMCPGrant is DELETE /mcp-servers/{id}/grants/{grant}.
func (c *Client) RevokeMCPGrant(ctx context.Context, serverID, grantID string) error {
	return c.del(ctx, "/mcp-servers/"+esc(serverID)+"/grants/"+esc(grantID))
}

// ---- events ----

// GetEventConnection finds a connection (the API has no single-connection GET).
func (c *Client) GetEventConnection(ctx context.Context, id string) (*EventConnection, error) {
	var out items[*EventConnection]
	if err := c.get(ctx, "/event-connections", &out); err != nil {
		return nil, err
	}
	for _, x := range out.Items {
		if x.ID == id {
			return x, nil
		}
	}
	return nil, notFound("GET /event-connections", "event connection "+id)
}

// CreateEventConnection is POST /event-connections.
func (c *Client) CreateEventConnection(ctx context.Context, in EventConnectionInput) (*EventConnection, error) {
	var out EventConnection
	return &out, c.do2(ctx, http.MethodPost, "/event-connections", 0, in, &out)
}

// UpdateEventConnection is PUT /event-connections/{id}.
func (c *Client) UpdateEventConnection(ctx context.Context, id string, version int64, in EventConnectionInput) (*EventConnection, error) {
	var out EventConnection
	return &out, c.put(ctx, "/event-connections/"+esc(id), version, in, &out)
}

// DeleteEventConnection is DELETE /event-connections/{id} (refused while a subscription uses it).
func (c *Client) DeleteEventConnection(ctx context.Context, id string) error {
	return c.del(ctx, "/event-connections/"+esc(id))
}

// ListEventTemplates is GET /event-templates.
func (c *Client) ListEventTemplates(ctx context.Context) ([]*EventTemplate, error) {
	var out items[*EventTemplate]
	return out.Items, c.get(ctx, "/event-templates", &out)
}

// GetEventTemplate is GET /event-templates/{id}.
func (c *Client) GetEventTemplate(ctx context.Context, id string) (*EventTemplate, error) {
	var out EventTemplate
	return &out, c.get(ctx, "/event-templates/"+esc(id), &out)
}

// CreateEventTemplate is POST /event-templates.
func (c *Client) CreateEventTemplate(ctx context.Context, in CreateEventTemplateInput) (*EventTemplate, error) {
	var out EventTemplate
	return &out, c.do2(ctx, http.MethodPost, "/event-templates", 0, in, &out)
}

// SaveEventTemplateDraft is PUT /event-templates/{id}/draft (replaces the draft or starts one).
func (c *Client) SaveEventTemplateDraft(ctx context.Context, id string, in EventTemplateDraftInput) (*EventTemplateVersion, error) {
	var out EventTemplateVersion
	return &out, c.put(ctx, "/event-templates/"+esc(id)+"/draft", 0, in, &out)
}

// PublishEventTemplateVersion is POST /event-template-versions/{id}/publish.
func (c *Client) PublishEventTemplateVersion(ctx context.Context, versionID string) (*PublishTemplateResult, error) {
	var out PublishTemplateResult
	return &out, c.post(ctx, "/event-template-versions/"+esc(versionID)+"/publish", 0, nil, &out)
}

// GetSubscription is GET /spaces/{id}/subscriptions/{sub}.
func (c *Client) GetSubscription(ctx context.Context, spaceID, id string) (*Subscription, error) {
	var out Subscription
	return &out, c.get(ctx, "/spaces/"+esc(spaceID)+"/subscriptions/"+esc(id), &out)
}

// CreateSubscription is POST /spaces/{id}/subscriptions.
func (c *Client) CreateSubscription(ctx context.Context, spaceID string, in CreateSubscriptionInput) (*Subscription, error) {
	var out Subscription
	return &out, c.do2(ctx, http.MethodPost, "/spaces/"+esc(spaceID)+"/subscriptions", 0, in, &out)
}

// UpdateSubscription is PUT /spaces/{id}/subscriptions/{sub}.
func (c *Client) UpdateSubscription(ctx context.Context, spaceID, id string, version int64, in UpdateSubscriptionInput) (*Subscription, error) {
	var out Subscription
	return &out, c.put(ctx, "/spaces/"+esc(spaceID)+"/subscriptions/"+esc(id), version, in, &out)
}

// DeleteSubscription is DELETE /spaces/{id}/subscriptions/{sub}.
func (c *Client) DeleteSubscription(ctx context.Context, spaceID, id string) error {
	return c.del(ctx, "/spaces/"+esc(spaceID)+"/subscriptions/"+esc(id))
}

// ---- human-action types ----

// GetHumanType is GET /human-types/{id} (ID or slug).
func (c *Client) GetHumanType(ctx context.Context, id string) (*HumanType, error) {
	var out HumanType
	return &out, c.get(ctx, "/human-types/"+esc(id), &out)
}

// CreateHumanType is POST /human-types.
func (c *Client) CreateHumanType(ctx context.Context, in CreateHumanTypeInput) (*HumanType, error) {
	var out HumanType
	return &out, c.do2(ctx, http.MethodPost, "/human-types", 0, in, &out)
}

// SaveHumanTypeDraft is PUT /human-types/{id}/draft.
func (c *Client) SaveHumanTypeDraft(ctx context.Context, id string, version int64, in HumanTypeDraftInput) (*HumanType, error) {
	var out HumanType
	return &out, c.put(ctx, "/human-types/"+esc(id)+"/draft", version, in, &out)
}

// PublishHumanType is POST /human-types/{id}/publish.
func (c *Client) PublishHumanType(ctx context.Context, id string, draftVersion int64) (*HumanType, error) {
	var out HumanType
	return &out, c.post(ctx, "/human-types/"+esc(id)+"/publish", draftVersion, nil, &out)
}

// GetSpaceHumanType finds a type enabled in a space.
func (c *Client) GetSpaceHumanType(ctx context.Context, spaceID, typeID string) (*SpaceHumanType, error) {
	var out items[*SpaceHumanType]
	if err := c.get(ctx, "/spaces/"+esc(spaceID)+"/human-types", &out); err != nil {
		return nil, err
	}
	for _, t := range out.Items {
		if t.TypeID == typeID {
			return t, nil
		}
	}
	return nil, notFound("GET /spaces/{space_id}/human-types", "human-action type "+typeID+" in the space")
}

// EnableSpaceHumanType is PUT /spaces/{id}/human-types/{type} (an upsert).
func (c *Client) EnableSpaceHumanType(ctx context.Context, spaceID, typeID string, in EnableHumanTypeInput) error {
	return c.put(ctx, "/spaces/"+esc(spaceID)+"/human-types/"+esc(typeID), 0, in, nil)
}

// DisableSpaceHumanType is DELETE /spaces/{id}/human-types/{type}.
func (c *Client) DisableSpaceHumanType(ctx context.Context, spaceID, typeID string) error {
	return c.del(ctx, "/spaces/"+esc(spaceID)+"/human-types/"+esc(typeID))
}

// ---- approval policies ----

// GetApprovalPolicy finds a policy (the API has no single-policy GET).
func (c *Client) GetApprovalPolicy(ctx context.Context, id string) (*ApprovalPolicy, error) {
	var out items[*ApprovalPolicy]
	if err := c.get(ctx, "/approval-policies", &out); err != nil {
		return nil, err
	}
	for _, p := range out.Items {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, notFound("GET /approval-policies", "approval policy "+id)
}

// CreateApprovalPolicy is POST /approval-policies.
func (c *Client) CreateApprovalPolicy(ctx context.Context, in ApprovalPolicyInput) (*ApprovalPolicy, error) {
	var out ApprovalPolicy
	return &out, c.do2(ctx, http.MethodPost, "/approval-policies", 0, in, &out)
}

// DeleteApprovalPolicy is DELETE /approval-policies/{id}.
func (c *Client) DeleteApprovalPolicy(ctx context.Context, id string) error {
	return c.del(ctx, "/approval-policies/"+esc(id))
}

// ---- knowledge bases and dreams ----

// GetKB is GET /spaces/{id}/kbs/{kb}.
func (c *Client) GetKB(ctx context.Context, spaceID, id string) (*KB, error) {
	var out KBDetail
	if err := c.get(ctx, "/spaces/"+esc(spaceID)+"/kbs/"+esc(id), &out); err != nil {
		return nil, err
	}
	if out.KB == nil {
		return nil, notFound("GET /spaces/{space_id}/kbs/{kb_id}", "knowledge base "+id)
	}
	return out.KB, nil
}

// CreateKB is POST /spaces/{id}/kbs.
func (c *Client) CreateKB(ctx context.Context, spaceID string, in KBInput) (*KB, error) {
	var out KB
	return &out, c.do2(ctx, http.MethodPost, "/spaces/"+esc(spaceID)+"/kbs", 0, in, &out)
}

// UpdateKB is PUT /spaces/{id}/kbs/{kb}.
func (c *Client) UpdateKB(ctx context.Context, spaceID, id string, version int64, in KBInput) (*KB, error) {
	var out KB
	return &out, c.put(ctx, "/spaces/"+esc(spaceID)+"/kbs/"+esc(id), version, in, &out)
}

// GetDream finds a dream configuration of a space.
func (c *Client) GetDream(ctx context.Context, spaceID, id string) (*Dream, error) {
	var out items[*Dream]
	if err := c.get(ctx, "/spaces/"+esc(spaceID)+"/dreams", &out); err != nil {
		return nil, err
	}
	for _, d := range out.Items {
		if d.ID == id {
			return d, nil
		}
	}
	return nil, notFound("GET /spaces/{space_id}/dreams", "dream configuration "+id)
}

// CreateDream is POST /spaces/{id}/dreams.
func (c *Client) CreateDream(ctx context.Context, spaceID string, in DreamInput) (*Dream, error) {
	var out Dream
	return &out, c.do2(ctx, http.MethodPost, "/spaces/"+esc(spaceID)+"/dreams", 0, in, &out)
}

// UpdateDream is PUT /spaces/{id}/dreams/{dream}.
func (c *Client) UpdateDream(ctx context.Context, spaceID, id string, version int64, in DreamInput) (*Dream, error) {
	var out Dream
	return &out, c.put(ctx, "/spaces/"+esc(spaceID)+"/dreams/"+esc(id), version, in, &out)
}

// ---- OIDC ----

// ListOIDCProviders is GET /oidc/providers.
func (c *Client) ListOIDCProviders(ctx context.Context) (*OIDCProviderList, error) {
	var out OIDCProviderList
	return &out, c.get(ctx, "/oidc/providers", &out)
}

// GetOIDCProvider is GET /oidc/providers/{id}.
func (c *Client) GetOIDCProvider(ctx context.Context, id string) (*OIDCProvider, error) {
	var out OIDCProvider
	return &out, c.get(ctx, "/oidc/providers/"+esc(id), &out)
}

// CreateOIDCProvider is POST /oidc/providers.
func (c *Client) CreateOIDCProvider(ctx context.Context, in OIDCProviderInput) (*OIDCProvider, error) {
	var out OIDCProvider
	return &out, c.create(ctx, "/oidc/providers", in, &out)
}

// UpdateOIDCProvider is PATCH /oidc/providers/{id}.
func (c *Client) UpdateOIDCProvider(ctx context.Context, id string, version int64, in OIDCProviderInput) (*OIDCProvider, error) {
	var out OIDCProvider
	return &out, c.patch(ctx, "/oidc/providers/"+esc(id), version, in, &out)
}

// DeleteOIDCProvider is DELETE /oidc/providers/{id}.
func (c *Client) DeleteOIDCProvider(ctx context.Context, id string) error {
	return c.del(ctx, "/oidc/providers/"+esc(id))
}

// GetOIDCMapping finds a group mapping of a provider.
func (c *Client) GetOIDCMapping(ctx context.Context, providerID, id string) (*OIDCMapping, error) {
	var out items[*OIDCMapping]
	if err := c.get(ctx, "/oidc/providers/"+esc(providerID)+"/mappings", &out); err != nil {
		return nil, err
	}
	for _, m := range out.Items {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, notFound("GET /oidc/providers/{provider_id}/mappings", "group mapping "+id)
}

// AddOIDCMapping is POST /oidc/providers/{id}/mappings.
func (c *Client) AddOIDCMapping(ctx context.Context, providerID string, in OIDCMappingInput) (*OIDCMapping, error) {
	var out OIDCMapping
	return &out, c.do2(ctx, http.MethodPost, "/oidc/providers/"+esc(providerID)+"/mappings", 0, in, &out)
}

// DeleteOIDCMapping is DELETE /oidc/providers/{id}/mappings/{mapping}.
func (c *Client) DeleteOIDCMapping(ctx context.Context, providerID, id string) error {
	return c.del(ctx, "/oidc/providers/"+esc(providerID)+"/mappings/"+esc(id))
}

// do2 is a plain (non-idempotent) call: creates whose operation does not accept an Idempotency-Key are not retried,
// so a lost response can never create a second object.
func (c *Client) do2(ctx context.Context, method, path string, ifMatch int64, in, out any) error {
	_, err := c.do(ctx, request{method: method, path: path, body: in, ifMatch: ifMatch}, out)
	return err
}
