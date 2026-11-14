package cli

import (
	"context"

	"github.com/spf13/cobra"
)

var (
	oidcCols     = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "display_name"), c("ISSUER", "issuer"), c("PROVISIONING", "provisioning"), c("ENABLED", "enabled"), c("SECRET", "has_client_secret"), c("TESTED", "test_login_ok_at")}
	mappingCols  = []col{c("ID", "id"), c("GROUP", "group_value"), c("ROLE", "role"), c("DOMAIN", "domain"), c("CREATED", "created_at")}
	identityCols = []col{c("PROVIDER", "provider_slug"), c("ISSUER", "issuer"), c("SUBJECT", "subject"), c("USER", "user_id"), c("VIA", "linked_via"), c("LAST LOGIN", "last_login_at")}
	sessionCols  = []col{c("ID", "id"), c("CURRENT", "current"), c("METHOD", "auth_method"), c("IP", "ip"), c("CREATED", "created_at"), c("LAST SEEN", "last_seen_at"), c("EXPIRES", "expires_at")}
)

// resolveDomain resolves body[key] (a space ID or slug, or "*") to a Casbin domain.
func (a *App) resolveDomain(key string) func(ctx context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
	return func(ctx context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
		s, _ := body[key].(string)
		if s == "" || s == "*" {
			return nil
		}
		id, err := a.resolveSpace(ctx, s)
		if err != nil {
			return err
		}
		body[key] = id
		return nil
	}
}

func (a *App) oidcCmd() *cobra.Command {
	provider := func(f *form) {
		f.str("display-name", "display_name", "name on the sign-in page").str("issuer", "issuer", "issuer URL").str("client-id", "client_id", "client ID").
			secret("client-secret", "client_secret", "client secret (sealed; never readable again)").
			list("scope", "scopes", "scope to request (replaces the list)").list("acr-value", "acr_values", "acr_values to request (replaces the list)").
			str("groups-claim", "groups_claim", "claim holding the user's groups").str("provisioning", "provisioning", "preprovisioned or jit").
			doc("jit", "jit", "just-in-time provisioning rules (JSON or YAML file)").str("stepup-fallback", "stepup_fallback", "none or local_totp").
			boolean("enabled", "enabled", "offer it on the sign-in page").boolean("allow-private-network", "allow_private_network", "allow a private-network issuer").
			boolean("allow-cross-origin-endpoints", "allow_cross_origin_endpoints", "allow endpoints on another origin than the issuer").
			boolean("end-session", "end_session_enabled", "sign out at the provider too (RP-initiated logout) when it advertises end_session_endpoint").
			textFile("ca-bundle", "ca_bundle_pem", "PEM file of the CAs that sign the issuer's certificates")
	}
	var removeSecret bool
	update := a.cmd(opts{use: "update", short: "Change an OIDC provider (only the flags given change)", op: "update-oidc-provider", get: "get-oidc-provider",
		args: pos("provider"), form: provider,
		long: "Changing the issuer or client ID clears the test-login evidence. --client-secret-file replaces the secret; " +
			"--remove-client-secret deletes it.",
		prepare: func(_ context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
			if removeSecret {
				if _, ok := body["client_secret"]; ok {
					return usageErrorf("give --client-secret-file or --remove-client-secret, not both")
				}
				body["client_secret"] = ""
			}
			if len(body) == 0 {
				return usageErrorf("nothing to change")
			}
			return nil
		}})
	update.Flags().BoolVar(&removeSecret, "remove-client-secret", false, "delete the stored client secret")
	providers := group("provider", "OIDC sign-in providers: list, show, create, update, delete and the test-login preview", []string{"providers"},
		a.cmd(opts{use: "list", short: "OIDC providers (never their client secrets)", op: "list-oidc-providers", alias: []string{"ls"},
			view: view{items: "items", cols: oidcCols, empty: "No OIDC providers."}}),
		a.cmd(opts{use: "get", short: "Show an OIDC provider", op: "get-oidc-provider", args: pos("provider")}),
		a.cmd(opts{use: "create", short: "Add an OIDC provider; the client secret comes from a file or standard input", op: "create-oidc-provider", idem: true,
			bodyArgs: []string{"slug"}, form: provider,
			long: "Discovery runs at the first sign-in. Sign in once through the provider's test login in the web UI and check the claim " +
				"mapping with `spaceman oidc provider test-login` before enabling it.",
			example: "spaceman oidc provider create porch --display-name Porch --issuer https://porch.example.com --client-id spaceman \\\n" +
				"  --client-secret-file ./porch.secret --scope openid,profile,email,groups --groups-claim groups"}),
		update,
		a.cmd(opts{use: "delete", short: "Delete an OIDC provider (users who sign in only through it lose access)", op: "delete-oidc-provider",
			args: pos("provider"), confirm: "delete the OIDC provider", done: "OIDC provider deleted."}),
		a.cmd(opts{use: "test-login", short: "Your latest test login's claim-mapping preview", op: "get-oidc-test-login", args: pos("provider"),
			long: "The test login itself runs in the browser (it is a sign-in); this shows what it found."}),
	)
	mappings := group("mapping", "Group mappings: a groups-claim value grants a role in a domain at every SSO sign-in", []string{"mappings"},
		a.cmd(opts{use: "list", short: "A provider's group mappings", op: "list-oidc-mappings", alias: []string{"ls"}, args: pos("provider"),
			view: view{items: "items", cols: mappingCols, empty: "No group mappings."}}),
		a.cmd(opts{use: "add", short: "Map a groups-claim value to a role in a domain", op: "add-oidc-mapping", args: pos("provider"),
			example: "spaceman oidc mapping add porch --group payments-oncall --role operator --domain payments",
			form: func(f *form) {
				f.str("group", "group_value", "groups-claim value (required)").str("role", "role", "viewer, operator or admin (required)").
					str("domain", "domain", "space ID or slug, or * for every space and global administration (required)")
			}, prepare: a.resolveDomain("domain")}),
		a.cmd(opts{use: "delete", short: "Remove a group mapping", op: "delete-oidc-mapping", args: pos("provider", "mapping"), done: "Group mapping removed."}),
	)
	settings := group("settings", "Sign-in settings: SSO-only enforcement and the direct provider", nil,
		a.cmd(opts{use: "get", short: "The sign-in settings and the preconditions of SSO-only enforcement", op: "get-sign-in-settings"}),
		a.cmd(opts{use: "set", short: "Change the sign-in settings (only the flags given change)", op: "set-sign-in-settings",
			long: "--sso-only turns password sign-in off. It needs an enabled provider with a successful test login and an enabled global " +
				"administrator with a linked identity on an enabled provider; while it is on, changes that would break either are refused. " +
				"Break-glass recovery (`spaceman admin recover`) still works. --direct-provider makes the sign-in page go straight to " +
				"that enabled provider (\"\" turns it off).",
			example: "spaceman oidc settings set --direct-provider porch\nspaceman oidc settings set --sso-only",
			form: func(f *form) {
				f.boolean("sso-only", "local_login_disabled", "turn password sign-in off (true) or back on (false)").
					str("direct-provider", "direct_provider", "slug of the provider the sign-in page goes to directly; empty turns it off")
			},
			prepare: func(_ context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
				if len(body) == 0 {
					return usageErrorf("nothing to change")
				}
				return nil
			}}),
	)
	return group("oidc", "Single sign-on: OIDC providers, group mappings and sign-in settings", []string{"sso"},
		providers, mappings, settings,
		a.cmd(opts{use: "sign-in-providers", short: "Providers offered on the sign-in page (needs no token)", op: "list-sign-in-providers", anonymous: true,
			view: view{items: "items", cols: []col{c("SLUG", "slug"), c("NAME", "display_name")}, empty: "No sign-in providers."}}),
	)
}

// identityFlags are the issuer and subject that name a linked identity (query parameters of the unlink operations).
func identityFlags() []qflag {
	return []qflag{qs("issuer", "issuer", "issuer of the identity (required)"), qs("subject", "subject", "subject of the identity (required)")}
}

func needQuery(keys ...string) func(context.Context, *cobra.Command, *call, map[string]any) error {
	return func(_ context.Context, _ *cobra.Command, c *call, _ map[string]any) error {
		for _, k := range keys {
			if c.query.Get(k) == "" {
				return usageErrorf("--%s is required", k)
			}
		}
		return nil
	}
}

func (a *App) identityCmd() *cobra.Command {
	return group("identity", "Your linked sign-in identities: list and unlink (a user's: `spaceman user identity`)", []string{"identities"},
		a.cmd(opts{use: "list", short: "Your linked sign-in identities", op: "list-my-identities", alias: []string{"ls"},
			view: view{items: "items", cols: identityCols, empty: "No linked identities."}}),
		a.cmd(opts{use: "unlink", short: "Unlink one of your identities (ends every session of yours)", op: "unlink-my-identity", query: identityFlags(),
			confirm: "unlink the identity and end every session of yours", prepare: needQuery("issuer", "subject"), done: "Identity unlinked."}),
	)
}

// userExtra are the user commands for identities and one-time tokens.
func (a *App) userExtra() []*cobra.Command {
	ids := group("identity", "A user's linked sign-in identities: list, link and unlink", []string{"identities"},
		a.cmd(opts{use: "list", short: "A user's linked sign-in identities", op: "list-user-identities", alias: []string{"ls"}, args: pos("user"),
			view: view{items: "items", cols: identityCols, empty: "No linked identities."}}),
		a.cmd(opts{use: "link", short: "Link an expected identity to a user", op: "link-user-identity", args: pos("user"),
			long: "The subject is the provider's stable sub claim (with Porch the u_… user ID), never an email.",
			form: func(f *form) {
				f.str("provider", "provider_id", "OIDC provider ID (required)").str("subject", "subject", "the provider's subject (required)")
			}}),
		a.cmd(opts{use: "unlink", short: "Unlink an identity from a user (drops their SSO grants and ends their sessions)", op: "unlink-user-identity",
			args: pos("user"), query: identityFlags(), confirm: "unlink the identity and end the user's sessions", prepare: needQuery("issuer", "subject"),
			done: "Identity unlinked."}),
	)
	return []*cobra.Command{
		a.cmd(opts{use: "reset-token", short: "Issue a one-time password reset token for a user", op: "issue-reset", args: pos("user"),
			secret: "token", onRequest: true, view: view{first: []string{"expires_at", "token"}},
			long: "Hand the token to the user out of band; they redeem it on the sign-in page (Reset password). Every session of the " +
				"user ends when it is used.",
			example: "spaceman user reset-token usr_01… --secret-file ./ada.reset"}),
		a.cmd(opts{use: "link-invite", short: "Issue a single-use SSO link invitation (72 hours)", op: "issue-link-invite", args: pos("user"),
			secret: "url", redact: []string{"token"}, onRequest: true, view: view{first: []string{"expires_at", "url"}},
			long: "Hand the URL to the user out of band: it links the identity they sign in with at the provider to this user.",
			form: func(f *form) { f.str("provider", "provider_id", "OIDC provider ID (required)") }}),
		ids,
	}
}

func (a *App) sessionCmd() *cobra.Command {
	return group("session", "Your sign-in sessions: list and revoke", []string{"sessions"},
		a.cmd(opts{use: "list", short: "Your active sessions", op: "list-sessions", alias: []string{"ls"},
			view: view{items: "items", cols: sessionCols, empty: "No active sessions."}}),
		a.cmd(opts{use: "revoke", short: "Revoke one of your sessions", op: "revoke-session", args: pos("session"), done: "Session revoked."}),
	)
}

func (a *App) notificationsCmd() *cobra.Command {
	return group("notifications", "Your notification preferences: what the web UI announces to you", []string{"notification-preferences"},
		a.cmd(opts{use: "get", short: "Your notification preferences (defaults until saved)", op: "get-notification-preferences"}),
		a.cmd(opts{use: "set", short: "Change your notification preferences (only the flags given change)", op: "set-notification-preferences",
			get: "get-notification-preferences", merge: true,
			form: func(f *form) {
				f.boolean("enabled", "enabled", "announce human-action changes in the web UI").boolean("desktop", "desktop", "also show desktop notifications").
					boolean("new-actions", "new_actions", "new pending actions").boolean("assigned-to-me", "assigned_to_me", "actions assigned to you").
					boolean("escalations", "escalations", "escalations").boolean("reminders", "reminders", "reminders").
					boolean("clarifications", "clarifications", "clarification requests and replies").
					str("min-priority", "min_priority", "low, normal, high or urgent").list("mute", "muted_spaces", "space ID never announced (repeatable; replaces the list)").
					str("timezone", "timezone", "your IANA time zone, e.g. Europe/Vilnius (the Telegram digest and quiet hours use it)")
			}}),
	)
}

func (a *App) mfaCmd() *cobra.Command {
	return group("mfa", "Your second factor: TOTP enrollment and recovery codes", nil,
		a.cmd(opts{use: "enroll", short: "Start TOTP enrollment; the otpauth URI (the TOTP secret) is shown once", op: "start-enroll", //nolint:gosec // G101 false positive: response field names, not credentials
			secret: "otpauth_uri", redact: []string{"secret"}, onRequest: true,
			long: "Import the otpauth URI into an authenticator app (most read it from a file or a QR code made from it), then " +
				"finish with `spaceman mfa confirm` and a code from the app.",
			example: "spaceman mfa enroll --secret-file ./totp.uri\nspaceman mfa confirm --code-stdin --secret-file ./recovery-codes"}),
		a.cmd(opts{use: "confirm", short: "Confirm TOTP enrollment with a code; the recovery codes are shown once", op: "confirm-enroll",
			secret: "recovery_codes", onRequest: true,
			form: func(f *form) { f.secret("code", "code", "current TOTP code from the authenticator app") }}),
		a.cmd(opts{use: "recovery-codes", short: "Replace your recovery codes (the old ones stop working)", op: "regenerate-recovery-codes",
			secret: "recovery_codes", onRequest: true, confirm: "replace your recovery codes"}),
	)
}

func (a *App) passwordCmd() *cobra.Command {
	return group("password", "Your password: change it (passwords come from files or standard input)", nil,
		a.cmd(opts{use: "change", short: "Change your password", op: "change-password", done: "Password changed.",
			example: "spaceman password change --current-password-file ./old --new-password-stdin < ./new",
			form: func(f *form) {
				f.secret("current-password", "current", "your current password").secret("new-password", "new", "the new password")
			}}),
	)
}

// systemExtra are the system commands for the instance's status, the outbox, the restore and break-glass notices and the root token.
func (a *App) systemExtra() []*cobra.Command {
	return []*cobra.Command{
		a.cmd(opts{use: "status", short: "This instance's status: version, uptime, boot ID, schema and protocol versions, storage, counts (admins)",
			op: "get-system"}),
		a.cmd(opts{use: "outbox", short: "Outbound event publication status (pending, retrying, dead, recent failures)", op: "get-outbound-events"}),
		a.cmd(opts{use: "attention", short: "What needs a person: human actions, blocked tasks, quarantine, runs, budgets, nodes, outbox, backups, storage",
			op: "get-attention", query: []qflag{qs("space", "space", "only this space")}}),
		group("restore", "The notice a central shows after a restore", nil,
			a.cmd(opts{use: "get", short: "The restore notice: what the restore changed and what is still paused", op: "get-restore-state"}),
			a.cmd(opts{use: "acknowledge", short: "Dismiss the restore notice", op: "acknowledge-restore"}),
		),
		group("break-glass", "Unacknowledged use of a break-glass recovery login", nil,
			a.cmd(opts{use: "get", short: "Whether a break-glass recovery login has not been acknowledged yet", op: "get-break-glass"}),
			a.cmd(opts{use: "acknowledge", short: "Acknowledge break-glass use", op: "acknowledge-break-glass", done: "Break-glass use acknowledged."}),
		),
		group("root-token", "The root token: show its metadata, rotate or revoke it", nil,
			a.cmd(opts{use: "get", short: "Active root token metadata (never the token)", op: "get-root-token"}),
			a.cmd(opts{use: "rotate", short: "Rotate the root token; the new one is shown once and the old one stops working", op: "rotate-root-token",
				secret: "secret", onRequest: true, confirm: "rotate the root token (the current one stops working)",
				example: "spaceman system root-token rotate --yes --secret-file ./root.token"}),
			a.cmd(opts{use: "revoke", short: "Revoke the root token (issue a new one offline with `spaceman admin root-token rotate`)", op: "revoke-root-token",
				confirm: "revoke the root token", done: "Root token revoked."}),
		),
	}
}
