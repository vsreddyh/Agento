#!/usr/bin/env bash
set -euo pipefail

: "${HERMES_HOME:=/opt/data}"

# NOTE: env is injected entirely by compose (from the single root
# .env). There are no per-profile .env files.
#
# Multiplex layout: HERMES_HOME is the gateway home AND the god profile
# (Hermes' built-in "default" profile IS the home dir). Story + resumes are
# side profiles nested under $HERMES_HOME/profiles/.
# compose mounts ../gateway:/opt/data and ../workspace:/workspace,
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
# Model traffic goes direct to OpenCode Go (the official image's hermes
# sends x-opencode-session natively). Override only to point elsewhere.
export HERMES_BASE_URL="${HERMES_BASE_URL:-https://opencode.ai/zen/go/v1}"
export MONGODB_URI="${MONGODB_URI:-}"
export MONGODB_DB="${MONGODB_DB:-hermes}"

render_config() {
    # Render one profile's $1/config.yaml.template → $1/config.yaml.
    # Env vars come from the process env (injected by compose
    # from the single root .env). Uses venv python when plain python3
    # is absent (official image keeps it at /opt/hermes/.venv/bin).
    local home="$1" t c py
    t="$home/config.yaml.template"
    c="$home/config.yaml"
    if [[ ! -f "$t" ]]; then
        warning "missing template $t"
        return 0
    fi
    py="$(command -v python3 || echo /opt/hermes/.venv/bin/python)"
    "$py" - "$t" "$c" <<'PY'
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

exec hermes gateway run --force --accept-hooks
