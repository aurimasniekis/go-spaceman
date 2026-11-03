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
  - In the default (auto) permission mode, an interactive session called a tool of the SpaceMan MCP server
    (`--mcp-config` + `--strict-mcp-config`, stdio `spaceman-agent mcp-bridge`) without a permission prompt
    (`TestRealClaudeUsesSpaceManTools`). Searching the whole test directory afterwards found the setup-token nowhere
    in clear (central database, agent folders, hc state, logs, the instance's Claude config).
- **Skills (spike S-6, verified live 2026-11-02, `claude-haiku-5-5`, $0.0004 print-mode check):** Claude Code 2.1.316
  loads `$CLAUDE_CONFIG_DIR/skills/<name>/SKILL.md` (frontmatter `name`, `description`) and lists it among its skills.
  SpaceMan writes assigned skills there (SM-MCP-004).
- **Transcripts and usage (spike S-7, verified live 2026-11-02, `claude-haiku-5-5`, $0.0014 print-mode check; again in
  an interactive run via `TestRealClaudeUsesGatewayTool`):** Claude Code writes its session to
  `$CLAUDE_CONFIG_DIR/projects/<cwd with / and . as ->/<session id>.jsonl`. `assistant` records carry `message.id`,
  `message.model` and `message.usage` (`input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
  `cache_read_input_tokens`); records of one request repeat while it streams (same `message.id`). A `cost-state` record
  carries `totalCostUSD`, `hasUnknownModelCost` and per-model `modelUsage`. Codex's equivalents are not verified.
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
- **Verified with codex-cli 0.166.0 (2026-11-03, isolated `CODEX_HOME` and `HOME`, no credential, no model call):**
  - **Skills:** `$CODEX_HOME/skills/<name>/SKILL.md` is listed in the model's prompt input (`codex debug prompt-input`:
    "refunds-runbook: … (file: r0/refunds-runbook/SKILL.md)", root `r0` = `$CODEX_HOME/skills`). Codex installs its own
    skills in `$CODEX_HOME/skills/.system`.
  - **Login storage:** `cli_auth_credentials_store` defaults to `"file"`; `codex login --with-api-key` writes
    `$CODEX_HOME/auth.json` (0600, `{"auth_mode":"apikey","OPENAI_API_KEY":…}`) and `auth.lock`; `codex login status`
    reads it.
  - **Links:** with `auth.json` and `auth.lock` as symlinks to a shared folder, a login rewrites the shared file
    through the link and the links stay; a second `CODEX_HOME` linked to the same file sees the change.
  - **Refresh tokens are single-use:** the binary's messages include "your refresh token was already used" and "Skipping
    token refresh because auth changed after guarded reload". Copies of one ChatGPT login would break each other.
  - `CODEX_ACCESS_TOKEN` (`codex login --with-access-token`) is "Agent Identity" authentication, not a ChatGPT login;
    SpaceMan does not use it.
  - Not verified: a real ChatGPT login (no account) and a real model turn.
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
