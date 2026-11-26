#!/usr/bin/env bash
# Launch one repo-local MCP server with its configuration loaded from the
# single root `.env`.
#
# Why this exists: `opencode.json` used to pass `{env:MONGODB_URI}` /
# `{env:MONGODB_DB}` into each server. That substitution reads the
# environment of whatever started T3/opencode — on this host that is a
# systemd unit with no environment at all, so every value expanded to "" and
# all five Go servers exited with "MONGODB_URI is not set". The repo owns its
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

# #584 TEMP-REMOVE (∃-Mail): refuse before the bin/ checks below, so a stale
# bin/reports left over from before the removal can neither exec nor produce
# a confusing stale-file error — remove it with `rm -f bin/reports`.
if [[ "$name" == "reports" ]]; then
    echo "mcp-run.sh: reports is #584 TEMP-REMOVEd (no inbox, no routes) — refusing to launch" >&2
    exit 1
fi

bin="$REPO/bin/$name"
if [[ ! -x "$bin" ]]; then
    echo "mcp-run.sh: $bin is not built — run ./scripts/build-mcps.sh" >&2
    exit 1
fi

# Refuse a binary older than its sources instead of exec-ing it (#557). `bin/`
# is git-ignored and only scripts/build-mcps.sh writes it, so a pull/merge plus
# a T3 restart re-runs the OLD file: that is how the live money server kept a
# removed 90-day TTL and an 11-tool surface days after main moved to 21 tools.
# The check is per-server: cmd/<server> plus exactly the internal packages that
# server builds on (its own store package plus the shared libs it imports —
# see the mapping below, pinned by TestMCPWatchSetsMatchGoDeps so a new import
# without a mapping update fails CI instead of serving stale tools). A change
# to cmd/health-api, another server's store package, or a shared lib the
# server does not import must not take down that MCP server, so those dirs are
# deliberately NOT scanned. All non-test files count, not just *.go: embedded
# assets (e.g. data files pulled in via //go:embed — #584 TEMP-REMOVEd the
# reports katex.min.js example with the inbox) compile into the binary too. *_test.go files are excluded — they never reach the binary.
# A deletion-only commit leaves no newer file behind, so a git HEAD check backs
# the mtime scan: if the newest commit touching the watched paths is newer than
# the binary, the binary predates it regardless of mtimes. The HEAD check uses
# the same test exclusion, so a test-only commit does not trip it. It passes
# -m --full-history: without them a merge that only brings a watched-path
# deletion is TREESAME to its feature-branch parent for those paths, so
# default history simplification prunes the merge and log -1 returns the older
# feature commit — missing a binary built between the feature commit and the
# merge. -m splits merges per parent and --full-history disables the pruning,
# so the merge commit itself (with its own date) is what gets compared.
newest=""
for f in "$REPO/go.mod" "$REPO/go.sum"; do
    if [[ -f "$f" && "$f" -nt "$bin" ]]; then
        newest="$f"
    fi
done
case "$name" in
    task-manager) own="tasks"; shared="mongo mongostore validate" ;;
    project-manager) own="projects"; shared="mongo mongostore" ;;
    miser-money) own="money"; shared="mongo mongostore validate" ;;
    cookbook) own="cookbook"; shared="mongo" ;;
    health-check) own="healthcheck"; shared="mongo validate" ;;
    # #584 TEMP-REMOVE (∃-Mail): reports) own="reports"; shared="mongo" ;;
    *) own=""; shared="" ;;
esac
watch=()
[[ -d "$REPO/cmd/$name" ]] && watch+=("$REPO/cmd/$name")
if [[ -n "$own" && -d "$REPO/internal/$own" ]]; then
    watch+=("$REPO/internal/$own")
elif [[ -z "$own" && -d "$REPO/internal" ]]; then
    # Unknown server (a future SERVERS entry with no mapping yet): scan wide
    # rather than risk serving stale tools.
    watch+=("$REPO/internal")
fi
if [[ -n "$own" ]]; then
    for s in $shared; do
        [[ -d "$REPO/internal/$s" ]] && watch+=("$REPO/internal/$s")
    done
fi
if [[ -z "$newest" && ${#watch[@]} -gt 0 ]]; then
    newest="$(find "${watch[@]}" -type f ! -name '*_test.go' -newer "$bin" -print -quit 2>/dev/null || true)"
fi
if [[ -z "$newest" && -d "$REPO/.git" ]] && command -v git >/dev/null 2>&1; then
    gitpaths=()
    for w in "${watch[@]}"; do gitpaths+=("${w#"$REPO"/}"); done
    gitpaths+=("go.mod" "go.sum")
    # Same test exclusion as the mtime scan: a test-only commit must not trip
    # the guard.
    last="$(git -C "$REPO" log -1 -m --full-history --format=%ct -- "${gitpaths[@]}" ':(exclude)*_test.go' 2>/dev/null || true)"
    if [[ "$last" =~ ^[0-9]+$ ]]; then
        bin_epoch="$(stat -c %Y "$bin" 2>/dev/null || stat -f %m "$bin" 2>/dev/null || echo 0)"
        if (( last > bin_epoch )); then
            newest="git HEAD ($(git -C "$REPO" log -1 -m --full-history --format=%h -- "${gitpaths[@]}" ':(exclude)*_test.go' 2>/dev/null || echo unknown))"
        fi
    fi
fi
if [[ -n "$newest" ]]; then
    echo "mcp-run.sh: bin/$name is stale — $newest is newer than $bin" >&2
    echo "mcp-run.sh: run ./scripts/build-mcps.sh, then restart T3/opencode" >&2
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
