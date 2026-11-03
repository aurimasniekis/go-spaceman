package cli

import (
	"context"

	"github.com/spf13/cobra"
)

var (
	accountCols  = []col{c("ID", "id"), c("NAME", "name"), c("KIND", "kind"), c("HARNESS", "harness"), c("ENABLED", "enabled"), c("SECRET V", "secret_version"), c("SPACES", "spaces")}
	promptCols   = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), c("KIND", "kind"), c("LATEST", "latest")}
	skillCols    = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), c("SOURCE", "source"), c("LATEST", "latest")}
	profileCols  = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), c("HARNESS", "harness"), c("MODEL", "model"), c("REVISION", "revision")}
	templateCols = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), c("KIND", "kind"), c("PROFILE", "profile_id"), c("PROMPT", "prompt_id"), c("REVISION", "revision")}
	starterCols  = []col{c("ROLE", "name"), c("KIND", "kind"), c("TEMPLATE", "template"), c("TEMPLATE ID", "template_id"), c("DESCRIPTION", "description")}
	roleCols     = []col{c("NAME", "name"), c("TEMPLATE", "template_id"), c("PROFILE", "profile_id"), c("PROMPT", "prompt_id"), c("MODEL", "model"), c("REVISION", "revision")}
	connCols     = []col{c("ID", "id"), c("NAME", "name"), c("URL", "url"), c("STREAM", "stream"), c("CLIENT", "client_id"), c("ENABLED", "enabled")}
	etplCols     = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), c("BUILTIN", "builtin"), {head: "VERSIONS", fn: countOf("versions")}}
	subCols      = []col{c("ID", "id"), c("NAME", "name"), c("STATE", "state"), c("ENABLED", "enabled"), c("DELIVERY", "delivery"), c("FOLLOW", "follow"), c("SUBJECT", "subject"), c("LAST EVENT", "last_event_at")}
	replayCols   = []col{c("ID", "id"), c("POLICY", "policy"), c("APPLIED", "applied"), c("REASON", "reason"), c("BY", "requested_by"), c("AT", "requested_at")}
	mcpCols      = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), c("URL", "url"), c("ENABLED", "enabled"), c("GRANTS", "grants"), c("HEADERS V", "headers_version")}
	grantCols    = []col{c("ID", "id"), c("SPACE", "space_id"), c("ROLE", "role"), c("TOOLS", "tools"), c("CREATED", "created_at")}
	policyCols   = []col{c("ID", "id"), c("OPERATION", "operation"), c("SPACE", "space_id"), c("USES", "uses"), c("VALID(S)", "valid_seconds"), c("NOTE", "note")}
)

func countOf(key string) func(map[string]any) string {
	return func(m map[string]any) string {
		vs, _ := m[key].([]any)
		return cell(len(vs))
	}
}

func (a *App) accountCmd() *cobra.Command {
	acct := func(f *form) {
		f.str("name", "name", "name").str("kind", "kind", "claude_oauth_token, anthropic_api_key or openai_api_key").
			str("description", "description", "description").boolean("enabled", "enabled", "enabled").kv("meta", "metadata", "metadata").
			secret("secret", "secret", "the credential")
	}
	return group("account", "Harness accounts (credentials): list, show, add, update, delete and bind to spaces", []string{"accounts"},
		a.cmd(opts{use: "list", short: "Accounts (never their secrets)", op: "list-accounts", alias: []string{"ls"},
			view: view{items: "items", cols: accountCols, empty: "No accounts."}}),
		a.cmd(opts{use: "get", short: "Show an account", op: "get-account", args: pos("account")}),
		a.cmd(opts{use: "create", short: "Add an account; the secret comes from a file or standard input", op: "create-account", idem: true, form: acct,
			example: "spaceman account create --name ci-claude --kind claude_oauth_token --secret-file ~/.config/claude-token\n" +
				"pass show claude | spaceman account create --name ci --kind anthropic_api_key --secret-stdin"}),
		a.cmd(opts{use: "update", short: "Edit an account; give --secret-file or --secret-stdin to rotate the secret", op: "update-account",
			get: "get-account", merge: true, args: pos("account"), form: acct}),
		a.cmd(opts{use: "delete", short: "Delete an account no space uses", op: "delete-account", args: pos("account"), done: "Account deleted."}),
		a.cmd(opts{use: "kinds", short: "Account kinds and the harness each serves", op: "list-account-kinds"}),
	)
}

func (a *App) promptCmd() *cobra.Command {
	return group("prompt", "Prompt catalog: list, show, create, draft, publish, diff, fork and roll back", []string{"prompts"},
		a.cmd(opts{use: "list", short: "The prompt catalog", op: "list-prompts", alias: []string{"ls"}, view: view{items: "items", cols: promptCols, empty: "No prompts."}}),
		a.cmd(opts{use: "get", short: "A prompt with its draft and versions (ID or slug)", op: "get-prompt", args: pos("prompt")}),
		a.cmd(opts{use: "create", short: "Add a prompt (with a first draft)", op: "create-prompt", bodyArgs: []string{"slug"},
			form: func(f *form) {
				f.str("name", "name", "name (required)").str("kind", "kind", "role, dream, kb, event or general (required)").
					str("description", "description", "description").text("body", "body", "first draft")
			}}),
		a.cmd(opts{use: "draft", short: "Save the prompt's draft (starts one when there is none)", op: "save-prompt-draft", get: "get-prompt", canonical: "prompt_id",
			args: pos("prompt"), version: draftVersion, optionalIfMatch: true,
			example: "spaceman prompt draft control --body-file control.md --notes \"tighten escalation\"",
			form: func(f *form) {
				f.text("body", "body", "draft text (required)").str("extends", "extends", "a published version of another prompt to build on").
					str("notes", "notes", "change notes")
			}}),
		a.cmd(opts{use: "publish", short: "Publish the draft as the next immutable version", op: "publish-prompt", get: "get-prompt", canonical: "prompt_id", args: pos("prompt"),
			version: draftVersion,
			prepare: needVersion("the prompt has no draft to publish; save one with `spaceman prompt draft`")}),
	)
}

// needVersion refuses a call whose If-Match could not be determined.
func needVersion(msg string) func(ctx context.Context, cmd *cobra.Command, c *call, body map[string]any) error {
	return func(_ context.Context, _ *cobra.Command, c *call, _ map[string]any) error {
		if c.ifMatch == "" {
			return usageErrorf("%s", msg)
		}
		return nil
	}
}

func (a *App) skillCmd() *cobra.Command {
	return group("skill", "Skill catalog: list, show, create, draft and publish", []string{"skills"},
		a.cmd(opts{use: "list", short: "The skill catalog", op: "list-skills", alias: []string{"ls"}, view: view{items: "items", cols: skillCols, empty: "No skills."}}),
		a.cmd(opts{use: "get", short: "A skill with its versions and files", op: "get-skill", args: pos("skill")}),
		a.cmd(opts{use: "publish", short: "Publish the skill's draft", op: "publish-skill", get: "get-skill", canonical: "skill_id", args: pos("skill"), version: draftVersion,
			prepare: needVersion("the skill has no draft to publish")}),
	)
}

func (a *App) profileCmd() *cobra.Command {
	prof := func(f *form) {
		f.str("name", "name", "name").str("model", "model", "model").kv("setting", "settings", "harness setting").
			str("description", "description", "description")
	}
	return group("profile", "Harness profiles: list, show, create, update and delete", []string{"profiles"},
		a.cmd(opts{use: "list", short: "Harness profiles", op: "list-harness-profiles", alias: []string{"ls"},
			view: view{items: "items", cols: profileCols, empty: "No harness profiles."}}),
		a.cmd(opts{use: "get", short: "Show a harness profile (ID or slug)", op: "get-harness-profile", args: pos("profile")}),
		a.cmd(opts{use: "create", short: "Add a harness profile", op: "save-harness-profile", bodyArgs: []string{"slug"},
			form: func(f *form) {
				prof(f)
				f.str("harness", "harness", "claude-code or codex (required)")
			},
			prepare: func(_ context.Context, _ *cobra.Command, c *call, _ map[string]any) error {
				c.params["profile_id"] = "new"
				return nil
			}}),
		a.cmd(opts{use: "update", short: "Edit a harness profile (only the flags given change)", op: "save-harness-profile", get: "get-harness-profile", canonical: "profile_id",
			merge: true, args: pos("profile"), form: prof}),
		a.cmd(opts{use: "delete", short: "Delete a harness profile no role template or space role uses (ID or slug)", op: "delete-harness-profile",
			args: pos("profile"), done: "Profile deleted."}),
	)
}

func (a *App) roleCmd() *cobra.Command {
	role := func(f *form) {
		f.str("template", "template_id", "role template ID (required for a new role)").str("profile", "profile_id", "harness profile ID").
			str("prompt", "prompt_id", "prompt ID").str("prompt-pin", "prompt_pin", "published prompt version to pin (empty follows the latest)").
			str("model", "model", "model").kv("setting", "settings", "harness setting").text("instructions", "instructions", "role instructions")
	}
	tplForm := func(f *form) {
		f.str("name", "name", "name").str("description", "description", "description").str("profile", "profile_id", "harness profile ID").
			str("prompt", "prompt_id", "prompt ID").str("prompt-pin", "prompt_pin", "published prompt version to pin (empty follows the latest)").
			text("memory-conventions", "memory_conventions", "memory conventions").kv("setting", "settings", "harness setting")
	}
	tpl := group("template", "Role templates: list, show, create, update, delete and the starter set", []string{"templates"},
		a.cmd(opts{use: "list", short: "Role templates", op: "list-role-templates", alias: []string{"ls"},
			view: view{items: "items", cols: templateCols, empty: "No role templates."}}),
		a.cmd(opts{use: "get", short: "Show a role template (ID or slug)", op: "get-role-template", args: pos("template")}),
		a.cmd(opts{use: "create", short: "Add a role template", op: "save-role-template", bodyArgs: []string{"slug"},
			form: func(f *form) {
				tplForm(f)
				f.str("kind", "kind", "control, worker, kb, dreamer or diagnostic (required)")
			},
			prepare: func(_ context.Context, _ *cobra.Command, c *call, _ map[string]any) error {
				c.params["template_id"] = "new"
				return nil
			}}),
		a.cmd(opts{use: "update", short: "Edit a role template (only the flags given change)", op: "save-role-template", get: "get-role-template",
			canonical: "template_id", merge: true, args: pos("template"), form: tplForm}),
		a.cmd(opts{use: "delete", short: "Delete a role template no space role uses (ID or slug)", op: "delete-role-template", args: pos("template"),
			done: "Template deleted."}),
		a.cmd(opts{use: "starters", short: "The starter roles and whether each one's template still exists", op: "list-starter-roles",
			view: view{items: "items", cols: starterCols, empty: "No starter roles."}}),
	)
	return group("role", "Roles of a space (--space), their options, and role templates", []string{"roles"},
		a.cmd(opts{use: "list", short: "A space's roles", op: "list-space-roles", alias: []string{"ls"}, scoped: true,
			view: view{items: "items", cols: roleCols, empty: "No roles."}}),
		a.cmd(opts{use: "get", short: "Show one of a space's roles", op: "get-space-role", scoped: true, args: pos("name")}),
		a.cmd(opts{use: "set", short: "Configure a role in a space (creates it, or replaces its configuration)", op: "save-space-role", scoped: true,
			args: pos("name"), form: role}),
		a.cmd(opts{use: "preview", short: "A role's effective configuration and where each value comes from", op: "preview-space-role", scoped: true,
			args: pos("name")}),
		a.cmd(opts{use: "delete", short: "Remove a role from a space", op: "delete-space-role", scoped: true, args: pos("name"), done: "Role removed."}),
		a.cmd(opts{use: "add-starters", short: "Give the space the starter roles it does not have yet (control, planner, implementor, reviewer, kb, dreamer)",
			op: "add-starter-roles", scoped: true, example: "spaceman role add-starters --space payments"}),
		tpl,
	)
}

const whilePausedHelp = "while the space is paused: defer (default: ingest, tell control on resume), wake (ingest, tell control at once) or stop (stop until resume)"

func (a *App) eventCmd() *cobra.Command {
	conn := func(f *form) {
		f.str("name", "name", "name").str("url", "url", "NATS URL of the Pulse server").str("stream", "stream", "JetStream stream").
			boolean("enabled", "enabled", "enabled").textFile("ca", "ca_pem", "PEM file of the CA that signs the server certificate").
			secret("creds", "creds", "Pulse .creds file of the client")
	}
	connections := group("connection", "Pulse connections: list, show, add, update and delete", []string{"connections", "conn"},
		a.cmd(opts{use: "list", short: "Pulse connections (never their credentials)", op: "list-event-connections", alias: []string{"ls"},
			view: view{items: "items", cols: connCols, empty: "No connections."}}),
		a.cmd(opts{use: "get", short: "Show a Pulse connection", op: "get-event-connection", args: pos("connection")}),
		a.cmd(opts{use: "create", short: "Add a Pulse connection", op: "create-event-connection", form: conn,
			example: "spaceman event connection create --name pulse --url tls://pulse.example.com:4222 --enabled --creds-file spaceman.creds",
			prepare: func(_ context.Context, cmd *cobra.Command, _ *call, body map[string]any) error {
				if !cmd.Flags().Changed("enabled") {
					if _, ok := body["enabled"]; !ok {
						body["enabled"] = true
					}
				}
				return nil
			}}),
		a.cmd(opts{use: "update", short: "Edit a Pulse connection; --creds-file replaces its credentials", op: "update-event-connection",
			get: "get-event-connection", merge: true, args: pos("connection"), form: conn}),
		a.cmd(opts{use: "delete", short: "Delete an unused Pulse connection", op: "delete-event-connection", args: pos("connection"), done: "Connection deleted."}),
	)
	templates := group("template", "Event templates: list, show, create, save a draft, preview and publish", []string{"templates"},
		a.cmd(opts{use: "list", short: "The event template catalog", op: "list-event-templates", alias: []string{"ls"},
			view: view{items: "items", cols: etplCols, empty: "No event templates."}}),
		a.cmd(opts{use: "get", short: "A template with its versions", op: "get-event-template", args: pos("template")}),
		a.cmd(opts{use: "create", short: "Add a template (as a draft)", op: "create-event-template", bodyArgs: []string{"slug"},
			form: func(f *form) {
				f.str("name", "name", "name (required)").str("description", "description", "description").
					doc("definition", "definition", "template definition (JSON or YAML file; required)")
			}}),
		a.cmd(opts{use: "draft", short: "Save a template's draft", op: "save-template-draft", args: pos("template"),
			form: func(f *form) {
				f.doc("definition", "definition", "template definition (JSON or YAML file; required)").str("notes", "notes", "change notes")
			}}),
		a.cmd(opts{use: "publish", short: "Publish a template version (subscriptions following the latest move to it)", op: "publish-template",
			args: pos("version-id")}),
	)
	subs := group("subscription", "A space's event subscriptions: list, show, add, update, delete, replay and quarantine (--space)", []string{"subscriptions", "sub"},
		a.cmd(opts{use: "list", short: "A space's subscriptions", op: "list-subscriptions", alias: []string{"ls"}, scoped: true,
			view: view{items: "items", cols: subCols, empty: "No subscriptions."}}),
		a.cmd(opts{use: "get", short: "Show a subscription", op: "get-subscription", scoped: true, args: pos("subscription")}),
		a.cmd(opts{use: "create", short: "Subscribe the space to events of a connection", op: "create-subscription", scoped: true,
			form: func(f *form) {
				f.str("name", "name", "name (required)").str("connection", "connection_id", "connection ID (required)").
					str("template-version", "template_version_id", "template version ID").str("follow", "follow", "pinned or latest").
					kv("var", "vars", "template variable").doc("custom", "custom", "custom template definition (JSON or YAML file)").
					doc("start", "start", "start cursor (JSON or YAML file)").
					str("delivery", "delivery", "durable (default, a JetStream durable) or core (plain NATS Core: non-durable, no replay or redelivery)").
					str("while-paused", "while_paused", whilePausedHelp)
			}}),
		a.cmd(opts{use: "update", short: "Edit a subscription (only the flags given change)", op: "update-subscription", get: "get-subscription",
			merge: true, drop: []string{"custom", "start"}, scoped: true, args: pos("subscription"),
			form: func(f *form) {
				f.str("name", "name", "name").boolean("enabled", "enabled", "enabled").str("template-version", "template_version_id", "template version ID").
					str("follow", "follow", "pinned or latest").kv("var", "vars", "template variable (replaces the variables)").
					str("while-paused", "while_paused", whilePausedHelp)
			}}),
		a.cmd(opts{use: "delete", short: "Delete a subscription", op: "delete-subscription", scoped: true, args: pos("subscription"), done: "Subscription deleted."}),
		a.cmd(opts{use: "replay", short: "Replay a subscription's events from a point (recorded with a reason)", op: "replay-subscription",
			get: "get-subscription", scoped: true, args: pos("subscription"),
			example: "spaceman event subscription replay --space payments sub_01... --policy time --time 2026-11-01T00:00:00Z \\\n" +
				"  --reason \"template bug dropped PR events\"",
			form: func(f *form) {
				f.str("policy", "policy", "all, time or seq (required)").str("time", "time", "with time: RFC 3339 point to replay from").
					integer("seq", "seq", "with seq: stream sequence to replay from").str("reason", "reason", "why (required, recorded in the audit log)").
					boolean("fresh-items", "fresh_items", "create new inbox items even for events already delivered")
			},
			prepare: func(_ context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
				if r, _ := body["reason"].(string); r == "" {
					return usageErrorf("--reason is required: replays are recorded with why they were made")
				}
				return nil
			}}),
		a.cmd(opts{use: "replays", short: "A subscription's replays", op: "list-subscription-replays", scoped: true, args: pos("subscription"),
			view: view{items: "items", cols: replayCols, empty: "No replays."}}),
		a.cmd(opts{use: "deliveries", short: "A subscription's recent deliveries", op: "list-subscription-delivery", scoped: true, args: pos("subscription"),
			query: []qflag{limitFlag()}}),
	)
	return group("event", "Events: Pulse connections, templates and subscriptions", []string{"events"}, connections, templates, subs)
}

func (a *App) mcpCmd() *cobra.Command {
	srv := func(f *form) {
		f.str("name", "name", "name").str("url", "url", "streamable HTTP URL of the MCP server").str("description", "description", "description").
			boolean("enabled", "enabled", "enabled").list("tool", "tools", "tool to expose (default: all)").
			boolean("allow-private", "allow_private", "allow a private-network URL").integer("call-timeout-ms", "call_timeout_ms", "call timeout").
			integer("max-concurrency", "max_concurrency", "concurrent calls").integer("max-result-bytes", "max_result_bytes", "largest result").
			secretDoc("headers", "headers", "JSON or YAML file with the request headers to send (they hold credentials; - reads standard input)")
	}
	withHeaders := func(_ context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
		if _, ok := body["headers"]; ok {
			body["set_headers"] = true
		} else {
			delete(body, "set_headers")
		}
		return nil
	}
	servers := group("server", "Upstream MCP servers: list, show, add, update, check and delete", []string{"servers"},
		a.cmd(opts{use: "list", short: "MCP servers", op: "list-mcp-servers", alias: []string{"ls"}, view: view{items: "items", cols: mcpCols, empty: "No MCP servers."}}),
		a.cmd(opts{use: "get", short: "Show an MCP server (never its header values)", op: "get-mcp-server", args: pos("server")}),
		a.cmd(opts{use: "create", short: "Add an MCP server", op: "create-mcp-server", idem: true, bodyArgs: []string{"slug"}, form: srv, prepare: withHeaders}),
		a.cmd(opts{use: "update", short: "Edit an MCP server; --headers-file replaces its headers", op: "update-mcp-server", get: "get-mcp-server", canonical: "server_id",
			merge: true, args: pos("server"), form: srv, prepare: withHeaders}),
		a.cmd(opts{use: "check", short: "Connect to the server and list its tools", op: "check-mcp-server", args: pos("server")}),
		a.cmd(opts{use: "delete", short: "Delete an MCP server", op: "delete-mcp-server", args: pos("server"), done: "MCP server deleted."}),
	)
	grants := group("grant", "Which spaces and roles may call an MCP server's tools", []string{"grants"},
		a.cmd(opts{use: "list", short: "An MCP server's grants", op: "list-mcp-grants", alias: []string{"ls"}, args: pos("server"),
			view: view{items: "items", cols: grantCols, empty: "No grants."}}),
		a.cmd(opts{use: "add", short: "Grant a space (and optionally one role) the server's tools", op: "add-mcp-grant", args: pos("server"),
			form: func(f *form) {
				f.str("space", "space_id", "space ID or slug (required)").str("role", "role", "only this role").list("tool", "tools", "only this tool")
			},
			prepare: func(ctx context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
				s, _ := body["space_id"].(string)
				id, err := a.resolveSpace(ctx, s)
				if err != nil {
					return err
				}
				body["space_id"] = id
				return nil
			}}),
		a.cmd(opts{use: "revoke", short: "Remove a grant", op: "revoke-mcp-grant", args: pos("server", "grant"), done: "Grant removed."}),
	)
	return group("mcp", "Central MCP gateway: servers, grants and a space's servers", nil, servers, grants)
}

func (a *App) approvalPolicyCmd() *cobra.Command {
	return group("approval-policy", "Operations that need a person's approval", []string{"approval-policies"},
		a.cmd(opts{use: "list", short: "Approval policies", op: "list-approval-policies", alias: []string{"ls"},
			view: view{items: "items", cols: policyCols, empty: "No approval policies."}}),
		a.cmd(opts{use: "get", short: "Show an approval policy", op: "get-approval-policy", args: pos("policy")}),
		a.cmd(opts{use: "create", short: "Make an operation need approval", op: "create-approval-policy", bodyArgs: []string{"operation"},
			long: "OPERATION is spaceman.<tool> (a built-in agent tool) or mcp.<server>.<tool> (a gateway tool); a prefix ending in .* covers several.",
			form: func(f *form) {
				f.str("space", "space_id", "only in this space (ID or slug; default every space)").integer("uses", "uses", "how many calls one approval allows").
					integer("valid-seconds", "valid_seconds", "how long an approval stays valid").
					integer("request-ttl-seconds", "request_ttl_seconds", "how long a request waits for a decision").str("note", "note", "note")
			},
			prepare: func(ctx context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
				if s, _ := body["space_id"].(string); s != "" {
					id, err := a.resolveSpace(ctx, s)
					if err != nil {
						return err
					}
					body["space_id"] = id
				}
				return nil
			}}),
		a.cmd(opts{use: "delete", short: "Remove a policy (approvals it produced stay usable until they expire)", op: "delete-approval-policy",
			args: pos("policy"), done: "Approval policy removed."}),
	)
}
