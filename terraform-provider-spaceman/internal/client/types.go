package client

import (
	"encoding/json"
	"time"
)

// The types below mirror the JSON of api/openapi.json (generated from the central's Go types). Only the fields the
// provider reads or writes are declared; unknown fields are ignored.

// Node is a registered node (GET /nodes, /nodes/{id}).
type Node struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	State        string            `json:"state"`
	Labels       map[string]string `json:"labels"`
	Connected    bool              `json:"connected"`
	Capacity     int64             `json:"capacity"`
	AgentVersion string            `json:"agent_version,omitempty"`
	Version      int64             `json:"version"`
}

// NodeEnrollment is the response of POST /nodes (tests only: the provider does not manage nodes).
type NodeEnrollment struct {
	Node  *Node  `json:"node"`
	Token string `json:"token"`
}

// Space is a space pinned to a node.
type Space struct {
	ID                   string `json:"id"`
	Slug                 string `json:"slug"`
	Name                 string `json:"name"`
	Description          string `json:"description"`
	Goals                string `json:"goals"`
	StandingInstructions string `json:"standing_instructions"`
	NodeID               string `json:"node_id"`
	DesiredNodeID        string `json:"desired_node_id,omitempty"`
	State                string `json:"state"`
	PauseMode            string `json:"pause_mode,omitempty"`
	DisconnectPolicy     string `json:"disconnect_policy"`
	Workdir              string `json:"workdir"`
	Version              int64  `json:"version"`
}

// StartRelocationInput is POST /spaces/{id}/relocation (the clean flow; the unavailable-source flow is an operator act in the
// UI or API and is not offered here).
type StartRelocationInput struct {
	DestinationNodeID string `json:"destination_node_id"`
	DirtyState        string `json:"dirty_state,omitempty"`
	StopRuns          bool   `json:"stop_runs,omitempty"`
	Recovery          string `json:"recovery,omitempty"`
	RetainDays        int64  `json:"retain_days,omitempty"`
}

// RelocationIssue is one finding of the source's inspection.
type RelocationIssue struct {
	Code       string `json:"code"`
	Path       string `json:"path,omitempty"`
	Detail     string `json:"detail"`
	Exportable bool   `json:"exportable"`
}

// Relocation is a space relocation as the API shows it (the fields the provider uses).
type Relocation struct {
	ID                string            `json:"id"`
	SpaceID           string            `json:"space_id"`
	SourceNodeID      string            `json:"source_node_id"`
	DestinationNodeID string            `json:"destination_node_id"`
	Mode              string            `json:"mode"`
	State             string            `json:"state"`
	BlockedStep       string            `json:"blocked_step,omitempty"`
	DirtyState        string            `json:"dirty_state"`
	Issues            []RelocationIssue `json:"issues"`
	Error             string            `json:"error,omitempty"`
	GenerationAfter   int64             `json:"generation_after,omitempty"`
}

// Finished reports whether the relocation reached an end state.
func (r *Relocation) Finished() bool {
	switch r.State {
	case "done", "checked", "failed", "cancelled":
		return true
	}
	return false
}

// CreateSpaceInput is POST /spaces.
type CreateSpaceInput struct {
	Slug                 string `json:"slug"`
	Name                 string `json:"name"`
	Description          string `json:"description,omitempty"`
	Goals                string `json:"goals,omitempty"`
	StandingInstructions string `json:"standing_instructions,omitempty"`
	NodeID               string `json:"node_id"`
	DisconnectPolicy     string `json:"disconnect_policy,omitempty"`
}

// UpdateSpaceInput is PUT /spaces/{id} (every field is replaced).
type UpdateSpaceInput struct {
	Name                 string `json:"name"`
	Description          string `json:"description"`
	Goals                string `json:"goals"`
	StandingInstructions string `json:"standing_instructions"`
	DisconnectPolicy     string `json:"disconnect_policy"`
}

// Account is a harness account (never its secret).
type Account struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Kind            string            `json:"kind"`
	Harness         string            `json:"harness"`
	Description     string            `json:"description,omitempty"`
	Metadata        map[string]string `json:"metadata"`
	SecretVersion   int64             `json:"secret_version"`
	SecretRotatedAt *time.Time        `json:"secret_rotated_at,omitempty"`
	Enabled         bool              `json:"enabled"`
	Version         int64             `json:"version"`
}

// AccountInput creates or edits an account. Secret is write-only: empty on an edit keeps the stored one.
type AccountInput struct {
	Name        string            `json:"name"`
	Kind        string            `json:"kind,omitempty"`
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Secret      string            `json:"secret,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
}

// AccountBinding lets a space's runs use an account.
type AccountBinding struct {
	AccountID string `json:"account_id"`
	Default   bool   `json:"default"`
	Harness   string `json:"harness,omitempty"`
	Name      string `json:"name,omitempty"`
	Enabled   bool   `json:"enabled,omitempty"`
}

// SkillRef assigns a skill (following its latest version, or pinned to one).
type SkillRef struct {
	SkillID string `json:"skill_id"`
	Pin     string `json:"pin,omitempty"`
}

// Profile is a harness profile.
type Profile struct {
	ID          string            `json:"id"`
	Slug        string            `json:"slug"`
	Name        string            `json:"name"`
	Harness     string            `json:"harness"`
	Model       string            `json:"model,omitempty"`
	Settings    map[string]string `json:"settings"`
	Description string            `json:"description,omitempty"`
	Revision    int64             `json:"revision"`
	Version     int64             `json:"version"`
}

// ProfileInput is PUT /harness-profiles/{id|new}.
type ProfileInput struct {
	Slug        string            `json:"slug,omitempty"`
	Name        string            `json:"name"`
	Harness     string            `json:"harness,omitempty"`
	Model       string            `json:"model,omitempty"`
	Settings    map[string]string `json:"settings,omitempty"`
	Description string            `json:"description,omitempty"`
}

// RoleTemplate is a role template.
type RoleTemplate struct {
	ID                string            `json:"id"`
	Slug              string            `json:"slug"`
	Name              string            `json:"name"`
	Kind              string            `json:"kind"`
	Description       string            `json:"description,omitempty"`
	ProfileID         string            `json:"profile_id,omitempty"`
	PromptID          string            `json:"prompt_id,omitempty"`
	PromptPin         string            `json:"prompt_pin,omitempty"`
	MemoryConventions string            `json:"memory_conventions,omitempty"`
	Settings          map[string]string `json:"settings"`
	Skills            []SkillRef        `json:"skills"`
	Revision          int64             `json:"revision"`
	Version           int64             `json:"version"`
}

// RoleTemplateInput is PUT /role-templates/{id|new}.
type RoleTemplateInput struct {
	Slug              string            `json:"slug,omitempty"`
	Name              string            `json:"name"`
	Kind              string            `json:"kind,omitempty"`
	Description       string            `json:"description,omitempty"`
	ProfileID         string            `json:"profile_id,omitempty"`
	PromptID          string            `json:"prompt_id,omitempty"`
	PromptPin         string            `json:"prompt_pin,omitempty"`
	MemoryConventions string            `json:"memory_conventions,omitempty"`
	Settings          map[string]string `json:"settings,omitempty"`
	Skills            []SkillRef        `json:"skills,omitempty"`
}

// SpaceRole is a role configured in a space.
type SpaceRole struct {
	SpaceID      string            `json:"space_id"`
	Name         string            `json:"name"`
	TemplateID   string            `json:"template_id"`
	ProfileID    string            `json:"profile_id,omitempty"`
	PromptID     string            `json:"prompt_id,omitempty"`
	PromptPin    string            `json:"prompt_pin,omitempty"`
	Model        string            `json:"model,omitempty"`
	Settings     map[string]string `json:"settings"`
	Instructions string            `json:"instructions,omitempty"`
	Skills       []SkillRef        `json:"skills"`
	Revision     int64             `json:"revision"`
	Version      int64             `json:"version"`
}

// SpaceRoleInput is PUT /spaces/{id}/roles/{name} (an upsert).
type SpaceRoleInput struct {
	TemplateID   string            `json:"template_id"`
	ProfileID    string            `json:"profile_id,omitempty"`
	PromptID     string            `json:"prompt_id,omitempty"`
	PromptPin    string            `json:"prompt_pin,omitempty"`
	Model        string            `json:"model,omitempty"`
	Settings     map[string]string `json:"settings,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	Skills       []SkillRef        `json:"skills,omitempty"`
}

// Prompt is a catalog prompt with its versions (draft first, then newest published first).
type Prompt struct {
	ID          string           `json:"id"`
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Kind        string           `json:"kind"`
	Description string           `json:"description,omitempty"`
	Latest      int64            `json:"latest"`
	Versions    []*PromptVersion `json:"versions"`
	Version     int64            `json:"version"`
}

// PromptVersion is a draft (number 0) or an immutable published version.
type PromptVersion struct {
	ID      string `json:"id"`
	Number  int64  `json:"number"`
	State   string `json:"state"`
	Body    string `json:"body"`
	Extends string `json:"extends,omitempty"`
	Notes   string `json:"notes,omitempty"`
	SHA256  string `json:"sha256"`
	Version int64  `json:"version"`
}

// CreatePromptInput is POST /prompts (the body becomes the first draft).
type CreatePromptInput struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	Body        string `json:"body,omitempty"`
}

// PromptDraftInput is PUT /prompts/{id}/draft.
type PromptDraftInput struct {
	Body    string `json:"body"`
	Extends string `json:"extends,omitempty"`
	Notes   string `json:"notes,omitempty"`
}

// Draft returns the prompt's draft, if any.
func (p *Prompt) Draft() *PromptVersion {
	for _, v := range p.Versions {
		if v.State == "draft" {
			return v
		}
	}
	return nil
}

// LatestPublished returns the newest published version, if any.
func (p *Prompt) LatestPublished() *PromptVersion {
	var best *PromptVersion
	for _, v := range p.Versions {
		if v.State == "published" && (best == nil || v.Number > best.Number) {
			best = v
		}
	}
	return best
}

// Skill is a catalog skill with its versions.
type Skill struct {
	ID          string          `json:"id"`
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Source      string          `json:"source"`
	Latest      int64           `json:"latest"`
	Versions    []*SkillVersion `json:"versions"`
}

// SkillVersion is a draft or an immutable published set of files.
type SkillVersion struct {
	ID      string            `json:"id"`
	Number  int64             `json:"number"`
	State   string            `json:"state"`
	Files   map[string]string `json:"files"`
	SHA256  string            `json:"sha256"`
	Notes   string            `json:"notes,omitempty"`
	Version int64             `json:"version"`
}

// CreateSkillInput is POST /skills.
type CreateSkillInput struct {
	Slug        string            `json:"slug"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Files       map[string]string `json:"files"`
}

// SkillDraftInput is PUT /skills/{id}/draft.
type SkillDraftInput struct {
	Files map[string]string `json:"files"`
	Notes string            `json:"notes,omitempty"`
}

// Draft returns the skill's draft, if any.
func (s *Skill) Draft() *SkillVersion {
	for _, v := range s.Versions {
		if v.State == "draft" {
			return v
		}
	}
	return nil
}

// LatestPublished returns the newest published version, if any.
func (s *Skill) LatestPublished() *SkillVersion {
	var best *SkillVersion
	for _, v := range s.Versions {
		if v.State == "published" && (best == nil || v.Number > best.Number) {
			best = v
		}
	}
	return best
}

// MCPServer is an upstream MCP server (never its header values).
type MCPServer struct {
	ID             string   `json:"id"`
	Slug           string   `json:"slug"`
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	URL            string   `json:"url"`
	HeaderNames    []string `json:"header_names"`
	HeadersVersion int64    `json:"headers_version"`
	AllowPrivate   bool     `json:"allow_private"`
	Tools          []string `json:"tools"`
	CallTimeoutMS  int64    `json:"call_timeout_ms"`
	MaxConcurrency int64    `json:"max_concurrency"`
	MaxResultBytes int64    `json:"max_result_bytes"`
	Enabled        bool     `json:"enabled"`
	Version        int64    `json:"version"`
}

// MCPServerInput creates or edits a server. Headers are write-only; on an edit they are replaced only with SetHeaders.
type MCPServerInput struct {
	Slug           string            `json:"slug,omitempty"`
	Name           string            `json:"name"`
	Description    string            `json:"description,omitempty"`
	URL            string            `json:"url"`
	SetHeaders     bool              `json:"set_headers,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	AllowPrivate   bool              `json:"allow_private,omitempty"`
	Tools          []string          `json:"tools,omitempty"`
	CallTimeoutMS  int64             `json:"call_timeout_ms,omitempty"`
	MaxConcurrency int64             `json:"max_concurrency,omitempty"`
	MaxResultBytes int64             `json:"max_result_bytes,omitempty"`
	Enabled        *bool             `json:"enabled,omitempty"`
}

// MCPGrant grants a server to a space role.
type MCPGrant struct {
	ID       string   `json:"id"`
	ServerID string   `json:"server_id"`
	SpaceID  string   `json:"space_id"`
	Role     string   `json:"role"`
	Tools    []string `json:"tools"`
}

// MCPGrantInput is POST /mcp-servers/{id}/grants.
type MCPGrantInput struct {
	SpaceID string   `json:"space_id"`
	Role    string   `json:"role,omitempty"`
	Tools   []string `json:"tools,omitempty"`
}

// EventConnection is a Pulse connection (never its credentials).
type EventConnection struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	ClientID string `json:"client_id"`
	HasCA    bool   `json:"has_ca"`
	Stream   string `json:"stream"`
	Enabled  bool   `json:"enabled"`
	Version  int64  `json:"version"`
}

// EventConnectionInput creates or updates a connection; empty Creds on an update keeps the stored credentials.
type EventConnectionInput struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Creds   string `json:"creds,omitempty"`
	CAPEM   string `json:"ca_pem,omitempty"`
	Stream  string `json:"stream,omitempty"`
	Enabled bool   `json:"enabled"`
}

// TemplateFilter is one filter of an event template definition.
type TemplateFilter struct {
	Path   string   `json:"path"`
	Op     string   `json:"op"`
	Value  string   `json:"value,omitempty"`
	Values []string `json:"values,omitempty"`
}

// TemplateVariable is a subscription variable of a definition.
type TemplateVariable struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
	Example     string `json:"example,omitempty"`
}

// TemplateDefinition is what an event template (or a custom subscription) turns an event into.
type TemplateDefinition struct {
	Subject      string             `json:"subject"`
	Variables    []TemplateVariable `json:"variables"`
	Filters      []TemplateFilter   `json:"filters"`
	InputSchema  json.RawMessage    `json:"input_schema,omitempty"`
	Title        string             `json:"title"`
	Body         string             `json:"body"`
	Instructions string             `json:"instructions,omitempty"`
	Correlation  string             `json:"correlation,omitempty"`
	Urgency      string             `json:"urgency,omitempty"`
}

// EventTemplate is a catalog template with its versions.
type EventTemplate struct {
	ID          string                  `json:"id"`
	Slug        string                  `json:"slug"`
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Builtin     bool                    `json:"builtin"`
	Versions    []*EventTemplateVersion `json:"versions"`
}

// EventTemplateVersion is a draft or a published (immutable) version. Version is its number, not a concurrency token.
type EventTemplateVersion struct {
	ID         string              `json:"id"`
	TemplateID string              `json:"template_id"`
	Version    int64               `json:"version"`
	Status     string              `json:"status"`
	Definition *TemplateDefinition `json:"definition"`
	Notes      string              `json:"notes,omitempty"`
}

// Draft returns the template's draft, if any.
func (t *EventTemplate) Draft() *EventTemplateVersion {
	for _, v := range t.Versions {
		if v.Status == "draft" {
			return v
		}
	}
	return nil
}

// LatestPublished returns the newest published version, if any.
func (t *EventTemplate) LatestPublished() *EventTemplateVersion {
	var best *EventTemplateVersion
	for _, v := range t.Versions {
		if v.Status == "published" && (best == nil || v.Version > best.Version) {
			best = v
		}
	}
	return best
}

// CreateEventTemplateInput is POST /event-templates (the definition becomes the first draft).
type CreateEventTemplateInput struct {
	Slug        string             `json:"slug"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Definition  TemplateDefinition `json:"definition"`
}

// EventTemplateDraftInput is PUT /event-templates/{id}/draft.
type EventTemplateDraftInput struct {
	Definition TemplateDefinition `json:"definition"`
	Notes      string             `json:"notes,omitempty"`
}

// PublishTemplateResult is POST /event-template-versions/{id}/publish.
type PublishTemplateResult struct {
	Version           *EventTemplateVersion `json:"version"`
	HeldSubscriptions []string              `json:"held_subscriptions"`
}

// Cursor is where a (re)created subscription consumer starts.
type Cursor struct {
	Policy     string     `json:"policy"`
	Time       *time.Time `json:"time,omitempty"`
	Seq        int64      `json:"seq,omitempty"`
	Replay     bool       `json:"replay,omitempty"`
	Generation int64      `json:"generation,omitempty"`
}

// TemplateRef is the template version a subscription uses.
type TemplateRef struct {
	TemplateID   string `json:"template_id,omitempty"`
	TemplateSlug string `json:"template_slug,omitempty"`
	Version      int64  `json:"version,omitempty"`
	VersionID    string `json:"version_id,omitempty"`
}

// Subscription binds a template version or a custom definition to a space.
type Subscription struct {
	ID           string              `json:"id"`
	SpaceID      string              `json:"space_id"`
	Name         string              `json:"name"`
	ConnectionID string              `json:"connection_id"`
	Template     *TemplateRef        `json:"template,omitempty"`
	Follow       string              `json:"follow"`
	Delivery     string              `json:"delivery"`
	Custom       *TemplateDefinition `json:"custom,omitempty"`
	Vars         map[string]string   `json:"vars"`
	Subject      string              `json:"subject"`
	Durable      string              `json:"durable"`
	Consumer     string              `json:"consumer"`
	Grant        string              `json:"grant"`
	Enabled      bool                `json:"enabled"`
	Cursor       Cursor              `json:"cursor"`
	Version      int64               `json:"version"`
}

// CreateSubscriptionInput is POST /spaces/{id}/subscriptions.
type CreateSubscriptionInput struct {
	Name              string              `json:"name"`
	ConnectionID      string              `json:"connection_id"`
	TemplateVersionID string              `json:"template_version_id,omitempty"`
	Follow            string              `json:"follow,omitempty"`
	Custom            *TemplateDefinition `json:"custom,omitempty"`
	Vars              map[string]string   `json:"vars,omitempty"`
	Start             *Cursor             `json:"start,omitempty"`
	Delivery          string              `json:"delivery,omitempty"`
}

// UpdateSubscriptionInput is PUT /spaces/{id}/subscriptions/{sub}.
type UpdateSubscriptionInput struct {
	Name              string              `json:"name,omitempty"`
	Enabled           bool                `json:"enabled"`
	TemplateVersionID string              `json:"template_version_id,omitempty"`
	Follow            string              `json:"follow,omitempty"`
	Custom            *TemplateDefinition `json:"custom,omitempty"`
	Vars              map[string]string   `json:"vars,omitempty"`
	Start             *Cursor             `json:"start,omitempty"`
}

// HumanType is a human-action type with its versions.
type HumanType struct {
	ID          string              `json:"id"`
	Slug        string              `json:"slug"`
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Latest      int64               `json:"latest"`
	Versions    []*HumanTypeVersion `json:"versions"`
}

// HumanTypeVersion is a draft (number 0) or a published version.
type HumanTypeVersion struct {
	ID                   string          `json:"id"`
	Number               int64           `json:"number"`
	State                string          `json:"state"`
	RequestSchema        json.RawMessage `json:"request_schema"`
	ResponseSchema       json.RawMessage `json:"response_schema"`
	UI                   json.RawMessage `json:"ui"`
	Outcomes             []string        `json:"outcomes"`
	DefaultPriority      string          `json:"default_priority"`
	DefaultExpiryMinutes int64           `json:"default_expiry_minutes"`
	Notes                string          `json:"notes,omitempty"`
	Version              int64           `json:"version"`
}

// Draft returns the type's draft, if any.
func (t *HumanType) Draft() *HumanTypeVersion {
	for _, v := range t.Versions {
		if v.State == "draft" {
			return v
		}
	}
	return nil
}

// LatestPublished returns the newest published version, if any.
func (t *HumanType) LatestPublished() *HumanTypeVersion {
	var best *HumanTypeVersion
	for _, v := range t.Versions {
		if v.State == "published" && (best == nil || v.Number > best.Number) {
			best = v
		}
	}
	return best
}

// HumanTypeDraftInput is the editable content of a type version.
type HumanTypeDraftInput struct {
	RequestSchema        json.RawMessage `json:"request_schema,omitempty"`
	ResponseSchema       json.RawMessage `json:"response_schema,omitempty"`
	UI                   json.RawMessage `json:"ui,omitempty"`
	Outcomes             []string        `json:"outcomes,omitempty"`
	DefaultPriority      string          `json:"default_priority,omitempty"`
	DefaultExpiryMinutes int64           `json:"default_expiry_minutes,omitempty"`
	Notes                string          `json:"notes,omitempty"`
}

// CreateHumanTypeInput is POST /human-types.
type CreateHumanTypeInput struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	HumanTypeDraftInput
}

// SpaceHumanType is a type enabled in a space with the version it uses now.
type SpaceHumanType struct {
	TypeID               string `json:"type_id"`
	Slug                 string `json:"slug"`
	Name                 string `json:"name"`
	Label                string `json:"label"`
	Pinned               bool   `json:"pinned"`
	VersionID            string `json:"version_id"`
	Number               int64  `json:"number"`
	DefaultPriority      string `json:"default_priority"`
	DefaultExpiryMinutes int64  `json:"default_expiry_minutes"`
}

// EnableHumanTypeInput is PUT /spaces/{id}/human-types/{type}.
type EnableHumanTypeInput struct {
	VersionPin           string `json:"version_pin,omitempty"`
	Label                string `json:"label,omitempty"`
	DefaultPriority      string `json:"default_priority,omitempty"`
	DefaultExpiryMinutes int64  `json:"default_expiry_minutes,omitempty"`
}

// ApprovalPolicy makes an operation need a person's approval.
type ApprovalPolicy struct {
	ID                string `json:"id"`
	SpaceID           string `json:"space_id"`
	Operation         string `json:"operation"`
	Uses              int64  `json:"uses"`
	ValidSeconds      int64  `json:"valid_seconds"`
	RequestTTLSeconds int64  `json:"request_ttl_seconds"`
	Note              string `json:"note,omitempty"`
}

// ApprovalPolicyInput is POST /approval-policies.
type ApprovalPolicyInput struct {
	SpaceID           string `json:"space_id,omitempty"`
	Operation         string `json:"operation"`
	Uses              int64  `json:"uses,omitempty"`
	ValidSeconds      int64  `json:"valid_seconds,omitempty"`
	RequestTTLSeconds int64  `json:"request_ttl_seconds,omitempty"`
	Note              string `json:"note,omitempty"`
}

// KB is a knowledge base configuration.
type KB struct {
	ID                string   `json:"id"`
	SpaceID           string   `json:"space_id"`
	Slug              string   `json:"slug"`
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	Repo              string   `json:"repo"`
	BaseBranch        string   `json:"base_branch"`
	SpaceRole         string   `json:"space_role"`
	EntryInstructions string   `json:"entry_instructions,omitempty"`
	Validator         []string `json:"validator"`
	WritePolicy       string   `json:"write_policy"`
	AllowedRoles      []string `json:"allowed_roles"`
	MaxSessions       int64    `json:"max_sessions"`
	IdleSleepSeconds  int64    `json:"idle_sleep_seconds"`
	RequestTimeout    int64    `json:"request_timeout_seconds"`
	SessionTTLSeconds int64    `json:"session_ttl_seconds"`
	Enabled           bool     `json:"enabled"`
	Version           int64    `json:"version"`
}

// KBInput creates or edits a knowledge base (PUT replaces every field).
type KBInput struct {
	Slug              string   `json:"slug,omitempty"`
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	Repo              string   `json:"repo"`
	BaseBranch        string   `json:"base_branch,omitempty"`
	SpaceRole         string   `json:"space_role"`
	EntryInstructions string   `json:"entry_instructions,omitempty"`
	Validator         []string `json:"validator,omitempty"`
	WritePolicy       string   `json:"write_policy,omitempty"`
	AllowedRoles      []string `json:"allowed_roles,omitempty"`
	MaxSessions       int64    `json:"max_sessions,omitempty"`
	IdleSleepSeconds  int64    `json:"idle_sleep_seconds,omitempty"`
	RequestTimeout    int64    `json:"request_timeout_seconds,omitempty"`
	SessionTTLSeconds int64    `json:"session_ttl_seconds,omitempty"`
	Enabled           *bool    `json:"enabled,omitempty"`
}

// KBDetail is GET /spaces/{id}/kbs/{kb}.
type KBDetail struct {
	KB *KB `json:"kb"`
}

// Dream is a dream configuration.
type Dream struct {
	ID              string   `json:"id"`
	SpaceID         string   `json:"space_id"`
	Slug            string   `json:"slug"`
	Name            string   `json:"name"`
	SpaceRole       string   `json:"space_role"`
	MemoryRoles     []string `json:"memory_roles"`
	TranscriptRoles []string `json:"transcript_roles"`
	IncludeOwn      bool     `json:"include_own"`
	Instructions    string   `json:"instructions,omitempty"`
	IntervalHours   int64    `json:"interval_hours"`
	MaxMinutes      int64    `json:"max_minutes"`
	MaxTokens       int64    `json:"max_tokens"`
	MaxFiles        int64    `json:"max_files"`
	MaxBytes        int64    `json:"max_bytes"`
	Enabled         bool     `json:"enabled"`
	Version         int64    `json:"version"`
}

// DreamInput creates or edits a dream configuration (PUT replaces every field).
type DreamInput struct {
	Slug            string   `json:"slug,omitempty"`
	Name            string   `json:"name"`
	SpaceRole       string   `json:"space_role"`
	MemoryRoles     []string `json:"memory_roles,omitempty"`
	TranscriptRoles []string `json:"transcript_roles,omitempty"`
	IncludeOwn      bool     `json:"include_own,omitempty"`
	Instructions    string   `json:"instructions,omitempty"`
	IntervalHours   int64    `json:"interval_hours,omitempty"`
	MaxMinutes      int64    `json:"max_minutes,omitempty"`
	MaxTokens       int64    `json:"max_tokens,omitempty"`
	MaxFiles        int64    `json:"max_files,omitempty"`
	MaxBytes        int64    `json:"max_bytes,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty"`
}

// JITRules restrict just-in-time provisioning of an OIDC provider.
type JITRules struct {
	EmailDomains  []string `json:"email_domains"`
	Tenants       []string `json:"tenants"`
	HostedDomains []string `json:"hosted_domains"`
	Groups        []string `json:"groups"`
}

// OIDCProvider is a sign-in provider (never its client secret).
type OIDCProvider struct {
	ID                        string   `json:"id"`
	Slug                      string   `json:"slug"`
	DisplayName               string   `json:"display_name"`
	Issuer                    string   `json:"issuer"`
	ClientID                  string   `json:"client_id"`
	HasClientSecret           bool     `json:"has_client_secret"`
	Scopes                    []string `json:"scopes"`
	GroupsClaim               string   `json:"groups_claim"`
	Provisioning              string   `json:"provisioning"`
	JIT                       JITRules `json:"jit"`
	AllowPrivateNetwork       bool     `json:"allow_private_network"`
	AllowCrossOriginEndpoints bool     `json:"allow_cross_origin_endpoints"`
	CABundlePEM               string   `json:"ca_bundle_pem"`
	StepUpFallback            string   `json:"stepup_fallback"`
	ACRValues                 []string `json:"acr_values"`
	EndSessionEnabled         bool     `json:"end_session_enabled"`
	Enabled                   bool     `json:"enabled"`
	Version                   int64    `json:"version"`
}

// OIDCProviderInput creates or patches a provider: nil fields are left unchanged; an empty ClientSecret removes it.
type OIDCProviderInput struct {
	Slug                      *string   `json:"slug,omitempty"`
	DisplayName               *string   `json:"display_name,omitempty"`
	Issuer                    *string   `json:"issuer,omitempty"`
	ClientID                  *string   `json:"client_id,omitempty"`
	ClientSecret              *string   `json:"client_secret,omitempty"`
	Scopes                    *[]string `json:"scopes,omitempty"`
	GroupsClaim               *string   `json:"groups_claim,omitempty"`
	Provisioning              *string   `json:"provisioning,omitempty"`
	JIT                       *JITRules `json:"jit,omitempty"`
	AllowPrivateNetwork       *bool     `json:"allow_private_network,omitempty"`
	AllowCrossOriginEndpoints *bool     `json:"allow_cross_origin_endpoints,omitempty"`
	CABundlePEM               *string   `json:"ca_bundle_pem,omitempty"`
	StepUpFallback            *string   `json:"stepup_fallback,omitempty"`
	ACRValues                 *[]string `json:"acr_values,omitempty"`
	EndSessionEnabled         *bool     `json:"end_session_enabled,omitempty"`
	Enabled                   *bool     `json:"enabled,omitempty"`
}

// OIDCProviderList is GET /oidc/providers.
type OIDCProviderList struct {
	Items       []*OIDCProvider `json:"items"`
	RedirectURI string          `json:"redirect_uri"`
}

// OIDCMapping maps a groups-claim value to a role in a domain.
type OIDCMapping struct {
	ID         string `json:"id"`
	ProviderID string `json:"provider_id"`
	GroupValue string `json:"group_value"`
	Role       string `json:"role"`
	Domain     string `json:"domain"`
}

// OIDCMappingInput is POST /oidc/providers/{id}/mappings.
type OIDCMappingInput struct {
	GroupValue string `json:"group_value"`
	Role       string `json:"role"`
	Domain     string `json:"domain"`
}
