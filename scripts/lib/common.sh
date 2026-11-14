#!/usr/bin/env bash
# Shared helpers for the orchestrator scripts (hermes.sh, retention.sh) and
# the MCP launchers (mcp-run.sh, workspace-mcp.sh), which read the root .env
# through the same parser instead of relying on an inherited environment.
# Sourced, never executed; REPO defaults to the checkout this file lives in.

# Resolve the repo root from the directory of whatever script sources us.
REPO="${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

# Apply the single root .env to the shell environment (no per-profile .env).
# NOTE: parsed, never sourced — sourcing would execute $(...)/backticks in
# values as shell code. Only plain `KEY=value` lines are accepted (optional
# leading `export`, single/double-quoted values unquoted). Whole-line comments
# and blank lines are skipped; an INLINE comment is not stripped — `FOO=bar #
# baz` yields the value `bar # baz` — so keep annotations on their own line
# (a `#` inside a URL fragment stays part of the value, which is why this is
# a documentation rule rather than a stripper).
#
#   _env_file_apply overwrite [KEY...]  every key (or only KEY...) wins
#   _env_file_apply fill      [KEY...]  only keys that are unset or empty
#
# `fill` exists for the MCP launch path (scripts/mcp-run.sh,
# scripts/workspace-mcp.sh): the servers are started by T3/opencode, whose
# environment is empty on this host, so the .env is the only source there —
# and a real variable in the environment (a throwaway MONGODB_URI for dev
# isolation, say) still wins over the file, because `fill` treats empty as
# unset and leaves a non-empty value alone.
_env_file_apply() {
    local mode="${1:-overwrite}"
    shift || true
    local -a only=("$@")
    [[ -f "$REPO/.env" ]] || return 0
    local line key val k wanted
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
        if [[ ${#only[@]} -gt 0 ]]; then
            wanted=1
            for k in "${only[@]}"; do
                [[ "$k" == "$key" ]] && { wanted=0; break; }
            done
            [[ "$wanted" == 0 ]] || continue
        fi
        [[ "$mode" == "fill" && -n "${!key:-}" ]] && continue
        # unquote matching single/double quotes
        if [[ "${#val}" -ge 2 && "${val:0:1}" == '"' && "${val: -1}" == '"' ]]; then
            val="${val:1:-1}"
        elif [[ "${#val}" -ge 2 && "${val:0:1}" == "'" && "${val: -1}" == "'" ]]; then
            val="${val:1:-1}"
        fi
        printf -v "$key" '%s' "$val"
        export "${key?}"
    done < "$REPO/.env"
    # All data consumers use MONGODB_URI as-is (remote Atlas) — there is no
    # local MongoDB service anymore.
}

# Load every key from the root .env, overriding the environment.
load_root_env() {
    _env_file_apply overwrite
}

# Load only the named keys, and only where the environment has no value yet.
# `load_root_env_fill` rather than `load_root_env` is what the MCP launchers
# call so a server gets exactly the keys it names and nothing else — .env also
# holds PASSWORD and the provider key, which no MCP server has any use for.
load_root_env_fill() {
    [[ $# -gt 0 ]] || return 0
    _env_file_apply fill "$@"
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
