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
  another version ran in the run's journal entry. Reporting it to central comes with a later protocol version.
- **Waiting.** When nothing usable is cached (the first launch on a new node, or a new exact version), the run waits
  in **starting** while the version downloads, up to `download_timeout`. Then it fails with "no Claude Code version …
  was available within the download timeout". A stop during the wait wins: the run ends as "stopped before start" and
  never starts. So does a lease that central fenced meanwhile (the run fails with "no valid lease"). Waiting blocks
  no other launch of the space. When the version a run waited for is gone by the time the launch takes it (the
  channel moved on, or GC removed a fallback), the run waits again, at most three times within `download_timeout`.
- **Live runs keep their binary.** A new version affects new runs only. GC never removes the version of a run that is
  live or that a restarted agent can adopt. It also keeps the newest `cache_keep` versions, the channel's current and
  previous targets, and a pinned exact version. Plan for about 340 MB per cached version (2.1.322 linux-x64), so
  about 1 GB with the defaults.
- **At agent start** the node removes interrupted downloads, re-hashes the versions it will use (those of live runs,
  the pinned version and the channel targets), removes any that fail, and collects garbage. GC runs again every
  `refresh`. A check that can't finish (the agent is stopping, or the launch's wait expired) keeps the version; only a
  real mismatch removes it.
- **Mirrors.** `base_url` must be https, and redirects must stay on its host. Only the node's configuration can set
  it.

`harness.claude.self_update` can't be combined with `manage: true`: nothing would launch a self-updated copy.

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
  - the tamper check, and a cancelled check keeping the version.
- **Real** (opt-in): `SPACEMAN_REAL=1 go test -tags real -run TestRealManagedClaude ./internal/agent/` downloads a
  real release (about 340 MB) into a temporary cache and runs it with the updater variables. It hasn't been run as
  part of this change.
