# SpaceMan — Test strategy

> Requirement: SM-VER-001 (every verification topic of the brief §40 is covered by automated tests, with simulated and real
> evidence kept apart). Per-requirement evidence: [TRACEABILITY.md](TRACEABILITY.md). Known gaps: [LIMITATIONS.md](LIMITATIONS.md).

## Kinds of evidence

- **Simulated** means an automated test that uses fakes or embedded dependencies:
  - the in-memory runner `internal/runner/runnertest` instead of herdr-core;
  - stub harness binaries;
  - an embedded nats-server (`internal/events/pulsetest`, operator/JWT mode with JetStream) driven by the real go-pulse SDK;
  - an in-process OIDC provider;
  - a fake S3 (`internal/s3client/s3test`);
  - a local central for Terraform.

  Simulated evidence shows that SpaceMan behaves correctly against the model. It does not show that the model matches the dependency.
- **Real** means a run against the real dependency: herdr-core, Claude Code, Codex, Pulse, an identity provider, S3, Linux/systemd, containers, browsers. These suites are opt-in (build tag `real` plus a variable per dependency: `SPACEMAN_REAL_CLAUDE`, `SPACEMAN_REAL_PORCH`, `SPACEMAN_REAL_PULSE`, `SPACEMAN_REAL_S3`). Credentialed ones are budget-bounded and clean up only what they create (SM-VER-002). Each real result is recorded with the date, the versions and the platform in TRACEABILITY.md.

## Suites

| Command | Suite | Evidence |
|---|---|---|
| `make check-fast` | The inner-loop gate: vet, the custom analyzers, golangci-lint, traceability, the analyzer tests and `go test ./...` without `-race` and with go test's cache (only changed packages and their dependents run again); `sqlc-check` and `api-check` when their inputs changed since the merge base. `make check` (race, the coverage floor) runs once before pushing ([AGENTS.md](../AGENTS.md), "Gates") | simulated |
| `make test` / `make race` (in `make check`) | Unit and in-process end-to-end tests of every package, including `internal/app`: central on a real HTTP server, a node agent with the in-memory runner, real Git, real SQLite. Locally `make race` uses go test's cache; CI and the release workflow run `make race RACE_COUNT=1`, so every test runs there (ADR-0070). `make race` runs `internal/app` as its own invocation next to the other packages, with its own `-parallel` cap (`RACE_APP_PARALLEL`; ADR-0069, below) | simulated |
| `make test-fault` | `test/fault` (build tag `fault`): crashes, a full disk, interrupted recordings, disconnects, partitions, corrupted state, database contention (below) | simulated; central's crash is a real `SIGKILL` of a child process |
| `make test-pulse` | Pulse/NATS integration: `internal/events/...` and `internal/outbox`, plus the central end-to-end event paths, against an embedded nats-server with the real go-pulse SDK | simulated; no `pulsed` |
| `make test-real` | Build tag `real`: an isolated herdr-core core (`internal/runner/hc`, `test/real`). With `SPACEMAN_REAL_CLAUDE=1` and `CLAUDE_CODE_OAUTH_TOKEN`, also budget-bounded Claude Code runs (Haiku). The suites below are part of it and skip without their own variables | real |
| `make test-real-porch` | `TestRealPorchOIDC`: go-porch built from the sibling checkout (`PORCH_SRC`) into a temp dir, with Porch's fake Google fixture upstream; SpaceMan registered as a Porch client; sign-in, `porch.groups` mapping, group JIT and step-up through a scripted browser | real Porch (development mode); fake upstream |
| `make test-real-pulse` | `TestRealPulseGitHub`: pulsed and nats-server (`PULSE_SRC`) and the `pulse-github` service (`PULSE_GITHUB_SRC`) publishing its own webhook fixtures; ingestion into two spaces, dedup, replay, quarantine, loop guard, outbound publication read back with go-pulse | real pulsed, nats-server and go-pulse-github; GitHub App lookup stubbed with pulse-github's test JSON |
| `make test-real-s3` | `TestRealS3BackupRestore`: a throwaway MinIO container (pinned digest, random port, TLS with a test CA, generated credentials); client calls, API and CLI backups, list, verify, tamper detection, offline restore and comparison. `TestRealS3Artifacts`: the S3 artifact store against the same image (store contract, an API attachment upload and range download with `artifacts.backend: s3`, an offline backup copying it in, `spaceman artifacts migrate` both ways, a restore migrated to a new prefix and served) | real MinIO |
| `make test-real-codex` | `TestRealCodexRun`: a real, billed Codex turn through SpaceMan with an `openai_api_key` account (`OPENAI_API_KEY`) or a `codex_chatgpt_login` account (`SPACEMAN_REAL_CODEX_AUTH_FILE`): Codex answers, herdr-core reports its turn through OSC 7501, and the credential is at rest only where ADR-0009, ADR-0053 and ADR-0054 put it | real (opt-in) |
| `make test-real-s3-external` | `TestRealS3External`: the artifact store and an S3 backup against your own bucket (AWS S3, Cloudflare R2 or a self-hosted service), under a random prefix it deletes afterwards | real (opt-in) |
| `make test-real-systemd` | `scripts/systemd-smoke.sh`: the linux release archive installed in a Debian 13 container with systemd as PID 1 (`--cgroupns=private --cap-add SYS_ADMIN`); `spaceman.service` health, readiness, stop/start/restart, KEK assertion, `systemd-analyze security`; `spaceman-agent.service` refusing cleanly without enrollment or herdr-core | real systemd 257 (container, linux/arm64) |
| provider repository: `make test`, `make acc` | Terraform provider unit tests. Acceptance tests run the Terraform binary against a throwaway local central | simulated (local central), real Terraform |
| `make web-check` | UI typecheck, vitest unit tests (Markdown sanitizer, contrast, forms) and the production build | simulated |
| `make e2e` | Playwright in Chromium against a fresh local stack: real central, a real `spaceman-agent`, an isolated herdr-core core, a stub harness | real browser, real hc; stub harness |
| `make demo` | `test/demo` (build tag `demo`): the brief's §41 demonstration, thirty steps in order against a real central with the UI, two real agents with their own herdr-core cores, devpulse and devmcp, through the browser, the API and the CLI; one evidence line per step ([DEMO.md](DEMO.md)) | real processes, browser and hc; stub harness (simulated steps marked per step); `SPACEMAN_DEMO_REAL=1` adds a real Claude Code worker |
| `make e2e-browsers` | The same journey in Firefox and WebKit (Playwright's builds, `SM_E2E_BROWSERS`), each on its own fresh stack | real browsers, real hc; stub harness |
| `make screenshots` | Seeded demo instance. Every captured page is audited with axe (WCAG 2.1 A/AA) in both themes | real browser |
| `make site-check` | Site build with a dead-link check, screenshot references, Mermaid parsing, API-reference drift, and axe on every site page | real browser |
| `make fuzz-smoke` | Every `Fuzz*` target for `FUZZTIME` | simulated |

The browser rows (`make e2e`, `make screenshots`, `make site-check`) take `SM_CHROMIUM_ARGS`, extra Chromium launch arguments separated by spaces, for hosts whose sandbox stops Chromium from starting, for example `SM_CHROMIUM_ARGS='--no-sandbox --disable-gpu --disable-dev-shm-usage --no-zygote'` inside a container. The Makefile passes it through from the environment (a node can set it for every run); unset, as in CI, it adds nothing. The site targets (`make site`, `site-verify`, `site-check`, `site-a11y`) add `--single-process` to a non-empty value, which the Mermaid check and the audit need on such hosts.

## Heavy gates on a shared node (ADR-0069)

Several agents often run their gates on one machine. Full suites running at once slow each other down, and the
contention trips the write-transaction guard and the timing tests. So the heavy targets (`make race`, `make coverage`,
`make site-a11y`, `make e2e`, `make e2e-browsers` and `make screenshots`) run through `scripts/gate-slot.sh`, which
holds one of a fixed number of `flock` slots while the command runs:

- `SM_GATE_SLOTS`: the number of slots, 2 by default on a machine with 16 or more CPUs, otherwise 1. `0` disables
  the semaphore, and it is always off when `CI=true` (each CI job has its own runner), even with `SM_GATE_SLOTS` set.
- `SM_GATE_LOCK_DIR`: the directory of the lock files, `${XDG_CACHE_HOME:-~/.cache}/spaceman-gates` by default
  (created when missing). Every run that should take turns must use the same directory, so a node shared by several
  users sets it in their common environment and pre-creates it with mode 1777 (`install -d -m 1777 <dir>`); the
  script creates the slot files readable by everyone, which is all `flock` needs.
- Without `flock(1)` (macOS has none by default) the script prints a warning and runs the command without a slot.
- `scripts/gate-scripts-test.sh` checks the slots and `scripts/race.sh`'s interrupt handling in a few seconds (no Go
  build): run it after changing either script.
- `make check` takes one slot for the coverage floor and the race run together; nested heavy targets don't take a second.
- While every slot is busy the run prints `gate-slot: <target> waiting for a heavy-gate slot (N busy: …)` with what holds
  them, then `got slot … after N s` when it starts. Waiting costs no CPU.
- The kernel releases a slot when its holder exits, however it exits (`kill -9` included), so a crashed run never
  leaks one.

The race run's critical path is `internal/app` (about an hour of test time under `-race`). `scripts/race.sh` runs
it as its own `go test -race` invocation, started first, with `-parallel` `RACE_APP_PARALLEL` (default
`min(16, nproc/2)`, at least 4; CI uses 4), and every other package alongside with `RACE_PARALLEL` (4). With 16 or
more CPUs (`RACE_APP_SPLIT=1`, the default there) internal/app's serial tests, which go test would otherwise run one
after another before any parallel test starts, run in a second process at the same time (`-run` and `-skip` with the
same pattern, so every test runs exactly once). Its output is printed when it finishes, after the other packages'.

## Fault suite (`make test-fault`)

| Test | Fault | What must hold |
|---|---|---|
| `TestFaultRecordingDiskFull` | The node's disk fills up while a run is being recorded (an injected segment file returns `ENOSPC` mid-frame) | The run keeps running and its exit is observed. Central's copy keeps the earlier records and ends with a `trunc` (`disk`) record and the close record. The local recording stays readable. |
| `TestFaultInterruptedRecordingAndUpload` | Central becomes unreachable during the upload, then the agent is killed with a torn zstd frame on disk | The torn tail is reported (`Truncated`) and the frames before it are kept. The restarted agent adopts the run and resumes with a gap marker. Catch-up ships what central missed. Central's copy is strictly ordered without duplicates. |
| `TestFaultAgentKilledMidRun` | The agent dies with live runs (journal closed under it, no graceful shutdown), and again between the runner starting a program and the journal recording it | Every program is adopted on restart. Nothing is launched twice. |
| `TestFaultCentralKilledMidWrite` | Central, a real child process, is `SIGKILL`ed while four writers commit transactions. Three rounds on one data directory. | The next start opens cleanly. Every write reported as committed survives. The audit chain verifies and is extended by new writes. |
| `TestFaultDisconnectBeyondLeaseAndGrace` | Central is unreachable longer than the lease plus the grace | The node stops the run on its own, not before the lease could expire. Nothing is relaunched when central returns, and the end is reported. |
| `TestFaultPartitionDuringCommand` | The launch reaches the node, but its acknowledgement and run report are black-holed, then the connection is cut (a TCP proxy) | The command stays `sent` during the partition. After healing it is re-delivered (at least once) and acknowledged as `duplicate`. The program is started once. |
| `TestFaultCorruptNodeState` | A corrupted or truncated journal, a partial lease file, a partial secrets file, a partial harness configuration, a garbage recording segment, a corrupted central database | The journal and the database are refused with `ErrCorrupt` and a hint to restore. The lease guard ends runs on an unreadable lease (fail closed). The launch shim refuses a half-read environment and removes it. A partial configuration is never overwritten. Garbage segments are reported as truncated and skipped. |
| `TestFaultDatabaseBusyIsRetryable` | Another process holds SQLite's write lock past the store's bounded retries | The API answers 503 `unavailable` with `Retry-After` and writes nothing. A retry with the same `Idempotency-Key` creates the resource once. |

## Coverage of the brief's verification topics (§40)

| # | Topic | Simulated | Real |
|---|---|---|---|
| 1 | Queue order and incident demotion | `internal/queue` `TestOrderingAndIncidents`, `TestStatesAndPreemption`; `TestQueueOverHTTP` | `make e2e` (inbox → task, Alt+↑ reorder, start) |
| 2 | Dependencies, exclusions, human blockers | `TestEligibility` (cycles, blocked, dependent and excluded tasks skipped with reasons), `TestBlockingAffectsOnlyTheTask` | — |
| 3 | Idempotent launches, control uniqueness, fencing | `TestFleetLifecycle`, `TestRestartReconciliation`, `TestRelocationUnavailableSource` (stale generation fenced), `TestFaultPartitionDuringCommand`, `TestFaultAgentKilledMidRun` | `TestRealLaunchAdoptObserveClose`, `TestRealNodeEndToEnd` (hc 0.1.16, macOS) |
| 4 | Restart and disconnect enforcement | `TestDisconnectEnforcement`, `TestRestartReconciliation`, `TestFaultDisconnectBeyondLeaseAndGrace`, `TestFaultCentralKilledMidWrite`, `TestRelocationResumesAfterCentralRestart` | `TestRealDisconnectEnforcement`, `TestRealNodeEndToEnd`, `TestRealLeaseGuardParent` |
| 5 | Memory conflicts and sync | `TestMemoryInstancesShareAndPreserve`, `TestApplyRefusesAndPreserves`, `TestApplyFastForwardsAndServes`, `TestMemorySyncedToCentral`, `TestRunRefusesUnsafeRepositoryConfig` | — |
| 6 | Prompt and template provenance | `internal/catalog` `TestPromptLifecycle`, `TestResolution` (layers, origins, policy ceilings, manifests); `TestTemplatesAndConnections` (immutable template versions, follow-latest) | — |
| 7 | MCP isolation, authorization and failures | `internal/gateway` `TestGatewayCalls`, `TestEgressPolicy`, `TestUpstreamRouting`; `TestGatewayOverBridge`, `TestGatedGatewayCall`, `TestAgentToolsOverBridge` | `TestRealClaudeUsesGatewayTool`, `TestRealClaudeUsesSpaceManTools` (Claude Code, Haiku) |
| 8 | Auth, bootstrap, OIDC, revocation | `TestBootstrapAndForcedEnrollment`, `TestAPIBootstrapLoginAndConventions`, `TestLoginThrottlingAndEnumeration`, `TestMFAThrottleHoldsUnderConcurrency`, `TestLoginThrottleHoldsUnderConcurrency`, `TestIDTokenValidation`, `TestOIDCSignInOverHTTP`, `TestWatchStreamEndsOnRevocation`, `TestTerminalStreamEndsOnRevocation`, `TestPurgeSpaceForgetsItsGrants` | `make e2e` (setup, sign-in with TOTP, password reset); `TestRealPorchOIDC` (real Porch v0.3.0: PKCE S256, RFC 9207 `iss`, `porch.groups` mapping, group JIT, replay refused, step-up, group removal) |
| 9 | Secrets | `TestCreateIsWriteOnly`, `TestLaunchPayloadRedacted`, `TestRedactionNeverLeaks`, `TestScrubEnv`, `TestSealBindsContext`, `TestClientSecretWriteOnly`, `TestFaultCorruptNodeState` (partial secrets file) | `TestRealClaudeRun` (secrets file unlinked; token absent from hc state), `TestRealClaudeUsesSpaceManTools`; `TestRealS3BackupRestore` (no plaintext secret in any bucket object; credentials generated at runtime) |
| 10 | Event schema, variable and wildcard validation | `TestValidateDefinition`, `TestBindRejectsWidening`, `TestInputSchemaAndFilters`, `TestInstructionsSeeOnlyVariables`; `TestEventsOverHTTP` (wildcard variable refused) | — |
| 11 | Durable acknowledgement, dedup, fan-out | `TestIngestFanOutAndProvenance`, `internal/inbox` `TestDedup`, `TestDurableRoundTrip`, `TestAcknowledge`, `TestMessagesDeliveredToIdleRuns` | `TestRealPulseGitHub` (real pulsed + go-pulse-github: durables under exact grants, two spaces one item each, webhook redelivery, same-ID re-publish and replay deduplicated) |
| 12 | Subscription edits, replay, quarantine | `TestCursorSemantics`, `TestQuarantineAndReprocess`, `TestReplayIsExplicitAndDistinctFromRetry`, `TestEventsOverHTTP`; core NATS refuses cursors: `TestCoreSubscriptionRefusesCursors`, `TestCoreSubscriptionEndToEnd` | `TestRealPulseGitHub` (schema edit with `If-Match`, quarantine and reprocess, explicit replay recreating the consumer on real pulsed) |
| 13 | Restore cursor reconciliation | `TestApplyPendingRestore` (subscriptions replay from the oldest stored delivery), `TestBackupRestoreEndToEnd` | — |
| 14 | Feedback-loop prevention | `TestOwnEventsAreNotReingested`, `TestDepthLoopsAndQuotas` (cross-space chains) | `TestRealPulseGitHub` (own outbound events subscribed back are filtered) |
| 15 | KB allocation, reuse, continuity, busy handling | `TestKBSupportSessions` (one session per requester, reuse, queuing at capacity, evidence), `TestKnowledgeRoutedToSupportSession` | — |
| 16 | KB capacity without deadlock | `TestAdmissionPoolsAndCapacity` (KB support has its own pool), `TestKBSupportSessions` (requests queue at capacity and complete) | — |
| 17 | Mixed-harness communication, concurrent edits | `TestMessagesDeliveredToIdleRuns` (Claude/Claude, Claude control with a Codex worker, Codex control with a Claude worker), `TestControlLaunchesWorkersInWorktrees` (separate worktrees), `TestMemoryInstancesShareAndPreserve` (concurrent memory edits merge or are preserved) | — (no real Codex run yet: no Codex or OpenAI credential) |
| 18 | Human default and custom schemas, immutable versions | `TestTypeCatalog`, `TestCustomActions` (actions keep their type version; custom outcomes grant nothing), `TestTypeSpecificResponses` | — |
| 19 | Human submission permissions and idempotency | `TestHumanActionsOverHTTP` (If-Match on responses, approval decisions need `approval:decide`), `TestRequesterLimit`, `TestAttachmentsAndHuman` | `make e2e` (a blocking question answered) |
| 20 | Concurrent responses, assignment, expiry, cancellation | `TestConcurrentResponsesOneWins`, `TestCommentsAssignCancel`, `TestBlockingAffectsOnlyTheTask` (expiry), `TestRemindersAndEscalation` | — |
| 21 | Requester sleep, restart and loss; durable routing | `TestHumanResponseRoutedWhenTheRequesterEnds`, `TestResponsesForAnEndedRequesterAreRerouted`, `TestNextLaunchHoldsForTheInstance`, `TestStrandedNotificationsMoveOn`, `TestHumanResponsesReachOnlyTheirTarget` | — |
| 22 | Global-page filtering | `TestHumanActionsOverHTTP` (the global view filtered to readable spaces), `TestWatchStreamFiltersAndExpires`, `TestSpaceScopedRoles` | — |
| 23 | Approval binding, revocation, replay prevention | `TestDigest`, `TestApproveExecuteOnce`, `TestConcurrentUseRunsOnce`, `TestDecisionsAndRechecks`, `TestGatedGatewayCall` | — |
| 24 | Approval versus execution outcomes | `TestApproveExecuteOnce`, `TestRecoverInterrupted` (an interrupted execution is never retried), `TestApprovalEventsRecorded` | — |
| 25 | Acknowledgement not granting permission | `internal/messages` `TestAcknowledge`, `TestTools` (only recipients acknowledge, reply or resolve), `TestCustomActions` (a custom "approved" outcome grants nothing), `TestRevocationDoesNotUndoCompletedActions` | — |
| 26 | Recording interruption and disk exhaustion | `TestTornSegmentRecovers`, `TestQuotaTruncates`, `TestDiskFullTruncates`, `TestFaultRecordingDiskFull`, `TestFaultInterruptedRecordingAndUpload`, `TestRecordingShippedReplayedAndStreamed` | `TestRealNodeEndToEnd` (recording through the real agent and hc); `make e2e` (live and replayed run) |
| 27 | Backup and restore | `internal/backup` `TestRoundTrip`, `TestVerifyDetectsTampering`, `TestVerifyDatabaseCopy`, `TestServiceS3` (fake S3); `TestBackupRestoreEndToEnd` | `TestRealS3BackupRestore` (MinIO over TLS: API and CLI backups, list, verify, tamper detection, offline restore from the bucket, records compared); MinIO only |
| 28 | Dirty relocation refusal | `TestRelocationDirtyRefusedThenExported`, `TestRelocationInspectExportRestore`, `TestAddWorktreesRefusesEscapes`, `TestReadLooseRefusesChainedAndReservedLinks` | — (no relocation between real machines) |
| 29 | Terraform drift, import, secrets | provider `make test` (`TestNoSecretsStored`, `TestVersionInPrivateState`, `TestSplitImportID`, `TestDocsUpToDate`) | provider `make acc` (`TestAcc*`: Terraform 1.17 against a local central) |
| 30 | UI and accessibility | `make web-check` (vitest, including contrast tests) | `make e2e`, `make screenshots` (axe in both themes, Chromium); `make e2e-browsers` (Firefox, WebKit) |
| 31 | Site, screenshots, links, Mermaid | — | `make site-check` (dead links, screenshot references, Mermaid, API drift, axe on every page) |

A topic marked "—" under Real has no real-environment evidence yet. LIMITATIONS.md and TRACEABILITY.md record what is missing (for example real Codex runs, AWS S3, a physical Linux host and amd64 under systemd, and Google and GitHub behind Porch and go-pulse-github). Fakes are never presented as working integrations.

## Cross-cutting invariants

Tests added for invariant and architecture requirements that had no feature-specific test (traceability audits of
the `todo` and `partial` rows, 2026-11-03). All are simulated.

| Test | Requirement | What must hold |
|---|---|---|
| `internal/app` `TestNodeDaemonIsNotAController` | SM-INV-005, SM-RUN-003 | With central unreachable, the bridge refuses inbox, task, worker-launch and queue tools with `unavailable`. The node starts no program, and nothing it refused is replayed after reconnecting. |
| `internal/app` `TestIdleSpaceKeepsNoLeaseGuard` | SM-ARCH-004, ADR-0043 | A space's lease guard lives while the space has live runs and ends with the last one. The next launch starts a fresh guard and runs under it. |
| `internal/app` `TestUsageAndTranscript` (extended) | SM-REC-001 | The recording holds terminal output and not the transcript. Deleting the transcript or the recording leaves the other artifacts and the usage unchanged. |
| `internal/app` `TestHumanActionsOverHTTP` (extended) | SM-INV-011 | Asking, blocking, commenting and answering add nothing to the inbox. The queue still holds only the operator's task. |
| `internal/agenttools` `TestCatalogs` (extended) | SM-INV-012 | `spaceman_human_ask` is in the catalog of every run kind. |
| `internal/harness` `TestCapabilitiesAreExplicit` | SM-HARN-003/004, SM-INV-014 | Each adapter advertises exactly its verified capabilities, and none advertises native team messaging. A missing binary advertises nothing. |
| `internal/config` `TestDeployExamplesLoad` | SM-INV-017 | The example configurations in `deploy/examples` load and validate as shipped. |
| `internal/app` `TestRootDoesNotBypassExecutionSafety` | SM-INV-016 | With the root token over HTTP, a second control run is refused and none is created, a space with an active run is neither archived nor purged, editing a space cannot change its pinned node, and a paused space admits no launch. |
| `internal/app` `TestWatchResumesFromItsPosition` | SM-API-002 | The change feed resumes from `after` or `Last-Event-ID` with exactly the missed changes, in order; a position beyond the feed yields a `gap` event and the stream continues. |
| `internal/runner/hc` `TestClientInterruptKillSend` | SM-RUN-001, SM-COMM-003 | Interrupt, kill and safe input map to herdr-core's `terminal.interrupt`, `terminal.kill` and `terminal.send`; idle-gated input sends `if: idle`, and a refusal is `ErrBusy`. |
| `internal/app` `TestKBSupportSessions` (extended) | SM-KB-004 | One requester is a Codex run; its request is served by a Claude Code KB session through the same tools. |
| `internal/agenttools` `TestInboxAndTasks` (extended) | SM-INBOX-001 | Dreamer and KB runs add inbox items under their own source kinds, attributed to the run. |

## Real-evidence run (2026-11-03, SM-VER-002)

macOS (Darwin 24.6), Docker 29.8.2 (linux/arm64 VM), hc 0.1.16, Claude Code 2.1.317, nats-server 2.15.1, Playwright 1.64.1.

| Dependency | Suite | Result |
|---|---|---|
| herdr-core | `go test -tags real ./internal/runner/hc/`; `TestRealNodeEndToEnd`, `TestRealDisconnectEnforcement` | PASS (isolated cores; `HC_TERMINAL_ID`, `HC_TERMINAL_SOCKET`, `HC_SCOPE` unset) |
| Claude Code | `TestRealClaudeRun`, `TestRealClaudeUsesSpaceManTools`, `TestRealClaudeUsesGatewayTool` (Haiku) | PASS |
| Codex | `TestRealCodexReadsNodeLayout` (codex-cli 0.166.0, no credential); `TestRealCodexRun` dry run with an invalid key (2026-11-04: Codex passes sign-in, starts its MCP servers and calls the API) | layout PASS; a real turn not run: no credential |
| Porch OIDC | `make test-real-porch` (go-porch v0.3.0) | PASS |
| Pulse, NATS, go-pulse-github | `make test-real-pulse` (pulsed v0.1.3, go-pulse-github v0.1.0) | PASS |
| S3 | `make test-real-s3` (MinIO RELEASE.2026-10-23T20-06-51Z) | PASS |
| Linux/systemd | `make test-real-systemd` (Debian 13, systemd 257.13, linux/arm64) | PASS; amd64 not run |
| Containers | `make image-smoke` | PASS |
| Browsers | `make e2e-browsers` (Firefox, WebKit) | PASS |

## Verifying a release candidate on another server (SM-VER-002)

The evidence still missing, with the command that collects each piece. Every suite is opt-in, works in a throwaway
directory or under a random prefix, and removes only what it created. Run from a checkout of the release tag with Go,
herdr-core (`hc`), Docker and pnpm installed; unset `HC_TERMINAL_ID`, `HC_TERMINAL_SOCKET` and `HC_SCOPE` first.

| Evidence | Needs | Command |
|---|---|---|
| A real Codex turn (API key) | an OpenAI API key; `codex` on `PATH` | `OPENAI_API_KEY=… make test-real-codex` |
| A real Codex turn (ChatGPT login) | the `auth.json` of a **dedicated** ChatGPT login: sign in again in a spare `CODEX_HOME` (a refresh token works once, so do not use your everyday file) | `SPACEMAN_REAL_CODEX_AUTH_FILE=…/auth.json make test-real-codex` |
| S3 or R2 | a scratch bucket and keys that can put, get, list and delete in it | `SPACEMAN_REAL_S3_ENDPOINT=https://s3.<region>.amazonaws.com SPACEMAN_REAL_S3_REGION=<region> SPACEMAN_REAL_S3_BUCKET=… SPACEMAN_REAL_S3_ACCESS_KEY_ID=… SPACEMAN_REAL_S3_SECRET_ACCESS_KEY=… make test-real-s3-external` (R2: `https://<account>.r2.cloudflarestorage.com`, region `auto`) |
| A physical Linux host and amd64 | a Linux machine (amd64), systemd | `make release-snapshot`, then install the archive with the shipped units as in site/operators/install.md, and run `make test-real` and `make e2e` there; `make test-real-systemd` also on amd64 |
| Real Google sign-in | a Porch deployment federated with Google, a test Google account | configure SpaceMan's OIDC provider against that Porch (site/operators/sso.md) and sign in through the UI |
| Real GitHub events | a GitHub App or webhook for a test repository, go-pulse-github and pulsed | follow site/guide/events.md with the `github-issue` template, open an issue in the test repository and see the inbox item |

Record each result in the table above (date, versions, PASS or what failed) and in TRACEABILITY.md's real-evidence
column; SM-VER-002 becomes implemented when every row has real evidence.
