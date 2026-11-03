package cli

import (
	"context"

	"github.com/spf13/cobra"
)

var (
	humanTypeCols  = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), c("LATEST", "latest"), {head: "VERSIONS", fn: countOf("versions")}}
	spaceTypeCols  = []col{c("TYPE", "type_id"), c("SLUG", "slug"), c("NAME", "name"), c("LABEL", "label"), c("VERSION", "number"), c("PINNED", "pinned"), c("OUTCOMES", "outcomes")}
	typeOptionCols = []col{c("ID", "id"), c("SLUG", "slug"), c("NAME", "name"), {head: "VERSIONS", fn: countOf("versions")}}
	policyEffCols  = []col{c("TYPE", "action_type"), c("SOURCE", "source"), c("REMIND AFTER", "remind_after_minutes"), c("EVERY", "remind_every_minutes"),
		c("MAX", "max_reminders"), c("ESCALATE AFTER", "escalate_after_minutes"), c("REQUESTER ENDED", "requester_ended"), c("VERSION", "version")}
	approvalCols = []col{c("ID", "id"), c("OPERATION", "operation"), c("STATE", "state"), c("REQUESTER", "requester_role"), c("USES LEFT", "uses_left"), c("VALID UNTIL", "valid_until"), c("ACTION", "human_action_id")}
)

// humanExtra are the human-action commands beyond list, get, respond and cancel.
func (a *App) humanExtra() []*cobra.Command {
	var unassign bool
	assign := a.cmd(opts{use: "assign", short: "Assign (claim) a human action to a user, or --unassign it", op: "assign-human-action", get: "get-human-action",
		scoped: true, args: pos("action"),
		form: func(f *form) { f.str("assignee", "assignee", "user ID to assign it to") },
		prepare: func(_ context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
			_, ok := body["assignee"]
			switch {
			case unassign && ok:
				return usageErrorf("give --assignee or --unassign, not both")
			case unassign:
				body["assignee"] = ""
			case !ok:
				return usageErrorf("give --assignee USER-ID or --unassign")
			}
			return nil
		}})
	assign.Flags().BoolVar(&unassign, "unassign", false, "return the action to open")
	return []*cobra.Command{
		a.cmd(opts{use: "create", short: "Ask a person for something (you are the requester)", op: "create-human-action", scoped: true, idem: true,
			example: "spaceman human create --space payments --type approval --title \"Deploy to production?\" --request-file approval.json",
			form: func(f *form) {
				f.str("type", "type", "question, approval, todo, message or custom (required)").str("type-slug", "type_slug", "with custom: a catalog type enabled in the space").
					str("title", "title", "title (required)").text("body", "body", "body (Markdown)").
					doc("request", "request", "type-specific request (JSON or YAML file): question {options, allow_text}, approval {action, details}, todo {steps}, message {requires_ack}").
					str("task", "task_id", "task it belongs to").boolean("blocks-task", "blocks_task", "block the task until a person responds").
					str("priority", "priority", "low, normal, high or urgent").str("due-at", "due_at", "RFC 3339 due time").
					str("expires-at", "expires_at", "RFC 3339 expiry").str("dedup-key", "dedup_key", "a repeated key returns the original action").
					str("correlation-id", "correlation_id", "correlation ID").str("follow-up-of", "follow_up_of", "earlier action this corrects")
			}}),
		assign,
		a.cmd(opts{use: "comment", short: "Comment on a human action (comments never count as a response)", op: "comment-human-action", scoped: true,
			args: pos("action"), form: func(f *form) {
				f.text("body", "body", "comment (Markdown, required)").list("attachment", "attachment_ids", "attachment ID to reference (repeatable)")
			}}),
		a.cmd(opts{use: "clarify", short: "Ask the requester for more information (the action stays pending; never a response)", op: "clarify-human-action",
			scoped: true, args: pos("action"), form: func(f *form) {
				f.text("body", "body", "the question (Markdown, required)").list("attachment", "attachment_ids", "attachment ID to reference (repeatable)")
			}}),
		a.cmd(opts{use: "move", short: "Reorder a pending human action (before another, or last)", op: "move-human-action", scoped: true, args: pos("action"),
			form: func(f *form) { f.str("before", "before", "place it before this pending action (default: last)") }}),
		a.cmd(opts{use: "route", short: "Send a response notification no agent acknowledged to control or a running role", op: "route-human-notification",
			scoped: true, args: pos("action", "notification"),
			form: func(f *form) { f.str("to", "to", "control, or role:NAME (a running run of that role; required)") }}),
		a.cmd(opts{use: "approval", short: "The enforced approval behind an approval action", op: "get-action-approval", scoped: true, args: pos("action")}),
		a.humanTypeCmd(),
		a.humanPolicyCmd(),
	}
}

// typeDraftBase is the draft to edit: the current draft, or a new one starting from the latest published version.
func typeDraftBase(obj map[string]any) map[string]any {
	vs, _ := obj["versions"].([]any)
	var pick map[string]any
	best := int64(-1)
	for _, v := range vs {
		m, _ := v.(map[string]any)
		if st, _ := m["state"].(string); st == "draft" {
			pick = m
			break
		}
		if n, ok := atoiPositive(numberString(m["number"])); ok && n > best {
			best, pick = n, m
		}
	}
	out := map[string]any{}
	for _, k := range []string{"request_schema", "response_schema", "ui", "outcomes", "default_priority", "default_expiry_minutes", "notes"} {
		if v, ok := pick[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

func typeDraftFields(f *form) {
	f.list("outcome", "outcomes", "outcome a response may choose (replaces the list)").
		doc("request-schema", "request_schema", "JSON Schema 2020-12 of the request (JSON or YAML file)").
		doc("response-schema", "response_schema", "JSON Schema 2020-12 of the response (JSON or YAML file)").
		doc("ui", "ui", "form layout {order, fields} (JSON or YAML file)").str("default-priority", "default_priority", "low, normal, high or urgent").
		integer("default-expiry-minutes", "default_expiry_minutes", "default expiry").str("notes", "notes", "change notes")
}

func (a *App) humanTypeCmd() *cobra.Command {
	return group("type", "Human-action types: the catalog (list, get, create, draft, publish) and a space's enabled types", []string{"types"},
		a.cmd(opts{use: "list", short: "The human-action type catalog", op: "list-human-types", alias: []string{"ls"},
			view: view{items: "items", cols: humanTypeCols, empty: "No custom types."}}),
		a.cmd(opts{use: "get", short: "A type with its versions (ID or slug)", op: "get-human-type", args: pos("type")}),
		a.cmd(opts{use: "create", short: "Add a type (with a first draft)", op: "create-human-type", bodyArgs: []string{"slug"},
			form: func(f *form) {
				f.str("name", "name", "name (required)").str("description", "description", "description")
				typeDraftFields(f)
			}}),
		a.cmd(opts{use: "draft", short: "Save the type's draft (starts one from the latest version when there is none)", op: "save-human-type-draft",
			get: "get-human-type", canonical: "type_id", version: draftVersion, optionalIfMatch: true, base: typeDraftBase, args: pos("type"),
			form: typeDraftFields}),
		a.cmd(opts{use: "publish", short: "Publish the draft as the next immutable version", op: "publish-human-type", get: "get-human-type",
			canonical: "type_id", version: draftVersion, args: pos("type"),
			prepare: needVersion("the type has no draft to publish; save one with `spaceman human type draft`")}),
		a.cmd(opts{use: "enabled", short: "Custom types enabled in a space, with the version each uses", op: "list-space-human-types", scoped: true,
			view: view{items: "items", cols: spaceTypeCols, empty: "No custom types enabled."}}),
		a.cmd(opts{use: "options", short: "Catalog types a space can enable", op: "list-human-type-options", scoped: true,
			view: view{items: "items", cols: typeOptionCols, empty: "No published types."}}),
		a.cmd(opts{use: "enable", short: "Enable a type in a space (follow the latest version, or --pin one)", op: "enable-space-human-type", scoped: true,
			args: pos("type"), view: view{items: "items", cols: spaceTypeCols},
			form: func(f *form) {
				f.str("pin", "version_pin", "published version ID to pin (default: follow the latest)").str("label", "label", "label of this variant").
					str("default-priority", "default_priority", "low, normal, high or urgent").integer("default-expiry-minutes", "default_expiry_minutes", "default expiry")
			}}),
		a.cmd(opts{use: "disable", short: "Stop new actions of a type in a space (outstanding ones are unaffected)", op: "disable-space-human-type", scoped: true,
			args: pos("type"), done: "Type disabled in the space."}),
	)
}

func (a *App) humanPolicyCmd() *cobra.Command {
	const atHelp = "ACTION-TYPE is question, approval, todo, message, custom or all (every type)."
	const setHelp = atHelp + " The fields not given keep the stored policy of the type, or else the policy in force for it; for all " +
		"without a stored policy they start from the policy in force for questions."
	return group("policy", "Reminder, escalation, rate-limit and routing policies (installation defaults, or a space's with --space)", []string{"policies"},
		a.cmd(opts{use: "list", short: "The policy in force for each type, with its source", op: "list-space-human-policies", global: "list-human-policies",
			globalHelp: "without it, the installation defaults", alias: []string{"ls"}, scoped: true, view: view{items: "effective", cols: policyEffCols}}),
		a.cmd(opts{use: "set", short: "Set the policy of a type (only the flags given change; the rest is the policy in force)", op: "set-space-human-policy",
			global: "set-human-policy", globalHelp: "without it, the installation default", scoped: true, args: pos("action-type"), long: setHelp,
			find: &finder{op: "list-space-human-policies", global: "list-human-policies", items: "items", param: "action_type", keys: []string{"action_type"},
				fallback: "effective", fallbackFirst: true}, merge: true, optionalIfMatch: true,
			example: "spaceman human policy set approval --remind-after-minutes 30 --escalate-after-minutes 120 --escalate-priority urgent\n" +
				"spaceman human policy set --space payments all --requester-ended reroute --successor-role control",
			form: func(f *form) {
				f.integer("remind-after-minutes", "remind_after_minutes", "remind this long after the action was asked (0: no reminders)").
					integer("remind-every-minutes", "remind_every_minutes", "repeat the reminder this often (0: once)").
					integer("max-reminders", "max_reminders", "most reminders").
					integer("escalate-after-minutes", "escalate_after_minutes", "escalate this long after the action was asked (0: never)").
					str("escalate-priority", "escalate_priority", "raise the priority to high or urgent when it escalates").
					str("escalate-assignee", "escalate_assignee", "user ID to assign it to when it escalates").
					str("requester-ended", "requester_ended", "when the requester's run has ended: reroute or next_launch").
					integer("hold-minutes", "hold_minutes", "with next_launch: how long to wait before rerouting").
					str("successor-role", "successor_role", "reroute to a live run of this role before control").
					boolean("wake-idle", "wake_idle", "wake an idle requester with a one-line terminal notice").
					integer("rate-per-run-hour", "rate_per_run_hour", "new actions of this type one run may ask per hour").
					integer("rate-per-space-hour", "rate_per_space_hour", "new actions of this type a space's agents may ask per hour")
			}}),
		a.cmd(opts{use: "delete", short: "Remove a stored policy (the next less specific one applies)", op: "delete-space-human-policy",
			global: "delete-human-policy", globalHelp: "without it, the installation default", scoped: true, args: pos("action-type"), long: atHelp,
			done: "Policy removed."}),
	)
}

func (a *App) approvalCmd() *cobra.Command {
	return group("approval", "Enforced approvals of a space: list, show and revoke (decide them with `spaceman human respond`)", []string{"approvals"},
		a.cmd(opts{use: "list", short: "A space's approvals with their executions", op: "list-approvals", alias: []string{"ls"}, scoped: true,
			view: view{items: "items", cols: approvalCols, empty: "No approvals."}}),
		a.cmd(opts{use: "get", short: "Show an approval", op: "get-approval", scoped: true, args: pos("approval")}),
		a.cmd(opts{use: "revoke", short: "Revoke an approval before (further) execution", op: "revoke-approval", scoped: true, args: pos("approval"),
			form: func(f *form) { f.str("reason", "reason", "reason") }}),
	)
}
