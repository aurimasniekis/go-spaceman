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
- **Terminal question picker and plan mode denied (ADR-0066, verified 2026-11-08 with Claude Code 2.1.317, live, no
  model; the operator checked the same on 2.1.319):** `AskUserQuestion` draws its picker in the terminal and waits
  there; `EnterPlanMode` leads to `ExitPlanMode`'s approval dialog. A whole-tool rule in `settings.json`
  `permissions.deny` drops a tool from the tools Claude Code sends (`filterToolsByDenyRules`). Checked in an isolated tmux
  session (fresh `HOME` and `CLAUDE_CONFIG_DIR`, `DISABLE_AUTOUPDATER=1`, onboarding and folder trust seeded,
  `ANTHROPIC_BASE_URL` pointing to a local fake API that answered 400 and recorded each request's tool names,
  `ANTHROPIC_AUTH_TOKEN` a dummy): without the rule every request carried `AskUserQuestion` (13 and 25 tools); with
  `{"permissions":{"deny":["AskUserQuestion"]}}` none did (12 and 24). With `"deny":["AskUserQuestion","EnterPlanMode"]`
  neither appeared (12 and 23 tools; `ExitPlanMode` stays listed, as it only acts inside plan mode). Print mode (`-p`)
  never offers `AskUserQuestion`. SpaceMan appends both rules to the profile's deny list unless the profile allows a
  tool itself.
- **PermissionRequest hook and additional directories (ADR-0068, verified 2026-11-08 with Claude Code 2.1.317, live,
  no model):** the binary's hook schemas (`strings`): input = the common fields (`session_id`, `transcript_path`,
  `cwd`, `permission_mode`, …) plus `hook_event_name: "PermissionRequest"`, `tool_name`, `tool_input`,
  `permission_suggestions?`, `mcp_server?`; output `hookSpecificOutput: {hookEventName: "PermissionRequest", decision:
  {behavior: "allow", updatedInput?, updatedPermissions?} | {behavior: "deny", message?, interrupt?}}`. Only `command`
  and `http` hooks may answer it (agent and prompt hooks are refused for this event), and in auto mode Claude Code
  ignores a hook's allow ("a hook cannot approve"). Checked in an isolated tmux session (fresh `HOME` and
  `CLAUDE_CONFIG_DIR`, `DISABLE_AUTOUPDATER=1`, onboarding and trust seeded, a local fake API that answered with one
  scripted `tool_use` and recorded the following `tool_result`): without a hook, a `Read` of a file outside the
  working directory stopped at the "Read outside the working directories" dialog. With
  `hooks.PermissionRequest = [{matcher: "*", hooks: [{type: "command", command: "'<agent>' hook permission-request"}]}]`
  the same read, and a `Bash` call matched by an `ask` rule, came back to the model as `is_error` tool results carrying
  the hook's message; the terminal showed "Denied by PermissionRequest hook". A file under a directory in
  `permissions.additionalDirectories` was read without a dialog. A deny rule `Read(//<abs path glob>)` (a leading `//`
  is an absolute path) refused a read there with "File is in a directory that is denied by your permission settings."
- **Deny rules against other instances, and failing hooks (ADR-0068 review fixes, verified 2026-11-08 with Claude
  Code 2.1.317, live, no model; `make test-real-claude-cli`):**
  - **Setup.** Print mode (`-p`) with a local fake API scripting the tool calls, `--permission-mode acceptEdits`, a
    throwaway HOME, config dir and space folder.
  - **Without an Edit deny, other instances are writable.** With the space folder in `additionalDirectories`, `Write`
    and `Edit` to another instance's `claude/settings.json` succeed without a dialog.
  - **What the deny rules refuse.** `Edit(//<abs glob>)` deny rules refuse `Write` and `Edit` alike ("File is in a
    directory that is denied by your permission settings."). That covers both `Edit(//<space>/instances/<id>/**)` and
    `Edit(//<space>/instances/*/claude/settings.json)`.
  - **Deny rules also block the run's own auto memory.** Claude Code writes its auto memory to
    `$CLAUDE_CONFIG_DIR/projects/<project key>/memory/` with the same `Write` tool, and a deny rule covering
    that folder refuses it. So `Edit(//<space>/instances/**)` would block the run's own memory.
  - **Auto memory elsewhere (review round 2, verified live with 2.1.317, `make test-real-claude-cli`).**
    - `autoMemoryDirectory` in the user `settings.json` (an absolute path; `~/` is expanded; ignored in a checked-in
      project settings file) replaces the folder. Claude Code names it in the system prompt's "auto memory" section,
      loads its `MEMORY.md` into the first request, and its `Write` and `Read` tools reach it without a dialog in
      `acceptEdits` even when it is outside every working directory (its built-in carve-out, "auto memory files are
      allowed for writing"/"reading").
    - The default folder's project key: Claude Code keys by the main checkout of the git repository (for a linked
      worktree, the repository its `.git` file points at), else the working directory. Every UTF-16 code unit other
      than an ASCII letter or digit becomes `-`; a key over 200 characters is cut there and gets `-` and the base-36
      absolute value of the path's 32-bit Java-style string hash. Live, the default folder named in the system prompt
      for a worktree cwd matched `agent.ClaudeProjectKey(agent.ClaudeProjectRoot(cwd))`.
    - With 52 instances (no per-instance rules), `Edit(//<space>/instances/*/claude/projects/**)` and the other named
      globs refused writes to another instance's memory, transcript, `rules/`, `CLAUDE.md`, `policy-limits.json`, shell
      snapshots and session environment, and to the run's own `rules/` and `remote-settings.json` (which Claude Code
      also asks about on its own, as one of its config files).
    - With a profile adding the node's whole spaces folder to `additionalDirectories`, a rule anchored on the run's own
      space left another space's instance `settings.json`, `CLAUDE.md` and `rules/` writable; anchored on
      `//<data_dir>/spaces/*/instances/*/claude/...`, the writes are refused (a `*` in the path matches the space).
    - The binary protects these entries of its own config dir (they are what SpaceMan denies for every instance):
      `shell-snapshots`, `session-env`, `plugins`, `hooks`, `skills`, `workflows`, `commands`, `agents`, `routines`,
      `rules`, `output-styles`, `scheduled_tasks.json`, `launch.json`, `CLAUDE.md`, `projects`, `daemon.json`,
      `policy-limits.json` (and its `.signature*.json` files), `backups`. Its plan files (`plans/`), scratchpad and
      background job folder (`jobs/`) are written by the session's own file tools.
  - **Bracket negation doesn't work.** `[!A]` in a rule did not act as "not A" (it matched names starting with `A`).
  - **Hook command failures.** A `PermissionRequest` command hook whose command fails shows the dialog: exit code 2
    does not deny this event. The command `'<bin>' hook permission-request 2>/dev/null || printf '%s' '<deny JSON>'`
    still denies when `<bin>` is missing (the reviewer checked this interactively; `TestRealClaudeCLIDeniesOtherInstances`
    checks it in print mode).
  - **Settings keys that run commands** (from the binary's settings schema): `apiKeyHelper`, `otelHeadersHelper`,
    `proxyAuthHelper`, `policyHelper(s)`, `awsAuthRefresh`, `awsCredentialExport`, `gcpAuthRefresh`, `processWrapper`,
    `statusLine`, `subagentStatusLine`, `fileSuggestion`, `sshHostCommands`, `hooks`. `autoMemoryDirectory` exists too.
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
- **Question tool (ADR-0066; code: codex-cli 0.166.0 binary strings, 2026-11-08, not run):** Codex has its own
  blocking question tool, `request_user_input` (`core/src/tools/handlers/request_user_input.rs`). Its instructions offer it
  in Plan mode, and in Default mode only with the `default_mode_request_user_input` feature; it is refused in `codex exec`.
  There is also a `request_user_input_async` variant, and MCP tool approvals can use it (`mcp_tool_call_approval`).
  SpaceMan runs start in Default mode and set no feature override for it (not verified against a live session).
- **Status:** OSC 7501 emitter present.

## Harbor Pilot gateway spike (2026-11-06)

Evidence for ADR-0060 (Accepted, `docs/DECISIONS.md`); the gateway contract is `docs/contracts/harbor-pilot.md`.

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

## Gateway runs through SpaceMan (HP-T4, 2026-11-08)

Real runs through the whole stack: `TestRealHarborPilotGatewayRuns` (`make test-real-harbor-pilot`). **live**

**Setup.**
- SpaceMan built from this branch: real central, `spaceman-agent` (protocol minor 9), an isolated herdr-core core
  (hc 0.1.17) and the launcher shim.
- Harbor Pilot v0.1.3 `serve --demo` on loopback (synthetic upstreams), provisioned by `hptest`. The connection and the
  space settings were set through SpaceMan's API (`allow_insecure_http`, since the demo is plain http on loopback).
- The space also had a direct account bound (a Claude token or an OpenAI key), which the run must not get.
- Each harness binary was wrapped by a script. It wrote the names of the variables the harness started with (and the
  tokens, to a private test folder outside the stack) and then exec'd the real binary with `DISABLE_AUTOUPDATER=1`.
- Claude Code 2.1.317 (`/usr/local/bin/claude`) with no model. codex-cli 0.166.0 (npm, project-local) with
  `-m coding/default`, `-s read-only`, `-a never`. Each run got a first prompt, then a second one through SpaceMan's
  safe input.

**Result: both harnesses worked through SpaceMan with no seeding beyond ADR-0021.**
- **Environment.** Claude Code started with `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` and no other `ANTHROPIC_*`,
  `CLAUDE_CODE_OAUTH*`, `OPENAI_*`, `CODEX_API_KEY` or `SPACEMAN_GATEWAY_TOKEN` variable. Codex started with
  `HARBOR_PILOT_TOKEN` and none of those either.
- **Attribution.** Harbor Pilot's request detail showed the test binding and these trusted refs: `spaceman.space`,
  `spaceman.run`, `spaceman.instance`, `session`, `project_id`, and the binding's `spaceman.fleet`. The harness label
  was `claude_code` for Claude Code and `generic` for Codex. The bound `session` was Claude's central-chosen session ID,
  and the run ID for Codex.
- **Sessions.** For each run, `activity/sessions?ref=session:<value>` found exactly one Harbor Pilot session. Both of
  the run's requests (two turns) belonged to it, and `activity/requests?ref=session:<value>` listed the same two
  requests as `ref=spaceman.run:<run>`.
- **End.** After the run was stopped, the token got 401 on `/v1/models`, and Harbor Pilot showed the credential
  revoked. This was central's own revoker on its deadline timer, not a test call.
- **At rest.** Neither the gateway token, the run's `SPACEMAN_RUN_TOKEN` nor the direct account's secret was in
  herdr-core's `terminals.json`, the central or agent logs, or any file under central's or the agent's data
  directories.

**Finding: Codex's shell snapshot keeps the run's environment.** codex-cli 0.166.0's `shell_snapshot` feature
(stable, on by default) writes a snapshot of the shell environment to
`$CODEX_HOME/shell_snapshots/<id>.sh` (0600). The snapshot includes `HARBOR_PILOT_TOKEN` and `SPACEMAN_RUN_TOKEN`, so
both were at rest in the instance's folder after the run. This affected every Codex run, not only gateway runs; the
earlier Codex test didn't look for the run token. The adapter now passes `-c features.shell_snapshot=false`
(`codex features list` shows it off with that override). With it, the same run found no token at rest. Every Codex
launch also removes the `shell_snapshots` folder earlier runs left, which could hold a direct account's API key.

**Codex `config.toml` against the `-c` provider overrides (code: codex-cli 0.166.0 from npm, 2026-11-08).** `codex
exec` with the five overrides pointed at one local server, and a `config.toml` pointing at another, sent every request
to the override's server: with a root `model_provider = "evil"`, with an `evil.config.toml` profile setting it (with and
without `--profile evil`), and with `config.toml` redefining `[model_providers.harbor-pilot].base_url`. A `profile =`
key (in `config.toml` or as `-c profile=…`) fails at start: "legacy `profile = …` config is no longer supported; use
`--profile <name>` with `<name>.config.toml` instead".

**Not shown.** A real model provider behind Harbor Pilot; a profile's `claude_settings` in a gateway run (refused by
the adapter, unit-tested only); a Claude Code project `.claude/settings.json` in the working directory that sets `env`
(not checked by SpaceMan: LIMITATIONS).

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
