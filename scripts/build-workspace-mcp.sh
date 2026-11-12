#!/usr/bin/env bash
# Install the Google Workspace (Gmail) MCP server into a repo-local venv (#515).
#
# Second install path next to `scripts/build-mcps.sh`: that one compiles the
# in-repo Go servers into `bin/`, this one pip-installs the one external
# server into the git-ignored `.workspace-mcp/venv`. `scripts/hermes.sh init`
# runs both. `scripts/workspace-mcp.sh` is the launcher opencode.json calls;
# it refuses to start until this has run.
#
# Why a pin: the launcher's flags (--single-user --read-only --tools gmail
# --tool-tier core) and the three-tool surface they produce were verified
# against this version, and
# TestWorkspaceMCPSurfaceStaysReadOnly (opencode_perm_test.go) pins them.
# Bump WORKSPACE_MCP_VERSION deliberately, then re-check the tool list.
#
# No secrets here: the OAuth client id/secret and the token cache live in the
# root .env and `.workspace-mcp/credentials/` (both git-ignored).
set -euo pipefail

cd "$(dirname "$0")/.."

ROOT="$PWD"
VENV="$ROOT/.workspace-mcp/venv"
PYTHON="${PYTHON:-python3}"
# Pinned outright, not `${VAR:-...}`: an env override would install an
# unreviewed upstream version behind the flags this repo verified and
# TestWorkspaceMCPSurfaceStaysReadOnly pins. Bump it here (and in that test).
WORKSPACE_MCP_VERSION="2.4.2"

if ! command -v "$PYTHON" &>/dev/null; then
    echo "build-workspace-mcp: $PYTHON not found (./scripts/hermes.sh init installs python3)." >&2
    exit 1
fi

if ! "$PYTHON" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 10) else 1)'; then
    echo "build-workspace-mcp: workspace-mcp needs Python >= 3.10, found $("$PYTHON" --version 2>&1)." >&2
    exit 1
fi

if [[ "${1:-}" == "--force" ]]; then
    echo "removing $VENV"
    rm -rf "$VENV"
fi

if [[ ! -x "$VENV/bin/python" ]]; then
    echo "creating venv at .workspace-mcp/venv"
    "$PYTHON" -m venv "$VENV" || {
        echo "build-workspace-mcp: venv creation failed — install python3-venv (apt install python3-venv)." >&2
        exit 1
    }
fi

echo "installing workspace-mcp==$WORKSPACE_MCP_VERSION"
"$VENV/bin/python" -m pip install --quiet --disable-pip-version-check \
    "workspace-mcp==$WORKSPACE_MCP_VERSION"

mkdir -p "$ROOT/.workspace-mcp/credentials"
chmod 700 "$ROOT/.workspace-mcp/credentials" # holds refresh tokens

# Prove the entry point the launcher execs exists and runs; a silent partial
# install would only show up as a failed MCP server in T3.
"$VENV/bin/workspace-mcp" --help >/dev/null

echo "installed .workspace-mcp/venv/bin/workspace-mcp ($("$VENV/bin/python" -c \
    'import importlib.metadata as m; print(m.version("workspace-mcp"))'))"
echo "next: set GOOGLE_OAUTH_CLIENT_ID / GOOGLE_OAUTH_CLIENT_SECRET / USER_GOOGLE_EMAIL in .env,"
echo "      restart T3/opencode, then ask the agent for mail — the first call returns the consent"
echo "      URL (documentation.md → \"Google Workspace (Gmail)\"). Running the launcher by hand"
echo "      only opens stdio and waits; it has no mail or URL to show without an MCP client."
