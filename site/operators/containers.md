# Containers and images

SpaceMan has two Dockerfiles: one for central and one for a node agent **base** image. Both images are Linux only
(amd64 and arm64).

Each release publishes them to Docker Hub, tagged with the version (`latest` only for a final release, not a release
candidate):

```sh
docker pull docker.io/aurimasniekis/spaceman:<version>         # central, web UI embedded
docker pull docker.io/aurimasniekis/spaceman-agent:<version>   # node agent base (not agent-ready by itself)
```

Each pushed image carries an SBOM and a build provenance attestation: `gh attestation verify
oci://docker.io/aurimasniekis/spaceman:<version> --repo aurimasniekis/go-spaceman`. You can also build them yourself
from a tagged source tree, as below.

## Image matrix

| Image | Built from | Git | herdr-core (`hc`) | Harnesses (Claude Code, Codex) | Agent-ready |
|---|---|---|---|---|---|
| `spaceman`: central, web UI embedded | `Dockerfile` | **yes** | no | no | does not run agents |
| `spaceman-agent`: node agent base | `Dockerfile.agent` | **yes** (plus `ssh`) | **no** | **no** | **no**. `run` refuses to start without `hc` |
| operator runtime image | `deploy/examples/agent-runtime/Dockerfile`, built by you | yes | only what you add | only what you add | only after you add and check them. Untested by SpaceMan |
| release archives (not an image) | `scripts/release-build.sh` | no | no | no | only on a host where you install them |

Each image states the same facts in its labels: `io.spaceman.contains.git`, `io.spaceman.contains.herdr-core`,
`io.spaceman.contains.harnesses` and `io.spaceman.agent-ready`. `scripts/container-smoke.sh` checks the labels against
the actual contents.

### Why neither image is agent-ready

A node needs herdr-core and at least one harness. SpaceMan bundles neither (ADR-0034):

- **herdr-core.** SpaceMan has inspected only an installed `hc` binary (`docs/contracts/herdr-core.md`), which records
  Apache-2.0 without a source checkout. It has not verified a release download location, a checksum source or an
  upstream licence file covering redistribution in an image.
- **Claude Code and Codex.** These are third-party products with their own licence terms. This project has not
  reviewed those terms for redistribution.

None of these programs has been run inside a container by this project.

### What each image contains

- **`spaceman`** is Debian 13 slim with `spaceman`, `git`, `ca-certificates` and `tini`. It runs as UID/GID 65532.
  Central runs `git` for role memory and for the memory bundles in backups, so a distroless image without git is not
  enough. As a result the image has a shell (`dash`), but no `curl`, compilers or package caches.
- **`spaceman-agent`** is Debian 13 slim (glibc) with `spaceman-agent`, `git`, `openssh-client`, `ca-certificates` and
  `tini`. It runs as UID/GID 65532 with `HOME=/var/lib/spaceman-agent`.

Base images are pinned by digest. Debian package versions are whatever trixie had at build time, so rebuilds are not
byte-identical. Generate an image SBOM yourself (for example `syft spaceman:local`) if you need the exact package list.

## Build and check

```sh
make image                # spaceman:local for the build machine's platform (docker buildx, --load, never pushed)
make image-agent          # spaceman-agent:local
make image-smoke          # runs scripts/container-smoke.sh against both
```

`make image-smoke` runs these checks:

- **Image metadata:** a non-root user, the exposed ports, the data volume and the labels.
- **Contents:** central has `git` and no `hc`, `claude`, `codex` or `spaceman-agent`; the agent image has `git` and
  `ssh` and no `hc`, `claude` or `codex`.
- **KEK:** creation with `keys init-kek` (`0400`, UID 65532), and refusal of a KEK with group permissions.
- **Serving:** `serve` with a production configuration, a read-only root filesystem and no capabilities. `/healthz` and
  `/readyz` answer on the management port, the embedded UI on the public port, and the process exits 0 on `SIGTERM`.
- **Backups:** `backup create` and `backup verify`.
- **Agent:** `spaceman-agent run` refuses to start before enrollment, and again without herdr-core.

## Central on Docker Compose

`deploy/compose.yaml` runs **central only**. It uses a read-only root filesystem, drops all capabilities, publishes
both ports on loopback and mounts the KEK as a Compose secret. Its header lists the steps:

1. Create the KEK as UID 65532 with mode `0400`, outside the data volume.
2. Copy `deploy/examples/spaceman.container.yaml` and replace the placeholders.
3. Run `SM_VERSION=local docker compose up -d`.
4. Stop central, run `docker compose run --rm spaceman admin bootstrap` once, then start central again.

The container configuration sets `http.management_listen: "0.0.0.0:9090"`, so the management port can be published. It
must stay private, because `/metrics` is unauthenticated. The image has no health check because it has no `curl`.
Probe `/readyz` from the host or from your orchestrator.

Put a TLS-terminating reverse proxy in front of port 8080 and list its address in `http.trusted_proxies`.
`public_url` must be the `https://` address that browsers and nodes use.

## Node agent in a container (operator-built)

To run a node in a container, build a runtime image on top of the base image with herdr-core and harnesses that you
obtained yourself:

```text
runtime/
  Dockerfile         copy of deploy/examples/agent-runtime/Dockerfile
  bin/hc             herdr-core for the image's platform
  bin/codex          optional single-binary harnesses
  bin/SHA256SUMS     checksums you verified against the vendor's
```

```sh
docker buildx build --load --build-arg BASE_IMAGE=spaceman-agent:local -t spaceman-agent-runtime:local runtime/
```

The build fails when `bin/hc` is missing, a file is not listed in `SHA256SUMS` or a checksum does not match. Harnesses
that need a language runtime need their own install steps, following the vendor's documented method, in the marked
section of that Dockerfile. Check each program's licence before you keep it in an image or a registry.

When you run it:

- Mount a volume at `/var/lib/spaceman-agent` and a tmpfs at `/run/spaceman-agent` (mode `0700`, owner 65532).
- Mount the configuration at `/etc/spaceman-agent/agent.yaml`.
- Enroll once with `spaceman-agent enroll --token …`, with the volume mounted.
- Agents in the container can use what the container can use. Treat the container as the node: one per trust domain,
  with only the credentials and network access its spaces need.

The combination of herdr-core, the harnesses and a container (pseudo terminals, process supervision, the harnesses' own
sandboxes) has not been tested by this project. The supported node setup is a Linux host with
[systemd](/operators/install#node-agent-with-systemd).
