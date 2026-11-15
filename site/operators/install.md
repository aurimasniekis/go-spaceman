# Installation

SpaceMan has two programs: **`spaceman`**, the central control plane with the web UI embedded, and
**`spaceman-agent`**, the node agent that runs coding agents through herdr-core. Both are single static Go binaries.

::: warning Supported platform
The production target is **Linux** on amd64 and arm64 (SM-PKG-003). macOS builds exist for development and are
labelled *unsupported*. The systemd units below ran a real installation of the linux/arm64 archive under systemd 257 in a
Debian 13 container, but not yet on a physical or cloud Linux host or on amd64. See
[known limitations](/reference/limitations).
:::

You can install SpaceMan in three ways:

- **Release archives** with systemd (this page). This is the main path for central and nodes.
- **Container images** from Docker Hub (`aurimasniekis/spaceman`, `aurimasniekis/spaceman-agent`) or built yourself
  ([Containers and images](/operators/containers)). This works for central.
  The node agent image is *not* agent-ready by itself.
- **From source** ([below](#from-source)).

Upgrades and rollback are described in [Upgrade and rollback](/operators/upgrade).

## What you install yourself

SpaceMan ships neither herdr-core nor any harness. A node needs these installed separately:

| Component | Central | Node | Shipped with SpaceMan? |
|---|---|---|---|
| `git` | required: role memory, memory bundles in backups | required: repositories, worktrees, memory, knowledge bases | no (your distribution's package) |
| [herdr-core](https://github.com/herdrdev/herdr-core) (`hc`) | not used | required | **no** |
| Claude Code (`claude`), Codex (`codex`) | not used | the harnesses you want to run | **no**. Third-party products under their own licence terms |
| `ssh` client | not used | for Git remotes over SSH | no |

Without `git`, central still starts. It logs `role memory cannot be stored centrally until git is installed`;
backups then leave role memory out (with a warning in the manifest), and restores of backups that contain role memory
are refused. Without `hc`, `spaceman-agent run` refuses to start.

A node can download and verify Claude Code itself instead of using the one you installed: see
[Managed Claude Code](./claude-code).

## Release archives

Each release has one archive per platform, `spaceman_<version>_linux_<arch>.tar.gz`. Each archive contains:

```text
spaceman                        central, web UI embedded
spaceman-agent                  node agent
systemd/spaceman.service        hardened unit for central
systemd/spaceman-agent.service  unit for nodes (sandbox relaxed for harnesses, see below)
examples/spaceman.example.yaml  central configuration, secret-free
examples/agent.example.yaml     node configuration, secret-free
LICENSE  NOTICE  THIRD_PARTY.md  CHANGELOG.md
```

Next to the archives are `SHA256SUMS` and SPDX SBOMs: one per archive (the Go modules in both binaries) and one for the
source tree (Go modules plus the web UI's locked npm packages). Releases are created as drafts and published by hand.
No release has been published yet.

Verify the download before installing:

```sh
sha256sum --ignore-missing --check SHA256SUMS
tar -xzf spaceman_<version>_linux_amd64.tar.gz
cd spaceman_<version>_linux_amd64
```

To build the same archives yourself, run `make web-build release-snapshot`. The output goes to `dist/`. SBOMs are
generated when [syft](https://github.com/anchore/syft) is on `PATH`. Without it they are skipped with a notice;
`SBOM=require` turns the notice into a failure.

## Central with systemd

```sh
sudo install -m 0755 -o root -g root spaceman /usr/local/bin/spaceman
sudo apt-get install git                       # or your distribution's equivalent
sudo useradd --system --home-dir /var/lib/spaceman --shell /usr/sbin/nologin spaceman
sudo install -d -m 0700 -o spaceman -g spaceman /var/lib/spaceman
sudo install -d -m 0755 /etc/spaceman
sudo install -m 0640 -o root -g spaceman examples/spaceman.example.yaml /etc/spaceman/spaceman.yaml
sudoedit /etc/spaceman/spaceman.yaml           # public_url, listeners, TLS or trusted_proxies
sudo spaceman keys init-kek --out /etc/spaceman/kek
sudo chown spaceman:spaceman /etc/spaceman/kek
```

`keys init-kek` refuses to overwrite a file and writes mode `0400`. SpaceMan refuses a KEK that group or others can
read, or that is owned by a user other than its own or root. The service user must be able to read it, so the result
is `/etc/spaceman/kek`, `spaceman:spaceman`, `0400`, outside the data directory. Back it up offline, separately from
data backups. Without it, every credential and every backup is unrecoverable.

The essential configuration (all keys are in `examples/spaceman.example.yaml`):

```yaml
mode: production
data_dir: /var/lib/spaceman
public_url: https://spaceman.example.com      # the address browsers and agents use; https in production
kek: {file: /etc/spaceman/kek}
http:
  listen: ":8080"
  management_listen: "127.0.0.1:9090"         # /healthz, /readyz and /metrics; unauthenticated, keep it private
  # Either terminate TLS here (tls_cert_file, tls_key_file) or behind a reverse proxy listed in trusted_proxies.
```

Install the unit, issue the root token and start:

```sh
sudo install -m 0644 systemd/spaceman.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo -u spaceman spaceman -c /etc/spaceman/spaceman.yaml admin bootstrap   # prints the root token once
sudo systemctl enable --now spaceman
curl -fsS http://127.0.0.1:9090/readyz
```

`admin bootstrap` prints the **root token** once. Open `public_url`, follow **Set up SpaceMan** and create the first
administrator with it (or `POST /api/v1/users` with `Authorization: Bearer <root token>`). Store the root token offline.
You can rotate or revoke it later on the Account page. See [Sign-in and access](/operators/access).

`admin` commands work offline: they take the data-directory lock, so run them while the service is stopped. Production
mode refuses plain-HTTP public URLs, relative key paths, passwords shorter than 12 characters and
`auth.require_totp: false`. Every key is listed in the [configuration reference](/reference/configuration).

The unit runs central as `spaceman` with a strict sandbox:

- The OS is read-only. Only `/var/lib/spaceman` is writable (`StateDirectory`, mode `0700`).
- The process has no capabilities, no new privileges and a private `/tmp` and `/dev`.
- System calls are filtered, and writable-and-executable memory is denied.

Two settings need edits to the unit:

- **Backups on another disk.** If `backups.local_dir` is outside the data directory, add it to `ReadWritePaths=`.
- **A port below 1024.** To bind one directly, add `CAP_NET_BIND_SERVICE` as the unit's comments describe. Terminating
  TLS at a reverse proxy is the preferred setup.

Check the sandbox on your host with `systemd-analyze security spaceman.service`.

## Node agent with systemd

Install herdr-core, `git` and the harnesses first, outside `/home` (for example in `/usr/local/bin`). The unit hides
`/home`, so a binary installed in a user's home directory is not found. Then:

```sh
sudo install -m 0755 -o root -g root spaceman-agent /usr/local/bin/spaceman-agent
sudo useradd --system --home-dir /var/lib/spaceman-agent --shell /bin/sh spaceman-agent
sudo install -d -m 0700 -o spaceman-agent -g spaceman-agent /var/lib/spaceman-agent
sudo install -d -m 0755 /etc/spaceman-agent
sudo install -m 0640 -o root -g spaceman-agent examples/agent.example.yaml /etc/spaceman-agent/agent.yaml
sudoedit /etc/spaceman-agent/agent.yaml        # central.url
```

The essential configuration:

```yaml
mode: production
data_dir: /var/lib/spaceman-agent
central:
  url: https://spaceman.example.com
  credential_file: /var/lib/spaceman-agent/node.credential
runner:
  hc_binary: hc
  session: spaceman                    # the agent's own herdr-core core, separate from interactive use
  socket: /run/spaceman-agent/hc.sock  # the unit's RuntimeDirectory
harness:
  claude_binary: claude
  codex_binary: codex
capacity: 4
```

In the web UI, add the node under **Nodes → Add node** and copy the one-time enrollment token. Enroll, then start the
unit:

```sh
sudo -u spaceman-agent HOME=/var/lib/spaceman-agent \
  spaceman-agent -c /etc/spaceman-agent/agent.yaml enroll --token 'spaceman_en_…'
sudo install -m 0644 systemd/spaceman-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now spaceman-agent
journalctl -u spaceman-agent -o cat | grep -E 'starting|herdr-core|error'
```

`enroll` writes the node credential with mode `0600`. `run` starts (or adopts) the herdr-core core, reports the
harnesses it finds and connects to central. The agent's `HOME` is its data directory, so the node's Git configuration
and SSH keys for repository remotes go in `/var/lib/spaceman-agent/.gitconfig` and `/var/lib/spaceman-agent/.ssh/`.

### Agent sandbox

The agent's sandbox is looser than central's, on purpose. The agent starts herdr-core, herdr-core starts the harnesses, and the harnesses run arbitrary developer tools in the
node's spaces. `spaceman-agent.service` keeps these protections:

- no capabilities and no new privileges, so agents cannot use `sudo` or setuid programs;
- a read-only OS, with only the data directory writable;
- `/home` hidden, and a private `/tmp`;
- kernel tunables, modules and logs protected;
- other users' processes hidden.

It relaxes these, each explained in the unit and in ADR-0034:

| Setting | Central | Agent | Why |
|---|---|---|---|
| `MemoryDenyWriteExecute` | yes | not set | JIT runtimes (JavaScript engines, the JVM) used by harnesses and tools need it |
| `RestrictNamespaces` | yes | not set | harness and tool sandboxes may create namespaces (which ones do is untested) |
| `ProcSubset=pid` | yes | not set | runtimes and build tools read `/proc/cpuinfo`, `/proc/meminfo` |
| `@debug` system calls | denied | allowed | debuggers, profilers and sanitizer runs use `ptrace` and `perf_event_open` |
| PTY devices | n/a | `/dev/ptmx`, `/dev/pts` allowed | herdr-core runs every program in a pseudo terminal |

The sandbox protects the rest of the host from the agent's user. It does not isolate one space from another. Every
space on a node runs as the same user (see [known limitations](/reference/limitations)).

**Stopping the unit stops everything on the node.** `KillMode=control-group` sends `SIGTERM` to the agent, its
herdr-core core and every run. `systemctl restart spaceman-agent` therefore ends running agents. Pause the node's spaces
or let the runs finish first. The unit's comments describe an untested alternative that keeps runs alive across an agent
restart.

## From source

```sh
git clone https://github.com/aurimasniekis/go-spaceman
cd go-spaceman
make web-install
make build-ui        # builds the web UI, then bin/spaceman and bin/spaceman-agent with the UI embedded
```

`make build` (without `-ui`) produces binaries that serve the API only. `make web-build release-snapshot` builds the
release archives, SBOMs and checksums in `dist/`.
