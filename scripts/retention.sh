#!/usr/bin/env bash
set -euo pipefail

# Data retention for the Agento domains. Runs via cron (installed by
# scripts/hermes.sh init) and once on every start.
#
# The live stack is fully containerized (Podman), so this just runs the `retention`
# one-shot service from docker/docker-compose.yml (its own minimal image,
# Go `retention` binary baked in, MONGODB_URI/MONGODB_DB injected from the root .env).
# The actual policy logic lives in cmd/retention/main.go.
#
#   story       git repo (workspace/portals)   — no-op
#   resumes     git repo (workspace/portfolio-resume) — no-op
#   money       soft-deleted — no-op (flagged rows vanish from ledger reads at once;
#               the ttl_deleted_at index removes them 10 days after flagging)
#   health-check is permanent — no-op (all five collections; no TTL, no prune tool)
#   cookbook    permanent — no-op
#   #584 TEMP-REMOVE (∃-Mail): reports pruning is DISABLED, stored reports
#   are left as-is (growth is no longer 7-day bounded). Restore the block
#   in cmd/retention/main.go on revert.
#   tasks       reconciled, never pruned (structured repeats with no
#               successor are REPORTED); task_mutations pruned after 90 days

REPO="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE="$REPO/docker/docker-compose.yml"

# shellcheck source=scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"
load_root_env

case "${1:-run}" in
    --dry-run) podman_compose -f "$COMPOSE" run --rm retention --dry-run ;;
    run|"") podman_compose -f "$COMPOSE" run --rm retention ;;
    *) echo "usage: $0 [run|--dry-run]" >&2; exit 2 ;;
esac
