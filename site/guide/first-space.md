# First node and space

This guide continues from [Try it locally](/guide/try-locally) or a real [installation](/operators/install), with one
connected node.

## Nodes

The **Nodes** page shows every node, whether it is connected, the agent version, its herdr-core core and which harnesses
it found. A node that was created but never enrolled stays **pending**.

<Shot name="setup-node-enroll" alt="Nodes page with build-01 connected and gpu-02 pending" caption="One connected node, one waiting for enrollment" />

Enrollment tokens are single use and expire. **New enrollment token** in a node's menu issues another; **Disable** keeps a
node from connecting, and **Revoke credential** ends its access for good.

## Create a space

In **Spaces → New space**, give the space a name, a slug and the node it is pinned to. Goals and standing instructions are
given to every agent in the space. The disconnect policy decides what the node does with running work when it loses
central beyond the grace period: **interrupt** (keep the session resumable) or **stop**.

<Shot name="space-create" alt="New space dialog with name, slug, pinned node, goals and standing instructions" caption="Creating a space pinned to build-01" />

Once the node renews its next heartbeat, the space shows a valid **execution lease**.

<Shot name="space-overview" alt="Space page with node, valid lease generation 1, and three runs" caption="A space with its lease and runs" />

## Give the space an account

Runs need a credential for their harness. An administrator adds it once under **Accounts** (a Claude subscription
token from `claude setup-token`, an Anthropic API key or an OpenAI API key), then uses **Choose accounts** on the space
page to allow it for the space. See [Accounts](/operators/accounts).

## Launch a run

**Launch run** starts one harness session on the pinned node. Pick a configured **space role**, or the harness (the
dialog says if the node did not detect it), the run kind and role, and optionally an account (the space's default is used otherwise), a model, an
initial prompt, extra system prompt text, a workspace folder inside the space's working directory, and a **Git
worktree of** a repository with its **Branch**. A space has at
most one control run.

<Shot name="run-launch" alt="Launch run dialog with harness, kind, role, model and prompt fields" caption="Launching a run" />

The run page opens on the live terminal. **Stop** interrupts the session (it stays resumable) or stops the program;
**Message the agent** delivers text the next time the agent is idle. See [Terminals and recordings](/operators/terminals).

## Pause a space

**Pause** has three modes: **Hold** admits no new work and lets running agents finish, **Interrupt** also interrupts
running agents and stops them, and **Emergency stop** also kills every run at once. **Resume** admits work again.

## Archive, restore and purge

A space you no longer use can be **archived**: it admits no work and leaves the Overview list. Archiving needs no
active runs. **Restore** makes an archived space active again. **Purge** permanently deletes an archived space's
SpaceMan records (runs, commands, its lease and grants). Purge never touches upstream repositories, and the folder on
the node is left in place.

In the web UI, the space's **More actions** menu (on its overview and on each row of **Spaces**) has **Archive…**,
**Restore…** and **Purge…**. Each asks for confirmation; purge also asks you to type the space's slug, is offered only
for an archived space, needs global administration (`space:purge`) and a fresh sign-in with your second factor.
**Spaces** lists active and paused spaces by default; choose **Archived** to find one to restore or purge. Archiving
is refused while runs are active: pause the space with **Interrupt** or **Emergency stop** first.

<Shot name="space-purge" alt="The purge confirmation dialog of an archived space, with the space's slug typed to confirm" caption="Purging an archived space asks for its slug" />

The same operations from the command line or the API:

```sh
spaceman space archive payments
spaceman space restore payments
spaceman space purge payments      # only an archived space
```

The API equivalents are `POST /api/v1/spaces/{space_id}/state` with `{"state": "archived"}` or `{"state": "active"}`,
and `DELETE /api/v1/spaces/{space_id}` ([CLI reference](/reference/cli/space), [HTTP API](/reference/api)).
