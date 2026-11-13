#!/usr/bin/env bash
# Enforce the statement-coverage floor for security-critical packages.
# Packages that do not exist yet are skipped and reported, so the floor applies as soon as a package lands.
#
# All packages are tested in one `go test -count=1 -cover` invocation, COVERAGE_PARALLEL packages at a time (below),
# and each package's result is read from go test's summary lines. A package fails the floor
# when its tests fail (or don't build), when its coverage is below COVERAGE_FLOOR, and when go test reports no coverage
# for it at all.
set -euo pipefail

FLOOR="${COVERAGE_FLOOR:-80}"
MODULE="github.com/aurimasniekis/go-spaceman"
PACKAGES=(
  internal/keyring
  internal/audit
  internal/auth
  internal/authz
  internal/oidc
  internal/proto
  internal/approvals
  internal/events/template
  internal/attachments
  internal/human
  internal/accounts
  internal/agenttools
  internal/catalog
  internal/messages
  internal/gateway
  internal/harness/dist
  internal/spacemanmcp
  internal/memory
  internal/usage
  internal/outbox
  internal/backup
  internal/s3client
  internal/artifacts
  internal/limits
  internal/retention
  internal/spaceexport
  internal/schedule
  internal/xspace
  internal/cli
)

present=()
for pkg in "${PACKAGES[@]}"; do
  if [[ ! -d "$pkg" ]]; then
    echo "skip  $pkg (not present yet)"
    continue
  fi
  present+=("./$pkg")
done
[[ ${#present[@]} -gt 0 ]] || { echo "FAIL  no floor package present"; exit 1; }

# How many packages run their tests at once (go test -p). Their store tests enforce the write-transaction guard
# (SM_TEST_TX_MAX), and too many at once make the CPU contention that trips it: all 28 at once exceeded 250 ms on a
# 4-core CI runner, which passes one package at a time. The default is a quarter of the CPUs, at least 1 and at most
# 8; CI sets 1. The test binaries are built first with full parallelism, so the limit only applies to running them.
cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
default_parallel=$(( cpus / 4 ))
(( default_parallel < 1 )) && default_parallel=1
(( default_parallel > 8 )) && default_parallel=8
PARALLEL="${COVERAGE_PARALLEL:-$default_parallel}"
[[ "$PARALLEL" =~ ^[1-9][0-9]*$ ]] || { echo "FAIL  COVERAGE_PARALLEL must be a positive integer"; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# A build failure here is reported again, per package, by the run below.
go test -cover -c -o "$tmp" "${present[@]}" >/dev/null 2>&1 || true

status=0
out=$(go test -p "$PARALLEL" -count=1 -cover "${present[@]}" 2>&1) || status=$?
# Passing packages print one summary line each; a failure prints its test output, which is shown whole.
if [[ "$status" -ne 0 ]]; then
  echo "$out"
fi

fail=0
for p in "${present[@]}"; do
  pkg="${p#./}"
  full="$MODULE/$pkg"
  # Summary lines: "ok  <pkg> 1.2s coverage: N% of statements", "FAIL <pkg> 1.2s" or "FAIL <pkg> [build failed]", and
  # "<tab><pkg><tab><tab>coverage: 0.0% of statements" for a package without tests. Fields are tab-separated, and the
  # path is compared exactly.
  if awk -F'\t' -v p="$full" '$1 == "FAIL" { split($2, f, " "); if (f[1] == p) found = 1 } END { exit !found }' <<<"$out"; then
    echo "FAIL  $pkg (tests failed)"
    fail=1
    continue
  fi
  pct=$(awk -F'\t' -v p="$full" '
    $1 != "FAIL" && $2 == p {
      if (match($0, /coverage: [0-9.]+% of statements/)) { s = substr($0, RSTART + 10); sub(/%.*/, "", s); print s; exit }
    }' <<<"$out")
  if [[ -z "$pct" ]]; then
    echo "FAIL  $pkg (no coverage reported)"
    fail=1
    continue
  fi
  if awk -v p="$pct" -v f="$FLOOR" 'BEGIN { exit !(p + 0 < f + 0) }'; then
    echo "FAIL  $full ${pct}% < ${FLOOR}%"
    fail=1
  else
    echo "ok    $full ${pct}%"
  fi
done

# go test failed for a reason no package line explains (a bad flag, a module error): never pass silently.
if [[ "$status" -ne 0 && "$fail" -eq 0 ]]; then
  echo "FAIL  go test exited with status $status"
  fail=1
fi
exit "$fail"
