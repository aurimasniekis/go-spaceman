# Contract: harness CLIs

Inspected 2026-11-02 on the development Mac via `--help`, `--version`, binary strings and config-file key names. **No sessions were started**
for this inspection. Real-run evidence is recorded in `docs/TRACEABILITY.md` once the opt-in suites run.

## Claude Code — 2.1.316
- **Sessions:**
  - `--session-id <uuid>`, `-r/--resume [id|name]`, `-c/--continue`, `--fork-session`, `-n/--name`.
  - Background agents: `--bg` plus the `agents|attach|logs|stop|rm|respawn` subcommands. SpaceMan does not use these, since hc supervises.
- **Configuration and context flags:**
  - MCP: `--mcp-config <files|json…>`, `--strict-mcp-config`.
  - Settings: `--settings <file|json>`, `--setting-sources user,project,local`.
  - System prompt: `--append-system-prompt`, `--system-prompt`.
  - Tool scope: `--add-dir`, `--agents <json>`, `--plugin-dir`, `--tools`, `--allowedTools`, `--disallowedTools`.
- **Permissions:**
  - `--permission-mode acceptEdits|auto|bypassPermissions|manual|dontAsk|plan`.
  - Bypass and restriction flags: `--dangerously-skip-permissions`, `--restricted`, `--safe-mode`, `--bare`.
- **Auth:**
  - `claude setup-token` creates a long-lived token and needs a subscription.
  - Env `CLAUDE_CODE_OAUTH_TOKEN` (strings-verified) and `ANTHROPIC_API_KEY`.
  - `--bare` ignores OAuth and the keychain, so it is never combined with an OAuth account.
- **Config isolation:** env `CLAUDE_CONFIG_DIR` (strings-verified).
- **Verified live 2026-11-02 (spike S-5, isolated hc core, `claude-haiku-5-5`, $0.0014 print-mode check):**
  - Interactive mode authenticates with `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token` and an otherwise empty
    `CLAUDE_CONFIG_DIR` (K-10). Without it the session says "Not logged in".
  - A fresh `CLAUDE_CONFIG_DIR` first shows the theme picker, then the folder-trust prompt for the working directory.
    Both are skipped when `$CLAUDE_CONFIG_DIR/.claude.json` contains `hasCompletedOnboarding: true`, `theme`, and
    `projects["<real path>"].hasTrustDialogAccepted: true`. The key is the real path (`/private/tmp/…` on macOS) (ADR-0021).
  - The startup notice says auto mode is the default permission mode in this version.
  - Inherited `CLAUDE_CODE_CHILD_SESSION` (set inside another Claude Code session) turns transcript saving off; the
    launcher shim removes inherited session markers.
  - hc reports `status {app: claude-code, state: done}` and `turn: 1` after the reply (OSC 7501).
- **Status:** native OSC 7501 reporting (see herdr-core.md).
- **Hidden flags:** `--channels`, `--dangerously-load-development-channels` (strings only, so **not** relied on until verified).
- **Print mode:** `-p`, `--output-format stream-json`, `--max-budget-usd`. Used only by opt-in real tests, never for managed runs, which are interactive in hc.

## Codex — codex-cli 0.163.0
- **Config isolation:** `CODEX_HOME`. Sessions live in `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`.
- **Config overrides:** `-c key=value` (TOML, dotted paths, e.g. `mcp_servers.<name>.command`), `-p/--profile`, `--strict-config`.
- **Sandbox, approvals and workspace:**
  - Sandbox and approvals: `-s read-only|workspace-write|danger-full-access`, `-a on-request|never`.
  - Workspace: `-C/--cd`, `--add-dir`.
  - Must use `--no-daemon` under hc (per the hc manual).
- **Sessions:** `codex resume [SESSION_ID]`, `codex exec [--json] [-o file]`, `codex exec resume`.
- **Auth:**
  - `codex login --with-api-key` (key on stdin, stored in `$CODEX_HOME/auth.json`) and `--with-access-token` (`CODEX_ACCESS_TOKEN`).
  - ChatGPT login writes `auth.json` with `auth_mode: chatgpt`, `tokens`, `last_refresh`.
  - Env `OPENAI_API_KEY` and `CODEX_API_KEY` (strings-verified).
- **Notify hook:** `notify = [argv…]` in config.toml, called at turn end.
- **Status:** OSC 7501 emitter present.

## Capability advertisement (initial, conservative)
| Capability | Claude Code | Codex |
|---|---|---|
| Fixed session id at launch | yes (`--session-id`) | no (discover from `sessions/`) |
| Resume | yes (`--resume <id>`) | yes (`resume <id>`) |
| MCP config injection | yes (`--mcp-config`, strict) | yes (`-c mcp_servers.*`) |
| Program status via OSC 7501 | yes (verified live) | emitter present (to verify live) |
| Safe input via hc `terminal.send if:idle` | yes (hc-level) | yes (hc-level) |
| Native team messaging (SendMessage) | only within its own team topology, so **not** across standalone runs | no |
| Native transcript | `$CLAUDE_CONFIG_DIR/projects/**.jsonl` (to verify) | `$CODEX_HOME/sessions/**.jsonl` |
| Usage extraction | from transcript if present (to verify); else unknown | from rollout if present (to verify); else unknown |
