#!/usr/bin/env bash
set -euo pipefail

# Fully-containerized live stack orchestrator (Podman).
#
# The stack is health-api, proxy and the one-shot retention job, all as compose
# services in docker/docker-compose.yml, against MongoDB Atlas. The agent no
# longer runs here: T3/opencode runs on the host (#494) and is started outside
# this script. init self-installs the host tools it needs (curl, podman +
# podman-compose, python3, cron), builds the images, seeds the single root .env
# and installs the retention cron. Only git + sudo must pre-exist. All env lives
# in the root .env.
# NOTE: container harness only — this script never installs the opencode CLI.
#
# The name is left over from when the agent host was the Hermes gateway.
# Renaming is a separate mechanical change — README.md, documentation.md,
# AGENTS.md, vps.md and the cron/sysmon helpers all reference it — and a
# half-renamed script is worse than an honestly-documented old name.

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN_DIR="$REPO/run"
COMPOSE="$REPO/docker/docker-compose.yml"

# shellcheck source=scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; }
active()  { echo -e "  ${GREEN}●${NC} $1"; }
inactive(){ echo -e "  ${RED}○${NC} $1"; }

usage() {
    cat <<EOF
Usage: $(basename "$0") <command>

Commands:
  init       Build images, seed the root .env, set up host tools (curl, podman, python, cron), install retention cron
  start      Start the whole container stack (health-api, proxy, retention)
  stop       Stop the container stack
  restart    Recreate containers in place (up --build --force-recreate)
  status     Show all service states
  clean      Wipe run state, containers and cron. Destructive.
EOF
}

# ────────────────────────────────────────────────────────────
# RETENTION CRON
# ────────────────────────────────────────────────────────────
retention_cron_line() {
    echo "0 3 * * * bash $SCRIPTS_DIR/retention.sh run >> $RUN_DIR/retention.log 2>&1"
}

install_retention_cron() {
    local line cmd
    line="$(retention_cron_line)"
    cmd="$(command -v crontab || true)"
    if [[ -z "$cmd" ]]; then
        warn "crontab not found — install cron or run scripts/retention.sh manually."
        return 0
    fi
    mkdir -p "$RUN_DIR"
    {
        flock -n 9 || { warn "cron update already in progress — skipping install."; return 0; }
        if crontab -l 2>/dev/null | grep -qF "retention.sh run"; then
            info "Retention cron already installed."
        else
            ( crontab -l 2>/dev/null | grep -vF "retention.sh run" || true; echo "$line" ) | crontab -
            info "Retention cron installed (daily 03:00): $line"
        fi
    } 9>"$RUN_DIR/cron.lock"
}

remove_retention_cron() {
    if command -v crontab &>/dev/null; then
        mkdir -p "$RUN_DIR"
        {
            flock -n 9 || { warn "cron update already in progress — skipping removal."; return 0; }
            ( crontab -l 2>/dev/null | grep -vF "retention.sh run" || true ) | crontab - || true
            info "Retention cron removed."
        } 9>"$RUN_DIR/cron.lock"
    fi
}

# ────────────────────────────────────────────────────────────
# HOST TOOLS (podman, curl, python, cron)
# ────────────────────────────────────────────────────────────
# apt-install <pkgs...> — runs apt-get install, prints output indented, and
# returns apt's real exit code (the pipe to sed must not mask failures).
apt_install() {
    local out rc
    set +e
    sudo apt-get update -qq 2>/dev/null
    out="$(sudo apt-get install -y "$@" 2>&1)"
    rc=$?
    set -e
    printf '%s\n' "$out" | sed 's/^/  /'
    return "$rc"
}

# Generic installer that dispatches to the host's package manager.
# Debian/Ubuntu → apt, Arch → pacman, Fedora → dnf. Maps common package names.
pkg_install() {
    if command -v apt-get &>/dev/null; then
        apt_install "$@"
        return $?
    elif command -v pacman &>/dev/null; then
        local mapped=()
        local p
        for p in "$@"; do
            case "$p" in
                cron) p="cronie" ;;
                python3-venv) p="python-virtualenv" ;;
                python3-pip) p="python-pip" ;;
            esac
            mapped+=("$p")
        done
        local out rc
        set +e
        out="$(sudo pacman -Sy --noconfirm "${mapped[@]}" 2>&1)"
        rc=$?
        set -e
        printf '%s\n' "$out" | sed 's/^/  /'
        return "$rc"
    elif command -v dnf &>/dev/null; then
        local out rc
        set +e
        out="$(sudo dnf install -y "$@" 2>&1)"
        rc=$?
        set -e
        printf '%s\n' "$out" | sed 's/^/  /'
        return "$rc"
    else
        warn "No supported package manager (apt-get/pacman/dnf) — install manually: $*"
        return 1
    fi
}

ensure_curl() {
    command -v curl &>/dev/null && return 0
    info "curl not found — installing (needed for healthchecks and image builds)."
    pkg_install curl || { warn "curl install failed — install curl manually."; return 1; }
    info "curl installed."
}

ensure_podman() {
    if command -v podman &>/dev/null; then
        info "podman available ($(podman --version 2>&1))."
    else
        info "podman not found — installing podman..."
        pkg_install podman || {
            warn "podman install failed — install manually: https://podman.io/docs/installation"
            return 1
        }
    fi
    if command -v podman-compose &>/dev/null; then
        info "podman-compose available."
    elif pkg_install podman-compose 2>/dev/null; then
        info "podman-compose installed."
    else
        info "podman-compose not in system repos — trying pip..."
        # ensure_podman runs before ensure_python in init, so guarantee pip
        # exists first. PIPESTATUS (like apt_install) — `| sed` would mask
        # pip's exit code and the failure branch below would never fire.
        ensure_python || true
        set +e
        { pip install podman-compose 2>&1 \
            || python3 -m pip install --break-system-packages podman-compose 2>&1; } | sed 's/^/  /'
        rc=${PIPESTATUS[0]}
        set -e
        if [[ "$rc" != "0" ]]; then
            warn "podman-compose install failed — install manually (apt: podman-compose, or pip: pip install podman-compose)."
            return 1
        fi
        info "podman-compose installed via pip."
    fi
    # Rootless podman needs lingering so user containers survive logout
    # (the retention cron runs outside any login session). enable-linger
    # takes a username, so this works both rootless and under sudo.
    sudo loginctl enable-linger "${SUDO_USER:-$USER}" 2>&1 | sed 's/^/  /' || true
}

ensure_python() {
    if command -v python3 &>/dev/null &&
        python3 -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 9) else 1)' 2>/dev/null; then
        info "python3 $(python3 --version 2>&1 | sed 's/Python //') available."
        return 0
    fi
    warn "python3 >= 3.9 not found — installing python3 + pip (host tooling)."
    pkg_install python3 python3-pip python3-venv \
        || { warn "python install failed — install python3 manually."; return 1; }
    info "python3 installed: $(python3 --version 2>&1)."
}

ensure_cron() {
    command -v crontab &>/dev/null && return 0
    info "cron not found — installing."
    if ! pkg_install cron; then
        warn "cron install failed — retention won't schedule. On Arch: sudo pacman -S cronie && sudo systemctl enable --now cronie"
        return 1
    fi
    sudo systemctl enable --now cron 2>&1 | sed 's/^/  /' || sudo systemctl enable --now cronie 2>&1 | sed 's/^/  /' || true
    info "cron installed."
}

# ────────────────────────────────────────────────────────────
# INIT
# ────────────────────────────────────────────────────────────
cmd_init() {
    if [[ ! -f "$REPO/.env" ]]; then
        if [[ -f "$REPO/.env.example" ]]; then
            cp "$REPO/.env.example" "$REPO/.env"
            warn "root .env created from .env.example — EDIT IT (all tokens, API keys, Mongo URI)."
        else
            warn "root .env missing and no .env.example exists — create it (the whole stack needs it)."
        fi
    fi
    load_root_env
    mkdir -p "$RUN_DIR"

    ensure_curl || true
    ensure_podman || true
    ensure_python || true
    ensure_cron || true

    info "Building container images (health-api, retention)..."
    podman_compose -f "$COMPOSE" build 2>&1 || { error "podman-compose build failed."; exit 1; }
    info "Pruning unused images (prevents GBs of bloat)..."
    podman image prune -f 2>&1 | sed 's/^/  /' || true

    mkdir -p "$REPO/exports/browser"
    # The retired side-profile checkouts (workspace/portals,
    # workspace/portfolio-resume) stay on disk, orphaned and unmanaged:
    # init neither clones nor deletes them (#289), and nothing mounts them
    # any more. An empty orphan dir would later read as deliberate, so
    # nothing is created here either.

    install_retention_cron
    bash "$SCRIPTS_DIR/sysmon.sh" install || true

    echo
    info "Initialization complete."
    echo "  Next: edit .env with real keys (PASSWORD, Mongo URI), then ./scripts/hermes.sh start"
    echo "  Access: Agento app (sync) at http://<host>:${APP_PORT:-8080}  (APP_PORT in .env)"
    echo "  Access: health-api at http://<host>:${HEALTH_HOST_PORT:-8001}  (HEALTH_HOST_PORT in .env)"
}

# ────────────────────────────────────────────────────────────
# START / STOP / RESTART
# ────────────────────────────────────────────────────────────
cmd_start() {
    load_root_env
    mkdir -p "$RUN_DIR"

    info "Starting container stack (health-api, proxy, retention)..."
    # Restarts reuse layers; only `init` prunes. `up --build` rebuilds layers
    # whose COPY/requirements changed.
    # NO_BUILD, with PI_NO_BUILD / HERMES_NO_BUILD still honoured: the old names are
    # what any existing muscle memory, runbook or .env uses, and silently ignoring
    # them would turn "skip the rebuild" into a full rebuild — the expensive direction.
    # --force-recreate whenever we build, because `up --build` on its own does NOT
    # recreate a container whose compose config is unchanged — and rebuilding the image does
    # not change that config. Without this flag a restart silently keeps the OLD container:
    # the build succeeds and the deploy is a no-op that reports success. That is not
    # hypothetical; it is how a live fix sat in an image while the running server 404'd,
    # through two restarts.
    #
    # Skipped when NO_BUILD=1, where nothing was built and there is nothing to roll.
    if [[ "${NO_BUILD:-${PI_NO_BUILD:-${HERMES_NO_BUILD:-0}}}" == "1" ]]; then
        podman_compose -f "$COMPOSE" up -d 2>&1 || { error "podman-compose up failed."; exit 1; }
    else
        podman_compose -f "$COMPOSE" up -d --build --force-recreate 2>&1 || { error "podman-compose up failed."; exit 1; }
        podman image prune -f 2>&1 | sed 's/^/  /' || true
    fi

    info "Running data retention ..."
    bash "$SCRIPTS_DIR/retention.sh" run 2>&1 | sed 's/^/  /' || true

    echo ""
    cmd_status
}

cmd_stop() {
    podman_compose -f "$COMPOSE" down 2>&1 || warn "podman-compose down failed."

    # Best-effort: some previous setups still have a hermes-gateway systemd unit.
    if systemctl --user is-active hermes-gateway &>/dev/null 2>&1; then
        warn "Stopping legacy hermes-gateway systemd unit."
        systemctl --user stop hermes-gateway 2>&1 || true
    fi

    echo ""
    info "All services stopped."
}

cmd_restart() {
    # Rebuild for changed files (`--build` only rebuilds layers whose COPY/requirements
    # changed), and recreate unconditionally: see the --force-recreate note in cmd_start.
    # A rebuild that leaves the old container running is not a restart.
    echo "=== Restarting (rebuild with cache) ==="
    info "Rebuilding changed layers..."
    podman_compose -f "$COMPOSE" up -d --build --force-recreate 2>&1 || { error "podman-compose up failed."; exit 1; }
    # `start` prunes dangling images, `restart` does not
    info "Running data retention ..."
    bash "$SCRIPTS_DIR/retention.sh" run 2>&1 | sed 's/^/  /' || true
    echo ""
    cmd_status
}

# ────────────────────────────────────────────────────────────
# STATUS
# ────────────────────────────────────────────────────────────
cmd_status() {
    echo "Stack Status (podman stack)" && echo ""
    podman_compose -f "$COMPOSE" ps
    echo ""
    echo "Logs: podman-compose --env-file \"$REPO/.env\" -f docker/docker-compose.yml logs -f <service>"
}

# ────────────────────────────────────────────────────────────
# CLEAN (destructive)
# ────────────────────────────────────────────────────────────
cmd_clean() {
    echo -e "${RED}This wipes:${NC}"
    echo "  - the retention cron entry and the every-minute sysmon sampler"
    echo "  - container volumes and containers"
    echo "  - run/ (logs and sampled metrics)"
    echo -e "${RED}Remote MongoDB is NOT touched, and no tracked file is deleted.${NC}"
    read -r -p "Type 'yes' to wipe everything: " answer
    if [[ "$answer" != "yes" ]]; then
        warn "Clean aborted."
        exit 0
    fi

    podman_compose -f "$COMPOSE" down -v 2>&1 || true
    info "Containers and volumes removed."

    remove_retention_cron
    bash "$SCRIPTS_DIR/sysmon.sh" remove || true
    rm -rf "$RUN_DIR"
    info "run/ removed."

    echo ""
    info "Clean complete. Re-run ./scripts/hermes.sh init to start over."
}

# ────────────────────────────────────────────────────────────
# MAIN
# ────────────────────────────────────────────────────────────
case "${1:-help}" in
    init)    cmd_init ;;
    start)   cmd_start ;;
    stop)    cmd_stop ;;
    restart) cmd_restart ;;
    status)  cmd_status ;;
    clean)   cmd_clean ;;
    help|--help|-h) usage ;;
    *)       error "Unknown command: $1" && usage && exit 1 ;;
esac
