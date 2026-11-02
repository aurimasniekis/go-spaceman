#!/usr/bin/env bash
# A throwaway local SpaceMan stack for UI development, end-to-end tests and screenshots: central (with the embedded UI)
# plus a node agent configuration that uses its own herdr-core core (never the default one) and a stub harness that
# prints a few lines and ticks. Nothing here talks to a real model.
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
  if [[ "${SKIP_UI_BUILD:-}" != 1 ]]; then (cd "$root/web" && pnpm run build >/dev/null); fi
  (cd "$root" && go build -tags embedui -o "$dir/bin/" ./cmd/spaceman ./cmd/spaceman-agent)
  local url="http://127.0.0.1:$port"
  cat >"$dir/central.yaml" <<YAML
mode: development
data_dir: $dir/central
public_url: $url
kek: {file: $dir/kek}
http: {listen: "127.0.0.1:$port", management_listen: "127.0.0.1:$mport"}
nodes: {heartbeat_interval: 2s, lease_ttl: 10s, disconnect_grace: 10s}
log: {level: info, format: text}
YAML
  "${clean_env[@]}" "$dir/bin/spaceman" keys init-kek --out "$dir/kek" >/dev/null
  "${clean_env[@]}" "$dir/bin/spaceman" -c "$dir/central.yaml" admin bootstrap | grep -o 'spaceman_rt_[A-Za-z0-9_-]*' >"$dir/root-token"
  chmod 600 "$dir/root-token"
  "${clean_env[@]}" "$dir/bin/spaceman" -c "$dir/central.yaml" serve >"$dir/central.log" 2>&1 &
  echo $! >"$dir/central.pid"

  # Stub harness: answers --version, prints a banner, then ticks until stopped.
  cat >"$dir/stub-harness.sh" <<'SH'
#!/bin/sh
if [ "$1" = --version ]; then echo '0.0.0 (stub)'; exit 0; fi
printf '\033[1;35mstub harness\033[0m started in %s\r\n' "$(pwd)"
printf 'args: %s\r\n' "$*"
i=0; while true; do printf '\033[36mtick\033[0m %d\r\n' "$i"; i=$((i+1)); sleep 1; done
SH
  chmod 700 "$dir/stub-harness.sh"
  local session; session="smd$(printf %s "${dir##*/smdev-}" | tr "[:upper:]" "[:lower:]")"
  cat >"$dir/agent.yaml" <<YAML
mode: development
data_dir: $dir/agent
central: {url: $url, credential_file: $dir/agent/node.credential, reconnect_max: 2s}
runner: {session: $session, socket: $dir/hc.sock}
harness: {claude_binary: $dir/stub-harness.sh, codex_binary: $dir/stub-harness.sh}
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
ENV
  for _ in $(seq 1 100); do
    curl -fsS "$url/api/v1/system/info" >/dev/null 2>&1 && break
    sleep 0.1
  done
  curl -fsS "$url/api/v1/system/info" >/dev/null
  echo "$dir"
}

down() {
  local dir=${1:?usage: dev-stack.sh down DIR}
  [[ "$dir" == /tmp/smdev-* ]] || { echo "refusing: $dir is not a dev stack" >&2; exit 2; }
  # shellcheck disable=SC1091
  source "$dir/env"
  local pids=()
  for p in agent central; do
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
