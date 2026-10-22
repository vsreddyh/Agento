# Multi-stage: Go build, then minimal runtime with CA certs (Atlas needs TLS).
# Context is the repo root.
#
# Base images are pinned by digest, as global ARGs, so a rebuild of this file
# reproduces the same toolchain months later — the floating `golang:1.27-alpine`
# and `alpine:3.24.3` tags move under us, and a silent toolchain change is what
# turns into an unexplainable build diff. Global (before the first FROM) rather
# than stage-scoped: podman resolves a stage-scoped ARG in a later FROM to
# nothing and fails with a misleading `no FROM statement found`. The digests are
# the same ones the retention image pins — one builder, one runtime, for both
# jobs.
#
#   podman build --build-arg GO_IMAGE=golang@sha256:<new> \
#                --build-arg RUNTIME_IMAGE=alpine@sha256:<new> \
#                -f docker/health-api/Dockerfile .
#
# golang is build-time only — the binary is copied out — so its digest affects
# reproducibility, not the shipped runtime surface.
ARG GO_IMAGE=golang@sha256:d530cf60723abf1ffae0cd16bb546b379ec0550d2f3815e5b16ed69dbc00fba4
ARG RUNTIME_IMAGE=alpine@sha256:b3413572228e34bd1c9245f5565e6cd127005655e8a7899aef33d8e83255c9e6

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/health-api/ ./cmd/health-api/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/health-api ./cmd/health-api

FROM ${RUNTIME_IMAGE}
RUN apk add --no-cache ca-certificates curl
COPY --from=build /out/health-api /usr/local/bin/health-api

EXPOSE 8000

CMD ["/usr/local/bin/health-api"]
