#!/usr/bin/env bash
# The scripted end-to-end demonstration (SM-VER-003, docs/DEMO.md): thirty steps against a real central with the embedded
# UI, two real node agents with their own herdr-core cores, devpulse and devmcp, driven by test/demo (build tag demo),
# the browser (web/e2e/demo.spec.ts) and the spaceman CLI. Never touches the default herdr-core core; every port is a
# free random one; the driver removes only what it created and fails if a process is left behind.
#
# Usage: scripts/demo.sh            stub harness for Claude Code and Codex (reproducible, no credentials)
#        SPACEMAN_DEMO_REAL=1 SPACEMAN_DEMO_CLAUDE_TOKEN=… scripts/demo.sh
#                                   the Claude Code worker is a real, budget-bounded Claude Code run (haiku)
# Environment:
#   SPACEMAN_DEMO_EVIDENCE=FILE  where the evidence JSON lines go (default /tmp/spaceman-demo-<mode>-<time>.jsonl)
#   SM_KEEP=1                    keep the run directory (/tmp/smdemo-*: logs, browser screenshots) for inspection
#   SM_DEMO_SKIP_UI_BUILD=1      reuse an existing web/dist
# Needs Go, pnpm, node, git, hc and the Playwright Chromium browser (cd web && pnpm exec playwright install chromium).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
for p in go pnpm node git hc; do
  command -v "$p" >/dev/null || { echo "demo: $p is not on PATH" >&2; exit 2; }
done
[[ -d "$root/web/node_modules" ]] || (cd "$root/web" && pnpm install --frozen-lockfile)
[[ -d "$root/site/node_modules" ]] || (cd "$root/site" && pnpm install --frozen-lockfile)
if [[ "${SPACEMAN_DEMO_REAL:-}" == 1 && -z "${SPACEMAN_DEMO_CLAUDE_TOKEN:-}" ]]; then
  echo "demo: SPACEMAN_DEMO_REAL=1 needs SPACEMAN_DEMO_CLAUDE_TOKEN (a token from \`claude setup-token\`; see docs/DEMO.md)" >&2
  exit 2
fi
cd "$root"
# A surrounding herdr-core terminal or SpaceMan configuration is never inherited (the driver also scrubs the
# environment of every process it starts).
exec env -u HC_TERMINAL_ID -u HC_TERMINAL_SOCKET -u HC_SCOPE -u HC_SESSION -u HC_SOCKET -u SPACEMAN_CONFIG -u SPACEMAN_AGENT_CONFIG \
  go test -tags demo -count=1 -timeout 40m -run '^TestDemo$' -v ./test/demo/
