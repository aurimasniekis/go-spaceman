#!/usr/bin/env bash
# The race-detector run behind `make race` (and so `make check`): every Go package under -race, as concurrent go test
# invocations.
#
# internal/app is the critical path. Its end-to-end tests add up to about an hour of test time under the race
# detector, in two parts:
#   - about 20 tests that can't call t.Parallel (they restart central within a node lease, or change the process
#     environment or a package variable). go test runs these one after another before any parallel test starts:
#     about 260 s on a 32-core machine, whatever -parallel is;
#   - about 90 parallel tests, bounded by -parallel: at 4 (the cap the other packages need) they take about 5 minutes
#     more, so the package alone took about 9.
# So internal/app runs as its own invocation with its own cap (RACE_APP_PARALLEL), started first. On a machine with
# 16 or more CPUs (RACE_APP_SPLIT) it runs as two processes at the same time: one with its non-parallel tests (-run)
# and one with the rest (-skip, the same pattern), so every test still runs exactly once whatever the split finds.
# The tests that are serial are found in the source: a top-level Test function whose first statement isn't
# t.Parallel(). The other packages run alongside with RACE_PARALLEL, so a many-core machine doesn't starve their
# leases and write transactions. The output of internal/app is held back and printed when it finishes, so the
# invocations don't interleave.
#
# Environment (the Makefile passes its variables through):
#   GO                 go binary (default go)
#   RACE_PARALLEL      go test -parallel for every package except internal/app (default 4, CI's runner size)
#   RACE_APP_PARALLEL  go test -parallel for internal/app (default min(16, nproc/2), at least 4; CI sets 4)
#   RACE_APP_SPLIT     1 runs internal/app's serial tests in a process of their own, 0 doesn't (default 1 with 16 or
#                      more CPUs, else 0)
#   RACE_COUNT         go test -count when set (CI sets 1, so a cached pass never stands in for a run)
#   SM_TEST_TX_MAX     write-transaction guard (default 500ms, relaxed for the race detector as in CI)
set -euo pipefail

GO="${GO:-go}"
APP_PKG=github.com/aurimasniekis/go-spaceman/internal/app
APP_DIR=internal/app

cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
app_default=$(( cpus / 2 ))
(( app_default > 16 )) && app_default=16
(( app_default < 4 )) && app_default=4
split_default=0
(( cpus >= 16 )) && split_default=1
RACE_PARALLEL="${RACE_PARALLEL:-4}"
RACE_APP_PARALLEL="${RACE_APP_PARALLEL:-$app_default}"
RACE_APP_SPLIT="${RACE_APP_SPLIT:-$split_default}"
for v in RACE_PARALLEL RACE_APP_PARALLEL; do
  [[ "${!v}" =~ ^[1-9][0-9]*$ ]] || { echo "race: $v must be a positive integer" >&2; exit 2; }
done
[[ "$RACE_APP_SPLIT" =~ ^[01]$ ]] || { echo "race: RACE_APP_SPLIT must be 0 or 1" >&2; exit 2; }
[[ -z "${RACE_COUNT:-}" || "$RACE_COUNT" =~ ^[1-9][0-9]*$ ]] || { echo "race: RACE_COUNT must be a positive integer" >&2; exit 2; }
export SM_TEST_TX_MAX="${SM_TEST_TX_MAX:-500ms}"

# -race also turns on the compiler's checkptr instrumentation (unsafe.Pointer rule checks) in every package. In
# modernc.org's SQLite, C transpiled to Go with unsafe pointer arithmetic on almost every line, it was about a quarter
# of all test CPU and half the CPU spent inside write transactions, so on a CPU-bound runner it doubled how long they
# held the write lock (HP-DB-004 guard). It stays on for every other package, ours included; the race detector itself
# stays on everywhere. The non-race runs (coverage) have no checkptr to begin with.
flags=(-race -timeout 30m '-gcflags=modernc.org/...=-d=checkptr=0')
[[ -n "${RACE_COUNT:-}" ]] && flags+=("-count=$RACE_COUNT")

# The package list is captured first, so a failing go list fails the run instead of testing less.
all="$("$GO" list ./...)"
others=()
while read -r p; do
  [[ "$p" == "$APP_PKG" ]] || others+=("$p")
done <<<"$all"
grep -qxF "$APP_PKG" <<<"$all" || { echo "race: $APP_PKG not found in go list ./..." >&2; exit 1; }

# internal/app's serial tests: top-level `func TestX(t *testing.T) {` whose first statement (comments and blank lines
# skipped) isn't t.Parallel(). A wrong guess only changes which process runs a test, never whether it runs.
serial=""
if (( RACE_APP_SPLIT )); then
  serial=$(awk '
    /^func Test[A-Za-z0-9_]*\(t \*testing\.T\) \{$/ { name = $2; sub(/\(.*/, "", name); want = 1; next }
    want && /^[[:space:]]*(\/\/.*)?$/ { next }
    want { if ($0 !~ /^[[:space:]]*t\.Parallel\(\)[[:space:]]*$/) print name; want = 0 }
  ' "$APP_DIR"/*_test.go | sort -u | paste -sd'|' -)
fi

tmp=$(mktemp -d)
pids=()
# shellcheck disable=SC2329 # invoked by the EXIT trap
cleanup() {
  # Interrupted: stop the internal/app runs too, and the test binaries go test started for them.
  for pid in "${pids[@]}"; do
    pkill -TERM -P "$pid" 2>/dev/null || true
    kill -TERM "$pid" 2>/dev/null || true
  done
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

start=$SECONDS
names=()
if [[ -n "$serial" ]]; then
  echo "race: internal/app serial tests ($(tr -cd '|' <<<"$serial" | wc -c | awk '{print $1 + 1}')) and parallel tests" \
    "(-parallel $RACE_APP_PARALLEL) as two processes, ${#others[@]} other packages with -parallel $RACE_PARALLEL (SM_TEST_TX_MAX=$SM_TEST_TX_MAX)"
  "$GO" test "${flags[@]}" -run "^($serial)\$" "$APP_PKG" >"$tmp/app-serial.out" 2>&1 &
  pids+=($!); names+=("internal/app (serial tests)")
  "$GO" test "${flags[@]}" -parallel "$RACE_APP_PARALLEL" -skip "^($serial)\$" "$APP_PKG" >"$tmp/app-parallel.out" 2>&1 &
  pids+=($!); names+=("internal/app (parallel tests)")
  outs=("$tmp/app-serial.out" "$tmp/app-parallel.out")
else
  echo "race: internal/app with -parallel $RACE_APP_PARALLEL, ${#others[@]} other packages with -parallel $RACE_PARALLEL (SM_TEST_TX_MAX=$SM_TEST_TX_MAX)"
  "$GO" test "${flags[@]}" -parallel "$RACE_APP_PARALLEL" "$APP_PKG" >"$tmp/app.out" 2>&1 &
  pids+=($!); names+=("internal/app")
  outs=("$tmp/app.out")
fi

# The other packages stream their output as they go, but run in the background like the rest: bash runs a trap only
# between commands, and `wait` returns at once on a trapped signal, so a TERM stops every invocation right away. (A
# kill -9 of this script can't run the trap: its go test children go on running; stop the process group instead.)
"$GO" test "${flags[@]}" -parallel "$RACE_PARALLEL" "${others[@]}" &
others_pid=$!
pids+=("$others_pid")
rc=0
wait "$others_pid" || rc=$?
summary="other packages done after $(( SECONDS - start )) s (exit $rc)"

for (( i = 0; i < ${#names[@]}; i++ )); do
  prc=0
  wait "${pids[$i]}" || prc=$?
  echo "== ${names[$i]}"
  cat "${outs[$i]}"
  summary+=", ${names[$i]} (exit $prc)"
  (( prc == 0 )) || rc=$prc
done
pids=()
echo "race: $summary, total $(( SECONDS - start )) s"
exit "$rc"
