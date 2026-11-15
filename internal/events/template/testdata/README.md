# Test data

- `harbor-pilot-*.json` are Harbor Pilot v0.1.3 event envelopes, the payload its Pulse relay publishes
  (docs/contracts/harbor-pilot.md, Events). `budget-threshold-warning`, `budget-threshold-exceeded` (one warn-only
  budget's warning and exhaustion in one period), `budget-threshold-exceeded-block` (hptest's tiny blocking budget on
  the second project) and `access-request-blocked` (a budget refusal of a request whose client sent a hostile model
  name) were **captured** from a real `harbor-pilot serve --demo` v0.1.3 (`GET /api/v1/events`, 2026-11-07) through
  `internal/harborpilot/hptest`, unchanged. Their ids are the demo's synthetic, random ULIDs. The demo emits no quota
  window transitions, so `quota-window-exhausted` and `quota-window-recovered` are **derived** from Harbor Pilot's code
  (`internal/quota/service.go:314-329`), with ids from the same demo run (its `team-1-subscription` monitor and
  `anthropic-team-1` account), hand-made event ids, and a hostile label.
- `pulse-github-issues-opened.json` is the event payload go-pulse-github v0.1.0 (`aef635e`, MIT) publishes for its
  `testdata/webhooks/issues_opened.json` fixture: the fixture is the inner `payload`, wrapped as
  `internal/events/webhook.go` `NormalizeWebhook` and `draft.go` `Render` build it (account, source collector `webhook`,
  `github` ref, actor from `sender`, repository, organization, `resource` from `issue`, `occurred_at` from
  `issue.created_at`, routing `installation_account`). `observation_id` and `publication_id` are placeholders: upstream
  computes them as hashes. It is derived from upstream code, not captured from a live pulse-github.
- There is no upstream pull request fixture; older pull request tests use clearly synthetic payloads.
- `pulse-github-issue_comment-created-pr.json`, `…-issue_comment-created-issue.json`,
  `…-pull_request_review-submitted.json`, `…-pull_request_review_comment-created.json`,
  `…-workflow_run-completed.json`, `…-check_suite-completed.json` and `…-pull_request-opened.json` are built by `gen-github-fixtures.py` (run it with
  this directory as its argument). Their inner `payload` is an abridged GitHub webhook body in the shape GitHub
  documents for that event, with placeholder accounts (`example-org`, `alice`, `bob`) and deliberately hostile text in
  titles, comments and workflow names. The script wraps each one as go-pulse-github v0.1.0 does (`resource` from the
  event's primary object, `actor` from `sender`, `occurred_at` only for the event/actions upstream lists,
  `payload_sha256` of the inner body). None was captured from a live pulse-github.
