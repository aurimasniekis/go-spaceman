# Screenshot coverage inventory

> Every product-facing guide uses real SpaceMan UI screenshots captured by `make screenshots` (`scripts/screenshots.sh`).
> The capture runs Playwright against a fresh local stack seeded with fictional data (example.com people, invented spaces,
> the stub harness) at a 1440×900 viewport, in both the Night Owl (dark) and Light Owl (light) themes. Relative times in
> the images reflect the moment of capture. The stack and every credential it generated are destroyed afterwards; the
> authenticator key shown in `setup-mfa` belongs to such a throwaway account. No placeholders or mock-ups.
> A row is `current` only when its images were regenerated after the last change to that UI area.

| Shot name | Guide(s) | UI area | Status |
|---|---|---|---|
| `landing-overview` | Site landing | Overview | current |
| `setup-bootstrap` | Try it locally, Sign-in and access | First-run setup with the root token | current |
| `setup-mfa` | Sign-in and access | TOTP enrollment on first sign-in | current |
| `setup-node-enroll` | First node and space, Nodes and leases | Nodes | current |
| `space-create` | First node and space | Space create form | current |
| `space-overview` | Site landing, First node and space | Space overview with lease and runs | current |
| `accounts` | Accounts | Accounts page: credential kinds, harness, bound spaces, rotation age (secrets never shown) | current |
| `space-accounts` | Accounts | Choosing a space's accounts and the default per harness | current |
| `run-launch` | First node and space | Launch run form | current |
| `terminal-live` | Site landing, Terminals and recordings | Live watch-only terminal | current |
| `terminal-replay` | Site landing, Terminals and recordings | Replay with seek and speed | current |
| `auth-users` | Sign-in and access | Users and grants | current |
| `auth-tokens` | Sign-in and access | API tokens | current |
| `audit-log` | Sign-in and access | Audit log | current |
| `sign-in-sso` | Single sign-on | Sign-in page with a provider button above the password form | current |
| `sign-in-providers` | Single sign-on | Sign-in providers: redirect URI, providers, group mappings to grants | current |
| `sign-in-provider-form` | Single sign-on | Provider form with the write-only client secret and the porch.groups claim | current |
| `queue` | The execution queue | Queue with running, ready, demoted incident, blocked, dependent and backlog tasks | current |
| `queue-task` | The execution queue | Task drawer: eligibility reasons, state, dependencies, blockers | current |
| `inbox-list` | The inbox | Space inbox with status filter and counts | current |
| `inbox-composer` | The inbox | TipTap composer with formatting and an attachment | current |
| `inbox-item` | The inbox | Inbox item with rendered Markdown, inline image and file attachment | current |
| `human-global` | Human actions | Needs you: pending actions across spaces with the nav badge and a reminded to-do | current |
| `human-space` | Human actions | A space's human actions, including a resolved one | current |
| `human-form-default` | Human actions | Approval drawer with the advisory notice and distinct Approve/Deny | current |
| `human-policies` | Human actions | A space's policies in force per type with their source (space override, built-in) | current |
| `human-rerouted` | Human actions | An answered question whose requester run had ended, routed to control with its provenance | current |
| `messages` | Agent messaging | A space's messages with delivery states and a request's detail | current |
| `human-form-custom` | Human action types | A custom type's request and schema-driven response form with its outcomes | current |
| `human-types` | Human action types | Type catalog with schema editors and a live request form preview | current |
| `kb-sessions` | KB/support (M3) | KB support sessions | todo |
| `events-catalog` | Events and subscriptions | Template catalog | current |
| `events-publishing` | Publishing SpaceMan events | Outbound publication status: connection, pending, retrying, dead-lettered, lag, recent failures (global admins) | current |
| `event-template` | Events and subscriptions | Template definition with a sample test | current |
| `events-subscribe` | Events and subscriptions | Subscription form with template variables | current |
| `events-subscription` | Events and subscriptions | Subscription with grant, cursor and delivery history | current |
| `events-quarantine` | Events and subscriptions | Quarantined event with reprocess/discard | current |
| `memory-sync` | Memory/dreams (M3) | Memory collection sync state | todo |
| `prompts-diff` | Prompts | Line diff between two published versions of a prompt | current |
| `prompts` | Prompts | Prompt catalog with kinds, latest versions and provenance | current |
| `prompt-detail` | Prompts | A prompt's draft editor (with extends) and published versions | current |
| `roles` | Roles and profiles | Role templates with prompt binding and profile | current |
| `skills` | Skills | Skill catalog with the built-in HallPass skill and a custom skill | current |
| `approval-policies` | Approval policies | Operations that need a person's approval, with uses, validity and request expiry | current |
| `approval-enforced` | Human actions | A pending enforced approval: bound operation, run, argument digest and redacted arguments | current |
| `approval-executed` | Human actions | An approved enforced approval with its execution recorded separately | current |
| `space-knowledge` | Space knowledge | Knowledge bases with sessions, capacity, questions and proposals | current |
| `kb-detail` | Space knowledge | A KB's questions with answers, their basis and evidence | current |
| `space-memory` | Space memory | Dreams and role memory collections with central's head and the node's sync state | current |
| `dream-runs` | Space memory | A dream configuration's recent dreams with summaries and findings | current |
| `run-usage` | Run | A run's token usage from its native transcript and the stored transcript | current |
| `space-usage` | Space overview | Known usage totals with unknown runs counted separately | current |
| `memory-files` | Space memory | A role's acknowledged memory files, history and conflicts | current |
| `mcp-servers` | MCP servers | Registered MCP servers with header names (never values), grants and limits | current |
| `mcp-server-detail` | MCP servers | A server's connection check with its namespaced tools, and its grants to space roles | current |
| `skill-detail` | Skills | A skill's files, published versions and draft editor | current |
| `space-role-preview` | Roles and profiles | A space role's effective configuration with origins and system text | current |
| `run-manifest` | Roles and profiles | A run's configuration at launch (manifest) | current |
| `ops-backups` | Backups and restore | Backups page: target, schedule, retention, a verified backup, what backups include and exclude | current |
