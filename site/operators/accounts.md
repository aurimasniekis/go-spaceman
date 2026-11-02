# Accounts

Claude Code and Codex runs need a credential. In SpaceMan a credential is an **account**: you add it once, then allow
spaces to use it. Runs never use credentials from the node's own environment or login.

<Shot name="accounts" alt="Accounts page with two Claude Code accounts, a disabled one and a Codex account, showing bound spaces and when each credential was set" caption="Accounts" />

## Credential kinds

| Kind | Harness | Where it comes from |
|---|---|---|
| Claude subscription token | Claude Code | `claude setup-token` (needs a Claude subscription) |
| Anthropic API key | Claude Code | The Anthropic console |
| OpenAI API key | Codex | The OpenAI platform |

Codex ChatGPT logins (`auth.json`) are not supported yet.

## Adding and rotating

Only administrators manage accounts. **Add account** asks for a name, the kind and the credential.

- The credential is sealed with the server's keyring when you save it. It is never shown again: not on this page, not
  in the API, the audit log or the logs.
- **Edit or rotate** changes the name, description or enabled state. Entering a new credential rotates it: runs that
  start afterwards use the new one, and running harnesses keep the one they started with.
- A **disabled** account is never chosen for a launch. **Delete** removes the account and its space bindings.

## Allowing spaces to use an account

On a space's page, **Choose accounts** lists the accounts per harness. Tick the ones the space may use and pick a
default for each harness. Binding an account hands its credential to the space's runs, so it also needs an
administrator.

<Shot name="space-accounts" alt="Choose accounts dialog: two Claude Code accounts allowed with team-claude as default, a disabled one not allowed, and team-codex as the Codex default" caption="A space's accounts" />

A launch uses, in order:

1. the account the launch names (it must be allowed for the space and enabled);
2. the space's default for the harness;
3. the space's only allowed account for the harness.

Otherwise the launch is refused with a message that says why: no account is allowed, or there are several and no
default. If an account is deleted, disabled or unbound after a launch is accepted but before the node receives it, the
run fails with that reason instead of waiting.

## How a run receives the credential

The credential leaves central only when the launch is sent to the node, over the node's authenticated connection. The
node writes it to a file only the agent can read; a small launcher reads the file, deletes it, and starts the harness
with the credential in its environment. It is never part of herdr-core's saved terminal state.

Before starting Claude Code, SpaceMan also marks the per-space configuration as set up and trusts exactly the run's
working directory, so an unattended run does not stop at the first-run screens.

::: warning Credentials are visible on the node
A running harness needs its credential. Anyone with root, or the agent's operating-system user, on that node can read
it from the process. Give nodes to people you would also give the credential to.
:::

## Verified

A real Claude Code run (2.1.316, Haiku) was started through SpaceMan with a subscription token held only as an
account: it answered, herdr-core reported the turn as done, and the token was not found in herdr-core's state, the
central database or the logs (`test/real` `TestRealClaudeRun`, opt-in).
