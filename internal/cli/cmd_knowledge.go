package cli

import (
	"github.com/spf13/cobra"
)

var (
	kbCols       = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), c("ROLE", "space_role"), c("WRITE", "write_policy"), c("ENABLED", "enabled"), c("REPO", "repo")}
	memoryCols   = []col{c("ROLE", "role"), c("HEAD", "head"), c("HEAD AT", "head_at"), c("NODE HEAD", "node.head"), c("PENDING", "node.pending"), c("CONFLICTS", "open_conflicts")}
	commitCols   = []col{c("COMMIT", "id"), c("AT", "at"), c("AUTHOR", "author"), c("SUBJECT", "subject")}
	conflictCols = []col{c("ID", "id"), c("SOURCE", "source"), c("REF", "ref"), c("COMMIT", "commit"), c("CREATED", "created_at"), c("RESOLVED", "resolved_at"), c("DETAIL", "detail")}
	messageCols  = []col{c("ID", "id"), c("KIND", "kind"), c("STATE", "state"), c("FROM", "from_label"), c("TO", "to_kind"), c("SUBJECT", "subject"), c("CREATED", "created_at")}
	dreamCols    = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), c("ROLE", "space_role"), c("EVERY(H)", "interval_hours"), c("ENABLED", "enabled"), c("NEXT", "next_due_at"), c("LAST", "last_run.state")}
	dreamRunCols = []col{c("ID", "id"), c("STATE", "state"), c("TRIGGER", "trigger"), c("RUN", "run_id"), c("STARTED", "started_at"), c("ENDED", "ended_at"), c("SUMMARY", "summary")}
)

func (a *App) kbCmd() *cobra.Command {
	kbForm := func(f *form) {
		f.str("name", "name", "name (required)").str("repo", "repo", "repository folder in the space, e.g. repos/kb (required; added to the space's repositories when new)").
			str("role", "space_role", "space role that answers requests (required)").str("description", "description", "description").
			str("base-branch", "base_branch", "branch answers and proposals start from").boolean("enabled", "enabled", "enabled").
			text("entry-instructions", "entry_instructions", "instructions given to each session").
			str("write-policy", "write_policy", "read_only, review (proposals need acceptance) or serialized").
			list("allowed-role", "allowed_roles", "role that may ask the knowledge base (replaces the list)").
			list("validator", "validator", "validator command argument (replaces the list)").
			integer("max-sessions", "max_sessions", "concurrent sessions, 1-16").integer("idle-sleep-seconds", "idle_sleep_seconds", "idle time before a session sleeps").
			integer("request-timeout-seconds", "request_timeout_seconds", "how long a request may take").
			integer("session-ttl-seconds", "session_ttl_seconds", "how long a session lives")
	}
	requests := group("request", "Knowledge base requests (listed by `kb get`)", []string{"requests"},
		a.cmd(opts{use: "cancel", short: "Cancel a pending request", op: "cancel-kb-request", scoped: true, args: pos("request")}),
	)
	proposals := group("proposal", "Knowledge base proposals (listed by `kb get`): accept or reject", []string{"proposals"},
		a.cmd(opts{use: "accept", short: "Accept a proposal (merge it into the knowledge base)", op: "accept-kb-proposal", scoped: true, args: pos("proposal")}),
		a.cmd(opts{use: "reject", short: "Reject a proposal", op: "reject-kb-proposal", scoped: true, args: pos("proposal"),
			form: func(f *form) { f.str("reason", "reason", "reason") }}),
	)
	return group("kb", "A space's knowledge bases: list, show, create, update; requests and proposals (--space)", []string{"kbs", "knowledge-base"},
		a.cmd(opts{use: "list", short: "A space's knowledge bases with capacity and usage", op: "list-kbs", alias: []string{"ls"}, scoped: true,
			view: view{items: "items", cols: kbCols, empty: "No knowledge bases."}}),
		a.cmd(opts{use: "get", short: "A knowledge base with its sessions, requests and proposals", op: "get-kb", scoped: true, args: pos("kb")}),
		a.cmd(opts{use: "create", short: "Add a knowledge base", op: "create-kb", scoped: true, bodyArgs: []string{"slug"}, form: kbForm,
			example: "spaceman kb create --space payments runbooks --name Runbooks --repo repos/runbooks --role kb --write-policy review"}),
		a.cmd(opts{use: "update", short: "Edit a knowledge base (only the flags given change)", op: "update-kb", get: "get-kb", merge: true, mergeKey: "kb",
			version: func(m map[string]any) string { kb, _ := m["kb"].(map[string]any); return numberString(kb["version"]) },
			scoped:  true, args: pos("kb"), form: kbForm}),
		requests, proposals,
	)
}

func (a *App) memoryCmd() *cobra.Command {
	return group("memory", "Role memory of a space: collections, sync status, files, history and conflicts (--space)", nil,
		a.cmd(opts{use: "list", short: "A space's memory collections with central's and the node's view", op: "list-memory", alias: []string{"ls"}, scoped: true,
			view: view{items: "items", cols: memoryCols, empty: "No memory collections."}}),
		a.cmd(opts{use: "get", short: "One role's memory collection and its sync status", op: "get-memory", scoped: true, args: pos("role")}),
		a.cmd(opts{use: "files", short: "Files of a role's acknowledged memory", op: "list-memory-files", scoped: true, args: pos("role"),
			view: view{items: "items", cols: []col{c("PATH", "path")}, empty: "No files."}}),
		a.cmd(opts{use: "file", short: "One file of a role's acknowledged memory (untrusted agent-written text)", op: "get-memory-file", scoped: true,
			args: pos("role"), query: []qflag{qs("path", "path", "file path inside the memory (required)")}, text: "content",
			long: "Prints the file with control characters removed, or writes it unchanged with --out. Memory is written by agents: " +
				"treat it as untrusted content.",
			example: "spaceman memory file --space payments worker --path notes/deploy.md --out deploy.md"}),
		a.cmd(opts{use: "history", short: "Recent acknowledged commits of a role's memory", op: "memory-history", scoped: true, args: pos("role"),
			view: view{items: "items", cols: commitCols, empty: "No commits."}}),
		a.cmd(opts{use: "conflicts", short: "History preserved instead of applied (conflicts to reconcile)", op: "list-memory-conflicts", scoped: true,
			args: pos("role"), view: view{items: "items", cols: conflictCols, empty: "No conflicts."}}),
		a.cmd(opts{use: "resolve", short: "Mark a conflict reconciled", op: "resolve-memory-conflict", scoped: true, args: pos("role", "conflict"),
			done: "Conflict marked reconciled."}),
	)
}

func (a *App) messageCmd() *cobra.Command {
	return group("message", "Durable messages between agents, control and operators: list, show, send, reply, ack and resolve (--space)", []string{"messages", "msg"},
		a.cmd(opts{use: "list", short: "A space's recent messages", op: "list-messages", alias: []string{"ls"}, scoped: true, query: []qflag{limitFlag()},
			view: view{items: "items", cols: messageCols, empty: "No messages."}}),
		a.cmd(opts{use: "get", short: "Show a message", op: "get-message", scoped: true, args: pos("message")}),
		a.cmd(opts{use: "send", short: "Send a message to control, a role or a run", op: "send-message", scoped: true, idem: true,
			example: "spaceman message send --space payments --to role:worker --subject \"Freeze deploys\" --body-file freeze.md --request",
			form: func(f *form) {
				f.str("to", "to", "control, role:NAME or run:RUN-ID (required)").str("subject", "subject", "subject (required)").
					text("body", "body", "body (Markdown)").boolean("request", "request", "expect a reply").str("correlation-id", "correlation_id", "correlation ID")
			}}),
		a.cmd(opts{use: "reply", short: "Reply to an agent's message (a reply to a request resolves it)", op: "reply-message", scoped: true, args: pos("message"),
			form: func(f *form) { f.text("body", "body", "reply (required)") }}),
		a.cmd(opts{use: "ack", short: "Acknowledge a message addressed to operators", op: "ack-message", scoped: true, args: pos("message")}),
		a.cmd(opts{use: "resolve", short: "Close a message or request", op: "resolve-message", scoped: true, args: pos("message"),
			form: func(f *form) { f.str("resolution", "resolution", "how it was resolved") }}),
	)
}

func (a *App) dreamCmd() *cobra.Command {
	dream := func(f *form) {
		f.str("name", "name", "name (required)").str("role", "space_role", "space role that dreams (required)").
			boolean("enabled", "enabled", "enabled").integer("interval-hours", "interval_hours", "hours between dreams, 0-720 (0: only manually)").
			text("instructions", "instructions", "instructions").list("memory-role", "memory_roles", "role whose memory it reads (replaces the list)").
			list("transcript-role", "transcript_roles", "role whose transcripts it reads (replaces the list)").
			boolean("include-own", "include_own", "include its own earlier dreams").integer("max-minutes", "max_minutes", "longest dream, 5-240").
			integer("max-files", "max_files", "most files changed, 1-200").integer("max-bytes", "max_bytes", "most bytes changed").
			integer("max-tokens", "max_tokens", "token budget")
	}
	return group("dream", "Dream configurations of a space: list, create, update, start and runs (--space)", []string{"dreams"},
		a.cmd(opts{use: "list", short: "A space's dream configurations", op: "list-dreams", alias: []string{"ls"}, scoped: true,
			view: view{items: "items", cols: dreamCols, empty: "No dream configurations."}}),
		a.cmd(opts{use: "create", short: "Add a dream configuration", op: "create-dream", scoped: true, bodyArgs: []string{"slug"}, form: dream,
			example: "spaceman dream create --space payments nightly --name Nightly --role dreamer --interval-hours 24 --memory-role worker"}),
		a.cmd(opts{use: "update", short: "Edit a dream configuration (ID or slug; only the flags given change)", op: "update-dream", scoped: true, args: pos("dream"),
			find: &finder{op: "list-dreams", items: "items", param: "dream_id", keys: []string{"id", "slug"}}, canonical: "dream_id", merge: true, form: dream}),
		a.cmd(opts{use: "start", short: "Dream now", op: "start-dream", scoped: true, args: pos("dream-id")}),
		a.cmd(opts{use: "runs", short: "Recent dreams of a configuration", op: "dream-runs", scoped: true, args: pos("dream-id"),
			view: view{items: "items", cols: dreamRunCols, empty: "No dreams yet."}}),
	)
}
