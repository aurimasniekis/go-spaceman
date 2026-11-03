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
| `space-purge` | First node and space | Purge confirmation of an archived space with the slug typed | current |
| `space-overview` | Site landing, First node and space | Space overview with lease and runs with the Export and lifecycle actions | current |
| `attention` | Observability | Attention page: what needs a person across spaces (human actions, tasks, quarantine, runs, budgets, nodes, outbox, backups, storage) | current |
| `accounts` | Accounts | Accounts page: credential kinds, harness, bound spaces, rotation age (secrets never shown) | current |
| `space-accounts` | Accounts | Choosing a space's accounts and the default per harness | current |
| `run-launch` | First node and space | Launch run form | current |
| `terminal-live` | Site landing, Terminals and recordings | Live watch-only terminal | current |
| `terminal-takeover` | Terminals and recordings | Live terminal taken over by the signed-in admin, with the takeover banner | current |
| `terminal-replay` | Site landing, Terminals and recordings | Replay with seek and speed | current |
| `auth-users` | Sign-in and access | Users and grants | current |
| `auth-tokens` | Sign-in and access | API tokens | current |
| `account-notifications` | Human actions | Notification preferences on the Account page | current |
| `audit-log` | Sign-in and access | Audit log | current |
| `sign-in-sso` | Single sign-on | Sign-in page with a provider button above the password form | current |
| `sign-in-providers` | Single sign-on | Sign-in providers: redirect URI, providers, group mappings to grants | current |
| `sign-in-provider-form` | Single sign-on | Provider form with the write-only client secret and the porch.groups claim | current |
| `queue` | The execution queue | Queue with running, ready, demoted incident, blocked, dependent and backlog tasks | current |
| `queue-task` | The execution queue | Task drawer: eligibility reasons, state, dependencies, blockers | current |
| `queue-reassign` | The execution queue | Task drawer's Assignee form: role and run chosen | current |
| `inbox-list` | The inbox | Space inbox with status filter and counts | current |
| `inbox-composer` | The inbox | TipTap composer with formatting and an attachment | current |
| `inbox-item` | The inbox | Inbox item with rendered Markdown, inline image and file attachment | current |
| `human-global` | Human actions | Needs you: pending actions across spaces with the nav badge and a reminded to-do | current |
| `human-space` | Human actions | A space's human actions, including a resolved one | current |
| `human-search` | Human actions | A space's human actions searched and sorted by priority | current |
| `human-clarify` | Human actions | A pending question with a clarification request to its requester | current |
| `human-form-default` | Human actions | Approval drawer with the advisory notice and distinct Approve/Deny | current |
| `human-policies` | Human actions | A space's policies in force per type with their source (space override, built-in) | current |
| `human-rerouted` | Human actions | An answered question whose requester run had ended, routed to control with its provenance | current |
| `messages` | Agent messaging | A space's messages with delivery states and a request's detail | current |
| `human-form-custom` | Human action types | A custom type's request and schema-driven response form with its outcomes | current |
| `human-types` | Human action types | Type catalog with schema editors and a live request form preview | current |
| `events-catalog` | Events and subscriptions | Template catalog | current |
| `events-publishing` | Publishing SpaceMan events | Outbound publication status: connection, pending, retrying, dead-lettered, lag, recent failures (global admins) | current |
| `event-template` | Events and subscriptions | Template definition with a sample test | current |
| `events-subscribe` | Events and subscriptions | Subscription form with template variables and the delivery choice | current |
| `events-subscribe-core` | Events and subscriptions | Subscription form with core NATS delivery and the non-durable warning | current |
| `events-subscribe-custom` | Events and subscriptions | Subscribe dialog with a custom JSON definition and an accepted sample-event preview | current |
| `events-subscription` | Events and subscriptions | Subscription with grant, cursor and delivery history with origins | current |
| `events-quarantine` | Events and subscriptions | Quarantined event with reprocess/discard | current |
| `events-core-subscription` | Events and subscriptions | Core NATS subscription: non-durable badge and explanation, subscribe grant, no cursor | current |
| `event-replay` | Events and subscriptions | After an explicit replay: deliveries with live and replay origins | current |
| `space-limits` | Limits and budgets | A space's effective limits with sources, the budget card and its usage-reporting note, retention periods | current |
| `space-schedules` | Schedules | A space's schedules: a weekday cron in Europe/Vilnius and an hourly task schedule | current |
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
| `kb-sessions` | Knowledge bases | A KB's support sessions: one per requester instance, with state, answered questions and last activity | current |
| `xspace-requests` | Cross-space requests | A space's incoming and outgoing cross-space requests with a question's detail (untrusted-content notice, disclosure) | current |
| `xspace-access` | Cross-space requests | A space's settings and what it accepts from and may send to each peer, with the disclosure level and limits | current |
| `xspace-admin` | Cross-space requests | Global Cross-space page: every request between spaces (source → destination) | current |
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
| `space-relocate` | Relocating a space | Relocate dialog: destination (a never-connected node is named as not available), dirty-state policy, stop runs, source folder retention | current |
| `ops-backups` | Backups and restore | Backups page: target, schedule, retention, a verified backup, what backups include and exclude, the data retention card | current |
| `space-repositories` | Repositories and worktrees | A space's configured repositories with the node's report: remote, default branch, state, changes, unpushed commits, worktrees, last fetch | current |
| `space-repository-detail` | Repositories and worktrees | A repository's detail: branches with unpushed commits and its worktrees with the runs that own them | current |
| `space-repository-form` | Repositories and worktrees | Adding a repository: name, folder, clone URL without credentials, default branch, role and clone policy | current |
| `space-export` | Retention and export | The space export dialog: what the archive holds, the not-encrypted warning, the size estimate and limit | current |
| `sign-in-signed-out` | Single sign-on | The sign-in page after signing out of an SSO session, offering sign-out at the provider | current |
