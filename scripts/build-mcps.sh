#!/usr/bin/env bash
# Build the kept MCP servers as static local binaries for T3/opencode (#489).
#
# Produces repo-root `bin/<server>` (git-ignored). `opencode.json` does not
# execute those directly: each entry runs `./scripts/mcp-run.sh <server>`,
# which fills MONGODB_URI/MONGODB_DB from the root .env (T3 supplies nothing of
# its own, though a variable inherited from the shell that started T3 still
# passes through) and then execs the binary. CGO_ENABLED=0 + -trimpath
# so the binaries cannot break on a libc change (the same flags the container
# images use).
#
# Kept servers only (the notes, memory, browser, gmail and calendar servers
# were removed in #495). Secrets are never baked in — every server gets
# MONGODB_URI/MONGODB_DB at runtime from scripts/mcp-run.sh, which reads the
# root .env (see that launcher, not opencode.json: its entries declare no
# `environment` blocks at all).
set -euo pipefail

cd "$(dirname "$0")/.." # repo root

# shellcheck disable=SC2034
SERVERS="task-manager project-manager miser-money cookbook health-check reports"

mkdir -p bin
export CGO_ENABLED=0
for s in $SERVERS; do
  go build -trimpath -ldflags="-s -w" -o "bin/$s" "./cmd/$s"
  echo "built bin/$s"
done
