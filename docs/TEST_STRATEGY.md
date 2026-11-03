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
- **Real** means a run against the real dependency: herdr-core, Claude Code, Codex, Pulse, an identity provider, S3, Linux/systemd, containers, browsers. These suites are opt-in. Credentialed ones need `SPACEMAN_REAL=1`, are budget-bounded and clean up only what they create (SM-VER-002). Each real result is recorded with the date, the versions and the platform in TRACEABILITY.md.

## Suites

| Command | Suite | Evidence |
|---|---|---|
| `make test` / `make race` (in `make check`) | Unit and in-process end-to-end tests of every package, including `internal/app`: central on a real HTTP server, a node agent with the in-memory runner, real Git, real SQLite | simulated |
| `make test-fault` | `test/fault` (build tag `fault`): crashes, a full disk, interrupted recordings, disconnects, partitions, corrupted state, database contention (below) | simulated; central's crash is a real `SIGKILL` of a child process |
| `make test-pulse` | Pulse/NATS integration: `internal/events/...` and `internal/outbox`, plus the central end-to-end event paths, against an embedded nats-server with the real go-pulse SDK | simulated; no `pulsed` |
| `make test-real` | Build tag `real`: an isolated herdr-core core (`internal/runner/hc`, `test/real`). With `SPACEMAN_REAL=1`, also Claude Code and Codex runs on the local logins | real |
| `make tf-test`, `make tf-acc` | Terraform provider unit tests. Acceptance tests run the Terraform binary against a throwaway local central | simulated (local central), real Terraform |
| `make web-check` | UI typecheck, vitest unit tests (Markdown sanitizer, contrast, forms) and the production build | simulated |
| `make e2e` | Playwright in Chromium against a fresh local stack: real central, a real `spaceman-agent`, an isolated herdr-core core, a stub harness | real browser, real hc; stub harness |
| `make demo` | `test/demo` (build tag `demo`): the brief's §41 demonstration, thirty steps in order against a real central with the UI, two real agents with their own herdr-core cores, devpulse and devmcp, through the browser, the API and the CLI; one evidence line per step ([DEMO.md](DEMO.md)) | real processes, browser and hc; stub harness (simulated steps marked per step); `SPACEMAN_DEMO_REAL=1` adds a real Claude Code worker |
| `make screenshots` | Seeded demo instance. Every captured page is audited with axe (WCAG 2.1 A/AA) in both themes | real browser |
| `make site-check` | Site build with a dead-link check, screenshot references, Mermaid parsing, API-reference drift, and axe on every site page | real browser |
| `make fuzz-smoke` | Every `Fuzz*` target for `FUZZTIME` | simulated |

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
| 8 | Auth, bootstrap, OIDC, revocation | `TestBootstrapAndForcedEnrollment`, `TestAPIBootstrapLoginAndConventions`, `TestLoginThrottlingAndEnumeration`, `TestMFAThrottleHoldsUnderConcurrency`, `TestLoginThrottleHoldsUnderConcurrency`, `TestIDTokenValidation`, `TestOIDCSignInOverHTTP`, `TestWatchStreamEndsOnRevocation`, `TestTerminalStreamEndsOnRevocation`, `TestPurgeSpaceForgetsItsGrants` | `make e2e` (setup, sign-in with TOTP, password reset); no real identity provider (Porch) |
| 9 | Secrets | `TestCreateIsWriteOnly`, `TestLaunchPayloadRedacted`, `TestRedactionNeverLeaks`, `TestScrubEnv`, `TestSealBindsContext`, `TestClientSecretWriteOnly`, `TestFaultCorruptNodeState` (partial secrets file) | `TestRealClaudeRun` (secrets file unlinked; token absent from hc state), `TestRealClaudeUsesSpaceManTools` |
| 10 | Event schema, variable and wildcard validation | `TestValidateDefinition`, `TestBindRejectsWidening`, `TestInputSchemaAndFilters`, `TestInstructionsSeeOnlyVariables`; `TestEventsOverHTTP` (wildcard variable refused) | — |
| 11 | Durable acknowledgement, dedup, fan-out | `TestIngestFanOutAndProvenance`, `internal/inbox` `TestDedup`, `TestDurableRoundTrip`, `TestAcknowledge`, `TestMessagesDeliveredToIdleRuns` | — (embedded NATS with the real SDK only) |
| 12 | Subscription edits, replay, quarantine | `TestCursorSemantics`, `TestQuarantineAndReprocess`, `TestReplayIsExplicitAndDistinctFromRetry`, `TestEventsOverHTTP`; core NATS refuses cursors: `TestCoreSubscriptionRefusesCursors`, `TestCoreSubscriptionEndToEnd` | — |
| 13 | Restore cursor reconciliation | `TestApplyPendingRestore` (subscriptions replay from the oldest stored delivery), `TestBackupRestoreEndToEnd` | — |
| 14 | Feedback-loop prevention | `TestOwnEventsAreNotReingested`, `TestDepthLoopsAndQuotas` (cross-space chains) | — |
| 15 | KB allocation, reuse, continuity, busy handling | `TestKBSupportSessions` (one session per requester, reuse, queuing at capacity, evidence), `TestKnowledgeRoutedToSupportSession` | — |
| 16 | KB capacity without deadlock | `TestAdmissionPoolsAndCapacity` (KB support has its own pool), `TestKBSupportSessions` (requests queue at capacity and complete) | — |
| 17 | Mixed-harness communication, concurrent edits | `TestMessagesDeliveredToIdleRuns` (Claude/Claude, Claude control with a Codex worker, Codex control with a Claude worker), `TestControlLaunchesWorkersInWorktrees` (separate worktrees), `TestMemoryInstancesShareAndPreserve` (concurrent memory edits merge or are preserved) | — (no real Codex run yet, K-9) |
| 18 | Human default and custom schemas, immutable versions | `TestTypeCatalog`, `TestCustomActions` (actions keep their type version; custom outcomes grant nothing), `TestTypeSpecificResponses` | — |
| 19 | Human submission permissions and idempotency | `TestHumanActionsOverHTTP` (If-Match on responses, approval decisions need `approval:decide`), `TestRequesterLimit`, `TestAttachmentsAndHuman` | `make e2e` (a blocking question answered) |
| 20 | Concurrent responses, assignment, expiry, cancellation | `TestConcurrentResponsesOneWins`, `TestCommentsAssignCancel`, `TestBlockingAffectsOnlyTheTask` (expiry), `TestRemindersAndEscalation` | — |
| 21 | Requester sleep, restart and loss; durable routing | `TestHumanResponseRoutedWhenTheRequesterEnds`, `TestResponsesForAnEndedRequesterAreRerouted`, `TestNextLaunchHoldsForTheInstance`, `TestStrandedNotificationsMoveOn`, `TestHumanResponsesReachOnlyTheirTarget` | — |
| 22 | Global-page filtering | `TestHumanActionsOverHTTP` (the global view filtered to readable spaces), `TestWatchStreamFiltersAndExpires`, `TestSpaceScopedRoles` | — |
| 23 | Approval binding, revocation, replay prevention | `TestDigest`, `TestApproveExecuteOnce`, `TestConcurrentUseRunsOnce`, `TestDecisionsAndRechecks`, `TestGatedGatewayCall` | — |
| 24 | Approval versus execution outcomes | `TestApproveExecuteOnce`, `TestRecoverInterrupted` (an interrupted execution is never retried), `TestApprovalEventsRecorded` | — |
| 25 | Acknowledgement not granting permission | `internal/messages` `TestAcknowledge`, `TestTools` (only recipients acknowledge, reply or resolve), `TestCustomActions` (a custom "approved" outcome grants nothing), `TestRevocationDoesNotUndoCompletedActions` | — |
| 26 | Recording interruption and disk exhaustion | `TestTornSegmentRecovers`, `TestQuotaTruncates`, `TestDiskFullTruncates`, `TestFaultRecordingDiskFull`, `TestFaultInterruptedRecordingAndUpload`, `TestRecordingShippedReplayedAndStreamed` | `TestRealNodeEndToEnd` (recording through the real agent and hc); `make e2e` (live and replayed run) |
| 27 | Backup and restore | `internal/backup` `TestRoundTrip`, `TestVerifyDetectsTampering`, `TestVerifyDatabaseCopy`, `TestServiceS3` (fake S3); `TestBackupRestoreEndToEnd` | — (no real S3) |
| 28 | Dirty relocation refusal | `TestRelocationDirtyRefusedThenExported`, `TestRelocationInspectExportRestore`, `TestAddWorktreesRefusesEscapes`, `TestReadLooseRefusesChainedAndReservedLinks` | — (no relocation between real machines) |
| 29 | Terraform drift, import, secrets | `make tf-test` (`TestNoSecretsStored`, `TestVersionInPrivateState`, `TestSplitImportID`, `TestDocsUpToDate`) | `make tf-acc` (`TestAcc*`: Terraform 1.17 against a local central) |
| 30 | UI and accessibility | `make web-check` (vitest, including contrast tests) | `make e2e`, `make screenshots` (axe in both themes, Chromium) |
| 31 | Site, screenshots, links, Mermaid | — | `make site-check` (dead links, screenshot references, Mermaid, API drift, axe on every page) |

A topic marked "—" under Real has no real-environment evidence yet. LIMITATIONS.md and TRACEABILITY.md record what is missing (for example a real `pulsed`, a real identity provider, real S3, real Codex runs and Linux hosts). Fakes are never presented as working integrations.
