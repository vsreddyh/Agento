#!/usr/bin/env bash
set -euo pipefail

: "${HERMES_HOME:=/hermes-home}"

# NOTE: env is injected entirely by compose (from the single root
# .env). There are no per-profile .env files.
#
# Multiplex layout: HERMES_HOME is the gateway home (profiles/master) and
# every bot is a NAMED profile under $HERMES_HOME/profiles/<name>.
# compose mounts ../profiles/master:/hermes-home and ../workspace:/workspace,
# so this renders config.yaml for the gateway home + each named profile.
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
export HERMES_BASE_URL="${HERMES_BASE_URL:-https://opencode.ai/zen/go/v1}"
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
    # ── Gateway home (profiles/master — not a bot, just the multiplex host) ──
    export HERMES_CWD="${HERMES_CWD:-/workspace}"
    render_config "$HERMES_HOME"

    # ── Named profiles (story, resumes, default-god) ───────
    for home in "$HERMES_HOME"/profiles/*/; do
        [[ -d "$home" ]] || continue
        name="$(basename "$home")"
        if [[ "$name" == "default" ]]; then
            HERMES_CWD="/workspace" render_config "$home"
        else
            HERMES_CWD="/workspace/$name" render_config "$home"
        fi
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
