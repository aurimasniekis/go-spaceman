# Contract: go-pulse

- **Revision:** `github.com/aurimasniekis/go-pulse` **v0.1.3**, commit `9aba53b5c8cfe5eb8ff01321d30a36dfb6cac273` (local `../pulse`), Apache-2.0.
  Inspected 2026-11-02. Specs: `docs/ENVELOPE.md`, `docs/GOAL.md`. Code: `pkg/pulse/{client,subscribe,blob}.go`, `pkg/pulse/envelope/envelope.go`,
  `internal/streams/events.go`, `internal/natsauth/grants.go`.

## Envelope (CloudEvents-lite in NATS headers; the body is the raw payload)
`Nats-Msg-Id` (ULID event ID and idempotency key), `ce-type` (a label, versioned like `x.y.z.v1`; **not a validated schema**), `ce-source`,
`ce-time` (RFC3339Nano UTC), `content-type`, plus `pulse-blob`, `pulse-blob-size` and `pulse-blob-sha256` for offloaded payloads (>1 MiB go to the
Object Store bucket `blobs`). Extra headers pass through unchanged, and `Nats-*` headers are dropped on decode.
**Pulse defines no correlation or causation fields.** There is no schema registry.

## Subjects
`evt.<domain>.<entity>.<action>[.<key>…]`, at least 4 tokens, each `^[a-z0-9_-]+$`, ≤256 bytes (`ValidateSubject`). Filters allow `*` and a trailing `>` (`ValidateFilter`).

## JetStream
Stream `EVENTS` on `evt.>`: file storage, limits policy, DiscardOld, `Duplicates: 2m`, `MaxMsgSize 1 MiB`, `MaxConsumers 256`, Replicas 1.

## Go SDK
```go
func Connect(url string, opts ...Option) (*Client, error) // WithCredsFile|WithCreds|WithCAFile|WithCA|WithSource|WithName|WithStream|WithNATSOptions
func (c *Client) Publish(ctx, subject, typ string, data any, opts ...PublishOption) (*PubAck, error) // WithID|WithTime|WithContentType|WithEventSource|WithHeader|WithBlob
type PubAck struct { ID, Stream string; Sequence uint64; Duplicate bool }
func (c *Client) Subscribe(ctx, filter string, h Handler, opts ...SubOption) (*Subscription, error) // Since|FromSeq|OnlyNew|Durable|AckWait|MaxDeliver|OnError|FromStream
type Handler func(ctx context.Context, e Event) error
func Terminal(err error) error
```
- **Without `Durable`:** an ordered ephemeral consumer, no acks.
- **With `Durable(name)`:**
  - The consumer is `<clientID>_<name>`, with explicit ack and a single `FilterSubject`.
  - **If the consumer exists, the SDK binds without updating its config.**
  - Handler results: nil → Ack; `Terminal(err)` → Term; any other error → `NakWithDelay` (1 s doubling, 5 min cap). MaxDeliver is unlimited by default.
- **Auth:** `.creds` (user JWT + nkey seed) only, plus TLS via `tls://` and an optional CA. The client ID comes from the JWT tag `pulse-client:<id>`.

## Grants (`internal/natsauth/grants.go`)
- Actions: `publish | subscribe | request | reply | durable | blobs`.
- Durable names are exact, `^[a-z0-9][a-z0-9_-]{0,47}$`.
- A `durable` grant permits CONSUMER.CREATE `<stream>.<clientID>_<name>.<subject pattern>`, plus INFO, MSG.NEXT, DELETE, ACK and FC for that consumer only.
- A permission denial shows up as a **timeout** (default 5 s), not as an error.

## Implications for SpaceMan
- See ADR-0011 (inbound) and ADR-0015 (outbound).
- Deduplicate on `Nats-Msg-Id` with a DB unique key. The JetStream window is only 2 min.
- Correlation and causation are SpaceMan extension headers, never claimed as Pulse contract.
- For plain NATS Core (no JetStream), use `c.Conn()` with subscribe grants. Ingestion is labeled **non-durable**.

## Core NATS subscriptions (ADR-0042)
Inspected 2026-11-03: go-pulse v0.1.3 (`pkg/pulse/client.go`, `pkg/pulse/envelope/envelope.go`), Pulse
`internal/natsauth/grants.go` at `9aba53b`, nats.go v1.55.0 (`nats.go`), nats-server v2.15.1 (`server/client.go`).
- **Grant:** `subscribe` "allows core subscriptions on Subject and replay through filtered consumers on Stream"
  (`ActSubscribe`); the CLI form is `action[:subject[:name]]`, so a core subscription needs `subscribe:<subject>`.
- **Decoding:** `envelope.DecodeMsg(*nats.Msg) Event` decodes a core message like a JetStream one; `Nats-Msg-Id` is read
  into `Event.ID` (it may be absent: nothing assigns an ID to a plain `nats pub`), `Nats-*` headers are dropped from `Extra`.
- **nats.go calls used:** `(*nats.Conn).Subscribe(subject, handler)` (asynchronous; handlers of one subscription run in
  order on one goroutine), `FlushTimeout`, `LastError`, `IsConnected`, `(*nats.Subscription).Pending()`, `Dropped()`
  and `Unsubscribe()`, and the `nats.ErrorHandler` option (through `pulse.WithNATSOptions`).
- **A refused subscription** is not an error of `Subscribe`: the server answers `-ERR 'Permissions Violation for
  Subscription to "<subject>"'` before the flush's PONG; nats.go records it as the connection's last error and calls the
  error handler with `nats.ErrPermissionViolation` and no subscription. The connection stays up.
- **Slow consumer:** past the subscription's pending limits (nats.go defaults: 500,000 messages, 64 MiB) the client drops
  messages, counts them in `Dropped()` and calls the error handler with `nats.ErrSlowConsumer` and the subscription.
- **Reconnects:** `pulse.Connect` sets `MaxReconnects(-1)`; nats.go resubscribes after a reconnect. Messages published
  while disconnected are not delivered.
- **JetStream consumer info** for lag: `jetstream.JetStream.Consumer(ctx, stream, name)` returns the consumer with fresh
  `CachedInfo()` (`NumPending`, `NumAckPending`); the durable grant allows its INFO request. `pulse.Subscription.Consumer()`
  names the consumer. The SDK itself exposes no pending counts.

## What SpaceMan publishes (ADR-0015, ADR-0030)
SpaceMan's outbound side (`internal/outbox`, config `events.outbound`, off by default) uses only the SDK calls above:
`pulse.Connect(url, WithCredsFile, WithSource(source), WithName("spaceman-publisher"), WithCAFile?)` and
`Client.Publish(ctx, subject, type, json.RawMessage(payload), WithID(id), WithTime(created), WithHeader(…))`.
The Pulse client needs a `publish:evt.spaceman.>` grant and nothing else.

- **Subjects:** `evt.spaceman.<area>.<entity>.<action>` (5 tokens, each `^[a-z][a-z0-9_]*$`).
- **Types (`ce-type`):** `spaceman.<area>.<entity>_<action>.v<n>` (Porch convention, `docs/contracts/porch.md`).
- **`Nats-Msg-Id`:** the outbox row ID, `evt_` + 26 lowercase Crockford base32 characters (a UUIDv7, ADR-0002), the same
  on every retry. Pulse does not validate the ID; Pulse's own `NewID` would be a ULID.
- **`ce-source`:** `events.outbound.source` (default `spaceman`). **`ce-time`:** when the event was recorded (the commit
  of the change), not when it was published. **`content-type`:** `application/json`.

| Subject | Type |
|---|---|
| `evt.spaceman.fleet.run.launched` | `spaceman.fleet.run_launched.v1` |
| `evt.spaceman.fleet.run.started` | `spaceman.fleet.run_started.v1` |
| `evt.spaceman.fleet.run.ended` | `spaceman.fleet.run_ended.v1` |
| `evt.spaceman.queue.task.created` | `spaceman.queue.task_created.v1` |
| `evt.spaceman.queue.task.status_changed` | `spaceman.queue.task_status_changed.v1` |
| `evt.spaceman.inbox.item.created` | `spaceman.inbox.item_created.v1` |
| `evt.spaceman.human.action.created` / `.resolved` / `.cancelled` / `.expired` | `spaceman.human.action_created.v1` / `_resolved` / `_cancelled` / `_expired` |
| `evt.spaceman.approvals.approval.decided` | `spaceman.approvals.approval_decided.v1` |
| `evt.spaceman.approvals.approval.executed` | `spaceman.approvals.approval_executed.v1` |
| `evt.spaceman.memory.collection.synced` | `spaceman.memory.collection_synced.v1` |
| `evt.spaceman.kb.proposal.accepted` | `spaceman.kb.proposal_accepted.v1` |

Payload fields are listed in `site/guide/publishing-events.md` (identifiers and states only).

**SpaceMan extension headers (not part of the Pulse contract; Pulse passes extra headers through and decodes them as
`Event.Extra`):**

| Header | Value |
|---|---|
| `spaceman-correlation-id` | The work the event belongs to (run, task, human action correlation, approval, collection, proposal, inbox item correlation). Omitted when unknown. |
| `spaceman-causation-id` | What caused it, when known: an inbound Pulse event ID, a task origin, a deciding human action, a run, a node sync or command ID, or the API request ID (`req_…`). |
| `spaceman-origin` | The publishing installation's `source`. SpaceMan's inbound side drops (records as `filtered`) events carrying its own origin. |
| `spaceman-depth` | The automation depth (ADR-0049): how many SpaceMan ingestions led to the change, as a decimal number; sent only when above 0. The inbound side gives an item the event's depth plus one (no header: 0) and quarantines an event at or over the space's `event_depth` limit, or with a value that is not a plain decimal number (at most 1,000,000). |

Header values are identifier-like (`^[A-Za-z0-9][A-Za-z0-9._:/#@+=-]{0,127}$`); other values are not sent.
Delivery is at least once: deduplicate on `Nats-Msg-Id` (JetStream only does it for 2 minutes).
