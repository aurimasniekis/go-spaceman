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
| `queue` | The execution queue | Queue with running, ready, demoted incident, blocked, dependent and backlog tasks | current |
| `queue-task` | The execution queue | Task drawer: eligibility reasons, state, dependencies, blockers | current |
| `inbox-list` | The inbox | Space inbox with status filter and counts | current |
| `inbox-composer` | The inbox | TipTap composer with formatting and an attachment | current |
| `inbox-item` | The inbox | Inbox item with rendered Markdown, inline image and file attachment | current |
| `human-global` | Human actions | Needs you: pending actions across spaces with the nav badge | current |
| `human-space` | Human actions | A space's human actions, including a resolved one | current |
| `human-form-default` | Human actions | Approval drawer with the advisory notice and distinct Approve/Deny | current |
| `human-form-custom` | Custom types (M3) | Schema-driven form | todo |
| `kb-sessions` | KB/support (M3) | KB support sessions | todo |
| `events-catalog` | Events and subscriptions | Template catalog | current |
| `event-template` | Events and subscriptions | Template definition with a sample test | current |
| `events-subscribe` | Events and subscriptions | Subscription form with template variables | current |
| `events-subscription` | Events and subscriptions | Subscription with grant, cursor and delivery history | current |
| `events-quarantine` | Events and subscriptions | Quarantined event with reprocess/discard | current |
| `memory-sync` | Memory/dreams (M3) | Memory collection sync state | todo |
| `prompts-diff` | Prompts/profiles (M3) | Prompt version diff | todo |
| `ops-backups` | Backups/operations (M4) | Operations page | todo |
