# Contract: Harbor Pilot gateway

Pinned to **Harbor Pilot v0.1.3**, commit `2e957c71bf160660588a6468bfe7cc1ed855e3ca`
(`github.com/aurimasniekis/go-harbor-pilot`, tag `v0.1.3`, released 2026-11-07). File paths below are in that
repository at that revision; `D-0nn` are Harbor Pilot's own decisions in its `docs/DECISIONS.md`. SpaceMan's design on
top of it is ADR-0060 (Accepted) in `docs/DECISIONS.md`. **SpaceMan's minimum Harbor Pilot version is 0.1.3.**

**How this was established:**
- 2026-11-06, v0.1.1 (commit `68d6eae`): the tag was built from source, `harbor-pilot serve --demo` ran on loopback
  with a temporary data directory, its HTTP API was exercised with curl, and real Claude Code and real codex-cli ran
  against it (see `harnesses.md`, "Harbor Pilot gateway spike").
- 2026-11-07, v0.1.3: `go install …/cmd/harbor-pilot@v0.1.3` (it needs Go ≥ 1.27.3, which `GOTOOLCHAIN=auto`
  fetches). The facts below were re-checked on that binary through the `hptest` tests and by hand, and Claude Code
  ran against it again (`harnesses.md`, "Harbor Pilot 0.1.3 re-run"). Facts that only changed in the source are
  re-cited at v0.1.3.
- The demo's upstreams are synthetic, so no provider credentials were involved.

**Labels:**
- **live** means observed on that demo gateway.
- **code** means read in the source only.
- Nothing here is verified against a non-demo Harbor Pilot or a real provider upstream.

The opt-in helper `internal/harborpilot/hptest` reproduces the setup (`make test-harbor-pilot`; see the end of this file).

## What changed since v0.1.1
These answer SpaceMan's upstream requests (ADR-0060 Q5); each is detailed in its section below.
- **0.1.2:** `system`-role entries in Anthropic `messages[]` are accepted (K-13); usage, activity and sessions filter
  by trusted ref (D-041); budget refusals are recorded as blocked and emit `access.request.blocked` (D-040); minted
  children record the minting credential as `created_by` (D-040); the demo's `spaceman-runs` binding pins only
  `spaceman.fleet`; the orchestrator and event docs were fixed.
- **0.1.3:** optional `check_budget` on mint (D-043); per-model downgrade of mid-conversation `system` entries
  (D-042); `ref=` and `binding_id` filters on `/usage/series`; a generic 502 for internal mint errors.
- **Unchanged:** a mint without `check_budget` ignores budgets. The Pulse relay is still not verified against a real
  `pulsed` deployment (by Harbor Pilot or by SpaceMan); SpaceMan ran it into its Pulse-shaped test server (Events).

## Credentials and scopes
- **Gateway credentials:**
  - Tokens are `hpk_…`. Scopes are `gateway:invoke`, `gateway:models` and `tokens:mint` (`internal/controlplane/credentials.go:26`).
  - A credential belongs to one client binding.
  - An operator issues one through the admin API: `POST /api/v1/client-bindings/{id}/credentials` with `{name, scopes}`.
    The response is `201 {credential, token, base_urls}`, where `base_urls` lists `anthropic` (origin), `openai` (`<origin>/v1`) and others. **live**
- **Service-principal API tokens:**
  - Tokens are `hpa_…`, issued with `POST /api/v1/service-principals/{id}/tokens` (`internal/auth/authapi/principals.go:209`).
  - Their permissions are those of the principal, e.g. `usage.read`, `config.read`, `activity.read`.
  - They authenticate the control-plane API (`/api/v1/…`) without a session or CSRF. **live**
- **The two token kinds are not interchangeable.** An `hpk_` token on `GET /api/v1/budgets/status` gets **401**. **live**

## Mint: `POST /v1/harbor/credentials`
Handler: `internal/gateway/models.go:214` (`mintChild`), service: `internal/controlplane/credentials.go:167` (`MintChild`).

- **Request body.** The body is exactly `{name, scopes, ttl_seconds, bound_refs, check_budget}`, decoded with
  `DisallowUnknownFields` (`models.go:231`). An unknown field gets 400 `invalid body: json: unknown field "…"`. **live**
  (v0.1.1; `check_budget` added in 0.1.3, **live**).
- **Response.** `201 {credential:{id:"crd_…", prefix, binding_id, name, scopes, parent_id, bound_refs, managed_by:"api",
  expires_at, created_by, created_at}, token:"hpk_…"}`. **live**
- **Caller.** The caller must hold `tokens:mint`. A child credential has no `tokens:mint`, so it cannot mint: 403
  `{"error":{"type":"permission_error","message":"… parent credential lacks tokens:mint"}}`. **live**
- **Scopes.** They must be a subset of the parent's and may not include `tokens:mint`; otherwise 400 (`credentials.go:183`). **live**
- **TTL** (`credentials.go:167-180`):
  - it defaults to 12 h when it is 0 or absent (**code**);
  - the maximum is 7 days, and more gets 400 `child credentials live at most 7 days` (**live**);
  - it is silently shortened to the parent's expiry (**code**).
- **`bound_refs`** (`internal/controlplane/specs.go`, `credentials.go:190-255`):
  - at most 32 keys, each matching `^[a-z0-9][a-z0-9._-]{0,63}$` (**code**);
  - `project_id` must be the binding's project or one of its `allowed_project_ids`. Otherwise 400 `project must be the
    client's project or one of its allowed projects` (`credentials.go:249`). **live**
  - A key that the binding pins in `external_refs` may be repeated only with the same value. A different value gets
    400 `cannot override a reference configured on the client` (`credentials.go:253`). **live** (v0.1.1, with the
    then demo binding that pinned `spaceman.space`).
  - A key bound on the parent credential cannot be overridden either (`credentials.go:199`). **code**
- **Budgets: only with `check_budget: true`** (0.1.3, D-043; `models.go:238`, `mintBudgetCheck` `models.go:277`).
  - Without it (the default), budgets are not checked at mint: a child for a project whose blocking budget is used up
    is minted normally. **live**
  - With it, an enabled `block` budget covering the child (the parent's binding, the project the child's requests are
    attributed to, that project's workspace, or a global budget) whose current period is used up refuses the mint:
    `429 {"error":{"type":"insufficient_quota","code":"budget_exceeded","budget_id":"bdg_…","message":"budget <name> is
    exhausted for this day (<spent> of <limit> USD) (bdg_…)","param":null}}` (`writeMintBudgetError`, `models.go:343`).
    **live**
  - "Used up" is settled spend plus active reservations at or over the limit, with no estimate for the coming request.
    A budget with no spend yet passes, even when the first request is then refused for its own estimate. **live**: a
    fresh 0.000001 USD budget with zero spend let a `check_budget` mint through, and the next request got the 429.
  - Account budgets are not checked at mint. The check is skipped when the binding's plan can only reach local or free
    accounts (`budgetedRouteReachable`, `models.go:306`). **code**
  - If budgets can't be read, the mint fails with `502`, message `could not check budgets`. **code**
  - A refused mint writes no audit entry and emits no event (D-043). **code**
- **Internal errors.** A mint that fails for any other internal reason answers `502` `could not mint the credential`
  (`models.go:264`; 0.1.3). It used to be a 403 with the raw error. **code**
- **`created_by`** is the minting (parent) credential's id, `crd_…` (D-040). **live** (it was `"anonymous"` in 0.1.1).
- **Error shape** (gateway routes): OpenAI style `{"error":{"message","type","code","param"}}`.
  - `type` carries the class: `invalid_request_error`, `permission_error`, `invalid_api_key`, …
  - `code` was `null` on every mint and revoke error seen. **live**
- **Auth-failure throttling.** Failed credential checks are rate-limited per client address
  (`internal/gateway/gateway.go:55`, `models.go:25`). **code**: the limit constant was not exercised.

## Revoke: `DELETE /v1/harbor/credentials/{id}`
Handler: `internal/gateway/models.go:361` (`revokeChild`).

- **Success.** Returns 204, and a second DELETE of the same id returns 204 again (it is idempotent). **live**
- **Who may revoke.** The parent of the credential, or the credential itself; a child revoking itself gets 204 (**live**).
  Any other caller, or an unknown id, gets 404 `no child credential with this id is owned by the caller` (**live**).
  A revoked or invalid caller gets 401 (**code**, `authenticate` runs first).
- **After revocation.** A revoked child gets **401** on the gateway, e.g. `GET /v1/models`. **live**
- **Revoking a parent revokes all its children.** `UPDATE gateway_credentials SET revoked_at … WHERE parent_id = ?`
  (`internal/controlplane/credentials.go`, revoke). **code** (seen at v0.1.1; not re-read line by line at v0.1.3)

## Gateway data plane
- **Anthropic Messages.** `ANTHROPIC_BASE_URL=<origin>` makes requests go to `<origin>/v1/messages`. Both
  `Authorization: Bearer` and `x-api-key` are accepted. **live**
- **OpenAI Responses.** `<origin>/v1/responses`. **live**
- **Models.** `GET /v1/models` lists the plan's aliases. On the demo these are `coding/deep`, `coding/default`,
  `coding/fast`, `local/private` and `review/default`. **live**
- **Demo upstreams.** The demo's fake upstreams answer both protocols, so real harnesses can complete turns with no
  provider credential. **live**
- **Unrouted models are refused.** A model the plan can't route gets 403 `no eligible account is available for model …`.
  This happened, for example, with Codex 0.166.0's default model on the demo plan. **live**
- **Blocking budgets.** A request over a budget with `enforcement: block` gets 429. **live**
  - Anthropic shape: `{"type":"error","error":{"type":"rate_limit_error","message":"budget <name> is exhausted for this day (<spent> of <limit> USD)"}}`.
    There is no `code` in the Anthropic shape. **live** (0.1.3)
  - OpenAI shape: `{"error":{"type":"insufficient_quota","code":"insufficient_quota",…}}`. **live** (0.1.1)
  - Since 0.1.2 the refused request is recorded with `status: blocked`, `error_kind: budget_exceeded`, `attempts: 0`,
    and emits `access.request.blocked` (D-040; see Events). **live**
  - A request's own estimate counts: a 0.000001 USD budget with zero spend refuses the first request. **live**
- **`system`-role entries in `messages[]`** (K-13; `internal/protocol/anthropic/request.go:241-248`). Since 0.1.2 the
  roles `user`, `assistant` and `system` are accepted; anything else gets 400 `role must be user, assistant or system`.
  - To an Anthropic upstream the entry is sent unchanged, unless the target model is on a list of models known not to
    support mid-conversation system messages (`internal/protocol/anthropic/models.go:16`: Claude Sonnet 5 and models
    up to Claude Opus 4.7). For those, since 0.1.3, it is sent as user text with a `mid_conversation_system` loss
    (D-042). Unknown models keep the pass-through, so a model that doesn't support it gets the provider's own 400.
    **code**
  - **live** (2026-11-07, demo upstreams): Claude Code 2.1.317 on its default `claude-opus-5-5` sends `["user","system"]`.
    The request got 200; on the demo's Anthropic-shaped upstream (`demo-sonnet`) it was recorded with no losses, and
    on a Responses-shaped upstream (`demo-gpt`) it was translated. See `harnesses.md`. Whether a real Anthropic
    upstream accepts it is not verified (no real upstream).
- **Attribution.** Each request records `trusted_refs`. These are the credential's `bound_refs` merged with the binding's
  `external_refs`, e.g. `spaceman.fleet` from the binding plus `spaceman.run` from the token (**live**). The ledger keeps
  a `run_ref` (`internal/gateway/deliver.go:52,137`, **code**). Header claims from the client, such as Claude Code's
  `x-claude-code-session-id`, are recorded under `claims`, never as trusted. **live**
- **Request detail fields** (0.1.3, **live**) include `trusted_refs`, `claims`, `losses` (`{feature, detail,
  severity}`), `attempt_list`, `credential_id`, `harness`, `harness_version`, `final_model` and `final_account_id`.

## Budget status: `GET /api/v1/budgets/status`
Handler: `internal/quota/quotaapi/quotaapi.go:208`. It needs an `hpa_` token or a session with `usage.read`. **live**

- **No filters.** Every budget is returned.
- **Fields of an item:** `budget_id, name, scope (global|workspace|project|binding|account), scope_id, period,
  period_start, period_end, currency, cost_kinds, enforcement (block|warn), enabled, limit_e10, spent_e10, reserved_e10,
  by_kind_e10, fraction, warn_at, projected_spend_e10, state (ok|warning|exceeded|disabled)`. **live**
- **Demo spend.** In 0.1.1 the seeded traffic had spend on `data-pipeline`, so a fresh `0.000001 USD` budget there was
  `exceeded` at once. In 0.1.3 `data-pipeline` has no spend at start (the budget is `ok`, `spent_e10: 0`), while
  `web-app` has seeded spend (about 0.12 USD), so a tiny budget on `web-app` is used up at once. **live**
- **Limits.** A budget `limit` must be a positive decimal; `"0"` gets 422. **live**
- **Workspace lookup.** Finding a project's workspace needs `config.read` (`GET /api/v1/projects`, `spec.workspace_id`). **live**

## Activity and usage
- **Request list.** `GET /api/v1/activity/requests` (`internal/telemetry/activityapi/activityapi.go`, `activity.read`)
  filters by binding, project, `session_id`, model, harness, time and similar. **live**: `binding_id` and `session_id`.
- **The `session_id` filter takes Harbor Pilot's internal `ses_…` id, not the bound `session` value.** Filtering on a
  bound `session` returned 0 rows; filtering on the request's `ses_…` returned its rows. **live** (0.1.1)
- **Where `trusted_refs` appear.** They are only on the per-request detail (`GET /api/v1/activity/requests/{id}`), not
  in list rows. **live**
- **Per-run usage by trusted ref** (0.1.2, D-041):
  - `ref=<key>:<value>` (split at the first `:`, repeatable, at most 4, ANDed) filters `GET /api/v1/usage/summary`
    (`internal/accounting/usageapi/usageapi.go:675`), `GET /api/v1/activity/requests` (`activityapi.go:190`) and
    `GET /api/v1/activity/sessions` (`activityapi.go:276`); since 0.1.3 also `GET /api/v1/usage/series`. The key
    follows the bound-ref key rules and the value is 1 to 256 bytes. **code**
  - `usage/summary` also takes `binding_id`, and `group_by=ref:<key>` (rows without the key form the `""` group), with
    `cursor`/`limit` (default 50, at most 500) and `next_cursor`. `totals` cover every matching row, whatever the page.
    The response echoes `filters: {refs, binding_id}`. **code**; **live**: `ref`, `binding_id` and
    `group_by=ref:spaceman.run`.
  - **live**: two runs' child tokens with different `spaceman.run` values sent 1 and 2 requests;
    `usage/summary?ref=spaceman.run:<run>&binding_id=<binding>` reported 1 and 2 attempts respectively (per-group
    `attempts`, `tokens`, `costs`), and 0 for a run without requests. `activity/requests?ref=spaceman.run:<run>` lists
    the run's requests, but its row is written by a telemetry writer after the response, so it can lag briefly; the
    tests poll both for up to 15 s.
  - **live**: `activity/sessions?ref=session:<value>` returns the session with Harbor Pilot's `ses_…` id
    (`source: bound_ref`). The raw value is not stored on the session.
  - **Trusted refs only.** Only `trusted_refs` (mint-time `bound_refs` plus the binding's `external_refs`) are indexed,
    never claims or headers. A minting credential chooses its runs' values, so a consumer also filters on
    `binding_id` (or a key its binding pins, such as `spaceman.fleet`) to avoid counting another client's runs that
    reuse a value. SpaceMan always passes `binding_id`. **code** (D-041)
  - **Response fields SpaceMan reads** (HP-T5; `UsageSummary`, `UsageGroup`, `UsageTokens`, `CostAmount` in
    `internal/accounting/usageapi/usageapi.go:40-96`): `from`, `to`, `group_by`, `groups[] {key, label, attempts,
    tokens {input, output, cache_read, cache_write, reasoning, unknown_usage_attempts}, costs[] {kind, currency,
    amount}}`, `totals[]`. Amounts are decimal strings; cost kinds are `estimated_api_charge`, `actual_api_charge`,
    `list_equivalent`, `subscription_fee`, `allocation` and are never summed. Without `from` the range is the last 30
    days before `to` (default now), at most 400 days (`parseRange`, `usageapi.go:642`). **code**; **live**
    (`TestRealHarborPilotBudgetAndUsage`: `group_by=model` with `ref` and `binding_id`, and `group_by=project`).
  - The first start of 0.1.2 backfills the ref index from stored requests, once. **code**
  - Subscription fees ignore filters. **code**
- **Harness label.** Claude Code requests were labelled `harness: claude_code`. Codex 0.166.0 requests through a custom
  provider were labelled `generic`, even though the request carried a `session-id` header. **live**

## Events
- **Subjects and envelope** (`internal/events/catalog.go`; relay `internal/events/relay/relay.go`). The subject is
  `evt.harborpilot.<area>.<entity>.<action>` and the type is `harborpilot.<area>.<entity>_<action>.v1`. The envelope is
  `{schema_version, id, type, name, occurred_at, severity, correlation_id, subjects{…}, data{…}}`. **live**: through
  `GET /api/v1/events`.
- **Events SpaceMan cares about, with their `subjects` and `data` keys:**

  | Event | `subjects` | `data` keys | Observed |
  |---|---|---|---|
  | `budget.threshold.exceeded` (and `.warning`) | `budget_id`, plus the scope's id only (`project_id` for a project budget) | `budget, currency, enforcement, fraction, limit, period, period_start, scope, spent` | **live**; still once per period (D-027), with the refused request's id as `correlation_id` (0.1.3) |
  | `access.request.blocked` | `request_id, binding_id`, no project | `excluded, plan_id, reason, requested_model` | **live**, for the routing 403 (0.1.1) and, since 0.1.2, for every budget 429 (one per refused request, D-040). For a budget refusal `reason` is the budget message text and `excluded` is 0. |
  | `quota.window.exhausted` / `.recovered` | `monitor_id, account_id` | `monitor, bucket, label, reset_at, source` | **code**, `catalog.go:30` |
  | `access.credential.issued` | `credential_id, binding_id` | `child, prefix, scopes` | **live**; the docs say so since 0.1.2 |
- **Pulse payload** (`internal/events/relay/pulse.go`, `relay.go:336`). The relay publishes each event with the go-pulse
  client: subject as above, `ce-type` the event type, the Pulse id the event id (`WithID`), content type
  `application/json`, and the whole envelope as the payload. So a SpaceMan template reads `data.name`,
  `data.subjects.*` and `data.data.*`, and a redelivery carries the same id. **live** (2026-11-07, see below)
- **Pulse destination** (`internal/controlplane/events_specs.go:75`). `POST /api/v1/event-destinations` with
  `spec {type: "pulse", enabled, pulse_url, pulse_source (default harbor-pilot), allow_private_network, filter {events
  (name globs), min_severity, subjects}}` and the write-only secret `creds` (a `.creds` file's content; `creds_ref`
  `file:` is the alternative). Loopback or private servers need `allow_private_network`. **live**
- **A new destination starts at the outbox head** (`relay.go:168` `Cursor`): the relay creates its cursor the first
  time it sees the destination (it polls every second), and earlier events are never sent there, unless replayed
  (`POST /api/v1/event-destinations/{id}/replay`). **live**: events emitted within about two seconds of creating a
  destination were not delivered.
- **Relay exercised live, but not against a real `pulsed`.** `internal/events` `TestRealHarborPilotEventsThroughPulse`
  (opt-in, 2026-11-07) points a real `serve --demo` v0.1.3 at SpaceMan's Pulse-shaped JetStream test server
  (`pulsetest`, the server devpulse runs: JWT operator mode, the `EVENTS` stream on `evt.>`), and the relay's events
  reach SpaceMan subscriptions. Harbor Pilot 0.1.3 itself still says its relay is not verified against a real go-pulse
  deployment, and neither is it here.
- **Values the templates rely on.** Ids are `<prefix>_<26 lowercase ULID characters>` (`internal/platform/ids`:
  `bdg`, `prj`, `wsp`, `cbn`, `acc`, `qmn`, `req`, `pln`, `evt`, …). Resource names, including budget and monitor
  names, match `[a-z0-9][a-z0-9._-]{0,62}` (`internal/controlplane/kind.go:63`; **live**: a name with spaces gets 422).
  A budget's `spent` and `limit` are decimal strings and `fraction` a number. `access.request.blocked`'s
  `requested_model` is whatever the client sent (**live**: a model name with spaces and `<script>` is echoed) and
  `reason` is free text. **live** + **code**
- **What the demo emits.** Budget warnings and exhaustion (once per budget period; a request that crosses both at once
  emits only `exceeded`), `access.request.blocked` for budget refusals, and `quota.monitor.auth_failed` for its expired
  monitor. **No `quota.window.exhausted` or `.recovered`**: the demo's quota buckets are not global, and only global,
  balance and budget buckets count (`internal/quota/service.go:364`). Those two events are known from code only
  (`quota/service.go:314-329`: `subjects {monitor_id, account_id when linked}`, `data {monitor, bucket, label,
  reset_at, source}` and `{monitor, bucket, label}`).

## Demo seed (v0.1.3)
- **Fixed names, new ids.** The names are fixed, but every id (ULID) is new on each start. Look ids up by name:
  - workspace `acme`;
  - projects `web-app` and `data-pipeline`;
  - routing plans `team-default` and `private-only`;
  - bindings `claude-code-laptop`, `codex-ci`, `spaceman-runs` and `private-notebook`;
  - budgets `acme-monthly` (workspace, warn) and `ci-daily` (binding, block);
  - service principal `demo-terraform`.
- **Admin login.** The demo administrator is `admin@example.test` with the public password from `internal/demo/demo.go:17-18`.
  Sign-in is `POST /api/v1/session`, which returns `csrf_token`. Writes need `X-CSRF-Token` and an `Origin` equal to the
  public URL. **live**
- **The demo's `spaceman-runs` binding** pins only `spaceman.fleet=fleet_synthetic` since 0.1.2 and may charge any
  demo project (`internal/demo/traffic.go:158-160`), so it could carry SpaceMan run tokens now. hptest still
  provisions its own binding, so its tests don't depend on the seed's fixture or traffic. **code**
- **Demo routing.** The demo's accounts go in and out of rate-limit cooldown (seeded traffic), so the same alias can be
  served by an Anthropic-shaped account (`demo-sonnet`) right after start and by a Responses-shaped one (`demo-gpt`)
  later. Tests must not depend on which account serves a request. **live**
- **Claude Code bridge.** At start the demo logged that it had detected the local `claude` binary
  (`bridge.claude_code.ready`). Nothing in this contract depends on that.

## Doc/code mismatches
The three recorded at v0.1.1 are fixed in 0.1.2:
1. **`spaceman.space` in `bound_refs`.** The demo `spaceman-runs` binding no longer pins `spaceman.space`, so
   `site/tools/orchestrators.md`'s example mint works, and the page says to pin only what every run shares. **code**
2. **`access.credential.issued` data.** `site/reference/events.md:89` and `internal/events/catalog.go:28` now list
   `prefix, scopes, child`, as emitted. **code** + **live**
3. **`created_by`.** It is the minting credential (D-040). **live**

None are known at v0.1.3.

## What SpaceMan's connection validation relies on (HP-T2)
Checked 2026-11-07 against v0.1.3 by `TestRealHarborPilotConnection` (opt-in) and in the source.
- **Version probe.** The mint body is decoded with unknown fields refused *before* any scope check
  (`internal/gateway/models.go:230-235`, **code**). So a mint that sends `"check_budget": false` is refused by any
  gateway before 0.1.3 with 400 `invalid body: json: unknown field "check_budget"`, whatever the token's scopes, and is
  accepted by 0.1.3 (**live**). There is no version endpoint (see HP-T1b).
- **A parent without `tokens:mint`** gets 403 `permission_error` with the message `invalid, expired or revoked gateway
  credential: parent credential lacks tokens:mint` (`credentials.go:168-170`, wrapped `ErrCredentialInvalid`).
  **live**, with a credential issued on the test binding with only `gateway:invoke` and `gateway:models`. A child used
  as the parent gets 403 `child credentials cannot mint further credentials` (**code**).
- **Parent expiry from a probe.** A child's TTL is shortened to its parent's expiry (`credentials.go:188-190`), so a
  probe minted with the maximum TTL (604800 s) reveals the parent's expiry in its `expires_at`. **live** for a parent
  without expiry (`expires_at` = now + 7 days); the clamp itself is **code**.
- **Projects by id or name.** `GET /api/v1/projects/{id}` takes an id *or a name* (`internal/controlplane/cpapi/cpapi.go:137-139`)
  and needs `config.read`; a missing project is 404 problem+json. **live**: `web-app` returned its `prj_…` id. A child's
  `bound_refs.project_id` must be the id: a name is refused with 400 `project must be the client's project or one of
  its allowed projects` (**live**).
- **Listing a binding's children.** `GET /api/v1/client-bindings/{id}/credentials?children=true` (`config.read`;
  `cpapi/extra.go:152-178`) lists child credentials with `parent_id`, `expires_at` and `revoked_at`. **live** with the
  service-principal token; the test uses it to show the probe credentials were revoked.

## What SpaceMan's run-token lifecycle relies on (HP-T3)
Checked 2026-11-07 against v0.1.3 by `internal/app` `TestRealHarborPilotRunToken` (opt-in, `make test-harbor-pilot`).
- **Mint at dispatch.** A child minted with `bound_refs` {`spaceman.space`, `spaceman.run`, `spaceman.instance`,
  `session`, `project_id`} (plus `spaceman.task` when the run has one) and scopes `gateway:invoke`, `gateway:models`
  answers `GET /v1/models` with 200 (**live**). The binding provisioned by hptest pins only `spaceman.fleet`, so these
  keys are accepted.
- **Reading a credential back.** `GET /api/v1/client-bindings/{binding}/credentials/{id}` (`config.read`;
  `cpapi/extra.go`, op `get-gateway-credential`) returns its metadata with `bound_refs`, `parent_id` and `scopes`, never
  its token (**live** with the service-principal token). The test uses it to check the bound refs.
- **Revoke at the end.** After the run's end and the revoker's `DELETE /v1/harbor/credentials/{id}` with the parent,
  the child gets 401 on `/v1/models` (**live**).
- **Revoking the parent.** `DELETE /api/v1/client-bindings/{binding}/credentials/{parent id}` (admin session,
  `tokens.manage`; op `revoke-gateway-credential`, "Revoke a gateway credential and its children") revokes the parent.
  A later mint with it gets 401 `invalid_api_key`, which SpaceMan reports as "the token is invalid, expired or revoked"
  and fails the run as `gateway_unavailable` (**live**).
- **A new parent can't revoke the old one's children.** Revoke answers 404 for a credential not owned by the caller
  (see Revoke above; **code**). So when the parent is replaced, SpaceMan revokes with the old parent everything it
  minted that may still be valid, and it treats a 404 or 401 from any parent other than the minting one as a failure.
- **Bound refs SpaceMan sends** (ADR-0060 decision 5):

  | Key | Value |
  |---|---|
  | `spaceman.space` | the space ID |
  | `spaceman.run` | the run ID |
  | `spaceman.instance` | the run's instance ID |
  | `spaceman.task` | the task the run is assigned to (left out when it has none) |
  | `session` | a fresh Claude Code run: the session ID central chose. A resumed run: the session it resumes. **Any other run, a fresh Codex run for example: the run ID.** |
  | `project_id` | the space's Harbor Pilot project |

## Test harness: `internal/harborpilot/hptest`
- **Opt-in.** Set `SPACEMAN_HARBOR_PILOT=1`. The helper uses the binary at `HARBOR_PILOT_BIN`, or else runs
  `go install github.com/aurimasniekis/go-harbor-pilot/cmd/harbor-pilot@v0.1.3` into the user cache directory
  (`<cache>/spaceman/hptest/v0.1.3`). A cached binary is used only when its build information names that module and
  version.
- **Setup.** `Start(t)` runs `serve --demo --listen 127.0.0.1:<free port> --data-dir <t.TempDir()>/data` and waits for
  `/readyz`. It then provisions the following through the admin API:
  - binding `spaceman-test` on the `team-default` plan: project `web-app`, `allowed_project_ids` [`web-app`,
    `data-pipeline`], `external_refs` {`spaceman.fleet`: `test`} (not `spaceman.space`);
  - budget `spaceman-test-tiny`: project `data-pipeline`, daily, 0.000001 USD, `block`;
  - service principal `spaceman-test-reader` (`usage.read`, `config.read`, `activity.read`) and its `hpa_` token;
  - parent credential `spaceman-parent` (`gateway:invoke`, `gateway:models`, `tokens:mint`).
- **Teardown.** The cleanup sends SIGTERM to the process (and kills it after 10 s), then the temporary directory is removed.
- **Self-test.** `TestRealHarborPilotSelfTest` checks:
  - mint with bound refs, then `/v1/models` 200;
  - revoke 204 twice, then `/v1/models` 401;
  - a mint for a project outside `allowed_project_ids` gets 400;
  - the service token reads budget status.
- **Run accounting.** `TestRealHarborPilotRunAccounting` checks:
  - per-run usage: two runs' tokens (1 and 2 requests) get separate totals from `usage/summary?ref=spaceman.run:<run>`
    with `binding_id`, and a run without requests gets 0;
  - a request over the tiny budget gets 429, is listed by `activity/requests?ref=…` as `blocked`/`budget_exceeded`,
    and has an `access.request.blocked` event;
  - after that spend, a tiny budget on `web-app` (`CreateBudget`) refuses a `check_budget` mint with 429
    `budget_exceeded` naming the budget, while a mint without `check_budget` still succeeds.
- **Connection checks.** `internal/harborpilot` `TestRealHarborPilotConnection` (same opt-in) validates SpaceMan's
  connection and space settings against the demo; see "What SpaceMan's connection validation relies on". It uses
  `IssueCredential` (a parent without `tokens:mint`) and `ActiveChildren` (no probe credential left active).
- **Status:**
  - v0.1.1: the self-test passed on 2026-11-06 with both a source-built binary and the `go install` path.
  - v0.1.3: both tests passed on 2026-11-07 with the `go install` binary, and `TestRealHarborPilotConnection` too.
  - Without the opt-in both are skipped.
