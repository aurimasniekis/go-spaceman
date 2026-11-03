#!/usr/bin/env bash
# Linux/systemd smoke test (SM-PKG-001, SM-PKG-003, SM-VER-002): installs a linux release archive into a Debian 13
# container whose PID 1 is systemd, following site/operators/install.md, and runs the shipped units.
#
# Usage: scripts/systemd-smoke.sh [ARCHIVE]
#   ARCHIVE is a spaceman_<version>_linux_<arch>.tar.gz for the Docker VM's architecture. Without it, the script builds
#   one with scripts/release-build.sh into a temp dir (UI embedded when web/dist exists, otherwise ALLOW_NO_UI=1).
#   SMOKE_ARCH=amd64|arm64 selects another architecture under Docker's emulation. On Docker Desktop 29.8 (arm64 Mac)
#   amd64 does not get this far: systemd's own sandboxed services (journald, tmpfiles, sysusers) fail under emulation
#   and the system ends up 'degraded' (2026-11-03), so amd64 needs a real amd64 Docker host.
#
# Checks, in the container:
#   1. the archive layout (the units are deploy/systemd's); `systemd-analyze verify` of each unit once installed
#   2. central installed as the guide says: binary, system user, /etc/spaceman (0640 root:spaceman config, the
#      example configuration unchanged), `keys init-kek` (0400 spaceman:spaceman), `admin bootstrap` as the service user
#   3. `systemctl enable --now spaceman`: active, main process runs as `spaceman`, data dir 0700, /healthz and /readyz on
#      the management port, the API (and the embedded UI when built) on the public port, no error-level journal lines
#   4. `systemctl stop` (clean exit, Result=success) and `start` again, then `restart`, healthy each time
#   5. `systemd-analyze security spaceman.service` exposure score (printed; the unit is the shipped one, unedited)
#   6. the unit's AssertPathExists refuses a start without the KEK
#   7. spaceman-agent.service: without enrollment the unit's assertion refuses the start (no restart loop); with a
#      placeholder credential and no herdr-core installed, `run` refuses with a herdr-core error, and Restart=on-failure
#      retries every RestartSec until stopped (recorded, then stopped); `systemd-analyze security` score
#
# Container settings, chosen as the least privilege that works on Docker Desktop 29.8 (cgroup v2, linux VM):
#   --cgroupns=private --cap-add SYS_ADMIN, and PID 1 is `sh -c 'mount -o remount,rw /sys/fs/cgroup; exec /sbin/init'`.
#     Without SYS_ADMIN Docker mounts the namespaced cgroup2 tree read-only and systemd exits ("Failed to create
#     /init.scope control group: Read-only file system"). SYS_ADMIN lets PID 1 remount its own cgroup namespace
#     read-write and lets systemd set up the units' mount namespaces (ProtectSystem=, PrivateTmp=, ...). It is not
#     --privileged: no host devices, the default seccomp profile, and the host cgroup tree is not mounted.
#   --tmpfs /run --tmpfs /run/lock: systemd expects them as tmpfs.
#   --network none: nothing is published; every HTTP check runs inside the container.
# Only the container and the image this script creates are removed afterwards (by recorded ID and unique tag).
#
# Needs docker and go on the host.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
archive=${1:-}

# Debian 13 (trixie), pinned by index digest (docker buildx imagetools inspect debian:trixie, 2026-11-03).
base_image=docker.io/library/debian:trixie@sha256:2dc419c6d7fe3dad0a2f9fd469c34cc90b0f9ba642d8fde837fe3d7d34a0e7f1
tag="spaceman-systemd-smoke:$$-$RANDOM"
work=$(mktemp -d)
cid=""
built_image=false

cleanup() {
  local rc=$?
  if [[ -n $cid ]]; then
    if [[ $rc -ne 0 ]]; then
      docker exec "$cid" journalctl --no-pager -n 60 -o short-precise 2>/dev/null | sed 's/^/  journal: /' >&2 || true
    fi
    docker rm -f "$cid" >/dev/null 2>&1 || true
  fi
  if $built_image; then docker rmi "$tag" >/dev/null 2>&1 || true; fi
  rm -rf "$work"
  if [[ $rc -eq 0 ]]; then echo "systemd smoke: PASS"; else echo "systemd smoke: FAIL" >&2; fi
}
trap cleanup EXIT

step() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
in_c() { docker exec "$cid" "$@"; }
sh_c() { docker exec "$cid" sh -euc "$1"; }

case "${SMOKE_ARCH:-$(docker info --format '{{.Architecture}}')}" in
  aarch64 | arm64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) fail "unsupported architecture" ;;
esac
platform=(--platform "linux/$arch")

# ---- release archive -------------------------------------------------------------------------------------------------
if [[ -z $archive ]]; then
  step "build: linux/$arch release archive (scripts/release-build.sh)"
  if [[ ! -f $root/web/dist/index.html ]]; then
    echo "web/dist missing: building without the embedded UI (run 'make web-build' first to include it)"
    export ALLOW_NO_UI=1
  fi
  SBOM=skip "$root/scripts/release-build.sh" -o "$work/dist" "linux/$arch" >/dev/null
  archive=$(find "$work/dist" -name "spaceman_*_linux_${arch}.tar.gz" | head -n 1)
fi
[[ -f $archive ]] || fail "no archive at '$archive'"
[[ $(basename "$archive") == *"_linux_${arch}.tar.gz" ]] || fail "archive $(basename "$archive") is not for linux/$arch"
echo "archive: $(basename "$archive")"

# ---- image and container ---------------------------------------------------------------------------------------------
step "image: $base_image + systemd, dbus, git, curl"
mkdir -p "$work/image"
cat >"$work/image/Dockerfile" <<EOF
FROM $base_image
RUN apt-get update \\
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd systemd-sysv dbus git curl ca-certificates \\
 && rm -rf /var/lib/apt/lists/*
STOPSIGNAL SIGRTMIN+3
EOF
docker build -q "${platform[@]}" -t "$tag" "$work/image" >/dev/null
built_image=true

cid=$(docker run -d "${platform[@]}" --cgroupns=private --cap-add SYS_ADMIN --tmpfs /run --tmpfs /run/lock --network none \
  "$tag" sh -c 'mount -o remount,rw /sys/fs/cgroup && exec /sbin/init')
for _ in $(seq 1 60); do
  state=$(in_c systemctl is-system-running 2>/dev/null || true)
  [[ $state == running || $state == degraded ]] && break
  sleep 0.5
done
[[ $state == running ]] || fail "systemd is '$state' (want running): $(in_c systemctl --failed --no-legend 2>&1 || true)"
echo "PID 1: $(in_c cat /proc/1/comm), $(in_c systemctl --version | head -n 1), system state $state"
echo "os: $(in_c grep -oP "(?<=^PRETTY_NAME=\")[^\"]+" /etc/os-release), kernel $(in_c uname -r)"

docker cp "$archive" "$cid:/root/spaceman.tar.gz" >/dev/null
sh_c 'mkdir /root/release && tar -xzf /root/spaceman.tar.gz -C /root/release --strip-components=1'

step "archive layout"
for f in spaceman spaceman-agent systemd/spaceman.service systemd/spaceman-agent.service examples/spaceman.example.yaml \
  examples/agent.example.yaml LICENSE NOTICE THIRD_PARTY.md CHANGELOG.md; do
  in_c test -f "/root/release/$f" || fail "archive lacks $f"
done
in_c /root/release/spaceman version --json | tr -d '\n'
echo
for unit in spaceman.service spaceman-agent.service; do
  diff -q <(in_c cat "/root/release/systemd/$unit") "$root/deploy/systemd/$unit" >/dev/null || fail "$unit differs from deploy/systemd"
done

# ---- central ---------------------------------------------------------------------------------------------------------
step "central: install as site/operators/install.md says"
sh_c '
cd /root/release
install -m 0755 -o root -g root spaceman /usr/local/bin/spaceman
useradd --system --home-dir /var/lib/spaceman --shell /usr/sbin/nologin spaceman
install -d -m 0700 -o spaceman -g spaceman /var/lib/spaceman
install -d -m 0755 /etc/spaceman
install -m 0640 -o root -g spaceman examples/spaceman.example.yaml /etc/spaceman/spaceman.yaml
spaceman keys init-kek --out /etc/spaceman/kek >/dev/null
chown spaceman:spaceman /etc/spaceman/kek
install -m 0644 systemd/spaceman.service /etc/systemd/system/
systemctl daemon-reload'
in_c systemd-analyze verify /etc/systemd/system/spaceman.service
echo "systemd-analyze verify spaceman.service: clean"
# The example configuration is used unchanged: its placeholder https public_url is valid in production mode, and the
# listeners (:8080, management 127.0.0.1:9090) are the guide's.
echo "kek: $(in_c stat -c '%a %U:%G' /etc/spaceman/kek), config: $(in_c stat -c '%a %U:%G' /etc/spaceman/spaceman.yaml)"
[[ $(in_c stat -c '%a %U:%G' /etc/spaceman/kek) == "400 spaceman:spaceman" ]] || fail "KEK mode/owner"
# The root token is printed once; it is checked for, never shown.
sh_c 'runuser -u spaceman -- spaceman -c /etc/spaceman/spaceman.yaml admin bootstrap 2>/dev/null | grep -q spaceman_rt_' \
  || fail "admin bootstrap did not issue a root token"
echo "admin bootstrap: root token issued (not shown)"

wait_ready() {
  for _ in $(seq 1 120); do
    if in_c curl -fsS -o /dev/null http://127.0.0.1:9090/readyz 2>/dev/null; then return 0; fi
    sleep 0.5
  done
  fail "/readyz did not answer: $(in_c systemctl status --no-pager spaceman 2>&1 | tail -n 5)"
}
check_central() {
  [[ $(in_c systemctl is-active spaceman) == active ]] || fail "spaceman is not active"
  wait_ready
  local pid user
  pid=$(in_c systemctl show -P MainPID spaceman)
  user=$(in_c stat -c %U "/proc/$pid")
  [[ $user == spaceman ]] || fail "main process runs as $user"
  echo "active, pid $pid as $user; healthz $(in_c curl -fsS http://127.0.0.1:9090/healthz | tr -d '\n'); readyz $(in_c curl -fsS http://127.0.0.1:9090/readyz | tr -d '\n')"
}

step "central: systemctl enable --now spaceman"
in_c systemctl enable --now spaceman 2>&1 | sed 's/^/  /'
check_central
[[ $(in_c stat -c '%a %U' /var/lib/spaceman) == "700 spaceman" ]] || fail "data dir mode/owner"
info=$(in_c curl -fsS http://127.0.0.1:8080/api/v1/system/info)
[[ $info == *'"version"'* ]] || fail "system info: $info"
echo "api: /api/v1/system/info answered"
if in_c curl -fsS http://127.0.0.1:8080/ | grep -q '<div id="app"\|<div id="root"\|<script type="module"'; then
  echo "ui: embedded UI served on the public port"
elif [[ ${ALLOW_NO_UI:-} == 1 ]]; then
  echo "ui: not embedded in this build (ALLOW_NO_UI=1)"
else
  fail "the embedded UI is not served on the public port"
fi
in_c journalctl -u spaceman -o cat --no-pager | grep -q 'until git is installed' && fail "central did not find git"

step "central: stop, start, restart"
in_c systemctl stop spaceman
[[ $(in_c systemctl is-active spaceman || true) == inactive ]] || fail "spaceman did not stop"
result=$(in_c systemctl show -P Result spaceman)
status=$(in_c systemctl show -P ExecMainStatus spaceman)
[[ $result == success ]] || fail "stop result '$result' (exit status $status)"
echo "stopped: Result=$result ExecMainStatus=$status"
in_c systemctl start spaceman
check_central
in_c systemctl restart spaceman
check_central
errors=$(in_c journalctl -u spaceman -o cat --no-pager | grep -c '"log.level":"error"' || true)
[[ $errors -eq 0 ]] || fail "$errors error-level journal lines: $(in_c journalctl -u spaceman -o cat --no-pager | grep '"log.level":"error"' | head -n 3)"
echo "journal: no error-level lines across 3 starts"

step "central: systemd-analyze security spaceman.service"
in_c systemd-analyze security --no-pager spaceman.service | tail -n 1

step "central: AssertPathExists refuses a start without the KEK"
in_c systemctl stop spaceman
in_c mv /etc/spaceman/kek /etc/spaceman/kek.moved
if in_c systemctl start spaceman 2>/dev/null; then fail "spaceman started without the KEK"; fi
[[ $(in_c systemctl show -P AssertResult spaceman) == no ]] || fail "assertion did not refuse the start"
echo "refused: AssertResult=no ($(in_c systemctl is-active spaceman || true))"
in_c mv /etc/spaceman/kek.moved /etc/spaceman/kek
in_c systemctl start spaceman
check_central

# ---- node agent ------------------------------------------------------------------------------------------------------
step "agent: install as site/operators/install.md says (no herdr-core, no harnesses)"
sh_c '
cd /root/release
install -m 0755 -o root -g root spaceman-agent /usr/local/bin/spaceman-agent
useradd --system --home-dir /var/lib/spaceman-agent --shell /bin/sh spaceman-agent
install -d -m 0700 -o spaceman-agent -g spaceman-agent /var/lib/spaceman-agent
install -d -m 0755 /etc/spaceman-agent
install -m 0640 -o root -g spaceman-agent examples/agent.example.yaml /etc/spaceman-agent/agent.yaml
install -m 0644 systemd/spaceman-agent.service /etc/systemd/system/
systemctl daemon-reload'
in_c systemd-analyze verify /etc/systemd/system/spaceman-agent.service
echo "systemd-analyze verify spaceman-agent.service: clean"
for bin in hc claude codex; do
  if in_c sh -c "command -v $bin" >/dev/null 2>&1; then fail "$bin is installed; this check needs a node without it"; fi
done
in_c runuser -u spaceman-agent -- spaceman-agent version --json | tr -d '\n'
echo

step "agent: the unit refuses to start before enrollment"
if in_c systemctl start spaceman-agent 2>/dev/null; then fail "spaceman-agent started without a node credential"; fi
[[ $(in_c systemctl show -P AssertResult spaceman-agent) == no ]] || fail "the credential assertion did not refuse"
sleep 6
restarts=$(in_c systemctl show -P NRestarts spaceman-agent)
[[ $restarts -eq 0 ]] || fail "assertion failure restarted $restarts times"
echo "refused: AssertResult=no, $(in_c systemctl is-active spaceman-agent || true), NRestarts=$restarts after 6 s"

step "agent: with a placeholder credential, run refuses without herdr-core"
# Not a credential: the agent stops at herdr-core before it sends it anywhere (the node has no network either).
sh_c 'umask 077; echo placeholder-not-a-credential > /var/lib/spaceman-agent/node.credential; chown spaceman-agent:spaceman-agent /var/lib/spaceman-agent/node.credential'
in_c systemctl start spaceman-agent || true
# The process's own lines only (_COMM), so the unit description, which names herdr-core, does not match.
refusal=""
for _ in $(seq 1 40); do
  refusal=$(in_c journalctl -u spaceman-agent _COMM=spaceman-agent -o cat --no-pager | grep -m 1 'herdr-core: ' || true)
  [[ -n $refusal ]] && break
  sleep 0.25
done
[[ -n $refusal ]] || fail "no herdr-core refusal in the journal: $(in_c journalctl -u spaceman-agent -o cat --no-pager | tail -n 5)"
echo "refused: $refusal"
sleep 12
restarts=$(in_c systemctl show -P NRestarts spaceman-agent)
echo "Restart=on-failure: NRestarts=$restarts after 12 s (RestartSec=5s), ExecMainStatus=$(in_c systemctl show -P ExecMainStatus spaceman-agent)"
in_c systemctl stop spaceman-agent
echo "stopped by the operator: $(in_c systemctl is-active spaceman-agent || true)"

step "agent: systemd-analyze security spaceman-agent.service"
in_c systemd-analyze security --no-pager spaceman-agent.service | tail -n 1

[[ $(in_c systemctl is-active spaceman) == active ]] || fail "central stopped while the agent was tested"
