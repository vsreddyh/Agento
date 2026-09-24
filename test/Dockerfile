# Derived image (upstream-blessed pattern): official Tier-1 Hermes runtime +
# our Go MCP servers/CLIs. PyPI is upstream-Unsupported and pinned 0.19.0
# predates the x-opencode-session header Go chat needs; the official image
# (Sep releases, post-v0.21.0 incl. PR #101864) sends it natively.
# Context is the repo root (see compose `dockerfile:`).
# ── Go toolchain: builds the in-repo MCP servers + retention CLI ──
FROM golang:1.26-bookworm AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN go build -trimpath -ldflags="-s -w" -o /out/miser-money ./cmd/miser-money \
 && go build -trimpath -ldflags="-s -w" -o /out/cookbook ./cmd/cookbook \
 && go build -trimpath -ldflags="-s -w" -o /out/health-check ./cmd/health-check \
 && go build -trimpath -ldflags="-s -w" -o /out/retention ./cmd/retention \
 && go build -trimpath -ldflags="-s -w" -o /out/mongo ./cmd/mongo

# ── Hermes runtime: official image (hermes venv, node, playwright/chromium,
# s6-overlay as PID 1). We add Go binaries + our template-render entrypoint.
# NOTE: we intentionally override ENTRYPOINT (render + exec gateway), which
# bypasses their s6 tree — same tradeoff as before, keeps our multiplex +
# template rendering. Revisit if supervised per-profile gateways are wanted.
FROM docker.io/nousresearch/hermes-agent:latest
USER root
# In-repo Go binaries (MCP servers + CLIs).
COPY --from=gobuild /out/miser-money /out/cookbook /out/health-check /out/retention /out/mongo /usr/local/bin/
RUN chmod 755 /usr/local/bin/miser-money /usr/local/bin/cookbook /usr/local/bin/health-check /usr/local/bin/retention /usr/local/bin/mongo
COPY test/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
# Compat shims for upstream skew in the official image (each script is a
# no-op once upstream ships a consistent image, and fails the build if it
# can neither confirm nor establish consistency).
COPY docker/gateway-patches/ /tmp/gateway-patches/
RUN set -e; \
    PY="$(command -v python3 || true)"; \
    if [ -z "$PY" ]; then PY=/opt/hermes/.venv/bin/python; fi; \
    "$PY" /tmp/gateway-patches/01-skills-include-editorial.py && \
    rm -rf /tmp/gateway-patches

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
