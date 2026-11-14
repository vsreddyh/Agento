#!/usr/bin/env bash
# Shared helpers for the orchestrator scripts (hermes.sh, retention.sh) and
# the MCP launchers (mcp-run.sh, workspace-mcp.sh), which read the root .env
# through the same parser instead of relying on an inherited environment.
# Sourced, never executed; REPO defaults to the checkout this file lives in.

# Resolve the repo root from the directory of whatever script sources us.
REPO="${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

# Apply the single root .env to the shell environment (no per-profile .env).
# NOTE: parsed, never sourced — sourcing would execute $(...)/backticks in
# values as shell code. `KEY=value`, `KEY = value` and a leading `export` are
# all accepted; a quoted value is taken verbatim and an unquoted one is
# trimmed. Whole-line comments and blank lines are skipped, and so is an
# inline comment — whitespace followed by `#` (`FOO=bar # baz` → `bar`,
# `FOO="bar" # baz` → `bar`). A `#` with no whitespace before it is part of
# the value, so a URL fragment (`.../db#frag`) survives, and a fully quoted
# value is taken verbatim.
#
#   _env_file_apply overwrite [KEY...]  every key (or only KEY...) wins
#   _env_file_apply fill      [KEY...]  only keys that are unset or empty
#
# `fill` exists for the MCP launch path (scripts/mcp-run.sh,
# scripts/workspace-mcp.sh): the servers are started by T3/opencode, whose
# environment is empty on this host, so `.env` supplies whatever the
# environment does not — a real variable in it (a throwaway MONGODB_URI for
# dev isolation, say) still wins, because `fill` treats empty as unset and
# leaves a non-empty value alone.
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
        [[ "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=(.*)$ ]] || continue
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
        # trim, then unquote matching single/double quotes: trimming first is
        # what makes `KEY=  "value"` work, and it means an unquoted value is
        # dotenv-trimmed while a quoted one keeps its interior verbatim
        val="${val#"${val%%[![:space:]]*}"}"
        val="${val%"${val##*[![:space:]]}"}"
        # Inline comment: whitespace then `#` is stripped (`FOO=bar # note` →
        # `bar`), which also rescues `FOO="bar" # note`. A `#` with no
        # whitespace before it stays part of the value, so a URL fragment
        # survives; a fully quoted value is taken verbatim.
        if [[ "$val" == *[[:space:]]"#"* ]] \
            && ! { [[ "${#val}" -ge 2 ]] \
                && { [[ "${val:0:1}" == '"' && "${val: -1}" == '"' ]] || [[ "${val:0:1}" == "'" && "${val: -1}" == "'" ]]; }; }; then
            val="${val%%[[:space:]]#*}"
            val="${val%"${val##*[![:space:]]}"}"
        fi
        # A value that still opens a quote it never closes would be loaded with
        # the quote in it and quietly fail every downstream check.
        if [[ "${val:0:1}" == '"' || "${val:0:1}" == "'" ]]; then
            if [[ "${val:0:1}" != "${val: -1}" || "${#val}" -lt 2 ]]; then
                echo "common.sh: $REPO/.env: KEY=${key} opens a quote it never closes — the value is used verbatim and will fail downstream" >&2
            fi
        fi
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

# Trim the surrounding whitespace of the named variables. The parser trims
# values it reads, but an already-exported one arrives verbatim, so a padded
# export would defeat a launcher's `-z`/placeholder checks; launchers call this
# after filling.
trim_env_keys() {
    local k v
    for k in "$@"; do
        v="${!k:-}"
        v="${v#"${v%%[![:space:]]*}"}"
        v="${v%"${v##*[![:space:]]}"}"
        printf -v "$k" '%s' "$v"
        export "${k?}"
    done
}

# Load only the named keys, and only where the environment has no value yet.
# `load_root_env_fill` rather than `load_root_env` is what the MCP launchers
# call, so a server has a value for the keys it names. It only fills — it
# never unsets anything, so whatever the parent process already exported
# (PASSWORD, the provider key, ...) still reaches the server through `exec`;
# starting T3 from a clean shell is what keeps that out.
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
