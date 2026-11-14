#!/usr/bin/env bash
# Launch one repo-local MCP server with its configuration loaded from the
# single root `.env`.
#
# Why this exists: `opencode.json` used to pass `{env:MONGODB_URI}` /
# `{env:MONGODB_DB}` into each server. That substitution reads the
# environment of whatever started T3/opencode — on this host that is a
# systemd unit with no environment at all, so every value expanded to "" and
# all six Go servers exited with "MONGODB_URI is not set". The repo owns its
# secrets, so the launcher owns them too: this wrapper fills the two Mongo
# variables from the root `.env` (parsed, never sourced — see
# scripts/lib/common.sh) and execs the built binary.
#
#   opencode.json: command = ["./scripts/mcp-run.sh", "<server>"]
#
# A real MONGODB_URI/MONGODB_DB in the environment still wins, so pointing a
# session at a throwaway Atlas database (documentation.md, dev isolation)
# keeps working. Only those two keys are read from `.env`: PASSWORD and the
# provider key are not pulled in here (anything T3 itself inherited still
# passes through the exec below).
#
# stdio is the MCP transport here: every diagnostic goes to stderr, never
# stdout.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"

name="${1:-}"
if [[ -z "$name" ]]; then
    available=""
    for b in "$REPO"/bin/*; do
        [[ -x "$b" ]] && available+="$(basename "$b") "
    done
    echo "usage: mcp-run.sh <server>   (built: ${available:-(none — run ./scripts/build-mcps.sh)})" >&2
    exit 2
fi
# A plain server name only: it is used as a path segment under bin/, so
# anything with a slash could exec an arbitrary binary from this config.
if [[ ! "$name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    echo "mcp-run.sh: invalid server name '$name'" >&2
    exit 2
fi

bin="$REPO/bin/$name"
if [[ ! -x "$bin" ]]; then
    echo "mcp-run.sh: $bin is not built — run ./scripts/build-mcps.sh" >&2
    exit 1
fi

load_root_env_fill MONGODB_URI MONGODB_DB
trim_env_keys MONGODB_URI MONGODB_DB

if [[ -z "${MONGODB_URI:-}" ]]; then
    echo "mcp-run.sh: MONGODB_URI is empty — put it in $REPO/.env or export it (see .env.example)." >&2
    exit 1
fi
# Reject the .env.example placeholder, like the Gmail launcher does for its
# three keys: a copied-but-unedited .env otherwise gets as far as a cryptic
# Atlas DNS/auth failure instead of saying what is wrong.
if [[ "$MONGODB_URI" == "mongodb+srv://user:pass@cluster0.example.mongodb.net/" ]]; then
    echo "mcp-run.sh: MONGODB_URI is still the .env.example placeholder — put the real Atlas URI in $REPO/.env or export it." >&2
    exit 1
fi

exec "$bin"
