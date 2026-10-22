# Retention — the one-shot data-lifecycle job, on its own minimal image.
#
# It used to build from `test/Dockerfile`, which is the Hermes runtime: a
# Python venv, node, playwright/chromium, s6-overlay, and eight Go binaries on
# PATH, of which this job used exactly one (`/usr/local/bin/retention`, its
# compose entrypoint). That made the Hermes image a dependency of a service with
# no relationship to the agent, which is the last thing standing between the
# removal and deleting that file.
#
# Modelled on docker/health-api/Dockerfile, because it has the same shape: one
# Go binary, one-shot, needs nothing but TLS to reach Atlas.
#
# ca-certificates is not optional: MONGODB_URI is an Atlas `mongodb+srv://` URI
# and the driver verifies the server certificate against this bundle. Without it
# the job fails at connect with a TLS error that reads like a credentials
# problem.
#
# Context is the repo root (see compose `dockerfile:`).
#
# Base images are pinned by digest, as global ARGs, so a rebuild of this file
# reproduces the same toolchain months later — the floating `golang:1.27-alpine`
# and `alpine:3.24.3` tags move under us, and a silent toolchain change is what
# turns into an unexplainable build diff. Global (before the first FROM) rather
# than stage-scoped: podman resolves a stage-scoped ARG in a later FROM to
# nothing and fails with a misleading `no FROM statement found`.
#
#   podman build --build-arg GO_IMAGE=golang@sha256:<new> \
#                --build-arg RUNTIME_IMAGE=alpine@sha256:<new> \
#                -f docker/retention/Dockerfile .
#
# golang is build-time only — the binary is copied out — so its digest affects
# reproducibility, not the shipped runtime surface.
ARG GO_IMAGE=golang@sha256:d530cf60723abf1ffae0cd16bb546b379ec0550d2f3815e5b16ed69dbc00fba4
ARG RUNTIME_IMAGE=alpine@sha256:b3413572228e34bd1c9245f5565e6cd127005655e8a7899aef33d8e83255c9e6

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/retention/ ./cmd/retention/
COPY internal/ ./internal/
# CGO_ENABLED=0: the runtime stage is alpine, and a static binary cannot break on
# a base-image libc change the way a dynamically linked one does.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/retention ./cmd/retention

FROM ${RUNTIME_IMAGE}
RUN apk add --no-cache ca-certificates
COPY --from=build /out/retention /usr/local/bin/retention

# No EXPOSE and no CMD: compose overrides `entrypoint:` with the binary path
# anyway, and this image serves nothing. A one-shot job that accidentally started
# a listener would outlive its purpose silently.
ENTRYPOINT ["/usr/local/bin/retention"]
