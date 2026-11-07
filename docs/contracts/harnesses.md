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
- **Settings (ADR-0059, verified 2026-11-06 with 2.1.319, `claude auto-mode config`, no model call):** Claude Code
  reads `$CLAUDE_CONFIG_DIR/settings.json` as the user settings file. Its `autoMode.environment`, `autoMode.allow` and
  `autoMode.soft_deny` lists with `"$defaults"` keep the built-in rules and add the others (21 + 1, 16 + 1 and 72 + 1
  rules). `hard_deny` isn't configurable. SpaceMan sets the profile's keys there and keeps the rest.
- **Renderer (ADR-0064, verified 2026-11-07: `strings` on the 2.1.317 binary, and its start-up
  as recorded in the stub harness's `fullscreen.bin`; no model call):** with no `tui` setting, Claude Code picks the
  renderer itself: full-screen for a recent install (`fresh_install_on`, needs `firstStartVersion` in `.claude.json`,
  fewer than 3 upsells seen) or by server-side flags (`gb_on`), else classic. Signed out in an isolated tmux it stayed
  classic, even on a relaunch; the operator's signed-in runs used the full-screen renderer. That renderer uses the alternate screen
  (`?1049h`), mouse tracking `?1000h ?1002h ?1003h` with SGR `?1006h`, focus events `?1004h`, synchronized output
  `?2026h`, and a virtualized scrollback that only the program can scroll (wheel reports, PgUp/PgDn). Arrow keys
  that look like a scroll wheel (a burst in one direction) make it show "Scroll wheel is sending arrow keys · use PgUp/PgDn to scroll"
  (`scroll-as-arrows`). `settings.json` `"tui": "default"` selects the classic renderer (`settings_off`, checked before
  the fresh-install default); `"fullscreen"` forces the full-screen one. `CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN` (or
  `CLAUDE_CODE_NO_FLICKER` set false) also forces the classic renderer, and `CLAUDE_CODE_NO_FLICKER=1` forces the full-screen one; both environment
  variables are checked before `tui` (so a profile's `env` can override it). SpaceMan sets `"tui": "default"` unless the profile sets
  `tui`. Checked live (isolated tmux, fresh `HOME` and config dir, `DISABLE_AUTOUPDATER=1`, no credentials, no prompt):
  `"tui": "fullscreen"` gave `alternate_on=1`, `mouse_any_flag=1`, `mouse_sgr_flag=1`; `"tui": "default"` gave 0, 0, 0.
  The "Scroll wheel is sending arrow keys" hint did not show for bursts of `ESC[A` in an empty, signed-out session
  (it is in the operator's screenshot of a real run).
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
  - **Interactive sign-in (2026-11-04):** started like a SpaceMan run (`--no-daemon -s read-only -a never -- <prompt>` in
    an isolated herdr-core core, fresh `CODEX_HOME`), Codex ignores `OPENAI_API_KEY` and `CODEX_API_KEY` (also with
    `-c forced_login_method="api"`) and shows "Welcome to Codex … Choose how you want to use Codex". With an API-key
    `auth.json` it skips that screen and shows no folder-trust prompt (with this sandbox and approval policy), sends the
    prompt (`401 Unauthorized` for the test key) and reports `status {app: codex, state: working}` through OSC 7501
    (ADR-0054).
  - Not verified: a real ChatGPT login (no account) and a real model turn.
  - Env `OPENAI_API_KEY` and `CODEX_API_KEY` (strings-verified).
- **Notify hook:** `notify = [argv…]` in config.toml, called at turn end.
- **Status:** OSC 7501 emitter present.

## Harbor Pilot gateway spike (2026-11-06)

Evidence for ADR-0060 (Proposed, `docs/DECISIONS.md`); the gateway contract is `docs/contracts/harbor-pilot.md`.

**Setup.**
- The gateway was Harbor Pilot v0.1.1 `serve --demo` on loopback, with synthetic upstreams and the `hptest` fixture
  binding (`docs/contracts/harbor-pilot.md`).
- Each harness got its own child token, minted with the parent token. Its `bound_refs` were `spaceman.space`,
  `spaceman.run`, `spaceman.instance`, `session` and `project_id`; scopes were `gateway:invoke` and `gateway:models`;
  the TTL was 1 h.
- Each harness was started by hand the way a run is: `spaceman-agent launch --secrets-file <0600 JSON> -- <harness argv>`
  (spaceman-agent v0.1.0-rc.8). It ran in a dedicated tmux server with `env -i`, a fresh `HOME`, a fresh config dir and
  a working directory.
- A logging pass-through proxy sat in front of the gateway, to see the requests.
- Afterwards every token was revoked (204), and each then got 401 on `/v1/models`.

**Claude Code 2.1.317.** The secrets file held only `ANTHROPIC_BASE_URL=<gateway origin>` and `ANTHROPIC_AUTH_TOKEN=<child token>`.
- **First run: worked, with the existing seeding.**
  - With the ADR-0021 `.claude.json` seed (`hasCompletedOnboarding`, `theme`, trust for the working directory), the
    interactive session went straight to the prompt.
  - There was no login screen and no "use this API key?" question. The banner said "API Usage Billing".
  - Without the seed, it stopped at the theme picker, as before. So `ANTHROPIC_AUTH_TOKEN` needs no seeding beyond
    ADR-0021.
- **Model turn: needs a model choice with this Harbor Pilot.**
  - With no `--model`, the default model is `claude-opus-5-5`. Claude Code then sends a `system`-role entry inside
    `messages[]` (the environment block), and Harbor Pilot 0.1.1 refuses it: 400 `messages[1]: role must be user or
    assistant`. The session showed "API Error: 400 …", and only a side request (`messages` with the `user` role only)
    went through.
  - With `--model claude-sonnet-4-5`, every request had only `user` messages, and the turn completed with the demo's
    synthetic reply.
  - This is a Harbor Pilot protocol gap (`internal/protocol/anthropic/request.go:242`), not a SpaceMan one.
- **Attribution: worked.** The Harbor Pilot request detail showed `harness: claude_code`, the `spaceman-test` binding and
  the project. Its `trusted_refs` were `spaceman.run`, `spaceman.space`, `spaceman.instance`, `session` and
  `project_id`, plus the binding's `spaceman.fleet`. Claude's own `x-claude-code-session-id` appeared under `claims`
  (untrusted).
- **Auto-update.** On the first launch Claude Code auto-updated itself into the fresh `HOME` (`~/.local/bin`; the system
  binary was unchanged). Later launches set `DISABLE_AUTOUPDATER=1`.

**codex-cli 0.166.0** (npm `@openai/codex@0.166.0`). The secrets file held only `HARBOR_PILOT_TOKEN=<child token>`. The
argv was `codex --no-daemon` with these five overrides, then `-s read-only -a never -- <prompt>`:
```
-c model_provider="harbor-pilot"
-c model_providers.harbor-pilot.name="Harbor Pilot"
-c model_providers.harbor-pilot.base_url="<origin>/v1"
-c model_providers.harbor-pilot.wire_api="responses"
-c model_providers.harbor-pilot.env_key="HARBOR_PILOT_TOKEN"
```
`CODEX_HOME` was fresh, with no `auth.json`.
- **Login screen: skipped, no seeding needed.** Interactive Codex went straight to the prompt; it never showed "Welcome
  to Codex … Choose how you want to use Codex" (unlike `OPENAI_API_KEY`, ADR-0054). There was no folder-trust prompt
  with this sandbox and approval policy. Codex wrote only `[tui] screen_reader_detection_done = true` to `config.toml`,
  and no `auth.json`.
- **Model turn: needs a model the plan can route.**
  - With Codex's default model (`gpt-6.1-sol`), Harbor Pilot answered 403 `no eligible account is available for model
    gpt-6.1-sol`, because the demo plan has no route for it.
  - With `-m coding/default` (a plan alias), `/v1/responses` streamed and the turn completed.
  - Codex then warned "Model metadata for `coding/default` not found. Defaulting to fallback metadata".
- **Attribution: worked.** The Harbor Pilot request detail showed `trusted_refs.spaceman.run=run_…` and the other bound
  refs. Codex's own `session-id` header appeared under `claims`. Harbor Pilot labelled the harness `generic`, not `codex`.

**Consequences for SpaceMan's harness adapters (HP-T4):**
- Both harnesses can be driven through the gateway with only the secret env above. Codex needs no `auth.json` or
  first-run seeding.
- A model must be chosen that both the harness sends cleanly and the Harbor Pilot plan routes.
  - For Codex, a run needs a `-m` the plan routes (a space setting or allowed-alias default).
  - For Claude with Harbor Pilot 0.1.1, a model whose requests carry no `system`-role messages, until Harbor Pilot
    accepts them. Harbor Pilot 0.1.2 does (re-run below), and SpaceMan requires ≥ 0.1.3.

## Harbor Pilot 0.1.3 re-run: Claude Code on its default model (K-13, 2026-11-07)

Re-run of the Claude Code model turn above after Harbor Pilot 0.1.2/0.1.3 started accepting `system`-role entries in
`messages[]` (contract: Gateway data plane).

**Setup.**
- Harbor Pilot v0.1.3 `serve --demo` (from `go install`, commit `2e957c7`) on loopback, provisioned by `hptest`
  (binding `spaceman-test`, synthetic upstreams).
- One child token minted with the parent token: scopes `gateway:invoke` and `gateway:models`, TTL 1 h, `bound_refs`
  `spaceman.space`, `spaceman.run`, `spaceman.instance`, `session` and `project_id`.
- Claude Code 2.1.317 (`/usr/local/bin/claude`), started with `env -i`, a fresh `HOME`, `DISABLE_AUTOUPDATER=1`, and
  only `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` (the child token) as credentials. **No `--model`**, so it used
  its default, `claude-opus-5-5`.
- It ran in print mode (`claude -p "…" --output-format json`), not interactively through `spaceman-agent launch` as in
  the first spike. Print mode sends the same `system`-role entry, as the proxy showed.
- A pass-through proxy in front of the gateway logged each request's model and the roles in `messages[]` (no content).

**Result: K-13 is resolved on the demo.**
- The model request was `POST /v1/messages?beta=true`, model `claude-opus-5-5`, with roles `["user","system"]`. That
  is the shape Harbor Pilot 0.1.1 refused with 400.
- Harbor Pilot answered 200, and the turn completed (`is_error: false`, `subtype: success`) with the demo's synthetic
  reply. Claude reported its usage under `claude-opus-5-5`.
- Two runs, each on its own gateway, were served by different demo accounts (contract: Demo seed, demo routing):
  - on one, started right after the gateway was up, the request went to the demo's Anthropic-shaped upstream
    (`demo-sonnet`). Its request detail showed **no losses**, so the `system` entry was passed through unchanged;
  - on the other, it went to a Responses-shaped upstream (`demo-gpt`) and was translated, with losses only for fields
    Responses lacks (among them `output_config` on the mid-conversation system message).
- **Attribution: worked.** `harness: claude_code`; `trusted_refs` held the five bound refs plus the binding's
  `spaceman.fleet`; Claude's `x-claude-code-session-id` was under `claims`. `activity/requests?ref=spaceman.run:<run>`
  and `activity/sessions?ref=session:<value>` found the request and its session; `usage/summary` with
  `group_by=ref:spaceman.run` showed the run.
- **Cleanup.** The token was revoked (204, twice), then got 401 on `/v1/models`. The gateway and its data directory
  were removed by the test cleanup.

**What this does not show.**
- The demo's upstreams are synthetic, so whether a **real** Anthropic upstream accepts `claude-opus-5-5`'s
  mid-conversation `system` entry is not verified. Harbor Pilot passes it through unchanged for models not on its
  no-support list (D-042), so a real model without support would return the provider's own 400.
- Interactive Claude through `spaceman-agent launch` was not re-run. The first spike already showed that path reaches
  the prompt with the ADR-0021 seed; only the model turn changed.

**Consequence for HP-T4:** with Harbor Pilot ≥ 0.1.2, a Claude Code gateway run doesn't need a model chosen to avoid
`system`-role messages. It still needs a model the space's Harbor Pilot plan routes.

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
