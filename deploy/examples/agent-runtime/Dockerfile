# syntax=docker/dockerfile:1.7
#
# OPERATOR-BUILT agent runtime image: extends the spaceman-agent base image (Dockerfile.agent, which is NOT agent-ready)
# with herdr-core and harness CLIs that YOU obtained. SpaceMan ships no herdr-core, Claude Code or Codex binaries and
# names no download source for them (ADR-0034, site/operators/containers.md). Before building, check that the licence
# terms of every program you add allow your use, including keeping it in a private image registry.
#
# Build context: a directory you prepare, for example
#
#   runtime/
#     Dockerfile            this file
#     bin/hc                herdr-core client/server binary for the image's platform (linux/amd64 or linux/arm64)
#     bin/codex             optional: Codex CLI, if it is a single binary in your distribution
#     bin/claude            optional: Claude Code, if it is a single binary in your distribution
#     bin/SHA256SUMS        `sha256sum hc codex claude > SHA256SUMS`, from checksums you verified against the vendor
#
#   docker buildx build --load --build-arg BASE_IMAGE=spaceman-agent:local -t spaceman-agent-runtime:local runtime/
#
# Every file in bin/ must be listed in SHA256SUMS, and the build fails when a checksum does not match or `hc` is
# missing. Harnesses that are not single binaries (for example ones that need a language runtime) need their own
# install steps here, following the vendor's documented method; add them in the marked section and pin versions.
# The base image is Debian 13 (glibc). Whether a given hc or harness build runs on it is for you to check; SpaceMan
# has not run herdr-core or any harness inside a container (docs/LIMITATIONS.md).
#
# After building, confirm the agent sees its runtime:
#   docker run --rm --entrypoint sh spaceman-agent-runtime:local -c 'hc --version; git --version; command -v claude codex'

ARG BASE_IMAGE=spaceman-agent:local
FROM ${BASE_IMAGE}

USER 0:0
COPY --chown=0:0 --chmod=0555 bin/ /opt/spaceman-runtime/bin/
RUN set -eu; \
    cd /opt/spaceman-runtime/bin; \
    chmod 0444 SHA256SUMS; \
    test -x hc || { echo "bin/hc is required" >&2; exit 1; }; \
    for f in *; do \
      [ "$f" = SHA256SUMS ] && continue; \
      grep -q "  $f\$" SHA256SUMS || { echo "bin/$f is not listed in SHA256SUMS" >&2; exit 1; }; \
    done; \
    sha256sum -c SHA256SUMS

# ---- Operator section: harness installs that are not single binaries (vendor-documented method, pinned version). ----
# RUN ...
# ---------------------------------------------------------------------------------------------------------------------

ENV PATH=/opt/spaceman-runtime/bin:$PATH
USER 65532:65532
LABEL io.spaceman.contains.herdr-core="operator-supplied" \
      io.spaceman.contains.harnesses="operator-supplied" \
      io.spaceman.agent-ready="operator-assembled"
