# Managed Claude Code

A node can run the Claude Code you installed yourself, or download and manage Claude Code itself. Managed mode is off
by default; turn it on per node with `harness.claude.manage`. The design is ADR-0077.

## What changes either way

Every Claude Code run gets `DISABLE_AUTOUPDATER=1` and `DISABLE_UPDATES=1` in its environment, so a run never
updates itself into the agent's `HOME` (about 340 MB per version) and an agent can't run `claude update` from its shell
("Updates are disabled by your administrator"). The node decides the version, not the run. Only on an unmanaged node
can you let runs update themselves again with `harness.claude.self_update: true`.

A profile's Claude Code settings `autoUpdatesChannel`, `minimumVersion` or `env.DISABLE_AUTOUPDATER` /
`env.DISABLE_UPDATES` don't choose the version the node runs while the updater is off. The node logs a warning when a
profile sets them, unless `self_update` is on (then they take effect).

## Unmanaged (default)

```yaml
harness:
  claude_binary: claude        # a name on PATH or an absolute path
  claude:
    manage: false
```

The run executes `claude_binary`. The launch shim resolves a bare name on the herdr-core core's `PATH`, which is the
agent's environment when the agent started the core and can differ from the agent's own. Point `claude_binary` at an
absolute path if you install Claude Code yourself, and keep it updated with your own tooling.

::: warning Upgrading a node that relied on self-update
If the node's `claude` stayed current only through Claude Code's own self-update (the copy in `~/.local/bin/claude`),
it now **stays on its current version**: the updater is off in every run. Do one of these:

- set `harness.claude.manage: true` (the node downloads and updates Claude Code itself);
- update it with your own tooling;
- set `harness.claude.self_update: true` (runs update themselves again, as before).
:::

## Managed

```yaml
harness:
  claude:
    manage: true
    channel: latest            # latest, stable or an exact X.Y.Z
    cache_keep: 3
    cache_keep_pinned: 5
    refresh: 15m
    download_timeout: 15m
```

- **Resolving.** The node resolves the channel through `{base_url}/{channel}` and keeps the answer for `refresh`. A
  background loop resolves again every `refresh` and downloads the new version ahead of the next launch. An exact
  version is used as it is.
- **Downloading.** It downloads `{base_url}/{version}/manifest.json`, then the binary for the node's platform, and
  checks the manifest's version, size and SHA-256. Only a verified binary is renamed into the cache at
  `<data_dir>/harness/claude-code/<version>/claude`. The check is checksum-only: the manifest's signature is not
  verified yet. Whoever controls the release host or your `base_url` mirror controls the binary.
- **Launching.** A run launches that cached binary by **absolute path**, so the core's `PATH` doesn't matter. When the
  channel's new target is still downloading, the run gets the channel's previous target, or else the newest cached
  version not newer than the target. The node logs this, and records the requested channel, the version and why
  another version ran in the run's journal entry. A node at protocol 1.11 or later also reports them to central, which
  shows them with the run (see [Versions on runs](#versions-on-runs)).
- **Waiting.** When nothing usable is cached (the first launch on a new node, or a new exact version), the run waits
  in **starting** while the version downloads, up to `download_timeout`. Then it fails with "no Claude Code version …
  was available within the download timeout". A stop during the wait wins: the run ends as "stopped before start" and
  never starts. So does a lease that central fenced meanwhile (the run fails with "no valid lease"). Waiting blocks
  no other launch of the space. When the version a run waited for is gone by the time the launch takes it (the
  channel moved on, or GC removed a fallback), the run waits again, at most three times within `download_timeout`.
- **Live runs keep their binary.** A new version affects new runs only. GC never removes the version of a run that is
  live or that a restarted agent can adopt. It also keeps the newest `cache_keep` versions, the channel's current and
  previous targets, the node's exact `channel`, and up to `cache_keep_pinned` versions that profiles pinned (see
  [Pinning a version per profile](#pinning-a-version-per-profile)). Plan for about 340 MB per cached version (2.1.322
  linux-x64), so about 1 GB with the defaults, plus up to `cache_keep_pinned` pinned versions.
- **At agent start** the node removes interrupted downloads, re-hashes the versions it will use (those of live runs,
  the pinned version and the channel targets), removes any that fail, and collects garbage. GC runs again every
  `refresh`. A check that can't finish (the agent is stopping, or the launch's wait expired) keeps the version; only a
  real mismatch removes it.
- **Mirrors.** `base_url` must be https, and redirects must stay on its host. Only the node's configuration can set
  it.

`harness.claude.self_update` can't be combined with `manage: true`: nothing would launch a self-updated copy.

## Pinning a version per profile

A claude-code harness profile can pin the version its runs get: set **Claude Code version** in the profile form, or
`harness_version` in the API. The value is empty (follow the node's `channel`), `latest`, `stable` or an exact
`X.Y.Z` such as `2.1.322`. The pin overrides the node's channel for that profile's runs:

```text
profile pin  →  node channel  →  latest
```

| Node | Pin `latest` / `stable` | Pin `X.Y.Z` |
|---|---|---|
| Managed (`manage: true`) | Follows that channel as described above, fallbacks included | Exactly that version: downloaded when it isn't cached (the run waits in starting), **never a fallback**. A version the release host doesn't have, or that doesn't download within `download_timeout`, fails the launch |
| Unmanaged (`manage: false`) | Fails: "this node does not manage Claude Code versions" | Runs only when it equals the version detected at `claude_binary`, by that absolute path; otherwise it fails with "this node does not manage Claude Code versions" |
| Agent below protocol 1.11 | Fails as `node_too_old`: "node X doesn't support pinned Claude Code versions; upgrade it" | Same |

Pinning back to an older version is just launching the pin again: older releases stay downloadable, and a version a
profile pinned is kept in the cache. The node learns pins only from launches it has seen. GC keeps the exact versions
that profiles pinned on runs the node started in the last 30 days, at most `cache_keep_pinned` of them (default 5, the
most recently launched), besides the usual `cache_keep`, channel targets and live runs. A launch that failed before
its run started doesn't count. A version that a live run, or a run a restarted agent can adopt, uses is always kept,
whatever the cap.

An API update that leaves `harness_version` out keeps the stored pin, so an older Terraform provider doesn't clear it.
Send `""` to clear it. A pin is accepted on claude-code profiles only.

## Versions on runs

From node protocol 1.11 on, a run's detail (API `GET /spaces/{space_id}/runs/{run_id}`, and **Version** in the run
workbench) shows what the node launched:

- `harness_version`: the version that ran;
- `harness_requested`: the profile's pin, or else the node's channel;
- `harness_fallback`: why another version than the request's target ran (`previous_target`, `older_cached`,
  `last_target`, `newest_cached`). It is absent when the target ran.

The run manifest stays the configuration resolved at creation. On a node that manages Claude Code, its
`dependencies["claude-code"]` is then `{requested, node_default}`: the pin or channel, and the version the node last
reported a new run would get. Runs on older nodes, on nodes that don't manage Claude Code, and of other harnesses keep
the plain version string. An unmanaged node reports a version only for pinned runs.

## Versions on nodes

The node page (**Nodes**, then a node's row) shows what each harness on the node would run now:

- **Managed** Claude Code: the node's channel, the version a new run would get, the resolved `latest` and `stable`
  targets with when they were resolved, the cached versions and the cache's size on disk.
- **Unmanaged** harnesses: the version and path detected at `claude_binary` (or `codex_binary`).
- When the node last reported them, with a **stale** badge once a connected node's report is more than an hour old,
  or **offline** while the node is offline.

The API's node detail (`GET /nodes/{node_id}`) has the same data: `capabilities` and `capabilities_updated_at`. A
harness entry's `version` and `path` mean "what a new run would get now", not what a running run uses; a run's own
version is on the run (see [Versions on runs](#versions-on-runs)).

A node reports its capabilities when it connects. From node protocol 1.11 on, a connected node also sends them again:

- when its Claude Code cache changes (a version was downloaded or removed) or a channel resolves to another version;
- and at least every 30 minutes. Each time it detects every harness again, so a `claude` you updated yourself on an
  unmanaged node shows up within 30 minutes.

Central keeps only the report of the node's current connection. A node agent older than 1.11 reports only when it
connects, and its report turns stale after an hour of connection; an offline node keeps its last report, marked
offline.

Central checks the capabilities in hello and in each report against the same bounds. A bad part of a harness entry,
such as a cached version that isn't an exact `X.Y.Z`, is dropped from that entry with a warning in central's log; the
rest of the report is stored and the node stays connected.

### Tamper policy

Runs execute as the node's user, the same user that owns the cache. The cache's read-only modes (binary 0555,
directory 0500) only stop accidental changes.

At each launch the node compares the binary's stat fingerprint (device, inode, size, mode and ctime) with the one it
recorded when it last hashed the file, at download or at the startup check. If any of them changed, the binary is
hashed again (about 0.35 s for 340 MB on a 32-core EPYC 7551P with SHA-NI, page cache warm). If the hash doesn't match,
the version is removed and downloaded again. ctime changes with every write, chmod or replacement, and no
unprivileged call can set it back.

The check runs when the launch acquires its version, which is after the clone, worktree and memory preparation and
right before the launch is built. This finds a binary that was changed before that check. It doesn't stop a process
running as the node's user from replacing the binary between the check and the exec: that gap is the launch's last
step: building the launch, writing its files (settings, skills, login, secrets) and starting the process. Such a
process can already change much else the node runs, so the cache is no boundary against it. Re-hashing every launch
would not close that gap either, and it would cost a 340 MB read per run.

## Verified

- **Simulated** (`internal/agent`, `internal/harness/dist`, run on every check): an `httptest` fake release server and
  a stub binary. These tests cover:
  - launching by absolute path with both variables;
  - the unmanaged path;
  - waiting for a download, and the timeout;
  - a stop or a fenced lease during that wait, and a stop right before the start;
  - a fallback recorded in the journal;
  - an adopted run's version kept by GC;
  - GC racing a launch;
  - the tamper check, and a cancelled check keeping the version;
  - a profile pin downloaded and run exactly, never a fallback, and kept by GC;
  - an unmanaged node refusing a mismatching or channel pin;
  - central refusing a pin to a node below protocol 1.11, and storing and showing a run's reported version (`internal/app`, a scripted node).
  - the node re-reporting its capabilities after a cache change and every 30 minutes (and an unmanaged `claude`
    detected again), never to a central below 1.11, and central storing them from the current connection only.
- **Real** (opt-in): `SPACEMAN_REAL=1 go test -tags real -run TestRealManagedClaude ./internal/agent/` downloads a
  real release (about 340 MB) into a temporary cache and runs it with the updater variables. It hasn't been run as
  part of this change.
