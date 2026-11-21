# Plan: Usage page, Overview and Attention rework, settings hubs in the sidebar

- **Status:** accepted 2026-11-20. The operator answered §7 with "all recommended" (see §8). ADR-0103, SM-UI-008,
  SM-UI-009 and, if needed, migration 0069 are confirmed free.
- **Goal:** an operator UI batch for release v0.1.16.
  1. A cross-space **Usage** page built from the usage ledger, broken down per space, account, model, role and harness.
  2. An **Overview** page that leads with usage and with what needs a person.
  3. An **Attention** page that lists the actual items to handle, not only per-space counts.
  4. A shorter **sidebar**: the Configuration, Integrations and Access groups each become one entry that opens a tabbed
     page. Every existing URL keeps working.
- **Base:** `next/v0.1.16` at 60e2cdc6. Every **[code]** fact below was read there.
- **Out of scope:** human-action comments that don't reload (a separate task already exists), booking Codex and
  Harbor Pilot usage into the ledger (§7, Q5), and any change to per-space pages beyond the links named below.

---

## 1. What exists today [code]

### 1.1 Usage data

- **`usage_buckets`** (migration `0057_usage_ledger.sql:70`) is the ledger.
  - Key: `(space_id, account_id, role, harness, model, granularity, bucket_start)`, on a WITHOUT ROWID table.
  - Counters: input, output, cache-write, cache-read and reasoning tokens, `requests`, and `cost_usd`.
  - `account_id` is `''` for a run without a subscription account. `model` is `unknown` when the report named none.
  - Indexes: `ix_usage_buckets_time (granularity, bucket_start)` and
    `ix_usage_buckets_account (account_id, granularity, bucket_start)` (`0057:88-89`).
  - Hourly rows are kept 90 days, then rolled into daily rows, which are kept 730 days
    (`internal/retention/usageledger.go`).
  - There is no duration or turn count.
- **Only Claude Code usage reaches the ledger.** `usage.Service.Report` books into the buckets only when
  `rep.Source == proto.UsageClaudeTranscript` (`internal/usage/usage.go:172`).
  - Codex reports `unavailable` (ADR at `DECISIONS.md:553`).
  - Harbor Pilot read-back fills `run_usage` with source `harbor-pilot`, but not the buckets.
  - So a cross-space page **silently under-counts Codex and gateway-only runs** unless it says so (§7, Q5).
- **`run_usage`** has one cumulative row per run, with its tokens in JSON. SQL cannot aggregate it. `get-space-usage`
  reads it (the last 500 runs) for the space overview card.
- **Account windows:** `account_windows` holds the latest 5-hour and 7-day `used_pct` and `resets_at` per account.
  `account_window_samples` holds 10-minute samples, kept 30 days. `usage_limits`, `limit_waits` and
  `account_switches` hold the limit state (ADR-0079, ADR-0090).

### 1.2 Usage API (`internal/api/usage.go`)

| Op | Path | group_by | Rule |
|---|---|---|---|
| `get-space-usage` | `GET /spaces/{id}/usage` | (run_usage summary) | `InSpace(RunRead)` |
| `get-space-usage-series` | `GET /spaces/{id}/usage/series` | role, account, model, harness | `InSpace(RunRead)` |
| `get-account-usage` | `GET /accounts/{id}/usage` | space, role, model | `Global(AccountRead)`, rows filtered to spaces with `run:read` |
| `get-account-windows` | `GET /accounts/{id}/windows` | n/a | `Global(AccountRead)` plus a bound-readable-space check |
| `list-space-account-windows` | `GET /spaces/{id}/account-windows` | n/a | `InSpace(RunRead)` |

- **There is no cross-space series and no cross-account windows list.**
- Series are **folded in Go** (`internal/usage/query.go:149` `fold`): every bucket row in the range is loaded, summed
  in a map, then cut into pages. A daily series reads both the hour and day rows.
- Ranges are at most 31 days of hours or 800 days of days (`query.go:20-21`).
- The CLI binds `spaceman usage space` and `spaceman usage account` (`internal/cli/cmd_extra.go:220-223`).

### 1.3 Web

- **Sidebar** (`web/src/lib/nav.ts`, rendered by `components/AppLayout.tsx`) has 24 entries in five groups. Visibility
  is per entry (`global`, `admin`).
  - **Work:** Overview, Attention, Needs you (with a badge), Terminals, Spaces.
  - **Fleet:** Nodes.
  - **Configuration** (all global): Prompts, Roles & profiles, Skills, Approval policies, Action types, Cross-space.
  - **Integrations:**
    - Events, shown to everyone.
    - MCP servers, global.
    - Accounts, Git SSH keys and Harbor Pilot, admin only.
  - **Access:**
    - Users and Audit log, global.
    - Sign-in providers, Backups and System, admin only.
    - API tokens and Account, shown to everyone.
  - "Configuration", "Integrations" and "Access" are **group headings, not pages**.
- **Space usage** (`features/usage/SpaceUsage.tsx`, route `/spaces/$spaceId/usage`) is a per-space page.
  - Range: 24 h, 7 d or 30 d. Group by: role, account, model or harness.
  - It has total tiles, an SVG chart (at most seven groups plus Other), a table and the cost note.
  - Account names come from `useSpaceAccounts`.
  - The space overview's `SpaceUsageCard` (from `run_usage`) does not link to it.
- **Overview** (`routes/index.tsx`) has stat tiles (Spaces, Active runs, Needs attention count, Nodes), Active runs,
  Spaces and Nodes tables. **It shows no usage.**
- **Attention** (`routes/attention.tsx`, `features/attention/AttentionView.tsx`, `GET /attention` with
  `FilteredRead()`) is a table of spaces with **count badges** that link to space pages, plus cards for Nodes, Outbound,
  Backups and Storage.
  - It names no concrete item. You can't see which task is blocked or which run failed without clicking through.
  - It largely repeats **Needs you** (`/human`) for human actions.
  - It doesn't cover runs parked at a usage limit, open account usage limits, failed account switches, or incoming
    cross-space requests.
  - This is the "pretty much useless" the operator describes.
- **Links that must keep working:**
  - Telegram Mini App: `tg/deeplink.ts:18-19` (`attention` → `/attention`, `needs` → `/human`) and
    `tg/pages/TgHome.tsx:59,70`. `SpaceAttentionCard` links to `/attention`.
  - Web: `RestoreBanner` → `/backups`, `SpacemanMcpDetail` → `/tokens`, `SpaceMcpCard` → `/mcp-servers`.
  - Go: `internal/api/oidc.go:206` redirects to `/sign-in-providers?test_login=…`; `oidc.go:120-122` redirects to
    `/account`.
  - `/settings` is already taken: it is the Mini App's settings page and redirects to `/account` on the web
    (`routes/settings.tsx`).
- **No redirect pattern exists** (no `beforeLoad`/`redirect()`); the only one is `<Navigate>` in `settings.tsx`.
  Tabs (Mantine) are local state on `tokens.tsx`, `events.index.tsx` and the space overview, not URL-synced.
- **Tests pinning the nav:**
  - `web/e2e/upgrade-navigation.spec.ts:33-40` lists all 24 entries.
  - `ui-overflow.spec.ts:149-160` lists the pages.
  - `smoke.spec.ts` covers Overview and Attention and clicks Events in the main nav.
  - `ui-request-budget.spec.ts:21` caps `/attention` at 4 requests.
  - `tg-pages.spec.ts` covers the Mini App.
  - Screenshots `attention`, `space-usage-page`, `space-usage` (`docs/SCREENSHOTS.md`).

---

## 2. Design

### 2.1 Usage page (`/usage`, sidebar Work group)

- **Name:** "Usage" (recommended; §7, Q1). It matches the per-space page and the API, and avoids "Spend" or "Cost",
  which would read as a bill (ADR-0090: tokens first, cost only as "≈ $").
- **Who sees it:** anyone with `run:read` in at least one space. Rows and totals cover only those spaces, the same rule
  as `get-account-usage`. No new permission.
- **Layout:**
  - Controls: range (24 h, 7 d, 30 d, 90 d; 90 d uses daily buckets; §7, Q8) and group by (space, account, model, role,
    harness).
  - Filter chips: space, account, model, harness.
  - All of it lives in URL search params, so the Overview and other pages can deep-link (for example
    `/usage?group=model&space=spc_…`).
  - Totals tiles: tokens, input/output, cache, ≈ cost. A delta against the previous period of the same length is
    optional (§7, Q7).
  - Chart and table, reusing `UsageChart`, `series.ts` and `perGroup`. The table's space rows link to
    `/spaces/$id/usage`. Account and model rows set the filter.
  - **Accounts** section: each account bound to a readable space, with its 5-hour and 7-day window (used %, resets at),
    open usage limits and its tokens in the range. This is the "per account" view the operator asked for, with the
    limit state next to the tokens.
  - **Not counted** note: runs in the range whose harness books no ledger usage (Codex, Harbor Pilot read-back), as a
    count per harness, linking to the per-run figures. Unknown is missing, not zero (SM-REC-005).
- **API (new, read-only, no migration expected):**
  - `get-usage-series` — `GET /usage/series`
    - Rule: `FilteredRead()`.
    - Query: the existing `UsageSeriesQuery`, plus `group_by=space|account|model|role|harness` (default `space`) and
      optional filters `space_id`, `account_id`, `model`, `harness`, `role`, each repeatable and bounded (≤ 50 values).
    - Response: `UsageSeriesPage` plus `labels` (`{group: display name}`) for space and account groups. Names appear
      only for spaces the caller reads and accounts bound to them, the same visibility rule as
      `get-account-windows`.
    - Also `not_counted`: run counts per harness with no ledger usage in the range.
    - **Aggregation in SQL** (`GROUP BY` bucket and group over `ix_usage_buckets_time`, with `space_id IN (readable)`),
      not the Go fold, because a global range reads every space's rows.
  - `list-account-windows` — `GET /account-windows`
    - Rule: `FilteredRead()`.
    - The latest windows and open usage limits of every account bound to a readable space, with account name and
      harness. It reuses `ListSpaceAccountWindows` and `usage_limits`.
  - CLI: `spaceman usage all` and `spaceman usage windows` (ADR-0040 binds every op, or adds an exclusion).
  - Admin MCP: none needed; check its op allowlist.
- **Migration:** none planned. `ix_usage_buckets_time` serves the scan. If the T1 benchmark shows a temp B-tree on
  90-day daily queries, T1 proposes a covering index as migration **0069**, the only migration in this plan. It is
  additive and cheap to undo.

### 2.2 Overview rework (`/`)

From top to bottom:

1. **Needs attention:** the top 5 items from the reworked attention list (§2.3), most severe first, with "See all
   (N)" linking to `/attention`. It replaces the "Needs attention" count tile.
2. **Usage:** tokens today and in the last 7 days, with a 14-day daily sparkline. Top 5 spaces and top 5 models (7 d),
   with "Open Usage" linking to `/usage`. Data: two `/usage/series` reads (`bucket=day`, `group_by=space` and `model`).
3. **Accounts:** a compact row per account with its 5-hour and 7-day window bars and any open limit (from
   `list-account-windows`). It is hidden when no account is bound.
4. Active runs, Spaces (now with a "tokens 7 d" column from the space series), and Nodes. These stay as they are.

The request budget stays bounded: Overview adds at most 3 requests, all cached and invalidated by the change feed like
the rest. `ui-request-budget.spec.ts` gets an entry. In the Mini App, `/` stays `TgHome`; it is unchanged.

The space overview's `SpaceUsageCard` gains a "Usage over time" link to `/spaces/$id/usage` (a small fix that came up
in the sweep).

### 2.3 Attention rework

- **The problem:** today the page holds counts without items, and largely repeats Needs you.
- **Proposal (recommended; §7, Q2):** keep `/attention` as the full **actionable list**, remove it from the sidebar
  because the Overview shows its top items, and keep the route. Mini App deep links and `SpaceAttentionCard` keep
  working.
- **API:** `GET /attention` gains `items[]`, additive, so the counts stay for the Mini App and TG. Each item has:
  - `kind`, `severity` (`act` | `watch`), `space_id`, `title`, `since`.
  - A `target`: `{kind: run|task|human_action|limit_wait|account|event_subscription|node|backup|outbox|xspace_request,
    id}`. The web maps the target to a route, so the server stores no URLs.
  - Items are capped at 20 per kind, with the kind's full count beside them.
  - New categories, each behind its existing read permission:
    - Runs parked at a usage limit (open `limit_waits`, with resume time).
    - Open account usage limits.
    - Failed account switches in the last 24 h.
    - Incoming cross-space requests awaiting a decision.
    - Harbor Pilot budget exceeded, if it can be read without a network call inside the request (cached status only;
      otherwise left out).
  - Titles that come from agents (task and human-action titles) are untrusted text. They are rendered as plain text
    and truncated.
- **Web:** a list grouped **Act now** (unknown runs, overdue or escalated human actions, blocked or failed tasks,
  quarantined events, failed account switches, incoming cross-space requests, dead letters, failing backups,
  disconnected nodes, storage problems) and **Watch** (failed runs of the last 24 h, parked runs, account limits,
  budget warnings, storage near quota). A failed run is already over, so it is Watch; a quarantined event stays until a
  person reprocesses or discards it, so it is Act now. The server's severities decide (ADR-0103 does not name them).
  - Each row says what, where, since when, and links directly to the item: a run page, the queue, or
    `/spaces/$id/human?action=…`.
  - Filters by space and kind. The installation cards (Storage, Backups) stay below.
  - Pending human actions show as a single "N human actions — open Needs you" row instead of repeating `/human`.

### 2.4 Settings hubs (sidebar consolidation)

- **Sidebar after:**
  - **Work:** Overview, Needs you, Usage, Terminals, Spaces.
  - **Fleet:** Nodes.
  - Then three entries: **Configuration**, **Integrations** and **Access** (name: §7, Q4). Each is shown when at least
    one of its tabs is visible to the user.
  - Account stays in the header's user menu, where it already is, and leaves the sidebar (§7, Q4).
- **Hub pages:** each existing page renders a shared `HubTabs` bar under its header.
  - The tabs are links, not local state. Their visibility uses the same `global`/`admin` flags from `nav.ts` (moved to a
    `hubs` table).
  - The sidebar entry is active for any path in its group.
- **URLs (recommended, option A; §7, Q3):** every existing page keeps its URL (`/prompts`, `/accounts`,
  `/sign-in-providers`, …).
  - New entry routes `/configuration`, `/integrations` and `/access` redirect (`beforeLoad` + `redirect`) to the first
    tab the user may see.
  - Nothing that links into these pages changes: Go redirects, Mini App, banners, bookmarks.
  - `/settings` is not used, because it is the Mini App's.
- **Option B** (if preferred): nested URLs `/configuration/prompts` and so on, with redirects from the 18 old paths.
  - Detail routes `/prompts/$promptId` and `/events/templates/$templateId` also move.
  - `oidc.go:206` and three in-app links have to change.
  - It costs more and adds redirect tests, with no user-visible gain over A.

### 2.5 Security and invariants

- **No new permission and no relaxation:** both new ops are `FilteredRead()`, filtered per space by `run:read` (the
  `get-account-usage` rule). Totals, labels and `not_counted` never include an unreadable space. Account names show
  only for accounts bound to a readable space (the `get-account-windows` rule).
- No secrets, prompts or terminal output in any response. Metric labels are unchanged.
- Attention items carry agent-written titles: they are treated as untrusted and rendered as text.
- No write paths change. Not a security-relevant change.

---

## 3. Tasks

All tasks target `next/v0.1.16`. Tier: **light** is mechanical, **medium** is the default, **hard** is a risky core.

| ID | Task | Tier | Depends on |
|---|---|---|---|
| UI-T0 | Record ADR-0103 (Proposed → Accepted after Q1–Q8) and requirements SM-UI-008/009 | light | answers to §7 |
| UI-T1 | `get-usage-series` cross-space API with SQL aggregation, CLI `usage all` | **hard** | UI-T0 (numbers only) |
| UI-T2 | `list-account-windows` API, CLI `usage windows` | medium | — |
| UI-T3 | Web Usage page `/usage` | medium | UI-T1, UI-T2 |
| UI-T4 | Attention API: `items[]` and new categories | medium | — |
| UI-T5 | Attention page rework and `SpaceAttentionCard` | medium | UI-T4 |
| UI-T6 | Overview rework | medium | UI-T1, UI-T2, UI-T4 |
| UI-T7 | Settings hubs: `HubTabs`, sidebar, entry redirects | medium | — (option B adds UI-T7b, light) |
| UI-T8 | Docs, screenshots, traceability, CHANGELOG | light | UI-T3, UI-T5, UI-T6, UI-T7 |

Three tasks can start in parallel: UI-T1, UI-T2 and UI-T4. UI-T7 is independent of all of them. UI-T3, UI-T5 and
UI-T6 touch different routes but all of them change `nav.ts` and the e2e nav list: **land UI-T7 first, or rebase the
others onto it**.

### UI-T1 — Cross-space usage series (hard)

- **Files:**
  - `internal/store/queries/usage_ledger.sql` (a new `SumUsageBuckets…` query per group column; sqlc cannot vary the
    GROUP BY column, so use one query per group or a `CASE` expression), `internal/store/db` (generated).
  - `internal/usage/query.go` (`GlobalSeries`, groups list), `internal/api/usage.go` (op, rule, labels, `not_counted`).
  - `internal/cli/cmd_extra.go`, `api/openapi` (regenerated), `web/src/api/schema.gen.ts`.
- **Acceptance:**
  1. A viewer with `run:read` in space A only gets no row, total, label or `not_counted` count from space B. There is
     a test per group_by and per filter.
  2. The results equal the Go fold of `SpaceSeries`/`AccountSeries` for the same rows, as a property test over random
     buckets including hour and day mixes.
  3. Benchmark: 200k bucket rows (50 spaces × 10 accounts × 5 models, hourly over 31 days plus daily over 90 days).
     A 30-day `group_by=space` query answers in < 150 ms. A 90-day daily query has no full table scan (record the
     `EXPLAIN QUERY PLAN`). The numbers go in the PR. If a covering index is needed, it is migration 0069 with a
     rollback note.
  4. Filters are bounded (≤ 50 values each, 400 otherwise). The range limits are the same as today.
  5. `make check`, `api-check`, `sqlc-check`. `spaceman usage all --group-by model` works in the CLI e2e test.
- **Risks:**
  - The `IN (readable spaces)` list can be large for global readers. Use no filter when the caller reads `*`.
  - Paging after a SQL GROUP BY must stay stable (order by bucket, group).

### UI-T2 — Account windows across spaces (medium)

- **Files:** `internal/store/queries/usage_ledger.sql`, `internal/usage`, `internal/api/usage.go`, CLI, OpenAPI.
- **Acceptance:**
  - Lists each account bound to at least one readable space, once: name, harness kind, 5-hour and 7-day used % with
    resets_at and observed_at, and open `usage_limits` (kind, resets_at).
  - An account bound only to unreadable spaces is absent.
  - Tests cover a viewer, an operator and an admin. CLI `spaceman usage windows`.

### UI-T3 — Usage page (medium)

- **Files:** `web/src/routes/usage.tsx` (new, `validateSearch` for range, group and filters),
  `features/usage/UsageExplorer.tsx` (generalised from `SpaceUsage`, which then wraps it with a fixed space filter so
  the per-space page doesn't regress), `features/usage/AccountWindowsTable.tsx`, `api/queries.ts`, `lib/nav.ts`.
- **Acceptance:**
  1. Group by space, account, model, role and harness. Filter chips by space, account, model and harness. The state is
     in the URL and survives a reload.
  2. Space rows link to the space Usage page. Labels come from `labels`, not raw IDs.
  3. The Accounts section shows window bars and open limits. The "Not counted" note shows per-harness run counts when
     they are non-zero.
  4. An empty state, and the "not every row" alert kept.
  5. Vitest for the explorer (group switch, filters to URL, labels). e2e: nav list updated. `ui-overflow` gets `/usage`
     at 390 px and 1280 px. Screenshot `usage-page` in both themes with axe.

### UI-T4 — Attention items (medium)

- **Files:** `internal/store/queries/attention.sql`, `internal/api/attention.go`, `internal/app` test
  (`TestAttentionOverHTTP` extended), OpenAPI, schema.
- **Acceptance:**
  1. `items[]` with kind, severity, space, title, since and target, at most 20 per kind with full counts. Existing
     fields are unchanged.
  2. The new categories (parked runs, open account limits, failed switches in 24 h, incoming cross-space requests)
     each require their existing read permission. A viewer sees none of another space's items.
  3. No network I/O while serving. Harbor Pilot is included only from the cached status, or left out with a note.
  4. Response time with 10k open tasks stays within today's per-category query count, plus one query per new category.

### UI-T5 — Attention page (medium)

- **Files:** `features/attention/AttentionView.tsx` (rewritten), `features/attention/targets.ts` (target → route),
  `SpaceAttentionCard.tsx`, `routes/attention.tsx`, `lib/nav.ts` (remove the entry, per Q2), Mini App
  (`TgHome`, the `/attention` view in the Mini App keeps working, with web-only targets unlinked as today).
- **Acceptance:**
  - Rows are grouped Act now / Watch, each with what, where and since, and a link to the item. Filter by space and
    kind. Human actions collapse to one row linking to Needs you.
  - Vitest for target mapping and grouping. `smoke.spec.ts` and `tg-pages.spec.ts` are updated. The `attention`
    screenshot is refreshed.

### UI-T6 — Overview (medium)

- **Files:** `routes/index.tsx`, `features/overview/*` (new: `AttentionTop`, `UsageSummary`, `AccountWindowsStrip`),
  `features/usage/UsageCards.tsx` (a link to the space Usage page).
- **Acceptance:**
  - The order of §2.2 holds. Sparkline and top-5 lists come from `/usage/series`. The accounts strip is hidden without
    accounts.
  - Overview adds ≤ 3 requests, and the `ui-request-budget.spec.ts` entry is updated. The Mini App home is unchanged.
  - Vitest per new component. Screenshot `overview` in both themes. The 390 px layout has no horizontal scroll
    (`ui-overflow`).

### UI-T7 — Settings hubs (medium)

- **Files:**
  - `lib/nav.ts`: a `hubs` table and the `NavItem` union.
  - `components/AppLayout.tsx`: hub entries and active state by group.
  - `components/HubTabs.tsx` (new). `PageHeader` usage on the 18 member pages: one line each.
  - `routes/configuration.tsx`, `routes/integrations.tsx`, `routes/access.tsx` (`beforeLoad` redirect to the first
    visible tab, or a "nothing here" page when none is visible).
- **Acceptance:**
  1. The sidebar shows the §2.4 list. A hub entry appears only if a tab is visible to the user (viewer, operator and
     admin checked).
  2. Every old URL still loads its page directly. `/configuration` and the other entry routes land on the first
     visible tab.
  3. Tabs are links (keyboard and middle-click work), with `aria-current` on the active one.
  4. `upgrade-navigation.spec.ts` is rewritten to visit each hub and each tab. `smoke.spec.ts`'s Events click goes
     through Integrations. Tests at 390 px and 1280 px, light and dark, with axe.
  5. A vitest for hub visibility per role.

### UI-T7b — Only if Q3 = option B (light)

- Move the member routes under `/configuration/…` and the other hubs, add redirect routes for the 18 old paths and the
  two detail routes, and update `oidc.go:206`, `RestoreBanner`, `SpacemanMcpDetail` and `SpaceMcpCard`.
- A redirect test for every old path.

### UI-T8 — Docs (light)

- `site/` guide pages for Usage, Overview and Attention, and the sidebar layout. `docs/SCREENSHOTS.md` entries.
  `docs/TRACEABILITY.md` rows for SM-UI-003, 008 and 009. `docs/PLAN.md` progress log. `CHANGELOG.md` Unreleased.
  `make site-check`.

---

## 4. API and migration changes (summary)

| Change | Kind | Undo cost |
|---|---|---|
| `GET /usage/series` (`get-usage-series`) | new read op, `FilteredRead()` | low |
| `GET /account-windows` (`list-account-windows`) | new read op, `FilteredRead()` | low |
| `GET /attention` gains `items[]` | additive response field | low |
| CLI `usage all`, `usage windows` | new commands | low |
| Covering index on `usage_buckets` (migration 0069) | **only if the UI-T1 benchmark needs it** | low (drop index) |

Nothing is expensive to undo.

## 5. Requirements (proposed; Q0)

- **SM-UI-008:** a cross-space Usage view shows ledger tokens per space, account, model, role and harness over a
  chosen range, and the latest account windows, for the spaces the caller can read. It says which runs' usage is not
  counted.
- **SM-UI-009:** the main navigation groups rarely used administration pages into tabbed hubs, and every existing page
  URL keeps working.
- **SM-UI-003** evidence changes: Attention lists items, not only counts.

## 6. ADR-0103 (draft, Proposed)

> **ADR-0103 — Operator UI: a cross-space Usage page, an itemised Attention list, and settings hubs.**
> Context: §1. Decision: §2.1–§2.4, with URL option A (existing page URLs stay, hubs are entry routes that redirect)
> and Attention kept as a route but taken off the sidebar. Consequences: two new read ops with per-space filtering
> like `get-account-usage`, no new permission, no migration unless the benchmark needs an index. Codex and Harbor
> Pilot usage stay outside the ledger and are shown as "not counted" (follow-up ADR if they should be booked).

## 7. Open product questions

- **Q0:** Confirm the numbers: ADR-0103, SM-UI-008 and SM-UI-009 (and migration 0069 if one is needed).
- **Q1 — Name:** "Usage" (recommended), or "Spend", "Consumption" or "Tokens"?
- **Q2 — Attention:**
  - (a) Keep `/attention` as the full itemised list, off the sidebar, with the top 5 on Overview (recommended).
  - (b) Fold it entirely into Overview and redirect `/attention` → `/`.
  - (c) Keep the sidebar entry, with a count badge.
- **Q3 — Hub URLs:** (A) keep every existing URL and add `/configuration`, `/integrations` and `/access` entry routes
  (recommended), or (B) nested `/configuration/<tab>` URLs with redirects from the old ones?
- **Q4 — Hub names and contents:**
  - Keep the three group names, or rename Access to "Administration"?
  - Should Account leave the sidebar (it is in the user menu)?
  - Should API tokens stay under Access?
  - Should Nodes stay alone under Fleet, or move into Work?
- **Q5 — Codex and Harbor Pilot usage:** for v0.1.16, show them as "not counted" (recommended), or book Harbor Pilot
  read-back into the ledger (needs its own ADR and a migration-free ledger write path; probably hard)?
- **Q6 — Visibility:** the Usage page for every user with `run:read` in some space, filtered per space (recommended),
  or admins only? Should account names show to non-admins for accounts bound to their spaces (recommended: yes, the
  same rule as `get-account-windows`)?
- **Q7 — Money on Overview:** tokens only, or also the "≈ $" estimate? And a delta against the previous period: yes or
  no?
- **Q8 — Ranges:** add 90 days (daily) on the Usage page? The ledger keeps 730 days of daily rows.

## 8. Decisions (2026-11-20)

The operator accepted every recommended option:

- **Q0:** ADR-0103, SM-UI-008, SM-UI-009 and, if the UI-T1 benchmark needs an index, migration 0069.
- **Q1:** the page is called **Usage**.
- **Q2:** (a) `/attention` stays as the full itemised list, off the sidebar, with the top 5 items on Overview.
- **Q3:** (A) every existing URL stays; `/configuration`, `/integrations` and `/access` are entry routes with tabs.
  UI-T7b is not needed.
- **Q4:** keep "Access"; Account leaves the sidebar (it stays in the user menu); API tokens go under Access and Nodes
  under Configuration.
- **Q5:** Codex and Harbor Pilot usage is shown as "not counted" in v0.1.16.
- **Q6:** Usage is visible to anyone with `run:read`, filtered to their spaces; account names show for accounts bound
  to those spaces.
- **Q7:** Overview shows tokens only, with a delta against the previous period.
- **Q8:** Usage offers a 90-day (daily) range.
