#!/usr/bin/env bash
# Usage: scripts/changed.sh <extended-regex>
# Exits 0 when a path matching the regex differs from the merge base with SM_CHANGED_BASE (default origin/main):
# committed changes on the branch, staged and unstaged edits, and untracked files all count. Exits 1 when none does.
# When the base can't be found it exits 0. Callers skip a check only on exit status 1, so any error runs the check
# instead of skipping it.
# make check-fast uses it to run sqlc-check and api-check only when their inputs changed; CI always runs both.
set -euo pipefail

[[ $# -eq 1 ]] || { echo "usage: $0 <extended-regex>" >&2; exit 2; }
base_ref="${SM_CHANGED_BASE:-origin/main}"
# Without the remote-tracking branch (a clone whose remote has another name), fall back to the local main. A stale
# base only makes more paths count as changed.
if [[ -z "${SM_CHANGED_BASE:-}" ]] && ! git rev-parse -q --verify origin/main >/dev/null; then
  base_ref=main
fi
if ! base=$(git merge-base HEAD "$base_ref" 2>/dev/null); then
  echo "changed.sh: no merge base with $base_ref; treating everything as changed" >&2
  exit 0
fi
# Collected first: `grep -q` stops reading at the first match, and under pipefail the writer's SIGPIPE would turn a
# match into a failure.
paths="$(git diff --name-only "$base"; git ls-files --others --exclude-standard)"
grep -qE -- "$1" <<<"$paths"
