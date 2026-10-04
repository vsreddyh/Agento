#!/usr/bin/env bash
set -euo pipefail

# Fully-containerized live stack orchestrator (Podman).
#
# Everything (health-api, pi (3 profiles), proxy, retention) — direct to
# https://opencode.ai/zen/go/v1, no LLM proxy — runs as compose services in
# docker/docker-compose.yml. init self-installs the host tools it needs (curl,
# podman + podman-compose, python3, cron), builds the images, seeds the single
# root .env, reports the skills it finds, and installs the retention cron. Only git
# + sudo must pre-exist. All env lives in the root .env (no per-profile .env files).
# No host agent install, venvs, or native processes.
# NOTE: container harness only — this script never installs the opencode CLI
# (LLM traffic goes direct to OpenCode Go over HTTPS; no CLI needed).
#
# The name is left over from when the agent host was the Hermes gateway.
# Renaming is a separate mechanical change — README.md, documentation.md,
# AGENTS.md, vps.md and the cron/sysmon helpers all reference it — and a
# half-renamed script is worse than an honestly-documented old name.

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN_DIR="$REPO/run"
COMPOSE="$REPO/docker/docker-compose.yml"
# Every agent profile, not just the git-backed ones. Under Hermes these were
# split: `gateway/` was both the gateway home AND the god profile, with story and
# resumes nested under it. Pi has no such asymmetry — each profile is a directory
# under the agent dir and each runs as its own process, god included.
PROFILES=(god story resumes)
PI_HOME="$REPO/pi"

profile_home() {
    echo "$PI_HOME/profiles/$1"
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
  init       Build images, seed the root .env (all env vars), report the skills found, set up host tools (curl, podman, python, cron), install retention cron
  start      Start the whole container stack (health-api, pi (3 profiles), proxy, retention)
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

    info "Building container images (agent, health-api)..."
    podman_compose -f "$COMPOSE" build 2>&1 || { error "podman-compose build failed."; exit 1; }
    info "Pruning unused images (prevents GBs of bloat)..."
    podman image prune -f 2>&1 | sed 's/^/  /' || true

    mkdir -p "$PI_HOME" "$REPO/workspace"
    # No chown here, and that is the fix rather than an omission. This used to hand
    # the workspace to uid/gid 10000 because the official Hermes image ran as 10000
    # and bind mounts had to belong to it or the gateway crash-looped. Nothing runs as
    # 10000 any more: the pi service is `user: "0:0"` precisely so the host-owned
    # workspace stays writable. Left in place it did the opposite of its purpose —
    # run as root without SUDO_USER it handed the tree to a uid that owns nothing
    # here, and with SUDO_USER it was silently undone a few lines later.
    #
    # Ownership of the host's own files is the SUDO_USER fix-up's job, further down,
    # and on a non-sudo run the invoking user already owns what they just created.
    # HERMES_UID/HERMES_GID therefore have no remaining reader in this script.
    local b
    for b in "${PROFILES[@]}"; do
        mkdir -p "$(profile_home "$b")"
        # Only story and resumes are repo-backed. God works in its own profile
        # directory and has no repo, so it gets NO workspace/<name> — an empty
        # orphan dir there would later read as deliberate.
        if [[ "$b" == "story" ]]; then
            # Story lives in the portals vault, not workspace/story (retired) —
            # never recreate it.
            mkdir -p "$REPO/workspace/portals"
            # Retired dir: rmdir only removes it when empty, so this can
            # never delete real content — manual deletion also stays safe.
            rmdir "$REPO/workspace/story" 2>/dev/null || true
        elif [[ "$b" == "resumes" ]]; then
            mkdir -p "$REPO/workspace/resumes"
        fi
    done
    # Fix ownership before cloning: when run via sudo, dirs are root-owned and
    # clone as $SUDO_USER would get Permission denied. Do it now, not after.
    if [[ -n "${SUDO_USER:-}" && "$(id -u)" == "0" ]]; then
        chown -R "$SUDO_USER:$(id -gn "$SUDO_USER")" "$REPO/workspace" "$PI_HOME" 2>/dev/null || true
    fi

    # Each git-backed bot keeps its own repo clone in workspace/ (private; SSH
    # auth needs a key on this host — set it up before init). The container
    # commits locally only; pull/push happen here on the host. Both repos stay
    # as separate git remotes; this repo does NOT vendor their files.
    #  - vsreddyh/portals → workspace/portals (story bot lore vault AND story cwd)
    #  - vsreddyh/Resume  → workspace/resumes  (resumes bot cwd IS the repo)
    # The host key at ~/.ssh (or $SUDO_USER's ~/.ssh when run with sudo)
    # is reused — no key generation. Add the deploy key to ~/.ssh before
    # running init.
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
        chown -R "$SUDO_USER:$(id -gn "$SUDO_USER")" "$REPO/workspace" "$PI_HOME" 2>/dev/null || true
    fi

    # Skills are NOT copied. They used to be: `init` copied every skill from the
    # repo's `skills/` tree into each profile's own `skills/` directory, which is how
    # Hermes loaded them. Pi discovers `<agent-dir>/skills/` on its own (measured —
    # pi/README.md), so the copies were a second source of truth that could drift from
    # the first, and `if [[ ! -d "$target" ]]` meant an existing checkout never
    # received a fix. Skills now live in git, in the two places Pi reads them:
    #   pi/skills/<name>/          — shared by every profile (agent dir)
    #   pi/profiles/<name>/skills/ — that profile only, passed with --skill
    # Both are bind-mounted into the container, so `init` has nothing to install.
    # Distinct loop variables (`b` for the profile, `d`/`s` for directories) so a
    # reader does not have to check which one a given line means.
    local skill_count=0
    local s
    for s in "$PI_HOME"/skills/*/; do
        [[ -d "$s" ]] || continue
        skill_count=$((skill_count + 1))
        info "  shared skill: $(basename "$s")"
    done
    local b d
    for b in "${PROFILES[@]}"; do
        d="$(profile_home "$b")/skills"
        [[ -d "$d" ]] || continue
        for s in "$d"/*/; do
            [[ -d "$s" ]] || continue
            skill_count=$((skill_count + 1))
            info "  $b skill: $(basename "$s")"
        done
    done
    # `if`, not `[[ … ]] && warn`: an &&-list evaluates to 1 when the test is false,
    # which is the happy path here, and a construct that fails on success is one
    # edit away from a `set -e` trap.
    if (( skill_count == 0 )); then
        warn "no skills found under $PI_HOME/skills or any profile's skills/ — profiles will run without them"
    fi

install_retention_cron
    bash "$SCRIPTS_DIR/sysmon.sh" install || true

    echo
    info "Initialization complete."
    echo "  Next: edit .env with real keys (OPENCODE_API_KEY, PASSWORD, Mongo URI), then ./scripts/hermes.sh start"
    echo "  Access: Agento app (single URL: chat + sync) at http://<host>:8080  (APP_PORT in .env)"
    echo "  Access: agent API at http://<host>:8643  (bearer PASSWORD)"
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

# One real turn through the app's own path, asserted to return words.
#
# This is not a health check: the container healthcheck passes on a process that is listening
# while every turn it handles fails. That gap is not theoretical — a rebuild left this stack
# answering HTTP 200 with an empty body for every request, healthcheck green throughout, and
# the only evidence was `stopReason: "error"` inside session files. One cheap turn closes it,
# so a broken deploy fails here instead of being noticed later by a person.
smoke_chat() {
    # Not fatal to the caller: a provider outage is not a reason to leave the stack down,
    # and tearing down a working gateway because a model is briefly unavailable helps
    # nobody. The failure is loud and the exit code says so.
    if bash "$SCRIPTS_DIR/smoke.sh"; then
        return 0
    fi
    warn "The stack is up but did not answer a real turn. See the checks above."
    warn "If it was already like this before this run, the cause is not this deploy."
    return 1
}

cmd_start() {
    load_root_env
    mkdir -p "$RUN_DIR"

    legacy_native_bots || {
        warn "Native bots still running — stopping them (legacy migration)."
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

    info "Starting container stack (health-api, pi (3 profiles), proxy)..."
    # Restarts reuse layers; only `init` prunes. `up --build` rebuilds layers
    # whose COPY/requirements changed.
    # PI_NO_BUILD, with HERMES_NO_BUILD still honoured: the old name is what any
    # existing muscle memory, runbook or .env uses, and silently ignoring it would
    # turn "skip the rebuild" into a full rebuild — the expensive direction.
    # --force-recreate whenever we build, because `up --build` on its own does NOT
    # recreate a container whose compose config is unchanged — and rebuilding the image does
    # not change that config. Without this flag a restart silently keeps the OLD container:
    # the build succeeds, the smoke test passes against the previous image, and the deploy
    # is a no-op that reports success. That is not hypothetical; it is how a live session-
    # routes fix sat in an image while the running server 404'd, through two restarts.
    #
    # Skipped when PI_NO_BUILD=1, where nothing was built and there is nothing to roll.
    if [[ "${PI_NO_BUILD:-${HERMES_NO_BUILD:-0}}" == "1" ]]; then
        podman_compose -f "$COMPOSE" up -d 2>&1 || { error "podman-compose up failed."; exit 1; }
    else
        podman_compose -f "$COMPOSE" up -d --build --force-recreate 2>&1 || { error "podman-compose up failed."; exit 1; }
        podman image prune -f 2>&1 | sed 's/^/  /' || true
    fi

    info "Running data retention ..."
    bash "$SCRIPTS_DIR/retention.sh" run 2>&1 | sed 's/^/  /' || true

    echo ""
    # Before the status table, so a failed turn is the last thing on screen.
    smoke_chat || SMOKE_FAILED=1

    cmd_status
    return "${SMOKE_FAILED:-0}"
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
    smoke_chat || SMOKE_FAILED=1
    cmd_status
    return "${SMOKE_FAILED:-0}"
}

# ────────────────────────────────────────────────────────────
# STATUS
# ────────────────────────────────────────────────────────────
cmd_status() {
    echo "Agent Status (podman stack)" && echo ""
    podman_compose -f "$COMPOSE" ps
    echo ""
    echo "Logs: podman-compose -f docker/docker-compose.yml logs -f <service>"
}

# ────────────────────────────────────────────────────────────
# CLEAN (destructive)
# ────────────────────────────────────────────────────────────
cmd_clean() {
    echo -e "${RED}This wipes:${NC}"
    echo "  - every conversation transcript, and pi runtime state (sessions, logs, cache)"
    echo "  - the provider credentials Pi wrote at login (pi/auth.json)"
    echo "  - the retention cron entry"
    echo "  - container volumes and containers"
    echo -e "${RED}Remote MongoDB is NOT touched. Everything tracked in git —"
    echo -e "pi/settings.json, pi/mcp.json, the AGENTS.md files, skills, the"
    echo -e "workspace repos — is KEPT.${NC}"
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
    for b in "${PROFILES[@]}"; do
        wipe_profile "$b"
    done
    # Agent-dir runtime state, named file by file on purpose: pi/ is a TRACKED
    # directory (settings.json, mcp.json, AGENTS.md, profiles/*/AGENTS.md, skills),
    # so a blanket `rm -rf` here would delete committed configuration that `init`
    # does not recreate. Only what Pi writes at runtime goes.
    if [[ -d "$PI_HOME" ]]; then
        info "Wiping pi agent-dir runtime state ..."
        rm -rf "$PI_HOME"/sessions "$PI_HOME"/logs "$PI_HOME"/cache "$PI_HOME"/tmp
        rm -f "$PI_HOME"/auth.json "$PI_HOME"/models-store.json "$PI_HOME"/mcp-auth.json
    fi

    echo ""
    info "Clean complete. Re-run ./scripts/hermes.sh init to start over."
}

# A profile directory is TRACKED too (AGENTS.md, skills/), so this removes
# session and cache state and nothing else — `rm -rf` on a profile would delete
# the instructions the next `init` would not put back.
wipe_profile() {
    local b="$1"
    local d; d="$(profile_home "$b")"
    [[ -d "$d" ]] || return 0
    info "Wiping $b runtime state ..."
    rm -rf "$d"/sessions "$d"/logs "$d"/cache "$d"/tmp
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
