#!/usr/bin/env bash
set -euo pipefail

: "${HERMES_HOME:=/hermes-home}"

# NOTE: env is injected entirely by compose (from the single root
# .env). There are no per-profile .env files.
#
# Multiplex layout: HERMES_HOME is the gateway home AND the god profile
# (Hermes' built-in "default" profile IS the home dir). Story + resumes are
# side profiles nested under $HERMES_HOME/profiles/.
# compose mounts ../gateway:/hermes-home and ../workspace:/workspace,
# so this renders config.yaml for god (home) + each side profile.
#
# The gateway container runs the multiplexed gateway supervised by
# s6-overlay (`gateway run` is an s6-rc service, mirroring official
# nousresearch/hermes-agent).

if [[ "${1:-}" == "chown-data" ]]; then
    uid="${HERMES_UID:-1000}"
    gid="${HERMES_GID:-1000}"
    chown -R "$uid:$gid" "$HERMES_HOME" /workspace
    exit 0
fi

# Container-environment defaults, used by both the live and test stacks.
# Model traffic goes through the local go-shim sidecar (injects the
# x-opencode-session header hermes 0.19.0 doesn't send). Override only to
# bypass the shim (direct upstream needs a client that sends the header).
export HERMES_BASE_URL="${HERMES_BASE_URL:-http://127.0.0.1:18081}"
export MONGODB_URI="${MONGODB_URI:-}"
export MONGODB_DB="${MONGODB_DB:-hermes}"

render_config() {
    # Render one profile's $1/config.yaml.template → $1/config.yaml.
    # Env vars come from the process env (injected by compose
    # from the single root .env).
    local home="$1" t c
    t="$home/config.yaml.template"
    c="$home/config.yaml"
    if [[ ! -f "$t" ]]; then
        warning "missing template $t"
        return 0
    fi
    python3 - "$t" "$c" <<'PY'
import os, re, sys
src = open(sys.argv[1]).read()
def sub(m):
    return os.environ.get(m.group(1), m.group(0))
out = re.sub(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}", sub, src)
open(sys.argv[2], "w").write(out)
PY
    info "rendered $c"
}

warning() { echo "[entrypoint] WARN: $*" >&2; }
info() { echo "[entrypoint] $*"; }

do_render() {
    # ── God (gateway home = Hermes' built-in "default" profile) ──
    export HERMES_CWD="${HERMES_CWD:-/workspace}"
    render_config "$HERMES_HOME"

    # ── Side profiles (story, resumes) ───────
    for home in "$HERMES_HOME"/profiles/*/; do
        [[ -d "$home" ]] || continue
        HERMES_CWD="/workspace/$(basename "$home")" render_config "$home"
    done

    export HERMES_HOME
}

# render-only is used by s6 cont-init ( /etc/cont-init.d/01-render-config )
if [[ "${1:-}" == "render-only" ]]; then
    do_render
    exit 0
fi

# Normal startup: render first, then run the multiplexed gateway.
do_render

# go-shim sidecar (localhost reverse proxy injecting x-opencode-session for
# Go chat). s6 service defs exist but s6 itself is bypassed (entrypoint execs
# the gateway directly), so the shim starts here as a background child of
# PID 1 — it dies with the container, same lifecycle as the gateway.
if command -v /usr/local/bin/go-shim &>/dev/null; then
    info "starting go-shim sidecar (:18081 -> opencode.ai Go)"
    /usr/local/bin/go-shim 2>&1 | sed 's/^/[go-shim] /' &
fi

exec hermes gateway run --force --accept-hooks
