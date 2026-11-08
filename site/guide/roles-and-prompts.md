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

Editing a profile or template creates a new **revision**; it applies to launches from then on.

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
  SpaceMan 0.1.0-rc.10 or later on the node: a run on an older node keeps Claude Code's own choice of renderer. The
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
    and notes from a checkout doesn't raise a dialog. The Claude Code and Codex login files in it are denied.

  Both are added after the profile's own hooks, **Deny** rules and directories, which stay as they are (each entry
  once). A profile that sets `disableAllHooks` also turns SpaceMan's hook off. Like the other defaults, the node adds
  these, and the resolved settings don't show them.

## Space roles

A space configures the roles it uses from these templates on its **Overview**. A space role can override the profile,
the prompt (or its version), the model and settings, and add instructions that apply only in this space. **Effective
configuration** shows what a launch of the role gets now and where each value comes from.

<Shot name="space-role-preview" alt="Effective configuration of the control role: harness from the profile, kind from the template, permission mode from the profile, and the composed system text" caption="A role's effective configuration" />

Launching a run with a configured role needs only the role's name; its template, profile and prompt fill in the rest.

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
| System text | Concatenated in order: SpaceMan's orientation, the role prompt, the template's memory conventions, the space's goals and standing instructions, the space role's instructions, then the launch's text. |

**Policy ceilings** apply last, whatever layer set the value. The central setting `harness.max_permission_mode`
(default `auto`) is the most permissive Claude Code permission mode any run gets; a higher mode is lowered to it, and
the run's configuration shows that the policy decided.

## What each run records

Each run stores a manifest of what it started with: the prompt version and its content, the profile, template and
space-role revisions, every effective value with its origin, the account reference, the versions of SpaceMan, the
node agent, the runner and the harness, and the policy version. Secrets are never part of it. The run page shows it
under **Configuration at launch**.

<Shot name="run-manifest" alt="A run's configuration at launch: role, template, profile and prompt versions with effective values and dependency versions" caption="A run's configuration at launch" />
