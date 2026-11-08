# Observability

SpaceMan writes structured JSON logs and serves health checks and Prometheus metrics on a **separate management
listener** (`http.management_listen`, for example `127.0.0.1:9090`). The management listener is unauthenticated:
bind it to a private address and let only your monitoring reach it.

| Path | What it answers |
|---|---|
| `/healthz` | The process is alive. |
| `/readyz` | 200 while the database is healthy and writable, also when only degraded (database busy, or the data disk below its 256 MiB / 2 % reserve); 503 otherwise. It is served once central has started, which needs the KEK. |
| `/metrics` | Prometheus metrics in the text exposition format. Scrapes are limited in concurrency and time. |

## Attention page

For people, **Attention** in the web UI (and `GET /api/v1/attention`, `spaceman system attention`) gathers what needs
a person in one view: pending, overdue and escalated human actions, blocked and failed tasks, quarantined events, runs
whose state is unknown and runs that failed or were lost in the last 24 hours, budget warnings of the last 24 hours,
attachment storage at 90 % or more of a space's quota, and spaces whose node is not connected, each linked to the page
that handles it. With `system:read` it also shows enrolled nodes that are not connected, outbound dead letters,
failing backups or verifications, and central's storage: readiness and its checks, the database's health and the data
directory's free space against its reserve. Every section appears only where you may read it. Each space's overview
shows the same counters for that space.

It is computed on request, one grouped query per category, and refreshes when the change feed reports a change to a
space, run, node, task, human action, subscription, limit or backup. Storage health has no change event: use
**Refresh**, or the metrics and `/readyz` for alerting. Node disks are not reported; a recording cut short by a full
node disk shows on its run.

<Shot name="attention" alt="The Attention page: two spaces with pending human actions and quarantined events, outbound events, backups and central's storage with the data disk's free space" caption="Everything that needs a person" />

## System page

**System** (under Access, for global administrators) is this instance's own status, like Hallpass's System page. It is
also `GET /api/v1/system` (`system:read`) and `spaceman system status`:

- **Status:** readiness, whether the database accepts writes (the `store.writable` check), the database's health, the
  version, commit and build date, the **boot ID** (new at every start of central, so a restart is easy to spot and to
  match with the logs), the start time and uptime, the database schema version (the last applied migration; a warning
  shows when the binary has a newer one), and the node protocol range central accepts.
- **Activity:** spaces that are not archived, connected nodes of the active ones, runs starting, running or stopping,
  open human actions, and connected live-event streams (browser change feeds, terminal and recording streams).
- **This instance:** the public URL, the data directory, the database and WAL sizes, the data disk's free space, and
  the Go version and platform.

It names no key material, key path or token. The page refreshes every 15 seconds. The version alone is public: the
account menu shows it to everyone, and `spaceman system info` (`GET /api/v1/system/info`) reads it without a token.

<Shot name="ops-system" alt="The System page: readiness, accepting writes, version, commit, boot ID, start time, schema version and node protocol range; counts of spaces, nodes, runs, human actions and live streams; the public URL, data directory, database size and data disk" caption="This instance's status" />

## Labels are bounded

Every label value comes from a fixed list or a capped set of code-defined names (transaction names, job names, OIDC
provider slugs, HTTP route templates). Anything else is reported as `other`. Space, node, run, task, subscription and
event identifiers, subjects, client addresses, prompts, payloads and credentials are never labels.

## Metric catalog

All SpaceMan metrics use the `spaceman_` prefix. The Go runtime (`go_*`) and process (`process_*`) collectors are
included as well.

### Storage

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `spaceman_store_tx_duration_seconds` | histogram | `name` | Write transaction duration including retries. |
| `spaceman_store_tx_total` | counter | `name`, `result` | Write transactions by final result (`ok`, `busy`, `full`, `io`, `corrupt`, `constraint`, `conflict`, `not_found`, `canceled`, `error`). |
| `spaceman_store_tx_busy_retries_total` | counter | | Attempts retried after `SQLITE_BUSY`. |
| `spaceman_store_writer_wait_seconds` | histogram | | Time spent waiting for the single writer connection. |
| `spaceman_store_writer_queue_depth` | gauge | | Write transactions waiting for the writer. |
| `spaceman_store_db_bytes`, `spaceman_store_wal_bytes` | gauge | | Database and WAL file sizes. |
| `spaceman_storage_filesystem_size_bytes`, `spaceman_storage_filesystem_avail_bytes` | gauge | `volume` (`data`, `artifacts`, `backups`) | Size and free space of the filesystem holding a volume. |

### Inbound events (ingestion)

These cover [event subscriptions](/guide/events): durable JetStream consumers and core NATS subscriptions. The
`delivery` label is `durable` or `core`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `spaceman_ingest_deliveries_total` | counter | `delivery`, `origin`, `result` | Events handled. `origin` is `live`, `redelivery` (the stream delivering again: a retry), `replay` (an operator's replay) or `reprocess` (a reprocessed quarantine entry). `result` is `created` (an inbox item), `duplicate`, `filtered` (a filter did not match, or SpaceMan's own event), `quarantined`, `error` (storage or other transient failure) or `rate_limited`. |
| `spaceman_ingest_processing_duration_seconds` | histogram | `delivery` | Time to handle one event: validate, render and store. Rate-limited events are not timed. |
| `spaceman_ingest_consumer_pending` | gauge | `delivery` | Events waiting for SpaceMan, summed over subscriptions. Durable: messages the JetStream consumers have not delivered yet (consumer lag, `num_pending`). Core: messages buffered in the client. |
| `spaceman_ingest_consumer_ack_pending` | gauge | | Events the durable consumers delivered and SpaceMan has not acknowledged yet (`num_ack_pending`). |
| `spaceman_ingest_core_dropped_total` | counter | | Core NATS messages the client dropped because a subscription fell behind (slow consumer). They are lost. |
| `spaceman_ingest_subscriptions` | gauge | `delivery`, `state` (`running`, `stopped`, `error`, `disabled`, `paused`) | Subscriptions by runtime state. |
| `spaceman_ingest_connections` | gauge | `connection_kind` (`pulse`), `state` (`connected`, `reconnecting`, `error`, `disabled`) | Event connections by state. |
| `spaceman_ingest_quarantine_open` | gauge | | Open quarantine entries over every subscription. |
| `spaceman_ingest_replays_total` | counter | | Explicit replays recorded: replay operations and cursor changes that deliver history again. |

The gauges are refreshed after every round of the ingestion manager: on any connection or subscription change, and
every 30 seconds. Durable pending counts take one consumer-info request per subscription and round; a consumer that
cannot be reached is left out of the sum. Rate-limited events are counted with origin `live`, because they are
refused before the delivery history is read.

### Outbound events and outbox

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `spaceman_events_publish_total` | counter | `outcome` (`published`, `retry`, `dead`) | Publish attempts of SpaceMan's own events ([publishing](/guide/publishing-events)). |
| `spaceman_events_pending`, `spaceman_events_dead` | gauge | | Events waiting to be published, and dead-lettered ones. |
| `spaceman_events_lag_seconds` | gauge | | Age of the oldest event waiting to be published. |
| `spaceman_outbox_pending`, `spaceman_outbox_dead` | gauge | | Outbox messages queued or awaiting retry, and dead-lettered ones. |

### Jobs, backups, HTTP and sign-in

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `spaceman_job_runs_total` | counter | `job`, `result` (`ok`, `error`) | Background job runs. |
| `spaceman_backup_runs_total` | counter | `result` (`succeeded`, `failed`) | Backup runs made by central (schedule, Backups page, API); offline `spaceman backup` commands are not counted. |
| `spaceman_backup_duration_seconds` | histogram | | Backup run duration. |
| `spaceman_backup_last_success_timestamp_seconds` | gauge | | Start of the last successful backup (Unix seconds; 0 if none). |
| `spaceman_http_requests_total` | counter | `route`, `method`, `code` | Requests by route template, method and status class. |
| `spaceman_http_request_duration_seconds` | histogram | `route`, `method` | Request duration. |
| `spaceman_http_requests_in_flight` | gauge | | Requests being served. |
| `spaceman_sse_clients` | gauge | | Connected live-event streams. |
| `spaceman_oidc_logins_total` | counter | `provider`, `outcome` | OIDC login completions. |
| `spaceman_user_logins_total` | counter | `method`, `outcome` | Login attempts. |

### Nodes and runs

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `spaceman_nodes_connected` | gauge | | Connected node agents. |
| `spaceman_node_connects_total` | counter | `result` | Node connection attempts. |
| `spaceman_node_commands_total` | counter | `type`, `result` | Commands sent to nodes by acknowledgement. |
| `spaceman_execution_lease_fenced_total` | counter | | Reports or commands rejected for a stale lease generation. |
| `spaceman_runs_active` | gauge | `harness` | Live runs. |
| `spaceman_runs_started_total` | counter | `harness`, `result` | Launch outcomes. |
| `spaceman_runs_ended_total` | counter | `harness`, `reason` | Run ends. |
| `spaceman_gateway_revocations_failed` | gauge | | Harbor Pilot run or validation tokens whose revocation failed; each stays valid until it expires unless a person revokes it, or its parent, in Harbor Pilot. |
| `spaceman_build_info` | gauge | `version`, `revision`, `goversion` | Always 1. |

## Suggested alerts

| Condition | Expression |
|---|---|
| Ingestion failing | `sum(rate(spaceman_ingest_deliveries_total{result="error"}[10m])) > 0` |
| Events piling up | `sum(spaceman_ingest_consumer_pending) > 1000` for 15 minutes |
| Core NATS losing messages | `increase(spaceman_ingest_core_dropped_total[15m]) > 0` |
| Quarantine growing | `spaceman_ingest_quarantine_open > 0` |
| Subscription or connection down | `spaceman_ingest_subscriptions{state="error"} > 0` or `spaceman_ingest_connections{state=~"error|reconnecting"} > 0` for 5 minutes |
| Retries hot | `sum(rate(spaceman_ingest_deliveries_total{origin="redelivery"}[10m])) > 1` |
| Publication stuck | `spaceman_events_lag_seconds > 300` or `spaceman_events_dead > 0` |
| Backups stale | `time() - spaceman_backup_last_success_timestamp_seconds > 2 * 86400` |

## Logs

Logs go to standard error as JSON (`slog`, one object per line; `log.format: text` for a terminal). They never contain
credentials, request bodies, prompt text, terminal output or event payloads. An event that core NATS ingestion lost is
logged as `core NATS message lost` with the subscription and event IDs only.
