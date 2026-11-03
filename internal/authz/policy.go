package authz

import "slices"

// Role is a built-in role (SM-AUTH-003).
type Role string

// Built-in roles, weakest first.
const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// Roles lists the built-in roles, weakest first.
var Roles = []Role{RoleViewer, RoleOperator, RoleAdmin}

// Valid reports whether r is a built-in role.
func (r Role) Valid() bool { return slices.Contains(Roles, r) }

func (r Role) atLeast(o Role) bool {
	return slices.Index(Roles, r) >= slices.Index(Roles, o) && r.Valid() && o.Valid()
}

// Permission is one (object, action) pair. GlobalOnly permissions are only ever granted in the "*" domain: a space
// admin cannot manage nodes, users or catalogs.
type Permission struct {
	Object     string
	Action     string
	GlobalOnly bool
}

func (p Permission) valid() bool { return p.Object != "" && p.Action != "" }

// String renders obj:act.
func (p Permission) String() string { return p.Object + ":" + p.Action }

func perm(obj, act string) Permission { return Permission{Object: obj, Action: act} }
func global(obj, act string) Permission {
	return Permission{Object: obj, Action: act, GlobalOnly: true}
}

// The permission catalog. Separate objects exist for terminals, recordings, secrets, queues, human actions and
// approvals, nodes, templates, subscriptions and replay, MCP and skills, KB and global administration (SM-AUTH-004).
var (
	SpaceRead  = perm("space", "read")
	SpaceWrite = perm("space", "write")
	// SpaceCreate and SpaceDelete are global: creating or purging a space is administration across the installation.
	SpaceCreate = global("space", "create")
	SpacePurge  = global("space", "purge")
	SpaceMove   = global("space", "relocate")
	// SpaceExport downloads a space's records and files as a plaintext archive (ADR-0045); only admins hold it.
	SpaceExport = perm("space", "export")

	NodeRead   = global("node", "read")
	NodeWrite  = global("node", "write")
	NodeEnroll = global("node", "enroll")

	RunRead    = perm("run", "read")
	RunControl = perm("run", "control") // launch, stop, interrupt, restart control
	// RunDiag is needed, on top of run:control, to launch a diagnostic run (audited as every launch, SM-CTRL-004).
	RunDiag = perm("run", "diagnose")

	TerminalWatch    = perm("terminal", "watch")
	TerminalTakeover = perm("terminal", "takeover")
	RecordingRead    = perm("recording", "read")
	RecordingDelete  = perm("recording", "delete")

	InboxRead    = perm("inbox", "read")
	InboxWrite   = perm("inbox", "write")
	QueueRead    = perm("queue", "read")
	QueueWrite   = perm("queue", "write") // reorder, cancel, reassign
	MessageRead  = perm("message", "read")
	MessageWrite = perm("message", "write") // send to agents, acknowledge, reply and resolve operator messages

	HumanRead      = perm("human_action", "read")
	HumanRespond   = perm("human_action", "respond") // answer, complete, acknowledge, comment, assign
	ApprovalDecide = perm("approval", "decide")      // approve or deny permission requests

	ApprovalPolicyRead  = perm("approval_policy", "read")
	ApprovalPolicyWrite = global("approval_policy", "write") // which operations need approval

	AttachmentRead  = perm("attachment", "read")
	AttachmentWrite = perm("attachment", "write")

	SubscriptionRead  = perm("subscription", "read")
	SubscriptionWrite = perm("subscription", "write")
	ReplayExecute     = perm("replay", "execute")

	KBRead      = perm("kb", "read")
	KBWrite     = perm("kb", "write")
	MemoryRead  = perm("memory", "read")
	MemoryWrite = perm("memory", "write")

	SpaceConfigRead  = perm("space_config", "read") // roles, prompts bindings, limits, policies of a space
	SpaceConfigWrite = perm("space_config", "write")
	// RetentionWrite changes a space's retention periods (ADR-0045): a shorter period deletes data in the next daily
	// pass, so it is held by admins only, like recording:delete, on top of space_config:write.
	RetentionWrite = perm("retention", "write")

	// Cross-space access (ADR-0038): a space's permissions per peer and its incoming and outgoing requests.
	XSpaceRead  = perm("xspace", "read")
	XSpaceWrite = perm("xspace", "write")

	SecretWrite = perm("secret", "write") // write-only: secrets are never readable through any permission

	TemplateRead  = perm("template", "read")
	TemplateWrite = global("template", "write")
	// HumanTypePermissionWrite changes who may see, create and respond to a human-action type in a space
	// (SM-HUI-008, ADR-0050): authorization, so admins only.
	HumanTypePermissionWrite = perm("human_type_permission", "write")
	HumanTypeRead            = perm("human_type", "read")
	HumanTypeWrite           = global("human_type", "write")
	PromptRead               = perm("prompt", "read")
	PromptWrite              = global("prompt", "write")
	MCPRead                  = perm("mcp", "read")
	MCPWrite                 = global("mcp", "write")
	SkillRead                = perm("skill", "read")
	SkillWrite               = global("skill", "write")
	AccountRead              = global("account", "read")
	AccountWrite             = global("account", "write")
	ConnectionRead           = global("event_connection", "read")
	ConnectionWrite          = global("event_connection", "write")

	UserRead    = global("user", "read")
	UserWrite   = global("user", "write")
	TokenAdmin  = global("token", "admin") // see and revoke other users' tokens
	AuditRead   = global("audit", "read")
	BackupRun   = global("backup", "run")
	SystemRead  = global("system", "read")
	SystemWrite = global("system", "write")
)

// rolePermissions defines what each role may do; stronger roles include the weaker roles' permissions.
var rolePermissions = map[Role][]Permission{
	RoleViewer: {
		SpaceRead, RunRead, InboxRead, QueueRead, MessageRead, HumanRead, AttachmentRead, SubscriptionRead, KBRead,
		MemoryRead, SpaceConfigRead, TemplateRead, HumanTypeRead, PromptRead, MCPRead, SkillRead, NodeRead, ApprovalPolicyRead, XSpaceRead,
	},
	RoleOperator: {
		SpaceWrite, RunControl, RunDiag, TerminalWatch, RecordingRead, InboxWrite, QueueWrite, MessageWrite, HumanRespond, ApprovalDecide,
		AttachmentWrite, SubscriptionWrite, ReplayExecute, KBWrite, MemoryWrite, SpaceConfigWrite, ConnectionRead, XSpaceWrite,
	},
	// Admins hold every permission ("*"), including terminal takeover, recording deletion, secrets and,
	// in the "*" domain, global administration.
}

// policyRows returns the Casbin p rows (role, obj, act).
func policyRows() [][]string {
	rows := [][]string{{string(RoleAdmin), "*", "*"}}
	for i, r := range Roles[:len(Roles)-1] {
		for _, inherited := range Roles[:i+1] {
			for _, p := range rolePermissions[inherited] {
				rows = append(rows, []string{string(r), p.Object, p.Action})
			}
		}
	}
	return rows
}

// Permissions returns the effective permission list of a role (for documentation and the UI).
func Permissions(r Role) []Permission {
	if r == RoleAdmin {
		return []Permission{{Object: "*", Action: "*"}}
	}
	var out []Permission
	for _, inherited := range Roles[:slices.Index(Roles, r)+1] {
		out = append(out, rolePermissions[inherited]...)
	}
	return out
}
