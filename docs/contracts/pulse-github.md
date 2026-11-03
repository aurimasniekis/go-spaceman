# Contract: go-pulse-github

- **Revision:** `github.com/aurimasniekis/go-pulse-github` **v0.1.0**, commit `aef635eeda89caa61a279f90ac69fcaccc43e60a` (local `../pulse-github`),
  **MIT** licence. Inspected 2026-11-02. Code: `internal/events/{payload,tokens,webhook}.go`. Docs: `site/reference/event-contract.md`.
  Fixtures: `testdata/webhooks/issues_opened.json`, `push.json`.
- The canonical location is the sibling checkout's module path and repository. SpaceMan never invents one.

## Subjects
`evt.<domain>.<account_key>.<source>.<event>.<action>`
- `domain`: default `github` (config `pulse.subject_domain`).
- `account_key`: `^[a-z0-9][a-z0-9_-]{0,38}$`, the configured monitored account. **The repository owner/name is never a subject token.**
- `source`: one of `webhook | notifications | activity | audit | reconcile | enrichment | collector`.
- Webhooks: `event` is `X-GitHub-Event`, and `action` is the payload `action` or `none`.
- Token encoding: values matching `[a-z0-9_]{1,48}` are used verbatim. Anything else becomes `x-` + base32hex if that fits in 64 bytes, otherwise `h-` + 26 chars of a sha256.
- Examples: `evt.github.example-org.webhook.issues.opened`, `evt.github.<acct>.webhook.pull_request.<action>`.

## Headers
- `Nats-Msg-Id` = `publication_id`
- `ce-type` = `github.<source>.<event>.<action>.v1`, e.g. `github.webhook.issues.opened.v1`
- `ce-source` = `pulse-github` by default
- `ce-time` = occurred_at, or observed_at when occurred_at is unknown
- `github-time-basis` = `occurred|observed`
- `content-type: application/json`

## Payload (`SchemaVersion = 1`)
```go
type Payload struct {
	SchemaVersion int `json:"schema_version"`; ObservationID string `json:"observation_id"`; PublicationID string `json:"publication_id"`
	Account AccountRef `json:"account"`; Source SourceRef `json:"source"`; GitHub GitHubRef `json:"github"` // {event, action?, delivery_id?, …}
	Actor *User `json:"actor"`; Repository *Repository `json:"repository"` // {id, full_name, owner{id,login,type}, private, visibility}
	Organization *User `json:"organization"`; Resource *Resource `json:"resource"` // {kind,id,node_id,number,ref,sha,url,html_url}
	OccurredAt *time.Time `json:"occurred_at"`; ObservedAt time.Time `json:"observed_at"`; CollectionMode string `json:"collection_mode"`
	Provenance Provenance `json:"provenance"`; Routing Routing `json:"routing"`
	PayloadBlob *BlobRef `json:"payload_blob,omitempty"`; Payload json.RawMessage `json:"payload"` // GitHub's raw webhook JSON
}
```
- Issues and pull requests have **no custom body schema**: `payload` is GitHub's own JSON, and `resource.kind` is `issue` or `pull_request`.
- Delivery is at-least-once, and duplicates can arrive after the JetStream 2-minute window, so **dedup on `publication_id`/`Nats-Msg-Id`**.
- Ordering is FIFO per account only.

## SpaceMan seed templates derived from this contract
- `github-issue` and `github-pull-request` templates:
  - variables `account_key` (validated against the regex above) and `repository` (`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`);
  - subject `evt.github.{{account_key}}.webhook.issues.*` or `…pull_request.*`;
  - payload filter `repository.full_name == {{repository}}`.
- Observed from a running `pulse-github run` (v0.1.0, `TestRealPulseGitHub`, 2026-11-03): the `issues_opened.json` webhook publishes
  on `evt.github.<acct>.webhook.issues.opened` with the headers above, and `push.json` on `….webhook.push.none`. The publication also
  carries `account.github_id` and `github.payload_sha256`, which SpaceMan's derived sample
  (`internal/events/template/testdata/pulse-github-issues-opened.json`) lacks; no seed template reads them.
- Sample-event tests use `issues_opened.json`. **There is no upstream pull_request fixture.** The PR template's sample is synthetic and
  labeled as such, until a real PR event is captured from a live pulse-github.
