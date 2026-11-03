# Try it locally

The repository includes a script that starts a throwaway SpaceMan stack on your machine: central with the web UI
built in, and a node agent configuration that uses **its own herdr-core core**. It never touches your default core or
your herdr-core terminals.

Runs in this stack use a **stub harness** that prints a banner and a counter. No model is contacted and no credential is
needed. Real Claude Code and Codex runs need a harness account: see
[Give the space an account](/guide/first-space#give-the-space-an-account) and [Accounts](/operators/accounts). Codex has
not been run for real yet.

## Requirements

- Go (the version in `go.mod`), Node.js 24 and pnpm
- [herdr-core](https://github.com/herdrdev/herdr-core) (`hc`) on your `PATH`
- macOS or Linux

## 1. Start central

```sh
git clone https://github.com/aurimasniekis/go-spaceman
cd go-spaceman
(cd web && pnpm install)
scripts/dev-stack.sh up        # prints a directory such as /tmp/smdev-AbC123
source /tmp/smdev-AbC123/env   # use the directory it printed
cat "$SM_ROOT_TOKEN_FILE"      # the root token, needed once
```

`up` builds the UI, both binaries and the stub harness, creates a key-encryption key, bootstraps a root token, and
starts a local Pulse-shaped NATS server (devpulse), a fake MCP server (devmcp) and central on
<http://127.0.0.1:38180> (set `SM_DEV_PORT` and `SM_DEV_MANAGEMENT_PORT` to use other ports).

## 2. Create the first administrator

Open <http://127.0.0.1:38180>. SpaceMan sends you to **Set up SpaceMan**: paste the root token and create your account.
Then sign in. Administrators must use an authenticator app, so you scan a QR code, enter a code, save your recovery
codes and enter a fresh code.

<Shot name="setup-bootstrap" alt="Set up SpaceMan page with the root token, name and email fields" caption="First-run setup with the root token" />

## 3. Add the node

In **Nodes → Add node**, create a node and copy the one-time enrollment token. In the shell where you sourced the env
file, enroll and start the agent against the stack's own core:

```sh
hcenv=(env -u HC_TERMINAL_ID -u HC_TERMINAL_SOCKET -u HC_SCOPE
  HC_SESSION=$SM_HC_SESSION HC_SOCKET=$SM_HC_SOCKET
  XDG_STATE_HOME=$SM_HC_HOME/state XDG_CONFIG_HOME=$SM_HC_HOME/config)

"${hcenv[@]}" "$SM_BIN/spaceman-agent" -c "$SM_AGENT_CONFIG" enroll --token 'spaceman_en_…'
"${hcenv[@]}" "$SM_BIN/spaceman-agent" -c "$SM_AGENT_CONFIG" run
```

The node turns **connected** within a few seconds. Continue with [First node and space](/guide/first-space).

## 4. Stop everything

Stop the agent with Ctrl-C, then:

```sh
scripts/dev-stack.sh down "$SM_DIR"   # KEEP=1 keeps the directory
```

This stops central, the agent, devpulse, devmcp and the stack's own herdr-core core, and removes the directory.

## The same journey, automated

`make e2e` runs this whole journey in a headless Chromium against a fresh stack and removes it afterwards: setup,
sign-in with TOTP, node enrollment, a space, a run watched live, stopped and replayed.
