# syntax=docker/dockerfile:1.7
#
# SpaceMan central (`spaceman`) container image. Image matrix and operator guide: site/operators/containers.md.
#
#   web    : builds the web UI (web/dist) with the locked pnpm dependency tree.
#   build  : static Go binary (CGO_ENABLED=0, -trimpath, -tags embedui), cross-compiled on the build platform.
#   final  : Debian 13 slim with git, ca-certificates and tini; non-root (UID/GID 65532).
#
# Why not distroless: central runs `git` for role memory (internal/memory) and for the memory bundles in backups
# (internal/backup), and distroless static has no git. Without git, central starts but cannot store role memory and
# refuses backups and restores that contain memory (ADR-0034). The final image therefore has a shell (dash) and the
# Debian package database; it has no package manager cache, no compilers and no curl.
#
# Contents: spaceman (with the embedded UI), git, ca-certificates, tini. It does NOT contain herdr-core (hc), Claude
# Code, Codex or spaceman-agent: it is the control plane only and never runs agents.
#
# Every base image is pinned by digest. Update a digest with:
#   docker buildx imagetools inspect <image>:<tag>      # use the index "Digest:" line
# Debian package versions are not pinned (apt resolves the current trixie versions at build time); the image SBOM
# records what was installed.
#
# Runtime layout:
#   /usr/local/bin/spaceman          binary (root-owned, read-only)
#   /etc/spaceman/spaceman.yaml      configuration, mount read-only (SPACEMAN_CONFIG points here)
#   /var/lib/spaceman                data volume (0700, 65532:65532)
#   KEK                              mount separately (e.g. /run/secrets/spaceman/kek, 0400, owned by 65532);
#                                    never inside the data volume
#
# Build:  docker buildx build --load --build-arg VERSION=v1.2.3 --build-arg COMMIT=$(git rev-parse HEAD) \
#           --build-arg BUILD_DATE=2026-01-02T03:04:05Z -t spaceman:local .

ARG NODE_IMAGE=docker.io/library/node:24.22.0-bookworm-slim@sha256:91acba91b842bec0bf7ea6f5e09c644c7ccad72c4996ed9616d861d1c7cd60d4
ARG GO_IMAGE=docker.io/library/golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9
ARG RUNTIME_IMAGE=docker.io/library/debian:trixie-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4

# ---------------------------------------------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS web
ENV CI=true \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
    PNPM_HOME=/pnpm \
    PATH=/pnpm:$PATH
WORKDIR /src/web
# pnpm version comes from "packageManager" in web/package.json (corepack verifies the download signature).
COPY web/package.json web/pnpm-lock.yaml ./
RUN corepack enable && corepack install
RUN --mount=type=cache,id=spaceman-pnpm-store,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && \
    pnpm install --frozen-lockfile
COPY web/ ./
# `pnpm build` = typecheck + vite build + dist check (web/scripts/check-dist.mjs).
RUN pnpm build

# ---------------------------------------------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=
ARG BUILD_DATE=
ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify
COPY cmd/ cmd/
COPY internal/ internal/
COPY api/ api/
# The webui Go package (embed and SPA handler) plus the UI bundle it embeds with -tags embedui.
COPY web/*.go web/
COPY --from=web /src/web/dist/ web/dist/
# -buildvcs=false: the build context deliberately excludes .git; version metadata comes from build args instead.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags embedui -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid= \
        -X github.com/aurimasniekis/go-spaceman/internal/buildinfo.Version=${VERSION} \
        -X github.com/aurimasniekis/go-spaceman/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/aurimasniekis/go-spaceman/internal/buildinfo.Date=${BUILD_DATE}" \
      -o /out/spaceman ./cmd/spaceman

# ---------------------------------------------------------------------------------------------------------------------
FROM ${RUNTIME_IMAGE}
ARG VERSION=dev
ARG COMMIT=
# git: role memory and memory bundles in backups. ca-certificates: TLS to S3, Pulse and identity providers.
# tini: PID 1 that reaps orphaned git helper processes and forwards SIGTERM to spaceman.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates git tini; \
    rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*; \
    groupadd --system --gid 65532 spaceman; \
    useradd --system --uid 65532 --gid 65532 --home-dir /var/lib/spaceman --no-create-home \
      --shell /usr/sbin/nologin spaceman; \
    install -d -m 0700 -o 65532 -g 65532 /var/lib/spaceman; \
    install -d -m 0755 -o 0 -g 0 /etc/spaceman
LABEL org.opencontainers.image.title="spaceman" \
      org.opencontainers.image.description="SpaceMan central control plane (no herdr-core, no harnesses)" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.base.name="docker.io/library/debian:trixie-slim" \
      org.opencontainers.image.licenses="Apache-2.0" \
      io.spaceman.contains.git="true" \
      io.spaceman.contains.herdr-core="false" \
      io.spaceman.contains.harnesses="false" \
      io.spaceman.agent-ready="false"
COPY --chown=0:0 --chmod=0444 LICENSE NOTICE docs/THIRD_PARTY.md /usr/share/doc/spaceman/
COPY --from=build --chown=0:0 --chmod=0555 /out/spaceman /usr/local/bin/spaceman
ENV SPACEMAN_CONFIG=/etc/spaceman/spaceman.yaml
USER 65532:65532
WORKDIR /var/lib/spaceman
VOLUME ["/var/lib/spaceman"]
# http.listen and http.management_listen. The management listener (/healthz, /readyz, /metrics) defaults to
# 127.0.0.1:9090; the container config sets 0.0.0.0:9090 so it can be probed, and Compose publishes it on loopback only.
EXPOSE 8080/tcp 9090/tcp
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/spaceman"]
CMD ["serve"]
