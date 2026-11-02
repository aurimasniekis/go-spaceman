# Install from source

Release binaries, container images and systemd units are planned for a later milestone. Until then, build SpaceMan from
source. Both programs are single static Go binaries.

## Build

```sh
git clone https://github.com/aurimasniekis/go-spaceman
cd go-spaceman
(cd web && pnpm install)
make build-ui        # builds the web UI, then bin/spaceman and bin/spaceman-agent with the UI embedded
```

`make build` (without `-ui`) produces binaries that serve the API only.

## Central

Create a configuration file, for example `/etc/spaceman/spaceman.yaml`:

```yaml
mode: production
data_dir: /var/lib/spaceman
public_url: https://spaceman.example.com      # the address browsers and agents use; https in production
kek: {file: /etc/spaceman/kek}                # key-encryption key, created below
http:
  listen: ":8080"
  management_listen: "127.0.0.1:9090"         # /healthz, /readyz and /metrics; keep it private
  # Either terminate TLS here (tls_cert_file, tls_key_file) or behind a reverse proxy listed in trusted_proxies.
nodes:
  heartbeat_interval: 10s
  lease_ttl: 45s
  disconnect_grace: 2m
# events: {outbound: {...}}                   # optional: publish SpaceMan's own events to Pulse (guide/publishing-events)
```

Then create the key-encryption key, issue the root token and start the server:

```sh
spaceman keys init-kek --out /etc/spaceman/kek    # mode 0400, never overwrites
spaceman -c /etc/spaceman/spaceman.yaml admin bootstrap
spaceman -c /etc/spaceman/spaceman.yaml serve
```

`admin bootstrap` prints the **root token** once. Open `public_url`, follow **Set up SpaceMan** and create the first
administrator with it (or `POST /api/v1/users` with `Authorization: Bearer <root token>`). Store the root token offline;
you can rotate or revoke it later on the Account page. See [Sign-in and access](/operators/access).

`admin` commands work offline: they take the data-directory lock, so run them while `serve` is stopped. Production mode
refuses plain-HTTP public URLs, relative key paths and passwords shorter than 12 characters.

## Node agent

Each node needs [herdr-core](https://github.com/herdrdev/herdr-core) (`hc`) and the harnesses you want to run (`claude`,
`codex`). Create `/etc/spaceman-agent/agent.yaml`:

```yaml
mode: production
data_dir: /var/lib/spaceman-agent
central:
  url: https://spaceman.example.com
  credential_file: /var/lib/spaceman-agent/node.credential
runner:
  session: spaceman                    # the agent's own herdr-core core, separate from interactive use
  socket: /run/spaceman-agent/hc.sock
harness:
  claude_binary: claude
  codex_binary: codex
capacity: 4
```

In the web UI, add the node under **Nodes → Add node** and copy the one-time enrollment token. On the node:

```sh
spaceman-agent -c /etc/spaceman-agent/agent.yaml enroll --token 'spaceman_en_…'
spaceman-agent -c /etc/spaceman-agent/agent.yaml run
```

`enroll` writes the node credential with mode 0600. `run` starts (or adopts) the herdr-core core, reports the harnesses it
finds and connects to central. Run the agent as a dedicated user and keep it running under your service manager.
