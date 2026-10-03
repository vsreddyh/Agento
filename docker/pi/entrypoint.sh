#!/usr/bin/env bash
# ──────────────────────────────────────────────────────
# Pi runtime entrypoint.
#
# Pi's own process model is stdio: `--mode rpc` speaks JSONL on stdin/stdout.
# This image therefore does NOT serve HTTP. The HTTP/OpenAI-compatible surface
# is a separate gateway binary that speaks RPC to Pi over the container's
# stdio pair, and that is what gets published (see the compose service).
#
# This script only owns start-up, so a misconfigured container fails loudly
# instead of idling as "healthy".
#
# Runs as root. That is deliberate and load-bearing, not an oversight: the
# bind-mounted workspace is owned by the host uid (10000), so a non-root process
# cannot write the agent's session trees and repo checkouts there. The blast
# radius is an agent with `bash`, which is already the point of this container.
# Do not "fix" this without reworking the mount ownership first.
# ──────────────────────────────────────────────────────
set -euo pipefail

: "${PI_CODING_AGENT_DIR:=/opt/pi}"
export PI_CODING_AGENT_DIR

log() { printf '[pi] %s\n' "$*"; }
die() { printf '[pi] FATAL: %s\n' "$*" >&2; exit 1; }

# Everything git should be allowed to treat as safe, discovered rather than
# hardcoded.
#
# Bind-mounted repos are owned by the host uid while this container runs as
# root, so git rejects them as "dubious ownership" and every repo write fails.
# Discovered at boot instead of baked in, because a literal list silently rots:
# the fourth bind-mounted repo breaks the same way, and a path baked into the
# image that no longer exists reads as drift.
#
# Scoped to /workspace only. A blanket `*` would exempt every directory in the
# container, including paths that have no business being exempted.
#
# Two shapes of repo, both of which hit the ownership check:
#   a normal clone has a .git DIRECTORY
#   a worktree / submodule has a .git FILE pointing at the real git dir
# So match on the name with no -type filter, and accept either. Depth 4 reaches
# an org/repo layout under a single workspace entry. The scan is bounded in both
# depth and name, so cost is a few ms even over a large workspace.
#
# Requires GNU find for -printf (the image installs findutils explicitly rather
# than relying on npm pulling it in).
register_workspace_repos() {
  local d found=0
  [ -d /workspace ] || return 0

  # Cache the current entries once: `git config --add` is append-only, so without
  # this every `podman restart` of the same container re-runs this entrypoint
  # against the same filesystem and grows the list (3 -> 6 -> 9 entries).
  local existing
  existing=$(git config --system --get-all safe.directory 2>/dev/null || true)

  while IFS= read -r -d '' d; do
    d=${d%/}
    if [ ! -d "$d/.git" ] && [ ! -f "$d/.git" ]; then
      continue
    fi
    if printf '%s\n' "$existing" | grep -qxF "$d"; then
      log "  safe.directory = $d (already registered)"
      found=1
      continue
    fi
    git config --system --add safe.directory "$d"
    existing="$existing
$d"
    log "  safe.directory += $d"
    found=1
  done < <(find /workspace -mindepth 1 -maxdepth 4 -name .git -printf '%h\0' 2>/dev/null)
  [ "$found" -eq 1 ] || log "  (no git repos under /workspace)"
}

# ── required secrets ─────────────────────────────────────
# Fail fast rather than surfacing an opaque 400 on the first chat turn.
[ -n "${OPENCODE_API_KEY:-}" ] || die "OPENCODE_API_KEY is unset (root .env)"

# ── agent dir ───────────────────────────────────────────
# /opt/pi is the Pi agent dir: settings.json, mcp.json, auth.json,
# models-store.json, sessions/. Per-profile personality lives at
# /opt/pi/profiles/<name>/AGENTS.md, loaded because each Pi session runs with
# its cwd set there.
mkdir -p "$PI_CODING_AGENT_DIR"

[ -f "$PI_CODING_AGENT_DIR/settings.json" ] \
  || die "missing $PI_CODING_AGENT_DIR/settings.json — is the pi/ dir mounted?"

# ── git ownership ───────────────────────────────────────
if command -v git >/dev/null 2>&1; then
  log "registering bind-mounted repos with git..."
  register_workspace_repos
else
  log "git not present; skipping safe.directory setup"
fi

# ── MCP health ──────────────────────────────────────────
# `pi mcp list` connects every enabled server and exits non-zero if any fails,
# so this is a real connectivity probe (not a config lint) — but only once
# mcp.json lists at least one server.
#
# Only `mcp list` output lands here — server names, tool names, connection
# errors. Never point this at a command that could echo credentials.
if [ -f "$PI_CODING_AGENT_DIR/mcp.json" ]; then
  MCP_CHECK_LOG=$(mktemp)
  log "checking MCP servers..."
  if ! pi mcp list >"$MCP_CHECK_LOG" 2>&1; then
    log "MCP check reported problems:"
    sed 's/^/[pi]   /' "$MCP_CHECK_LOG" >&2 || true
    rm -f "$MCP_CHECK_LOG"
    die "one or more MCP servers failed to connect"
  fi
  # `|| true` here too, matching the failure branch: under `set -e` a sed failure
  # on a *healthy* probe would otherwise abort the boot.
  sed 's/^/[pi]   /' "$MCP_CHECK_LOG" >&2 || true
  rm -f "$MCP_CHECK_LOG"
  log "MCP servers healthy"
fi

# ── hand off ────────────────────────────────────────────
# Default to RPC. Anything else passed on the command line is executed as-is
# (that is how the image is smoke-tested).
if [ "$#" -eq 0 ]; then
  set -- rpc
fi

case "$1" in
  rpc) log "starting pi in RPC mode"; shift; exec pi --mode rpc "$@" ;;
  *)  exec "$@" ;;
esac
