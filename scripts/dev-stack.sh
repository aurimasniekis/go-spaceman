#!/usr/bin/env bash
# A throwaway local SpaceMan stack for UI development, end-to-end tests and screenshots: central (with the embedded UI),
# a node agent configuration that uses its own herdr-core core (never the default one) and a stub harness that prints a
# few lines, makes the tool calls its prompt asks for, and ticks, a Pulse-shaped NATS server (devpulse) to publish sample events into, and a fake ticket-tracker MCP
# server (devmcp, bearer token "dev-mcp-token") for the gateway. Nothing here talks to a real model or a real Pulse
# deployment.
#
# Usage:
#   scripts/dev-stack.sh up        builds, bootstraps and starts central; prints the stack directory
#   scripts/dev-stack.sh down DIR  stops central, the agent and the stack's own hc core, then removes DIR (KEEP=1 keeps it)
#
# The stack directory holds env (source it: SM_URL, SM_ROOT_TOKEN_FILE, SM_AGENT_*), logs and the root token (0600).
# Paths are kept short (/tmp/smdev-*) because unix socket paths are limited to 103 bytes.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)

# Never inherit a surrounding herdr-core terminal or SpaceMan configuration.
# An array, not a function: a backgrounded command then keeps its own PID (env execs it), so `down` can stop it.
clean_env=(env -u HC_TERMINAL_ID -u HC_TERMINAL_SOCKET -u HC_SCOPE -u HC_SESSION -u HC_SOCKET -u SPACEMAN_CONFIG -u SPACEMAN_AGENT_CONFIG)

up() {
  local dir port mport
  dir=$(mktemp -d /tmp/smdev-XXXXXX)
  port=${SM_DEV_PORT:-38180}
  mport=${SM_DEV_MANAGEMENT_PORT:-38181}
  mkdir -p "$dir/bin"
  # A failed build prints its output and leaves nothing behind.
  if [[ "${SKIP_UI_BUILD:-}" != 1 ]] && ! (cd "$root/web" && pnpm run build >"$dir/ui-build.log" 2>&1); then
    cat "$dir/ui-build.log" >&2
    rm -rf "$dir"
    echo "dev-stack: the UI build failed" >&2
    exit 1
  fi
  if ! (cd "$root" && go build -tags embedui -o "$dir/bin/" ./cmd/spaceman ./cmd/spaceman-agent && go build -o "$dir/bin/" ./internal/events/pulsetest/devpulse ./internal/gateway/gatewaytest/devmcp ./internal/harness/harnesstest/stubharness); then
    rm -rf "$dir"
    echo "dev-stack: the Go build failed" >&2
    exit 1
  fi
  mkdir -p "$dir/pulse"
  "${clean_env[@]}" "$dir/bin/devpulse" serve -dir "$dir/pulse" -port "${SM_DEV_PULSE_PORT:-38422}" >"$dir/devpulse.log" 2>&1 &
  echo $! >"$dir/devpulse.pid"
  "${clean_env[@]}" "$dir/bin/devmcp" -port "${SM_DEV_MCP_PORT:-38431}" -token dev-mcp-token -url-file "$dir/mcp-url" >"$dir/devmcp.log" 2>&1 &
  echo $! >"$dir/devmcp.pid"
  local url="http://127.0.0.1:$port"
  # Central publishes its own events to devpulse (SM-OBS-004), so the creds must exist before it starts.
  for _ in $(seq 1 100); do
    [[ -s "$dir/pulse/url" && -s "$dir/pulse/spaceman.creds" ]] && break
    sleep 0.1
  done
  [[ -s "$dir/pulse/url" ]] || { echo "devpulse did not start (see $dir/devpulse.log)" >&2; exit 1; }
  cat >"$dir/central.yaml" <<YAML
mode: development
data_dir: $dir/central
public_url: $url
kek: {file: $dir/kek}
http: {listen: "127.0.0.1:$port", management_listen: "127.0.0.1:$mport"}
nodes: {heartbeat_interval: 2s, lease_ttl: 10s, disconnect_grace: 10s}
log: {level: info, format: text}
events: {outbound: {enabled: true, url: "$(cat "$dir/pulse/url")", creds_file: $dir/pulse/spaceman.creds, source: spaceman-dev}}
YAML
  "${clean_env[@]}" "$dir/bin/spaceman" keys init-kek --out "$dir/kek" >/dev/null
  "${clean_env[@]}" "$dir/bin/spaceman" -c "$dir/central.yaml" admin bootstrap | grep -o 'spaceman_rt_[A-Za-z0-9_-]*' >"$dir/root-token"
  chmod 600 "$dir/root-token"
  "${clean_env[@]}" "$dir/bin/spaceman" -c "$dir/central.yaml" serve >"$dir/central.log" 2>&1 &
  echo $! >"$dir/central.pid"

  # The stub harness (internal/harness/harnesstest/stubharness) answers --version, prints a banner, runs "/call" lines of its
  # prompt as MCP tool calls through the SpaceMan bridge, then ticks until stopped.
  local session; session="smd$(printf %s "${dir##*/smdev-}" | tr "[:upper:]" "[:lower:]")"
  cat >"$dir/agent.yaml" <<YAML
mode: development
data_dir: $dir/agent
central: {url: $url, credential_file: $dir/agent/node.credential, reconnect_max: 2s}
runner: {session: $session, socket: $dir/hc.sock}
harness: {claude_binary: $dir/bin/stubharness, codex_binary: $dir/bin/stubharness}
log: {level: info, format: text}
YAML
  cat >"$dir/env" <<ENV
export SM_DIR=$dir
export SM_URL=$url
export SM_BIN=$dir/bin
export SM_ROOT_TOKEN_FILE=$dir/root-token
export SM_AGENT_CONFIG=$dir/agent.yaml
export SM_HC_SESSION=$session
export SM_HC_SOCKET=$dir/hc.sock
export SM_HC_HOME=$dir/agent/hc
export SM_PULSE_DIR=$dir/pulse
export SM_MCP_URL_FILE=$dir/mcp-url
ENV
  for _ in $(seq 1 100); do
    curl -fsS "$url/api/v1/system/info" >/dev/null 2>&1 && break
    sleep 0.1
  done
  curl -fsS "$url/api/v1/system/info" >/dev/null
  for _ in $(seq 1 100); do
    [[ -s "$dir/pulse/url" ]] && break
    sleep 0.1
  done
  [[ -s "$dir/pulse/url" ]] || { echo "devpulse did not start (see $dir/devpulse.log)" >&2; exit 1; }
  for _ in $(seq 1 100); do
    [[ -s "$dir/mcp-url" ]] && break
    sleep 0.1
  done
  [[ -s "$dir/mcp-url" ]] || { echo "devmcp did not start (see $dir/devmcp.log)" >&2; exit 1; }
  echo "$dir"
}

down() {
  local dir=${1:?usage: dev-stack.sh down DIR}
  [[ "$dir" == /tmp/smdev-* ]] || { echo "refusing: $dir is not a dev stack" >&2; exit 2; }
  # shellcheck disable=SC1091
  source "$dir/env"
  local pids=()
  for p in agent central devpulse devmcp; do
    if [[ -f "$dir/$p.pid" ]]; then pids+=("$(cat "$dir/$p.pid")"); fi
  done
  if ((${#pids[@]})); then
    kill "${pids[@]}" 2>/dev/null || true
    for _ in $(seq 1 100); do
      kill -0 "${pids[@]}" 2>/dev/null || break
      sleep 0.1
    done
  fi
  # Stop only this stack's own core.
  "${clean_env[@]}" HC_SESSION="$SM_HC_SESSION" HC_SOCKET="$SM_HC_SOCKET" XDG_STATE_HOME="$SM_HC_HOME/state" XDG_CONFIG_HOME="$SM_HC_HOME/config" \
    hc server stop >/dev/null 2>&1 || true
  if [[ "${KEEP:-}" != 1 ]]; then rm -rf "$dir"; fi
}

case "${1:-}" in
  up) up ;;
  down) down "${2:-}" ;;
  *) echo "usage: $0 up | down DIR" >&2; exit 2 ;;
esac
