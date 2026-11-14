# SpaceMan web UI: UX review

Reviewed: all 86 `-dark.png` shots and 15 `-light.png` shots (landing-overview, space-overview, queue,
terminal-live, run-manifest, human-space, human-global, human-form-default, inbox-list, space-limits, attention,
events-subscription, setup-node-enroll, space-export, sign-in-sso).
Source read where structure mattered: `routes/spaces.$spaceId.runs.$runId.tsx`, `features/spaces/SpaceNav.tsx`,
`components/StatusBadge.tsx`, `components/PageHeader.tsx`, `theme/theme.ts`, `routes/spaces.$spaceId.repositories.tsx`,
and the `FleetRun` / `QueueTask` schemas in `api/schema.gen.ts`.

Priorities: **P1** confusing or broken, **P2** clearly unprofessional or inconsistent, **P3** polish.
Effort: **S** under a day, **M** a few days, **L** a week or more.

Shot names are written without the theme suffix. "(light)" means the `-light.png` shot.

---

## 1. Summary: the five most important problems

1. **The run page is a debug dump, not a place to watch an agent** (terminal-live, terminal-replay, run-usage,
   run-manifest). The identity line is a 15px role name plus two badges. The facts an operator wants (task, turn,
   duration, tokens/cost, node, account) are missing or sit below the fold in nine equal-weight cards of IDs
   (Instance, Session, Lease generation, `user:usr_…`). "Exit code" sits alone on its own row. The controls are
   spread over two rows ("Take over terminal" sits alone under Live/Replay and Stop).
2. **You can't watch your agents.** No view shows a space's terminals together, and no global view either. The
   only way into a terminal is a ULID link (`run_01m6pqe1yhea…`) in the Runs table, and that table sits *below the
   fold* on the space overview, under Accounts, Roles and Usage (space-overview). For a product whose core is
   "persistent teams of agents in terminals", this is the biggest gap in the information architecture.
3. **The dark theme (the default) loses status colour.** Mantine's `light` variant on Night Owl gives near-white
   text on a background tinted about 10%. READY, BACKLOG, WAITING, OPEN, PENDING, LIVE and REPLAY all read as the
   same grey chip (queue, human-global, inbox-list, landing-overview). Compare queue (light), where READY is blue,
   WAITING amber and RUNNING green. The same flattening turns warning alerts grey: "The archive is not encrypted"
   (space-export), "Non-durable subscription" (events-subscribe-core), "This cannot be undone" (space-purge). One
   fix in `theme/theme.ts` repairs all of them.
4. **Wayfinding breaks as soon as you leave a list.** Detail pages (run, inbox item, subscription) render outside
   the space shell: the space tabs disappear, and the breadcrumbs differ between pages ("Spaces / Payments
   platform / run_01m6…" vs "Spaces / Payments platform · Inbox"). The space header's subtitle and action buttons
   change on every tab, so the header jumps (space-repositories: the action buttons stack and the tabs move down
   18px). The sidebar is a flat list of 21 items that mixes daily work with admin. "Attention" and "Needs you"
   look like the same thing.
5. **Noise hides the signal: raw IDs, all-caps badges and paragraphs of helper text.** Names exist but IDs are
   shown instead: `by usr_01m6pqdk98…` (human-rerouted, approval-executed), `account acc_01m6pqe1d6…`
   (run-manifest), `extends prv_01m6…` (prompt-detail), every actor and target in audit-log. Plain attributes
   become shouting badges: space names (PAYMENTS PLATFORM), whole task titles (SKIPPED: MAKE THE SETTLEMENT RETRY
   BUDGET TIME-BASED), filenames (REFUNDS-EXPORT.CSV), and OPEN on every row of a list that only holds open items.
   Many pages open with one or two sentences about implementation ("One timer drives them; no agent waits or
   polls", "Payloads carry identifiers and states only"). The result is dense in the wrong way: hard to scan,
   and it does not feel calm.

What works and should stay:
- The **eligibility reasons** in the queue drawer ("Not eligible now · waiting for … · 1 top-level task already
  running").
- The **untrusted-content** labelling (inbox-item, approval-enforced, xspace-requests).
- The **advisory vs enforced** approval explanation.
- The **Attention** page's per-subsystem cards.
- The **memory-files** layout (file list plus rendered preview).
- The repository **node report** with unpushed commits highlighted.
- The **typed-slug purge** confirmation.
- The **Inbox status filter with counts**.
- Event **deliveries with origin** (live/replay).
- The **sign-in and setup** screens: clean, with good focus order.
- The **light theme**: its status colours are already right.

---

## 2. Cross-cutting issues

### C1. Night Owl flattens `light`-variant badges and alerts (P1, S)
Shots: queue vs queue (light), human-global vs human-global (light), human-space vs human-space (light), inbox-list
vs inbox-list (light), landing-overview (PENDING vs CONNECTED), space-export vs space-export (light),
events-subscribe-core, events-core-subscription, space-purge, sign-in-providers ("Not ready for SSO only"), event-replay
(LIVE vs REPLAY), space-limits (THIS SPACE vs INSTALLATION).

`Badge` defaults to `variant: 'light'`, and the theme only overrides `filled` (`filledText` in `theme/theme.ts`).
In the dark scheme, Mantine resolves the light variant's text to a low index of the colour tuple. Indexes 0–2 of
`owlYellow`, `owlGreen`, `owlBlue` and the others are near-white tints, and the background tint barely shows on
`#011627` / `#0b2942`. So hue carries almost no meaning in dark mode. In light mode the same badges are clearly
blue, amber, green and red.

Fix: extend the `variantColorsResolver`. For `variant === 'light'` in the dark scheme, return
`background: rgba(<shade 5>, 0.18)`, `color: <shade 4 or 5>` (`#82aaff`, `#addb67`, `#ecc48d`, `#f2726f`,
`#c792ea`) and `hover: rgba(<shade 5>, 0.26)`. The resolver has no scheme input, so use CSS variables: emit
`color: var(--sm-light-<color>)` and define the variables per scheme in `cssVariablesResolver`. Add these pairs to
`contrast.test.ts` (text ≥ 4.5:1 on the card colour `#0b2942`). `Alert` uses the same variant, so the yellow and
red alerts get their colour back at the same time.

### C2. Badges are used for everything, in uppercase (P2, S)
Shots: queue ("SKIPPED: MAKE THE SETTLEMENT RETRY BUDGET TIME-BASED"), human-global (PAYMENTS PLATFORM on every row;
OPEN on every row), inbox-composer (REFUNDS-EXPORT.CSV · 39 B, which changes the case of a filename), space-overview
(TEAM-CLAUDE · CLAUDE CODE · DEFAULT), skill-detail / human-types (V1 · 11/5/2026, 8:47:10 PM), setup-node-enroll
(HERDR-CORE 0.1.17), xspace-access.

A badge should mean "state" or "category". Here it also carries names, titles, versions and files, so real
states stop standing out.
- Set `Badge` `styles: { root: { textTransform: 'none' } }` (or `tt="none"` in defaultProps) and `fw: 600`.
  Sentence case reads calmer and still scans.
- Show these as plain text, `Anchor` or `Code` instead of badges: space names, task titles, account names,
  harness names, versions, filenames, runner versions.
- Hide the status column when a filter makes it constant (OPEN in "Needs you", pending lists).

### C3. Raw identifiers where a name exists (P2, M)
Shots:
- run-manifest / run-usage: `user:usr_01m6pqdk98…`, `account acc_01m6pqe1d6…`.
- human-rerouted and approval-executed: `by usr_01m6…`.
- approval-enforced: `Resource: mcp_server:mcp_01m6pqe1kpec…`, `Enforced approval apv_01m6…`.
- prompt-detail: author `usr_…`, `extends prv_…`.
- inbox-item: `tsk_01m6…`.
- xspace-requests: `inbox item inb_01m6…`.
- messages: recipients `run_01m6pqe1q3ery…`.
- kb-sessions: `ins_…`.
- audit-log: every actor and target.
- ops-backups: `bkp_…`, `usr_…`.
- space-overview: run IDs as the primary column.

Fix: add one `<EntityRef kind="user|run|account|task|inbox|prompt|mcp" id>` component. It resolves the name from the
TanStack cache (users, accounts, the space's runs and tasks), renders it as a link, and shows the ID in a
`Tooltip` with a Mantine `CopyButton`. For runs, show `role · #turn` (for example "control #4"). Put IDs in a
collapsed "Identifiers" section wherever they're still needed. In audit-log keep the ID as a dimmed second line
under the name.

### C4. Time and duration formatting (P2, S)
- Drawers use absolute times with seconds: "next reminder 11/6/2026, 12:51:45 AM · escalates 11/6/2026, 8:51:45 PM"
  (human-clarify, human-form-default).
- Durations are printed in Go format: "Uses: 1, within 1h0m0s" (approval-enforced).
- "168 h" (events-publishing).
- Inputs ask for raw minutes: "First after (min) 240 · Then every (min) 1440" (human-policy-inbox), "Expires after
  (minutes) 1440" (human-types), "Duration (minutes)" (human-form-custom).

Fix: add one `<Time value mode="relative|absolute">` component. Relative by default ("in 4 h", "3 m ago"), with the
absolute time, without seconds, in a tooltip. Use absolute only in audit and delivery tables. Add a
`formatDuration` helper ("1 h", "7 days"). For inputs, add a `DurationInput` (a NumberInput plus a unit Select, or
a text field that parses "4h", "1d") that stores minutes.

### C5. Harness, kind and plural wording isn't consistent (P2, S)
- The harness appears as `claude-code` (landing-overview, space-overview), CLAUDE-CODE (terminal-live, nodes),
  CLAUDE CODE (accounts) and "Claude Code" (roles).
- Strings use "(s)": "1 top-level task(s)", "4 run(s)", "1 COMMIT(S) TO SYNC", "2 instance(s)", "at most 1 time(s)".

Fix: add a `harnessLabel()` / `kindLabel()` map used everywhere (Claude Code, Codex; Control, Worker, Knowledge
base, Dreamer, Diagnostic), and a `plural(n, 'task')` helper built on `Intl.PluralRules`.

### C6. The space shell, headers and breadcrumbs (P1, M)
Shots: space-overview, queue, space-repositories, inbox-item, events-subscription, terminal-live.

- `routes/spaces.$spaceId.tsx` is a bare `Outlet`. Every section page re-renders the space name as its H1, with a
  *section* description as the subtitle and *section* actions on the right. The space's identity (state, node,
  slug) only appears on Overview.
- When the description wraps, the action buttons stack and the tabs move (space-repositories).
- Detail pages drop `SpaceNav` and hand-roll breadcrumbs in different styles: "Spaces / Payments platform /
  run_01m6…", and "Spaces / Payments platform · Inbox", where the "·" joins two levels into one link.

Fix: make `spaces.$spaceId.tsx` a real layout.
- **Space header** (always shown): name, state badge, `node build-01 · connected` as dimmed text, and the
  space-wide actions (Launch run plus a ⋯ menu).
- **SpaceNav**.
- **Section toolbar row**: section title `Title order={4}`, an optional one-line description, and section
  actions on the right with `wrap="nowrap"`.
- Detail pages render inside the layout with a short breadcrumb under the tabs ("Inbox / Settlement retries fail
  on weekends"), and their tab stays highlighted.

### C7. Sidebar information architecture (P2, M)
Shots: every page with the app shell.

- Twenty-one items in five groups, always expanded. Configuration, Integrations and Access hold 15 items, which
  most operators rarely use, while the work entries are spread out.
- "Attention" (system health plus per-space counts) and "Needs you" (human actions) have overlapping names.
- Spaces aren't listed in the sidebar, so switching spaces takes two clicks via the Spaces list.

Fix:
- **Work**: Overview, Needs you (badge), Terminals (new, see §4), Spaces with the user's spaces as nested
  `NavLink`s, each with a state dot and a needs-you count.
- Rename **Attention** to **Health** (it is mostly outbound events, backups, storage and nodes), or merge it into
  Overview as a "Health" card.
- Collapse **Configuration, Integrations and Access** into collapsible `NavLink` groups (`childrenOffset`,
  open state in `localStorage`), and hide what the user's role can't change.
- Keep "Collapse" at the bottom, but let the terminal views use a focus mode (`AppShell` `navbar.collapsed`).

### C8. Too much helper text (P2, M, mostly writing)
Examples:
- Page subtitles: "Recurring work: inbox items or tasks at cron or interval occurrences in a time zone. One timer
  drives them; no agent waits or polls." (space-schedules); "…SpaceMan never merges, pushes, resets or deletes."
  (space-repositories); "…through a transactional outbox. Payloads carry identifiers and states only."
  (events-publishing).
- Modal intros: "Without a permission, viewers see a type, operators ask and respond, and any agent may ask. A
  permission raises those minimums (it never widens the roles' own permissions; approvals still need
  approval:decide)…" (human-permissions).
- Permanent alerts: budgets "Not a hard currency limit" (three lines, space-limits).
- Whole prose columns: "When lowered" (space-limits); "Included / Not included" (two cards of bullet points,
  ops-backups).
- Field helpers written as syntax: "{"order": […], "fields": {"name": {"label", "help", "widget",
  "placeholder"}}}; nothing else is accepted" (human-types).

They are accurate and honest, but they are written for the implementer. Rules:
- A subtitle states the page's purpose in at most about 80 characters and in the user's terms ("Recurring tasks
  and inbox items for this space").
- Semantics go behind an `IconInfoCircle` `HoverCard` or a "How this works" docs link.
- Per-row explanations move into a row expansion or a tooltip.
- Field descriptions stay under one line. Anything longer goes into a `Tooltip` on an info icon next to the
  label.

Also raise the type scale: `fontSizes` is xs 11 / sm 12 / md 13, so field descriptions and table hints render at
about 10–11px in dimmed grey. Use sm 13 / xs 12 and never put text below 11px (WCAG readability).

### C9. Destructive and secondary actions look different everywhere (P2, S)
Shots:
- Delete: red-outlined in light, dark grey in dark (events-subscription).
- Revoke: grey filled (auth-tokens).
- Remove: plain text (xspace-access, space-repository-detail).
- Discard: grey (events-quarantine).
- "Withdraw this action…": red link (human-clarify).
- Delete: plain text next to "Edit…" (mcp-server-detail).
- Trash icon only (approval-policies, mcp-server-detail grants).
- The × on a chip deletes an override (human-policies).
- Dialogs close with "Close" (space-export, space-relocate) or "Cancel" (most others).

Fix:
- In rows, destructive actions are `color="owlRed" variant="subtle"` with an icon and a label, or sit in a ⋯ menu
  as a red `Menu.Item`.
- In confirm dialogs, the destructive button is `filled` red.
- Every destructive action asks for confirmation (`modals.openConfirmModal`).
- Dialog footers are always "Cancel" plus a verb ("Download archive", "Relocate").

### C10. Disabled buttons that look like ghosts (P2, S)
Shots:
- "Save draft / Publish" in prompt-detail: almost invisible.
- "Send answer" (human-clarify), "Comment / Ask the requester" (several drawers), "Reassign" (queue-task).
- "Claim / Acknowledge messages", always shown disabled (human-global, human-space).
- "Run precheck / Relocate" (space-relocate).
- "Save" at the bottom of sign-in-providers' settings card.

Fix:
- Keep primary buttons enabled and validate on click (inline error, focus the field).
- Where a button must stay disabled, wrap it in a `Tooltip` that says why.
- Show bulk actions only once something is selected: a sticky `Paper` bar with "2 selected · Claim · Acknowledge".

### C11. Modal mechanics (P2, M)
- Long modals scroll their footer out of view (events-subscribe-custom, sign-in-provider-form).
- A modal opens on top of a modal (human-policy-inbox over human-policies).
- Modal titles are body-weight 13px ("Queue policy", "Launch a run in Payments platform") and look like plain text.

Fix:
- Theme `Modal`: `styles.title: { fontWeight: 600, fontSize: 'var(--mantine-font-size-lg)' }`.
- Long forms use `scrollAreaComponent={ScrollArea.Autosize}` and render the footer outside the scroll area (a
  sticky `Group` with a top border), or move to a `Drawer size="xl"`.
- Policies become a page or a drawer with inline edit, so there's never a modal on a modal.

### C12. Clickable rows that don't look clickable, and monospace in the wrong places (P3, S)
Shots:
- Rows open drawers, but their names are plain text: roles, skills, space-memory, space-schedules,
  space-knowledge, human-types.
- On the prompts page, names are blue links. Pick one rule: the primary column is always an `Anchor`, and the
  whole row has `cursor: pointer` and a hover state.
- Monospace is used for prose values ("off (manual only)", "newest 7", "Kept" in ops-backups; counters in
  events-publishing).
- Monospace leaks into form labels in space-repository-form: "Folder", "Clone URL", "Default branch" and "Known
  hosts" are mono while "Name", "Clone" and "Role" are sans. The cause is `className="mono"` on the
  `TextInput`/`Textarea` root in `routes/spaces.$spaceId.repositories.tsx` lines 350–376, which styles the label and
  the description too. The same pattern appears in `routes/spaces.$spaceId.schedules.tsx` and
  `features/oidc/UserIdentitiesModal.tsx`. Use `classNames={{ input: 'mono' }}`.

### C13. Target sizes and focus (P3, S)
- Queue row arrows, trash icons and the kb-sessions "run" link are 16–22px. `ActionIcon` defaults to `sm`
  (22px), below the WCAG 2.5.8 minimum of 24px. Set `size: 'md'` (28px) for row actions.
- Several drawers open with a focus ring on the close button (human-clarify, human-types, skill-detail,
  kb-detail, xspace-requests). It looks like a stray highlight. Put `data-autofocus` on the first meaningful field
  or on the title.

---

## 3. Per-page findings

### 3.1 Global

| Shot | Problem | Fix | P | E |
|---|---|---|---|---|
| landing-overview | "Active runs 5 · 0 working" reads as "nothing works". `status` is free text and is empty for these runs. | Break down by `status_kind`: "5 running · 3 working · 2 idle", or drop the hint when the status is unknown. | P2 | S |
| landing-overview | The Active runs table has no headers, an unlabelled "—" column (agent status) and "15s ago" with no meaning. Role names link to the run but the space doesn't. | Add headers (Agent, Space, State, Activity, Started), a status dot instead of the RUNNING badge, a space link, and a "Watch" icon that opens the Terminals view (§4). | P2 | S |
| landing-overview | Spaces come after runs and carry no numbers. The right column is empty under Nodes. | Lead with a Spaces table (state, running agents, needs-you count, budget %, node), then Agents, then compact Nodes in the right column. | P2 | M |
| landing-overview | The Needs attention card shows the link text "human actions, tasks, runs, storage…", cut off. | Render the top three counts as separate links ("6 human actions · 1 quarantined event"). | P3 | S |
| attention | Backups: "No backup has run yet" is marked **OK** in green. | Make it a yellow "Never backed up" card with a "Back up now" button. | P1 | S |
| attention | The "Blocked or failed tasks" header wraps over two lines. "Items" is vague. | Use "Blocked tasks" and "Total". | P3 | S |
| attention | "Refresh" plus "checked now" on a live app. | Keep it, but as a subtle `ActionIcon` next to "checked now". | P3 | S |
| human-global | Seven filters plus Sort wrap to two rows, and Sort sits alone on the second. | Show Search, Space, Type and Status. Put Assignee, Priority, Due and Requester in a "More filters" `Popover` with a count badge. Make Sort a right-aligned `Menu` button ("Sort: Newest"). | P2 | M |
| human-global | Every row ends in OPEN, and space names are uppercase outlined badges, the loudest thing on the row. | Hide the status column when only open items are listed. Show the space as dimmed text. | P2 | S |
| human-global | Claim and Acknowledge messages are always shown, always disabled. | Show them in a selection bar (C10). | P3 | S |
| human-global (light) | APPROVAL is red, which reads as an error or failure. | Give approval amber or purple, and keep red for errors and urgent items. | P2 | S |
| account-notifications | Four sections, four widths: Sessions full width, Identities and Notifications two-thirds, Password half. Looks unfinished. | Use one column, `Container size={760}`, or a GitHub-style settings layout with a section list on the left. | P2 | S |
| account-notifications | The session device is a raw user agent ("Mozilla/5.0 (Windows NT 10.0; Win64; x64)…"). | Show "Chrome on Windows", with the full user agent in a tooltip. | P3 | S |
| sign-in-sso, sign-in-sso (light), setup-bootstrap | Clean and well ordered. The helper "Printed once by \`spaceman admin bootstrap\`" shows literal backticks. | Render the command with `<Code>`. | P3 | S |
| setup-mfa | The manual key wraps, leaving a lone "W" on the second line. No copy button. "Code" is a plain input. | Group the key in blocks of 4 with a `CopyButton`. Use `PinInput length={6} type="number" oneTimeCode` and submit automatically on the sixth digit. | P3 | S |
| sign-in-signed-out | Two stacked alerts (blue "You are signed out" and a grey box). | Merge them into one alert, "You're signed out of SpaceMan. You're still signed in at Porch", with the two buttons. | P3 | S |

### 3.2 Space

| Shot | Problem | Fix | P | E |
|---|---|---|---|---|
| space-overview | The header has six buttons: Edit, Export, Relocate, Pause, Launch run, ⋯. | Keep **Launch run** (primary) and **Pause ▾**. Move Edit, Export, Relocate and Archive into the ⋯ menu. | P2 | S |
| space-overview | The Runs table, the space's most important content, is below the fold, after Accounts, Roles and Usage. | Reorder: a status strip (Node · Budget · Needs attention), then an **Agents** card, then Roles, Accounts, Usage. The Agents card shows one row per live agent (dot, role, kind, #turn, activity, started) with "Open terminals". The Runs history table moves to the bottom or into its own tab. | P1 | M |
| space-overview | The run column links ULIDs. Role is secondary. Agent status is "—" for every row. | Make the role and #turn the link, with the ID in a tooltip. Show `status`/`status_kind` as dot plus text, or hide the column while it's empty. | P2 | S |
| space-overview | "Execution lease · VALID · gen 1 · expires in 9s" ticks down every few seconds: internal jargon, and it looks alarming. | Fold it into the Node card ("build-01 · connected · lease ok") with details in a tooltip. Show a lease card only when the lease is invalid. | P2 | S |
| space-overview | Accounts show as three uppercase tuple badges. | Use a small two-line list: "Claude Code: team-claude (default), ci-anthropic · Codex: team-codex (default)". | P3 | S |
| space-overview, space-usage | Usage leads with output tokens. It shows no cost and no budget context. The note strip is long. | Show "Est. cost · tokens in/out · cache", a budget `Progress` linked to Limits, and the note as "4 runs reported no usage (i)". | P3 | S |
| space-overview | The Roles card puts "Effective configuration · Edit" about 700px from the role names. | Use a compact table (Role, Template, Harness/profile, Prompt v, MCP), with row actions in a ⋯ `Menu`. | P3 | S |
| space-create | "When the node loses central" is jargon. | Rename it to "If the node disconnects", with the options "Interrupt runs (resumable)" and "Stop runs". | P3 | S |
| space-purge | A good typed confirmation, but the danger box is grey in dark (C1). | Covered by C1. Keep the pattern. | P3 | – |
| space-export | Good. "NOT encrypted" is in caps inside an alert that already says so. "Close" instead of "Cancel". | Write "not encrypted". Use "Cancel" (C9). | P3 | S |
| space-relocate | Clear, and it names the unavailable nodes. "Keep for (days, 0 until released)" packs the rule into the label. | Label "Keep source folder for", with a "Until I release it" checkbox that disables the number. | P3 | S |
| space-accounts | The Default radio shows even for accounts that aren't allowed, and for the disabled old-claude. | Disable the radio unless the account is allowed. Hide disabled accounts behind "Show disabled". | P3 | S |
| queue | "Next eligible: nothing" is followed by three uppercase badges, each holding a whole task title. | One line: "Nothing can start now: limit 1 running task (1 running) · 3 ready tasks wait on dependencies or blockers". The per-task reasons go in a `Popover`. | P2 | S |
| queue | Rows read **READY** and **WAITING** at the same time. That's a contradiction for a new operator. | Derive one display state: "Waiting on #1" (amber), "Blocked: 1" (red), "Ready" (blue), "Running" (green). Show the dependency as "after #1" text. | P1 | S |
| queue | The running task doesn't show who is running it, for how long, or a link to the terminal. The data exists (`assignee_run`). | On the right of the row: "implementor-backend #3 · 12 m", with an icon button that opens its terminal. | P1 | S |
| queue | The hand icon on row 2 (`IconHandStop`, "Placed by an operator") has a tooltip, but nothing visible says what it means. A stop hand also reads as "blocked" or "halted". | Use `IconPin` with a visible "pinned" label, or show "placed by operator" as dimmed text. | P3 | S |
| queue | Drag handle plus arrows, and the arrows are 16px. | Show the arrows on row hover or focus as `ActionIcon size="md"`. Keep the Alt+↑/↓ hint. | P3 | S |
| queue-preempted | The next-eligible row has a green outline that nothing explains. | Add a "Next" label at the left of that row. | P3 | S |
| queue-task, queue-reassign | "Change state" and "Escalate" sit *above* the "Reason" field they use. The field floats on its own. | Ask for the reason inside the action: a `Menu` → state → small modal with a required reason. Remove the floating field. | P2 | M |
| queue-task | The drawer leaves out objective/acceptance (only shown when preempted), the source inbox item, the assigned run and the history. | Sections in order: Objective, Acceptance, Assignee (link to the run), Depends on, Blockers, Source (inbox item link), Activity. | P2 | M |
| queue-reassign | "Run: No run" next to a disabled Reassign. | Show the current assignee as text with "Change…", which reveals the form. | P3 | S |
| queue-preempted | "Preempted" is stated three times (badge, reason line, alert). The empty "Depends on" box has no placeholder. | Keep the alert only. Add the placeholder "Add a dependency…". | P3 | S |
| queue-policy | Trusted triggers are typed as syntax (`subscription:<id>`, `template:<id or slug>`). The "Incidents" divider appears after two incident-related controls. | Use a `MultiSelect` with grouped, named options (Roles / Subscriptions / Templates). Split into sections: General (concurrency), then Incidents (everything else). | P2 | M |
| inbox-list | Some titles are bold and some regular, without explanation. The long GitHub titles are bold. | Bold only for New, and say so (the New filter). Truncate titles to one line with a tooltip. | P3 | S |
| inbox-list, queue, human-global | Priority looks different on each page: URGENT filled red, HIGH grey filled (inbox) or outlined (queue, human). | One `<Priority>` component with an icon plus text: urgent red, high amber, normal hidden, low grey. | P2 | S |
| inbox-composer | The attachment chip uppercases the filename (C2). The inserted link has no space before it ("does not.refunds-export.csv"). | Keep the filename's case. Insert a space or a new line before the link. | P2 | S |
| inbox-item | The breadcrumb "Spaces / Payments platform · Inbox" is one link spanning two levels. "Associated with task tsk_01m6…" is a raw ID. | Use the C6 layout. Write "Became task: Make the settlement retry budget time-based →" (opens the task drawer). | P2 | S |
| human-space | Resolved rows have no checkbox, so their badges shift left and the columns misalign. The requester column truncates ("control (control) · run_01…", "implementor-backend (w…"). | Use fixed columns (CSS grid or `Table`) with an empty checkbox cell. Requester: "control" plus a run-link icon. | P2 | S |
| human-search | The Sort select shows "Priority", which looks like the Priority filter. "Clear" sits on its own row. | Make Sort a "Sort: Priority ▾" `Menu` button. Put "Clear filters" inline at the end of the filter row. | P2 | S |
| human-space | Policies, Types and Permissions are three equal header buttons next to New action. | Group them in a "Settings ▾" menu (gear icon). | P3 | S |
| human-clarify, human-form-default | Reminder and escalation times are absolute, with seconds. | "Reminder in 4 h · escalates in 1 d" (C4). | P2 | S |
| human-clarify, human-form-default, human-rerouted | Two full TipTap editors with complete toolbars (answer and comment) in one drawer. | Collapse the comment box to a single-line input that expands on focus, with the toolbar shown only on focus. | P2 | M |
| human-form-default | "Deny" is a ghost text button next to a filled green Approve. It is silently disabled until a note is typed. | Make Deny `color="owlRed" variant="light"` and always enabled. Clicking it with an empty note focuses the note and shows "Add a note to deny". | P2 | S |
| human-form-custom | The outcome buttons are raw slugs ("scheduled", "declined"), and the first is filled because it comes first. | Use labels from the type definition, and let a type mark each outcome as positive, negative or neutral for its colour. | P2 | S |
| human-form-custom | `format: date-time` renders a native browser input ("mm/dd/yyyy, --:-- --"). "Duration (minutes)" is a plain number. | Use `DateTimePicker` from `@mantine/dates` with the time zone shown, and `DurationInput` (C4). | P2 | M |
| approval-enforced, approval-executed | "1h0m0s", the raw `mcp_server:mcp_01…`, "Requested by run run_01…", and an "Enforced approval apv_…" line that tells the user nothing. | "Ticket tracker · ticket_comment", "control #4 →", "within 1 h". Move digest and IDs into a "Technical details" `Accordion`. | P2 | S |
| human-rerouted, approval-executed | "Response: answered by usr_01m6pqdk98…". | Show the display name (C3). | P2 | S |
| human-rerouted | "Requester notified (resolved) · PENDING · REROUTED": resolved and pending on one line. | "Delivery: waiting for control. The requester's run ended 4 m ago, so the answer was routed to control." | P2 | S |
| human-policies, human-policy-inbox | The policy edit modal opens on top of the policies modal. | Make Policies a section page or drawer with inline row edit (C11). | P2 | M |
| human-policy-inbox | Minute fields (240, 1440, 60). "Assign to: A user ID usr_…" and "Successor role" are free text. | `DurationInput`, a user `Select`, a role `Select`. | P2 | S |
| human-policies | The × on the override chips ("APPROVAL · V1 ×") deletes an override without asking. | Add a per-row "Reset to default" button with confirmation, as human-permissions already does. | P2 | S |
| human-permissions | The intro paragraph is dense (C8). DEFAULTS is outlined and SET filled, with no clear meaning. | Rewrite: "By default viewers see, operators ask and respond, and any agent may ask. Raise the minimum per type." Show "Default" / "Custom" as text, with Reset. | P3 | S |
| messages | Nineteen of 22 rows are "NOTIFICATION · SpaceMan → control". The operator's messages get lost. | Add a `SegmentedControl` "People & agents / System / All", defaulting to People & agents. | P1 | S |
| messages | The delivery state (stored, delivered, acknowledged) only shows in the drawer. Recipients are run IDs. | Add a state column with an icon. Show recipients as role names (C3). | P2 | S |
| space-repositories | The header actions stack (Sync now above Add repository) because the subtitle wraps. The tabs move down. | C6 layout, `wrap="nowrap"`, and a shorter subtitle. | P2 | S |
| space-repository-form | Mono and sans labels mixed (C12 bug). | Use `classNames={{ input: 'mono' }}`. | P2 | S |
| space-repositories | The "Last fetch" header wraps. "0 changed · 0 untracked" wraps. | Show "clean" when both are 0, counts in amber when not. Shorten the header to "Fetched". | P3 | S |
| space-repository-detail | "Generate SSH key" (filled primary) competes with the footer's Edit. | Give it the `default` variant. | P3 | S |
| space-knowledge | One half-width card. The meaning of "REVIEWED PROPOSALS" is unclear. | Use a full-width table of knowledge bases. Write "Proposals need review". | P3 | S |
| kb-sessions | A tiny underlined "run" link under the ACTIVE badge. Instance IDs as the second line. | Link the requester name to the run. Put the instance ID in a tooltip. | P3 | S |
| kb-detail | "Evidence: README.md@787e466e" is plain text. | Link it to the file at that commit, or add a copy button. | P3 | S |
| space-memory, dream-runs, memory-files | Good. Rows don't look clickable (C12). "1 COMMIT(S) TO SYNC" (C5). | C12, C5. | P3 | S |
| events-subscription, events-quarantine, events-core-subscription, event-replay | The breadcrumb joins "Payments platform · Events". "PULSE GRANT TO ADD" doesn't say whether the grant already exists. Core subscriptions show a disabled "Cursor" button. | C6 layout. Label it "Pulse grant this subscription needs", plus a status if one can be detected. Hide Cursor for core. | P2 | S |
| events-quarantine | The raw error "jsonschema validation failed with 'urn:spaceman:schema#' - at '': missing property 'severity'". Discard is grey and asks for no confirmation. | Headline: "Missing required field: severity", with the raw text in a details block. Discard in red subtle, with confirmation. | P3 | S |
| events-subscribe, events-subscribe-paused | Variable inputs are labelled with raw keys ("account_key", "repository") and two-line helpers in tiny type. | Label from the variable's description ("GitHub account key"), with the key as a mono hint on the right. | P3 | S |
| events-subscribe-custom | The modal scrolls, and Subscribe scrolls off-screen. | Sticky footer, or a large drawer (C11). | P2 | M |
| events-subscribe-core | The data-loss warning is a grey alert (C1). | Make it `color="owlYellow"`. | P2 | S |
| space-limits, space-limits (light) | Budget numbers have no visual ("4,746 of 5,000,000"). | `Progress` bars with a percentage. The 80% warning shows as a marker. | P2 | S |
| space-limits | A permanent three-line "Not a hard currency limit" alert. | One line plus "How budgets work (i)" in a `HoverCard`. | P2 | S |
| space-limits | The "Enforced at" and "When lowered" prose makes each row 60–70px. In dark, overrides (THIS SPACE) don't stand out from INSTALLATION. | Show Limit, Value, Source and Default, with the rest in a row expansion. Overridden values in blue. Show the default only when it differs. | P2 | M |
| space-schedules | No row actions or enabled state. The cron string is shown raw. | Add an Enabled `Switch` and a ⋯ menu (Run now, Edit, Delete). Show "Weekdays at 09:00 (Vilnius)" with the cron in mono below it. | P2 | S |
| xspace-requests | The only action is "Decline" (with Reason). Nothing tells the operator that control answers the request. Inbox item shown as a raw ID. | "Control answers this request. Status: waiting." Decline as a secondary red button. Link to the inbox item. | P2 | S |
| xspace-access | Tabs "Requests / Cross-space access" repeat the section name. "Remove" is plain text. | Rename the tab to "Access". Make Remove red subtle with confirmation. | P3 | S |

### 3.3 Terminal and runs

The structural fix is in §4. These are the page-level findings it has to address.

| Shot | Problem | Fix | P | E |
|---|---|---|---|---|
| terminal-live | The header is a 15px role name and two badges (CLAUDE-CODE, RUNNING). There's no H1 and nothing says it's a terminal page. | Header bar from §4.2: status dot, "control", "Control · Claude Code", "#4", state, a live duration. | P1 | M |
| terminal-live | "Take over terminal" sits alone on a second row under Live/Replay and Stop. | Put it in the same action group as the other controls. | P2 | S |
| terminal-live, run-manifest | Nine equal cards (Kind, Instance, Session, Lease generation, Launched by, Created, Started, Ended, Exit code), with Exit code alone on its own row, all below the fold. | An inspector panel to the right of the terminal (§4.2). IDs go into a collapsed Identifiers section. | P1 | M |
| terminal-live | The task, turn, duration, tokens/cost, node and account are missing from the page. `turn`, `node_id`, `launch.account_ref` and the task (`QueueTask.assignee_run`) are already in the API. | §4.2. | P1 | M |
| terminal-takeover | The banner is four lines long, and a toast repeats it ("You have taken over the terminal") over the bottom-right of the terminal. | Drop the toast. One-line banner: "You're typing in this terminal. Automated input is paused. Since 8:48 PM. [Release]". | P2 | S |
| terminal-replay | "EXITED" (header) and "EXITED · KILLED" (terminal toolbar). Exit code "—" for a killed run. The download icon has no label. | One state badge, "Exited · killed", in the header. Hide Exit code when there is none. Label the download "Download recording" (tooltip and aria-label). | P3 | S |
| run-usage | The usage table is good ("from the transcript"). It sits at the very bottom of the page. | Show a summary in the inspector (tokens, est. cost) with "Usage details" opening this table in a drawer. | P2 | S |
| run-manifest | "Configuration at launch" is one dense dot-separated line with `acc_…`. | A key–value list in the inspector's Config section, with names and links (prompt v2 → prompt page). | P2 | S |
| run-launch | Model is an empty text field ("The role's model when empty"). "Git worktree of" is an odd label for a repository path. You can't launch for a task. | Model: `Select`/`Autocomplete` with the placeholder "Role default (claude-sonnet-5-5)". "Worktree from repository": a `Select` of the space's repositories plus a branch field. Optional "For task" `Select`. Make the modal title bold. | P2 | S |
| terminal-live, terminal-live (light) | The terminal is fitted to 160 columns (`fitFont`), so at 1440px with the 200px sidebar the font is about 12px. Any side panel will shrink it further. The light theme keeps the dark terminal, which is right. | Focus mode (collapse the AppShell navbar), and a collapsible inspector (§4). | P2 | S |

### 3.4 Configuration

| Shot | Problem | Fix | P | E |
|---|---|---|---|---|
| prompts | The "Provenance" column holds descriptions and is empty for 4 of 9 rows. | Rename it "Description". | P3 | S |
| prompt-detail | The page opens on an empty "New draft" box. The current prompt's content is only visible after clicking "View". | Default to the latest version as rendered, read-only, with an "Edit as new draft" button. The versions list stays on the right. | P2 | M |
| prompt-detail | Version rows show `usr_…` and `extends prv_…`. v2 offers View/Diff/Fork while v1 offers View/Roll back/Fork. Save draft and Publish are ghosts (C10). | Names (C3). The same actions on every version through a ⋯ menu. "Compare with previous". | P2 | S |
| prompts-diff | A narrow modal (about 770px) that cuts long lines. No line numbers. | `Modal size="90%"` or `fullScreen`, with line numbers, a wrap toggle, and unified or side-by-side views. | P2 | M |
| roles | "New role template" sits under the tabs, while other pages put their primary action in the header. Template names aren't links. Every kind badge is the same blue. | Tab-specific action in the header. Names as `Anchor`. Kind as an icon plus text. | P3 | S |
| skills, skill-detail | Fine. The placeholder for the new file name repeats an existing file ("references/commands.md"). Publish is a ghost button. | Placeholder "references/new-file.md". C10. | P3 | S |
| space-role-preview | Good: field, value and origin. `system_text_sha256` shows to everyone. | Move the hash into Identifiers. | P3 | S |
| approval-policies | Good scope alert. "Usable for" and "Request waits" are cryptic. Rows have a trash icon only, with no edit. | "Approval valid for", "Request expires after". Add an edit action. C9. | P3 | S |
| human-types | The type editor is three raw JSON Schema text areas, with helper text written as JSON syntax. The nav says "Action types" while the page says "Human action types". The installation's "Reminders, escalation and routing" defaults sit on the same page. | Keep JSON as the source of truth, but put a live form preview beside it, validate inline, and plan a field builder. Use one name. Move the defaults to their own tab. | P2 | L |
| events-catalog | Each card shows the subject twice (description and mono line). | Keep the mono line only. | P3 | S |
| events-publishing | "168 h". Counters in mono. Below the card the page is empty. | "7 days" (C4). Sans numbers with tabular figures. | P3 | S |
| event-template | Dense, but right for an advanced editor. Good sample test. Regex patterns wrap in the middle of a token. | `white-space: nowrap` with horizontal scroll for the pattern column. | P3 | S |
| mcp-servers, mcp-server-detail | The slug "tickets__…" is cut off in a wide column. "Delete" is plain text. A grant for every role is entered as "*". | Don't truncate the slug. C9. Role `Select` with an "All roles" option. | P3 | S |
| accounts | Good table and honest copy ("sealed when saved"). Harness naming (C5). | C5. | P3 | S |
| xspace-admin | Good. | Keep. | – | – |

### 3.5 Admin

| Shot | Problem | Fix | P | E |
|---|---|---|---|---|
| setup-node-enroll, setup-node-enroll (light) | Pending nodes show "— /" in the Agent column (empty OS and arch joined with "/"). The page mixes "—" and "–". Nothing tells the user how to finish enrolling a pending node. | Show "Waiting for agent" with a "Copy enroll command" button on pending rows. Use one dash glyph everywhere. | P2 | S |
| auth-users | The grant badges are well coloured by role. MFA "REQUIRED" is ambiguous: it means required but not enrolled. | "Not enrolled", in amber, with "Required by policy" in a tooltip. | P2 | S |
| auth-user-profile | Fine. | Keep. | – | – |
| auth-tokens | Revoke is a grey filled button on every row (C9). | Red subtle, with confirmation. | P3 | S |
| audit-log | Actors and targets are raw IDs. Filters are free-text ID fields with no time range. Every row has a green SUCCESS badge. | Names via `EntityRef`. Actor and target `Select`s with search, plus a `DatePickerInput type="range"`. Show the outcome only when it's a failure (red), otherwise dimmed text. | P2 | M |
| ops-backups | Two cards of "Included / Not included" prose are always on screen. Values in mono. `bkp_…` and `usr_…` IDs. | An `Accordion` "What a backup contains". Sans values. Names. Make the KEK sentence a yellow alert. | P2 | S |
| sign-in-providers | "Not ready for SSO only" uses ✗ text glyphs in a grey box. The settings Save is a ghost button, far from the fields. | `List` with `IconX`/`IconCheck` icons in red and green. Save beside the field, or in a sticky bar. | P3 | S |
| sign-in-provider-form | A long form where every advanced field (tid, hd, acr, step-up, PEM) is always shown. | An `Accordion` "Advanced" for tenants, domains, ACR, step-up and CA certificates. | P2 | S |

---

## 4. The operator's four points

### 4.1 Run page breadcrumbs (P1, S)

**What the screenshots show.** terminal-live, terminal-replay and terminal-takeover already render "Spaces /
Payments platform / run_01m6pqe1pfexrs2qtb8j7mgw04". `routes/spaces.$spaceId.runs.$runId.tsx` lines 86–90 link the
space. What's actually wrong:

- The run page renders **outside the space shell**: no SpaceNav, so you can't jump to the Queue or Human actions of
  the space you're looking at, and the sidebar only highlights "Spaces".
- While the space query loads, the crumb shows the raw `spaceId` (`spaceName ?? spaceId`). On a slow load, or for a
  user without access to the space record, that's the only thing shown. This may be what the operator saw.
- There's no level between the space and the run, and the run crumb is a ULID.

**Fix.**
- Render the run inside the space layout (C6), with the new **Terminals** tab active (§4.3).
- Breadcrumb under the tabs: `Terminals / control #4`, with the run ID in a `Tooltip` plus `CopyButton`.
- Show a `Skeleton` while the space name loads, never the raw ID.
- `Esc` (when the terminal isn't taken over) or a "← All terminals" link goes back to the space's Terminals view
  with this tab still selected.

### 4.2 Run page overhaul (P1, M)

Goal: one glance tells you who the agent is, what it's doing, for which task, for how long, at what cost and where
it runs. The terminal gets as much space as possible, and everything secondary sits one click away.

```
┌ Payments platform  ● active   build-01 · connected                       [Launch run] [⋯] ┐
│ Overview  Terminals●4  Inbox 6  Queue  Human actions 6  Messages  …                          │
├──────────────────────────────────────────────────────────────────────────────────────────────┤
│ Terminals / control #4                                                                       │
│ ● control   Control · Claude Code   #4   working · 12m 30s        [Live|Replay] [⌨ Take over] [■ Stop ▾] [⤢] │
├──────────────────────────────────────────────────────────────┬───────────────────────────────┤
│ ▸ live · watch-only · 160×48                                 │ NOW                        [»]│
│ ┌──────────────────────────────────────────────────────────┐ │ ● working  "Reviewing PR #42" │
│ │ $ …                                                      │ │ turn 4 · running 12m 30s      │
│ │                                                          │ │ last output 3s ago            │
│ │                    terminal (fills)                      │ │ ⚠ 1 approval waiting →        │
│ │                                                          │ ├───────────────────────────────┤
│ │                                                          │ │ TASK                          │
│ │                                                          │ │ Bump the card SDK to 7.2 →    │
│ │                                                          │ │ running · high · #3 in queue  │
│ │                                                          │ ├───────────────────────────────┤
│ │                                                          │ │ USAGE            from transcript│
│ │                                                          │ │ 2,760 in · 1,986 out          │
│ │                                                          │ │ cache 91.5k read · 11.2k write│
│ └──────────────────────────────────────────────────────────┘ │ ≈ $0.04 · budget 0.1% ▁       │
│ [ Message the agent (delivered at its next idle point) ][Send]│ ├───────────────────────────────┤
│                                                              │ PLACEMENT                     │
│                                                              │ node build-01 ● · herdr 0.1.17 │
│                                                              │ account team-claude           │
│                                                              │ worktree spaceman/retry-budget│
│                                                              │   in settlement →             │
│                                                              │ ▸ Launch & config             │
│                                                              │ ▸ Identifiers                 │
└──────────────────────────────────────────────────────────────┴───────────────────────────────┘
```

**Header bar** (one row, `Group justify="space-between" wrap="nowrap"`):
- Status dot. Colour by `observed` plus `status_kind`: green working, blue idle or waiting for input, amber waiting
  on a person, grey ended, red failed or lost, purple unknown (node offline).
- Role (`Title order={3}`), then "Kind · Harness" as dimmed text, not badges.
- `#turn` and a live duration (`started_at` → now or `ended_at`).
- Actions on the right: Live/Replay `SegmentedControl`, "Take over" (admins only), "Stop ▾" (Interrupt / Stop the
  program), and a focus-mode toggle that collapses the AppShell navbar and the inspector.

**Terminal column**: `TerminalView` with `flex: 1` and its toolbar badges (live/watch-only, 160×48), then the
message box. When the run has ended, the message box becomes a one-line strip: "Ended 8:47 PM (12 m ago) · killed ·
exit —   [Replay from start] [Launch again]".

**Inspector** (320px, `ScrollArea`, collapsible with `»`, open state remembered):
- **Now**: status text (`run.status`), turn, duration, last output time, and links to pending human actions this run
  opened (filter human actions by requester run).
- **Task**: the task where `assignee_run === run.id` (the queue query is already cached). Title links to the queue
  drawer. Shows state, priority and position. If there's no task: "No task: launched manually by Alex Morgan".
- **Usage**: the totals from `RunUsageCard`, an estimated cost when the harness reports one, and a budget
  `Progress` for the space. "Usage details" opens the full table in a drawer. Unknown usage reads "No usage
  reported yet" in grey.
- **Placement**: node (with state dot), runner version, account *name*, worktree/branch/repository as links.
- **Launch & config** (`Accordion`, closed): launched by (name), created/started/ended, end reason, exit code;
  role revision, template, profile, prompt version (link), skills, MCP servers. This replaces "Configuration at
  launch".
- **Identifiers** (`Accordion`, closed): run, instance, session, lease generation, terminal ID, each with a
  `CopyButton`.

The takeover banner moves into the terminal column header as one line (§3.3). Errors (`r.error`) show as a red
`Alert` above the terminal, not at the bottom of the page.

### 4.3 Space "Terminals" view

**Where it lives:** a new SpaceNav tab, **Terminals**, second after Overview, at `/spaces/$spaceId/terminals`. The
selected run lives in the search params (`?run=<id>`). The tab label shows the number of live agents ("Terminals
4") with a dot coloured by the worst state: amber if any agent waits on a person, red if any failed.

**Unify with the run page:** `/spaces/$spaceId/runs/$runId` redirects to (or renders) `terminals?run=$runId` with
the inspector open. There is then one terminal UI, not three. The overview's "Agents" card and the queue's run
links point here.

```
Payments platform  ● active   build-01 · connected                                  [Launch run] [⋯]
Overview  Terminals 4  Inbox 6  Queue  Human actions 6  Messages  Repositories  …
──────────────────────────────────────────────────────────────────────────────────────────────────
┌───────────────────┬────────────────────┬────────────────────┬──────────────────┬─────────────┐
│ ● control  ♛  #4  │ ● reviewer ⚒  #2   │ ● kb-support 📖 #1 │ ◐ impl-backend ⚒ │ Ended (3) ▾ │  [Tabs|Grid]
│ working · 12m     │ waiting approval 1 │ idle · 40s         │ #3 interrupting  │             │  [+ Launch]
└───────────────────┴────────────────────┴────────────────────┴──────────────────┴─────────────┘
 ● control #4 · Control · Claude Code · working 12m · task: Bump the card SDK to 7.2 →    [Live|Replay] [⌨] [■▾] [»]
┌────────────────────────────────────────────────────────────────────────────┬─────────────────┐
│                                                                            │ inspector       │
│                       live terminal of the selected tab                    │ (as in §4.2,    │
│                                                                            │  collapsed by   │
│                                                                            │  default here)  │
└────────────────────────────────────────────────────────────────────────────┴─────────────────┘
 [ Message control (typed into its session at once)                                    ] [Send]
```

**Each tab** (Mantine `Tabs` with a custom two-line `Tabs.Tab`, horizontally scrollable via `ScrollArea`):
- line 1: **status dot**, **role**, **kind icon** (control `IconCrown`, worker `IconHammer`, kb `IconBook`, dreamer
  `IconMoon`, diagnostic `IconStethoscope`; each also has an `aria-label`), **#turn**;
- line 2: **state** in words. "working · 12m" uses `status` when present, otherwise the observed state. Other
  examples: "waiting for you · 1" (pending human actions from this run, amber), "idle · 40s", "taken over by
  Alex", "starting…", "node offline" (purple).
- Order: control first, then running workers by start time, then KB and dreamer. Ended runs go under an "Ended
  (n) ▾" `Menu` at the end of the bar. Choosing one opens it as a temporary tab.
- Keyboard: `Ctrl+1…9` selects a tab, `[` and `]` move to the previous and next tab. These are disabled while the
  terminal is taken over, so keys go to the program. Hover shows a `Tooltip` with the task title and the start
  time.

**Tab content:**
- Running: a live, watch-only `TerminalView`, with the action bar and message box described above.
- **Ended**: the last frame. Replay is seeked to the end and paused, with a translucent overlay strip at the top:
  "Exited 12 m ago · killed · turn 3 · 4,746 tokens  [Replay from start] [Open details]". The tab's dot is grey.
  Failed or lost runs are red, and their error goes in the strip.
- Node disconnected: the last frame, with "build-01 is offline. Showing the last output from 8:47 PM. The run state
  will reconcile when the node reconnects." Purple dot.

**Grid mode** (`Tabs|Grid` toggle): `SimpleGrid cols={2}` with up to four small live terminals (font fitted to
cell, read-only). A click focuses one and switches back to Tabs. Browsers allow about six connections per host on
HTTP/1.1, and `TerminalView` already limits streams for hidden tabs. So the grid streams at most four terminals
live and refreshes the others as snapshots every few seconds.

**What ended runs show:** runs that ended in the last 24 hours, behind "Ended (n)". The filter `Show ended: 24 h ·
7 days · All` is remembered per user. Older runs stay in the Runs history table on the overview.

**Empty states:**
- No roles yet: "This space has no roles. Add the starter roles to get control, a planner, implementors and a
  reviewer. [Add starter roles]".
- Roles but no runs ever: "No agents have run here yet. Control coordinates the space. Start it first. [Launch
  control] [Launch another role…]".
- Nothing running now, ended runs present: the ended tabs are listed, with a banner above: "No agent is running.
  Control stopped 2 h ago (exited 0). [Launch control] [Replay last control run]".
- Space paused: an amber banner "Paused (hold): agents keep running but get no new work. [Resume]". For an
  interrupt or emergency pause, "Agents were interrupted", and the tabs show their ended or interrupted state.
- No permission to watch terminals: "You can see this space but not its terminals. Ask an admin for the
  operator role."

### 4.4 Workspace view (herdr-style)

**Where it lives:** a global sidebar entry, **Terminals**, under Work, at `/terminals`, with the shortcut `g t` and
⌘K for quick switching. It's a full-bleed layout: the AppShell navbar collapses to icons (`navbar.width` 56,
labels in tooltips), and the page has its own left panel, a session sidebar like herdr's.

```
┌─┬─────────────────────────────┬────────────────────────────────────────────────────────────────┐
│≡│ Terminals          ⌘K       │ Payments platform  ● active · build-01            [Open space →]│
│ │ [ Filter agents…          ] │ ┌─────────────┬──────────────┬──────────────┬────────┬────────┐│
│⌂│ ☐ Running only  ☐ Needs you │ │● control #4 │● reviewer #2 │● kb-support  │◐ impl… │Ended 3▾││
│ │                             │ │working 12m  │waiting you 1 │idle 40s      │#3 int… │        ││
│◎│ ★ PINNED                    │ └─────────────┴──────────────┴──────────────┴────────┴────────┘│
│ │ ▾ ● Payments platform  4 ⚠1 │  ● control #4 · working · task: Bump the card SDK →  [⌨][■▾][»]│
│⚑│     ● control      #4  12m  │ ┌────────────────────────────────────────────────────────────┐ │
│ │     ● reviewer     #2  ⚠1   │ │                                                            │ │
│▤│     ● kb-support   #1  idle │ │                                                            │ │
│ │     ◐ impl-backend #3  int  │ │                selected agent's live terminal              │ │
│ │     ▸ Ended (3)             │ │                                                            │ │
│ │ ▾ ● Docs site          1    │ │                                                            │ │
│ │     ● control      #1  idle │ │                                                            │ │
│ │ ALL SPACES                  │ │                                                            │ │
│ │ ▸ ◌ Infrastructure  paused  │ └────────────────────────────────────────────────────────────┘ │
│ │                             │ [ Message control …                                    ][Send]│
│ │                             ├────────────────────────────────────────────────────────────────┤
│ │ build-01 ● · gpu-02 ○       │ LIVE · 6 agents running · 1 waiting for you · node build-01 ok │
└─┴─────────────────────────────┴────────────────────────────────────────────────────────────────┘
```

**Left panel** (260px, resizable, collapsible with `Ctrl+B`):
- Filter input (filters agents and spaces by name), and toggles "Running only" and "Needs you".
- A **space tree**. Each space row has a dot for its worst agent state, its name, the number of running agents
  and an amber ⚠ count of pending human actions. Paused spaces are dimmed with "paused". Archived spaces are
  hidden.
- Under each space, its **agents**: dot, role, `#turn`, a short state ("12m", "idle", "int" for interrupting,
  ⚠n). Ended agents fold under "Ended (n)".
- Pinned spaces come first (star on hover), then all spaces alphabetically. Expanded state is remembered.
- Footer: node health in one line (`build-01 ●  gpu-02 ○`), each linking to Nodes.
- Implementation: Mantine `Tree` (v8) or nested `NavLink`s. Keyboard: ↑/↓ moves, → expands, Enter opens.

**Main area**: the same `SpaceTerminals` component as §4.3, for the selected space: tab bar, terminal and
collapsible inspector. A click on an agent in the tree selects its tab. A click on a space opens its tabs with the
last-selected or first agent. The URL carries both (`/terminals?space=payments&run=…`), so views can be shared and
restored.

**Status bar** (32px, `Group` with a top border): connection (LIVE), the total of running agents, the count waiting
for you (a link to Needs you), and the focused agent's tokens and duration.

**Quick switcher**: `@mantine/spotlight` with every live agent across spaces ("payments / control #4 · working"),
recent ended ones, and actions ("Launch control in Docs site").

**Attention integration:** an agent waiting on a person shows an amber dot and ⚠n everywhere: tree, tab and status
bar. Clicking ⚠ opens the human action drawer on top of the workspace, so you can answer without leaving.

**Permissions:** viewers watch. Operators can message and stop. Admins can take over. Hide the controls a user
can't use, don't just disable them.

**Empty states:**
- No spaces: "No spaces yet. A space is a team of agents on one node. [Create a space]".
- Spaces but no node connected: "No node is connected, so no agent can run. [Enroll a node]".
- No running agents anywhere: the tree shows every space dimmed, and the main area says "Nothing is running. Pick a
  space to see its last runs, or launch control." with a space `Select` and a [Launch control] button.
- Filter matches nothing: "No agents match "rev". [Clear filter]".

**Responsive:** below 1100px the tree becomes a `Drawer` opened from a button in the tab bar. Below 768px the
view becomes a list of agents linking to single-terminal pages. This is a desktop tool, so the small layout
doesn't need to be more than that.

---

## 5. Suggested implementation order

1. **Theme fixes** (S): dark `light` variant resolver with contrast tests (C1); badge sentence case (C2); modal
   title weight (C11); type scale sm 13 / xs 12 (C8); `classNames={{ input: 'mono' }}` (C12). One PR, visible on
   every page.
2. **Shared formatting** (M): `EntityRef`, `Time`, `formatDuration`, `DurationInput`, `harnessLabel`/`kindLabel`,
   `plural`, `Priority`. Then apply them on the busiest pages: human-action drawers, approvals, run page,
   messages, audit log (C3–C5).
3. **Space shell** (M): a real layout in `spaces.$spaceId.tsx` with a persistent header, SpaceNav, section
   toolbar and inner breadcrumbs. Move the run, inbox-item and subscription detail pages into it. This fixes
   operator point 1 and C6.
4. **Run page overhaul** (M): header bar, inspector panel (Now, Task, Usage, Placement, Launch & config,
   Identifiers), ended strip, focus mode. Operator point 2.
5. **Space Terminals tab** (M–L): `SpaceTerminals` (tabs, ended handling, empty states, grid with a stream
   budget). Make the run route render it. Replace the overview's Runs table with the Agents card. Operator point
   3.
6. **Queue and messages correctness** (S): one display state per task, the assignee run with a terminal link,
   the system-notification filter in messages, the attention backup status. These are cheap P1s, so they can run
   in parallel with steps 3–5.
7. **Workspace view** (L): `/terminals` with the space tree, status bar and Spotlight, reusing step 5. Operator
   point 4.
8. **Sidebar IA** (M): Work / Terminals / Spaces tree, collapsible admin groups, rename Attention to Health (C7).
   Do this after step 7 so the new entries land together.
9. **Copy pass** (M, writing): subtitles, helpers and alerts per C8, plus the rewrites in §3. Best done in one
   sweep by one person.
10. **Forms and dialogs** (M): no stacked modal for policies, sticky footers, pickers instead of IDs, durations,
    `DateTimePicker`, outcome labels, Deny behaviour, the reason inside actions in the queue drawer (C9–C11).
11. **Polish** (S each): the remaining P3 rows in §3 (pluralisation, target sizes, drawer autofocus, node enrol
    call to action, MFA input, account page width, prompt diff size).
