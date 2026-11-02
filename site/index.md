---
layout: home

hero:
  name: SpaceMan
  text: A control plane for persistent teams of coding agents
  tagline: Run Claude Code and Codex on your own machines, in herdr-core terminals, under leases you control, with every terminal recorded and every action audited.
  image:
    src: /logo.svg
    alt: SpaceMan logo
  actions:
    - theme: brand
      text: Try it locally
      link: /guide/try-locally
    - theme: alt
      text: What is SpaceMan?
      link: /guide/what-is-spaceman
    - theme: alt
      text: Project status
      link: /guide/status

features:
  - title: Spaces pinned to your nodes
    details: A space is a long-lived team of agents with its own goals and instructions, pinned to one machine running spaceman-agent. Nothing runs anywhere else.
  - title: Leases, not hope
    details: Every run needs a renewable execution lease. A node that loses central stops new work and ends running work after a grace period; a lease-guard terminal ends runs even if the agent daemon dies.
  - title: Restarts never duplicate work
    details: Runs are identified by herdr-core terminal names and labels, so a restarted agent or central adopts what is running instead of starting it again.
  - title: Every terminal recorded
    details: The node records each run's terminal into compressed segments and ships them to central. Watch live (read-only) or replay with seeking and speed control.
  - title: Access you can explain
    details: Viewer, operator and admin roles per space or globally (Casbin), mandatory TOTP for privileged accounts, step-up for sensitive actions, scoped API tokens, a hash-chained audit log.
  - title: Built to be automated
    details: A documented HTTP API with OpenAPI 3.1, optimistic concurrency, idempotent creates and a live change feed. The web UI uses the same API.
---

<div class="sm-home">

## A look at the UI

These are real screenshots of the SpaceMan web UI on a local demo stack with fictional data and a stub harness. Switch the
site theme to see the Light Owl variants.

<div class="sm-gallery">
<Shot name="landing-overview" alt="Overview page listing three spaces, three active runs and two nodes" caption="Overview: spaces, active runs and nodes" />
<Shot name="space-overview" alt="Payments platform space with its node, a valid execution lease and three runs" caption="A space: pinned node, lease and runs" />
<Shot name="terminal-live" alt="Live watch-only terminal of a running control agent" caption="Live, watch-only terminal" />
<Shot name="terminal-replay" alt="Replay of a finished run with play, speed and seek controls" caption="Replay with seeking and speed" />
</div>

SpaceMan is under active development. The [project status](/guide/status) page lists what is implemented and tested today.

</div>
