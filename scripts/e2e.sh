#!/usr/bin/env bash
# Browser end-to-end tests against a fresh local stack (scripts/dev-stack.sh): real central with the embedded UI, a real
# spaceman-agent and its own herdr-core core, and the stub harness. Needs Go, pnpm, hc and the Playwright Chromium
# browser (cd web && pnpm exec playwright install chromium). Extra arguments go to `playwright test`.
#   SM_SHOTS=DIR  where the journey's screenshots go (default web/test-results/shots); review them, do not commit them
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
command -v hc >/dev/null || { echo "e2e needs herdr-core (hc) on PATH" >&2; exit 2; }
dir=$("$root/scripts/dev-stack.sh" up | tail -1)
# On failure the stack's logs are kept in web/test-results/stack-logs (git-ignored) before the stack is removed.
keep_logs() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    mkdir -p "$root/web/test-results/stack-logs"
    cp "$dir"/*.log "$root/web/test-results/stack-logs/" 2>/dev/null || true
    echo "stack logs kept in web/test-results/stack-logs" >&2
  fi
  "$root/scripts/dev-stack.sh" down "$dir"
}
trap keep_logs EXIT
# shellcheck disable=SC1091
source "$dir/env"
cd "$root/web"
pnpm exec playwright test "$@"
