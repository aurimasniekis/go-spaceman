package cli

import "github.com/spf13/cobra"

func (a *App) telegramCmd() *cobra.Command {
	botForm := func(f *form) {
		f.secret("bot-token", "bot_token", "bot token from BotFather; required when configuring, omit to keep the stored one").
			boolean("enabled", "enabled", "run the bot").
			str("mode", "mode", "webhook (default; needs an https public_url) or polling").
			str("miniapp-host", "miniapp_host", "the Mini App's own host, e.g. tg.central.example.com (empty: Mini App off)").
			list("webhook-ip-allowlist", "webhook_ip_allowlist", "CIDR the webhook accepts requests from, e.g. 149.154.160.0/20 (empty: no check)")
	}
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
			boolean("topics-mode-default", "topics_mode_default", "deprecated, no effect (notification_defaults.threads chooses the chat layout)")
	}
	spaceForm := func(f *form) {
		f.boolean("enabled", "enabled", "make the space visible through Telegram").
			str("content-level", "content_level", "full, summary or off (default full)").
			str("enforced-approvals", "enforced_approvals", "web_only or confirm (default web_only)").
			boolean("topics", "topics", "deprecated, no effect (the chat layout is chosen per link)").
			boolean("terminal-watch", "terminal_watch", "allow watching run terminals (read-only) from the Mini App").
			integer("group-chat-id", "group_chat_id", "bind the forum supergroup with this chat ID (negative; the bot must be in it) to the space, 0 to unbind; admins only")
	}
	bot := group("bot", "The bot's connection: token, mode, Mini App host and health (ADR-0083)", nil,
		a.cmd(opts{use: "get", short: "The bot's connection and health (never its token)", op: "get-telegram-bot"}),
		a.cmd(opts{use: "set", short: "Configure the bot or change its configuration (only the flags given change)", op: "put-telegram-bot",
			get: "get-telegram-bot", getOptional: true, merge: true, form: botForm,
			long: "A new token is checked with Telegram (getMe) before it is stored; it comes from a file or standard input and is never " +
				"shown again. Configuring a different bot revokes every Telegram link. Needs system:write and a step-up.",
			example: "spaceman telegram bot set --bot-token-file ~/.tg/token --enabled --mode polling"}),
		a.cmd(opts{use: "delete", short: "Remove the bot's configuration (Telegram off; links stay)", op: "delete-telegram-bot",
			get: "get-telegram-bot", confirm: "remove the Telegram bot's configuration", done: "Telegram bot configuration removed."}),
	)
	return group("telegram", "The Telegram bot: connection, policy and defaults, and a space's Telegram settings (ADR-0083)", nil,
		bot,
		a.cmd(opts{use: "settings", short: "The bot's policy and defaults (effective values)", op: "get-telegram-settings"}),
		a.cmd(opts{use: "set", short: "Replace the bot's policy and defaults (what isn't given takes its default)", op: "put-telegram-settings",
			get: "get-telegram-settings", optionalIfMatch: true, form: settingsForm,
			long: "The settings are replaced as a whole: a field neither in --file nor given as a flag takes its built-in default, and " +
				"only what was given is stored. The send_limits, inbound_limits and notification_defaults blocks are set with --file. " +
				"The Bot API base URL is not here: it is set only in central's configuration file. Needs system:write and a step-up.",
			example: "spaceman telegram set --file telegram.yaml --links-per-user 2"}),
		a.cmd(opts{use: "reset", short: "Reset the bot's policy and defaults to the built-in defaults", op: "delete-telegram-settings",
			get: "get-telegram-settings", confirm: "reset the Telegram settings to their defaults", done: "Telegram settings reset."}),
		a.cmd(opts{use: "links", short: "Every linked Telegram account", op: "list-telegram-links",
			view: view{cols: []col{c("ID", "id"), c("USER", "user_id"), c("TELEGRAM", "telegram.username"), c("STATE", "state"), c("CAP", "max_role"),
				c("RECONFIRM BY", "reconfirm_due_at")}, empty: "No Telegram account is linked."}}),
		a.cmd(opts{use: "unlink", short: "Unlink a Telegram account at once", op: "delete-telegram-link", args: pos("link"),
			confirm: "unlink this Telegram account", done: "Telegram account unlinked.",
			long: "Ends the link: the account's next message is refused and its grants are removed. Needs user:write and a step-up."}),
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
