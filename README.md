# Agento Android stack

The Agento **Android app** (Tasks/Projects, Task Manager, Files, ∃-Mail, Health Sync, reminders and widgets) against a small Go backend: **`health-api`** for sync and domain data, an nginx **proxy** as the app's single URL, a one-shot **`retention`** job, and remote **MongoDB Atlas**.

The agent does not run in this stack any more (#494). **T3/opencode runs on the host**, with the kept MCP servers built from this repo (`./scripts/build-mcps.sh` → `bin/<server>`) and wired in `opencode.json`. It reads and writes the same Atlas database the phone uses.

---

## Architecture

```
                                        Remote MongoDB Atlas
                                                  ▲
                                                  │
  Agento (Android) ──► proxy (:8080 compose default) ──► /api/* ──► health-api ──┘
                                                          /health ──┘
  T3/opencode (host) ──► bin/{task-manager,project-manager,miser-money,
                              cookbook,health-check,reports} ──► MongoDB Atlas

  Retention ─────────────► one-shot container (cron 03:00 / on start)
```
(Ports are compose defaults: `APP_PORT` / `HEALTH_HOST_PORT` remap the host side.)

| Service | Container / Process | Published Port | Purpose |
|---|---|---|---|
| `health-api` | `health-api` container | `8001` (`HEALTH_HOST_PORT` remaps) | Tasks, projects, files, Health Connect sync and ∃-Mail reports for the app; persists to MongoDB |
| `proxy` | `proxy` container (nginx) | `8080` (`APP_PORT` remaps) | Single app URL: routes `/api/*` and `/health` → health-api |
| `retention` | `retention` container (one-shot) | — | Data retention policy runner (`cmd/retention`, its own `docker/retention/Dockerfile` image) |

---

## Agent side (host)

| Piece | What it is |
|---|---|
| `opencode.json` | T3/opencode config: the six kept MCP servers (`task-manager`, `project-manager`, `miser-money`, `cookbook`, `health-check`, `reports`), each running `./bin/<server>` with the two Mongo env vars |
| `.opencode/chat.md` | The operator persona and per-server rules |
| `.opencode/skills/` | Shared skills (`podman-management`, `project-manager`, `task-manager`) |
| `scripts/build-mcps.sh` | Builds the kept MCP binaries into the git-ignored `bin/` |

The five retired servers (`notes`, `memory`, `browser`, `gmail`, `calendar`) are still in the source tree until #495; none of them is wired into `opencode.json`.

---

## Data domains & retention

| Domain | Storage | Retention policy |
|---|---|---|
| Money | `money_transactions` | >90d autowipe |
| Cookbook | `cookbook_*` | **Permanent** |
| Health tracking | `hc_meals`, `hc_days`, `hc_weight` + Health Connect sync via `health-api` | `hc_meals`/`hc_days` >30d; `hc_weight` **never pruned** |
| Tasks / projects | `tasks`, `projects` (+ `task_mutations`, `tasks_changes`) | Task TTL 3 days; history outlives it |
| ∃-Mail | `reports` | Read + unstarred >7d; unread and starred are never pruned |
| Notes | `notes` | Permanent (server retired in #495) |

(Retired in #289: the `story` and `resumes` side profiles. Their checkouts,
`workspace/portals` and `workspace/portfolio-resume`, stay on disk, orphaned
and unmanaged.)

---

## Prerequisites & System Requirements

### VPS Sizing Guidelines

| Specification | Minimum Requirement | Recommended (Production) | Notes |
|---|---|---|---|
| **CPU** | 1 vCPU (x86_64 or ARM64) | 2 vCPUs | Image builds (Go) benefit from multiple cores. |
| **RAM** | 1 GB RAM | 2 GB RAM | The stack is now `health-api` + nginx + a one-shot retention job; the former ~350 MB agent container is gone. T3/opencode runs on the host when used, and its footprint is the host tool's, not a container's. |
| **Disk Storage** | 10 GB SSD | 20+ GB SSD | Base images, Go build cache and logs. |
| **OS** | Linux (Ubuntu 22.04+, Debian 12+, Arch, Fedora) | Ubuntu 22.04/24.04 LTS or Debian 12 | Linux kernel 5.10+ with systemd and package manager (`apt`, `pacman`, or `dnf`). |

### Required Host Tools & Access
- **Git** (`git`) and **sudo** privileges (pre-installed).
- **Podman** (4.0+) & **podman-compose** (`podman-compose`). Auto-installed by `./scripts/hermes.sh init` if missing. No daemon — Podman is daemonless.
- **T3/opencode** on the host, only if you want the agent. It is not installed by `init`.

### Required External Services & API Keys
- **OpenCode API Key**: `OPENCODE_API_KEY` from [opencode.ai](https://opencode.ai), for T3/opencode on the host.
- **Android App Password**: `PASSWORD` (single bearer credential for sync; generate with `openssl rand -hex 32`).
- **MongoDB Cluster**: MongoDB Atlas connection URI (`MONGODB_URI`) and database name (`MONGODB_DB`, default `hermes`) — the single data backend for money/health/cookbook/tasks/projects/reports.
- (Retired: `USDA_API_KEY` — health-check takes user-supplied macros only. Retired: `API_SERVER_KEY`, `HEALTH_SYNC_TOKEN` — `PASSWORD` is now the only app password. Retired with the agent container: every `PI_*` variable and `VPS_SSH_DIR`.)

### Network & Firewall Ports
(Ports are compose defaults: `APP_PORT` / `HEALTH_HOST_PORT` remap the host side.)
- Port `${APP_PORT:-8080}/tcp` (App proxy — single URL) — Inbound HTTP access for the Android app (sync, single-password auth). The phone must reach the VPS: public IP + firewall rule; put a TLS reverse proxy in front if exposed publicly. App Settings: Server URL `http://<host>:${APP_PORT:-8080}`, Password = `PASSWORD`.
- Port `${HEALTH_HOST_PORT:-8001}/tcp` (Health API) — direct access for debugging (same reachability note as the proxy).
- Outbound HTTPS (`443/tcp`) for OpenCode Go (`opencode.ai`), MongoDB Atlas and GitHub.

---

## Quick Start

### Setup and Execution

```bash
# 1. Initialize environment file
cp .env.example .env && nano .env

# 2. Build images and register daily retention cron
./scripts/hermes.sh init

# 3. Start the entire container stack
./scripts/hermes.sh start

# 4. Inspect container health and logs
./scripts/hermes.sh status
podman-compose --env-file .env -f docker/docker-compose.yml logs -f health-api  # from the repo root

# 5. Stop the stack
./scripts/hermes.sh stop
```

---

## Management CLI

`scripts/hermes.sh` is the single entry-point orchestrator — run the commands, not the parenthesised spellings, which are what the script's `podman_compose()` wrapper executes (it supplies `--env-file .env -f docker/docker-compose.yml` from the repo root; bare `podman-compose` fails everywhere without them):

| Command | Action |
|---|---|
| `./scripts/hermes.sh init` | Self-installs host deps (curl, podman + compose, python3, cron), builds images, creates the exports dir, sets up cron. Container harness only — never installs the opencode CLI. |
| `./scripts/hermes.sh start` | Starts all services (`podman-compose up -d --build --force-recreate`, plain `up -d` with `NO_BUILD=1`) and runs retention once. |
| `./scripts/hermes.sh stop` | Shuts down the stack (`podman-compose down`). |
| `./scripts/hermes.sh restart` | Rebuilds changed layers and recreates containers in place (`up -d --build --force-recreate`) — not a stop-then-start. |
| `./scripts/hermes.sh status` | Displays container health and published ports (`podman-compose ps`). |
| `./scripts/hermes.sh clean` | **Destructive.** Wipes containers, volumes, `run/`, and the retention/sysmon cron entries. Remote MongoDB and every tracked file are untouched. |

---

## Development Mode

All services read the same root `.env`, so a dev checkout just points `MONGODB_URI` at a separate Atlas database (or a throwaway `MONGODB_DB` name) — no local MongoDB container, no profile switching.

---

## Health Connect Ingestion

1. **Agento Android App** (`android/agento/`): Reads steps, calories, sleep stages, and workout sessions from Health Connect. Syncs periodically or manually to `POST /api/health/sync` with Bearer auth (`PASSWORD`).
2. **`health-api` Service**: Validates auth, upserts one doc per date in `hc_days` (steps, active kcal, sleep hours, workouts — same shape the health-check MCP writes).

> **Upgrading from Health Gateway?** Agento is a new app listing (`com.vishnu.agento`), so it installs **alongside** the old Health Gateway build — no auto-update, no settings carry-over (Android sandboxes are per-package).
> 1. Install Agento, then re-enter the sync server URL + password by hand.
> 2. Re-grant Health Connect permissions inside Agento (grants are per-package).
> 3. Verify one hourly sync lands in `hc_days`, then **uninstall Health Gateway** so its hourly worker stops and the two apps don't double-sync.

## Agento Releases & In-App Updates

- Every push to `main` that touches `android/**` builds the release APK and publishes it as a GitHub Release (`agento-v<version>`, version-only, e.g. `agento-v8.0.0` — same-version rebuilds upsert the existing Release), so installable APKs live under the repo's **Releases** page. PR/branch builds run unit tests + lint only.
- In the app, **Settings → App updates → Check for updates** diffs `/releases/latest` against the installed build, then **Download & install** streams the release APK and fires the platform installer (grants "install unknown apps" when prompted). Same-package updates keep all Settings + Health Connect grants.
- Stable signing is required for updates to install: add these repo secrets once (Settings → Secrets → Actions) with your upload keystore — `ANDROID_KEYSTORE_BASE64`, `KEYSTORE_PASSWORD`, `KEY_ALIAS`, `KEY_PASSWORD`. Without them CI signs with a throwaway debug key and those APKs will not install over each other. `versionCode` derives from semver (`MAJOR*1000000+MINOR*1000+PATCH`) so every version bump sorts higher than the last — no build numbers anywhere.

## Android App Screens (8.x thin client)

- **Sidebar**: Tasks (Projects) + Task Manager + Files + ∃-Mail + Settings.
- **Tasks / Projects**: project board and the task manager; task reminders arrive as exact-alarm notifications and survive reboot.
- **Files**: two-way file shelf over the VPS `exports/` folder (subfolders supported) — upload from the device picker, download to mobile Downloads; uploads never replace an existing file and refuse past 25 MiB with a named error.
- **∃-Mail**: the agent-filed report inbox.
- **Settings sections**: Connection / Health / Appearance / Widget / Notifications / Behaviour / Storage / About, plus app updates and settings backup.
- Chat, Skills, Tools, Teach, Memory, Runs, Approvals, Questions, Connectors and model pickers were removed in 8.0.0 (#493); the agent now runs in T3/opencode.

## Settings Backup (Survives Upgrade, Reinstall, Reinstall-After-Gap)

- All config lives in one `SharedPreferences("agento")` file, which Android preserves across same-package upgrades automatically.
- `backup_rules.xml` / `data_extraction_rules.xml` include sharedprefs + databases in Google Auto Backup, so settings also restore on reinstall from the same account.
- For a reinstall after a long gap (cloud backup expired), use **Settings → Settings backup → Export settings** to keep a JSON copy, then **Import settings** to re-apply it. Import only applies known keys (old chat-era keys are skipped) and reloads the on-screen fields.

---

## Remote MongoDB & Data Retention

The shared CLI tool `mongo` (`cmd/mongo/main.go`) provides database operations:

```bash
go run ./cmd/mongo insert money_transactions '{"date":"2026-08-08","amount":300,"type":"expense","category":"groceries"}'
go run ./cmd/mongo aggregate money_transactions '[{"$group":{"_id":"$category","total":{"$sum":"$amount"}}}]'
```

Data lifecycle is governed by the `retention` Go binary (`cmd/retention/main.go`, `scripts/retention.sh run`):
- `money_transactions`: Purges records where `date < today - 90d`.
- `hc_meals`, `hc_days`: Purges records where `date < today - 30d`.
- `hc_weight`: **Permanent retention** (never pruned).
- `cookbook_ingredients`, `cookbook_recipes`, `cookbook_cook_log`: **Permanent retention** (never pruned).
- `notes`: **Permanent retention** (never pruned) — the agent's no-op (collection retired with the server in #495).
- `reports` (∃-Mail): purges records where `createdAt < today - 7d AND read AND NOT starred` — unread and starred are never pruned.
- `tasks`: reconciled, never pruned — completed structured repeats with no successor are reported (#180).
- `task_mutations`: purges entries where `at < today - 90d`.

---

## Repository Layout

```
├── AGENTS.md                # Agent rules and guidelines
├── README.md                # Stack overview and quickstart guide
├── documentation.md         # Deep-dive architecture and component documentation
├── opencode.json            # T3/opencode config: the six kept MCP servers
├── .opencode/               # Persona (chat.md) + shared skills
├── bin/                     # MCP binaries built by scripts/build-mcps.sh (git-ignored)
├── docker/
│   ├── docker-compose.yml   # Unified compose configuration (health-api + proxy + retention)
│   ├── health-api/          # Health Connect + domain Go service
│   ├── retention/           # The one-shot retention image (one Go binary)
│   └── proxy/               # nginx conf: the app's single URL
├── mcps/
│   ├── money/               # miser-money MCP (accounts + transactions; code in cmd/ + internal/)
│   ├── cookbook/            # cookbook MCP (permanent recipe library; code in cmd/ + internal/)
│   ├── health_check/        # health-check MCP (meals + days + weight; code in cmd/ + internal/)
│   ├── task-manager/        # tasks MCP (code in cmd/ + internal/)
│   ├── project-manager/     # projects MCP (code in cmd/ + internal/)
│   ├── notes/               # notes MCP (retired in #495; code in cmd/ + internal/)
│   └── reports/             # reports MCP (∃-Mail inbox writer, 7-day retention; code in cmd/ + internal/)
├── cmd/                     # Go services (each builds to a static binary)
│   ├── mongo/               # MongoDB CLI helper
│   ├── retention/           # Data lifecycle prune runner
│   ├── health-api/          # App backend (sync, tasks, projects, files, reports)
│   ├── miser-money/         # money MCP server (stdio)
│   ├── cookbook/            # cookbook MCP server (stdio)
│   ├── health-check/        # health-check MCP server (stdio)
│   ├── reports/             # reports MCP server — ∃-Mail inbox writer (stdio)
│   ├── task-manager/        # tasks MCP server (stdio)
│   ├── project-manager/     # projects MCP server (stdio)
│   ├── notes/               # notes MCP server (retired in #495)
│   ├── memory/              # memory MCP server (retired in #495)
│   ├── browser/             # browser MCP server (retired in #495)
│   ├── gmail/               # Gmail connector MCP server (retired in #495)
│   └── calendar/            # Calendar connector MCP server (retired in #495)
├── internal/                # Shared Go packages (mongo, validate, money, cookbook, healthcheck, reports, tasks, projects)
└── scripts/
    ├── hermes.sh            # Main orchestration CLI
    ├── retention.sh         # Retention execution wrapper
    ├── build-mcps.sh        # Builds the kept MCP binaries into bin/
    └── sysmon.sh            # Resource metrics monitoring script
```
