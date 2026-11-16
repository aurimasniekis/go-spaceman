#!/usr/bin/env bash
# Usage: scripts/gate-scripts-test.sh
#
# Quick checks of scripts/gate-slot.sh and scripts/race.sh (a few seconds, no Go build): run it after changing either.
#   - without flock in PATH, gate-slot warns and runs the command;
#   - CI=true disables the slots even when SM_GATE_SLOTS is set;
#   - with SM_GATE_SLOTS=1 a second run prints a waiting line and runs after the first;
#   - a holder killed with kill -9 releases its slot;
#   - race.sh stopped with TERM exits at once and stops every go test invocation (a fake GO);
#   - race.sh exiting normally with RACE_OUT_DIR leaves no sampler sleep behind (a fake GO and sleep).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
gate="$root/scripts/gate-slot.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1" >&2; fails=$((fails + 1)); }
export SM_GATE_LOCK_DIR="$tmp/locks"
unset CI SM_GATE_HELD

# No flock: a PATH with only getconf.
mkdir "$tmp/bin"
ln -s "$(command -v getconf)" "$tmp/bin/getconf"
out=$(timeout 5 env PATH="$tmp/bin" SM_GATE_SLOTS=1 "$BASH" "$gate" t /bin/echo ran 2>&1) || true
if [[ "$out" == *"flock not found"* && "$out" == *ran* ]]; then ok "no flock: warns and runs"; else bad "no flock: $out"; fi

# A slot held for the next checks.
# shellcheck disable=SC2016 # expanded by the inner bash
SM_GATE_SLOTS=1 "$gate" holder bash -c 'echo $$ >"$1"; exec sleep 30' _ "$tmp/holder-child" &
holder=$!
for _ in $(seq 50); do [[ -e "$SM_GATE_LOCK_DIR/slot-0.owner" ]] && break; sleep 0.1; done

out=$(CI=true SM_GATE_SLOTS=1 timeout 5 "$gate" t echo ran 2>&1) || true
if [[ "$out" == ran ]]; then ok "CI=true ignores SM_GATE_SLOTS"; else bad "CI=true: $out"; fi

SM_GATE_SLOTS=1 timeout 10 "$gate" waiter echo ran >"$tmp/waiter.out" 2>&1 &
waiter=$!
sleep 1.5
if grep -q "waiting for a heavy-gate slot" "$tmp/waiter.out" && ! grep -q '^ran$' "$tmp/waiter.out"; then
  ok "busy slot: prints a waiting line and waits"
else
  bad "busy slot: $(cat "$tmp/waiter.out")"
fi

kill -9 "$holder"
wait "$holder" 2>/dev/null || true
rc=0
wait "$waiter" || rc=$?
if (( rc == 0 )) && grep -q '^ran$' "$tmp/waiter.out" && grep -q "got slot 0" "$tmp/waiter.out"; then
  ok "kill -9 of the holder releases its slot"
else
  bad "kill -9: exit $rc: $(cat "$tmp/waiter.out")"
fi
kill "$(cat "$tmp/holder-child")" 2>/dev/null || true # the command itself outlives a kill -9 of the wrapper

# race.sh with a fake go: every `go test` records its pid and sleeps.
cat >"$tmp/go" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  list) printf '%s\n' github.com/aurimasniekis/go-spaceman/internal/app github.com/aurimasniekis/go-spaceman/internal/x ;;
  test) echo $$ >>"$FAKE_GO_PIDS"; sleep 60 & wait ;;
esac
EOF
chmod +x "$tmp/go"
export FAKE_GO_PIDS="$tmp/pids"
: >"$FAKE_GO_PIDS"
(cd "$root" && GO="$tmp/go" RACE_APP_SPLIT=1 exec ./scripts/race.sh) >"$tmp/race.out" 2>&1 &
race=$!
for _ in $(seq 50); do (( $(wc -l <"$FAKE_GO_PIDS") >= 3 )) && break; sleep 0.1; done
start=$SECONDS
kill -TERM "$race"
rc=0
wait "$race" || rc=$?
sleep 0.5
alive=0
while read -r p; do kill -0 "$p" 2>/dev/null && alive=$((alive + 1)); done <"$FAKE_GO_PIDS"
if (( rc == 143 && SECONDS - start <= 3 && alive == 0 )) && [[ $(wc -l <"$FAKE_GO_PIDS") -eq 3 ]]; then
  ok "race.sh on TERM: exits 143 at once and stops all 3 invocations"
else
  bad "race.sh on TERM: exit $rc after $(( SECONDS - start )) s, $alive of $(wc -l <"$FAKE_GO_PIDS") still running"
  while read -r p; do pkill -P "$p" 2>/dev/null; kill "$p" 2>/dev/null; done <"$FAKE_GO_PIDS" || true
fi

# race.sh with RACE_OUT_DIR (the mem.log sampler) exits normally: the sampler's sleep 20 must not outlive it, or a
# caller that captures the output waits for it. A sleep wrapper records its pid (exec keeps it); the fake go waits
# until the sampler has started a sleep, so the exit happens mid-sleep.
export REAL_SLEEP SLEEP_PIDS="$tmp/sleep-pids"
REAL_SLEEP=$(command -v sleep)
mkdir "$tmp/sleepbin"
cat >"$tmp/sleepbin/sleep" <<'EOF'
#!/usr/bin/env bash
echo $$ >>"$SLEEP_PIDS"
exec "$REAL_SLEEP" "$@"
EOF
cat >"$tmp/go-quick" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  list) printf '%s\n' github.com/aurimasniekis/go-spaceman/internal/app github.com/aurimasniekis/go-spaceman/internal/x ;;
  test) for _ in $(seq 100); do [[ -s "$SLEEP_PIDS" ]] && break; "$REAL_SLEEP" 0.1; done ;;
esac
EOF
chmod +x "$tmp/sleepbin/sleep" "$tmp/go-quick"
start=$SECONDS
rc=0
out=$(cd "$root" && PATH="$tmp/sleepbin:$PATH" GO="$tmp/go-quick" RACE_OUT_DIR="$tmp/out" RACE_APP_SPLIT=0 \
  timeout 60 ./scripts/race.sh 2>&1) || rc=$?
elapsed=$(( SECONDS - start ))
sleeps=$(wc -l <"$SLEEP_PIDS")
alive=0
while read -r p; do
  st=$(ps -o stat= -p "$p" 2>/dev/null || true)
  [[ -n "$st" && "$st" != Z* ]] && alive=$((alive + 1))
  kill "$p" 2>/dev/null || true
done <"$SLEEP_PIDS"
if (( rc == 0 && elapsed <= 5 && sleeps >= 1 && alive == 0 )) && [[ "$out" == *"race: other packages done"* ]]; then
  ok "race.sh exits at once with RACE_OUT_DIR: no sampler sleep left behind"
else
  bad "race.sh normal exit: exit $rc after $elapsed s, $sleeps sampler sleep(s), $alive still running: $out"
fi

(( fails == 0 )) || { echo "$fails check(s) failed" >&2; exit 1; }
echo "all gate script checks passed"
