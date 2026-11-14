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
	return group("telegram", "The Telegram bot (ADR-0083)", nil, bot)
}
