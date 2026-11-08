#!/usr/bin/env bash
# Enforce the statement-coverage floor for security-critical packages.
# Packages that do not exist yet are skipped and reported, so the floor applies as soon as a package lands.
#
# All packages are tested in one `go test -count=1 -cover` invocation (go test runs them in parallel; about 20 s instead
# of about 150 s one by one), and each package's result is read from go test's summary lines. A package fails the floor
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

status=0
out=$(go test -count=1 -cover "${present[@]}" 2>&1) || status=$?
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
