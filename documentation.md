# Documentation

Deep dive into every component of `opencode-remote`. For a fast start, refer to [README.md](README.md).

## Table of Contents

1. [System Architecture](#system-architecture)
2. [LLM Connection (Direct Go)](#llm-connection-direct-go)
3. [Stack Lifecycle (Podman)](#stack-lifecycle-podman)
4. [Bot Profiles & Multiplexing](#bot-profiles--multiplexing)
5. [Remote MongoDB & Storage Model](#remote-mongodb--storage-model)
6. [Data Retention & Lifecycle](#data-retention--lifecycle)
7. [Health Connect Pipeline](#health-connect-pipeline)
8. [Android App API (Chat)](#android-app-api-chat)
9. [Development Mode Isolation](#development-mode-isolation)
10. [Configuration & Environment Reference](#configuration--environment-reference)
11. [Security & Isolation](#security--isolation)
12. [Extending the Stack](#extending-the-stack)

---

## System Architecture

The stack runs god (main) + story/resumes (sides), a health sync API, and a scheduled retention job — **fully in containers**. Everything is defined in a single Compose file ([`docker/docker-compose.yml`](file:///home/vsreddyh/Documents/Discord-bots/docker/docker-compose.yml)).

```
                               Remote MongoDB (money, health, cookbook)
                                            ▲
                             Containers
  god ────┐               │
  story   ┤ ONE Gateway   │ HERMES_HOME=  ▼
  resumes ┘ god + 2 sides │ /opt/data │   OpenCode Go Direct
           (multiplexed)  │  (gateway +  │  (https://opencode.ai/zen/go/v1)
         └── API server :8642 ──────────┤   (Android app chat backend, via proxy /p/*)
Agento (Android) ──► proxy (:8080) ──┬──► /p/* ──► gateway ──► MongoDB
                                     └──► /api/* ─► health-api ──► MongoDB
Retention ───────────────► one-shot container (cron 03:00 / on start)
```

- **LLM Connection**: Direct HTTPS communication with OpenCode Go (`https://opencode.ai/zen/go/v1`, default model `mimo-v2.5`).
- **health-api**: FastAPI sync endpoint ([`docker/health-api/main.py`](file:///home/vsreddyh/Documents/Discord-bots/docker/health-api/main.py)) on port `:8001`, writing Health Connect metrics to MongoDB.
- **gateway**: Single multiplexed `hermes gateway run` container (`gateway.multiplex_profiles: true`) serving god + 2 sides from the official image (entrypoint renders templates with secret fail-fast, then execs the gateway directly — their s6 tree is bypassed).
- **proxy**: nginx single entrypoint (`:8080`, `docker/proxy/nginx.conf`) — routes `/p/*` → gateway chat, `/api/*` + `/health` → health sync. The app's one Server URL points here.
- **app API**: Hermes built-in OpenAI-compatible server (`platforms.api_server` in `gateway/config.yaml.template`) on `:8642` — the chat backend for the custom Android app (3 tabs, SSE streaming, `PASSWORD` single-password bearer auth). Direct port stays published; the app goes through the proxy.
- **playwright**: Browser automation via the official `@playwright/mcp` stdio server (headless chromium bundled in the bot image), configured per profile in `mcp_servers`.
- **retention**: One-shot retention job executing the `retention` Go binary (`cmd/retention/main.go`) via cron or on stack start.
- **Development Isolation**: all database operations go to the Atlas `MONGODB_URI` — point dev checkouts at a separate database to keep prod data untouched.

---

## LLM Connection (Direct Go)

All profiles connect directly to OpenCode Go (`https://opencode.ai/zen/go/v1`) using `OPENCODE_API_KEY` defined in the root `.env`.

- **Config Rendering**: Rendered as `api_key: ${OPENCODE_API_KEY}` in each profile's `config.yaml` from `config.yaml.template` by [`test/entrypoint.sh`](file:///home/vsreddyh/Documents/Discord-bots/test/entrypoint.sh).
- **Vision Model**: Auxiliary vision queries utilize `mimo-v2.5` natively over OpenCode Go.
- **Streaming Support**: Direct SSE passthrough when streaming is enabled in Hermes settings.

---

## Stack Lifecycle (Podman)

All container management is orchestrated through [`scripts/hermes.sh`](file:///home/vsreddyh/Documents/Discord-bots/scripts/hermes.sh), backed by [`docker/docker-compose.yml`](file:///home/vsreddyh/Documents/Discord-bots/docker/docker-compose.yml).

### `init`
1. Verifies host dependencies (podman, compose, python3, curl, cron) and installs missing requirements.
2. Builds the derived bot image ([`test/Dockerfile`](file:///home/vsreddyh/Documents/Discord-bots/test/Dockerfile): official hermes image + in-repo Go MCP binaries) and the `health-api` image.
3. Initializes root `.env` from `.env.example` if not already present.
4. Copies skill files from `skills/` into each profile directory.
5. Installs the daily data retention cron job (runs daily at 03:00).

### `start`
1. Cleans up any stale native PIDs in `run/bots/*.pid`.
2. Starts the compose stack in detached mode: `podman-compose -f docker/docker-compose.yml up -d --build`.
3. Runs an initial retention check using [`scripts/retention.sh`](file:///home/vsreddyh/Documents/Discord-bots/scripts/retention.sh).

### `stop`
Gracefully halts running containers: `podman-compose -f docker/docker-compose.yml down`.

### `restart`
Executes a stop followed by a full start sequence.

### `status`
Displays container states and published ports via `podman-compose -f docker/docker-compose.yml ps`.

### `clean` (Destructive)
Stops containers, wipes volumes (`down -v`), removes `run/`, clears rendered configs and per-profile `.env` files, and removes the retention crontab entry. **Never touches remote MongoDB.**

---

## Bot Profiles & Multiplexing

[`gateway/`](file:///home/vsreddyh/Documents/Discord-bots/gateway) IS the god profile — Hermes' built-in `default` profile is the gateway home itself (`HERMES_HOME=/opt/data`). Story and resumes are side profiles nested under `gateway/profiles/<bot>/`:

| Profile | App Tab | Workspace & Domain Data |
|---|---|---|
| `default` (god, main) | God | Money (`money_transactions`), cookbook (`cookbook_*`), health (`hc_meals`/`hc_days`/`hc_weight`) + Health Connect sync — lives at the gateway home itself |
| `story` (side) | Story | Lore vault in Git repo (`workspace/portals`, `vsreddyh/portals`) |
| `resumes` (side) | Resumes | LaTeX CV workspace in Git repo (`workspace/resumes`, `vsreddyh/Resume`) |

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
Bots and scripts interact with MongoDB using the `mongo` Go CLI (`cmd/mongo/main.go`, baked into the bot image at `/usr/local/bin/mongo`):

```bash
go run ./cmd/mongo count money_transactions '{"type":"expense"}'
go run ./cmd/mongo insert hc_weight '{"date":"2026-08-08","kg":63.2}'
go run ./cmd/mongo upsert hc_days '{"date":"2026-08-08"}' '{"steps":8452}'
```

---

## Data Retention & Lifecycle

Automated data pruning is executed by the `retention` Go binary (`cmd/retention/main.go`):

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
Agento Android App ──POST /api/health/sync──► proxy (:8080) ──► health-api (:8001)
                                                        │
                                                        ▼
                                       MongoDB: hc_days (one doc per date)
```

1. **Agento** (`android/agento/`): Built with Jetpack Compose & Health Connect SDK 1.1.0. Backfills 30 days on initial setup and runs hourly background syncs.
2. **`health-api` Endpoint** (`:8001`): Authenticates requests via `Authorization: Bearer <PASSWORD>` and upserts metrics into MongoDB.

> **Upgrading from Health Gateway?** Agento ships under a new `applicationId` (`com.vishnu.agento`), so it installs **alongside** the old Health Gateway app — settings do not transfer automatically.
> 1. Install Agento → re-enter the sync server URL/token and chat-backend Settings manually.
> 2. Re-grant Health Connect permissions in Agento (grants are per-package).
> 3. Confirm hourly syncs arrive, then **uninstall Health Gateway** to stop its worker and avoid double-syncs.

---

## Android App API (Chat)

The custom Android app (`android/agento/`, 3 chat tabs + Settings) uses ONE Server URL + Password — the proxy (`:8080`) — which routes chat to Hermes's built-in OpenAI-compatible API server on the gateway (`/p/* → :8642`, `PASSWORD` bearer auth) and sync to health-api (`/api/* → :8001`):

```bash
curl http://<host>:8080/p/story/v1/models -H "Authorization: Bearer <PASSWORD>"
```

Direct (bypassing the proxy):

```bash
curl http://<host>:8642/p/story/v1/models -H "Authorization: Bearer <PASSWORD>"
curl http://<host>:8642/p/story/v1/chat/completions \
  -H "Authorization: Bearer <PASSWORD>" -H "Content-Type: application/json" \
  -d '{"provider": "opencode-go", "model": "mimo-v2.5", "messages": [{"role": "user", "content": "hi"}], "stream": true}'
```

- One port for all tabs; each tab talks to its profile path (`/p/story`, `/p/resumes`, `/p/default` — overridable per tab in app Settings). **Verify live via `GET /p/<profile>/v1/models`**, the source of truth under multiplex.
- Provider + model are picked per tab in app Settings from live dropdowns backed by `GET /p/<profile>/api/model/options` (explicit selection required — no gateway default). The gateway's provider keys live ONLY in the git-ignored root `.env` on the VPS — never in git.
- Config lives in `gateway/config.yaml.template` (`platforms.api_server`, key rendered from `PASSWORD`); port published in `docker/docker-compose.yml` (`${API_SERVER_PORT:-8642}:8642`).

---

## Development Mode Isolation

There is no local MongoDB service — every environment (dev included) uses the Atlas `MONGODB_URI`. Isolate development by pointing `MONGODB_URI`/`MONGODB_DB` at a separate throwaway database.

---

## Configuration & Environment Reference

All settings are configured in the single root `.env` file:

| Variable | Required | Description |
|---|---|---|
| `OPENCODE_API_KEY` | **Yes** | API key for OpenCode Go (single provider `opencode-go`) |
| `MONGODB_URI` | **Yes** | Remote MongoDB connection string (used in prod) |
| `MONGODB_DB` | No | Target MongoDB database name (default: `hermes`) |
| `PASSWORD` | For Health + App | Single password for Agento Android chat + health-sync authentication |

---

## Security & Isolation

- **Secrets Management**: Live API keys and database credentials reside exclusively in the git-ignored `.env` file.
- **Container Isolation**: `gateway` container mounts only required directories (`gateway`, `workspace`, `/tools` read-only) with no host container-socket access (Podman is daemonless — there is no shared socket).

---

## Extending the Stack

### Adding a New Bot Profile
1. Create a plan in `profile-plans/<bot>-plan.md`.
2. Create profile directory `gateway/profiles/<bot>/` with `config.yaml.template`, `SOUL.md`, and skills.
3. Add the bot identifier to the `BOTS` array in `scripts/hermes.sh`.
4. Rebuild and restart the gateway container:
   ```bash
   ./scripts/hermes.sh restart
   ```

### Adding a Skill
Place the skill directory containing `SKILL.md` inside `skills/` and execute `./scripts/hermes.sh init` to distribute the skill to all bot profiles.
