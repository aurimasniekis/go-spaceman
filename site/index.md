---
layout: home

hero:
  name: SpaceMan
  text: Run Claude Code and Codex on your own machines
  tagline: Self-hosted orchestration for teams of coding agents in herdr-core terminals. Plan and queue their work, keep people in the loop, and see everything they do.
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
  - title: Self-hosted by design
    details: Agents run only on machines you enroll. Each space is pinned to one node and moves only through an explicit, audited relocation.
  - title: Safe when things fail
    details: Every run holds a renewable lease. If a node loses contact with central, its work stops within a bounded time, even when the agent daemon crashes.
  - title: Coordinated teams
    details: A control agent plans from each space's inbox and queue and hands tasks to workers, each in its own Git worktree.
  - title: People in the loop
    details: Agents ask questions and request approvals that people answer from one place. An approval covers the exact operation it was asked for.
  - title: Memory that lasts
    details: Each role keeps Git-backed memory that survives restarts and relocation. Knowledge bases answer questions with cited evidence.
  - title: Connected to your tools
    details: Subscribe spaces to GitHub and other events through Pulse, and give agents MCP tools through one gateway with per-role access.
  - title: Full visibility
    details: Every terminal is recorded for live viewing and replay, and every change lands in a tamper-evident audit log.
  - title: Built for automation
    details: One OpenAPI 3.1 API powers the web UI, the spaceman CLI and the Terraform provider.
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
