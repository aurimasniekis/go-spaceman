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
#   scripts/dev-stack.sh restart DIR
#                                  rebuilds the UI (unless SKIP_UI_BUILD=1) and the binaries from this checkout and restarts
#                                  central on the stack's existing data; the agent reconnects by itself (restart it to run
#                                  its new binary). Central's start-up work runs again, e.g. adding the starter roles.
#
# The stack directory holds env (source it: SM_URL, SM_ROOT_TOKEN_FILE, SM_AGENT_*), logs and the root token (0600).
# Paths are kept short (/tmp/smdev-*) because unix socket paths are limited to 103 bytes.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)

# Never inherit a surrounding herdr-core terminal or SpaceMan configuration.
# An array, not a function: a backgrounded command then keeps its own PID (env execs it), so `down` can stop it.
# Every SPACEMAN_* variable of the surrounding session is dropped too (an agent session's own SPACEMAN_* settings make
# central refuse to start or point the stack at the wrong place).
clean_env=(env -u HC_TERMINAL_ID -u HC_TERMINAL_SOCKET -u HC_SCOPE -u HC_SESSION -u HC_SOCKET -u SPACEMAN_CONFIG -u SPACEMAN_AGENT_CONFIG)
while IFS= read -r v; do clean_env+=(-u "$v"); done < <(compgen -e | grep '^SPACEMAN_' || true)

# port_busy PORT: something already listens on 127.0.0.1:PORT.
port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pick_port VAR DEFAULT: the port from the environment variable VAR when set (used as is); otherwise DEFAULT when free,
# otherwise a free port picked at random, so an orphaned stack on the default ports doesn't block a new one.
used_ports=" "
pick_port() {
  local p=${!1:-}
  if [[ -z "$p" ]]; then
    p=$2
    while port_busy "$p" || [[ "$used_ports" == *" $p "* ]]; do p=$((RANDOM % 20000 + 20000)); done
  fi
  used_ports+="$p "
  echo "$p"
}

# stop_pids PID...: TERM, then KILL what is still alive after 10 s.
stop_pids() {
  ((${#@})) || return 0
  kill "$@" 2>/dev/null || true
  for _ in $(seq 1 100); do
    kill -0 "$@" 2>/dev/null || return 0
    sleep 0.1
  done
  kill -9 "$@" 2>/dev/null || true
}

up() {
  local dir port mport
  dir=$(mktemp -d /tmp/smdev-XXXXXX)
  # Ports: SM_DEV_PORT, SM_DEV_MANAGEMENT_PORT, SM_DEV_PULSE_PORT and SM_DEV_MCP_PORT when set, else the default if free,
  # else a free one (the stack's env file says which).
  port=$(pick_port SM_DEV_PORT 38180)
  mport=$(pick_port SM_DEV_MANAGEMENT_PORT 38181)
  local pport mcpport
  pport=$(pick_port SM_DEV_PULSE_PORT 38422)
  mcpport=$(pick_port SM_DEV_MCP_PORT 38431)
  mkdir -p "$dir/bin"
  # A start that fails or is interrupted (Ctrl-C) halfway removes what it started; `up_ok` is set once the stack is up.
  up_ok=0
  trap '[[ $up_ok == 1 ]] || down "$dir" >/dev/null 2>&1' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
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
  "${clean_env[@]}" "$dir/bin/devpulse" serve -dir "$dir/pulse" -port "$pport" >"$dir/devpulse.log" 2>&1 &
  echo $! >"$dir/devpulse.pid"
  "${clean_env[@]}" "$dir/bin/devmcp" -port "$mcpport" -token dev-mcp-token -url-file "$dir/mcp-url" >"$dir/devmcp.log" 2>&1 &
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
  up_ok=1
  echo "$dir"
}

down() {
  local dir=${1:?usage: dev-stack.sh down DIR}
  [[ $dir =~ ^/tmp/smdev-[A-Za-z0-9]{6}$ ]] || { echo "refusing: $dir is not a dev stack" >&2; exit 2; }
  # The env file doesn't exist yet when a start is cleaned up halfway.
  # shellcheck disable=SC1091
  [[ -f "$dir/env" ]] && source "$dir/env"
  local pids=()
  for p in agent central devpulse devmcp; do
    if [[ -f "$dir/$p.pid" ]]; then pids+=("$(cat "$dir/$p.pid")"); fi
  done
  stop_pids "${pids[@]}"
  # Stop only this stack's own core.
  [[ -z "${SM_HC_SESSION:-}" ]] || "${clean_env[@]}" HC_SESSION="$SM_HC_SESSION" HC_SOCKET="$SM_HC_SOCKET" XDG_STATE_HOME="$SM_HC_HOME/state" XDG_CONFIG_HOME="$SM_HC_HOME/config" \
    hc server stop >/dev/null 2>&1 || true
  # Anything else started from this stack (an agent the tests enrolled, the stub harness) has the directory in its command
  # line; only those are stopped. The caller (make e2e, screenshots.sh) has the directory in its own command line too, so
  # this script and its ancestors are skipped.
  local self=" $$ " p=$$ q left=""
  while p=$(ps -o ppid= -p "$p" 2>/dev/null | tr -d ' ') && [[ -n $p && $p -gt 1 ]]; do self+="$p "; done
  # pgrep's exit status goes outside the substitution: a `|| true` inside it would leave a subshell with this script's
  # command line behind, and the subshell would match.
  local found; found=$(pgrep -u "$(id -u)" -f -- "$dir/") || true
  for q in $found; do
    [[ $self == *" $q "* ]] || left+="$q "
  done
  # shellcheck disable=SC2086
  stop_pids $left
  if [[ "${KEEP:-}" != 1 ]]; then rm -rf "$dir"; fi
}

restart() {
  local dir=${1:?usage: dev-stack.sh restart DIR}
  [[ $dir =~ ^/tmp/smdev-[A-Za-z0-9]{6}$ && -f "$dir/central.yaml" ]] || { echo "refusing: $dir is not a dev stack" >&2; exit 2; }
  # shellcheck disable=SC1091
  source "$dir/env"
  if [[ "${SKIP_UI_BUILD:-}" != 1 ]] && ! (cd "$root/web" && pnpm run build >"$dir/ui-build.log" 2>&1); then
    cat "$dir/ui-build.log" >&2
    echo "dev-stack: the UI build failed; central keeps running" >&2
    exit 1
  fi
  # Built next to the old binaries first, so a failed build leaves the running stack alone.
  mkdir -p "$dir/bin.new"
  if ! (cd "$root" && go build -tags embedui -o "$dir/bin.new/" ./cmd/spaceman ./cmd/spaceman-agent); then
    rm -rf "$dir/bin.new"
    echo "dev-stack: the Go build failed; central keeps running" >&2
    exit 1
  fi
  if [[ -f "$dir/central.pid" ]]; then
    local pid; pid=$(cat "$dir/central.pid")
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 100); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
  fi
  mv -f "$dir/bin.new/spaceman" "$dir/bin.new/spaceman-agent" "$dir/bin/"
  rmdir "$dir/bin.new"
  "${clean_env[@]}" "$dir/bin/spaceman" -c "$dir/central.yaml" serve >>"$dir/central.log" 2>&1 &
  echo $! >"$dir/central.pid"
  for _ in $(seq 1 100); do
    curl -fsS "$SM_URL/api/v1/system/info" >/dev/null 2>&1 && break
    sleep 0.1
  done
  if ! curl -fsS "$SM_URL/api/v1/system/info" >/dev/null 2>&1; then
    tail -n 5 "$dir/central.log" >&2
    echo "dev-stack: central did not come back (see $dir/central.log)" >&2
    exit 1
  fi
  echo "$dir"
}

case "${1:-}" in
  up) up ;;
  down) down "${2:-}" ;;
  restart) restart "${2:-}" ;;
  *) echo "usage: $0 up | down DIR | restart DIR" >&2; exit 2 ;;
esac
