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

race:
	$(GO) test -race -count=1 ./...

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
	cd web && pnpm exec openapi-typescript ../api/openapi.json -o src/api/schema.gen.ts

# Fails when generated code drifts from the API definitions.
api-check: api
	git diff --exit-code -- api/openapi.json web/src/api/schema.gen.ts

.PHONY: web-install web-build
web-install:
	cd web && pnpm install --frozen-lockfile

web-build:
	cd web && pnpm build

.PHONY: test-fault test-real test-pulse
# Fault-injection suite (crashes, disk full, interrupted recordings, disconnects); Go only.
test-fault:
	$(GO) test -tags fault -count=1 -timeout 20m ./test/fault/

# Opt-in real-dependency suite: an isolated herdr-core core, and (with SPACEMAN_REAL=1) budget-bounded Claude Code and
# Codex runs using the local logins. Never touches the default hc core. See docs/TEST_STRATEGY.md.
test-real:
	$(GO) test -tags real -count=1 -timeout 30m -v ./test/real/

# Pulse/NATS integration against an embedded nats-server and the real go-pulse SDK.
test-pulse:
	$(GO) test -tags pulse -count=1 -timeout 10m ./test/pulse/

# ---- Docs site and screenshots ----
.PHONY: site site-dev site-check screenshots
site:
	cd site && pnpm install --frozen-lockfile && pnpm build

site-dev:
	cd site && pnpm dev

site-check:
	cd site && pnpm check

# Regenerates every screenshot from a seeded demo instance (docs/SCREENSHOTS.md).
screenshots:
	./scripts/screenshots.sh

# ---- Packaging and release (docs/operator/install.md) ----
IMAGE ?= spaceman:local
AGENT_IMAGE ?= spaceman-agent:local

.PHONY: release-snapshot image image-agent image-smoke

# Static release archives + SHA256SUMS for linux/amd64, linux/arm64, darwin/arm64 in dist/ (scripts/release-build.sh).
release-snapshot:
	VERSION=$(VERSION) ./scripts/release-build.sh -o dist

# Local container images for the build platform (never pushed).
image:
	docker buildx build --load -t $(IMAGE) --build-arg VERSION=$(VERSION) -f Dockerfile .

image-agent:
	docker buildx build --load -t $(AGENT_IMAGE) --build-arg VERSION=$(VERSION) -f Dockerfile.agent .

image-smoke:
	./scripts/container-smoke.sh $(IMAGE)
# ---- end packaging ----
