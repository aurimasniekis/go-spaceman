# Test data

- `pulse-github-issues-opened.json` is the event payload go-pulse-github v0.1.0 (`aef635e`, MIT) publishes for its
  `testdata/webhooks/issues_opened.json` fixture: the fixture is the inner `payload`, wrapped as
  `internal/events/webhook.go` `NormalizeWebhook` and `draft.go` `Render` build it (account, source collector `webhook`,
  `github` ref, actor from `sender`, repository, organization, `resource` from `issue`, `occurred_at` from
  `issue.created_at`, routing `installation_account`). `observation_id` and `publication_id` are placeholders: upstream
  computes them as hashes. It is derived from upstream code, not captured from a live pulse-github.
- There is no upstream pull request fixture; pull request tests use clearly synthetic payloads.
