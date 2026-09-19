# Hermes Android stack

Fully Dockerized agent stack running **three Hermes profiles** (story, resumes, default-god) direct against OpenCode Zen (`https://opencode.ai/zen/v1`). Includes private SearXNG search, **one multiplexed gateway container** with an embedded password-protected dashboard and a built-in OpenAI-compatible API server for the custom **Android app** (3 chat tabs + Settings), Android Health Connect sync via `health-api`, and remote MongoDB persistence.

---

## Architecture

```
                                Remote MongoDB (money, health, cookbook)
                                             ▲
                              Docker containers
  story ──┐               │        ┌──── SearXNG (:8888)
  resumes ┤ ONE Gateway   │ HERMES_HOME=  ▼
  default ┘ (multiplexed  │ /hermes-home │   OpenCode Zen Direct
            3 profiles)   │  (gateway +  │  (https://opencode.ai/zen/v1)
          └── HERMES_DASHBOARD=1 via s6 ─┤   (model: muse-spark-1.2-contributor-free)
          └── API server :8642 ──────────┤   (Android app chat backend)
Agento (Android) ──► health-api (:8001) ──► MongoDB
Hermes Dashboard ────────► 0.0.0.0:9119 via gateway (s6, basic auth, unified)
Retention ───────────────► one-shot container (cron 03:00 / on start)
```

| Service | Container / Process | Published Port | Purpose |
|---|---|---|---|
| `gateway` | `gateway` container (`s6` supervised) | `9119` (dashboard), `8642` (app API) | Multiplexed gateway for all 3 profiles + embedded Web UI dashboard + OpenAI-compatible API server |
| `health-api` | `health-api` container | `8001` | Ingests Health Connect sync data from Android and persists to MongoDB |
| `searxng` | `searxng` container | `8888` | Private search backend for web search tool |
| `retention` | `retention` container (one-shot) | — | Data retention policy runner (`tools/retention.py`) |
| `mongodb` | `mongodb` container (dev-only) | `27017` (internal) | Ephemeral local MongoDB (single-node replica set `rs0`) active only when `HERMES_ENV=dev` |
| `mongodb-init` | `mongodb-init` container (dev-only, one-shot) | — | Runs `rs.initiate()` so dev transactions behave like prod |

---

## Profiles & Domains

| Profile | App Tab | Purpose & Storage | Data Retention Policy |
|---|---|---|---|
| `story` | Story | Mana Revolution lore vault in Git repo (`workspace/portals`, `vsreddyh/portals`) | No DB retention (Git tracked) |
| `resumes` | Resumes | LaTeX resume tailoring & cover letters in Git repo (`workspace/resumes`, `vsreddyh/Resume`) | No DB retention (Git tracked) |
| `default` | God | Money (`money_transactions`), cookbook (`cookbook_*`, permanent), health tracking (`hc_meals`/`hc_days`/`hc_weight`) + Health Connect sync via `health-api` | Money >90d autowipe; `hc_meals`/`hc_days` >30d; `hc_weight` + `cookbook_*` **never pruned** |

---

## Prerequisites & System Requirements

### VPS Sizing Guidelines

| Specification | Minimum Requirement | Recommended (Production) | Notes |
|---|---|---|---|
| **CPU** | 1 vCPU (x86_64 or ARM64) | 2–4 vCPUs | Docker image build (LaTeX/tectonic, Hermes) and SearXNG engine queries benefit from multiple cores. |
| **RAM** | 2 GB RAM (+ 2 GB swap) | 4–8 GB RAM | The multiplexed `gateway` (Python + 3 profiles + web dashboard) and `searxng` consume ~1.2–1.8 GB steady-state. 2 GB minimum with swap is required to avoid OOM during `docker build`. |
| **Disk Storage** | 15 GB SSD | 30+ GB SSD | Docker base images, pip caches, SearXNG indices, local repo clones, LaTeX build artifacts, and logs. |
| **OS** | Linux (Ubuntu 22.04+, Debian 12+, Arch, Fedora) | Ubuntu 22.04/24.04 LTS or Debian 12 | Linux kernel 5.10+ with systemd and package manager (`apt`, `pacman`, or `dnf`). |

### Required Host Tools & Access
- **Git** (`git`) and **sudo** privileges (pre-installed).
- **Docker Engine** (24.0+) & **Docker Compose v2** (`docker compose` plugin). Auto-installed by `./scripts/hermes.sh init` if missing.
- **SSH Key Pair**: Configured in `~/.ssh` with read/write access to private GitHub repos for Git-backed bots:
  - `git@github.com:vsreddyh/portals.git` (Story bot lore vault)
  - `git@github.com:vsreddyh/Resume.git` (Resumes bot CV repository)

### Required External Services & API Keys
- **OpenCode Zen API Key**: `OPENCODE_ZEN_API_KEY` from [opencode.ai](https://opencode.ai) (model: `muse-spark-1.2-contributor-free`).
- **OpenCode Go API Key**: `OPENCODE_GO_API_KEY` (provider `opencode-go`, selectable per request in app Settings).
- **Android App API Key**: `API_SERVER_KEY` (shared bearer key for all 3 chat tabs; generate with `openssl rand -hex 32`). Each tab uses its profile path (`/p/story|resumes|default`) + per-request provider (`opencode`|`opencode-go`) and model from app Settings. Provider keys live only in the VPS `.env`, never in git.
- **MongoDB Cluster**: MongoDB connection URI (`MONGODB_URI`) and database name (`MONGODB_DB`, default `hermes`). (In dev mode, `HERMES_ENV=dev` provides an ephemeral local single-node replica set instead.)
- **Health Sync Secret**: `HEALTH_SYNC_TOKEN` Bearer token matching the Agento Android app. (Retired: `USDA_API_KEY` — health-check takes user-supplied macros only.)
- **Dashboard Web Credentials**: `HERMES_DASHBOARD_BASIC_AUTH_USERNAME`, `HERMES_DASHBOARD_BASIC_AUTH_PASSWORD`, and `HERMES_DASHBOARD_BASIC_AUTH_SECRET` (32+ chars).

### Network & Firewall Ports
- Port `9119/tcp` (Hermes Dashboard) — Inbound access restricted or behind reverse proxy with Basic Auth.
- Port `8642/tcp` (App API server) — Inbound HTTP access for the Android app chat tabs (bearer key auth). The phone must reach the VPS: public IP + firewall rule, or private networking (Tailscale/WireGuard); put a TLS reverse proxy in front if exposed publicly. App Settings values: base URL `http://<host>:8642`, key = `API_SERVER_KEY`, paths `/p/story`, `/p/resumes`, `/p/default`.
- Port `8001/tcp` (Health API) — Inbound HTTP access for Android sync POST requests (same reachability note as `8642`).
- Port `8888/tcp` (SearXNG) — Internal compose network (optional host publish).
- Outbound HTTPS (`443/tcp`) for OpenCode Zen (`opencode.ai`), MongoDB Atlas, and GitHub.

---

## Quick Start

### Setup and Execution

```bash
# 1. Initialize environment file and workspace
cp .env.example .env && nano .env

# 2. Build images and register daily retention cron
./scripts/hermes.sh init

# 3. Start the entire Docker stack
./scripts/hermes.sh start

# 4. Inspect container health and logs
./scripts/hermes.sh status
docker compose -f docker/docker-compose.yml logs -f gateway

# 5. Stop the stack
./scripts/hermes.sh stop
```

Dashboard is accessible at `http://<host>:9119` using credentials configured via `HERMES_DASHBOARD_BASIC_AUTH_USERNAME` and `HERMES_DASHBOARD_BASIC_AUTH_PASSWORD` in `.env`.

---

## Management CLI

`scripts/hermes.sh` is the single entry-point orchestrator:

| Command | Action |
|---|---|
| `./scripts/hermes.sh init` | Self-installs host deps (curl, docker + compose, python3, cron), builds images, creates directories, copies skills, sets up cron. Hermes harness only — never installs the opencode CLI. |
| `./scripts/hermes.sh start` | Starts all services (`docker compose up -d --build`) and runs retention once. |
| `./scripts/hermes.sh stop` | Shuts down the stack (`docker compose down`). |
| `./scripts/hermes.sh restart` | Performs a clean stop and start sequence. |
| `./scripts/hermes.sh status` | Displays container health and published ports (`docker compose ps`). |
| `./scripts/hermes.sh clean` | **Destructive.** Wipes containers, volumes, `run/`, rendered configs, per-profile `.env` files, and retention cron. Remote MongoDB is untouched. |

---

## Development Mode

Setting `HERMES_ENV=dev` in the root `.env` switches the stack to an isolated dev environment:
- Starts a local ephemeral `mongo:7` container (`mongodb://mongodb:27017`, no volume) as a single-node replica set (`rs0`, initiated by the `mongodb-init` one-shot) so money transactions behave exactly like prod Atlas.
- All services (gateway, health-api, retention) connect to the local container.
- Production remote MongoDB is never touched.
- Data resets cleanly upon container destruction.

---

## Health Connect Ingestion

1. **Agento Android App** (`android/agento/`): Reads steps, calories, sleep stages, and workout sessions from Health Connect. Syncs periodically or manually to `POST /api/health/sync` with Bearer auth (`HEALTH_SYNC_TOKEN`).
2. **`health-api` Service**: Validates auth, upserts one doc per date in `hc_days` (steps, active kcal, sleep hours, workouts — same shape the health-check MCP writes).

> **Upgrading from Health Gateway?** Agento is a new app listing (`com.vishnu.agento`), so it installs **alongside** the old Health Gateway build — no auto-update, no settings carry-over (Android sandboxes are per-package).
> 1. Install Agento, then re-enter the sync server URL + token and chat Settings by hand (or copy values over from the old app's Settings screen).
> 2. Re-grant Health Connect permissions inside Agento (grants are per-package).
> 3. Verify one hourly sync lands in `hc_days`, then **uninstall Health Gateway** so its hourly worker stops and the two apps don't double-sync.

## Agento Releases & In-App Updates

- Every push to `main` that touches `android/**` builds debug + release APKs and publishes them as a GitHub Release (`agento-v<versionName>-<run_number>`), so installable APKs live under the repo's **Releases** page. PR builds only upload CI artifacts, never cut a release.
- In the app, **Settings → App updates → Check for updates** diffs `/releases/latest` against the installed build, then **Download & install** streams the release APK and fires the platform installer (grants "install unknown apps" when prompted). Same-package updates keep all Settings + Health Connect grants.
- Stable signing is required for updates to install: add these repo secrets once (Settings → Secrets → Actions) with your upload keystore — `ANDROID_KEYSTORE_BASE64`, `KEYSTORE_PASSWORD`, `KEY_ALIAS`, `KEY_PASSWORD`. Without them CI signs with a throwaway debug key and those APKs will not install over each other. `versionCode` is the GitHub run number so each main build sorts higher than the last.

---

## Remote MongoDB & Data Retention

The shared CLI tool `tools/mongo.py` provides database operations:

```bash
python3 tools/mongo.py insert money_transactions '{"date":"2026-08-08","amount":300,"type":"expense","category":"groceries"}'
python3 tools/mongo.py aggregate money_transactions '[{"$group":{"_id":"$category","total":{"$sum":"$amount"}}}]'
```

Data lifecycle is governed by `tools/retention.py` (`scripts/retention.sh run`):
- `money_transactions`: Purges records where `date < today - 90d`.
- `hc_meals`, `hc_days`: Purges records where `date < today - 30d`.
- `hc_weight`: **Permanent retention** (never pruned).
- `cookbook_ingredients`, `cookbook_recipes`, `cookbook_cook_log`: **Permanent retention** (never pruned).

---

## Repository Layout

```
├── AGENTS.md                # Agent sandbox rules and guidelines
├── README.md                # Stack overview and quickstart guide
├── documentation.md         # Deep-dive architecture and component documentation
├── docker/
│   ├── docker-compose.yml   # Unified compose configuration (searxng + health-api + gateway + retention)
│   ├── health-api/          # Health Connect FastAPI sync service
│   └── README.md            # Docker services documentation
├── test/
│   ├── Dockerfile           # Shared bot image definition (Alpine + hermes-god + s6-overlay)
│   └── entrypoint.sh        # Config rendering and s6 service orchestration
├── mcps/
│   ├── common/              # Shared Mongo/validation lib (not an MCP)
│   ├── money/               # miser-money MCP (accounts + transactions)
│   ├── cookbook/            # cookbook MCP (permanent recipe library)
│   └── health_check/        # health-check MCP (meals + days + weight)
├── profiles/
│   └── master/              # Gateway home
│       ├── config.yaml.template
│       ├── SOUL.md
│       └── profiles/        # Nested named profiles (story, resumes, default)
├── tools/
│   ├── mongo.py             # MongoDB CLI helper for bot toolsets
│   └── retention.py         # Data lifecycle prune runner
├── scripts/
│   ├── hermes.sh            # Main orchestration CLI
│   ├── retention.sh         # Retention execution wrapper
│   └── sysmon.sh            # Resource metrics monitoring script
└── skills/                  # Core skill definitions propagated to bot profiles
```
