SHELL := /usr/bin/env bash
GO ?= go
BIN := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS := -s -w -X github.com/aurimasniekis/go-spaceman/internal/buildinfo.Version=$(VERSION) -X github.com/aurimasniekis/go-spaceman/internal/buildinfo.Commit=$(COMMIT)
FUZZTIME ?= 30s
TAGS ?=

.PHONY: all build build-ui test race lint vet custom-vet fuzz-smoke coverage vuln tools-test generate check clean traceability

all: check build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -tags '$(TAGS)' -ldflags '$(LDFLAGS)' -o $(BIN)/spaceman ./cmd/spaceman
	CGO_ENABLED=0 $(GO) build -trimpath -tags '$(TAGS)' -ldflags '$(LDFLAGS)' -o $(BIN)/spaceman-agent ./cmd/spaceman-agent

# Production build with the embedded web UI (web/dist must be built first: make web-build).
build-ui: web-build
	$(MAKE) build TAGS=embedui

test:
	$(GO) test ./...

# The race detector slows SQLite transactions several-fold; as in CI, the 100 ms write-transaction guard is relaxed here
# and enforced by the non-race coverage run.
# internal/app's end-to-end tests take about 9 minutes under the race detector, close to go test's 10-minute default.
race:
	SM_TEST_TX_MAX=$${SM_TEST_TX_MAX:-500ms} $(GO) test -race -count=1 -timeout 30m ./...

$(BIN)/spaceman-vet: $(shell find tools -name '*.go' -not -path '*/testdata/*')
	cd tools && $(GO) build -o ../$(BIN)/spaceman-vet ./cmd/spaceman-vet

custom-vet: $(BIN)/spaceman-vet
	$(GO) vet -vettool=$(abspath $(BIN)/spaceman-vet) ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

tools-test:
	cd tools && $(GO) test ./...

# Runs every Fuzz* target for FUZZTIME (a smoke run; nightly CI uses FUZZTIME=10m).
fuzz-smoke:
	@set -e; for pkg in $$($(GO) list ./...); do \
	  for fz in $$($(GO) test -list '^Fuzz' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
	    echo "fuzz $$pkg $$fz"; $(GO) test -run '^$$' -fuzz "^$$fz$$" -fuzztime $(FUZZTIME) $$pkg; \
	  done; \
	done

coverage:
	./scripts/coverage-floor.sh

# Every GOAL.md requirement ID must have a row in docs/TRACEABILITY.md.
traceability:
	python3 -I scripts/check-traceability.py

vuln:
	govulncheck ./...

generate:
	$(GO) generate ./...

check: vet custom-vet lint race tools-test coverage traceability

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
	scripts/e2e.sh --reporter=list

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
	SM_E2E_BROWSERS=firefox,webkit scripts/e2e.sh --reporter=list --project=firefox
	SM_E2E_BROWSERS=firefox,webkit scripts/e2e.sh --reporter=list --project=webkit

.PHONY: test-fault test-real test-pulse
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

.PHONY: test-real-porch test-real-pulse test-real-s3 test-real-s3-external test-real-codex test-real-systemd
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
.PHONY: site site-dev site-check screenshots
site:
	cd site && pnpm install --frozen-lockfile && pnpm build

site-dev:
	cd site && pnpm dev

# Site build (dead links, screenshot references), Mermaid syntax in docs/ and site/, API, CLI and configuration
# reference drift, and an axe audit of every page in both color schemes.
site-check: cli-docs config-docs site
	cd site && pnpm check:mermaid
	cd web && SM_SITE_DIST=$(CURDIR)/site/.vitepress/dist pnpm exec playwright test e2e/site-a11y.spec.ts --reporter=list
	git diff --exit-code -- site/reference/api.md site/reference/cli site/reference/configuration.md

# Regenerates every screenshot from a seeded demo instance (docs/SCREENSHOTS.md).
screenshots:
	./scripts/screenshots.sh

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
