# Documentation

Deep dive into every component of `opencode-remote`. For a fast start, refer to [README.md](README.md).

## Table of Contents

1. [System Architecture](#system-architecture)
2. [LLM Connection (Direct Go)](#llm-connection-direct-go)
3. [Stack Lifecycle (Podman)](#stack-lifecycle-podman)
4. [Bot Profiles](#bot-profiles)
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

The stack runs one chat profile, a health sync API, and a scheduled retention job — **fully in containers**. Everything is defined in a single Compose file ([`docker/docker-compose.yml`](docker/docker-compose.yml)).

```
                               Remote MongoDB (money, health, cookbook)
                                            ▲
                             Containers
  chat ───  ONE pi host   │  agent dir   ▼   OpenCode Go Direct
          1 pi process    │   /opt/pi     │  (https://opencode.ai/zen/go/v1)
                          └─ API :8643 ─┤
Agento (Android) ──► proxy (:8080) ──┬──► /p/* ──► pi ──► MongoDB
                                     └──► /api/* ─► health-api ──► MongoDB
Retention ───────────────► one-shot container (cron 03:00 / on start)
```

- **LLM Connection**: Direct HTTPS communication with OpenCode Go (`https://opencode.ai/zen/go/v1`, default model `mimo-v2.6-flash`).
- **health-api**: Go sync endpoint (`cmd/health-api/main.go`, net/http) on port `:8001`, writing Health Connect metrics to MongoDB.
- **pi**: The agent host (`docker/pi/Dockerfile`, entrypoint `docker/pi/entrypoint.sh`). One `pi` process for the single chat profile with that profile's directory as its working directory, which is how it loads its own `AGENTS.md`; `pi-gateway` serves the OpenAI-compatible surface in front of it. The entrypoint refuses to start if any of the five MCP servers fails to connect.
- **proxy**: nginx single entrypoint (`:8080`, `docker/proxy/nginx.conf`) — routes `/p/*` → pi chat, `/api/*` + `/health` → health sync. The app's one Server URL points here.
- **app API**: `pi-gateway` (`cmd/pi-gateway`) on `:8643` — the chat backend for the custom Android app (Chat tab, SSE streaming, `PASSWORD` single-password bearer auth). Direct port stays published; the app goes through the proxy.
- **browser**: Web automation via the native built-in browser toolset (`browser_*` tools); no MCP server needed.
- **retention**: One-shot retention job executing the `retention` Go binary (`cmd/retention/main.go`) via cron or on stack start.
- **Development Isolation**: all database operations go to the Atlas `MONGODB_URI` — point dev checkouts at a separate database to keep prod data untouched.

---

## LLM Connection (Direct Go)

All profiles connect directly to OpenCode Go (`https://opencode.ai/zen/go/v1`) using `OPENCODE_API_KEY` defined in the root `.env`.

- **Config**: each profile's own `pi/profiles/<name>/AGENTS.md`, loaded because the profile directory is that process's working directory. There is no template rendering and no secret file to render: the provider key arrives as an environment variable.
- **Vision Model**: Auxiliary vision queries utilize `mimo-v2.6-flash` natively over OpenCode Go.
- **Streaming Support**: Direct SSE passthrough when the request sets `stream: true` (the app always does).

---

## Stack Lifecycle (Podman)

All container management is orchestrated through [`scripts/hermes.sh`](scripts/hermes.sh), backed by [`docker/docker-compose.yml`](docker/docker-compose.yml).

### `init`
1. Verifies host dependencies (podman, compose, python3, curl, cron) and installs missing requirements.
2. Builds the agent image ([`docker/pi/Dockerfile`](docker/pi/Dockerfile): Node + Pi + the in-repo Go MCP binaries) and the `health-api` image.
3. Initializes root `.env` from `.env.example` if not already present.
4. Reports the skills it finds under `pi/skills/` and each profile's own `skills/`, and warns if there are none. Nothing is copied: both locations are tracked in git and bind-mounted into the container.
5. Installs the daily data retention cron job (runs daily at 03:00).

### `start`
1. Cleans up any stale native PIDs in `run/bots/*.pid`.
2. Starts the compose stack in detached mode: `podman-compose -f docker/docker-compose.yml up -d --build`.
3. Runs an initial retention check using [`scripts/retention.sh`](scripts/retention.sh).

### `stop`
Gracefully halts running containers: `podman-compose -f docker/docker-compose.yml down`.

### `restart`
Executes a stop followed by a full start sequence.

### `status`
Displays container states and published ports via `podman-compose -f docker/docker-compose.yml ps`.

### `clean` (Destructive)
Stops containers, wipes volumes (`down -v`), removes `run/`, clears rendered configs and per-profile `.env` files, and removes the retention crontab entry. **Never touches remote MongoDB.**

---

## Bot Profile

One profile directory under [`pi/profiles/`](pi/profiles) — `chat`. It runs as
its own `pi` process with that directory as its working directory, which is how
Pi loads its `AGENTS.md`, so a profile *is* a directory rather than a runtime
option. `pi-gateway` routes `/p/<profile>/…` to it (`PI_DEFAULT_PROFILE=chat`):

| Profile | App Tab | Workspace & Domain Data |
|---|---|---|
| `chat` | Chat | Money (`money_transactions`), cookbook (`cookbook_*`), health (`hc_meals`/`hc_days`/`hc_weight`) + Health Connect sync — `pi/profiles/chat/` |

(Retired in #289: the `story` and `resumes` side profiles — lore vault
`workspace/portals` and LaTeX workspace `workspace/portfolio-resume` stay on
disk, orphaned and unmanaged.)

### Environment & Token Injection
- All tokens and channel IDs reside in the root `.env`.
- The pi entrypoint (`docker/pi/entrypoint.sh`) registers the bind-mounted repos with git, gates startup on `pi mcp list` connecting all five MCP servers, then execs `pi-gateway`. There is no config rendering: each profile's `AGENTS.md` is loaded because its directory is that process's working directory.
- This ensures discrete credential scoping without mixing secrets across bot instances.

---

## Remote MongoDB & Storage Model

Domain data for `money`, `health-check`, `cookbook`, `task-manager`, `project-manager`, and `notes` is managed in MongoDB (default database: `hermes`, configurable via `MONGODB_DB`):

| Collection | Associated Bot | Schema / Keys |
|---|---|---|
| `money_transactions` | Money | `date` (YYYY-MM-DD), `amount` (float), `type` (income\|expense), `category` (normalized string), `note` (string) |
| `hc_meals` | Health-check | `date` (YYYY-MM-DD), `items[{name, qty?, kcal, protein, carbs, fat, fiber}]`, `totals` (computed), `createdAt` |
| `hc_days` | Health-check / Health Sync | `date` (YYYY-MM-DD, unique), `steps` (int), `active_kcal` (float), `sleep_hours` (float), `workouts[{type, minutes, kcal}]`, `updatedAt` |
| `hc_weight` | Health-check | `date` (YYYY-MM-DD, unique), `kg` (float) — **exempt from retention pruning** |
| `cookbook_ingredients` | Cookbook | `name` (unique), `note`, `createdAt` — **permanent** |
| `cookbook_recipes` | Cookbook | `name` (unique), `servings`, per-serving `kcal/protein_g/carbs_g/fat_g/fiber_g`, `quantities[{ingredient_id, name, qty}]` — **permanent** |
| `cookbook_cook_log` | Cookbook | `recipe_id`, `date`, `cooking_note`, `aftertaste_note` — **permanent** |
| `tasks` | Task-manager | `idempotency_key` (client key, UNIQUE+SPARSE — makes create idempotent, #176), `source` (app vs MCP), `name`, `description`, `due_date`, `due_time`, `estimated_minutes`, `repeat_rule` (verbatim) — no status field; `completedAt` null = open, done tasks TTL 3d via `expiresAt`; `skippedAt` + derived `skipped` on occurrences skipped via the skip verb (#287) — still read as "not done" |
| `tasks_changes` | Task-manager | Single document (`_id: "revision"`) holding the collection-wide change counter every task write advances — create, update, complete (plus its rollover successor), skip, reopen, delete — from **both** writers, because the marker is bumped by the store itself, not by either transport (#178). Served read-only as `GET /api/tasks/changes?since=`; a collection that has never been written reports revision 0. **Permanent** — one tiny document, nothing to prune |
| `projects` | Project-manager | `name`, `status` (Todo\|Ongoing\|Paused\|Done), `note`, `createdAt`, `updatedAt` — **permanent**, shared with the app's Projects tab |
| `notes` | Notes | `title` (NOT unique — notes are addressed by id), `body` (markdown, one list item per line), `tags[]` (lowercased), `pinned` (bool), `createdAt`, `updatedAt` — **permanent**, shared across profiles |

### Database Helper CLI
Bots and scripts interact with MongoDB using the `mongo` Go CLI (`cmd/mongo/main.go`). It is **not** in the pi image — no skill invokes it, and agents reach MongoDB through the MCP servers rather than a shell — so it is built by hand when an operator needs it:

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
| `notes` | Permanent | **Never pruned** |
| `task_mutations` | > 90 days | Pruned when `at < today - 90d` — the task audit log (#176) |
| `tasks_changes` | Permanent | **Never pruned** — one single-document counter, not a log (#178) |
| `tasks` (repeats) | n/a | **Reconciled, not pruned**: completed structured repeats with no successor are reported (#180) |
| `story` / `resumes` (retired #289) | Git history | No database retention operations |

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

The custom Android app (`android/agento/`, sidebar: Chat + Tasks + Storage + Reminders + Settings) uses ONE Server URL + Password — the proxy (`:8080`) — which routes chat to the pi-gateway (`/p/* → pi :8643`, `PASSWORD` bearer auth) and sync to health-api (`/api/* → :8001`). There is no Hermes gateway any more — `:8642` is neither published nor proxied:

```bash
curl http://<host>:8080/p/chat/v1/models -H "Authorization: Bearer <PASSWORD>"
```

Direct (bypassing the proxy):

```bash
curl http://<host>:8643/p/chat/v1/models -H "Authorization: Bearer <PASSWORD>"
curl http://<host>:8643/p/chat/v1/chat/completions \
  -H "Authorization: Bearer <PASSWORD>" -H "Content-Type: application/json" \
  -d '{"provider": "opencode-go", "model": "mimo-v2.6-flash", "messages": [{"role": "user", "content": "hi"}], "stream": true}'
```

- One port, one profile path (`/p/chat`). **Verify live via `GET /p/chat/v1/models`**, the source of truth for which models the profile can actually switch to.
- Provider + model are picked from the Chat tab's own model sheet (top-bar button) backed by live dropdowns from `GET /p/chat/api/model/options` (explicit selection required — pi-gateway has no default). The provider key lives ONLY in the git-ignored root `.env` on the VPS — never in git.
- Chat history is threaded and persisted on-device (survives restarts; the switcher revisits/deletes threads). Tasks, reminders, and threads are included in Settings backup exports.
- Config lives in the `pi` service (`docker/docker-compose.yml`): the bearer token is `PASSWORD` and the bind port is `PI_SERVER_PORT` (`8643`), published as `${PI_HOST_PORT:-8643}:8643`. Through the proxy the same `PASSWORD` authenticates, which is why the cutover needed no app change.

### Resumable runs (#311)

A chat turn is a **run**, not the lifetime of the HTTP request. The executor
holds the profile lock and a background context; the request only subscribes.
Closing the app no longer cancels the work, and a reconnecting client picks up
where it left off:

```bash
# the app uses its profile path, so the proxy routes this to pi-gateway
curl http://<host>:8080/p/chat/api/runs?conversation_id=<id> \
  -H "Authorization: Bearer <PASSWORD>"
curl -N http://<host>:8080/p/chat/api/runs/<run-id>/events \
  -H "Authorization: Bearer <PASSWORD>"
curl -X POST http://<host>:8080/p/chat/api/runs/<run-id>/cancel \
  -H "Authorization: Bearer <PASSWORD>"
```

The events stream starts with one `{"agento_run":{...}}` snapshot (full output,
reasoning, tools, usage, status) and then live frames. One turn executes per
profile; a different prompt while busy returns `429` with `X-Agento-Run-Id`, and
resending the same prompt on the same conversation reattaches instead. State is
in the `agent_runs` Mongo collection with a 7-day TTL; a gateway restart cannot
resume execution and marks stale runs `interrupted`. See
`docs/adr/0001-resumable-runs.md`.

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
- **Container Isolation**: the `pi` container mounts only what it needs (`pi/`, `workspace/`, three read-only SSH files) with no host container-socket access (Podman is daemonless — there is no shared socket). The agent runs as root inside it, because the bind-mounted workspace is owned by the host uid.

---

## Extending the Stack

### Adding a New Bot Profile
Single-profile since #289 — the stack serves `chat` only. To add a profile back:
1. Create the profile directory `pi/profiles/<name>/` with its `AGENTS.md` and `skills/`.
2. Add the profile name to the `PROFILES` array in `scripts/hermes.sh`.
3. Rebuild and restart the pi container:
   ```bash
   ./scripts/hermes.sh restart
   ```

### Adding a Skill
Put the skill directory containing `SKILL.md` in one of two places, and commit it:

- `pi/skills/<name>/` — shared by every profile. The agent directory is a discovered
  skill location, so no further step is needed.
- `pi/profiles/<name>/skills/<name>/` — that profile only. Pi does **not** discover
  skills from a profile's working directory (measured; see `pi/README.md`), so
  pi-gateway passes it explicitly with `--skill`.

`./scripts/hermes.sh init` only reports what it finds; there is no distribution step to
run, and nothing to re-run after editing a skill.
