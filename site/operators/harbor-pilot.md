# Harbor Pilot gateway

[Harbor Pilot](https://aurimasniekis.github.io/go-harbor-pilot/) is a self-hosted LLM gateway with failover, hard
per-project spend limits and usage for every harness. SpaceMan can route a space's runs through it: each run then gets
its own short-lived Harbor Pilot token instead of an [account](./accounts), revoked when the run ends.

::: warning Before you opt a space in
Runs routed through Harbor Pilot need node agents at protocol minor 9 (this release): central refuses to send one to
an older agent, which fails it as `agent_too_old` before any token is minted, so it never starts on the node's own
login. Update every node of a space before opting the space in. There is no fallback to a direct account: while Harbor
Pilot is down, the space's routed harnesses can't run. Everything here was verified against Harbor Pilot's demo
gateway with synthetic upstreams, not yet with a real model provider (see [Verified](#verified)). The design is
ADR-0060 (Accepted) in [Decisions](../security/decisions); Terraform support is in the provider's next release.
:::

SpaceMan needs **Harbor Pilot 0.1.3 or later; 0.1.4 or later is recommended**. With 0.1.4, SpaceMan reads the
gateway's version (shown on the connection page) and reads only the budgets that cover each space instead of every
budget. With 0.1.3 the page shows "older than 0.1.4" and everything else works as before.

## Connecting Harbor Pilot

One installation has one connection. Under **Integrations → Harbor Pilot**, an administrator enters:

- **Gateway URL**: the address SpaceMan calls, for example `https://harbor-pilot.example.com`. It needs https, except
  for a loopback address.
- **URL runs use** (optional): when runs reach the gateway on a different address than SpaceMan does. Runs send their
  tokens to it, so it needs https even on loopback (a loopback address on central is another machine's loopback for a
  node).
- **Parent token**: a Harbor Pilot gateway credential (`hpk_…`) with `gateway:invoke`, `gateway:models` and
  `tokens:mint`, issued on the client binding SpaceMan's runs should use. SpaceMan uses it to mint run tokens.
- **Service-principal token** (optional): an `hpa_…` token with `usage.read` and `config.read`. SpaceMan uses it to
  check projects, read budget status for the Limits page and early launch refusals, read back per-run usage, and read
  the quota board to estimate when runs waiting on exhausted accounts can resume
  ([Runs through Harbor Pilot](../guide/usage-limits.md#runs-through-harbor-pilot)). Without it, budget status is unavailable, runs' usage isn't read
  back, and gateway waits have no estimated reset.
- **Allow plain HTTP**: for tests and trusted local networks only. Every token then crosses the network in clear text,
  and the page says so.

<Shot name="harbor-pilot-connect" alt="Connect Harbor Pilot dialog with the gateway URL, an optional URL for runs, the parent and service-principal tokens as password fields, and the plain HTTP switch" caption="Connecting Harbor Pilot" />

Saving checks the connection with Harbor Pilot before anything is stored:

- the parent token mints a probe token, which is revoked at once. This shows that it can mint (otherwise: "parent
  token lacks tokens:mint"), which client binding it belongs to, and when it expires;
- a gateway older than 0.1.3 is refused;
- a service-principal token must be able to read budget status and projects.

With a service-principal token the page also shows **Harbor Pilot version**, read from Harbor Pilot's
`GET /api/v1/system` (0.1.4 or later; reused for 10 minutes). The CLI shows it with `spaceman harbor-pilot version`.

Both tokens are sealed with the server's keyring and never shown again: not on this page, in the API, the audit log,
the change feed, exports or logs. The page shows only whether each is set, how many times it was replaced, the
binding and the parent's expiry. To replace a token, enter the new one; leave a field empty to keep the current token.

Stored tokens are only ever sent to the address they were entered for. Changing the gateway URL or the URL runs use, or
turning on plain HTTP, needs the parent token entered again (and the service-principal token, or removing it, when one
is set); otherwise SpaceMan refuses the change before calling anyone: "re-enter the tokens when the gateway URL
changes". Changing or removing the connection also asks you to confirm your sign-in (a step-up), and needs
`account:write`.

SpaceMan calls the gateway without a proxy and never on link-local (cloud metadata), unspecified, multicast or
broadcast addresses, including those reached through NAT64 or 6to4. Private and loopback addresses are allowed, since
a gateway usually runs on an internal network.

<Shot name="harbor-pilot" alt="Harbor Pilot page: the gateway URL, the parent and service-principal tokens shown only as set with their versions, the client binding, and the space using it" caption="The connection, without its tokens" />

Harbor Pilot shortens a run token's lifetime to its parent's expiry. When the parent expires before a space's run
tokens would, the connection and that space say so.

The connection can't be removed while a space uses it, or while unexpired tokens (run or validation) are live or
waiting to be revoked: revoking them needs the parent token.

## Opting a space in

On a space's overview, the **Harbor Pilot** card shows whether the space routes runs through Harbor Pilot. A space
operator sets everything but the project:

- **Project**: the Harbor Pilot project id (`prj_…`) the runs are charged to. It must be the binding's project or one
  of its allowed projects; SpaceMan checks this with Harbor Pilot, and asks for the id when you enter a name. The
  project decides who pays, so only an administrator (`account:write`) sets or changes it, as for binding an account
  to a space; for others the field is read-only. Saving without a project keeps the one already set.
- **Harnesses**: which harnesses go through Harbor Pilot (any harness it can serve; by default Claude Code and Codex).
  The others keep the space's accounts. For a listed harness there is no fallback to an account.
- **Run token lifetime**: 24 hours by default, between 15 minutes and 7 days. A run that outlives its token loses model
  access, because a running harness can't be given a new one.
- **When budget status can't be read**: launch anyway (the default; Harbor Pilot still enforces its budgets) or refuse
  the launch.
- **Allowed models**: optional model aliases runs may use. With a list, a launch must name one of them (the role's
  model); a launch without a model is refused.

<Shot name="space-harbor-pilot" alt="Harbor Pilot settings for a space: routing enabled, the project id, Codex checked, a 12 hour token lifetime and the budget fallback" caption="A space opting in" />

## Run tokens

For a harness the space routes through Harbor Pilot:

- **No account, ever.** A launch that names an account is refused, and the run never falls back to one, not even when
  Harbor Pilot is down. Without a connection, the launch is refused before it starts. The run's manifest records the
  gateway and the project.
- **One token per run, minted when its launch is sent to the node.** It can call models (`gateway:invoke`,
  `gateway:models`) but not mint, lives for the space's run token lifetime, and carries the space, run, instance,
  session and project (and the run's task) as trusted references, which Harbor Pilot records on every request. A launch
  sent again (a node reconnecting) gets the same token. If Harbor Pilot refuses the mint, the run fails as
  `gateway_unavailable` with Harbor Pilot's reason, for example "parent token lacks tokens:mint"; network errors and
  server errors are retried three times first.
- **Revoked when the run ends**, whichever way: it exits or fails, it is stopped or the space paused, its launch can't
  be sent, the space is relocated or purged, or its node stays away past the lease. The revocation is recorded in the
  same step as the end, so a restart can't lose it, and SpaceMan retries until the token expires.
- **Handed to the harness only through the launcher's secrets file**, which the launcher reads and deletes before it
  starts the harness. It never appears in the terminal's saved state, the node's records or logs, and the harness gets
  no other model credential, even when the space has accounts bound:
  - **Claude Code** gets `ANTHROPIC_BASE_URL` (the gateway) and `ANTHROPIC_AUTH_TOKEN` (the run's token). A profile
    whose Claude Code settings add another credential or endpoint (`apiKeyHelper`, `proxyAuthHelper`, the cloud
    credential helpers, `forceLoginMethod`, or `env` entries named `ANTHROPIC_*`, `CLAUDE_CODE_OAUTH_*` or
    `CLAUDE_CODE_USE_*`) fails the run with that reason. A Claude login stored in the instance's config folder is removed.
  - **Codex** is pointed at the gateway's Responses API as a custom model provider, with the token in
    `HARBOR_PILOT_TOKEN`. No `auth.json` is written, and one left from an earlier API-key or ChatGPT login is removed.
    Codex's own default model may not be one your Harbor Pilot plan routes: give the role a model the plan routes (for
    the demo, `coding/default`), or list the allowed models in the space settings.
  - The node checks the gateway address itself before it hands over a token: https, unless the connection allows plain
    HTTP.
- **Replacing the parent token revokes the tokens of running runs** with the old parent first (the new parent can't
  revoke them). Those runs then have no model access until they are started again, and the page says how many.
- **A token can't be refreshed inside a run.** A run that outlives the space's token lifetime loses model access and
  must be started again; choose the lifetime for your longest runs.

### Emergency stop

To cut every gateway run's model access at once, **revoke the parent token in Harbor Pilot**. Harbor Pilot revokes
every run token minted from it, so running runs are refused on their next request, whatever SpaceMan is doing (even
with central down). New launches then fail as `gateway_unavailable` and never fall back to an account. To stop one
run, stop it in SpaceMan, or revoke its credential in Harbor Pilot (its references name the space and run). To resume,
create a new parent in Harbor Pilot, save it on the connection, and start the runs again.

## Budgets and usage

Harbor Pilot's budgets are **hard limits**: Harbor Pilot refuses a request over a budget enforced with `block`. SpaceMan
reads their status with the connection's service-principal token (`usage.read`, and `config.read` for workspace
budgets) and uses it in three places:

- **The Limits page** of a space that routes runs through Harbor Pilot has a **Harbor Pilot budgets** card. It lists the
  budgets that cover the space: its project's, its project's workspace's, the connection's client binding's, and
  global ones (budgets on an upstream account are left out). Each shows what it covers, whether it is a hard limit or
  only warns, its spend of its limit (and what is left), its state and when it resets. A **one-time** budget (Harbor
  Pilot 0.1.4) never resets: the card shows **Never**, or when its window ends. One that hasn't started is
  **Scheduled**, one whose window ended is **Ended**; like disabled budgets, they never refuse a launch. The card's state is **within budgets**, **warning**
  (a budget passed its warning level, or a warn-only one is over), **budget used up: launches refused** (a hard-limit
  budget is used up) or **status unavailable** (with the reason). Below it, the **project's usage** in the budget's
  current period: requests, tokens and Harbor Pilot's cost. It counts **all clients** of the project, not only this
  space's runs.
- **Launches are refused early.** When a covering hard-limit budget is used up, launching a routed harness is refused at
  once with the budget's name, its spend and when it resets, for example: `Harbor Pilot budget "payments-daily"
  (project prj_payments) is used up for this day: 25.42 of 25.00 USD spent; it resets at 2026-11-09T00:00:00Z`. When
  control's automatic start is refused this way, the space gets a **Control could not start** action with that reason.
  A used-up one-time budget says it never resets (a person has to raise its limit, start it over or disable it in
  Harbor Pilot). Warn-only, disabled, scheduled and ended budgets never refuse.
- **When the run's token is minted**, SpaceMan asks Harbor Pilot to check its budgets too (`check_budget`, Harbor
  Pilot's own figures). A refusal fails the run as `gateway_unavailable` with Harbor Pilot's reason before the harness
  starts.

When budget status can't be read (no service-principal token, or Harbor Pilot failing), the space's **When budget
status can't be read** setting decides: launches proceed (the default; Harbor Pilot still checks at the mint and on
every request) or are refused, saying why. SpaceMan reads budget status at most every 30 seconds, so a budget used up a
moment ago can still let a launch through; Harbor Pilot then refuses its mint or requests.

<Shot name="space-limits-harbor-pilot" alt="A space's Limits page with the Harbor Pilot budgets card: the project's daily budget used up and launches refused, a warn-only workspace budget and the client binding's budget within limits, and the project's usage today over all clients" caption="Harbor Pilot budgets on a space's Limits page" />

<Shot name="space-limits-harbor-pilot-warning" alt="The Harbor Pilot budgets card in the warning state: the project's daily hard limit near its limit and the workspace's warn-only budget over its own, with no launches refused" caption="A warning: a budget near its limit, or a warn-only one over it" />

<Shot name="space-limits-harbor-pilot-unavailable" alt="The Harbor Pilot budgets card when budget status can't be read: the reason from Harbor Pilot and what launches do meanwhile" caption="Budget status unavailable" />

**Per-run usage.** After a routed run ends, SpaceMan reads what Harbor Pilot recorded for that run (requests, tokens per
model and Harbor Pilot's cost), filtered on the run and the connection's client binding. It reads three times, about
15 seconds, 2 minutes and 10 minutes after the end, because Harbor Pilot records a request when it finishes; the last
read is final. The figures are shown on the run's usage as **gateway-observed**. They become the run's usage (source
`harbor-pilot`) only when the harness reported none, as for Codex; Claude Code's own transcript figures are kept and
the gateway's are shown beside them. Without a service-principal token, nothing is read and the run's usage stays
unknown, never zero. A read that finds no request for the run leaves its usage unknown too.

## Events

Harbor Pilot can publish its events to Pulse: add a Pulse event destination in Harbor Pilot (its Pulse URL, a
`.creds` file allowed to publish `evt.harborpilot.>`, and the event names to send, for example `budget.*`, `quota.*`
and `access.request.blocked`). Then subscribe spaces with the built-in templates `harbor-pilot-budget`,
`harbor-pilot-quota` and `harbor-pilot-access-blocked` ([Events](../guide/events.md#built-in-harbor-pilot-templates)).
Give each space's budget subscription the same project id as its settings above. Quota events, blocked requests and
budgets that aren't per project reach only spaces that subscribe to them. A destination only sends events from after
Harbor Pilot picked it up.

After upgrading Harbor Pilot to 0.1.4, a budget that already warned or was used up in its current period sends that
alert once more (Harbor Pilot's upgrade note). It becomes one more inbox item, in the same thread as the first; a
redelivered event never does. Raising a budget's limit lets its alerts fire again in the same period too.

## Verified

Against a real `harbor-pilot serve --demo` 0.1.3, and the budget, connection, run-token, usage and event checks again
against 0.1.4 (with synthetic upstreams):

- the connection checks: a parent without `tokens:mint`, project checks;
- the run-token lifecycle with a scripted node: a run's token answering `/v1/models` with the expected references, the
  token refused after the run ended, and the emergency stop: revoking the parent refused a live run's token at once and
  failed the next run as `gateway_unavailable`;
- budgets and usage: a tiny hard-limit budget used up by the project's traffic shows as used up with its spend, the
  launch is refused naming it, and Harbor Pilot refuses the run's token mint; a run's two requests read back as its
  usage with source `harbor-pilot`;
- 0.1.4: the version read from `GET /api/v1/system`, budget status read by project, and one-time budgets: scheduled,
  ended and disabled used-up budgets show their state and never refuse a launch or the token mint, and a used-up
  one-time budget without an end refuses the launch saying it never resets;
- real Claude Code 2.1.317 and real Codex 0.166.0 runs through SpaceMan (`make test-real-harbor-pilot`): each harness
  started with only the gateway's variables, Harbor Pilot recorded each request with the client binding and the space,
  run and session references, the run's two turns were grouped in one Harbor Pilot session, no token was stored at
  rest, and the token was refused (401) once the run was stopped.

- Terraform: the provider's acceptance tests, the combined Harbor Pilot and SpaceMan example included, against the same
  demo gateway.

A real model provider behind Harbor Pilot was not used. The event templates were tested on events captured from the
same demo and with its own relay publishing into SpaceMan's Pulse-shaped test server, not a production Pulse.
