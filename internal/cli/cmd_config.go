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
		f.str("name", "name", "name").str("kind", "kind", "claude_oauth_token, anthropic_api_key, openai_api_key or codex_chatgpt_login").
			str("description", "description", "description").boolean("enabled", "enabled", "enabled").kv("meta", "metadata", "metadata").
			secret("secret", "secret", "the credential")
	}
	return group("account", "Harness accounts (credentials): list, show, add, update, delete and bind to spaces", []string{"accounts"},
		a.cmd(opts{use: "list", short: "Accounts (never their secrets)", op: "list-accounts", alias: []string{"ls"},
			view: view{items: "items", cols: accountCols, empty: "No accounts."}}),
		a.cmd(opts{use: "get", short: "Show an account", op: "get-account", args: pos("account")}),
		a.cmd(opts{use: "create", short: "Add an account; the secret comes from a file or standard input", op: "create-account", idem: true, form: acct,
			example: "spaceman account create --name ci-claude --kind claude_oauth_token --secret-file ~/.config/claude-token\n" +
				"pass show claude | spaceman account create --name ci --kind anthropic_api_key --secret-stdin\n" +
				"spaceman account create --name codex-plan --kind codex_chatgpt_login --secret-file ~/.codex/auth.json"}),
		a.cmd(opts{use: "update", short: "Edit an account; give --secret-file or --secret-stdin to rotate the secret", op: "update-account",
			get: "get-account", merge: true, args: pos("account"), form: acct}),
		a.cmd(opts{use: "delete", short: "Delete an account no space uses", op: "delete-account", args: pos("account"), done: "Account deleted."}),
		a.cmd(opts{use: "kinds", short: "Account kinds and the harness each serves", op: "list-account-kinds"}),
	)
}

var gitKeyCols = []col{c("ID", "id"), c("NAME", "name"), c("FINGERPRINT", "fingerprint"), c("ALL SPACES", "all_spaces"), c("UPDATED", "updated_at")}

func (a *App) gitKeyCmd() *cobra.Command {
	keyForm := func(f *form) {
		f.str("name", "name", "unique name").str("description", "description", "description").
			boolean("all-spaces", "all_spaces", "let every space use the key").list("space", "spaces", "space ID that may use the key")
	}
	return group("git-key", "Shared Git SSH keys (ADR-0058): generate, list, show, edit, rotate and delete", []string{"git-keys"},
		a.cmd(opts{use: "list", short: "Shared Git SSH keys (never their private halves)", op: "list-git-ssh-keys", alias: []string{"ls"},
			view: view{items: "items", cols: gitKeyCols, empty: "No Git SSH keys."}}),
		a.cmd(opts{use: "get", short: "Show a key with its public half, spaces and the repositories that use it", op: "get-git-ssh-key", args: pos("key")}),
		a.cmd(opts{use: "create", short: "Generate a key and print its public half", op: "create-git-ssh-key", form: keyForm,
			long: "SpaceMan generates an Ed25519 key and keeps the private half. Add the printed public key to the Git account the spaces " +
				"may clone with (a read-only machine user is safest: a key on a person's account can also push), then choose the key on " +
				"the spaces' repositories.",
			example: "spaceman git-key create --name github-bot --space spc_… --description \"read-only machine user\"",
			view:    view{first: []string{"public_key"}}}),
		a.cmd(opts{use: "update", short: "Rename a key or change the spaces that may use it", op: "update-git-ssh-key", get: "get-git-ssh-key", merge: true,
			args: pos("key"), form: keyForm}),
		a.cmd(opts{use: "rotate", short: "Replace a key (add the new public key where the old one was)", op: "rotate-git-ssh-key", get: "get-git-ssh-key",
			args: pos("key"), confirm: "replace the key (the old one stops working at once)", view: view{first: []string{"public_key"}}}),
		a.cmd(opts{use: "delete", short: "Delete a key no repository uses", op: "delete-git-ssh-key", args: pos("key"), done: "Git SSH key deleted."}),
		a.cmd(opts{use: "for-space", short: "The keys a space's repositories may use", op: "list-space-git-ssh-keys", scoped: true,
			view: view{items: "items", cols: gitKeyCols, empty: "No Git SSH keys for this space."}}),
	)
}

func (a *App) harborPilotCmd() *cobra.Command {
	connForm := func(f *form) {
		f.str("base-url", "base_url", "gateway origin SpaceMan calls (https unless loopback or --allow-insecure-http)").
			str("run-base-url", "run_base_url", "origin runs reach the gateway on, when it differs (needs https)").
			boolean("allow-insecure-http", "allow_insecure_http", "allow plain http: tokens then cross the network in clear text").
			secret("parent-token", "parent_token", "parent gateway credential (hpk_…) with tokens:mint; required when connecting or changing the gateway URL").
			secret("service-token", "service_token", "service-principal token (hpa_…) with usage.read and config.read; give it again (or --remove-service-token) when changing the gateway URL").
			boolean("remove-service-token", "remove_service_token", "remove the service-principal token")
	}
	spaceForm := func(f *form) {
		f.boolean("enabled", "enabled", "route the space's runs of the listed harnesses through Harbor Pilot").
			str("project", "project_id", "Harbor Pilot project id runs are charged to (required when enabled; setting or changing it needs account:write)").
			list("harness", "harnesses", "harness kind routed through Harbor Pilot (default: claude-code and codex)").
			integer("ttl", "ttl_seconds", "run token lifetime in seconds, 900 to 604800 (default 86400)").
			str("on-budget-status-unavailable", "on_budget_status_unavailable", "proceed or refuse").
			list("model", "allowed_model_aliases", "model alias runs may use (default: any)")
	}
	return group("harbor-pilot", "The Harbor Pilot gateway (ADR-0060): the connection and a space's settings", []string{"hp"},
		a.cmd(opts{use: "get", short: "The connection (never its tokens)", op: "get-harbor-pilot-connection"}),
		a.cmd(opts{use: "connect", short: "Connect Harbor Pilot or change the connection (checked with Harbor Pilot first)", op: "put-harbor-pilot-connection",
			get: "get-harbor-pilot-connection", getOptional: true, merge: true, form: connForm,
			long: "Saving mints a probe token with the parent token and revokes it at once: it shows the parent holds tokens:mint, its client " +
				"binding and expiry, and that the gateway is Harbor Pilot 0.1.3 or later. Tokens come from a file or standard input and are " +
				"never shown again; omit them to keep the current ones. Stored tokens are only sent to the address they were entered for: " +
				"changing --base-url or --run-base-url, or turning on --allow-insecure-http, needs --parent-token (and --service-token or " +
				"--remove-service-token when one is stored) again. Needs account:write.",
			example: "spaceman harbor-pilot connect --base-url https://harbor-pilot.example.com --parent-token-file ~/.hp/parent --service-token-file ~/.hp/reader"}),
		a.cmd(opts{use: "version", short: "The connected Harbor Pilot's version (from its GET /api/v1/system, 0.1.4 or later)", op: "get-harbor-pilot-gateway"}),
		a.cmd(opts{use: "disconnect", short: "Remove the connection (refused while a space uses it)", op: "delete-harbor-pilot-connection",
			get: "get-harbor-pilot-connection", confirm: "remove the Harbor Pilot connection", done: "Harbor Pilot connection removed."}),
		a.cmd(opts{use: "space", short: "A space's Harbor Pilot settings", op: "get-space-harbor-pilot", scoped: true}),
		a.cmd(opts{use: "status", short: "A space's Harbor Pilot budget status (hard limits) and project usage", op: "get-space-harbor-pilot-status", scoped: true}),
		a.cmd(opts{use: "set-space", short: "Set a space's Harbor Pilot settings (only the flags given change)", op: "put-space-harbor-pilot",
			get: "get-space-harbor-pilot", merge: true, optionalIfMatch: true, scoped: true, form: spaceForm,
			long: "Needs space_config:write. The project decides who pays, so setting or changing --project needs account:write " +
				"(a global administrator); without --project the stored project is kept.",
			example: "spaceman harbor-pilot set-space --space payments --enabled --project prj_01… --harness codex --ttl 43200"}),
		a.cmd(opts{use: "unset-space", short: "Remove a space's Harbor Pilot settings (it no longer routes runs through Harbor Pilot)",
			op: "delete-space-harbor-pilot", get: "get-space-harbor-pilot", scoped: true, done: "Harbor Pilot settings removed."}),
	)
}

func (a *App) telegramCmd() *cobra.Command {
	settingsForm := func(f *form) {
		f.str("link-default-max-role", "link_default_max_role", "cap a new link gets: viewer or operator (default operator)").
			boolean("link-allow-admin-cap", "link_allow_admin_cap", "allow the admin cap on a link at all").
			boolean("link-sso-grants", "link_sso_grants", "a link confirmed from an SSO session also carries the SSO-mapped grants, until that session ends").
			integer("link-reconfirm-days", "link_reconfirm_days", "days until a link must be confirmed again, 7 to 365 (default 90)").
			integer("links-per-user", "links_per_user", "Telegram accounts per user, 1 to 3 (default 1)").
			integer("link-code-ttl", "link_code_ttl_seconds", "link code lifetime in seconds, 120 to 1800 (default 600)").
			integer("init-data-max-age", "init_data_max_age_seconds", "oldest Mini App initData accepted, 60 to 3600 seconds (default 300)").
			integer("miniapp-session-idle", "miniapp_session_idle_seconds", "Mini App session idle timeout, 300 to 3600 seconds (default 900)").
			integer("miniapp-session-absolute", "miniapp_session_absolute_seconds", "Mini App session lifetime, 900 to 43200 seconds (default 3600)").
			boolean("topics-mode-default", "topics_mode_default", "new links start in topics mode")
	}
	spaceForm := func(f *form) {
		f.boolean("enabled", "enabled", "make the space visible through Telegram").
			str("content-level", "content_level", "full, summary or off (default full)").
			str("enforced-approvals", "enforced_approvals", "web_only or confirm (default web_only)").
			boolean("topics", "topics", "give the space its own topic for users in topics mode").
			boolean("terminal-watch", "terminal_watch", "allow watching run terminals (read-only) from the Mini App")
	}
	return group("telegram", "The Telegram bot's policy and defaults, and a space's Telegram settings (ADR-0083)", nil,
		a.cmd(opts{use: "settings", short: "The bot's policy and defaults (effective values)", op: "get-telegram-settings"}),
		a.cmd(opts{use: "set", short: "Replace the bot's policy and defaults (what isn't given takes its default)", op: "put-telegram-settings",
			get: "get-telegram-settings", optionalIfMatch: true, form: settingsForm,
			long: "The settings are replaced as a whole: a field neither in --file nor given as a flag takes its built-in default, and " +
				"only what was given is stored. The send_limits, inbound_limits and notification_defaults blocks are set with --file. " +
				"The Bot API base URL is not here: it is set only in central's configuration file. Needs system:write and a step-up.",
			example: "spaceman telegram set --file telegram.yaml --links-per-user 2"}),
		a.cmd(opts{use: "reset", short: "Reset the bot's policy and defaults to the built-in defaults", op: "delete-telegram-settings",
			get: "get-telegram-settings", confirm: "reset the Telegram settings to their defaults", done: "Telegram settings reset."}),
		a.cmd(opts{use: "spaces", short: "Spaces with Telegram settings", op: "list-space-telegram",
			view: view{items: "items", cols: []col{c("SPACE", "space_id"), c("ENABLED", "enabled"), c("CONTENT", "content_level"),
				c("APPROVALS", "enforced_approvals"), c("VERSION", "version")}, empty: "No space has Telegram settings."}}),
		a.cmd(opts{use: "space", short: "A space's Telegram settings", op: "get-space-telegram", scoped: true}),
		a.cmd(opts{use: "set-space", short: "Set a space's Telegram settings (only the flags given change)", op: "put-space-telegram",
			get: "get-space-telegram", merge: true, optionalIfMatch: true, scoped: true, form: spaceForm,
			long:    "A space is invisible to Telegram until enabled. Needs space_config:write in the space and a step-up.",
			example: "spaceman telegram set-space --space payments --enabled --content-level summary"}),
		a.cmd(opts{use: "unset-space", short: "Remove a space's Telegram settings (Telegram is off for it again)", op: "delete-space-telegram",
			get: "get-space-telegram", scoped: true, done: "Telegram settings removed."}),
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
	sm := func(f *form) {
		f.str("name", "name", "name").str("description", "description", "description").boolean("enabled", "enabled", "enabled").
			str("preset", "preset", "tool set: read, operate, admin (read, write and dangerous; the default) or custom").
			list("tool", "tools", "add a tool by name (without the slug); dangerous and sensitive-read tools only this way or with admin").
			str("approval", "approval", "none (default: only dangerous tools need an approval) or writes (write tools too)").
			integer("call-timeout-ms", "call_timeout_ms", "call timeout").integer("max-concurrency", "max_concurrency", "concurrent calls").
			integer("max-result-bytes", "max_result_bytes", "largest result")
	}
	token := func(f *form) {
		f.list("grant", "grants", grantHelp+"; default admin on every space with the admin preset").
			integer("expires-in-days", "expires_in_days", "token expiry in days, 1-366 (default: never)")
	}
	spacemanBase := func(o map[string]any) map[string]any {
		b := map[string]any{}
		for _, k := range []string{"name", "description", "enabled", "call_timeout_ms", "max_concurrency", "max_result_bytes"} {
			if v, ok := o[k]; ok {
				b[k] = v
			}
		}
		if s, ok := o["spaceman"].(map[string]any); ok {
			for _, k := range []string{"preset", "tools", "approval"} {
				if v, ok := s[k]; ok {
					b[k] = v
				}
			}
		}
		return b
	}
	spaceman := group("spaceman", "SpaceMan MCP servers: SpaceMan's own API as tools for a space's runs (ADR-0063)", nil,
		a.cmd(opts{use: "create", short: "Add a SpaceMan MCP server with a new internal token owned by you", op: "create-spaceman-mcp", idem: true,
			bodyArgs: []string{"slug"},
			long: "The server acts through an internal API token you own, with grants within yours (default: admin on every space) and no secret. " +
				"Dangerous tools always need a person's approval. Grant the server to a space role (`spaceman mcp grant add`) for its runs to see the tools.",
			example: "spaceman mcp spaceman create infra --name SpaceMan --enabled",
			form:    func(f *form) { sm(f); token(f) }, prepare: a.grantsPrepare}),
		a.cmd(opts{use: "update", short: "Edit a SpaceMan MCP server's tools, approval mode and limits", op: "update-spaceman-mcp", get: "get-mcp-server",
			canonical: "server_id", base: spacemanBase, args: pos("server"), form: sm}),
		a.cmd(opts{use: "reissue", short: "Replace a SpaceMan MCP server's token with a new one owned by you (new grants, expiry or owner)",
			op: "reissue-spaceman-mcp", get: "get-mcp-server", canonical: "server_id", args: pos("server"), form: token, prepare: a.grantsPrepare}),
		a.cmd(opts{use: "tools", short: "Tools a SpaceMan MCP server can offer, for a preset, named tools and grants", op: "list-spaceman-mcp-tools",
			example: "spaceman mcp spaceman tools --preset operate --grants operator@spc_123",
			view:    view{items: "items", cols: []col{c("TOOL", "name"), c("CLASS", "class"), c("SELECTED", "selected"), c("REACHABLE", "reachable"), c("SUMMARY", "summary")}}}),
	)
	return group("mcp", "Central MCP gateway: servers, grants and a space's servers", nil, servers, grants, spaceman)
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
