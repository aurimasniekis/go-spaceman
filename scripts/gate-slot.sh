#!/usr/bin/env bash
# Usage: scripts/gate-slot.sh <label> <command> [args...]
#
# A node-wide semaphore for the heavy gates (race, coverage, site-a11y, e2e, screenshots): runs the command while
# holding one of SM_GATE_SLOTS slots, so several agents' `make check` runs on one machine take turns instead of
# contending for the CPU (contention is what slows them down and trips the write-transaction guard and timing tests).
#
# Each slot is a lock file in SM_GATE_LOCK_DIR held with flock(2). The lock belongs to this script's open file, so the
# kernel releases it when the script exits however it ends, a kill -9 included: a crashed run never leaks a slot. The
# command runs as a child that doesn't inherit the lock file, so a daemon it leaves behind can't hold the slot either
# (killing only this script releases the slot while the command goes on; stop the whole process group instead).
# While every slot is busy it prints one waiting line (with what holds the slots) and then checks again every second.
#
# Environment:
#   SM_GATE_SLOTS     number of slots: default 2 when the machine has 16 or more CPUs, otherwise 1. 0 disables the
#                     semaphore (the command runs at once); it is always disabled when CI=true (GitHub Actions),
#                     where every job has its own runner, whatever SM_GATE_SLOTS says.
#   SM_GATE_LOCK_DIR  directory of the lock files, shared by every agent on the machine
#                     (default ${XDG_CACHE_HOME:-$HOME/.cache}/spaceman-gates, created when missing). For runs of
#                     several users, pre-create a common directory with mode 1777 (install -d -m 1777 <dir>); the
#                     slot files are created readable by everyone, which is all flock needs.
#
# Without flock(1) (macOS has none by default) it warns and runs the command without a slot.
#
# Nested calls (make check holds a slot for coverage and race together) see SM_GATE_HELD and run the command directly.
set -euo pipefail

[[ $# -ge 2 ]] || { echo "usage: $0 <label> <command> [args...]" >&2; exit 2; }
label=$1
shift

cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)
default_slots=1
(( cpus >= 16 )) && default_slots=2
slots="${SM_GATE_SLOTS:-$default_slots}"
[[ "$slots" =~ ^[0-9]+$ ]] || { echo "gate-slot: SM_GATE_SLOTS must be a non-negative integer" >&2; exit 2; }
[[ "${CI:-}" == "true" ]] && slots=0

if (( slots == 0 )) || [[ -n "${SM_GATE_HELD:-}" ]]; then
  exec "$@"
fi
if ! command -v flock >/dev/null 2>&1; then
  echo "gate-slot: warning: flock not found, running $label without a heavy-gate slot" >&2
  exec "$@"
fi

dir="${SM_GATE_LOCK_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/spaceman-gates}"
mkdir -p "$dir"

fd=
slot=
waited=0
wait_start=$SECONDS
while :; do
  for (( i = 0; i < slots; i++ )); do
    f="$dir/slot-$i.lock"
    if [[ ! -e "$f" ]]; then
      # Can fail when another user created it a moment ago (fs.protected_regular in a sticky directory): then it exists.
      : 2>/dev/null >>"$f" || true
      chmod a+r "$f" 2>/dev/null || true # readable is enough to lock it, for every user sharing the directory
    fi
    exec {fd}<"$f"
    if flock -n "$fd"; then
      slot=$i
      break 2
    fi
    exec {fd}<&-
  done
  if (( waited == 0 )); then
    holders=""
    for (( i = 0; i < slots; i++ )); do
      o="$dir/slot-$i.owner"
      [[ -r "$o" ]] && holders+="${holders:+; }$(<"$o")"
    done
    echo "gate-slot: $label waiting for a heavy-gate slot ($slots busy${holders:+: $holders}; SM_GATE_SLOTS=$slots, $dir)" >&2
    waited=1
  fi
  sleep 1
done

# The owner note is informational only (shown to waiters); the lock is what counts, so a stale note is harmless.
echo "$label pid $$ in ${PWD/#$HOME/\~} since $(date +%H:%M:%S)" >"$dir/slot-$slot.owner" 2>/dev/null || true
if (( waited )); then
  echo "gate-slot: $label got slot $slot after $(( SECONDS - wait_start )) s" >&2
fi

export SM_GATE_HELD="$label"
# In the foreground, so Ctrl-C (and make's interrupt) reaches the command directly; the slot is released when this
# script exits.
rc=0
"$@" {fd}<&- || rc=$?
exit "$rc"
