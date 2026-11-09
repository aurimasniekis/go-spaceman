SHELL := /usr/bin/env bash
GO ?= go
BIN := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS := -s -w -X github.com/aurimasniekis/go-spaceman/internal/buildinfo.Version=$(VERSION) -X github.com/aurimasniekis/go-spaceman/internal/buildinfo.Commit=$(COMMIT)
FUZZTIME ?= 30s
TAGS ?=

.PHONY: all build build-ui test race lint vet custom-vet fuzz-smoke coverage vuln tools-test generate check check-fast clean traceability

all: check build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -tags '$(TAGS)' -ldflags '$(LDFLAGS)' -o $(BIN)/spaceman ./cmd/spaceman
	CGO_ENABLED=0 $(GO) build -trimpath -tags '$(TAGS)' -ldflags '$(LDFLAGS)' -o $(BIN)/spaceman-agent ./cmd/spaceman-agent

# Production build with the embedded web UI (web/dist must be built first: make web-build).
build-ui: web-build
	$(MAKE) build TAGS=embedui

test:
	$(GO) test ./...

# The heavy gates (race, coverage, site-a11y, e2e, screenshots) take a node-wide slot first (scripts/gate-slot.sh:
# SM_GATE_SLOTS slots, default 2 on 16 or more CPUs and otherwise 1, in SM_GATE_LOCK_DIR), so several agents' runs on one
# machine take turns instead of contending. SM_GATE_SLOTS=0 disables it; it is off when CI=true.
GATE := $(CURDIR)/scripts/gate-slot.sh

# The race detector slows SQLite transactions several-fold; as in CI, the 100 ms write-transaction guard is relaxed here
# (SM_TEST_TX_MAX, default 500ms) and enforced by the non-race coverage run.
# scripts/race.sh runs internal/app, the critical path (its end-to-end tests add up to about an hour of test time under
# the race detector), as its own go test invocation started first, alongside one for every other package; with 16 or
# more CPUs its serial tests run in a second process next to its parallel ones (RACE_APP_SPLIT).
# RACE_PARALLEL caps t.Parallel tests per package for the other packages (go test's default is GOMAXPROCS, multiplied by
# the packages running at once), so a many-core machine doesn't starve leases and write transactions; 4 matches CI's
# runners. RACE_APP_PARALLEL is internal/app's own cap: empty means min(16, nproc/2), at least 4; CI sets 4.
RACE_PARALLEL ?= 4
RACE_APP_PARALLEL ?=
# Locally the race run uses go test's cache, so rerunning `make check` after a change (or a merge) reruns only the
# packages whose code, dependencies or test inputs changed. CI sets RACE_COUNT=1 so that every test runs there: its
# build cache is restored between runs, and a cached pass must not stand in for a run on the commit under test.
RACE_COUNT ?=
race:
	GO=$(GO) RACE_PARALLEL=$(RACE_PARALLEL) RACE_APP_PARALLEL=$(RACE_APP_PARALLEL) RACE_COUNT=$(RACE_COUNT) \
	  $(GATE) race ./scripts/race.sh

$(BIN)/spaceman-vet: $(shell find tools -name '*.go' -not -path '*/testdata/*')
	cd tools && $(GO) build -o ../$(BIN)/spaceman-vet ./cmd/spaceman-vet

custom-vet: $(BIN)/spaceman-vet
	$(GO) vet -vettool=$(abspath $(BIN)/spaceman-vet) ./...

vet:
	$(GO) vet ./...

# golangci-lint takes a per-user lock file in the temporary directory; --allow-serial-runners makes a second run on the
# machine (another worktree's make check) wait for it instead of failing with "parallel golangci-lint is running".
lint:
	golangci-lint run --allow-serial-runners ./...

tools-test:
	cd tools && $(GO) test ./...

# Runs every Fuzz* target for FUZZTIME (a smoke run; nightly CI uses FUZZTIME=10m). Packages whose test files declare no
# Fuzz function are skipped before `go test -list`, which would otherwise build every test binary. The package list is
# captured first and `go test -list` must succeed, so a failing `go list` or a test that doesn't compile fails the target
# instead of silently fuzzing less; so does finding no target at all.
fuzz-smoke:
	@set -euo pipefail; pkgs="$$($(GO) list -f '{{.ImportPath}} {{.Dir}}' ./...)"; n=0; \
	while read -r pkg dir; do \
	  grep -qsE '^func Fuzz' "$$dir"/*_test.go || continue; \
	  list="$$($(GO) test -list '^Fuzz' "$$pkg")"; \
	  for fz in $$(grep '^Fuzz' <<<"$$list" || true); do \
	    echo "fuzz $$pkg $$fz"; $(GO) test -run '^$$' -fuzz "^$$fz$$" -fuzztime $(FUZZTIME) "$$pkg"; n=$$((n+1)); \
	  done; \
	done <<<"$$pkgs"; \
	echo "fuzz-smoke: $$n targets"; test "$$n" -gt 0

coverage:
	$(GATE) coverage ./scripts/coverage-floor.sh

# Every GOAL.md requirement ID must have a row in docs/TRACEABILITY.md.
traceability:
	python3 -I scripts/check-traceability.py

vuln:
	govulncheck ./...

generate:
	$(GO) generate ./...

# The pre-push gate (AGENTS.md "Gates"). Ordered to fail fast: the static checks, then the non-race coverage floor
# (about 20 s, and it already runs most security-critical tests), then the race run, which takes most of the time. The
# coverage floor and the race run share one heavy-gate slot, taken once for both.
check: vet custom-vet lint traceability tools-test
	$(GATE) check $(MAKE) --no-print-directory coverage race

# The inner-loop gate: the static checks and every test without the race detector and with go test's cache, so only
# packages whose code, dependencies or test inputs changed run again (seconds when nothing changed). The sqlc and
# OpenAPI drift checks run only when their inputs changed since the merge base (scripts/changed.sh). It does not
# replace `make check`, which runs once before pushing.
check-fast: vet custom-vet lint traceability tools-test
	SM_TEST_TX_MAX=$${SM_TEST_TX_MAX:-500ms} $(GO) test -timeout 15m ./...
	@rc=0; ./scripts/changed.sh '^(internal/store/(migrations|queries)/|sqlc\.yaml$$)' || rc=$$?; \
	if [ $$rc -ne 1 ]; then $(MAKE) sqlc-check; \
	else echo "check-fast: sqlc-check skipped (no change in internal/store/migrations, internal/store/queries or sqlc.yaml)"; fi
	@rc=0; ./scripts/changed.sh '^(internal/api/|api/openapi\.json$$|web/src/api/)' || rc=$$?; \
	if [ $$rc -ne 1 ]; then $(MAKE) api-check; \
	else echo "check-fast: api-check skipped (no change in internal/api, api/openapi.json or web/src/api)"; fi

clean:
	rm -rf $(BIN)

.PHONY: sqlc sqlc-check
bin/sqlc:
	./scripts/install-sqlc.sh

# Regenerate typed queries from internal/store/{migrations,queries}.
sqlc: bin/sqlc
	bin/sqlc generate

# CI drift check: generated code must match the committed SQL.
sqlc-check: bin/sqlc
	bin/sqlc diff

.PHONY: api api-check
# Regenerate the OpenAPI document from the Go API definitions and the UI's TypeScript types from it.
api:
	$(GO) run ./cmd/spaceman openapi > api/openapi.json
	cd web && pnpm run gen:api

# Fails when generated code drifts from the API definitions.
api-check: api
	git diff --exit-code -- api/openapi.json web/src/api/schema.gen.ts

.PHONY: config-docs
# Regenerates the configuration reference (site/reference/configuration.md) from internal/config's types, doc comments
# and defaults. Drift is caught by internal/config TestConfigReferenceUpToDate (make check).
config-docs:
	$(GO) test ./internal/config -run '^TestConfigReferenceUpToDate$$' -count=1 -update

.PHONY: cli-docs
# Regenerates the command-line reference (site/reference/cli) from the command definitions. Drift is also caught by
# cmd/spaceman TestCLIReferenceUpToDate (make check), which notices added and removed pages too.
cli-docs:
	$(GO) run ./cmd/spaceman docs cli --out site/reference/cli

.PHONY: web-install web-build
web-install:
	cd web && pnpm install --frozen-lockfile

web-build:
	cd web && pnpm build

.PHONY: web-check e2e
# Typecheck, unit tests and the production build of the UI.
web-check:
	cd web && pnpm typecheck && pnpm test && pnpm build

# Browser journey against a fresh local stack with a real agent and an isolated herdr-core core (needs hc).
e2e:
	$(GATE) e2e scripts/e2e.sh --reporter=list

.PHONY: demo
# The scripted end-to-end demonstration of the brief's §41 (SM-VER-003, docs/DEMO.md): a real central with the UI, two
# real agents with their own herdr-core cores, devpulse and devmcp, driven through the browser, the API and the CLI.
# Stub harness by default; SPACEMAN_DEMO_REAL=1 with SPACEMAN_DEMO_CLAUDE_TOKEN makes the Claude Code worker real.
demo:
	scripts/demo.sh

.PHONY: e2e-browsers
# The same journey in Firefox and WebKit (Playwright's builds: cd web && pnpm exec playwright install firefox webkit).
# The journey is a first run (setup with the root token), so each engine gets its own fresh stack.
e2e-browsers:
	SM_E2E_BROWSERS=firefox,webkit $(GATE) e2e scripts/e2e.sh --reporter=list --project=firefox
	SM_E2E_BROWSERS=firefox,webkit $(GATE) e2e scripts/e2e.sh --reporter=list --project=webkit

.PHONY: test-fault test-real test-pulse test-login-timing
# The wall-clock half of the login enumeration check (Hallpass HP-AUTH-005): failed logins for a known and an unknown
# account must take about as long. It needs a quiet machine and no race detector or coverage instrumentation, so it is
# not part of `make test`, `race`, `coverage` or `check`; CI runs it as its own job (ADR-0071). The deterministic check
# (one verification against a same-parameter dummy hash) runs in every suite.
test-login-timing:
	SPACEMAN_LOGIN_TIMING=1 $(GO) test -count=1 -run '^TestLoginTimingWallClock$$' -v ./internal/auth/

# Fault-injection suite (crashes, disk full, interrupted recordings, disconnects); Go only.
test-fault:
	$(GO) test -tags fault -count=1 -timeout 20m ./test/fault/

# Opt-in real-dependency suite: an isolated herdr-core core, and (with SPACEMAN_REAL=1) budget-bounded Claude Code and
# Codex runs using the local logins. Never touches the default hc core. See docs/TEST_STRATEGY.md.
test-real:
	env -u HC_TERMINAL_ID -u HC_TERMINAL_SOCKET -u HC_SCOPE $(GO) test -tags real -count=1 -timeout 30m ./...

# Sibling checkouts (../porch, ../pulse, ../pulse-github), found from the main checkout so worktrees work too. They are
# only read and built into temp dirs, never modified.
SIBLINGS ?= $(abspath $(shell git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)/../..)
PORCH_SRC ?= $(SIBLINGS)/porch
PULSE_SRC ?= $(SIBLINGS)/pulse
PULSE_GITHUB_SRC ?= $(SIBLINGS)/pulse-github

.PHONY: test-real-porch test-real-pulse test-real-s3 test-real-s3-external test-real-codex test-real-systemd test-harbor-pilot test-real-harbor-pilot test-real-claude-cli
# The installed Claude Code CLI against the permission rules and hook SpaceMan writes (ADR-0068): print mode, a local
# fake API that scripts the tool calls, a throwaway HOME and space folder. No model, no credentials, no cost.
test-real-claude-cli:
	SPACEMAN_REAL_CLAUDE_CLI=1 $(GO) test -tags real -count=1 -timeout 5m -run '^TestRealClaudeCLI' -v ./test/real/

# Real Porch (built from PORCH_SRC into a temp dir, with Porch's own fake Google upstream): SpaceMan OIDC sign-in.
test-real-porch:
	SPACEMAN_REAL_PORCH=1 SPACEMAN_PORCH_SRC=$(PORCH_SRC) $(GO) test -tags real -count=1 -timeout 15m -run '^TestRealPorch' -v ./test/real/

# Real pulsed + nats-server (built from PULSE_SRC) and go-pulse-github's publisher and fixtures (PULSE_GITHUB_SRC).
test-real-pulse:
	SPACEMAN_REAL_PULSE=1 SPACEMAN_PULSE_SRC=$(PULSE_SRC) SPACEMAN_PULSE_GITHUB_SRC=$(PULSE_GITHUB_SRC) \
	  $(GO) test -tags real -count=1 -timeout 15m -run '^TestRealPulse' -v ./test/real/

# Real S3: a throwaway MinIO container (pinned digest, random port, removed afterwards).
test-real-s3:
	SPACEMAN_REAL_S3=1 $(GO) test -tags real -count=1 -timeout 15m -run '^TestRealS3' -v ./test/real/

# Real S3 service of your own (AWS S3, Cloudflare R2, self-hosted MinIO): SPACEMAN_REAL_S3_ENDPOINT, _BUCKET,
# _ACCESS_KEY_ID, _SECRET_ACCESS_KEY (optional _REGION, _PATH_STYLE=1, _CA_FILE, _ALLOW_PRIVATE_NETWORK=1). Works under a
# random prefix and deletes only what it wrote.
test-real-s3-external:
	SPACEMAN_REAL_S3_EXTERNAL=1 $(GO) test -tags real -count=1 -timeout 15m -run '^TestRealS3External$$' -v ./test/real/

# A real, billed Codex run through SpaceMan: OPENAI_API_KEY, or SPACEMAN_REAL_CODEX_AUTH_FILE (the auth.json of a
# dedicated ChatGPT login); optional SPACEMAN_REAL_CODEX_MODEL.
test-real-codex:
	env -u HC_TERMINAL_ID -u HC_TERMINAL_SOCKET -u HC_SCOPE SPACEMAN_REAL_CODEX=1 $(GO) test -tags real -count=1 -timeout 15m \
	  -run '^TestRealCodexRun$$' -v ./test/real/

# Real Harbor Pilot gateway (docs/contracts/harbor-pilot.md): `harbor-pilot serve --demo` v0.1.3 on loopback in a temp
# data dir, with its own binding, budget, service principal and parent token (internal/harborpilot/hptest). Uses
# HARBOR_PILOT_BIN, or installs the pinned release with `go install`. Synthetic upstreams; no provider credentials.
# internal/events relays the gateway's events through its own Pulse destination into an in-process Pulse-shaped server;
# internal/app covers the run-token lifecycle (mint at dispatch, revoke at the end) over a scripted node.
test-harbor-pilot:
	SPACEMAN_HARBOR_PILOT=1 $(GO) test -count=1 -timeout 15m -run '^TestRealHarborPilot' -v ./internal/harborpilot/... ./internal/events/ ./internal/app/

# Real Claude Code and Codex runs through SpaceMan (real central, agent, herdr-core and launcher shim) and the real
# gateway above. No provider credentials and no cost (synthetic upstreams). Codex from SPACEMAN_REAL_CODEX_BIN or PATH
# (npm install --prefix <dir> @openai/codex@0.166.0 works without root).
test-real-harbor-pilot:
	env -u HC_TERMINAL_ID -u HC_TERMINAL_SOCKET -u HC_SCOPE SPACEMAN_REAL=1 SPACEMAN_HARBOR_PILOT=1 $(GO) test -tags real -count=1 -timeout 15m \
	  -run '^TestRealHarborPilotGatewayRuns$$' -v ./test/real/

# Linux/systemd: a Debian 13 container with systemd as PID 1 running the linux release binary and the shipped units.
test-real-systemd:
	./scripts/systemd-smoke.sh

# Pulse/NATS integration against an embedded nats-server (internal/events/pulsetest, operator/JWT mode, JetStream) and
# the real go-pulse SDK: ingestion (durable consumers, dedup, fan-out, quarantine, cursors, replay, loop guard, paused-space
# policies, automation depth between two centrals), outbox
# publication, and the central end-to-end paths. These tests also run in `make test`; this target runs just them,
# uncached. Simulated evidence: a real pulsed deployment is not involved (docs/TEST_STRATEGY.md).
test-pulse:
	$(GO) test -count=1 -timeout 10m ./internal/events/... ./internal/outbox/
	$(GO) test -count=1 -timeout 10m -run '^(TestEventsOverHTTP|TestCoreSubscriptionEndToEnd|TestOutboundEventsOverHTTP|TestRunLifecycleEvents|TestBackupRestoreEndToEnd|TestPausedSpaceEventPolicies|TestEventLoopBetweenInstallationsIsBounded)$$' ./internal/app/

# ---- Docs site and screenshots ----
.PHONY: site site-dev site-check site-verify site-a11y screenshots

# Extra Chromium launch arguments for the browser targets (e2e, screenshots, the site's Mermaid check and axe audit),
# space-separated: taken from the environment (a node whose sandbox stops Chromium sets SM_CHROMIUM_ARGS, for example
# '--no-sandbox --disable-gpu --disable-dev-shm-usage --no-zygote'); a machine that doesn't set it, CI included, passes
# none. The site targets add --single-process to a non-empty value (the Mermaid check and the audit crash without it on
# such hosts; the app's e2e journey doesn't work with it, so only here).
SM_CHROMIUM_ARGS ?=
export SM_CHROMIUM_ARGS
site site-verify site-check site-a11y: SM_CHROMIUM_ARGS := $(strip $(SM_CHROMIUM_ARGS) $(if $(and $(strip $(SM_CHROMIUM_ARGS)),$(if $(filter --single-process,$(SM_CHROMIUM_ARGS)),,x)),--single-process))
site:
	cd site && pnpm install --frozen-lockfile && pnpm build

site-dev:
	cd site && pnpm dev

# Site build (dead links, screenshot references), Mermaid syntax in docs/ and site/, API, CLI and configuration
# reference drift, and an axe audit of every page in both color schemes. The audit is skipped when nothing the site is
# built from changed against the main branch (scripts/site-affected.sh, the same rule CI uses; ADR-0062);
# SM_SITE_A11Y=1 runs it regardless (the release gate does).
site-check: site-verify
	@if [ "$${SM_SITE_A11Y:-}" = 1 ] || ./scripts/site-affected.sh; then $(MAKE) site-a11y; fi

# Everything in site-check except the axe audit (CI runs the audit on pull requests only when the site can change;
# ADR-0062).
site-verify: cli-docs config-docs site
	cd site && pnpm check:mermaid
	git diff --exit-code -- site/reference/api.md site/reference/cli site/reference/configuration.md

# The axe audit (WCAG 2.1 A/AA) of every page of the built site (make site) in both color schemes. The pages are split
# into SM_A11Y_SHARDS parallel tests (both schemes each), run by as many Playwright workers (one Chromium each); the
# default is a quarter of the CPUs, between 1 and 8.
SM_A11Y_SHARDS ?= $(shell n=$$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4); s=$$((n / 4)); [ $$s -gt 8 ] && s=8; [ $$s -lt 1 ] && s=1; echo $$s)
site-a11y:
	cd web && SM_SITE_DIST=$(CURDIR)/site/.vitepress/dist SM_A11Y_SHARDS=$(SM_A11Y_SHARDS) $(GATE) site-a11y pnpm exec playwright test e2e/site-a11y.spec.ts --workers=$(SM_A11Y_SHARDS) --reporter=list

# Regenerates every screenshot from a seeded demo instance (docs/SCREENSHOTS.md).
screenshots:
	$(GATE) screenshots ./scripts/screenshots.sh

# ---- Packaging and release (site/operators/install.md, site/operators/containers.md, ADR-0034) ----
IMAGE ?= spaceman:local
AGENT_IMAGE ?= spaceman-agent:local

.PHONY: release-snapshot image image-agent image-smoke

# Static release archives (linux/amd64, linux/arm64), SPDX SBOMs when syft is installed, and SHA256SUMS in dist/
# (scripts/release-build.sh; needs web/dist: make web-build). darwin builds only when named, labelled unsupported.
release-snapshot:
	VERSION=$(VERSION) ./scripts/release-build.sh -o dist

# Local container images for the build platform (loaded into the local daemon, never pushed).
image:
	docker buildx build --load -t $(IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -f Dockerfile .

image-agent:
	docker buildx build --load -t $(AGENT_IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -f Dockerfile.agent .

# Runs both images (central serve, health, UI, backup; agent base image refuses to run without herdr-core).
image-smoke:
	./scripts/container-smoke.sh $(IMAGE) $(AGENT_IMAGE)
# ---- end packaging ----
