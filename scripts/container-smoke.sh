#!/usr/bin/env bash
# Container smoke test for the SpaceMan images (ADR-0034, site/operators/containers.md).
#
# Usage: scripts/container-smoke.sh CENTRAL_IMAGE [AGENT_IMAGE]
#
# Builds nothing and pushes nothing. Checks the central image:
#   1. metadata: non-root numeric user, exposed ports, data volume, contents labels
#   2. contents: git present; herdr-core, harness CLIs and spaceman-agent absent
#   3. `spaceman version --json`
#   4. KEK provisioning with `spaceman keys init-kek` into a separate volume (0400, UID 65532)
#   5. a KEK with group/other permissions is refused
#   6. `spaceman serve` with a production config, read-only root and no capabilities: /healthz and /readyz on the
#      management port, the embedded UI on the public port, git found (no "until git is installed" warning), exit 0
#      on SIGTERM
#   7. `spaceman backup create` and `backup verify` against the data the server left
# and, when AGENT_IMAGE is given, the node agent base image:
#   8. metadata and labels say it is not agent-ready; git and ssh present; hc, claude and codex absent
#   9. `spaceman-agent version --json`; `run` refuses to start without a node credential, and without herdr-core
#
# Needs docker and curl on the host.
set -euo pipefail

image=${1:?usage: $0 CENTRAL_IMAGE [AGENT_IMAGE]}
agent_image=${2:-}
id="spaceman-smoke-$$"
data_vol="$id-data"
kek_vol="$id-kek"
agent_vol="$id-agent"
cfg_dir=$(mktemp -d)
container=""

cleanup() {
  local rc=$?
  if [[ -n $container ]]; then
    if [[ $rc -ne 0 ]]; then docker logs "$container" 2>&1 | tail -n 50 >&2 || true; fi
    docker rm -f "$container" >/dev/null 2>&1 || true
  fi
  docker volume rm -f "$data_vol" "$kek_vol" "$agent_vol" >/dev/null 2>&1 || true
  rm -rf "$cfg_dir"
  if [[ $rc -eq 0 ]]; then echo "container smoke: PASS"; else echo "container smoke: FAIL" >&2; fi
}
trap cleanup EXIT

step() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
label() { docker image inspect -f "{{ index .Config.Labels \"$2\" }}" "$1"; }

# Hardened run options shared by every invocation (mirrors deploy/compose.yaml).
run_opts=(--rm --read-only --cap-drop ALL --security-opt no-new-privileges:true --user 65532:65532
  --tmpfs "/tmp:size=16m,mode=1777")

# ---- central ---------------------------------------------------------------------------------------------------------
step "central: image metadata"
user=$(docker image inspect -f '{{.Config.User}}' "$image")
[[ $user == "65532:65532" ]] || fail "image user is '$user', want 65532:65532"
ports=$(docker image inspect -f '{{json .Config.ExposedPorts}}' "$image")
[[ $ports == *'"8080/tcp"'* && $ports == *'"9090/tcp"'* ]] || fail "exposed ports $ports"
vols=$(docker image inspect -f '{{json .Config.Volumes}}' "$image")
[[ $vols == *'"/var/lib/spaceman"'* ]] || fail "volumes $vols"
for kv in contains.git=true contains.herdr-core=false contains.harnesses=false agent-ready=false; do
  got=$(label "$image" "io.spaceman.${kv%=*}")
  [[ $got == "${kv#*=}" ]] || fail "label io.spaceman.${kv%=*} is '$got', want '${kv#*=}'"
done
echo "user=$user ports=$ports volumes=$vols labels ok"

step "central: contents (git yes; herdr-core, harnesses, spaceman-agent no)"
docker run "${run_opts[@]}" --entrypoint git "$image" --version
for bin in hc herdr-core claude codex spaceman-agent; do
  if docker run "${run_opts[@]}" --entrypoint sh "$image" -c "command -v $bin" >/dev/null 2>&1; then
    fail "central image contains $bin"
  fi
done
echo "absent: hc herdr-core claude codex spaceman-agent"

step "central: version"
docker run "${run_opts[@]}" "$image" version --json | tee "$cfg_dir/version.json"
grep -q '"version"' "$cfg_dir/version.json" || fail "version output"

step "central: provision the KEK in a separate volume"
docker volume create "$kek_vol" >/dev/null
docker volume create "$data_vol" >/dev/null
# The KEK volume starts root-owned; hand a directory in it to the service UID.
docker run --rm --user 0:0 --entrypoint install -v "$kek_vol:/kek" "$image" -d -m 0700 -o 65532 -g 65532 /kek/spaceman
docker run "${run_opts[@]}" -v "$kek_vol:/kek" "$image" keys init-kek --out /kek/spaceman/kek >/dev/null
mode=$(docker run --rm --user 0:0 --entrypoint stat -v "$kek_vol:/kek:ro" "$image" -c '%a %u:%g' /kek/spaceman/kek)
[[ $mode == "400 65532:65532" ]] || fail "KEK mode/owner is '$mode', want '400 65532:65532'"
echo "KEK mode/owner: $mode"

cat >"$cfg_dir/spaceman.yaml" <<'EOF'
mode: production
data_dir: /var/lib/spaceman
public_url: https://spaceman.example.test
kek:
  source: file
  file: /run/secrets/spaceman/kek
http:
  listen: ":8080"
  management_listen: "0.0.0.0:9090"
log:
  level: info
  format: json
shutdown:
  timeout: 10s
EOF
chmod 0644 "$cfg_dir/spaceman.yaml"
mounts=(-v "$data_vol:/var/lib/spaceman" -v "$kek_vol:/run/secrets:ro"
  -v "$cfg_dir/spaceman.yaml:/etc/spaceman/spaceman.yaml:ro")

step "central: configuration accepted (backup list loads and validates it)"
docker run "${run_opts[@]}" "${mounts[@]}" "$image" backup list

step "central: KEK with group/other permissions is refused"
docker run --rm --user 0:0 --entrypoint sh -v "$kek_vol:/kek" "$image" \
  -c 'cp -p /kek/spaceman/kek /kek/spaceman/kek-loose && chmod 0440 /kek/spaceman/kek-loose'
if out=$(docker run "${run_opts[@]}" "${mounts[@]}" -e SPACEMAN_KEK_FILE=/run/secrets/spaceman/kek-loose \
  "$image" backup create 2>&1); then
  fail "backup create accepted a 0440 KEK: $out"
fi
[[ $out == *"must not be accessible by group or others"* ]] || fail "unexpected refusal message: $out"
echo "refused: ${out##*spaceman: }"

step "central: serve (health, readiness, embedded UI, git, graceful SIGTERM)"
container=$(docker run -d --read-only --cap-drop ALL --security-opt no-new-privileges:true --user 65532:65532 \
  --tmpfs "/tmp:size=16m,mode=1777" "${mounts[@]}" -p 127.0.0.1::8080 -p 127.0.0.1::9090 "$image" serve)
mgmt="" public="" health=""
for _ in $(seq 1 60); do
  mgmt=$(docker port "$container" 9090/tcp 2>/dev/null | head -n1) || true
  public=$(docker port "$container" 8080/tcp 2>/dev/null | head -n1) || true
  if [[ -n $mgmt ]] && health=$(curl -fsS --max-time 2 "http://$mgmt/healthz" 2>/dev/null); then break; fi
  [[ $(docker inspect -f '{{.State.Running}}' "$container") == true ]] || fail "serve exited early"
  sleep 0.5
done
[[ $health == *'"ok"'* ]] || fail "no /healthz on ${mgmt:-?}: '$health'"
echo "/healthz: $health"
ready=""
for _ in $(seq 1 40); do
  if ready=$(curl -fsS --max-time 2 "http://$mgmt/readyz" 2>/dev/null); then break; fi
  sleep 0.5
done
[[ -n $ready ]] || fail "/readyz did not become ready on $mgmt"
echo "/readyz: $ready"
ui=$(curl -fsS --max-time 5 "http://$public/" 2>/dev/null) || fail "embedded UI not served on $public"
[[ $ui == *'<html'* ]] || fail "public / did not return the UI's HTML"
echo "embedded UI served on the public listener"
if docker logs "$container" 2>&1 | grep -q 'until git is installed'; then fail "central did not find git"; fi
echo "git found by central (no missing-git warning)"
docker stop -t 30 "$container" >/dev/null
code=$(docker inspect -f '{{.State.ExitCode}}' "$container")
[[ $code == 0 ]] || fail "serve exited $code after SIGTERM"
echo "serve exited 0 after SIGTERM"
docker rm -f "$container" >/dev/null
container=""

step "central: backup create and verify"
docker run "${run_opts[@]}" "${mounts[@]}" "$image" backup create --json >"$cfg_dir/backup.json"
bid=$(grep -o '"id":"bkp_[^"]*"' "$cfg_dir/backup.json" | head -n1 | cut -d'"' -f4)
[[ -n $bid ]] || fail "no backup ID in backup create output"
echo "backup $bid created"
docker run "${run_opts[@]}" "${mounts[@]}" "$image" backup verify "$bid"

# ---- node agent base image -------------------------------------------------------------------------------------------
if [[ -z $agent_image ]]; then
  exit 0
fi

step "agent: image metadata (not agent-ready)"
user=$(docker image inspect -f '{{.Config.User}}' "$agent_image")
[[ $user == "65532:65532" ]] || fail "agent image user is '$user', want 65532:65532"
for kv in contains.git=true contains.herdr-core=false contains.harnesses=false agent-ready=false; do
  got=$(label "$agent_image" "io.spaceman.${kv%=*}")
  [[ $got == "${kv#*=}" ]] || fail "label io.spaceman.${kv%=*} is '$got', want '${kv#*=}'"
done
echo "user=$user labels ok (agent-ready=false)"

step "agent: contents (git and ssh yes; hc, claude, codex no)"
docker run "${run_opts[@]}" --entrypoint git "$agent_image" --version
docker run "${run_opts[@]}" --entrypoint ssh "$agent_image" -V
for bin in hc herdr-core claude codex; do
  if docker run "${run_opts[@]}" --entrypoint sh "$agent_image" -c "command -v $bin" >/dev/null 2>&1; then
    fail "agent base image contains $bin; it must not be advertised as not agent-ready then"
  fi
done
echo "absent: hc herdr-core claude codex"

step "agent: version"
docker run "${run_opts[@]}" "$agent_image" version --json | tee "$cfg_dir/agent-version.json"
grep -q '"version"' "$cfg_dir/agent-version.json" || fail "agent version output"

cat >"$cfg_dir/agent.yaml" <<'EOF'
mode: production
data_dir: /var/lib/spaceman-agent
central:
  url: https://central.example.test
  credential_file: /var/lib/spaceman-agent/node.credential
runner:
  session: spaceman
  socket: /run/spaceman-agent/hc.sock
log:
  level: info
  format: json
EOF
chmod 0644 "$cfg_dir/agent.yaml"
docker volume create "$agent_vol" >/dev/null
agent_mounts=(-v "$agent_vol:/var/lib/spaceman-agent" -v "$cfg_dir/agent.yaml:/etc/spaceman-agent/agent.yaml:ro"
  --tmpfs "/run/spaceman-agent:size=1m,mode=0700,uid=65532,gid=65532")

step "agent: run refuses to start before enrollment"
if out=$(docker run "${run_opts[@]}" "${agent_mounts[@]}" "$agent_image" run 2>&1); then
  fail "run started without a node credential: $out"
fi
[[ $out == *"spaceman-agent enroll"* ]] || fail "unexpected refusal message: $out"
echo "refused: ${out##*spaceman-agent: }"

step "agent: run refuses to start without herdr-core (the base image is not agent-ready)"
# A placeholder credential file: the agent stops at herdr-core before it reads or sends it.
docker run "${run_opts[@]}" -v "$agent_vol:/var/lib/spaceman-agent" --entrypoint sh "$agent_image" \
  -c 'umask 077; echo placeholder-not-a-credential > /var/lib/spaceman-agent/node.credential'
if out=$(docker run "${run_opts[@]}" "${agent_mounts[@]}" "$agent_image" run 2>&1); then
  fail "run started without herdr-core: $out"
fi
[[ $out == *"herdr-core"* ]] || fail "unexpected failure message: $out"
echo "refused: ${out##*spaceman-agent: }"
