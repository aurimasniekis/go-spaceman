# SpaceMan — Requirement → Test Traceability

> One row per GOAL.md requirement. **Impl** = implemented in code; **Simulated** = automated test with fakes or embedded
> dependencies (named); **Real** = evidence against the real dependency or environment (named suite and date). Empty means none yet.
> `scripts/check-traceability.py` fails CI when a GOAL.md ID is missing here.

| Requirement | WP | Impl | Simulated evidence | Real evidence | Notes |
|---|---|---|---|---|---|
| SM-INV-001 | M1-02 | partial | `internal/store` `TestLock*` (exclusive data-dir flock, second Open refused) |  | Lock implemented; "one active instance" also needs `serve` (M1-03). |
| SM-INV-002 | M1-08 | implemented | spaces.node_id pinned; runs carry node + lease generation; relocation-only change |  |  |
| SM-INV-003 | | todo | | | |
| SM-INV-004 | | todo | | | |
| SM-INV-005 | | todo | | | |
| SM-INV-006 | M1-08 | implemented | `ux_runs_one_control` unique index; `internal/app` `TestFleetLifecycle` (second control refused) |  |  |
| SM-INV-007 | | todo | | | |
| SM-INV-008 | | todo | | | |
| SM-INV-009 | | todo | | | |
| SM-INV-010 | | todo | | | |
| SM-INV-011 | | todo | | | |
| SM-INV-012 | | todo | | | |
| SM-INV-013 | | todo | | | |
| SM-INV-014 | | todo | | | |
| SM-INV-015 | | todo | | | |
| SM-INV-016 | M1-04 | partial | `TestRootIsNotAUserAndLastAdminGuard` |  | Service-level invariant enforcement continues per feature. |
| SM-INV-017 | | todo | | | |
| SM-INV-018 | | todo | | | |
| SM-WORK-001 | M1-01 | implemented | `scripts/check-traceability.py` (CI `make traceability`) |  |  |
| SM-WORK-002 | M1-01 | implemented | — | docs/contracts/* with revisions; hc spikes S-0..S-2 (2026-11-02, real hc 0.1.16) |  |
| SM-WORK-003 | M1-01 | implemented | — | commits in PLAN.md §3; D-1 deviation recorded |  |
| SM-ARCH-001 | | todo | | | |
| SM-ARCH-002 | | todo | | | |
| SM-ARCH-003 | | todo | | | |
| SM-ARCH-004 | | todo | | | |
| SM-NODE-001 | M1-05 | implemented | `internal/app` `TestFleetLifecycle` (enroll single-use, credential auth), `internal/fleet` revoke/disable via API |  | Credential rotation endpoint for agents pending. |
| SM-NODE-002 | M1-05 | implemented | `internal/app` `TestFleetLifecycle` (capabilities: runner + 2 harnesses), `proto.Negotiate` |  |  |
| SM-NODE-003 | M1-05 | implemented | `internal/proto` `TestCommandValidate`, `TestEnvelopeRoundTrip`; `internal/app` `TestFleetLifecycle` |  |  |
| SM-NODE-004 | M1-05 | implemented | `internal/app` `TestFleetLifecycle` (central restart re-delivers; starts unchanged) | `TestRealLaunchAdoptObserveClose` (name_taken + label adoption) |  |
| SM-NODE-005 | M1-05 | implemented | `internal/app` `TestFleetLifecycle` (runs `unknown` after disconnect) |  |  |
| SM-NODE-006 | M1-05 | implemented | `fleet.applyReport` generation fencing; `internal/app` `TestFleetLifecycle` |  | Dedicated fencing test with a stale-generation report pending (M1-10). |
| SM-NODE-007 | M1-05 | implemented | `TestDisconnectEnforcement` (run killed after lease+grace; launch refused centrally) |  | Real-hc variant pending (M1-10). |
| SM-NODE-008 | M1-05 | implemented | lease-guard command + parented runs (`agent.ensureGuard`) | `test/real` `TestRealNodeEndToEnd` (real spaceman + spaceman-agent + hc 0.1.16, stub harness, macOS, 2026-11-02): SIGKILLed agent → guard ends run (`finished/killed`); `TestRealLeaseGuardParent` | Holds only while hc core/holder live (LIMITATIONS). |
| SM-NODE-009 | M1-05 | implemented | `internal/app` `TestFleetLifecycle` (agent + central restart: no new Start) | `test/real` `TestRealNodeEndToEnd` (real spaceman + spaceman-agent + hc 0.1.16, stub harness, macOS, 2026-11-02): agent restart adopts the same terminal |  |
| SM-SPACE-001 | | todo | | | |
| SM-SPACE-002 | M1-08 | partial | `internal/fleet` create/update/pause/resume/archive/restore/purge; `internal/app` `TestFleetLifecycle` |  | UI pending (M1-11). |
| SM-SPACE-003 | M1-08 | implemented | `internal/app` `TestFleetLifecycle` (pause interrupt stops runs; launches refused while paused) |  |  |
| SM-SPACE-004 | M1-08 | implemented | `fleet.PurgeSpace` deletes SpaceMan records only |  |  |
| SM-SPACE-005 | | todo | | | |
| SM-SPACE-006 | | todo | | | |
| SM-SPACE-007 | | todo | | | |
| SM-SPACE-008 | | todo | | | |
| SM-REPO-001 | | todo | | | |
| SM-REPO-002 | | todo | | | |
| SM-REPO-003 | | todo | | | |
| SM-REPO-004 | | todo | | | |
| SM-REPO-005 | | todo | | | |
| SM-REPO-006 | | todo | | | |
| SM-ACCT-001 | | todo | | | |
| SM-ACCT-002 | | todo | | | |
| SM-ACCT-003 | | todo | | | |
| SM-ACCT-004 | M1-07 | partial | launch secrets dispatch-time only (`TestLaunchPayloadRedacted`), shim, unrelated secrets dropped (`TestClaudeBuild`) | `test/real` `TestRealNodeEndToEnd` (real spaceman + spaceman-agent + hc 0.1.16, stub harness, macOS, 2026-11-02): secrets file unlinked by shim; run token absent from hc terminals.json | Accounts catalog M3-01. |
| SM-ACCT-005 | | todo | | | |
| SM-ACCT-006 | | todo | | | |
| SM-HARN-001 | M1-07 | partial | `internal/harness` `TestClaudeBuild`, `TestCodexBuild` |  | Real Claude/Codex runs need an account credential (see PLAN §4 note). |
| SM-HARN-002 | M1-07 | implemented | per-space/instance CLAUDE_CONFIG_DIR / CODEX_HOME (`TestClaudeBuild`, `TestCodexBuild`) |  |  |
| SM-HARN-003 | M1-07 | implemented | `Detect` capability lists; `proto.HarnessInfo` |  |  |
| SM-HARN-004 | | todo | | | |
| SM-RUN-001 | M1-06 | partial | `internal/runner/hc` `TestClientFramingAttachCreditResizeClose`, `TestClientCreditsAndInput`, `TestEventDecoding` | `hc_real_test.go` (`-tags real`, hc 0.1.16, macOS, 2026-11-02): launch, observe attach, close info, events, remove | Interrupt/send exercised with harnesses (M1-07). |
| SM-RUN-002 | M1-06 | implemented | status comes only from hc `status`/`turn` fields (`runner.Status`) | OSC 7501 status verified live for Claude Code (contracts/herdr-core.md) |  |
| SM-RUN-003 | | todo | | | |
| SM-INBOX-001 | | todo | | | |
| SM-INBOX-002 | | todo | | | |
| SM-INBOX-003 | | todo | | | |
| SM-INBOX-004 | | todo | | | |
| SM-EVT-001 | | todo | | | |
| SM-EVT-002 | | todo | | | |
| SM-EVT-003 | | todo | | | |
| SM-EVT-004 | | todo | | | |
| SM-EVT-005 | | todo | | | |
| SM-EVT-006 | | todo | | | |
| SM-EVT-007 | | todo | | | |
| SM-EVT-008 | | todo | | | |
| SM-EVT-009 | | todo | | | |
| SM-EVT-010 | | todo | | | |
| SM-EVT-011 | | todo | | | |
| SM-EVT-012 | | todo | | | |
| SM-EVT-013 | | todo | | | |
| SM-EVT-014 | | todo | | | |
| SM-EVT-015 | | todo | | | |
| SM-EVT-016 | | todo | | | |
| SM-EVT-017 | | todo | | | |
| SM-EVT-018 | | todo | | | |
| SM-EVT-019 | | todo | | | |
| SM-QUEUE-001 | | todo | | | |
| SM-QUEUE-002 | | todo | | | |
| SM-QUEUE-003 | | todo | | | |
| SM-QUEUE-004 | | todo | | | |
| SM-QUEUE-005 | | todo | | | |
| SM-QUEUE-006 | | todo | | | |
| SM-QUEUE-007 | | todo | | | |
| SM-QUEUE-008 | | todo | | | |
| SM-QUEUE-009 | | todo | | | |
| SM-QUEUE-010 | | todo | | | |
| SM-CTRL-001 | | todo | | | |
| SM-CTRL-002 | | todo | | | |
| SM-CTRL-003 | | todo | | | |
| SM-CTRL-004 | | todo | | | |
| SM-CTRL-005 | | todo | | | |
| SM-CTRL-006 | | todo | | | |
| SM-COMM-001 | | todo | | | |
| SM-COMM-002 | | todo | | | |
| SM-COMM-003 | | todo | | | |
| SM-COMM-004 | | todo | | | |
| SM-HUM-001 | | todo | | | |
| SM-HUM-002 | | todo | | | |
| SM-HUM-003 | | todo | | | |
| SM-HUM-004 | | todo | | | |
| SM-HUM-005 | | todo | | | |
| SM-HUM-006 | | todo | | | |
| SM-HUM-007 | | todo | | | |
| SM-HUM-008 | | todo | | | |
| SM-HUM-009 | | todo | | | |
| SM-HUM-010 | | todo | | | |
| SM-HUM-011 | | todo | | | |
| SM-HTYPE-001 | | todo | | | |
| SM-HTYPE-002 | | todo | | | |
| SM-HTYPE-003 | | todo | | | |
| SM-HTYPE-004 | | todo | | | |
| SM-HTYPE-005 | | todo | | | |
| SM-HTYPE-006 | | todo | | | |
| SM-APPR-001 | | todo | | | |
| SM-APPR-002 | | todo | | | |
| SM-APPR-003 | | todo | | | |
| SM-APPR-004 | | todo | | | |
| SM-APPR-005 | | todo | | | |
| SM-APPR-006 | | todo | | | |
| SM-HUI-001 | | todo | | | |
| SM-HUI-002 | | todo | | | |
| SM-HUI-003 | | todo | | | |
| SM-HUI-004 | | todo | | | |
| SM-HUI-005 | | todo | | | |
| SM-HUI-006 | | todo | | | |
| SM-HUI-007 | | todo | | | |
| SM-HUI-008 | | todo | | | |
| SM-MCP-001 | | todo | | | |
| SM-MCP-002 | | todo | | | |
| SM-MCP-003 | | todo | | | |
| SM-MCP-004 | | todo | | | |
| SM-MEM-001 | | todo | | | |
| SM-MEM-002 | | todo | | | |
| SM-MEM-003 | | todo | | | |
| SM-MEM-004 | | todo | | | |
| SM-MEM-005 | | todo | | | |
| SM-MEM-006 | | todo | | | |
| SM-KB-001 | | todo | | | |
| SM-KB-002 | | todo | | | |
| SM-KB-003 | | todo | | | |
| SM-KB-004 | | todo | | | |
| SM-KB-005 | | todo | | | |
| SM-KB-006 | | todo | | | |
| SM-KB-007 | | todo | | | |
| SM-DREAM-001 | | todo | | | |
| SM-DREAM-002 | | todo | | | |
| SM-DREAM-003 | | todo | | | |
| SM-DREAM-004 | | todo | | | |
| SM-PROMPT-001 | | todo | | | |
| SM-PROMPT-002 | | todo | | | |
| SM-PROMPT-003 | | todo | | | |
| SM-PROMPT-004 | | todo | | | |
| SM-XSPACE-001 | | todo | | | |
| SM-XSPACE-002 | | todo | | | |
| SM-XSPACE-003 | | todo | | | |
| SM-AUTH-001 | M1-04 | partial | `internal/auth` (Hallpass-derived suite: Argon2id, TOTP replay, recovery codes, sessions idle/absolute, CSRF, throttle/enumeration, invite/reset, break-glass, review SR-01..SR-27) |  | HTTP wiring (M1-03) and OIDC (M4-01) pending; SSO-grant session scoping K-11. |
| SM-AUTH-002 | | todo | | | |
| SM-AUTH-003 | M1-04 | partial | `internal/authz` `TestSpaceScopedRoles`, `TestDomainsAndRoles` |  | Casbin RBAC-with-domains; API enforcement wiring pending (M1-03). |
| SM-AUTH-004 | M1-04 | partial | `internal/authz` `TestSpaceScopedRoles` (terminal/takeover/secret/approval/node separation) |  |  |
| SM-AUTH-005 | M1-04 | implemented | `TestRootTokenLifecycle`, `TestAPIBootstrapLoginAndConventions` | `spaceman admin bootstrap` + `serve` smoke 2026-11-02: root token once, refused twice, first admin via API |  |
| SM-AUTH-006 | M1-04 | partial | `internal/auth` `TestAPITokens*`, `internal/authz` `TestTokensBoundedByCreator` |  |  |
| SM-AUTH-007 | M1-03 | partial | — |  | Watch stream re-validates every `auth.stream_revalidate` (default 30 s); test with spaces (M1-08). |
| SM-UI-001 | | todo | | | |
| SM-UI-002 | | todo | | | |
| SM-UI-003 | | todo | | | |
| SM-UI-004 | | todo | | | |
| SM-UI-005 | | todo | | | |
| SM-UI-006 | | todo | | | |
| SM-REC-001 | | todo | | | |
| SM-REC-002 | M1-09 | implemented | `internal/recording` `TestWriteReadResume`; agent observe-attach recorder | `test/real` `TestRealNodeEndToEnd` (real spaceman + spaceman-agent + hc 0.1.16, stub harness, macOS, 2026-11-02): recording contains harness output |  |
| SM-REC-003 | M1-09 | partial | `TestTornSegmentRecovers` |  | Upload/replay/download API pending. |
| SM-REC-004 | M1-09 | partial | `TestQuotaTruncates`; recorder independent of viewers |  |  |
| SM-REC-005 | | todo | | | |
| SM-LIMIT-001 | | todo | | | |
| SM-LIMIT-002 | | todo | | | |
| SM-LIMIT-003 | | todo | | | |
| SM-OBS-001 | M1-02 | partial | `internal/obs` `TestLogger*`, `internal/obs/redact` tests |  | Logger + redaction done; HTTP/node log wiring pending. |
| SM-OBS-002 | M1-03 | implemented | `internal/obs/health` + `internal/obs/metrics` suites | `spaceman serve` smoke: /readyz 200 and /metrics (24 families) on the separate management listener, 2026-11-02 (macOS) | Node health lands with M1-05. |
| SM-OBS-003 | M1-02 | partial | `internal/audit` `TestAppendAndVerify*`, `TestCorrelation*` |  | Hash chain + verify done; durable read cursors and retention-gap signals pending (API). |
| SM-OBS-004 | | todo | | | |
| SM-OBS-005 | | todo | | | |
| SM-STORE-001 | M1-02 | partial | `internal/store` tests (pools, pragmas, migrations, snapshots, network-FS refusal) |  | Central SQLite done; node journal pending (M1-05). |
| SM-STORE-002 | | todo | | | |
| SM-STORE-003 | | todo | | | |
| SM-STORE-004 | | todo | | | |
| SM-STORE-005 | | todo | | | |
| SM-STORE-006 | | todo | | | |
| SM-API-001 | M1-03 | partial | `TestEveryOperationHasARule` |  | One huma API; agent/session tools separate (M1-07). |
| SM-API-002 | M1-03 | partial | `internal/app` `TestAPIBootstrapLoginAndConventions` (problems, If-Match 428/409, Idempotency-Key replay/conflict), `TestEveryOperationHasARule` | manual smoke of `spaceman serve` 2026-11-02 (macOS) | Watch stream implemented; e2e watch test lands with spaces (M1-08). Long-running operations pending. |
| SM-API-003 | | todo | | | |
| SM-API-004 | | todo | | | |
| SM-API-005 | | todo | | | |
| SM-DOCS-001 | | todo | | | |
| SM-DOCS-002 | | todo | | | |
| SM-DOCS-003 | | todo | | | |
| SM-DOCS-004 | | todo | | | |
| SM-DOCS-005 | | todo | | | |
| SM-PKG-001 | | todo | | | |
| SM-PKG-002 | | todo | | | |
| SM-PKG-003 | | todo | | | |
| SM-SEC-001 | | todo | | | |
| SM-ATT-001 | | todo | | | |
| SM-ATT-002 | | todo | | | |
| SM-ATT-003 | | todo | | | |
| SM-ATT-004 | | todo | | | |
| SM-ATT-005 | | todo | | | |
| SM-ATT-006 | | todo | | | |
| SM-ATT-007 | | todo | | | |
| SM-VER-001 | | todo | | | |
| SM-VER-002 | | todo | | | |
| SM-VER-003 | | todo | | | |
