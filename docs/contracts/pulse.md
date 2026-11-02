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
