package cli

import (
	"context"
	"encoding/base64"
	"io/fs"
	"path/filepath"

	"github.com/spf13/cobra"
)

// Extra commands of the existing groups (tasks, runs, spaces, accounts, MCP, roles, prompts, skills, events) and the
// attachment group.

var (
	decisionCols   = []col{c("AT", "at"), c("BY", "by"), c("SELECTED", "selected_id"), c("SKIPPED", "skipped")}
	recordCols     = []col{c("SEQ", "seq"), c("KIND", "k"), c("T", "t"), {head: "BYTES", fn: decodedLen("d")}, c("COLS", "cols"), c("ROWS", "rows")}
	quarantineCols = []col{c("ID", "id"), c("STATUS", "status"), c("TYPE", "type"), c("SUBJECT", "subject"), c("EVENT", "event_id"), c("CREATED", "created_at"), c("ERROR", "error")}
)

func decodedLen(key string) func(map[string]any) string {
	return func(m map[string]any) string {
		s, _ := m[key].(string)
		if s == "" {
			return ""
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
		return cell(len(b))
	}
}

func (a *App) taskExtra() []*cobra.Command {
	var none bool
	depends := a.cmd(opts{use: "depends", short: "Replace a task's dependencies (--on, repeatable; --none clears them; cycles are refused)",
		op: "set-task-dependencies", scoped: true, args: pos("task"),
		form: func(f *form) { f.list("on", "depends_on", "task it depends on") },
		prepare: func(_ context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
			_, ok := body["depends_on"]
			switch {
			case none && ok:
				return usageErrorf("give --on or --none, not both")
			case none:
				body["depends_on"] = []any{}
			case !ok:
				return usageErrorf("give the complete new set with --on (repeatable), or --none")
			}
			return nil
		}})
	depends.Flags().BoolVar(&none, "none", false, "remove every dependency")
	policy := a.cmd(opts{use: "set-policy", short: "Set the queue policy (only the flags given change)", op: "set-queue-policy", scoped: true,
		long: "Reads the queue's policy and its version just before the change, so a concurrent edit is a conflict (exit code 3).",
		form: func(f *form) {
			f.integer("max-active", "max_active", "top-level tasks that may run at once").
				str("incident-insertion", "incident_insertion", "where new incident tasks go: top or priority").
				boolean("incident-preempts", "incident_preempts", "a new incident returns running ordinary tasks to ready").
				boolean("escalation-reorders", "escalation_reorders", "escalation may move a task to the top")
		},
		prepare: func(ctx context.Context, _ *cobra.Command, c *call, body map[string]any) error {
			res, err := a.do(ctx, call{op: "list-tasks", params: c.params})
			if err != nil {
				return err
			}
			obj, err := res.Object()
			if err != nil {
				return err
			}
			cur, _ := obj["policy"].(map[string]any)
			for _, k := range []string{"max_active", "incident_insertion", "incident_preempts", "escalation_reorders", "version"} {
				if _, ok := body[k]; !ok || k == "version" {
					if v, ok := cur[k]; ok {
						body[k] = v
					}
				}
			}
			return nil
		}})
	policy.Annotations[annOps] = "list-tasks," + policy.Annotations[annOps]
	return []*cobra.Command{
		a.cmd(opts{use: "block", short: "Block a task explicitly (other tasks continue)", op: "add-task-blocker", scoped: true, args: pos("task"),
			form: func(f *form) {
				f.str("kind", "kind", "human_action or external (required)").str("ref", "ref_id", "ID of what it waits for (a human action)").
					str("note", "note", "note")
			}}),
		a.cmd(opts{use: "unblock", short: "Remove a blocker from a task", op: "resolve-task-blocker", scoped: true, args: pos("task", "blocker")}),
		a.cmd(opts{use: "escalate", short: "Escalate a task to urgent (to the top when the queue policy allows)", op: "escalate-task", scoped: true,
			args: pos("task"), form: func(f *form) { f.str("reason", "reason", "reason (required)") }}),
		depends,
		a.cmd(opts{use: "decisions", short: "Recorded queue selections: what was started and what was skipped, and why", op: "queue-decisions", scoped: true,
			view: view{items: "items", cols: decisionCols, empty: "No decisions recorded."}}),
		policy,
	}
}

func (a *App) runExtra() []*cobra.Command {
	recordings := group("recording", "A run's terminal recording: read records, download or delete central's copy", []string{"recordings"},
		a.cmd(opts{use: "get", short: "Records of a run's terminal recording (output is base64; use download for a file)", op: "get-recording", scoped: true,
			args: pos("run"), query: []qflag{qi("after", "after", "records with a sequence greater than this"), limitFlag()},
			view: view{items: "records", cols: recordCols, empty: "No records."}}),
		a.downloadCmd(download{short: "Download a run's recording (zstd-compressed JSON Lines) to a file", route: "/spaces/{space_id}/runs/{run_id}/recording/download",
			param: "run_id", arg: "run", related: []string{"get-recording"},
			long:    "Recordings may contain secrets typed or printed in the terminal; the file is created with mode 0600.",
			example: "spaceman run recording download --space payments run_01… --out run.smrec.zst"}),
		a.cmd(opts{use: "delete", short: "Delete central's copy of a run's recording", op: "delete-recording", scoped: true, args: pos("run"),
			confirm: "delete the recording permanently", done: "Recording deleted."}),
	)
	transcripts := group("transcript", "A run's stored native transcript: download or delete", []string{"transcripts"},
		a.downloadCmd(download{short: "Download a run's stored transcript (JSON Lines) to a file", route: "/spaces/{space_id}/runs/{run_id}/transcript",
			param: "run_id", arg: "run", related: []string{"get-run-usage"},
			long: "Transcripts may contain secrets; they are never printed, and the file is created with mode 0600. `spaceman run usage` " +
				"shows whether one is stored.",
			example: "spaceman run transcript download --space payments run_01… --out transcript.jsonl"}),
		a.cmd(opts{use: "delete", short: "Delete a run's stored transcript", op: "delete-transcript", scoped: true, args: pos("run"),
			confirm: "delete the transcript permanently", done: "Transcript deleted."}),
	)
	return []*cobra.Command{
		a.cmd(opts{use: "input", short: "Send input to a run (delivered only while the program is idle, unless --force)", op: "send-input", scoped: true,
			args: pos("run"), done: "Input accepted.",
			form: func(f *form) {
				f.text("text", "text", "text to type (required)").boolean("force", "force", "deliver even while the program is busy")
			}}),
		a.cmd(opts{use: "usage", short: "A run's token usage and cost estimate (unknown usage is reported as unknown)", op: "get-run-usage", scoped: true,
			args: pos("run")}),
		recordings, transcripts,
	}
}

func (a *App) spaceExtra() []*cobra.Command {
	return []*cobra.Command{
		a.cmd(opts{use: "usage", short: "A space's usage totals and runs (runs with unknown usage are counted separately)", op: "get-space-usage",
			args: spaceArg()}),
	}
}

func (a *App) accountExtra() []*cobra.Command {
	return []*cobra.Command{
		a.cmd(opts{use: "space", short: "Accounts a space's runs may use", op: "list-space-accounts", scoped: true,
			view: view{items: "items", cols: []col{c("ACCOUNT", "account_id"), c("NAME", "name"), c("HARNESS", "harness"), c("DEFAULT", "default"), c("ENABLED", "enabled")},
				empty: "No accounts bound."}}),
		a.cmd(opts{use: "set-space", short: "Replace the accounts a space's runs may use (at most one default per harness)", op: "set-space-accounts", scoped: true,
			example: "spaceman account set-space --space payments --account acc_01… --account acc_02… --default acc_01…",
			form: func(f *form) {
				f.list("account", "items", "account ID the space may use").list("default", "defaults", "account ID that is its harness's default")
			},
			prepare: func(_ context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
				raw, ok := body["items"].([]any)
				defs, _ := body["defaults"].([]any)
				delete(body, "defaults")
				if !ok {
					return usageErrorf("give the complete new set with --account (repeatable)")
				}
				isDef := map[string]bool{}
				for _, d := range defs {
					isDef[cell(d)] = true
				}
				items := []any{}
				for _, r := range raw {
					s, isStr := r.(string)
					if !isStr {
						return nil // objects from --file
					}
					items = append(items, map[string]any{"account_id": s, "default": isDef[s]})
					delete(isDef, s)
				}
				for d := range isDef {
					return usageErrorf("--default %s is not among the --account values", d)
				}
				body["items"] = items
				return nil
			}}),
	}
}

func (a *App) promptExtra() []*cobra.Command {
	return []*cobra.Command{
		a.cmd(opts{use: "diff", short: "Line diff between two prompt versions", op: "diff-prompt-versions",
			query:   []qflag{qs("from", "from", "version ID to compare from (required)"), qs("to", "to", "version ID to compare to (required)")},
			prepare: needQuery("from", "to"), view: view{items: "lines", cols: []col{c("OP", "op"), c("TEXT", "text")}}}),
		a.cmd(opts{use: "discard-draft", short: "Discard a prompt's draft", op: "discard-prompt-draft", args: pos("prompt"),
			confirm: "discard the draft", done: "Draft discarded."}),
		a.cmd(opts{use: "fork", short: "Fork a published version into a new prompt", op: "fork-prompt", args: pos("version-id"), bodyArgs: []string{"slug"},
			form: func(f *form) { f.str("name", "name", "name of the new prompt (required)") }}),
		a.cmd(opts{use: "rollback", short: "Publish an earlier version's content as a new version", op: "rollback-prompt", args: pos("prompt"),
			form: func(f *form) { f.str("version", "version_id", "version ID whose content to publish again (required)") }}),
	}
}

// readSkillDir reads every regular file under dir (skipping .git) as the files of a skill, keyed by slash path.
func (a *App) readSkillDir(dir string) (map[string]any, error) {
	files := map[string]any{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := a.readFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, &UsageError{Msg: err.Error()}
	}
	if len(files) == 0 {
		return nil, usageErrorf("%s holds no files", dir)
	}
	return files, nil
}

func (a *App) skillExtra() []*cobra.Command {
	var createDir, draftDir string
	withDir := func(dir *string) func(context.Context, *cobra.Command, *call, map[string]any) error {
		return func(_ context.Context, _ *cobra.Command, _ *call, body map[string]any) error {
			if *dir == "" {
				if _, ok := body["files"]; !ok {
					return usageErrorf("--dir is required (the skill's files, with SKILL.md)")
				}
				return nil
			}
			files, err := a.readSkillDir(*dir)
			if err != nil {
				return err
			}
			body["files"] = files
			return nil
		}
	}
	create := a.cmd(opts{use: "create", short: "Add a skill (with a first draft) from a directory of text files", op: "create-skill", bodyArgs: []string{"slug"},
		long:    "SKILL.md needs frontmatter naming the skill. Skills give guidance, never permission; nothing in them runs at install time.",
		example: "spaceman skill create release-notes --name \"Release notes\" --dir ./skills/release-notes",
		form: func(f *form) {
			f.str("name", "name", "name (required)").str("description", "description", "description")
		}, prepare: withDir(&createDir)})
	create.Flags().StringVar(&createDir, "dir", "", "directory with the skill's files (SKILL.md and the rest; .git is skipped)")
	draft := a.cmd(opts{use: "draft", short: "Save the skill's draft from a directory (starts one when there is none)", op: "save-skill-draft", get: "get-skill",
		canonical: "skill_id", version: draftVersion, optionalIfMatch: true, args: pos("skill"),
		form: func(f *form) { f.str("notes", "notes", "change notes") }, prepare: withDir(&draftDir)})
	draft.Flags().StringVar(&draftDir, "dir", "", "directory with the skill's files (replaces every file of the draft)")
	return []*cobra.Command{create, draft}
}

func (a *App) eventTemplateExtra() []*cobra.Command {
	return []*cobra.Command{
		a.cmd(opts{use: "preview", short: "Test a template definition against a sample event (nothing is stored)", op: "preview-event-template",
			example: "spaceman event template preview --definition-file tpl.yaml --event-file sample.json --var repository=acme/payments",
			form: func(f *form) {
				f.doc("definition", "definition", "template definition (JSON or YAML file; required)").
					doc("event", "event", "sample event (JSON or YAML file; required)").kv("var", "vars", "template variable")
			}}),
	}
}

func (a *App) quarantineCmd() *cobra.Command {
	resolve := func(verb, action, short, confirm string) *cobra.Command {
		return a.cmd(opts{use: verb, short: short, op: "resolve-quarantine", scoped: true, args: pos("subscription", "entry"),
			static: map[string]any{"action": action}, confirm: confirm})
	}
	return group("quarantine", "Events a subscription quarantined: list, reprocess or discard", nil,
		a.cmd(opts{use: "list", short: "Quarantined events of a subscription", op: "list-quarantine", alias: []string{"ls"}, scoped: true,
			args: pos("subscription"), query: []qflag{qs("status", "status", "open, reprocessed or discarded")},
			view: view{items: "items", cols: quarantineCols, empty: "Nothing quarantined."}}),
		resolve("reprocess", "reprocess", "Process a quarantined event again (after fixing the template)", ""),
		resolve("discard", "discard", "Discard a quarantined event", "discard the event"),
	)
}

func (a *App) attachmentCmd() *cobra.Command {
	return group("attachment", "A space's attachments: upload, show and download (--space)", []string{"attachments"},
		a.cmd(opts{use: "upload", short: "Upload a file; reference the returned uri from an inbox item or a message", op: "upload-attachment",
			scoped: true, idem: true, bodyArgs: []string{"file"}, prepare: a.uploadPrepare,
			long:    "Uploads FILE (- reads standard input) as a multipart form. Unreferenced uploads are removed after 24 hours.",
			example: "spaceman attachment upload --space payments ./screenshot.png"}),
		a.cmd(opts{use: "get", short: "Attachment metadata", op: "get-attachment", scoped: true, args: pos("attachment")}),
		a.downloadCmd(download{short: "Download an attachment's content to a file", route: "/spaces/{space_id}/attachments/{attachment_id}/content",
			param: "attachment_id", arg: "attachment", related: []string{"get-attachment"},
			long:    "Attachments are untrusted content: they are written to a file (mode 0600), never to the terminal.",
			example: "spaceman attachment download --space payments att_01… --out ./screenshot.png"}),
	)
}
