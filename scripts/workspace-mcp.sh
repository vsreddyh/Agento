#!/usr/bin/env bash
# Launcher for the Google Workspace (Gmail) MCP server (#515).
#
# `opencode.json` runs this script as the `workspace-mcp` local server, the
# same way it runs the repo's own `./bin/<server>` binaries. The server
# itself is the upstream Python package (taylorwilsdon/google_workspace_mcp,
# MIT), installed into the git-ignored `.workspace-mcp/venv` by
# `scripts/build-workspace-mcp.sh` — never vendored, never committed.
#
# The surface is deliberately narrow and pinned here, not in opencode.json:
#   --read-only     requests only read scopes (gmail.readonly + openid/email/
#                   profile) and disables every tool that needs a write scope,
#                   so there is no send, delete or label mutation to gate.
#   --tools gmail   one service. No Calendar/Drive/Docs surface.
#   --tool-tier core  the three read tools: search_gmail_messages,
#                   get_gmail_message_content, get_gmail_messages_content_batch.
#   --single-user   one mailbox, one client: credentials come from the
#                   credentials directory below, not a per-session mapping.
#   --transport stdio  matches every other server in this stack (one client).
#
# Enabling a write/send tool is a deliberate change: flip the flags here AND
# add matching `ask` rules to opencode.json, or
# TestWorkspaceMCPSurfaceStaysReadOnly (opencode_perm_test.go) fails.
#
# Credentials: GOOGLE_OAUTH_CLIENT_ID / GOOGLE_OAUTH_CLIENT_SECRET /
# USER_GOOGLE_EMAIL arrive from the root .env through opencode's
# `{env:...}` substitution. The OAuth token cache stays in the git-ignored
# `.workspace-mcp/credentials/`. First-run consent is headless — see
# documentation.md, "Google Workspace (Gmail)".
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MCP_BIN="$REPO/.workspace-mcp/venv/bin/workspace-mcp"

if [[ ! -x "$MCP_BIN" ]]; then
    echo "workspace-mcp is not installed: run ./scripts/build-workspace-mcp.sh (or ./scripts/hermes.sh init)." >&2
    exit 1
fi

# Fail fast instead of starting a server that can never finish consent: the
# OAuth client id/secret are the human-only step (#515 decision 2).
if [[ -z "${GOOGLE_OAUTH_CLIENT_ID:-}" || -z "${GOOGLE_OAUTH_CLIENT_SECRET:-}" ]]; then
    echo "workspace-mcp: GOOGLE_OAUTH_CLIENT_ID / GOOGLE_OAUTH_CLIENT_SECRET are empty." >&2
    echo "  Put them in the root .env (see .env.example) and restart T3/opencode." >&2
    exit 1
fi

# Token cache location. Override only to point at a store outside this
# checkout; the default is runtime state and git-ignored.
export WORKSPACE_MCP_CREDENTIALS_DIR="${WORKSPACE_MCP_CREDENTIALS_DIR:-$REPO/.workspace-mcp/credentials}"
# Pinned loopback callback port. The consent redirect is
# http://localhost:<port>/oauth2callback, which a Desktop-app OAuth client
# accepts on any loopback port, so nothing has to be registered in GCP. The
# port is fixed (not auto-fallback) because the headless procedure forwards
# exactly this port over SSH.
export WORKSPACE_MCP_PORT="${WORKSPACE_MCP_PORT:-8000}"
export GOOGLE_OAUTH_REDIRECT_URI="${GOOGLE_OAUTH_REDIRECT_URI:-http://localhost:$WORKSPACE_MCP_PORT/oauth2callback}"
# oauthlib refuses a plain-http redirect without this; the loopback hop is the
# operator's own SSH forward, never the open network.
export OAUTHLIB_INSECURE_TRANSPORT=1

mkdir -p "$WORKSPACE_MCP_CREDENTIALS_DIR"
# The cache holds refresh tokens: keep new files 0600 / dirs 0700.
umask 077

exec "$MCP_BIN" \
    --transport stdio \
    --single-user \
    --read-only \
    --tools gmail \
    --tool-tier core
