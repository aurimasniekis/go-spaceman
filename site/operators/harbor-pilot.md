# Harbor Pilot gateway

[Harbor Pilot](https://aurimasniekis.github.io/go-harbor-pilot/) is a self-hosted LLM gateway with failover, hard
per-project spend limits and usage for every harness. SpaceMan can route a space's runs through it: each run then gets
its own short-lived Harbor Pilot token instead of an [account](./accounts), revoked when the run ends.

::: warning Work in progress
This page covers the connection, the per-space settings, how central mints and revokes run tokens, and events. Handing the
token to the harness, and the budget status on the Limits page, come in later releases: until then a run routed through
Harbor Pilot starts without model credentials, so don't opt a space in yet.
The design is ADR-0060 (Proposed) in [Decisions](../security/decisions).
:::

SpaceMan needs **Harbor Pilot 0.1.3 or later**.

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
  check projects and, later, to read budget status and per-run usage.
- **Allow plain HTTP**: for tests and trusted local networks only. Every token then crosses the network in clear text,
  and the page says so.

<Shot name="harbor-pilot-connect" alt="Connect Harbor Pilot dialog with the gateway URL, an optional URL for runs, the parent and service-principal tokens as password fields, and the plain HTTP switch" caption="Connecting Harbor Pilot" />

Saving checks the connection with Harbor Pilot before anything is stored:

- the parent token mints a probe token, which is revoked at once. This shows that it can mint (otherwise: "parent
  token lacks tokens:mint"), which client binding it belongs to, and when it expires;
- a gateway older than 0.1.3 is refused;
- a service-principal token must be able to read budget status and projects.

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
- **Replacing the parent token revokes the tokens of running runs** with the old parent first (the new parent can't
  revoke them). Those runs then have no model access until they are started again, and the page says how many.

## Events

Harbor Pilot can publish its events to Pulse: add a Pulse event destination in Harbor Pilot (its Pulse URL, a
`.creds` file allowed to publish `evt.harborpilot.>`, and the event names to send, for example `budget.*`, `quota.*`
and `access.request.blocked`). Then subscribe spaces with the built-in templates `harbor-pilot-budget`,
`harbor-pilot-quota` and `harbor-pilot-access-blocked` ([Events](../guide/events.md#built-in-harbor-pilot-templates)).
Give each space's budget subscription the same project id as its settings above. Quota events, blocked requests and
budgets that aren't per project reach only spaces that subscribe to them. A destination only sends events from after
Harbor Pilot picked it up.

## Verified

The connection checks and the run-token lifecycle were run against a real `harbor-pilot serve --demo` 0.1.3 (with
synthetic upstreams): a parent without `tokens:mint`, project checks, a run's token answering `/v1/models` with the
expected references, the token refused after the run ended, and a revoked parent failing the next run as
`gateway_unavailable`. The node side is scripted in that test; harnesses don't receive the token yet. The event
templates were tested on events captured from the same demo and with its own relay publishing into SpaceMan's
Pulse-shaped test server, not a production Pulse.
