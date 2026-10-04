# ──────────────────────────────────────────────────────
# Base images are pinned by digest, declared as global ARGs so a rebuild of this
# file reproduces the same toolchain months later. The floating
# `golang:1.26-bookworm` / `node:24-bookworm-slim` tags move under us, and a
# silent toolchain change is what turns into an unexplainable build diff.
#
# Deliberately global (before the first FROM) and not stage-scoped: podman
# resolves a stage-scoped ARG in a later FROM to nothing and fails the build
# with a misleading "no FROM statement found".
#
# To bump either, pass a new digest rather than editing the build steps:
#   podman build --build-arg GO_IMAGE=golang@sha256:<new> \
#                --build-arg NODE_IMAGE=node@sha256:<new> -f docker/pi/Dockerfile .
#
# golang is build-time only (the Go binaries are copied out), so its digest
# affects reproducibility, not the shipped runtime surface.
ARG GO_IMAGE=golang@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d
ARG NODE_IMAGE=node@sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6

# ──────────────────────────────────────────────────────
# Pi runtime — the agent host that replaces the Hermes gateway.
#
# Two stages:
#   gobuild — builds the in-repo MCP servers (money, cookbook, health-check,
#             task-manager, project-manager) exactly as before.
#   runtime — Node for Pi (@earendil-works/pi-coding-agent) plus those
#             binaries on PATH.
#
# Deliberately NOT based on the Hermes image: that image carries the agent
# runtime we are removing, and inheriting it would leave the dependency alive
# in a base layer we would never look at again. Plain Node + our Go binaries.
#
# This image has no HTTP server. Pi is a stdio process (`--mode rpc`); the
# OpenAI-compatible surface the Android app speaks is a separate gateway binary
# added on top.
#
# Runs as root, matching the gateway it replaces. Deliberate: the bind-mounted
# workspace is owned by host uid 10000, so a non-root process cannot write the
# agent's session trees and repo checkouts there. See docker/pi/entrypoint.sh.
#
# Binary set: 5 of the 7 the old image shipped. The two omissions are on
# purpose and neither is agent-reachable:
#   retention — a one-shot lifecycle job. It runs as its own compose service
#               (`podman-compose run --rm retention`) on its own image
#               (docker/retention/Dockerfile), so it never needs to exist inside
#               the agent container — and, more importantly, so deleting the old
#               Hermes-derived image it used to build from was possible.
#   mongo     — an operator CLI for ad-hoc queries. No skill invokes it; agents
#               reach MongoDB through the MCP servers, not the shell.
#
# git safe.directory is handled in entrypoint.sh, not here, because it must
# discover the bind-mounted repos at runtime.
# ──────────────────────────────────────────────────────
FROM ${GO_IMAGE} AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
# Static binaries: the runtime stage is a slim libc, so link without it and the
# MCP servers cannot break on a base-image libc change.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/miser-money ./cmd/miser-money \
 && go build -trimpath -ldflags="-s -w" -o /out/cookbook ./cmd/cookbook \
 && go build -trimpath -ldflags="-s -w" -o /out/health-check ./cmd/health-check \
 && go build -trimpath -ldflags="-s -w" -o /out/task-manager ./cmd/task-manager \
 && go build -trimpath -ldflags="-s -w" -o /out/project-manager ./cmd/project-manager \
 && go build -trimpath -ldflags="-s -w" -o /out/pi-gateway ./cmd/pi-gateway

FROM ${NODE_IMAGE} AS runtime

# git: Pi's branchable session trees and any repo work the agent does.
# ca-certificates: the OpenCode Go endpoint, and MCP servers fetching over HTTPS.
# findutils: entrypoint.sh discovers bind-mounted repos with
#   `find ... -printf '%h\0'`, and -printf is GNU-only. It happens to be present
#   transitively today (npm pulls it in) but that is not a promise — installing
#   it explicitly means a busybox find or a leaner npm can never silently break
#   the git safe.directory registration.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates findutils curl \
 && rm -rf /var/lib/apt/lists/*

# tectonic — the LaTeX engine the `resumes` profile compiles every tailored .tex
# with. It is NOT in Debian stable (checked the package index by name across all
# suites), so apt cannot supply it and the upstream release asset is the only route.
#
# musl builds, deliberately: statically linked, so no runtime dependencies in this
# slim image. The -gnu assets are ~22 MiB and expect a glibc we would then be
# shipping assumptions about.
#
# Pinned by version AND sha256 per architecture, like the base digests above. An
# unpinned download from the network into a live image is exactly the supply-chain
# surface this repo avoids elsewhere. Both digests are of assets downloaded and
# hashed here, not copied from a release note.
#
# The arch is mapped rather than hardcoded. An earlier version pinned only the
# x86_64 triple, so a build on Apple Silicon or an ARM VPS fetched the Intel asset
# and failed at the version probe — loudly, but confusingly, and only after a wasted
# 10 MB download. Anything outside these two now fails immediately, naming the arch.
#
#   podman build --build-arg TECTONIC_VERSION=0.18.0 \
#                --build-arg TECTONIC_SHA256_AMD64=<sha> \
#                --build-arg TECTONIC_SHA256_ARM64=<sha> -f docker/pi/Dockerfile .
ARG TECTONIC_VERSION=0.17.0
ARG TECTONIC_SHA256_AMD64=8533d07f9ccbd7a65824b9e0459041bca34af1eb33daba48f59215593753a3b7
ARG TECTONIC_SHA256_ARM64=b10954a95404f3ab2328d2fa59a5ebab8e657f893fab096f98be8db7c0c979b8
# Declared explicitly, which is what makes it visible inside the RUN below. Podman
# and buildkit both supply TARGETARCH automatically, but an undeclared automatic arg
# is not in scope — and the symptom of getting that wrong is the quiet one:
# ${TARGETARCH:-amd64} falls back to amd64, a build for an unsupported arch succeeds
# with the Intel asset, and the guard below never fires. Verified by building with
# --build-arg TARGETARCH=riscv64 and confirming it is refused.
ARG TARGETARCH

RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
      amd64) triple=x86_64-unknown-linux-musl;  sha="${TECTONIC_SHA256_AMD64}" ;; \
      arm64) triple=aarch64-unknown-linux-musl; sha="${TECTONIC_SHA256_ARM64}" ;; \
      *) echo "FATAL: tectonic ${TECTONIC_VERSION} publishes no musl build for" >&2; \
         echo "       TARGETARCH='${TARGETARCH:-}'. Supported: amd64, arm64." >&2; \
         echo "       Build with --platform linux/amd64 to override." >&2; \
         exit 1 ;; \
    esac; \
    url="https://github.com/tectonic-typesetting/tectonic/releases/download/tectonic%40${TECTONIC_VERSION}/tectonic-${TECTONIC_VERSION}-${triple}.tar.gz"; \
    curl -fsSL "$url" -o /tmp/tectonic.tar.gz; \
    echo "${sha}  /tmp/tectonic.tar.gz" | sha256sum -c -; \
    tar xzf /tmp/tectonic.tar.gz -C /usr/local/bin tectonic; \
    chmod 755 /usr/local/bin/tectonic; \
    rm -f /tmp/tectonic.tar.gz; \
    tectonic --version

# Pi, pinned so an upstream release cannot change behaviour under a running
# stack without a deliberate bump here. Requires Node >= 22.19.0.
# Exposed as an ARG so a rebuild can move forward deliberately:
#   podman build --build-arg PI_VERSION=1.1.0 -f docker/pi/Dockerfile .
# The default is what CI and compose use.
#
# --ignore-scripts: npm runs lifecycle scripts as root during a global install.
# Pi and its entire dependency tree here declare no install/postinstall hooks
# (verified), so nothing is lost and the supply-chain surface is smaller. If a
# future Pi version needs one, this line is where to notice.
ARG PI_VERSION=1.0.0
RUN npm install -g --no-audit --no-fund --ignore-scripts \
      "@earendil-works/pi-coding-agent@${PI_VERSION}" \
 && npm cache clean --force \
 && pi --version

COPY --from=gobuild /out/miser-money      /usr/local/bin/miser-money
COPY --from=gobuild /out/cookbook         /usr/local/bin/cookbook
COPY --from=gobuild /out/health-check     /usr/local/bin/health-check
COPY --from=gobuild /out/task-manager     /usr/local/bin/task-manager
COPY --from=gobuild /out/project-manager  /usr/local/bin/project-manager

# pi-gateway is the container's main process, not an optional extra. It serves the
# OpenAI-compatible HTTP surface AND spawns one `pi --mode rpc` child per profile,
# each with its cwd set to that profile's directory — which is what makes Pi load
# the right AGENTS.md. Without this binary in the image the container would start
# `pi --mode rpc` on stdio (the default CMD), serve nothing, and pass its healthcheck
# on nothing at all.
COPY --from=gobuild /out/pi-gateway       /usr/local/bin/pi-gateway

RUN chmod 755 /usr/local/bin/miser-money /usr/local/bin/cookbook \
             /usr/local/bin/health-check /usr/local/bin/task-manager \
             /usr/local/bin/project-manager /usr/local/bin/pi-gateway

COPY docker/pi/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENV PI_CODING_AGENT_DIR=/opt/pi \
    NODE_ENV=production

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["rpc"]
