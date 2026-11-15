#!/usr/bin/env bash
# The fast e2e iteration loop: one dev stack that stays up between runs, and one spec (or --grep) per run, instead of
# `make e2e` building and starting a fresh stack for the whole suite (see site/development/testing.md).
#
#   scripts/e2e-loop.sh up                      start the stack (or reuse the one that is running); prints its directory
#   scripts/e2e-loop.sh test e2e/FILE.spec.ts   run that spec against it (extra arguments go to `playwright test`)
#   scripts/e2e-loop.sh test --grep 'title'     run the tests whose title matches
#   scripts/e2e-loop.sh restart                 rebuild central and the UI from the checkout, keep the data
#   scripts/e2e-loop.sh down                    stop the stack and remove its directory
#
# `test` starts the stack when none is running, so `up` is optional. The stack's directory is remembered in
# web/.e2e-stack (git-ignored), so every run from this checkout finds the same stack. Ports are the defaults when free and
# free ones otherwise; SM_DEV_PORT, SM_DEV_MANAGEMENT_PORT, SM_DEV_PULSE_PORT and SM_DEV_MCP_PORT set them.
# `smoke.spec.ts` is a first-run journey (setup with the root token) and works once per stack; every other spec seeds what
# it needs. Nothing here stops a stack it didn't start: `down` only removes the stack named in web/.e2e-stack.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
state="$root/web/.e2e-stack"
stack="$root/scripts/dev-stack.sh"

alive() { [[ -f "$state" ]] && d=$(<"$state") && [[ -f "$d/central.pid" ]] && kill -0 "$(<"$d/central.pid")" 2>/dev/null; }

up() {
  command -v hc >/dev/null || { echo "e2e needs herdr-core (hc) on PATH" >&2; exit 2; }
  if alive; then echo "$d"; return; fi
  [[ ! -f "$state" ]] || "$stack" down "$(<"$state")" 2>/dev/null || true # a stack that died: remove what is left
  dir=$("$stack" up | tail -1)
  echo "$dir" >"$state"
  echo "$dir"
}

case "${1:-}" in
  up) up ;;
  test)
    shift
    dir=$(up | tail -1)
    # shellcheck disable=SC1091
    source "$dir/env"
    cd "$root/web"
    exec pnpm exec playwright test "$@"
    ;;
  restart) alive || { echo "no stack is running (scripts/e2e-loop.sh up)" >&2; exit 1; }; "$stack" restart "$(<"$state")" ;;
  down)
    if [[ -f "$state" ]]; then "$stack" down "$(<"$state")"; rm -f "$state"; else echo "no stack is running"; fi
    ;;
  *) echo "usage: $0 up | test [playwright args] | restart | down" >&2; exit 2 ;;
esac
