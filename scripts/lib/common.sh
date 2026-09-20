#!/usr/bin/env bash
# Shared helpers for the orchestrator scripts (hermes.sh, retention.sh).
# Source this file after setting REPO.

# Resolve the repo root from the directory of whatever script sources us.
REPO="${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

# Load the single root .env into the shell environment (no per-profile .env).
# NOTE: parsed, never sourced — sourcing would execute $(...)/backticks in
# values as shell code. Only plain `KEY=value` lines are accepted (optional
# leading `export`, single/double-quoted values unquoted); everything else
# (comments, blank lines, multiline values, inline comments) is skipped.
load_root_env() {
    if [[ -f "$REPO/.env" ]]; then
        local line key val
        while IFS= read -r line || [[ -n "$line" ]]; do
            # trim leading/trailing whitespace
            line="${line#"${line%%[![:space:]]*}"}"
            line="${line%"${line##*[![:space:]]}"}"
            [[ -z "$line" || "${line:0:1}" == "#" ]] && continue
            line="${line#export }"
            line="${line#"${line%%[![:space:]]*}"}"
            [[ "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]] || continue
            key="${BASH_REMATCH[1]}"
            val="${BASH_REMATCH[2]}"
            # unquote matching single/double quotes
            if [[ "${#val}" -ge 2 && "${val:0:1}" == '"' && "${val: -1}" == '"' ]]; then
                val="${val:1:-1}"
            elif [[ "${#val}" -ge 2 && "${val:0:1}" == "'" && "${val: -1}" == "'" ]]; then
                val="${val:1:-1}"
            fi
            printf -v "$key" '%s' "$val"
            export "$key"
        done < "$REPO/.env"
    fi
    # All data consumers use MONGODB_URI as-is (remote Atlas) — there is no
    # local MongoDB service anymore.
}

# Run podman-compose, transparently falling back to sudo when the current
# session has no user podman socket (fresh install / first run).
#
# --env-file: interpolation reads the single root .env. Without it, compose
# looks for .env in the compose file's dir (docker/ or test/) and every
# ${VAR} (tokens, MONGODB_URI, ...) silently falls back to empty/default.
podman_compose() {
    if podman info &>/dev/null 2>&1; then
        podman-compose --env-file "$REPO/.env" "$@"
    else
        sudo podman-compose --env-file "$REPO/.env" "$@"
    fi
}
