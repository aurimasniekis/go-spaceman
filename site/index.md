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
    details: A space is a long-lived team of agents with its own goals and instructions, pinned to one machine running spaceman-agent. Nothing runs anywhere else, and moving a space to another node is an explicit, audited relocation.
  - title: Leases, not hope
    details: Every run needs a renewable execution lease. A node that loses central stops new work and ends running work after a grace period; a lease-guard terminal ends runs even if the agent daemon dies.
  - title: A coordinating control agent
    details: Each space has an inbox, an ordered execution queue, a control agent that plans and workers in their own Git worktrees, with durable messages between them and schedules that add work.
  - title: People stay in charge
    details: Agents ask people through human actions (questions, approvals, to-dos, custom types) gathered across spaces. Enforced approvals gate the operations SpaceMan performs, bound to the run and the exact arguments.
  - title: Knowledge and memory
    details: Role memory is checkpointed on the node and preserved centrally as Git history. Knowledge bases answer agents through per-requester support sessions, and dreams consolidate memory.
  - title: Events and tools
    details: Pulse subscriptions (GitHub through go-pulse-github) feed the inbox, SpaceMan publishes its own events through an outbox, and one MCP gateway mediates external tools with grants and limits.
  - title: Every terminal recorded
    details: The node records each run's terminal into compressed segments and ships them to central. Watch live (read-only) or replay with seeking and speed control.
  - title: Built to be automated
    details: A documented HTTP API (OpenAPI 3.1) with optimistic concurrency and idempotent creates. The web UI, the spaceman command line and the Terraform provider use the same API.
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

SpaceMan has not been released. The [project status](/guide/status) page lists what is implemented and how it was
tested (simulated or against the real dependency), and the [known limitations](/reference/limitations) page lists what
is missing. Codex support is implemented but has not been run against a real Codex yet.

This site is built from the repository (`make site`) and has **not been published**: the GitHub Pages workflow exists
but has never run. [Publishing the site](/development/publishing) explains how to publish it.

</div>
