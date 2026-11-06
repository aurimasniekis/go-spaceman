# Contract: Harbor Pilot gateway

Pinned to **Harbor Pilot v0.1.1**, commit `68d6eae735743bfa0730a36a203ef38235c0cf4d`
(`github.com/aurimasniekis/go-harbor-pilot`). File paths below are in that repository at that revision.

**How this was established (2026-11-06):**
- The tag was cloned and built with `go build ./cmd/harbor-pilot`. `go install …/cmd/harbor-pilot@v0.1.1` was also
  checked; it needs Go ≥ 1.27.3.
- `harbor-pilot serve --demo` ran on loopback with a temporary data directory, and its HTTP API was exercised with
  curl. Real Claude Code and real codex-cli then ran against it (see `harnesses.md`, "Harbor Pilot gateway spike").
- The demo's upstreams are synthetic, so no provider credentials were involved.

**Labels:**
- **live** means observed on that demo gateway.
- **code** means read in the source only.
- Nothing here is verified against a non-demo Harbor Pilot or a real provider upstream.

The opt-in helper `internal/harborpilot/hptest` reproduces the setup (`make test-harbor-pilot`; see the end of this file).

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
Handler: `internal/gateway/models.go:210` (`mintChild`), service: `internal/controlplane/credentials.go:153` (`MintChild`).

- **Request body.** The body is exactly `{name, scopes, ttl_seconds, bound_refs}`, decoded with `DisallowUnknownFields`
  (`models.go:224`). An unknown field gets 400 `invalid body: json: unknown field "…"`. **live**
- **Response.** `201 {credential:{id:"crd_…", prefix, binding_id, name, scopes, parent_id, bound_refs, managed_by:"api",
  expires_at, created_by, created_at}, token:"hpk_…"}`. **live**
- **Caller.** The caller must hold `tokens:mint`. A child credential has no `tokens:mint`, so it cannot mint: 403
  `{"error":{"type":"permission_error","message":"… parent credential lacks tokens:mint"}}`. **live**
- **Scopes.** They must be a subset of the parent's and may not include `tokens:mint`; otherwise 400 (`credentials.go:171`). **live**
- **TTL** (`credentials.go:160-176`):
  - it defaults to 12 h when it is 0 or absent (**code**);
  - the maximum is 7 days, and more gets 400 `child credentials live at most 7 days` (**live**);
  - it is silently shortened to the parent's expiry (**code**).
- **`bound_refs`** (`internal/controlplane/specs.go:33,50`, `credentials.go:178-215`):
  - at most 32 keys, each matching `^[a-z0-9][a-z0-9._-]{0,63}$` (**code**);
  - `project_id` must be the binding's project or one of its `allowed_project_ids`. Otherwise 400 `project must be the
    client's project or one of its allowed projects` (`credentials.go:197-209`). **live**
  - A key that the binding pins in `external_refs` may be repeated only with the same value. A different value gets
    400 `cannot override a reference configured on the client` (`credentials.go:212`). **live**: the demo binding
    pins `spaceman.space=spc_synthetic`, so `spc_other` is refused and `spc_synthetic` is accepted.
  - A key bound on the parent credential cannot be overridden either (`credentials.go:184`). **code**
- **Budgets are not checked at mint.** A child credential for a project whose blocking budget was `exceeded` was minted
  normally. **live**
- **`created_by`.** Minted credentials show `created_by:"anonymous"`. **live**
- **Error shape** (gateway routes): OpenAI style `{"error":{"message","type","code","param"}}`.
  - `type` carries the class: `invalid_request_error`, `permission_error`, `invalid_api_key`, …
  - `code` was `null` on every mint and revoke error seen. **live**
- **Auth-failure throttling.** Failed credential checks are rate-limited per client address
  (`internal/gateway/gateway.go:55`, `models.go:26`). **code**: the limit constant was not exercised.

## Revoke: `DELETE /v1/harbor/credentials/{id}`
Handler: `internal/gateway/models.go:256` (`revokeChild`).

- **Success.** Returns 204, and a second DELETE of the same id returns 204 again (it is idempotent). **live**
- **Who may revoke.** The parent of the credential, or the credential itself; a child revoking itself gets 204 (**live**).
  Any other caller, or an unknown id, gets 404 `no child credential with this id is owned by the caller` (**live**).
  A revoked or invalid caller gets 401 (**code**, `authenticate` runs first).
- **After revocation.** A revoked child gets **401** on the gateway, e.g. `GET /v1/models`. **live**
- **Revoking a parent revokes all its children.** `UPDATE gateway_credentials SET revoked_at … WHERE parent_id = ?`
  (`credentials.go:274-276`). **code**

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
- **Blocking budgets.** A budget with `enforcement: block` that is exhausted gets 429. **live**
  - Anthropic shape: `{"type":"error","error":{"type":"rate_limit_error","message":"budget <name> is exhausted for this day (<spent> of <limit> USD)"}}`.
  - OpenAI shape: `{"error":{"type":"insufficient_quota","code":"insufficient_quota",…}}`.
- **Anthropic parser limit.** It accepts only the `user` and `assistant` roles in `messages[]`, and anything else gets 400
  `messages[N]: role must be user or assistant` (`internal/protocol/anthropic/request.go:242`). **live**: Claude Code
  2.1.317 with its default Claude 5 model sends a `system`-role message in `messages[]` and is refused (see `harnesses.md`).
- **Attribution.** Each request records `trusted_refs`. These are the credential's `bound_refs` merged with the binding's
  `external_refs`, e.g. `spaceman.fleet` from the binding plus `spaceman.run` from the token (**live**). The ledger keeps
  a `run_ref` (`internal/gateway/deliver.go:52,137`, **code**). Header claims from the client, such as Claude Code's
  `x-claude-code-session-id`, are recorded under `claims`, never as trusted. **live**

## Budget status: `GET /api/v1/budgets/status`
Handler: `internal/quota/quotaapi/quotaapi.go:208`. It needs an `hpa_` token or a session with `usage.read`. **live**

- **No filters.** Every budget is returned.
- **Fields of an item:** `budget_id, name, scope (global|workspace|project|binding|account), scope_id, period,
  period_start, period_end, currency, cost_kinds, enforcement (block|warn), enabled, limit_e10, spent_e10, reserved_e10,
  by_kind_e10, fraction, warn_at, projected_spend_e10, state (ok|warning|exceeded|disabled)`. **live**
- **Demo spend.** The demo's seeded traffic has spend on the `data-pipeline` project. A fresh `0.000001 USD` daily
  budget on that project is already `exceeded` right after it is created (spent 0.00206 USD). **live**
- **Workspace lookup.** Finding a project's workspace needs `config.read` (`GET /api/v1/projects`, `spec.workspace_id`). **live**

## Activity and usage
- **Request list.** `GET /api/v1/activity/requests` (`internal/telemetry/activityapi/activityapi.go:192`, `activity.read`)
  filters by binding, project, `session_id`, model, harness, time and similar. **live**: `binding_id` and `session_id`.
- **The `session_id` filter takes Harbor Pilot's internal `ses_…` id, not the bound `session` value.** Filtering on a
  bound `session` returned 0 rows; filtering on the request's `ses_…` returned its rows. **live**
- **Where `trusted_refs` appear.** They are only on the per-request detail (`GET /api/v1/activity/requests/{id}`), not
  in list rows. **live**
- **No per-run usage.** Nothing filters or groups requests or usage by `spaceman.run` or by any trusted ref.
  - (Planner's note, 2026-11-06.) `usage/summary` groups by account, provider, model, binding, project, workspace and billing.
  - Per-run usage would need an upstream change.
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
  | `budget.threshold.exceeded` (and `.warning`) | `budget_id`, plus the scope's id only (`project_id` for a project budget) | `budget, currency, enforcement, fraction, limit, period, period_start, scope, spent` | **live** |
  | `access.request.blocked` | `request_id, binding_id`, no project | `excluded, plan_id, reason, requested_model` | **live**, emitted for the routing 403 above. The budget 429 did **not** emit it. |
  | `quota.window.exhausted` / `.recovered` | `monitor_id, account_id` | `monitor, bucket, label, reset_at, source` | **code**, `catalog.go:30` |
  | `access.credential.issued` | `credential_id, binding_id` | `child, prefix, scopes` | **live** (see the mismatches below) |
- **Pulse relay not exercised.** The events were read from the API only; the relay to Pulse was not run.

## Demo seed (v0.1.1)
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
- **The demo's `spaceman-runs` binding can't be used for SpaceMan run tokens.** It pins `spaceman.space=spc_synthetic`
  and allows no other projects, so per-space tokens would be refused. Tests provision their own binding instead.
- **Claude Code bridge.** At start the demo logged that it had detected the local `claude` binary
  (`bridge.claude_code.ready`). Nothing in this contract depends on that.

## Doc/code mismatches at v0.1.1
1. **`spaceman.space` in `bound_refs`.** `site/tools/orchestrators.md:57` mints with
   `bound_refs: {"spaceman.space": …, "spaceman.run": …}` against the demo `spaceman-runs` binding. That binding pins
   `spaceman.space`, and minting any other value is refused with 400 (`credentials.go:212`, **live**). The example only
   works with the pinned value. A binding shared by many spaces must not pin `spaceman.space`.
2. **`access.credential.issued` data.** The docs list `name, scopes` (`site/reference/events.md:89`, `docs/contracts/events.md:14`,
   `internal/events/catalog.go:28`). The emitted event carries `child, prefix, scopes`. **live**
3. **`created_by`.** Minted child credentials show `created_by:"anonymous"` rather than the parent credential or binding. **live**

## Test harness: `internal/harborpilot/hptest`
- **Opt-in.** Set `SPACEMAN_HARBOR_PILOT=1`. The helper uses the binary at `HARBOR_PILOT_BIN`, or else runs
  `go install github.com/aurimasniekis/go-harbor-pilot/cmd/harbor-pilot@v0.1.1` into the user cache directory.
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
- **Status:** it passed on 2026-11-06 with both a source-built binary and the `go install` path. Without the opt-in it is skipped.
