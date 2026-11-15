package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	inboxCols    = []col{c("ID", "id"), c("STATUS", "status"), c("URGENCY", "urgency"), c("SOURCE", "source_kind"), c("TITLE", "title"), c("RECEIVED", "received_at")}
	taskCols     = []col{c("ID", "id"), c("POS", "position"), c("STATUS", "status"), c("PRIORITY", "priority"), c("KIND", "kind"), c("ROLE", "assignee_role"), c("TITLE", "title")}
	humanCols    = []col{c("ID", "id"), c("SPACE", "space_id"), c("TYPE", "type"), c("STATUS", "status"), c("PRIORITY", "priority"), c("ASSIGNEE", "assignee"), c("TITLE", "title")}
	scheduleCols = []col{c("ID", "id"), c("NAME", "name"), c("KIND", "kind"), c("CRON", "cron"), c("EVERY(S)", "interval_seconds"), c("TARGET", "target"), c("ENABLED", "enabled"), c("NEXT", "next_at")}
	holdCols     = []col{c("ID", "id"), c("SCOPE", "scope"), c("ACCOUNT", "account_id"), c("GATEWAY", "gateway_ref"), c("KIND", "kind"), c("RESETS", "resets_at"), c("SHARED", "shared"), c("SEEN", "seen_at"), c("CLEARED", "cleared_at")}
	waitCols     = []col{c("ID", "id"), c("RUN", "run_id"), c("ROLE", "role"), c("TASK", "task_id"), c("STATE", "state"), c("RESUME", "resume_at"), c("RELEASE", "release_at"), c("ATTEMPTS", "attempts")}
	limitCols    = []col{c("KEY", "key"), c("VALUE", "value"), c("UNIT", "unit"), c("SOURCE", "source"), c("OVERRIDE", "override"), c("DEFAULT", "default"), c("LABEL", "label")}
)

func (a *App) inboxCmd() *cobra.Command {
	return group("inbox", "A space's inbox: list, show, add and triage items (--space)", nil,
		a.cmd(opts{use: "list", short: "Inbox items, newest first", op: "list-inbox", alias: []string{"ls"}, scoped: true, paged: true,
			query: []qflag{qs("status", "status", "only items with this status (new, in_progress, deferred, rejected, done)"), limitFlag()},
			view:  view{items: "items", cols: inboxCols, empty: "The inbox is empty."}}),
		a.cmd(opts{use: "get", short: "Show an inbox item", op: "get-inbox-item", scoped: true, args: pos("item")}),
		a.cmd(opts{use: "create", short: "Add an item to the inbox", op: "create-inbox-item", scoped: true, idem: true,
			example: "spaceman inbox create --space payments --title \"Rotate the signing key\" --body-file note.md --urgency high",
			form: func(f *form) {
				f.str("title", "title", "title").text("body", "body", "body (Markdown, or JSON with --content-type application/json; required)").
					str("urgency", "urgency", "low, normal, high or urgent").str("content-type", "content_type", "text/markdown (default) or application/json").
					str("correlation-id", "correlation_id", "correlation ID").list("attachment", "attachment_ids", "attachment ID")
			}}),
		a.cmd(opts{use: "set-status", short: "Triage an item: new, in_progress, deferred, rejected or done", op: "set-inbox-status", get: "get-inbox-item",
			scoped: true, args: pos("item"), bodyArgs: []string{"status"},
			form: func(f *form) {
				f.str("reason", "reason", "reason").str("defer-until", "defer_until", "with deferred: RFC 3339 time to bring it back").
					str("linked-kind", "linked_kind", "kind of a linked object").str("linked-id", "linked_id", "ID of a linked object")
			}}),
	)
}

func (a *App) taskCmd() *cobra.Command {
	create := func(f *form) {
		f.str("title", "title", "title (required)").text("objective", "objective", "what the task should achieve").
			text("acceptance", "acceptance", "acceptance criteria").str("priority", "priority", "low, normal, high or urgent").
			str("kind", "kind", "ordinary (default) or incident").str("status", "status", "backlog or ready (default)").
			str("role", "assignee_role", "role that should work on it").str("workspace", "workspace", "workspace").
			str("parent", "parent_id", "parent task ID").list("depends-on", "depends_on", "task it depends on").
			list("exclusion", "exclusions", "exclusion").str("from-inbox", "from_inbox_item", "inbox item the task comes from")
	}
	return group("task", "The execution queue: tasks, blockers, dependencies, escalation, decisions and policy (--space)", []string{"tasks", "queue"},
		a.cmd(opts{use: "list", short: "The space's queue in order, with its policy", op: "list-tasks", alias: []string{"ls"}, scoped: true,
			view: view{items: "tasks", cols: taskCols, empty: "The queue is empty."}}),
		a.cmd(opts{use: "get", short: "Show a task", op: "get-task", scoped: true, args: pos("task")}),
		a.cmd(opts{use: "create", short: "Add a task to the queue", op: "create-task", scoped: true, idem: true, form: create}),
		a.cmd(opts{use: "update", short: "Edit or reassign a task (only the flags given change)", op: "update-task", get: "get-task", merge: true, scoped: true,
			args: pos("task"),
			form: func(f *form) {
				f.str("title", "title", "title").text("objective", "objective", "objective").text("acceptance", "acceptance", "acceptance criteria").
					str("priority", "priority", "low, normal, high or urgent").str("role", "assignee_role", "assignee role (empty clears it)").
					str("run", "assignee_run", "assignee run ID (empty clears it)").
					str("workspace", "workspace", "workspace").list("exclusion", "exclusions", "exclusion (replaces the list)")
			}}),
		a.cmd(opts{use: "move", short: "Reorder a task: --top, --bottom, --before or --after", op: "move-task", scoped: true, args: pos("task"),
			form: func(f *form) {
				f.str("before", "before_id", "place it before this task").str("after", "after_id", "place it after this task").
					boolean("top", "to", "move it to the top").boolean("bottom", "to", "move it to the bottom")
			},
			prepare: func(_ context.Context, cmd *cobra.Command, _ *call, body map[string]any) error {
				top, _ := cmd.Flags().GetBool("top")
				bottom, _ := cmd.Flags().GetBool("bottom")
				n := 0
				for _, k := range []string{"before_id", "after_id"} {
					if _, ok := body[k]; ok {
						n++
					}
				}
				delete(body, "to")
				switch {
				case top && bottom:
					return usageErrorf("give one of --top, --bottom, --before or --after")
				case top:
					body["to"] = "top"
					n++
				case bottom:
					body["to"] = "bottom"
					n++
				}
				if n != 1 {
					return usageErrorf("give exactly one of --top, --bottom, --before or --after")
				}
				return nil
			}}),
		a.cmd(opts{use: "set-status", short: "Change a task's status", op: "set-task-status", get: "get-task", scoped: true, args: pos("task"),
			bodyArgs: []string{"status"},
			form: func(f *form) {
				f.str("reason", "reason", "reason").text("result", "result", "result summary")
			}}),
		a.cmd(opts{use: "next", short: "The task the queue would start next, and why", op: "queue-next", scoped: true}),
	)
}

func (a *App) humanCmd() *cobra.Command {
	return group("human", "Human actions: list, create, respond, assign, comment and cancel; types and policies", []string{"human-action", "human-actions"},
		a.cmd(opts{use: "list", short: "Open human actions (every space you can see, or one with --space)", op: "list-human-actions",
			global: "list-pending-human-actions", alias: []string{"ls"}, scoped: true,
			query: []qflag{qb("pending", "pending", "with --space: only open actions"), qs("type", "type", "only this type"),
				qs("search", "q", "search the title and body (every word must occur)"), qs("status", "status", "only this status"),
				qs("assignee", "assignee", "a user ID, me or unassigned"), qs("priority", "priority", "only this priority"),
				qs("requester", "requester", "the requester's kind, ID, run or role, or part of its label"),
				qs("due", "due", "overdue, due or expiring"), qs("sort", "sort", "rank, priority, due, newest, oldest or updated"), limitFlag()},
			view: view{items: "items", cols: humanCols, empty: "No human actions."}}),
		a.cmd(opts{use: "get", short: "Show a human action", op: "get-human-action", scoped: true, args: pos("action")}),
		a.cmd(opts{use: "respond", short: "Answer a human action with an outcome", op: "respond-human-action", get: "get-human-action", scoped: true,
			args:    pos("action"),
			example: "spaceman human respond --space payments hac_01... --outcome approved --note \"ok for staging\"",
			form: func(f *form) {
				f.str("outcome", "outcome", "outcome (required; one of the action type's outcomes)").text("answer", "answer", "answer text").
					str("note", "note", "note").doc("data", "data", "structured response (JSON or YAML file, - for standard input)")
			}}),
		a.cmd(opts{use: "cancel", short: "Cancel a human action", op: "cancel-human-action", get: "get-human-action", scoped: true, args: pos("action"),
			form: func(f *form) { f.str("reason", "reason", "reason") }}),
	)
}

func (a *App) scheduleCmd() *cobra.Command {
	sched := func(f *form) {
		f.str("name", "name", "name (required)").str("kind", "kind", "cron or interval (required)").str("cron", "cron", "cron expression (kind cron)").
			integer("every", "interval_seconds", "interval in seconds (kind interval)").str("timezone", "timezone", "IANA time zone of the cron expression").
			str("anchor", "anchor", "RFC 3339 start of the interval").str("target", "target", "inbox or task (required)").
			str("title", "title", "title of each created item (required)").text("body", "body", "body of each created item").
			str("priority", "priority", "low, normal, high or urgent").str("role", "assignee_role", "assignee role (task target)").
			str("missed", "missed", "skip, run_once or catch_up").integer("catch-up-max", "catch_up_max", "most occurrences to catch up").
			boolean("skip-if-open", "skip_if_open", "skip while the previous item is still open").boolean("enabled", "enabled", "enabled").
			str("description", "description", "description")
	}
	return group("schedule", "A space's schedules: list, show, create, update and delete (--space)", []string{"schedules"},
		a.cmd(opts{use: "list", short: "A space's schedules", op: "list-schedules", alias: []string{"ls"}, scoped: true,
			view: view{items: "items", cols: scheduleCols, empty: "No schedules."}}),
		a.cmd(opts{use: "get", short: "Show a schedule with its recent occurrences", op: "get-schedule", scoped: true, args: pos("schedule")}),
		a.cmd(opts{use: "create", short: "Add a schedule", op: "create-schedule", scoped: true, form: sched,
			example: "spaceman schedule create --space payments --name nightly --kind cron --cron \"0 2 * * *\" --timezone Europe/Vilnius \\\n" +
				"  --target task --title \"Nightly dependency review\" --role worker"}),
		a.cmd(opts{use: "update", short: "Edit a schedule (only the flags given change)", op: "update-schedule", get: "get-schedule", merge: true,
			scoped: true, args: pos("schedule"), form: sched}),
		a.cmd(opts{use: "preview", short: "The next occurrences of a schedule definition, without saving it", op: "preview-schedule", scoped: true, form: sched}),
		a.cmd(opts{use: "delete", short: "Delete a schedule", op: "delete-schedule", scoped: true, args: pos("schedule"), done: "Schedule deleted."}),
	)
}

func (a *App) limitsCmd() *cobra.Command {
	var sets, unsets []string
	var onLower string
	set := a.cmd(opts{use: "set", short: "Override a space's limits (keys not given keep their override)", op: "set-space-limits", get: "get-space-limits",
		scoped: true, optionalIfMatch: true,
		long: "Reads the space's current overrides, applies --set KEY=VALUE and --unset KEY, and stores the result with If-Match. A key " +
			"without an override inherits the installation default. See `spaceman limits definitions` for keys, units and bounds.",
		example: "spaceman limits set --space payments --set concurrent_runs=3 --unset tool_calls_per_run",
		base: func(obj map[string]any) map[string]any {
			values := map[string]any{}
			items, _ := obj["limits"].([]any)
			for _, it := range items {
				m, _ := it.(map[string]any)
				if src, _ := m["source"].(string); src == "space" && m["override"] != nil {
					values[cell(m["key"])] = m["override"]
				}
			}
			return map[string]any{"values": values}
		},
		prepare: func(_ context.Context, cmd *cobra.Command, c *call, body map[string]any) error {
			values, _ := body["values"].(map[string]any)
			for _, s := range sets {
				k, v, ok := strings.Cut(s, "=")
				n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				if !ok || strings.TrimSpace(k) == "" || err != nil {
					return usageErrorf("--set takes KEY=INTEGER, got %q", s)
				}
				values[strings.TrimSpace(k)] = n
			}
			for _, k := range unsets {
				delete(values, strings.TrimSpace(k))
			}
			if len(sets)+len(unsets) == 0 && !cmd.Flags().Changed("on-lower") {
				return usageErrorf("nothing to change: give --set, --unset or --on-lower")
			}
			if onLower != "" {
				body["on_lower"] = onLower
			}
			return nil
		}, view: limitsView()})
	set.Flags().StringArrayVar(&sets, "set", nil, "override KEY=VALUE (repeatable)")
	set.Flags().StringArrayVar(&unsets, "unset", nil, "remove the override of KEY (repeatable)")
	set.Flags().StringVar(&onLower, "on-lower", "", "drain (default) or stop: what lowering the concurrent-runs limit does to running work")
	return group("limits", "Space limits: show, override and list definitions; usage-limit holds and waits", []string{"limit"},
		a.cmd(opts{use: "get", short: "A space's effective limits and where each value comes from", op: "get-space-limits", scoped: true, view: limitsView()}),
		set,
		a.cmd(opts{use: "holds", short: "A space's usage-limit holds, and other spaces' open holds on its accounts (shared)", op: "list-space-usage-limits",
			scoped: true, view: view{items: "items", cols: holdCols, empty: "No usage-limit holds."}}),
		a.cmd(opts{use: "clear", short: "Clear one of the space's own usage-limit holds now (other spaces' holds on the account remain)", op: "clear-space-usage-limit",
			scoped: true, args: pos("limit")}),
		a.cmd(opts{use: "waits", short: "A space's runs parked at a usage limit, waiting to resume", op: "list-space-limit-waits",
			scoped: true, view: view{items: "items", cols: waitCols, empty: "No runs waiting for a usage limit."}}),
		a.cmd(opts{use: "resume", short: "Resume a run parked at a usage limit now (the usual launch checks apply)", op: "resume-limit-wait",
			scoped: true, args: pos("wait")}),
		a.cmd(opts{use: "release", short: "Give up on a run parked at a usage limit; its task goes back to the queue", op: "release-limit-wait",
			scoped: true, args: pos("wait")}),
		a.cmd(opts{use: "definitions", short: "Every limit: unit, bounds, default and enforcement point", op: "list-limit-definitions",
			view: view{items: "items", cols: []col{c("KEY", "key"), c("UNIT", "unit"), c("DEFAULT", "default"), c("MIN", "min"), c("MAX", "max"), c("ENFORCED", "enforced")}}}),
	)
}

// limitsView shows the limits as a table (the response is an object with a limits array).
func limitsView() view {
	return view{items: "limits", cols: limitCols}
}

// draftVersion returns the version of the draft in obj's versions (publishing needs it as If-Match).
func draftVersion(obj map[string]any) string {
	vs, _ := obj["versions"].([]any)
	for _, v := range vs {
		m, _ := v.(map[string]any)
		if st, _ := m["state"].(string); st == "draft" {
			return numberString(m["version"])
		}
	}
	if d, ok := obj["draft"].(map[string]any); ok {
		return numberString(d["version"])
	}
	return ""
}
