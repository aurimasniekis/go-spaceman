# Retention and export

SpaceMan keeps a space's recordings, transcripts, attachments and human actions until their **retention period** has
passed, and lets space admins **export** a space's records and files as one archive.

## Retention periods

Retention periods are [limits](/guide/limits): the installation sets a default for each in the `limits:` section of
the configuration file, and a space may override any of them on its **Limits** page, with the API
(`PUT /spaces/{id}/limits`), the CLI (`spaceman limits set`) or Terraform (`spaceman_space_limits`). Every default is 0,
which keeps the data until someone deletes it or the space is purged.

| Period | Configuration key | What is deleted once it has passed | What is never deleted |
|---|---|---|---|
| Recording retention | `limits.retention_recordings_days` | central's copy of a run's terminal recording, counted from the end of the run | the recording of a run that is still active; the node's own copy |
| Transcript retention | `limits.retention_transcripts_days` | a run's stored native transcript, counted from when it was stored | the run's usage record |
| Attachment retention | `limits.retention_attachments_days` | an attachment older than the period whose items are all finished: done or rejected inbox items, unchanged for the period, with no open task or human action linked to them | an attachment that an open inbox item, or an item linked to open work, uses |
| Finished human-action retention | `limits.retention_human_actions_days` | a resolved, cancelled or expired human action (with its comments and notifications), counted from when it finished | an action that an open task, an open follow-up or an open inbox item refers to, or whose response was not acknowledged yet |

The audit log has its own, installation-wide period, `retention.audit_days` in the configuration file (0, the default,
keeps it forever; otherwise at least 30 days). It cannot be set per space.

Changing a space's retention periods needs the `retention:write` permission, which only admins hold: a shorter period
deletes data. Other limits on the same page stay editable by the space's operators.

Uploads that no item ever used are still collected after a day by the hourly attachment collection, whatever the
retention period.

## The daily pass

One retention pass a day applies every period, in every space, on central's shared deadline timer: nothing polls per
space. The first pass runs ten minutes after central starts when none ran before, then a day after the previous one
started. Administrators see the installation defaults, audit retention, the next pass and recent passes on the
**Backups** page, and can run a pass at once (**Run a pass now**, or `spaceman system retention run`; it needs a
recent step-up).

- Every deletion is audited (`recording.deleted`, `transcript.deleted`, `attachment.deleted`, `human_action.deleted`,
  `audit.pruned`, by the system actor `retention`) and recorded in the change feed, so open pages update.
- Files are removed before the database change is recorded, never inside a database transaction.
- Nothing is deleted while central was restored from a backup and the restore is not acknowledged yet: the pass waits
  until an administrator has looked at the restored installation.
- Deleted data comes back only by restoring a backup.

### Audit retention and the hash chain

When audit retention deletes the oldest events, SpaceMan records the sequence and hash of the last deleted event.
**Verify chain** then checks the chain from the first retained event onwards, so tampering is still detected. The
newest event is never deleted. A consumer that reads the audit log forward (`GET /audit?after=<seq>`, see
[Access and audit](/operators/access#audit-log)) and whose position is older than the retained events gets a `gap`
in the response, like the change feed's gap event.

## Recording size

Each run's terminal recording is bounded on the node by the space's `recording_quota_bytes` limit (default 512 MiB,
1 MiB to 64 GiB). The node receives the bound with the launch; when a recording reaches it, the recording ends with a
truncation record and the run is unaffected. Runs already started keep the bound they started with. A node agent older
than this release ignores the setting and keeps 512 MiB.

## Exporting a space

Space admins export a space from the **Export** button on its overview, with the CLI
(`spaceman space export download --space payments --out payments.zip`), or with `GET /spaces/{id}/export`. The archive
is a ZIP file:

| File | Holds |
|---|---|
| `manifest.json` | the format (`spaceman.space-export/v1`), who exported it and when, counts, and every file with its size and SHA-256 |
| `README.txt` | the layout and the warning below |
| `space.json` | the space |
| `inbox_items.jsonl` | inbox items, one JSON object per line |
| `tasks.jsonl`, `task_dependencies.jsonl`, `task_blockers.jsonl` | queue tasks and how they relate |
| `human_actions.jsonl`, `human_comments.jsonl` | human actions with their requests and responses, and their comments |
| `messages.jsonl` | messages between agents, control and operators |
| `attachments.jsonl`, `attachment_refs.jsonl`, `attachments/<id>` | attachment metadata, which item uses which, and the content |

::: warning The archive is not encrypted
Unlike backups, an export is meant to leave SpaceMan, so it is plain JSON and files. It holds the space's content as
SpaceMan received it (bodies and attachments are untrusted content) and no credentials, tokens or sealed secrets. Store
and share it like the data it holds. Central streams it straight to you from one consistent snapshot and keeps no copy.
:::

Recordings and transcripts are not part of an export; download them per run. `GET /spaces/{id}/export/summary` (the
dialog, or `spaceman space export summary`) shows the counts, the estimated uncompressed size and the space's export
limit, `export_max_bytes` (default 2 GiB). An export over the limit is refused before it starts. Every export is
audited (`space.exported`).
