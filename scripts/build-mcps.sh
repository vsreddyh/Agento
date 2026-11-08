#!/usr/bin/env bash
# Build the kept MCP servers as static local binaries for T3/opencode (#489).
#
# Produces repo-root `bin/<server>` (git-ignored), which is what the
# `mcp.servers` entries in `opencode.json` execute. Same flags as the pi
# image build (`docker/pi/Dockerfile`): CGO_ENABLED=0 + -trimpath so the
# binaries cannot break on a libc change.
#
# Kept servers only: notes, memory and browser are being removed (#495)
# and must NOT be added here. Secrets are never baked in — every server
# reads MONGODB_URI/MONGODB_DB (gmail/calendar also need
# GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, CONNECTORS_ENCRYPTION_KEY)
# from the environment at runtime; see opencode.json.
set -euo pipefail

cd "$(dirname "$0")/.." # repo root

# shellcheck disable=SC2034
SERVERS="task-manager project-manager miser-money cookbook health-check reports gmail calendar"

mkdir -p bin
export CGO_ENABLED=0
for s in $SERVERS; do
  go build -trimpath -ldflags="-s -w" -o "bin/$s" "./cmd/$s"
  echo "built bin/$s"
done
