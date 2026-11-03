package cli

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	peerCols   = []col{c("SPACE", "space_id"), c("PEER", "peer.slug"), c("PEER ID", "peer.id"), c("IN Q/D/K", ""), c("OUT Q/D/K", ""), c("DISCLOSURE", "inbound.disclosure"), c("VERSION", "version")}
	xreqCols   = []col{c("ID", "id"), c("SIDE", "side"), c("KIND", "kind"), c("STATE", "state"), c("FROM", "source.slug"), c("TO", "destination.slug"), c("TITLE", "title")}
	userCols   = []col{c("ID", "id"), c("EMAIL", "email"), c("NAME", "display_name"), c("ENABLED", "enabled"), c("MFA", "mfa_enrolled"), {head: "GRANTS", fn: grantsOf}}
	tokenCols  = []col{c("ID", "id"), c("NAME", "name"), c("USER", "user_id"), c("HINT", "hint"), {head: "GRANTS", fn: grantsOf}, c("EXPIRES", "expires_at"), c("REVOKED", "revoked_at")}
	backupCols = []col{c("ID", "id"), c("STATE", "state"), c("TRIGGER", "trigger"), c("TARGET", "target"), c("BYTES", "bytes"), c("CREATED", "created_at"), c("VERIFIED", "verify_state")}
	auditCols  = []col{c("SEQ", "seq"), {head: "AT", fn: microsAt("at")}, c("ACTION", "action"), c("ACTOR", "actor_type"), c("ACTOR ID", "actor_id"), c("TARGET", "target_id"), c("OUTCOME", "outcome")}
	flagsInOut = func(side string) func(map[string]any) string {
		return func(m map[string]any) string {
			s, _ := m[side].(map[string]any)
			yn := func(k string) string {
				if b, _ := s[k].(bool); b {
					return "y"
				}
				return "-"
			}
			return yn("questions") + "/" + yn("delegation") + "/" + yn("knowledge")
		}
	}
)

func init() {
	peerCols[3].fn = flagsInOut("inbound")
	peerCols[4].fn = flagsInOut("outbound")
}

func grantsOf(m map[string]any) string {
	gs, _ := m["grants"].([]any)
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		gm, _ := g.(map[string]any)
		out = append(out, cell(gm["role"])+":"+cell(gm["domain"]))
	}
	return strings.Join(out, ",")
}

func microsAt(key string) func(map[string]any) string {
	return func(m map[string]any) string {
		n, err := strconv.ParseInt(numberString(m[key]), 10, 64)
		if err != nil {
			return ""
		}
		return time.UnixMicro(n).UTC().Format("2006-01-02 15:04:05Z")
	}
}

func (a *App) xspaceCmd() *cobra.Command {
	peerFlags := func(f *form) {
		f.boolean("discover", "inbound.discover", "let the peer's control discover this space").
			boolean("accept-questions", "inbound.questions", "accept questions from the peer").
			boolean("accept-delegation", "inbound.delegation", "accept delegated work from the peer").
			boolean("accept-knowledge", "inbound.knowledge", "accept knowledge requests from the peer").
			list("kb", "inbound.kbs", "knowledge base the peer may ask (replaces the list)").
			str("disclosure", "inbound.disclosure", "what replies disclose: none, summary or detailed").
			integer("max-pending", "inbound.max_pending", "open requests from the peer at once").
			integer("max-per-hour", "inbound.max_per_hour", "requests from the peer per hour").
			boolean("send-questions", "outbound.questions", "let this space's control ask the peer questions").
			boolean("send-delegation", "outbound.delegation", "let this space's control delegate work to the peer").
			boolean("send-knowledge", "outbound.knowledge", "let this space's control ask the peer's knowledge bases")
	}
	peers := group("peer", "What a space accepts from and may send to each peer space", []string{"peers"},
		a.cmd(opts{use: "list", short: "Peer configurations of a space (or of every space without --space)", op: "list-xspace-peers",
			global: "list-all-xspace-peers", alias: []string{"ls"}, scoped: true, view: view{items: "items", cols: peerCols, empty: "No peers."}}),
		a.cmd(opts{use: "get", short: "A space's configuration for one peer space", op: "get-xspace-peer", scoped: true, args: []arg{{"peer-space", aSpace}}}),
		a.cmd(opts{use: "set", short: "Set what a space accepts from and may send to a peer (only the flags given change)", op: "put-xspace-peer",
			get: "get-xspace-peer", getOptional: true, merge: true, scoped: true, args: []arg{{"peer-space", aSpace}}, form: peerFlags,
			example: "spaceman xspace peer set --space payments billing --accept-questions --disclosure summary --send-questions",
			prepare: func(_ context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
				// A first save sends every flag; unset ones are off (0 limits take the server's defaults).
				defaults := map[string]map[string]any{
					"inbound": {"discover": false, "questions": false, "delegation": false, "knowledge": false, "kbs": []any{},
						"max_pending": 0, "max_per_hour": 0},
					"outbound": {"questions": false, "delegation": false, "knowledge": false},
				}
				for side, d := range defaults {
					m, ok := body[side].(map[string]any)
					if !ok {
						m = map[string]any{}
						body[side] = m
					}
					for k, v := range d {
						if _, ok := m[k]; !ok {
							m[k] = v
						}
					}
				}
				return nil
			}}),
		a.cmd(opts{use: "delete", short: "Remove a space's configuration for a peer", op: "delete-xspace-peer", scoped: true,
			args: []arg{{"peer-space", aSpace}}, done: "Peer configuration removed."}),
	)
	reqs := group("request", "Cross-space requests: list, show, cancel and decline", []string{"requests"},
		a.cmd(opts{use: "list", short: "A space's requests (or every space's without --space)", op: "list-xspace-requests", global: "list-all-xspace-requests",
			alias: []string{"ls"}, scoped: true,
			query: []qflag{qs("direction", "direction", "with --space: incoming or outgoing"), qb("open", "open", "only open requests"), limitFlag()},
			view:  view{items: "items", cols: xreqCols, empty: "No requests."}}),
		a.cmd(opts{use: "get", short: "Show a request", op: "get-xspace-request", scoped: true, args: pos("request")}),
		a.cmd(opts{use: "cancel", short: "Withdraw an outgoing request", op: "cancel-xspace-request", scoped: true, args: pos("request"),
			form: func(f *form) { f.str("reason", "reason", "reason") }}),
		a.cmd(opts{use: "decline", short: "Decline an incoming request", op: "decline-xspace-request", scoped: true, args: pos("request"),
			form: func(f *form) { f.str("reason", "reason", "reason (disclosed according to the peer's level)") }}),
	)
	settings := group("settings", "A space's cross-space settings", nil,
		a.cmd(opts{use: "get", short: "Show a space's cross-space settings", op: "get-xspace-settings", scoped: true}),
		a.cmd(opts{use: "set", short: "Edit a space's cross-space settings", op: "update-xspace-settings", get: "get-xspace-settings", merge: true,
			optionalIfMatch: true, scoped: true,
			form: func(f *form) {
				f.text("profile", "profile", "public profile shown to peers that may discover the space").
					integer("max-depth", "max_depth", "deepest request chain accepted").
					integer("max-outgoing-per-hour", "max_outgoing_per_hour", "requests this space's control may send per hour")
			}}),
	)
	return group("xspace", "Cross-space collaboration: peers, requests and settings", []string{"cross-space"}, peers, reqs, settings)
}

// grantList parses --grant ROLE[:DOMAIN] (DOMAIN a space ID or slug, default "*").
func (a *App) grantList(ctx context.Context, raw []string) ([]any, error) {
	out := []any{}
	for _, g := range raw {
		for _, p := range strings.Split(g, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			role, domain, ok := strings.Cut(p, ":")
			if !ok || domain == "" {
				domain = "*"
			}
			if domain != "*" {
				id, err := a.resolveSpace(ctx, domain)
				if err != nil {
					return nil, err
				}
				domain = id
			}
			switch role {
			case "viewer", "operator", "admin":
			default:
				return nil, usageErrorf("--grant role must be viewer, operator or admin, got %q", role)
			}
			out = append(out, map[string]any{"role": role, "domain": domain})
		}
	}
	return out, nil
}

func (a *App) grantsPrepare(ctx context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
	raw, ok := body["grants"].([]any)
	if !ok {
		return nil
	}
	strs := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			strs = append(strs, s)
		} else {
			// From --file: already objects.
			return nil
		}
	}
	gs, err := a.grantList(ctx, strs)
	if err != nil {
		return err
	}
	body["grants"] = gs
	return nil
}

const grantHelp = "grant ROLE[:SPACE] (viewer, operator or admin; SPACE an ID or slug, default * = every space and global administration)"

func (a *App) userCmd() *cobra.Command {
	return group("user", "Users: list, show, create, disable, enable and set grants", []string{"users"},
		a.cmd(opts{use: "list", short: "Users", op: "list-users", alias: []string{"ls"}, view: view{items: "items", cols: userCols, empty: "No users."}}),
		a.cmd(opts{use: "get", short: "Show a user", op: "get-user", args: pos("user")}),
		a.cmd(opts{use: "create", short: "Create a user (a password, if any, comes from a file or standard input)", op: "create-user", idem: true,
			example: "spaceman user create --email ada@example.com --display-name Ada --grant operator:payments --password-file ./initial",
			form: func(f *form) {
				f.str("email", "email", "email (sign-in name)").str("display-name", "display_name", "display name (required)").
					list("grant", "grants", grantHelp).boolean("mfa-required", "mfa_required", "require a second factor").
					secret("password", "password", "initial password")
			},
			prepare: func(ctx context.Context, cmd *cobra.Command, c *call, body map[string]any) error {
				if _, ok := body["grants"]; !ok {
					body["grants"] = []any{}
				}
				return a.grantsPrepare(ctx, cmd, c, body)
			}}),
		a.cmd(opts{use: "disable", short: "Disable a user (ends their sessions and tokens)", op: "disable-user", get: "get-user", args: pos("user")}),
		a.cmd(opts{use: "enable", short: "Enable a disabled user", op: "enable-user", get: "get-user", args: pos("user")}),
		a.cmd(opts{use: "grants", short: "Replace a user's grants", op: "set-user-grants", get: "get-user", args: pos("user"),
			form: func(f *form) { f.list("grant", "grants", grantHelp) },
			prepare: func(ctx context.Context, cmd *cobra.Command, c *call, body map[string]any) error {
				if _, ok := body["grants"]; !ok {
					return usageErrorf("give the complete new set with --grant (repeatable)")
				}
				return a.grantsPrepare(ctx, cmd, c, body)
			}}),
	)
}

func (a *App) tokenCmd() *cobra.Command {
	var allUsers, anyUser bool
	list := a.cmd(opts{use: "list", short: "Your API tokens (--all-users: everyone's)", op: "list-my-tokens", alias: []string{"ls"},
		view: view{items: "items", cols: tokenCols, empty: "No API tokens."},
		prepare: func(_ context.Context, _ *cobra.Command, c *call, _ map[string]any) error {
			if allUsers {
				c.op = "list-all-tokens"
			}
			return nil
		}})
	list.Annotations[annOps] += ",list-all-tokens"
	list.Flags().BoolVar(&allUsers, "all-users", false, "every user's tokens (needs token administration)")
	revoke := a.cmd(opts{use: "revoke", short: "Revoke one of your API tokens (--any: anyone's)", op: "revoke-my-token", args: pos("token"),
		prepare: func(_ context.Context, _ *cobra.Command, c *call, _ map[string]any) error {
			if anyUser {
				c.op = "revoke-any-token"
			}
			return nil
		}})
	revoke.Annotations[annOps] += ",revoke-any-token"
	revoke.Flags().BoolVar(&anyUser, "any", false, "revoke another user's token (needs token administration)")
	return group("token", "API tokens: list, create and revoke", []string{"tokens"},
		list,
		a.cmd(opts{use: "create", short: "Create an API token for yourself; the secret is shown once", op: "create-api-token", idem: true,
			bodyArgs: []string{"name"}, secret: "secret",
			long: "The token acts for you with the given grants, each within your own; it loses anything you lose. The root token cannot " +
				"create API tokens (it is not a user).",
			example: "spaceman token create terraform --grant admin --expires-in-days 90 --secret-file ~/.config/spaceman/terraform.token",
			form: func(f *form) {
				f.list("grant", "grants", grantHelp+" (required)").integer("expires-in-days", "expires_in_days", "expiry in days, 1-366 (required unless --never-expires)").
					boolean("never-expires", "never_expires", "no expiry")
			},
			prepare: a.grantsPrepare, view: view{first: []string{"secret", "token"}}}),
		revoke,
	)
}

func (a *App) systemCmd() *cobra.Command {
	backups := group("backup", "Backups of a running central: list, create and verify (restore is offline: `spaceman backup restore`)", []string{"backups"},
		a.cmd(opts{use: "list", short: "Backups and the backup configuration", op: "list-backups", alias: []string{"ls"},
			view: view{items: "items", cols: backupCols, empty: "No backups."}}),
		a.cmd(opts{use: "create", short: "Start a backup now", op: "create-backup"}),
		a.cmd(opts{use: "verify", short: "Verify a stored backup", op: "verify-backup", args: pos("backup")}),
	)
	return group("system", "A running central: version and readiness, backups", nil,
		a.cmd(opts{use: "info", short: "Version, readiness and bootstrap state (needs no token)", op: "get-system-info", anonymous: true}),
		backups,
	)
}

func (a *App) auditCmd() *cobra.Command {
	return group("audit", "The audit log: list and verify the hash chain", nil,
		a.cmd(opts{use: "list", short: "Audit records, newest first", op: "list-audit", alias: []string{"ls"}, paged: true,
			query: []qflag{qs("action", "action", "only this action (e.g. space.created)"), qs("target", "target_id", "only this target ID"),
				qs("actor", "actor_id", "only this actor ID"), limitFlag()},
			view: view{items: "items", cols: auditCols, empty: "No audit records."}}),
		a.cmd(opts{use: "verify", short: "Verify the audit log's hash chain (exit code 2 when it is broken)", op: "verify-audit-chain",
			check: func(obj map[string]any) error {
				if ok, _ := obj["ok"].(bool); !ok {
					return &StateError{Msg: "the audit chain is broken at sequence " + cell(obj["broken_at"]) + ": " + cell(obj["problem"])}
				}
				return nil
			}}),
	)
}

func (a *App) whoamiCmd() *cobra.Command {
	return a.cmd(opts{use: "whoami", short: "Who the token acts as, and how", op: "get-me", view: view{first: []string{"root", "method", "user"}}})
}
