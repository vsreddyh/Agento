#!/usr/bin/env bash
# Launcher for the Google Workspace (Gmail) MCP server (#515).
#
# `opencode.json` runs this script as the `workspace-mcp` local server, the
# same way it runs `./scripts/mcp-run.sh <server>` for the repo's own
# `bin/<server>` binaries. The server
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
# USER_GOOGLE_EMAIL are read from the root .env right here. They do NOT come
# from opencode's `{env:...}` substitution: T3/opencode runs from a systemd
# unit with an empty environment, so that substitution expands to "" and the
# server would start with nothing (the failure that made every server report
# "not set" on 2026-11-14). The OAuth token cache stays in the git-ignored
# `.workspace-mcp/credentials/`. First-run consent is headless — see
# documentation.md, "Google Workspace (Gmail)".
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MCP_BIN="$REPO/.workspace-mcp/venv/bin/workspace-mcp"

if [[ ! -x "$MCP_BIN" ]]; then
    echo "workspace-mcp is not installed: run ./scripts/build-workspace-mcp.sh (or ./scripts/hermes.sh init)." >&2
    exit 1
fi

# Read the credentials from the root .env (parsed, never sourced — see
# scripts/lib/common.sh). A value already in the environment wins, so a
# one-off override still works; only these four keys are read from .env, so
# PASSWORD and the provider key are not pulled in here (whatever T3 itself
# inherited is still passed through by exec below).
# shellcheck source=scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"
load_root_env_fill GOOGLE_OAUTH_CLIENT_ID GOOGLE_OAUTH_CLIENT_SECRET \
    USER_GOOGLE_EMAIL WORKSPACE_MCP_PORT

# Fail fast instead of starting a server that can never finish consent. The
# client id/secret (#515 decision 2) are the human-only step and the account
# (decision 1) is the consent login_hint — without it the server starts and
# consent binds whatever account the browser happens to use. The placeholder
# values are the ones in .env.example, so a copied-but-unedited .env is caught
# here instead of failing at Google's consent screen.
missing=()
if [[ -z "${GOOGLE_OAUTH_CLIENT_ID:-}" || "${GOOGLE_OAUTH_CLIENT_ID}" == "000000000000-xxxxxxxxxxxxxxxx.apps.googleusercontent.com" ]]; then
    missing+=("GOOGLE_OAUTH_CLIENT_ID")
fi
if [[ -z "${GOOGLE_OAUTH_CLIENT_SECRET:-}" || "${GOOGLE_OAUTH_CLIENT_SECRET}" == "GOCSPX-..." ]]; then
    missing+=("GOOGLE_OAUTH_CLIENT_SECRET")
fi
if [[ -z "${USER_GOOGLE_EMAIL:-}" || "${USER_GOOGLE_EMAIL}" == "you@example.com" ]]; then
    missing+=("USER_GOOGLE_EMAIL")
fi
if [[ ${#missing[@]} -gt 0 ]]; then
    echo "workspace-mcp: ${missing[*]} unset or still the .env.example placeholder." >&2
    echo "  Fill in the root .env (see .env.example), then restart T3/opencode." >&2
    exit 1
fi

# Token cache location. Override only to point at a store outside this
# checkout; the default is runtime state and git-ignored.
export WORKSPACE_MCP_CREDENTIALS_DIR="${WORKSPACE_MCP_CREDENTIALS_DIR:-$REPO/.workspace-mcp/credentials}"
# The cache holds refresh tokens: 0600 files / 0700 dirs, set BEFORE the
# directory below is created, and re-applied after, so a directory left at a
# looser mode (created by an older launcher, a manual mkdir, or a restore) is
# repaired rather than silently kept — upstream only probes writability.
umask 077
mkdir -p "$WORKSPACE_MCP_CREDENTIALS_DIR"
chmod 700 "$WORKSPACE_MCP_CREDENTIALS_DIR"
# Same for token files restored from a backup or written before this umask.
chmod 600 "$WORKSPACE_MCP_CREDENTIALS_DIR"/*.json 2>/dev/null || true

# Pinned loopback callback port. The consent redirect is
# http://localhost:<port>/oauth2callback, which a Desktop-app OAuth client
# accepts on any loopback port, so nothing has to be registered in GCP. The
# port is fixed (not auto-fallback) because the headless procedure forwards
# exactly this port over SSH. Setting GOOGLE_OAUTH_REDIRECT_URI is also what
# makes the server honour this port: without it, PORT beats
# WORKSPACE_MCP_PORT in the upstream resolver and the redirect could land
# somewhere else (verified against 2.4.2: PORT=9999 still yields 8000 here).
# Assigned outright, not `${VAR:-...}`: an inherited redirect URI would then
# point somewhere other than WORKSPACE_MCP_PORT, which is the one knob the
# documentation tells the operator to turn.
export WORKSPACE_MCP_PORT="${WORKSPACE_MCP_PORT:-8000}"
export GOOGLE_OAUTH_REDIRECT_URI="http://localhost:$WORKSPACE_MCP_PORT/oauth2callback"
# oauthlib refuses a plain-http redirect without this; the loopback hop is the
# operator's own SSH forward, never the open network.
export OAUTHLIB_INSECURE_TRANSPORT=1

# Scrub settings that could widen what is documented here, so a stray shell
# variable cannot contradict this launcher (the CLI flags below win over the
# env in 2.4.2 — WORKSPACE_MCP_TOOLS/READ_ONLY/TOOL_TIER/PERMISSIONS are only
# consulted when their flag is absent — but the bind host and external URL are
# env-only, and the documentation makes a loopback-only claim).
# WORKSPACE_MCP_HTTP_PORT is env-only too: in stdio mode it adds a second,
# unauthenticated loopback HTTP listener serving the same tools.
unset WORKSPACE_MCP_HOST WORKSPACE_MCP_BASE_URI WORKSPACE_EXTERNAL_URL
unset WORKSPACE_MCP_HTTP_PORT
unset WORKSPACE_MCP_TOOLS WORKSPACE_MCP_TOOL_TIER WORKSPACE_MCP_READ_ONLY \
    WORKSPACE_MCP_PERMISSIONS

exec "$MCP_BIN" \
    --transport stdio \
    --single-user \
    --read-only \
    --tools gmail \
    --tool-tier core
