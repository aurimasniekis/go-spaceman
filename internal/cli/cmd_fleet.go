package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var (
	nodeCols  = []col{c("ID", "id"), c("NAME", "name"), c("STATE", "state"), c("CONNECTED", "connected"), c("AGENT", "agent_version"), c("LAST SEEN", "last_seen_at")}
	spaceCols = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), c("STATE", "state"), c("PAUSE", "pause_mode"), c("NODE", "node_id")}
	runCols   = []col{c("ID", "id"), c("ROLE", "role"), c("KIND", "kind"), c("HARNESS", "harness"), c("OBSERVED", "observed"), c("DESIRED", "desired"), c("STATUS", "status"), c("CREATED", "created_at")}
	relocCols = []col{c("ID", "id"), c("MODE", "mode"), c("STATE", "state"), c("FROM", "source_node_id"), c("TO", "destination_node_id"), c("CREATED", "created_at"), c("FINISHED", "finished_at")}
)

func (a *App) nodeCmd() *cobra.Command {
	state := func(verb, s, short, confirm string) *cobra.Command {
		return a.cmd(opts{use: verb, short: short, op: "set-node-state", args: []arg{{"node", aNode}},
			static: map[string]any{"state": s}, view: view{first: []string{"connected"}}, confirm: confirm})
	}
	return group("node", "Nodes: list, enroll, edit, disable, enable and revoke", []string{"nodes"},
		a.cmd(opts{use: "list", short: "List nodes", op: "list-nodes", alias: []string{"ls"}, view: view{items: "items", cols: nodeCols, empty: "No nodes."}}),
		a.cmd(opts{use: "get", short: "Show a node (ID or name)", op: "get-node", args: []arg{{"node", aNode}}}),
		a.cmd(opts{use: "create", short: "Register a node and issue its one-time enrollment token", op: "create-node", bodyArgs: []string{"name"}, idem: true,
			secret: "token",
			long: "Registers a node and prints its enrollment token once (or writes it with --secret-file). Start `spaceman-agent enroll` on " +
				"the node with it before it expires.",
			example: "spaceman node create build-01 --label zone=eu --secret-file ./build-01.enroll",
			form: func(f *form) {
				f.str("description", "description", "description").kv("label", "labels", "label")
			},
			view: view{first: []string{"expires_at", "token"}}}),
		a.cmd(opts{use: "update", short: "Edit a node's name, description and labels (only the flags given change)", op: "update-node", get: "get-node",
			merge: true, args: []arg{{"node", aNode}},
			form: func(f *form) {
				f.str("name", "name", "new name").str("description", "description", "description").kv("label", "labels", "label (replaces all labels)")
			}}),
		a.cmd(opts{use: "enrollment", short: "Issue a new one-time enrollment token for a pending node", op: "reissue-enrollment",
			args: []arg{{"node", aNode}}, secret: "token", view: view{first: []string{"expires_at", "token"}}}),
		state("disable", "disabled", "Disable a node (no new runs; its lease stays)", ""),
		state("enable", "active", "Enable a disabled node", ""),
		state("revoke", "revoked", "Revoke a node permanently (its credentials stop working)", "revoke the node permanently"),
	)
}

func spaceArg() []arg { return []arg{{"space", aSpace}} }

func (a *App) spaceCmd() *cobra.Command {
	state := func(verb, s, short, confirm string, mode bool) *cobra.Command {
		o := opts{use: verb, short: short, op: "set-space-state", get: "get-space", args: spaceArg(), static: map[string]any{"state": s}, confirm: confirm}
		if mode {
			o.form = func(f *form) {
				f.str("mode", "mode", "hold (admit no new work, default), interrupt (also stop runs politely) or emergency (kill runs)")
			}
		}
		return a.cmd(o)
	}
	profile := func(f *form) {
		f.str("name", "name", "display name").text("description", "description", "description").text("goals", "goals", "goals").
			text("standing-instructions", "standing_instructions", "standing instructions for the space's agents").
			str("disconnect-policy", "disconnect_policy", "interrupt or kill: what happens to runs when the node disconnects")
	}
	create := a.cmd(opts{use: "create", short: "Create a space pinned to a node", op: "create-space", idem: true, bodyArgs: []string{"slug"},
		example: "spaceman space create payments --name Payments --node build-01 --starter-roles",
		form: func(f *form) {
			profile(f)
			f.str("node", "node_id", "node ID or name the space is pinned to (required)").
				boolean("starter-roles", "starter_roles", "also give the space the starter roles (control, planner, implementor, reviewer, kb, dreamer)")
		},
		prepare: func(ctx context.Context, cmd *cobra.Command, c *call, body map[string]any) error {
			if n, _ := body["node_id"].(string); n != "" {
				id, err := a.resolveNode(ctx, n)
				if err != nil {
					return err
				}
				body["node_id"] = id
			}
			if _, ok := body["name"]; !ok {
				body["name"] = body["slug"]
			}
			return nil
		}})
	return group("space", "Spaces: create, edit, pause, archive, purge, relocate and usage", []string{"spaces"},
		a.cmd(opts{use: "list", short: "List the spaces you can see", op: "list-spaces", alias: []string{"ls"}, view: view{items: "items", cols: spaceCols, empty: "No spaces."}}),
		a.cmd(opts{use: "get", short: "Show a space (ID or slug)", op: "get-space", args: spaceArg()}),
		create,
		a.cmd(opts{use: "update", short: "Edit a space's profile (only the flags given change)", op: "update-space", get: "get-space", merge: true,
			args: spaceArg(), form: profile}),
		state("pause", "paused", "Pause a space", "", true),
		state("resume", "active", "Resume a paused space", "", false),
		state("archive", "archived", "Archive a space (needs no active runs)", "", false),
		state("restore", "active", "Restore an archived space", "", false),
		a.cmd(opts{use: "purge", short: "Permanently delete an archived space's records (never upstream repositories)", op: "purge-space",
			args: spaceArg(), confirm: "permanently delete the space's records", done: "Space purged."}),
		a.relocateCmd(),
		group("export", "A space's records and files as one plaintext ZIP archive (space admins only)", nil,
			a.cmd(opts{use: "summary", short: "What an export would hold now: counts, estimated size and the space's export limit", op: "get-space-export",
				scoped: true, view: view{first: []string{"estimated_bytes", "max_bytes", "within_limit", "encrypted", "warning"}}}),
			a.downloadCmd(download{short: "Download the space's export archive (ZIP of JSON Lines and attachment files) to a file",
				route: "/spaces/{space_id}/export", related: []string{"get-space-export"},
				long: "The archive is NOT encrypted: it holds the space's inbox items, tasks, human actions with responses, messages and " +
					"attachments in plain text (no credentials or tokens). The file is created with mode 0600; handle it like the data it holds. " +
					"An export over the space's export_max_bytes limit is refused.",
				example: "spaceman space export download --space payments --out payments-export.zip"}),
		),
	)
}

// relocTerminal are the states a relocation stops in (until an operator acts).
var relocTerminal = map[string]bool{"done": true, "checked": true, "failed": true, "cancelled": true, "blocked": true}

func relocView() view {
	return view{first: []string{"mode", "state", "blocked_step", "source_node_id", "destination_node_id", "error"}, after: writeIssues}
}

func writeIssues(w io.Writer, obj map[string]any) {
	issues, _ := obj["issues"].([]any)
	if len(issues) == 0 {
		return
	}
	fmt.Fprintln(w, "\nIssues:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  CODE\tPATH\tEXPORTABLE\tDETAIL")
	for _, it := range issues {
		m, _ := it.(map[string]any)
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", cell(m["code"]), cell(m["path"]), cell(m["exportable"]), cell(m["detail"]))
	}
	_ = tw.Flush()
	if l, ok := obj["loss"].(map[string]any); ok && cell(l["statement"]) != "" {
		fmt.Fprintf(w, "\nPossible loss: %s\n", cell(l["statement"]))
	}
}

func (a *App) relocateCmd() *cobra.Command {
	var wait bool
	var waitTimeout time.Duration
	waitFlags := func(cmd *cobra.Command) *cobra.Command {
		cmd.Flags().BoolVar(&wait, "wait", false, "poll until the relocation finishes, is blocked or fails")
		cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "give up waiting after this long")
		return cmd
	}
	wrap := func(cmd *cobra.Command) *cobra.Command {
		inner := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if !wait {
				return inner(cmd, args)
			}
			// Run the action quietly, then poll and print the final state.
			saved := cmd.OutOrStdout()
			cmd.SetOut(io.Discard)
			err := inner(cmd, args)
			cmd.SetOut(saved)
			if err != nil {
				return err
			}
			return a.waitRelocation(cmd, args[0], waitTimeout)
		}
		return waitFlags(cmd)
	}
	start := func(f *form) {
		f.str("to", "destination_node_id", "destination node ID or name (required)").
			str("dirty-state", "dirty_state", "refuse (default: stop at unpushed, changed or untracked state) or export (carry it through central)").
			boolean("stop-runs", "stop_runs", "pause the space (interrupt) and stop its runs first; otherwise it must already be paused with no active runs").
			str("recovery", "recovery", "what happens to the source folder: retain (default) or delete").
			integer("retain-days", "retain_days", "with retain: days to keep the source folder (0 keeps it until released)")
	}
	resolveTo := func(ctx context.Context, cmd *cobra.Command, c *call, body map[string]any) error {
		to, _ := body["destination_node_id"].(string)
		if to == "" {
			return usageErrorf("--to is required (the destination node)")
		}
		id, err := a.resolveNode(ctx, to)
		if err != nil {
			return err
		}
		body["destination_node_id"] = id
		return nil
	}
	precheck := a.cmd(opts{use: "precheck", short: "Inspect the source for a relocation; nothing changes", op: "start-relocation", args: spaceArg(), idem: true,
		static: map[string]any{"precheck": true}, view: relocView(),
		form: func(f *form) {
			f.str("to", "destination_node_id", "destination node ID or name (required)")
		}, prepare: resolveTo})
	startCmd := a.cmd(opts{use: "start", short: "Relocate a stopped space to another node", op: "start-relocation", args: spaceArg(), idem: true,
		view: relocView(),
		long: "Starts a clean relocation: the source syncs memory and uploads and its repositories are checked; unpushed, changed or untracked " +
			"work is refused unless --dirty-state export. When the source node is offline, disabled or revoked, --source-unavailable " +
			"relocates from central's copies only; it needs --acknowledge-loss and --acknowledgement with your own words (recorded in the " +
			"audit log), and --repo url[@branch][=path] for each repository to clone on the destination.",
		example: "spaceman space relocate start payments --to build-02 --stop-runs --wait\n" +
			"spaceman space relocate start payments --to build-02 --source-unavailable --acknowledge-loss \\\n" +
			"  --acknowledgement \"build-01 died; unpushed work there is lost\" --repo https://git.example.com/pay.git@main",
		form: func(f *form) {
			start(f)
			f.boolean("source-unavailable", "source_unavailable", "relocate from central's copies only, because the source is offline, disabled or revoked").
				boolean("acknowledge-loss", "acknowledge_loss", "with --source-unavailable: acknowledge that local state on the source may be lost").
				str("acknowledgement", "acknowledgement", "with --source-unavailable: your own words, recorded in the audit log").
				list("repo", "repositories", "with --source-unavailable: repository to clone, as url[@branch][=path]")
		},
		prepare: func(ctx context.Context, cmd *cobra.Command, c *call, body map[string]any) error {
			if err := resolveTo(ctx, cmd, c, body); err != nil {
				return err
			}
			if su, _ := body["source_unavailable"].(bool); su {
				ack, _ := body["acknowledge_loss"].(bool)
				text, _ := body["acknowledgement"].(string)
				if !ack || strings.TrimSpace(text) == "" {
					return usageErrorf("--source-unavailable needs --acknowledge-loss and --acknowledgement \"<your own words>\": local state on the source may be lost")
				}
			}
			if repos, ok := body["repositories"].([]any); ok {
				if su, _ := body["source_unavailable"].(bool); !su {
					return usageErrorf("--repo is only for --source-unavailable")
				}
				out := make([]any, 0, len(repos))
				for _, r := range repos {
					s, _ := r.(string)
					if strings.HasPrefix(s, "{") {
						out = append(out, r)
						continue
					}
					out = append(out, parseRepo(s))
				}
				body["repositories"] = out
			}
			return nil
		}})
	retry := a.cmd(opts{use: "retry", short: "Resume a blocked relocation", op: "retry-relocation", args: spaceArg(), view: relocView(),
		form: func(f *form) {
			f.str("dirty-state", "dirty_state", "change the policy first: export carries the reported state through central")
		}})
	g := group("relocate", "Move a space to another node: precheck, start, status, retry, cancel, release", []string{"relocation"},
		wrap(precheck), wrap(startCmd),
		a.cmd(opts{use: "status", short: "The relocation in progress, or the latest one", op: "get-relocation", args: spaceArg(), view: relocView()}),
		a.cmd(opts{use: "history", short: "Every relocation of the space, newest first", op: "list-relocations", args: spaceArg(), query: []qflag{limitFlag()},
			view: view{items: "items", cols: relocCols, empty: "No relocations."}}),
		wrap(retry),
		a.cmd(opts{use: "cancel", short: "Cancel a relocation before it finalizes (the space stays on its source, paused)", op: "cancel-relocation",
			args: spaceArg(), view: relocView()}),
		a.cmd(opts{use: "release", short: "Delete the source folder the latest relocation retained", op: "release-relocation", args: spaceArg(),
			view: relocView(), confirm: "delete the retained source folder"}),
	)
	return g
}

// parseRepo reads url[@branch][=path].
func parseRepo(s string) map[string]any {
	m := map[string]any{}
	if i := strings.LastIndex(s, "="); i > 0 {
		m["path"] = s[i+1:]
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i > strings.LastIndex(s, "/") && i > 0 {
		m["branch"] = s[i+1:]
		s = s[:i]
	}
	m["url"] = s
	return m
}

func (a *App) waitRelocation(cmd *cobra.Command, spaceRef string, timeout time.Duration) error {
	ctx := cmd.Context()
	format, err := a.format()
	if err != nil {
		return err
	}
	id, err := a.resolveSpace(ctx, spaceRef)
	if err != nil {
		return err
	}
	interval := a.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		res, err := a.do(ctx, call{op: "get-relocation", params: map[string]string{"space_id": id}})
		if err != nil {
			return err
		}
		obj, err := res.Object()
		if err != nil {
			return err
		}
		st, _ := obj["state"].(string)
		if st != last && format == FormatTable {
			fmt.Fprintf(cmd.ErrOrStderr(), "relocation %s: %s\n", cell(obj["id"]), st)
			last = st
		}
		if relocTerminal[st] {
			if err := render(cmd.OutOrStdout(), format, res.Body, relocView()); err != nil {
				return err
			}
			switch st {
			case "failed", "cancelled":
				return &StateError{Msg: fmt.Sprintf("relocation %s: %s", st, cell(obj["error"]))}
			case "blocked":
				return &StateError{Msg: "relocation blocked at " + cell(obj["blocked_step"]) + "; resolve the issues and run `spaceman space relocate retry`"}
			}
			return nil
		}
		if time.Now().After(deadline) {
			return &StateError{Msg: "still " + st + " after " + timeout.String() + "; check again with `spaceman space relocate status`"}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (a *App) runCmd() *cobra.Command {
	return group("run", "Runs: list, show, launch, stop, send input; usage, recordings and transcripts (--space)", []string{"runs"},
		a.cmd(opts{use: "list", short: "A space's runs, newest first", op: "list-runs", alias: []string{"ls"}, scoped: true,
			query: []qflag{limitFlag(), qs("include", "include", "manifest: also return each run's manifest")},
			view:  view{items: "items", cols: runCols, empty: "No runs."}}),
		a.cmd(opts{use: "get", short: "Show a run", op: "get-run", scoped: true, args: pos("run")}),
		a.cmd(opts{use: "launch", short: "Launch a run on the space's node", op: "launch-run", scoped: true, idem: true,
			example: "spaceman run launch --space payments --role worker --prompt-file task.md",
			form: func(f *form) {
				f.str("role", "role", "space role to run (required)").str("kind", "kind", "control, worker, kb, dreamer or diagnostic").
					str("harness", "harness", "claude-code or codex (default: the role's)").str("model", "model", "model override").
					text("prompt", "prompt", "first prompt").str("account", "account_ref", "account ID to use").
					str("workspace", "workspace", "workspace path inside the space").str("instance", "instance_id", "instance ID").
					str("session", "session_id", "native session to resume").kv("setting", "settings", "harness setting").
					str("worktree-repo", "worktree.repo", "repository of a dedicated worktree").str("worktree-branch", "worktree.branch", "worktree branch").
					str("worktree-base", "worktree.base", "worktree base ref")
			}}),
		a.cmd(opts{use: "stop", short: "Stop a run", op: "stop-run", scoped: true, args: pos("run"),
			form: func(f *form) {
				f.str("mode", "mode", "interrupt (default) or kill").str("reason", "reason", "reason, recorded with the run")
			}}),
	)
}
