#!/usr/bin/env bash
set -euo pipefail

# Data retention for the Hermes bots. Runs via cron (installed by
# scripts/hermes.sh init) and once on every start.
#
# The live stack is fully containerized (Podman), so this just runs the `retention`
# one-shot service from docker/docker-compose.yml (same bot image, Go
# `retention` binary baked in, MONGODB_URI/MONGODB_DB injected from the root .env).
# The actual policy logic lives in cmd/retention/main.go.
#
#   story       git repo (workspace/portals)   — no-op
#   resumes     git repo (workspace/resumes)   — no-op
#   money       transactions autowiped when the oldest entry is >90 days old
#   health-check hc_meals + hc_days pruned after 30 days; hc_weight is NEVER touched
#   cookbook    permanent — no-op

REPO="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE="$REPO/docker/docker-compose.yml"

# shellcheck source=scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"
load_root_env

case "${1:-run}" in
    --dry-run) podman_compose -f "$COMPOSE" run --rm retention -- --dry-run ;;
    run|"") podman_compose -f "$COMPOSE" run --rm retention ;;
    *) echo "usage: $0 [run|--dry-run]" >&2; exit 2 ;;
esac
