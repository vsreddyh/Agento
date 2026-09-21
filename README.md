# Hermes Android stack

Fully containerized agent stack running **three Hermes profiles** (story, resumes, default-god) direct against OpenCode Go (`https://opencode.ai/zen/go/v1`). Includes **one multiplexed gateway container** with a built-in OpenAI-compatible API server for the custom **Android app** (3 chat tabs + Settings), Playwright browser automation (bundled chromium MCP on every profile), Android Health Connect sync via `health-api`, and remote MongoDB persistence.

---

## Architecture

```
                                Remote MongoDB (money, health, cookbook)
                                             ▲
                              Containers
  story ──┐               │
  resumes ┤ ONE Gateway   │ HERMES_HOME=  ▼
  default ┘ (multiplexed  │ /hermes-home │   OpenCode Go Direct
            3 profiles)   │  (gateway +  │  (https://opencode.ai/zen/go/v1)
          └── API server :8642 ──────────┤   (Android app chat backend, via proxy /p/*)
Agento (Android) ──► proxy (:8080) ──┬──► /p/* ──► gateway ──► MongoDB
                                     └──► /api/* ─► health-api ──► MongoDB
Retention ───────────────► one-shot container (cron 03:00 / on start)
```

| Service | Container / Process | Published Port | Purpose |
|---|---|---|---|
| `gateway` | `gateway` container (`s6` supervised) | `8642` (app API) | Multiplexed gateway for all 3 profiles + OpenAI-compatible API server |
| `health-api` | `health-api` container | `8001` | Ingests Health Connect sync data from Android and persists to MongoDB |
| `proxy` | `proxy` container (nginx) | `8080` | Single app URL: routes `/p/*` → gateway chat, `/api/*` → health sync |
| `retention` | `retention` container (one-shot) | — | Data retention policy runner (`cmd/retention`, Go binary in bot image) |

---

## Profiles & Domains

| Profile | App Tab | Purpose & Storage | Data Retention Policy |
|---|---|---|---|
| `default` | God (main) | Money (`money_transactions`), cookbook (`cookbook_*`, permanent), health tracking (`hc_meals`/`hc_days`/`hc_weight`) + Health Connect sync via `health-api` | Money >90d autowipe; `hc_meals`/`hc_days` >30d; `hc_weight` + `cookbook_*` **never pruned** |
| `story` | Story (side) | Mana Revolution lore vault in Git repo (`workspace/portals`, `vsreddyh/portals`) | No DB retention (Git tracked) |
| `resumes` | Resumes (side) | LaTeX resume tailoring & cover letters in Git repo (`workspace/resumes`, `vsreddyh/Resume`) | No DB retention (Git tracked) |

---

## Prerequisites & System Requirements

### VPS Sizing Guidelines

| Specification | Minimum Requirement | Recommended (Production) | Notes |
|---|---|---|---|
| **CPU** | 1 vCPU (x86_64 or ARM64) | 2–4 vCPUs | Image build (LaTeX/tectonic, Hermes, Playwright chromium) benefits from multiple cores. |
| **RAM** | 2 GB RAM (+ 2 GB swap) | 4–8 GB RAM | The multiplexed `gateway` (Python + 3 profiles + bundled chromium) consumes ~1.2–1.8 GB steady-state. 2 GB minimum with swap is required to avoid OOM during `podman build`. |
| **Disk Storage** | 15 GB SSD | 30+ GB SSD | Base images, pip caches, local repo clones, LaTeX build artifacts, Playwright chromium, and logs. |
| **OS** | Linux (Ubuntu 22.04+, Debian 12+, Arch, Fedora) | Ubuntu 22.04/24.04 LTS or Debian 12 | Linux kernel 5.10+ with systemd and package manager (`apt`, `pacman`, or `dnf`). |

### Required Host Tools & Access
- **Git** (`git`) and **sudo** privileges (pre-installed).
- **Podman** (4.0+) & **podman-compose** (`podman-compose`). Auto-installed by `./scripts/hermes.sh init` if missing. No daemon — Podman is daemonless.
- **SSH Key Pair**: Configured in `~/.ssh` with read/write access to private GitHub repos for Git-backed bots:
  - `git@github.com:vsreddyh/portals.git` (Story bot lore vault)
  - `git@github.com:vsreddyh/Resume.git` (Resumes bot CV repository)

### Required External Services & API Keys
- **OpenCode API Key**: `OPENCODE_API_KEY` from [opencode.ai](https://opencode.ai). One key for the single `opencode-go` provider, selected per request in app Settings (model `mimo-v2.5`).
- **Android App Password**: `PASSWORD` (single bearer credential for chat + sync; generate with `openssl rand -hex 32`). The app takes one Server URL + Password; each tab picks provider/model from the live gateway catalog in Settings dropdowns. Provider keys live only in the VPS `.env`, never in git.
- **MongoDB Cluster**: MongoDB Atlas connection URI (`MONGODB_URI`) and database name (`MONGODB_DB`, default `hermes`) — the single data backend for money/health/cookbook.
- **App Password**: `PASSWORD` Bearer token matching the Agento Android app Password field (single credential for chat + sync). (Retired: `USDA_API_KEY` — health-check takes user-supplied macros only. Retired: `API_SERVER_KEY`, `HEALTH_SYNC_TOKEN` — `PASSWORD` is now the only app password.)

### Network & Firewall Ports
- Port `8080/tcp` (App proxy — single URL) — Inbound HTTP access for the Android app (chat + sync, single-password auth). The phone must reach the VPS: public IP + firewall rule; put a TLS reverse proxy in front if exposed publicly. App Settings values: Server URL `http://<host>:8080`, Password = `PASSWORD`, provider/model picked per tab from live dropdowns, paths `/p/story`, `/p/resumes`, `/p/default`. Direct ports `8642` (chat) / `8001` (sync) stay published for backward compatibility.
- Port `8001/tcp` (Health API) — Inbound HTTP access for Android sync POST requests (same reachability note as `8642`).
- Outbound HTTPS (`443/tcp`) for OpenCode Go (`opencode.ai`), MongoDB Atlas, and GitHub.

---

## Quick Start

### Setup and Execution

```bash
# 1. Initialize environment file and workspace
cp .env.example .env && nano .env

# 2. Build images and register daily retention cron
./scripts/hermes.sh init

# 3. Start the entire container stack
./scripts/hermes.sh start

# 4. Inspect container health and logs
./scripts/hermes.sh status
podman-compose -f docker/docker-compose.yml logs -f gateway

# 5. Stop the stack
./scripts/hermes.sh stop
```

---

## Management CLI

`scripts/hermes.sh` is the single entry-point orchestrator:

| Command | Action |
|---|---|
| `./scripts/hermes.sh init` | Self-installs host deps (curl, podman + compose, python3, cron), builds images, creates directories, copies skills, sets up cron. Hermes harness only — never installs the opencode CLI. |
| `./scripts/hermes.sh start` | Starts all services (`podman-compose up -d --build`) and runs retention once. |
| `./scripts/hermes.sh stop` | Shuts down the stack (`podman-compose down`). |
| `./scripts/hermes.sh restart` | Performs a clean stop and start sequence. |
| `./scripts/hermes.sh status` | Displays container health and published ports (`podman-compose ps`). |
| `./scripts/hermes.sh clean` | **Destructive.** Wipes containers, volumes, `run/`, rendered configs, per-profile `.env` files, and retention cron. Remote MongoDB is untouched. |

---

## Development Mode

All services read the same root `.env`, so a dev checkout just points `MONGODB_URI` at a separate Atlas database (or a throwaway `MONGODB_DB` name) — no local MongoDB container, no profile switching.

---

## Health Connect Ingestion

1. **Agento Android App** (`android/agento/`): Reads steps, calories, sleep stages, and workout sessions from Health Connect. Syncs periodically or manually to `POST /api/health/sync` with Bearer auth (`PASSWORD`).
2. **`health-api` Service**: Validates auth, upserts one doc per date in `hc_days` (steps, active kcal, sleep hours, workouts — same shape the health-check MCP writes).

> **Upgrading from Health Gateway?** Agento is a new app listing (`com.vishnu.agento`), so it installs **alongside** the old Health Gateway build — no auto-update, no settings carry-over (Android sandboxes are per-package).
> 1. Install Agento, then re-enter the sync server URL + token and chat Settings by hand (or copy values over from the old app's Settings screen).
> 2. Re-grant Health Connect permissions inside Agento (grants are per-package).
> 3. Verify one hourly sync lands in `hc_days`, then **uninstall Health Gateway** so its hourly worker stops and the two apps don't double-sync.

## Agento Releases & In-App Updates

- Every push to `main` that touches `android/**` builds the release APK and publishes it as a GitHub Release (`agento-v<version>`, version-only, e.g. `agento-v0.3.0` — same-version rebuilds upsert the existing Release), so installable APKs live under the repo's **Releases** page. PR/branch builds only upload the debug APK as a CI artifact, never cut a release.
- In the app, **Settings → App updates → Check for updates** diffs `/releases/latest` against the installed build, then **Download & install** streams the release APK and fires the platform installer (grants "install unknown apps" when prompted). Same-package updates keep all Settings + Health Connect grants.
- Stable signing is required for updates to install: add these repo secrets once (Settings → Secrets → Actions) with your upload keystore — `ANDROID_KEYSTORE_BASE64`, `KEYSTORE_PASSWORD`, `KEY_ALIAS`, `KEY_PASSWORD`. Without them CI signs with a throwaway debug key and those APKs will not install over each other. `versionCode` derives from semver (`MAJOR*1000000+MINOR*1000+PATCH`) so every version bump sorts higher than the last — no build numbers anywhere.

## Settings Backup (Survives Upgrade, Reinstall, Reinstall-After-Gap)

- All config lives in one `SharedPreferences("agento")` file, which Android preserves across same-package upgrades automatically.
- `backup_rules.xml` / `data_extraction_rules.xml` include sharedprefs + databases in Google Auto Backup, so settings also restore on reinstall from the same account.
- For a reinstall after a long gap (cloud backup expired), use **Settings → Settings backup → Export settings** to keep a JSON copy, then **Import settings** to re-apply it. Import only applies known keys and reloads the on-screen fields.

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

---

## Repository Layout

```
├── AGENTS.md                # Agent sandbox rules and guidelines
├── README.md                # Stack overview and quickstart guide
├── documentation.md         # Deep-dive architecture and component documentation
├── docker/
│   ├── docker-compose.yml   # Unified compose configuration (health-api + gateway + retention)
│   ├── health-api/          # Health Connect FastAPI sync service
├── test/
│   ├── Dockerfile           # Shared bot image definition (Debian slim + s6-overlay + Playwright chromium)
│   └── entrypoint.sh        # Config rendering and s6 service orchestration
├── mcps/
│   ├── common/              # Shared Mongo/validation lib (not an MCP)
│   ├── money/               # miser-money MCP (accounts + transactions)
│   ├── cookbook/            # cookbook MCP (permanent recipe library)
│   └── health_check/        # health-check MCP (meals + days + weight)
├── gateway/               # God = default profile = gateway home (HERMES_HOME)
│   ├── config.yaml.template # model + platforms.api_server + MCPs
│   ├── SOUL.md              # god operator
│   └── profiles/            # Nested side profiles (story, resumes)
├── cmd/                 # Go services (each builds to a static binary)
│   ├── mongo/             # MongoDB CLI helper for bot toolsets
│   ├── retention/         # Data lifecycle prune runner
│   ├── health-api/        # Health Connect sync service
│   ├── miser-money/       # money MCP server (stdio)
│   ├── cookbook/          # cookbook MCP server (stdio)
│   └── health-check/      # health-check MCP server (stdio)
├── internal/              # Shared Go packages (mongo, validate, money, cookbook, healthcheck)
├── scripts/
│   ├── hermes.sh            # Main orchestration CLI
│   ├── retention.sh         # Retention execution wrapper
│   └── sysmon.sh            # Resource metrics monitoring script
└── skills/                  # Core skill definitions propagated to bot profiles
```
