# Documentation

Deep dive into every component of `opencode-remote`. For a fast start, refer to [README.md](README.md).

## Table of Contents

1. [System Architecture](#system-architecture)
2. [LLM Connection (Direct Zen)](#llm-connection-direct-zen)
3. [Stack Lifecycle (Docker)](#stack-lifecycle-docker)
4. [Bot Profiles & Multiplexing](#bot-profiles--multiplexing)
5. [Remote MongoDB & Storage Model](#remote-mongodb--storage-model)
6. [Data Retention & Lifecycle](#data-retention--lifecycle)
7. [Health Connect Pipeline](#health-connect-pipeline)
8. [Android App API (Chat)](#android-app-api-chat)
9. [Hermes Dashboard & Web UI](#hermes-dashboard--web-ui)
10. [Development Mode Isolation](#development-mode-isolation)
11. [Configuration & Environment Reference](#configuration--environment-reference)
12. [Security & Isolation](#security--isolation)
13. [Extending the Stack](#extending-the-stack)

---

## System Architecture

The stack runs **three Hermes profiles** (`story`, `resumes`, `default`-god), a health sync API, an embedded web dashboard, and a scheduled retention job — **fully in Docker**. Everything is defined in a single Compose file ([`docker/docker-compose.yml`](file:///home/vsreddyh/Documents/Discord-bots/docker/docker-compose.yml)).

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
Health Gateway (Android) ──► health-api (:8001) ──► MongoDB
Hermes Dashboard ────────► 0.0.0.0:9119 via gateway (s6, basic auth, unified)
Retention ───────────────► one-shot container (cron 03:00 / on start)
```

- **LLM Connection**: Direct HTTPS communication with OpenCode Zen (`https://opencode.ai/zen/v1`, default model `muse-spark-1.2-contributor-free`).
- **health-api**: FastAPI sync endpoint ([`docker/health-api/main.py`](file:///home/vsreddyh/Documents/Discord-bots/docker/health-api/main.py)) on port `:8001`, writing Health Connect metrics to MongoDB.
- **gateway & dashboard**: Single multiplexed `hermes gateway run` container (`gateway.multiplex_profiles: true`) serving all three profiles. `s6-overlay` supervises both the gateway and the dashboard process on `:9119` when `HERMES_DASHBOARD=1`.
- **app API**: Hermes built-in OpenAI-compatible server (`gateway.api_server` in `profiles/master/config.yaml.template`) on `:8642` — the chat backend for the custom Android app (3 tabs, SSE streaming, shared `API_SERVER_KEY` bearer auth).
- **searxng**: Self-hosted metasearch instance (`:8888`) providing local privacy-preserving search tool capabilities.
- **retention**: One-shot retention job executing [`tools/retention.py`](file:///home/vsreddyh/Documents/Discord-bots/tools/retention.py) via cron or on stack start.
- **Development Isolation**: `HERMES_ENV=dev` directs all database operations to an ephemeral local `mongodb` container (`mongodb://mongodb:27017`), preventing dev writes from ever touching remote production data.

---

## LLM Connection (Direct Zen)

All profiles connect directly to OpenCode Zen (`https://opencode.ai/zen/v1`) using `OPENCODE_ZEN_API_KEY` defined in the root `.env`.

- **Config Rendering**: Rendered as `api_key: ${OPENCODE_ZEN_API_KEY}` in each profile's `config.yaml` from `config.yaml.template` by [`test/entrypoint.sh`](file:///home/vsreddyh/Documents/Discord-bots/test/entrypoint.sh).
- **Vision Model**: Auxiliary vision queries utilize `muse-spark-1.2-contributor-free` natively over OpenCode Zen.
- **Streaming Support**: Direct SSE passthrough when streaming is enabled in Hermes settings.

---

## Stack Lifecycle (Docker)

All container management is orchestrated through [`scripts/hermes.sh`](file:///home/vsreddyh/Documents/Discord-bots/scripts/hermes.sh), backed by [`docker/docker-compose.yml`](file:///home/vsreddyh/Documents/Discord-bots/docker/docker-compose.yml).

### `init`
1. Verifies host dependencies (docker, compose, python3, curl, cron) and installs missing requirements.
2. Builds the shared bot image ([`test/Dockerfile`](file:///home/vsreddyh/Documents/Discord-bots/test/Dockerfile), baking in `s6-overlay`) and the `health-api` image.
3. Initializes root `.env` from `.env.example` if not already present.
4. Copies skill files from `skills/` into each profile directory.
5. Installs the daily data retention cron job (runs daily at 03:00).

### `start`
1. Cleans up any stale native PIDs in `run/bots/*.pid`.
2. Starts the compose stack in detached mode: `docker compose -f docker/docker-compose.yml up -d --build`.
3. Runs an initial retention check using [`scripts/retention.sh`](file:///home/vsreddyh/Documents/Discord-bots/scripts/retention.sh).

### `stop`
Gracefully halts running containers: `docker compose -f docker/docker-compose.yml down`.

### `restart`
Executes a stop followed by a full start sequence.

### `status`
Displays container states and published ports via `docker compose -f docker/docker-compose.yml ps`.

### `clean` (Destructive)
Stops containers, wipes docker volumes (`down -v`), removes `run/`, clears rendered configs and per-profile `.env` files, and removes the retention crontab entry. **Never touches remote MongoDB.**

---

## Bot Profiles & Multiplexing

[`profiles/master/`](file:///home/vsreddyh/Documents/Discord-bots/profiles/master) acts as the gateway root (`HERMES_HOME=/hermes-home`). The individual bot profiles are organized under `profiles/master/profiles/<bot>/`:

| Profile | App Tab | Workspace & Domain Data |
|---|---|---|
| `story` | Story | Lore vault in Git repo (`workspace/portals`, `vsreddyh/portals`) |
| `resumes` | Resumes | LaTeX CV workspace in Git repo (`workspace/resumes`, `vsreddyh/Resume`) |
| `default` | God | Money (`money_transactions`), cookbook (`cookbook_*`), health (`hc_meals`/`hc_days`/`hc_weight`) + Health Connect sync |

### Environment & Token Injection
- All tokens and channel IDs reside in the root `.env`.
- During container startup, [`test/entrypoint.sh`](file:///home/vsreddyh/Documents/Discord-bots/test/entrypoint.sh) renders `config.yaml` for the gateway home and each named profile from the container env.
- This ensures discrete credential scoping without mixing secrets across bot instances.

---

## Remote MongoDB & Storage Model

Domain data for `money`, `health-check`, and `cookbook` is managed in MongoDB (default database: `hermes`, configurable via `MONGODB_DB`):

| Collection | Associated Bot | Schema / Keys |
|---|---|---|
| `money_transactions` | Money | `date` (YYYY-MM-DD), `amount` (float), `type` (income\|expense), `category` (normalized string), `note` (string) |
| `hc_meals` | Health-check | `date` (YYYY-MM-DD), `items[{name, qty?, kcal, protein, carbs, fat, fiber}]`, `totals` (computed), `createdAt` |
| `hc_days` | Health-check / Health Sync | `date` (YYYY-MM-DD, unique), `steps` (int), `active_kcal` (float), `sleep_hours` (float), `workouts[{type, minutes, kcal}]`, `updatedAt` |
| `hc_weight` | Health-check | `date` (YYYY-MM-DD, unique), `kg` (float) — **exempt from retention pruning** |
| `cookbook_ingredients` | Cookbook | `name` (unique), `note`, `createdAt` — **permanent** |
| `cookbook_recipes` | Cookbook | `name` (unique), `servings`, per-serving `kcal/protein_g/carbs_g/fat_g/fiber_g`, `quantities[{ingredient_id, name, qty}]` — **permanent** |
| `cookbook_cook_log` | Cookbook | `recipe_id`, `date`, `cooking_note`, `aftertaste_note` — **permanent** |

### Database Helper CLI
Bots and scripts interact with MongoDB using [`tools/mongo.py`](file:///home/vsreddyh/Documents/Discord-bots/tools/mongo.py):

```bash
python3 tools/mongo.py count money_transactions '{"type":"expense"}'
python3 tools/mongo.py insert hc_weight '{"date":"2026-08-08","kg":63.2}'
python3 tools/mongo.py upsert hc_days '{"date":"2026-08-08"}' '{"steps":8452}'
```

---

## Data Retention & Lifecycle

Automated data pruning is executed by [`tools/retention.py`](file:///home/vsreddyh/Documents/Discord-bots/tools/retention.py):

| Target | Retention Window | Action |
|---|---|---|
| `money_transactions` | > 90 days | Autowiped when `date < today - 90d` |
| `hc_meals` | > 30 days | Pruned when `date < today - 30d` |
| `hc_days` | > 30 days | Pruned when `date < today - 30d` |
| `hc_weight` | Permanent | **Never pruned** |
| `cookbook_*` | Permanent | **Never pruned** |
| `story` / `resumes` | Git history | No database retention operations |

Run manual dry-runs via:
```bash
./scripts/retention.sh --dry-run
```

---

## Health Connect Pipeline

```
Android Health Gateway App ──POST /api/health/sync──► health-api (:8001)
                                                        │
                                                        ▼
                                       MongoDB: hc_days (one doc per date)
```

1. **Android Gateway** (`android/health-gateway/`): Built with Jetpack Compose & Health Connect SDK 1.1.0. Backfills 30 days on initial setup and runs hourly background syncs.
2. **`health-api` Endpoint** (`:8001`): Authenticates requests via `Authorization: Bearer <HEALTH_SYNC_TOKEN>` and upserts metrics into MongoDB.

---

## Android App API (Chat)

The custom Android app (`android/health-gateway/`, 3 chat tabs + Settings) talks to Hermes's built-in OpenAI-compatible API server on the gateway (`:8642`, one port, shared `API_SERVER_KEY` bearer key):

```bash
curl http://<host>:8642/p/story/v1/models -H "Authorization: Bearer <API_SERVER_KEY>"
curl http://<host>:8642/p/story/v1/chat/completions \
  -H "Authorization: Bearer <API_SERVER_KEY>" -H "Content-Type: application/json" \
  -d '{"provider": "opencode", "model": "muse-spark-1.2-contributor-free", "messages": [{"role": "user", "content": "hi"}], "stream": true}'
```

- One port for all tabs; each tab talks to its profile path (`/p/story`, `/p/resumes`, `/p/default` — overridable per tab in app Settings). **Verify live via `GET /p/<profile>/v1/models`**, the source of truth under multiplex.
- Provider (`opencode` | `opencode-go` | `deepinfra`) + model are picked per request in app Settings (blank model = gateway default). Provider keys live ONLY in the git-ignored root `.env` on the VPS — never in git.
- Config lives in `profiles/master/config.yaml.template` (`gateway.api_server`, key rendered from `API_SERVER_KEY`); port published in `docker/docker-compose.yml` (`${API_SERVER_PORT:-8642}:8642`).

---

## Hermes Dashboard & Web UI

- **Supervision**: Supervised via `s6-overlay` in the `gateway` container when `HERMES_DASHBOARD=1`.
- **Binding**: Exposed on `0.0.0.0:9119`.
- **Authentication**: Uses basic HTTP authentication configured via `HERMES_DASHBOARD_BASIC_AUTH_USERNAME`, `HERMES_DASHBOARD_BASIC_AUTH_PASSWORD`, and `HERMES_DASHBOARD_BASIC_AUTH_SECRET` in `.env`.
- **Unified Overview**: Displays runtime state, active sessions, and configuration for all three multiplexed bot profiles.

---

## Development Mode Isolation

Setting `HERMES_ENV=dev` enables isolated development without impacting production databases:
- Activates the `dev` Compose profile, launching an ephemeral `mongo:7` container (`mongodb://mongodb:27017`, no volume) as a single-node replica set (`rs0`, via the `mongodb-init` one-shot) so transactions behave like prod.
- Gateway, `health-api`, and retention automatically target the local MongoDB instance.
- Tearing down the container clears dev data completely.

---

## Configuration & Environment Reference

All settings are configured in the single root `.env` file:

| Variable | Required | Description |
|---|---|---|
| `OPENCODE_ZEN_API_KEY` | **Yes** | API key for OpenCode Zen direct connection |
| `API_SERVER_KEY` | **Yes** | Shared bearer key for the Android app chat tabs (`:8642`) |
| `MONGODB_URI` | **Yes** | Remote MongoDB connection string (used in prod) |
| `MONGODB_DB` | No | Target MongoDB database name (default: `hermes`) |
| `HERMES_ENV` | No | Set to `dev` for local ephemeral MongoDB container |
| `HEALTH_SYNC_TOKEN` | For Health | Bearer token for Android Health Gateway authentication |
| `HERMES_DASHBOARD_BASIC_AUTH_USERNAME` | For Dashboard | Dashboard login username (default: `admin`) |
| `HERMES_DASHBOARD_BASIC_AUTH_PASSWORD` | For Dashboard | Dashboard login password |
| `HERMES_DASHBOARD_BASIC_AUTH_SECRET` | For Dashboard | Stable token signing key (32+ bytes recommended) |
| `SEARXNG_SECRET_KEY` | No | Secret key for SearXNG instance |

---

## Security & Isolation

- **Secrets Management**: Live API keys and database credentials reside exclusively in the git-ignored `.env` file.
- **Container Isolation**: `gateway` container mounts only required directories (`profiles/master`, `workspace`, `/tools` read-only) with no host Docker socket access.
- **Search Hardening**: The SearXNG container runs with all Linux capabilities dropped (`cap_drop: ALL`).
- **Dashboard Protection**: Basic auth protection on port `9119` with secret session token hashing.

---

## Extending the Stack

### Adding a New Bot Profile
1. Create a plan in `profile-plans/<bot>-plan.md`.
2. Create profile directory `profiles/master/profiles/<bot>/` with `config.yaml.template`, `SOUL.md`, and skills.
3. Add the bot identifier to the `BOTS` array in `scripts/hermes.sh`.
4. Rebuild and restart the gateway container:
   ```bash
   ./scripts/hermes.sh restart
   ```

### Adding a Skill
Place the skill directory containing `SKILL.md` inside `skills/` and execute `./scripts/hermes.sh init` to distribute the skill to all bot profiles.
