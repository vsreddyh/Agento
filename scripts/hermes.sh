#!/usr/bin/env bash
set -euo pipefail

# Fully-containerized live stack orchestrator (Podman).
#
# Everything (health-api, gateway (3 profiles), retention) — direct to https://opencode.ai/zen/v1, no proxy
# runs as compose services in docker/docker-compose.yml. init self-installs the
# host tools it needs (curl, podman + podman-compose, python3, cron),
# builds the images, seeds the single root .env, copies skills,
# and installs the retention cron. Only git + sudo must pre-exist.
# All env lives in the root .env (no per-profile .env files).
# No host Hermes install, venvs, or native processes.
# NOTE: Hermes harness only — this script never installs the opencode CLI
# (LLM traffic goes direct to OpenCode Zen over HTTPS; no CLI needed).

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN_DIR="$REPO/run"
COMPOSE="$REPO/docker/docker-compose.yml"
BOTS=(story resumes default)
GATEWAY_HOME="$REPO/profiles/master"

# Multiplex layout: profiles/master is the gateway home; bots are
# named profiles NESTED under it (profiles/master/profiles/<bot>/).
profile_home() {
    local b="$1"
    echo "$REPO/profiles/master/profiles/$b"
}

# shellcheck source=scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; }
active()  { echo -e "  ${GREEN}●${NC} $1"; }
inactive(){ echo -e "  ${RED}○${NC} $1"; }

usage() {
    cat <<EOF
Usage: $(basename "$0") <command>

Commands:
  init       Build images, seed the root .env (all env vars) + skills, set up host tools (curl, podman, python, cron), install retention cron
  start      Start the whole container stack (health-api, gateway (3 profiles))
  stop       Stop the container stack
  restart    Stop then start
  status     Show all service states
  clean      Wipe everything (profiles state, container volumes, cron). Destructive.
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
    else
        info "podman-compose not found — installing..."
        pkg_install podman-compose || {
            warn "podman-compose install failed — install manually (apt: podman-compose, or pip: pip install podman-compose)."
            return 1
        }
    fi
    # Rootless podman needs lingering so user containers survive logout
    # (the retention cron runs outside any login session).
    if [[ "$(id -u)" != "0" ]]; then
        sudo loginctl enable-linger "$USER" 2>&1 | sed 's/^/  /' || true
    fi
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

    info "Building container images (bot image, health-api)..."
    podman_compose -f "$COMPOSE" build 2>&1 || { error "podman-compose build failed."; exit 1; }
    info "Pruning unused images (prevents GBs of bloat)..."
    podman image prune -f 2>&1 | sed 's/^/  /' || true

    mkdir -p "$GATEWAY_HOME" "$REPO/workspace"
    local b
    for b in "${BOTS[@]}"; do
        mkdir -p "$(profile_home "$b")" "$REPO/workspace/$b"
    done
    # Fix ownership before cloning: when run via sudo, dirs are root-owned and
    # clone as $SUDO_USER would get Permission denied. Do it now, not after.
    if [[ -n "${SUDO_USER:-}" && "$(id -u)" == "0" ]]; then
        chown -R "$SUDO_USER:${SUDO_USER:-$(id -gn "$SUDO_USER")}" "$REPO/workspace" "$GATEWAY_HOME" 2>/dev/null || true
    fi

    # Each git-backed bot keeps its own repo clone in workspace/ (private; SSH
    # auth needs a key on this host — set it up before init). The container
    # commits locally only; pull/push happen here on the host. Both repos stay
    # as separate git remotes; this repo does NOT vendor their files.
    #  - vsreddyh/portals → workspace/portals (story bot lore vault; story cwd is workspace/story)
    #  - vsreddyh/Resume  → workspace/resumes  (resumes bot cwd IS the repo)
    # In dev (HERMES_ENV=dev) the existing host key at ~/.ssh (or $SUDO_USER's
    # ~/.ssh when run with sudo) is reused — no key generation. In prod add
    # the deploy key to ~/.ssh before running init.
    _clone_repo() {
        local url="$1" dest="$2"
        # When run via sudo, clone as the invoking user so the host's existing
        # key (e.g. /home/vsreddyh/.ssh/id_ed25519 in dev) is used and files
        # stay owned by that user, not root.
        if [[ -n "${SUDO_USER:-}" && "$(id -u)" == "0" ]]; then
            sudo -u "$SUDO_USER" env GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=accept-new" git clone "$url" "$dest" 2>&1
        else
            env GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=accept-new" git clone "$url" "$dest" 2>&1
        fi
    }
    if [[ ! -d "$REPO/workspace/resumes/.git" ]]; then
        info "Cloning Resumes repo into workspace/resumes..."
        _clone_repo "git@github.com:vsreddyh/Resume.git" "$REPO/workspace/resumes" \
            || warn "clone failed — configure an SSH key for this host first. ./scripts/hermes.sh start will still work, but the resumes bot won't have its workspace."
    fi
    if [[ ! -d "$REPO/workspace/portals/.git" ]]; then
        info "Cloning Portals (lore vault) repo into workspace/portals..."
        _clone_repo "git@github.com:vsreddyh/portals.git" "$REPO/workspace/portals" \
            || warn "clone failed — configure an SSH key for this host first. ./scripts/hermes.sh start will still work, but the story bot won't have its vault."
    fi
    unset -f _clone_repo
    # When run with sudo, ensure workspace/profile dirs stay owned by the
    # invoking user (not root), so dev edits don't need sudo. Prod also benefits.
    if [[ -n "${SUDO_USER:-}" && "$(id -u)" == "0" ]]; then
        chown -R "$SUDO_USER:${SUDO_USER:-$(id -gn "$SUDO_USER")}" "$REPO/workspace" "$GATEWAY_HOME" 2>/dev/null || true
    fi

    info "Installing project skills into each profile..."
    if [[ -d "$REPO/skills" ]]; then
        for b in "${BOTS[@]}"; do
            local home; home="$(profile_home "$b")"
            for skill_dir in "$REPO/skills"/*/; do
                skill_name="$(basename "$skill_dir")"
                target="$home/skills/$skill_name"
                if [[ ! -d "$target" ]]; then
                    mkdir -p "$home/skills"
                    cp -r "$skill_dir" "$target"
                    info "  $b: installed skill $skill_name"
                fi
            done
        done
    fi

install_retention_cron
    bash "$SCRIPTS_DIR/sysmon.sh" install || true

    echo
    info "Initialization complete."
    echo "  Next: edit .env with real keys (OPENCODE_API_KEY, PASSWORD, Mongo URI), then ./scripts/hermes.sh start"
    echo "  Access: Agento app (single URL: chat + sync) at http://<host>:8080  (APP_PORT in .env)"
    echo "  Access: app API at http://<host>:8642  (bearer PASSWORD)"
    echo "  Access: health-api at http://<host>:8001"
}

# ────────────────────────────────────────────────────────────
# START / STOP / RESTART
# ────────────────────────────────────────────────────────────
legacy_native_bots() {
    # PIDs left behind by the pre-container native launcher.
    local pf found=0
    for pf in "$RUN_DIR"/bots/*.pid; do
        [[ -f "$pf" ]] || continue
        if kill -0 "$(cat "$pf")" 2>/dev/null; then
            warn "Stale NATIVE bot process found: $pf (PID $(cat "$pf"))."
            warn "  Stop it before starting the container stack or ports will conflict."
            found=1
        fi
    done
    [[ "$found" == "1" ]] && return 1
    return 0
}

cmd_start() {
    load_root_env
    mkdir -p "$RUN_DIR"

    legacy_native_bots || {
        warn "Native gateways still running — stopping them (legacy migration)."
        local pf
        for pf in "$RUN_DIR"/bots/*.pid; do
            [[ -f "$pf" ]] || continue
            if kill -0 "$(cat "$pf")" 2>/dev/null; then
                kill "$(cat "$pf")" 2>/dev/null || true
                sleep 2
                kill -0 "$(cat "$pf")" 2>/dev/null && kill -9 "$(cat "$pf")" 2>/dev/null || true
                info "  stopped native bot $(basename "$pf")"
            fi
        done
    }

    info "Starting container stack (health-api, gateway (3 profiles))..."
    # Restarts reuse layers; only `init` prunes. `up --build` rebuilds layers
    # whose COPY/requirements changed.
    if [[ "${HERMES_NO_BUILD:-0}" == "1" ]]; then
        podman_compose -f "$COMPOSE" up -d 2>&1 || { error "podman-compose up failed."; exit 1; }
    else
        podman_compose -f "$COMPOSE" up -d --build 2>&1 || { error "podman-compose up failed."; exit 1; }
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
    # Rebuild for changed files (`up -d --build` only rebuilds layers whose
    # COPY/requirements changed).
    echo "=== Restarting (rebuild with cache) ==="
    info "Rebuilding changed layers..."
    podman_compose -f "$COMPOSE" up -d --build 2>&1 || { error "podman-compose up failed."; exit 1; }
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
    echo "Hermes Agent Status (podman stack)" && echo ""
    podman_compose -f "$COMPOSE" ps
    echo ""
    echo "Logs: podman-compose -f docker/docker-compose.yml logs -f <service>"
}

# ────────────────────────────────────────────────────────────
# CLEAN (destructive)
# ────────────────────────────────────────────────────────────
cmd_clean() {
    echo -e "${RED}This wipes:${NC}"
    echo "  - all profile runtime state (sessions, logs, DBs, rendered config)"
    echo "  - per-profile .env files (regenerated at container start)"
    echo "  - the retention cron entry"
    echo "  - container volumes and containers"
    echo -e "${RED}Remote MongoDB is NOT touched. Committed files (skills, memories,"
    echo -e "SOUL.md, templates) are KEPT. Committed files are NOT touched.${NC}"
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

    local b
    for b in "${BOTS[@]}"; do
        wipe_profile "$b"
    done
    # Also wipe gateway home rendered config / runtime (not a bot, but Hermes
    # writes state there too).
    if [[ -d "$GATEWAY_HOME" ]]; then
        info "Wiping gateway home runtime state ..."
        rm -f "$GATEWAY_HOME/config.yaml" "$GATEWAY_HOME/config.rendered.yaml" \
            "$GATEWAY_HOME/auth.lock" "$GATEWAY_HOME/gateway.lock" \
            "$GATEWAY_HOME/channel_directory.json" \
            "$GATEWAY_HOME/.skills_prompt_snapshot.json" "$GATEWAY_HOME/.clean_shutdown"
        rm -rf "$GATEWAY_HOME"/.cache "$GATEWAY_HOME"/.local "$GATEWAY_HOME"/sessions \
            "$GATEWAY_HOME"/state "$GATEWAY_HOME"/state.db* "$GATEWAY_HOME"/logs "$GATEWAY_HOME"/cron "$GATEWAY_HOME"/kanban* \
            "$GATEWAY_HOME"/gateway* "$GATEWAY_HOME"/bin "$GATEWAY_HOME"/data "$GATEWAY_HOME"/image_cache "$GATEWAY_HOME"/audio_cache \
            "$GATEWAY_HOME"/hooks "$GATEWAY_HOME"/sandboxes "$GATEWAY_HOME"/platforms "$GATEWAY_HOME"/pairing "$GATEWAY_HOME"/cache
    fi

    echo ""
    info "Clean complete. Re-run ./scripts/hermes.sh init to start over."
}

wipe_profile() {
    local b="$1"
    local d; d="$(profile_home "$b")"
    [[ -d "$d" ]] || return 0
    info "Wiping $b runtime state ..."
    rm -f "$d/config.yaml" "$d/config.rendered.yaml" "$d/.env" \
        "$d/auth.lock" "$d/gateway.lock" "$d/channel_directory.json" \
        "$d/.skills_prompt_snapshot.json" "$d/.clean_shutdown"
    rm -rf "$d"/.cache "$d"/.local "$d"/sessions \
        "$d"/state "$d"/state.db* "$d"/logs "$d"/cron "$d"/kanban* \
        "$d"/gateway* "$d"/bin "$d"/data "$d"/image_cache "$d"/audio_cache \
        "$d"/hooks "$d"/sandboxes "$d"/platforms "$d"/pairing "$d"/cache
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
