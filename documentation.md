# Documentation

Deep dive into every component of the Agento stack. For a fast start, refer to [README.md](README.md).

## Table of Contents

1. [System Architecture](#system-architecture)
2. [Agent Surface (T3/opencode)](#agent-surface-t3opencode)
3. [Stack Lifecycle (Podman)](#stack-lifecycle-podman)
4. [Remote MongoDB & Storage Model](#remote-mongodb--storage-model)
5. [Data Retention & Lifecycle](#data-retention--lifecycle)
6. [Health Connect Pipeline](#health-connect-pipeline)
7. [Android App API](#android-app-api)
8. [Development Mode Isolation](#development-mode-isolation)
9. [Configuration & Environment Reference](#configuration--environment-reference)
10. [Security & Isolation](#security--isolation)
11. [Extending the Stack](#extending-the-stack)

---

## System Architecture

The stack runs a health/domain API, an nginx proxy and a scheduled retention job — **fully in containers**. Everything is defined in a single Compose file ([`docker/docker-compose.yml`](docker/docker-compose.yml)). The agent no longer runs in this stack: T3/opencode runs on the host (#494).

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

- **health-api**: Go service (`cmd/health-api/main.go`, net/http) on container port `:8000` (compose default host publish `:8001`, `HEALTH_HOST_PORT` remaps the host side). Serves sync, tasks, projects, files and ∃-Mail reports, and writes them to MongoDB.
- **proxy**: nginx single entrypoint (`:8080` compose default, `APP_PORT` remaps; `docker/proxy/nginx.conf`) — routes `/api/*` and `/health` → health-api. The app's one Server URL points here.
- **retention**: one-shot retention job executing the `retention` Go binary (`cmd/retention/main.go`) via cron or on stack start.
- **Development Isolation**: all database operations go to the Atlas `MONGODB_URI` — point dev checkouts at a separate database to keep prod data untouched.

---

## Agent Surface (T3/opencode)

The agent runs on the host, not in a container. T3 discovers `opencode.json` by walking up from its working directory, so **the repo root must be the cwd** — that is also what the `./bin/<server>` commands in `mcp.servers` resolve against.

- Build the servers with `./scripts/build-mcps.sh` (static `CGO_ENABLED=0` binaries into git-ignored `bin/`). Kept servers: `task-manager`, `project-manager`, `miser-money`, `cookbook`, `health-check`, `reports`.
- Secrets reach the servers through the process environment, never the committed file: each entry passes `{env:MONGODB_URI}` / `{env:MONGODB_DB}` through. Source those from the single root `.env` with the safe loader before launching T3 — `source scripts/lib/common.sh && load_root_env` (parsed, never `source .env`: a pasted Atlas URI carries `&` query params and secrets may hold `$`/backticks, which bare sourcing mangles or executes).
- No secret is written into git; `.env` stays git-ignored.
- The persona and shared skills live in `.opencode/chat.md` and `.opencode/skills/`; `opencode.json`'s `instructions` entry loads the former.

### LLM connection

T3/opencode talks directly to OpenCode Go (`https://opencode.ai/zen/go/v1`) using `OPENCODE_API_KEY` from the root `.env`. The provider and model are selected in T3; no proxy and no in-repo model config remain.

---

## Stack Lifecycle (Podman)

All container management is orchestrated through [`scripts/hermes.sh`](scripts/hermes.sh), backed by [`docker/docker-compose.yml`](docker/docker-compose.yml). Run the commands through the script, not the parenthesised spellings: the script's `podman_compose()` wrapper adds `--env-file .env -f docker/docker-compose.yml` from the repo root, and the bare `podman-compose` spellings below fail everywhere without those flags.

### `init`
1. Verifies host dependencies (podman, compose, python3, curl, cron) and installs missing requirements.
2. Builds the `health-api` and `retention` images.
3. Initializes root `.env` from `.env.example` if not already present.
4. Installs the daily data retention cron job (runs daily at 03:00) and the sysmon sampler.

### `start`
1. Starts the compose stack in detached mode, rebuilding changed layers and
   recreating containers: `podman-compose -f docker/docker-compose.yml up -d --build --force-recreate`
   (skipped build with `NO_BUILD=1`; `PI_NO_BUILD` / `HERMES_NO_BUILD` are honoured as legacy names). Without `--force-recreate` a rebuild silently keeps the OLD container running.
2. Runs an initial retention check using [`scripts/retention.sh`](scripts/retention.sh).

### `stop`
Gracefully halts running containers: `podman-compose -f docker/docker-compose.yml down`.

### `restart`
Rebuilds changed layers and recreates containers in place (`up -d --build
--force-recreate`) — not a stop-then-start (no `down`; containers are never
removed, just recreated).

### `status`
Displays container states and published ports via `podman-compose -f docker/docker-compose.yml ps`.

### `clean` (Destructive)
Stops containers, wipes volumes (`down -v`), removes `run/`, and removes the retention and sysmon crontab entries. **Never touches remote MongoDB** and never deletes a tracked file; the root `.env` is never touched either.

---

## Remote MongoDB & Storage Model

Domain data is managed in MongoDB (default database: `hermes`, configurable via `MONGODB_DB`):

| Collection | Associated server | Schema / Keys |
|---|---|---|
| `money_transactions` | Miser-money | `date` (YYYY-MM-DD), `amount` (float), `type` (income\|expense), `category` (normalized string), `note` (string) |
| `hc_meals` | Health-check | `date` (YYYY-MM-DD), `items[{name, qty?, kcal, protein, carbs, fat, fiber}]`, `totals` (computed), `createdAt` |
| `hc_days` | Health-check / Health Sync | `date` (YYYY-MM-DD, unique), `steps` (int), `active_kcal` (float), `sleep_hours` (float), `workouts[{type, minutes, kcal}]`, `updatedAt` |
| `hc_weight` | Health-check | `date` (YYYY-MM-DD, unique), `kg` (float) — **exempt from retention pruning** |
| `cookbook_ingredients` | Cookbook | `name` (unique), `note`, `createdAt` — **permanent** |
| `cookbook_recipes` | Cookbook | `name` (unique), `servings`, per-serving `kcal/protein_g/carbs_g/fat_g/fiber_g`, `quantities[{ingredient_id, name, qty}]` — **permanent** |
| `cookbook_cook_log` | Cookbook | `recipe_id`, `date`, `cooking_note`, `aftertaste_note` — **permanent** |
| `tasks` | Task-manager | `idempotency_key` (client key, UNIQUE+SPARSE — makes create idempotent, #176), `source` (app vs MCP), `name`, `description`, `due_date`, `due_time`, `estimated_minutes`, `repeat_rule` (verbatim) — no status field; `completedAt` null = open, done tasks TTL 3d via `expiresAt`; `skippedAt` + derived `skipped` on occurrences skipped via the skip verb (#287) — still read as "not done" |
| `tasks_changes` | Task-manager | Single document (`_id: "revision"`) holding the collection-wide change counter every task write advances — create, update, complete (plus its rollover successor), skip, reopen, delete — from **both** writers, because the marker is bumped by the store itself (#178). Served read-only as `GET /api/tasks/changes?since=`; a collection that has never been written reports revision 0. **Permanent** |
| `task_mutations` | Task-manager | The change log (#176): one row per mutation — `op`, `task_id`, `source`, `detail`, `at` — from **both** writers. Served read-only as `GET /api/tasks/mutations?task_id=&op=&since=&limit=` and as the `task_history` MCP tool (#350). It outlives its subject on purpose (done tasks are TTL-deleted after 3 days). Pruned at 90 days. Indexed `{task_id, at}` and `{at, _id}` |
| `projects` | Project-manager | `name`, `status` (Todo\|Ongoing\|Testing\|Blocked\|Paused\|Done), `note`, `createdAt`, `updatedAt` — **permanent**, shared with the app's Projects tab |
| `reports` | Reports (∃-Mail, #365) | `title`, `body_md` (markdown with `$..$`/`$$..$$` LaTeX, compiled server-side on read), `source`, `read`, `starred`, `createdAt` — **7-day conditional retention**: pruned when `createdAt < today - 7d AND read AND NOT starred` |
| `approvals`, `audit_log` | MCP audit (retiring in #503) | Pre-migration approval rows and the 90-day audit trail written by `internal/approvalgate` |

### Database Helper CLI
Operators use the `mongo` Go CLI (`cmd/mongo/main.go`) directly:

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
| `reports` (#365) | > 7 days, conditional | Pruned when `createdAt < today - 7d AND read AND NOT starred` — unread/starred never pruned |
| `task_mutations` | > 90 days | Pruned when `at < today - 90d` — the task audit log (#176) |
| `tasks_changes` | Permanent | **Never pruned** — one single-document counter, not a log (#178) |
| `tasks` (repeats) | n/a | **Reconciled, not pruned**: completed structured repeats with no successor are reported (#180) |
| `approvals`, `audit_log` | TTL | 15-minute TTL on `approvals`, 90-day on `audit_log`; removed with #503 |

Run manual dry-runs via:
```bash
./scripts/retention.sh --dry-run
```

### Operator-set, not app settings (#355)

The retention windows above and the agent's personality and instructions
(`.opencode/chat.md`, `.opencode/skills/`) are operator-set by design: they live
in the repo / deployment, not in the app. The app's Settings → Behaviour section
owns the device-local knobs (sync cadence, reminder lead time, notifications) —
everything with an obvious default that the device can apply itself.

---

## Health Connect Pipeline

```
Agento Android App ──POST /api/health/sync──► proxy (:8080 compose default) ──► health-api (:8000 container; :8001 direct-host default)
                                                        │
                                                        ▼
                                       MongoDB: hc_days (one doc per date)
```

1. **Agento** (`android/agento/`): Built with Jetpack Compose & Health Connect SDK 1.1.0. Backfills 30 days on initial setup and runs hourly background syncs.
2. **`health-api` Endpoint** (compose default `:8001`, `HEALTH_HOST_PORT` remaps the host side): Authenticates requests via `Authorization: Bearer <PASSWORD>` and upserts metrics into MongoDB.

> **Upgrading from Health Gateway?** Agento ships under a new `applicationId` (`com.vishnu.agento`), so it installs **alongside** the old Health Gateway app — settings do not transfer automatically.
> 1. Install Agento → re-enter the sync server URL/password by hand.
> 2. Re-grant Health Connect permissions in Agento (grants are per-package).
> 3. Confirm hourly syncs arrive, then **uninstall Health Gateway** to stop its worker and avoid double-syncs.

---

## Android App API

The custom Android app (`android/agento/`) uses ONE Server URL + Password — the proxy (`:8080` compose default, `APP_PORT` remaps) — which routes `/api/*` and `/health` to health-api (`:8000` container, `:8001` direct-host default). Every domain route is bearer-authenticated with `PASSWORD`, except `GET /health` (unauthenticated liveness).

| Route | Purpose |
|---|---|
| `POST /api/health/sync` | Health Connect batch upsert |
| `/api/tasks`, `/api/tasks/batch`, `/api/tasks/mutations`, `/api/tasks/changes` | Task CRUD, batch, audit log and change counter |
| `/api/projects` | Project board CRUD |
| `/api/files`, `/api/files/upload`, `/api/files/download` | The Files tab shelf over `exports/` |
| `/api/reports` | ∃-Mail inbox (markdown + LaTeX) |
| `/health`, `/health/` | Liveness probe (unauthenticated) |

```bash
curl http://<host>:8080/api/tasks/changes -H "Authorization: Bearer <PASSWORD>"
```

---

## Development Mode Isolation

There is no local MongoDB service — every environment (dev included) uses the Atlas `MONGODB_URI`. Isolate development by pointing `MONGODB_URI`/`MONGODB_DB` at a separate throwaway database.

---

## Configuration & Environment Reference

All settings are configured in the single root `.env` file:

| Variable | Required | Description |
|---|---|---|
| `OPENCODE_API_KEY` | For the agent | API key for OpenCode Go, read by T3/opencode on the host |
| `MONGODB_URI` | **Yes** | Remote MongoDB connection string |
| `MONGODB_DB` | No | Target MongoDB database name (default: `hermes`) |
| `PASSWORD` | **Yes** | Single password for the Android app's bearer authentication |

Every `PI_*` variable and `VPS_SSH_DIR` was removed with the agent container in #494.

---

## Security & Isolation

- **Secrets Management**: Live API keys and database credentials reside exclusively in the git-ignored `.env` file.
- **Container Isolation**: `health-api` mounts only `exports/` (writable — the Files upload stages and renames there, jailed by the handler which never replaces an existing file) and the docker socket is not mounted anywhere (Podman is daemonless — there is no shared socket). No container runs the agent; T3/opencode runs as the invoking host user.
- **Single credential, one principal**: the same `PASSWORD` bearer authenticates every `health-api` route except the unauthenticated `GET /health` liveness, so domain writes from the app and the agent share one principal. Per-user auth arrives only with per-user auth, not before.

### Approval prompts + audit (#313, #487, #503)

Sensitive tool calls are approved by the T3/opencode permission prompts configured in `opencode.json` (the old in-app approval queue is gone). Each kept MCP server still wraps its tool registrations with `internal/approvalgate`, which runs the call at once and attempts one best-effort audit row (tool, redacted args, disposition, result; no decider) in the `audit_log` collection. A missing row is an outage gap, not proof nothing ran.

- Money writes, every `delete_*`/`archive_*`, destructive rewrites (`fix_*`), prunes, and account create/archive prompt first; reads and routine writes run free. (The harness cannot see arguments, so even a counting prune with `dry_run=true` prompts.)
- Arg keys are redacted by substring and error text is pattern-scrubbed (300-rune cap), so treat `error` as untrusted display text.
- The audit wrapper, its policy tables and the `approvals`/`audit_log` collections are scheduled for pure removal in #503.

---

## Extending the Stack

### Adding an MCP server
1. Add `cmd/<name>/` and `internal/<name>/` (stdio MCP server, Mongo-backed).
2. Add the server to `SERVERS` in `scripts/build-mcps.sh` and to `mcp.servers` in `opencode.json` with the Mongo env passthrough.
3. Run `./scripts/build-mcps.sh`, then confirm with `opencode mcp list` from the repo root.

### Adding a skill
Put the skill directory containing `SKILL.md` under `.opencode/skills/<name>/` and commit it. T3/opencode discovers that tree; `opencode.json` only needs the `instructions` entry that loads the persona.
