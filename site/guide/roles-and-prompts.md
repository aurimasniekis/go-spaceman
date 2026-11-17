# Roles and prompts

What an agent is told and how it runs comes from a few central catalogs, combined per space. Every run records the
exact result in its manifest, so a later change never rewrites what a past run used.

## Starter roles

A new installation comes with **starter roles**, so the first space can launch useful agents before you configure
anything:

| Role | Kind | What it does |
|---|---|---|
| `control` | control | Coordinates the space: triages the inbox, plans tasks, launches workers and reviews their results. |
| `planner` | worker | Turns a goal into small, checkable tasks with acceptance criteria; does not change code. |
| `implementor` | worker | Makes the change a task describes in its own worktree, with tests, ready for review. |
| `reviewer` | worker | Reviews a change for correctness, tests, security and maintainability. |
| `kb` | kb | Answers other agents' questions from the space's knowledge base, citing its files. |
| `dreamer` | dreamer | Consolidates role memory with SpaceMan's built-in dream prompt. |

Each one is an ordinary role template (`starter-<role>`) with a published prompt of the same slug and the
`starter-claude-code` harness profile; a `starter-codex` profile is there too. SpaceMan adds them once, at the first
start. Edit them, or delete what you do not need: a deleted starter is not added back.

A space gets the roles when you ask for it:

- **New space:** keep **Add the starter roles** ticked in the create dialog (`--starter-roles` with `spaceman space
  create`, `starter_roles: true` in the API).
- **Existing space:** **Add starter roles** on the space's Roles card, or `spaceman role add-starters --space <space>`.
  It adds only the roles the space does not have, and never changes the ones it has.

Then launch, for example, `control` from **Launch run** and let it start the other roles. To run them on Codex, set the
templates' profile to `starter-codex`, or give the space role another profile.

### Shared by every space

The starter templates, prompts and profiles belong to the **installation**, not to a space. Editing `starter-implementor`
changes what the implementor role runs with in **every** space that uses it, demo spaces included. A template or profile
that a space role uses can't be deleted until nothing uses it. A space never has its own copy of a starter; it has a
**space role** that points at the shared template.

Changing a template's or a prompt's **name or description** creates no prompt version. A template change creates a new
[revision](#harness-profiles-and-role-templates), and a run picks it up at its next launch. A prompt's name and
description can be changed in place (`spaceman prompt update`, `PATCH /prompts/{prompt_id}`). Its slug and kind stay
fixed: the slug identifies the prompt in configuration, and the kind decides which roles may use it.

### Overriding a starter in one space

To change a role in one space only, override it on that space's role instead of editing the template. The space role
can set:

- its **harness profile**, **model** and **settings**;
- its **prompt** and the **pinned version** (empty follows the latest);
- its **instructions**, appended after the prompt in this space only;
- its **skills**, added to the template's; a pin on the space role wins;
- its **description**, which says what the role is for in this space. Empty uses the template's description. Control
  reads it to choose a role (see [Control and workers](./control#choosing-a-role)).

Set an override on the space's role in the web UI, or with `spaceman role set <name> --space <space>`. The description
is set with `spaceman role set <name> --space <space> --description "…"` or through the API for now; the web form follows
in a later change. Skills are set in the web UI or through the API: `role set` has no skills flag, and because it
replaces the role's whole configuration, running it clears the role's skills. Pass every field you want to keep (read the
role first with `spaceman role get`). The override lasts only in that space, and the run's manifest records each effective
value (except the description, which is not part of the manifest) with where it came from.

### Copying a template for a group of spaces

When a group of spaces needs the same setup that differs from the starter, copy the template rather than overriding each
space role one by one. There is no per-space template layer and no automatic fork:

1. Read the template with `spaceman role template get <template>`.
2. Add a new template with the same kind, profile, prompt and settings, and a new slug:
   `spaceman role template create <new-slug> --kind <kind> --profile <profile-id> --prompt <prompt-id> --name "…" --description "…" --memory-conventions "…" --prompt-pin <version>`.
   `--profile` and `--prompt` take IDs; `--memory-conventions` and `--prompt-pin` are optional.
3. Point each space's role at the copy: `spaceman role set <name> --space <space> --template <template-id>`, using the
   ID the create step returned.

To change the copy's prompt text too, fork the prompt (`spaceman prompt fork`) and point the copy at the fork. Forks never
merge back, so later changes to the original do not reach the copy.

## Prompts

**Prompts** hold the instructions for roles, dreams, knowledge bases and events, in one versioning system.

<Shot name="prompts" alt="Prompt catalog with engineering base, control and reviewer prompts, their kinds and latest versions" caption="The prompt catalog" />

- You edit a **draft**. **Publish** turns it into the next version, which never changes again.
- A prompt can **extend** a published version of another prompt; that prompt's content comes first. A control prompt
  can build on a shared engineering base, for example.
- **Fork** copies a published version into a new prompt that records where it came from. Forks never merge back
  automatically.
- **Diff** compares any two versions line by line.
- **Roll back** publishes an earlier version's content as a new version: history only grows.

<Shot name="prompt-detail" alt="Control prompt with an empty new draft and two published versions with Diff, Roll back and Fork actions" caption="A prompt's draft and versions" />

<Shot name="prompts-diff" alt="Diff between version 1 and 2 of the control prompt, with the added production rule highlighted" caption="Comparing two versions" />

## Harness profiles and role templates

A **harness profile** says how a run executes: the harness (Claude Code or Codex), the model and settings such as
Claude Code's permission mode. A **role template** says what a role is: its run kind (control, worker, …), its prompt
(following the latest version, or pinned to one), its memory conventions and its default profile.

<Shot name="roles" alt="Role templates for control and reviewer with their prompts and profiles" caption="Role templates" />

Editing a profile or template creates a new **revision**; it applies to launches from then on. Changing only a template's
name or description also makes a revision, but no prompt version.

### Claude Code settings

A Claude Code profile also controls its runs' **Claude Code settings**, the keys of Claude Code's `settings.json`.
SpaceMan sets them in each run's own `settings.json` when it launches, and removes keys you take out at the next
launch. The profile form has a section for each kind of setting:

- **Permissions:**
  - the default permission mode;
  - tool rules such as `Bash(make test:*)` or `Read(./secrets/**)`: **Allow** runs without asking, **Ask** always
    asks first, **Deny** is refused;
  - additional directories the run may use.
- **Auto mode rules** are used when the permission mode is `auto`. Each rule is a sentence for Claude Code's
  classifier:
  - **Environment** says what it may trust, such as your Git host, repositories and services;
  - **Allowed actions** are exceptions to the block rules;
  - **Block rules** are more actions to block.

  Your rules are added to Claude Code's defaults. Switch on **Replace Claude Code's default rules** to use only yours.
- **Environment variables** are set for the Claude Code session.
- **Other settings** takes any other key as JSON, such as `hooks`, `statusLine` or `model`.

<Shot name="claude-settings" alt="The Claude Code profile's settings: permission rules, auto mode environment and allowed actions, and the other sections" caption="Claude Code settings on a profile" />

Things to know:

- **Hooks and status lines run commands** on the node as the agent's user. Anyone who can edit profiles controls them.
- **Don't put secrets here.** Settings are stored in clear and recorded in each run's manifest; use
  [harness accounts](/operators/accounts) for credentials.
- **The central ceiling still applies.** A default permission mode above `harness.max_permission_mode` is lowered to
  it, and the profile's `permission_mode` setting, when set, wins over the default mode.
- **Nodes need SpaceMan 0.1.0-rc.9 or later.** On an older node, a run of the profile fails (`node_too_old`) rather
  than starting without its settings.
- **In Terraform** they are the profile's `claude_settings`, written with `jsonencode`.
- **Runs use Claude Code's classic renderer** (`"tui": "default"`) unless the profile sets `tui`, so everything a run
  prints stays in its terminal and watchers can [scroll back](/operators/terminals#scrolling-back) without taking it
  over. To use the full-screen renderer instead, add `"tui": "fullscreen"` under **Other settings**; watchers then see
  only the current screen (ADR-0064). The node adds this default when it writes the run's settings, so it needs
  SpaceMan 0.1.0 or later on the node: a run on an older node keeps Claude Code's own choice of renderer. The
  profile's resolved settings in the API don't show the default; they show only what the profile sets.
- **Agents can't ask in the terminal.** Every run gets `AskUserQuestion` and `EnterPlanMode` in its
  `permissions.deny`, after the profile's own **Deny** rules (each rule once; your other permission settings stay as
  they are). Claude Code then never offers its question picker or plan mode, whose plan ends in an approval dialog: a
  run waiting there would wait for an answer typed into its terminal, which nobody may be watching. Agents ask through
  [human actions](./human-actions#agents-ask-here-not-in-the-terminal) instead. To allow one anyway, add it to the
  profile's **Allow** rules (ADR-0066). Like the renderer default, the node adds this, and the resolved settings don't
  show it.
- **Runs never wait at a permission dialog.** Even in auto mode, Claude Code asks in the terminal in three cases: for
  **Ask** rules, for reads outside the run's working directories, and when its classifier falls back to asking after
  repeated blocks (3 in a row or 20 in total). `skipAutoPermissionPrompt` doesn't change that: it only skips the
  one-time notice shown when auto mode starts. So SpaceMan adds two things to every run's settings (ADR-0068):
  - **A PermissionRequest hook** (`spaceman-agent hook permission-request`, part of the node agent) that answers every
    dialog with "deny" and tells the agent to ask through [human actions](./human-actions#agents-ask-here-not-in-the-terminal)
    instead of retrying. It can only deny, never allow, so an **Ask** rule works like a **Deny** rule: to let runs do
    something, put it under **Allow**.
  - **The run's own space folder** in `additionalDirectories` (only that space's folder), so reading the space's plans
    and notes from a checkout doesn't raise a dialog. The Claude Code and Codex login files in it are denied. Other
    instances' folders, and every instance's settings, instructions (`CLAUDE.md`, rules), memory and transcripts,
    can't be edited. Each instance's Claude Code auto memory lives outside the space folder, in
    `<data_dir>/automemory/<space-id>/<instance>` on the node, so no other run can write what it remembers.

  Both are added after the profile's own hooks, **Deny** rules and directories, which stay as they are (each entry
  once). Like the other defaults, the node adds these, and the resolved settings don't show them. Three more things
  to know:
  - **Key stripping.** At every launch the node removes settings keys that run commands or change hooks, credentials
    or permissions (`hooks`, `env`, `*Helper`, `statusLine`, `disableAllHooks`, …) from the instance's settings
    unless the profile set them. So set such keys in the profile, not by editing an instance's file.
  - **`disableAllHooks`.** A profile that sets it also turns SpaceMan's hook off.
  - **A profile's own PermissionRequest hook.** Claude Code runs it alongside SpaceMan's. Outside auto mode the first
    answer wins, so a profile hook that allows can override SpaceMan's deny. Give it only to profiles whose editors
    you trust with that (profile editors are trusted with run credentials anyway).

### Claude Code version

A claude-code profile can pin the Claude Code version its runs get with **Claude Code version** (`harness_version` in
the API; the Terraform provider attribute follows separately):

- empty: the node's own channel (by default `latest`);
- `latest` or `stable`: that release channel;
- an exact version such as `2.1.322`: exactly that version. A node that manages Claude Code downloads it if needed
  and never runs another version instead. Any other node runs the launch only if its installed version matches.

A pin needs the node agent at protocol 1.11 or later; on an older node the run fails as `node_too_old`. An API update
that leaves `harness_version` out keeps the pin, and `""` clears it. See
[Managed Claude Code](/operators/claude-code#pinning-a-version-per-profile) for how nodes resolve, download and keep
versions.

## Space roles

A space configures the roles it uses from these templates on its **Overview**. A space role can override the profile,
the prompt (or its version), the model and settings, add skills and instructions that apply only in this space, and give
the role its own description. **Effective configuration** shows what a launch of the role gets now and where each value
comes from. The [starter roles](#starter-roles) section explains how this works for the shared starters.

<Shot name="space-role-preview" alt="Effective configuration of the control role: harness from the profile, kind from the template, permission mode from the profile, and the composed system text" caption="A role's effective configuration" />

Launching a run with a configured role needs only the role's name; its template, profile and prompt fill in the rest.

## Rename a role

A space role can be renamed in place, so its configuration, pending work and memory stay with it. Deleting and re-creating
a role under a new name would leave every copy of the old name behind.

```sh
spaceman role rename implementor --to implementor-hard --space payments
spaceman role renames --space payments
```

The API is `POST /spaces/{space_id}/roles/{name}/rename` with `{"new_name": "…"}` (the same permission as saving a role;
an optional `If-Match` with the role's revision), and `GET /spaces/{space_id}/role-renames` lists the renames. Over the
administrator MCP the tools are `rename_space_role` (classified dangerous, like saving a role) and
`list_space_role_renames`. Telegram does not offer role operations.

**What moves.** In one transaction the rename changes the role and everything that still acts on its name: MCP grants,
knowledge-base and dream configuration (including the role lists), schedules, the human policy's successor, human type
permissions, the role's account pool, its memory collection, open tasks, messages to the role that have not been
acknowledged, open knowledge-base requests and pending human notifications. Wildcards (`*`) in role lists are left alone.

**What stays.** Records of what happened keep the name they were written with: runs and their manifests, usage, approvals,
cross-space and knowledge-base history, human actions, inbox items, the audit log, and closed tasks and messages. Only where
SpaceMan starts something again from a history row (the control run's restart and autostart) and for a dream's transcript
filter does a former name resolve to the role that carries it now. A name that is a live role is never redirected, so a new
role may reuse a name that was renamed away.

**The role must be quiet.** A rename is refused with `409 role_busy`, which lists what to stop or finish first, while the
role has a run that is not finished, an open usage-limit wait or account switch, a queued launch, an unfinished dream run
in the space, or while the space is being relocated. A run carries the old name in its token, its memory path and its
prompt, so renaming under it would split its routes and memory. A target name already used by a role or by a memory
collection is `409 name_taken`; memory is never merged. A stale `If-Match` is `409 precondition_failed`, and an invalid or
unchanged name is `422 validation_failed`. A node too old to move the role's memory (one that last connected with an
agent below node protocol 1.14, or never connected) makes the rename fail with `409 node_too_old`, and nothing changes:
upgrade the node's agent and let it connect first.

**Memory moves with the role.** The central copy of the role's memory moves at once (if a backup is bundling memory at
that moment, right after it). The space's node moves its folder when it next receives the rename, and
`spaceman role renames` shows that move as `pending` until the node reports it: `done` once the folder has moved, or
`failed` with the node's reason when it could not move it (the node tries again when its agent next starts).
`not_needed` is a space without a node. If a run of either name, or a dream run, is still active on the node (one
central no longer counted as live), the node waits: the move stays `pending` with that run named as the reason, and the
node moves the folder as soon as the run ends. On the node the role's instance checkouts and preserved `conflicts/`
folders move with the folder, and edits a run left in a checkout are checkpointed first; a folder that was already
under the new name there is set aside under `.retained/` in the space's memory folder, never merged or deleted. While the move is `pending` or `failed`, launches of the role under either name
and dream runs in the space are held, with the node named in the reason, and the space cannot be relocated; a node that
is offline keeps them held until it reconnects. Nothing the node wrote meanwhile is lost: it wrote under the old folder
name, which is the folder that moves, and central refuses that history under the old name (`role renamed`) without
consuming it, so the node offers it again under the new name after its move. See [Memory](memory.md).

**A rename that stays open.** A node that keeps reporting `error`, or that was downgraded below node protocol 1.14 after
the rename, leaves it `pending` or `failed` and the role held. Two admin-only operations end the wait (they need the
`space_role_rename:admin` permission, which only the admin role holds; an operator who may rename a role may not use them):

- **Retry** sends the move to the node again, for when the fault is fixed:
  `spaceman role retry-rename <rename-id> --space payments`, or `POST /spaces/{space_id}/role-renames/{rename_id}/retry`.
  It works on a `pending` or `failed` rename and never ends the hold itself: the hold lifts when the node reports `done`.
  For a node below protocol 1.14 the response says `node_too_old: true`: the command is queued and delivered once the node
  connects at 1.14 or more.
- **Close** gives up the node's move: `spaceman role close-rename <rename-id> --space payments --reason "node retired" --yes`,
  or `POST /spaces/{space_id}/role-renames/{rename_id}/close` with an optional `{"reason": "…"}`. The rename becomes
  `abandoned`, the holds lift at once (launches of both names, dream runs, another rename, a relocation), and central's
  memory stays under the new name. **Nothing is deleted:** the node's old-name folder stays on disk, orphaned, and is never
  synced again (a later role with the old name starts a fresh collection and the node sets the orphan aside under
  `.retained/`). Anything the node wrote there that central never received stays only on the node. If the node has already
  moved its folder by the time it hears of the close, the folder simply ends under the new name, where central's copy is.
  A close does not cancel a move the node has not received yet (it was offline, say): the node still gets it when it
  connects, and until then **another rename of either name is refused** (`409 role_busy`, naming the closed rename), so a
  later rename can never reach the node before the earlier one and strand or hand over the memory.
- **Close with `--forget-node`** (`"forget_node": true`) is the way out for a node that is not coming back: it cancels the
  move the node has not received and releases both names at once. It works on an open rename (closing it) and on a closed
  one the node has not received yet. If the node does come back, it is told the rename is closed before anything newer:
  it keeps the old-name folder as an orphan, never moves it, never syncs it, and sets it aside under `.retained/` when the
  name is used again, so no later role or rename of that name inherits it. Audited as `role_rename.forgotten`.

A rename that is not `pending` or `failed` is `409 rename_not_open` for both. Both are audited as `role_rename.retried` and
`role_rename.closed` with the actor, and are in the administrator MCP (`retry_space_role_rename` and
`close_space_role_rename`, classified dangerous). Telegram does not offer them.

Role names also appear in other configuration you manage outside SpaceMan, for example in Terraform. Update those
references to the new name.

## How values are combined

Layers apply in this order, each overriding the previous one:

1. SpaceMan's built-in defaults;
2. the harness profile;
3. the role template;
4. the space role;
5. the launch itself (a model, settings or extra instructions given when launching).

Field by field:

| Field | Rule |
|---|---|
| Run kind | From the role template. A launch cannot change it. |
| Harness | From the profile, otherwise from the launch. A launch cannot contradict the profile. |
| Model | The last layer that sets one wins. |
| Settings | Merged key by key; the last layer that sets a key wins. |
| Claude Code version | From the profile only (empty: the node's channel). |
| System text | Concatenated in order: SpaceMan's orientation, the role prompt, the template's memory conventions, the space's goals and standing instructions, the space role's instructions, then the launch's text. |
| Description | The space role's own, otherwise the template's. Not part of the system text and not in the run manifest: it is guidance for whoever chooses a role, shown to control. |

**Policy ceilings** apply last, whatever layer set the value. The central setting `harness.max_permission_mode`
(default `auto`) is the most permissive Claude Code permission mode any run gets; a higher mode is lowered to it, and
the run's configuration shows that the policy decided.

## What each run records

Each run stores a manifest of what it started with: the prompt version and its content, the profile, template and
space-role revisions, every effective value with its origin, the account reference, the versions of SpaceMan, the
node agent, the runner and the harness, and the policy version. Secrets are never part of it. The run page shows it
under **Configuration at launch**. From node protocol 1.11 on, the run also records the Claude Code version it actually
launched (**Version** on the run page; `harness_version`, `harness_requested` and `harness_fallback` in the API).

<Shot name="run-manifest" alt="A run's configuration at launch: role, template, profile and prompt versions with effective values and dependency versions" caption="A run's configuration at launch" />
