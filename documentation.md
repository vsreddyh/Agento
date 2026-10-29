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
- **pi**: The agent host (`docker/pi/Dockerfile`, entrypoint `docker/pi/entrypoint.sh`). One `pi` process for the single chat profile with that profile's directory as its working directory, which is how it loads its own `AGENTS.md`; `pi-gateway` serves the OpenAI-compatible surface in front of it. The entrypoint refuses to start if any of the ten MCP servers fails to connect.
- **proxy**: nginx single entrypoint (`:8080`, `docker/proxy/nginx.conf`) — routes `/p/*` → pi chat, `/api/*` + `/health` → health sync. The app's one Server URL points here.
- **app API**: `pi-gateway` (`cmd/pi-gateway`) on `:8643` — the chat backend for the custom Android app (Chat tab, SSE streaming, `PASSWORD` single-password bearer auth). Direct port stays published; the app goes through the proxy.
- **browser**: Read-only web tasks via a headless-Chromium MCP server
  (`browser_goto` gated per domain, `browser_read`, `browser_screenshot`,
  `browser_save_evidence`); evidence lands in `exports/browser/`. No click/type
  tools by design — logins are impossible and passwords never travel.
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
- The pi entrypoint (`docker/pi/entrypoint.sh`) registers the bind-mounted repos with git, gates startup on `pi mcp list` connecting all ten MCP servers, then execs `pi-gateway`. There is no config rendering: each profile's `AGENTS.md` is loaded because its directory is that process's working directory.
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
| `task_mutations` | Task-manager | The change log (#176): one row per mutation — `op` (create\|update\|delete\|complete\|skip\|reopen\|duplicate — `duplicate` appears only for batch duplicates), `task_id`, `source` (which caller, self-attested), `detail` (changed fields / rollover outcome), `at` — from **both** writers, since the agent and the app share one password and one collection. Served read-only as `GET /api/tasks/mutations?task_id=&op=&since=&limit=` and as the `task_history` MCP tool (#350). It outlives its subject on purpose: a completed task is TTL-deleted after 3 days, so the log is the only record left of it. Pruned at 90 days. Indexed for the two reads that matter: `{task_id, at}` (one task's history, newest first) and `{at, _id}` (the whole log, newest first), plus the prefixed singles, kept only because removing an index is a migration |
| `projects` | Project-manager | `name`, `status` (Todo\|Ongoing\|Testing\|Blocked\|Paused\|Done), `note`, `createdAt`, `updatedAt` — **permanent**, shared with the app's Projects tab |
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
| `connectors` (#316) | Permanent | **Never pruned** — 2 fixed rows, no TTL on `connectors` itself (grants live until disconnect/revoke; `expired` retains the refresh token for renewal and only bars data reads — disconnect is what clears tokens; the separate `oauth_states` collection carries its own 10-minute TTL) |

Run manual dry-runs via:
```bash
./scripts/retention.sh --dry-run
```

### Operator-set, not app settings (#355)

The retention windows above, the gateway timeouts (`PI_TURN_TIMEOUT`,
`PI_INVENTORY_TIMEOUT` in `docker/docker-compose.yml`) and the agent's
personality and instructions (`pi/profiles/<name>/AGENTS.md`) are
operator-set by design: they live in the repo / deployment, not in the app.
The app's Settings → Behaviour section owns the device-local knobs (sync
cadence, reminder lead time, TTS, notifications) — everything with an
obvious default that the device can apply itself.

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

The custom Android app (`android/agento/`, sidebar: Chat + Tasks + Files + Reminders + Settings) uses ONE Server URL + Password — the proxy (`:8080`) — which routes chat to the pi-gateway (`/p/* → pi :8643`, `PASSWORD` bearer auth) and sync to health-api (`/api/* → :8001`). There is no Hermes gateway any more — `:8642` is neither published nor proxied:

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
- Provider + model are picked from the Chat tab's own model sheet (top-bar button) backed by live dropdowns from `GET /p/chat/api/model/options` (the app always selects explicitly; the gateway falls back to the running model when none is sent). The provider key lives ONLY in the git-ignored root `.env` on the VPS — never in git.
- Chat history is threaded and persisted on-device (survives restarts; the switcher revisits/deletes threads). Tasks, reminders, and threads are included in Settings backup exports — large photo backups warn past ~5 MB on disk and refuse past ~12 MB on disk (the cap is a ~25 MB heap peak: the export string runs about twice its size in memory) — delete old photo threads in the switcher to shrink.
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
reasoning, tools, usage, status) and then live frames — that stream is the
progress path. The detail route keeps the full text but is a point read, not a
watch: poll the list for status, watch `/events` for progress, read detail for
the record — one detail read can serve up to 256 KiB, so never poll it per
second for progress. One turn executes per
profile; a different prompt while busy returns `429` with `X-Agento-Run-Id`, and
resending the same prompt on the same conversation reattaches instead. The list
shows previews (2 KB output, 1 KB reasoning — marked once the run settles,
unmarked heads while it is still running);
`limit` defaults to 100,
is capped at 100, and `limit=0` — or an absent or empty `limit` — means the
default, not an empty list. Only a present non-numeric or negative value 400s. The
detail route keeps the full stored text (capped). State is
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
| `GOOGLE_CLIENT_ID` | For Connectors | OAuth client ID for Gmail/Calendar grants (public in the consent URL by design) |
| `GOOGLE_CLIENT_SECRET` | For Connectors | OAuth client secret (VPS-only, never committed) |
| `GOOGLE_REDIRECT_URL` | For Connectors | Exact registered callback URL (must match the console spelling byte-for-byte) |
| `CONNECTORS_ENCRYPTION_KEY` | For Connectors | 64-hex-char envelope key for OAuth tokens at rest (`openssl rand -hex 32`) |

---

## Security & Isolation

- **Secrets Management**: Live API keys and database credentials reside exclusively in the git-ignored `.env` file.
- **Container Isolation**: the `pi` container mounts only what it needs (`pi/`, `workspace/`, three read-only SSH files) with no host container-socket access (Podman is daemonless — there is no shared socket). The agent runs as root inside it, because the bind-mounted workspace is owned by the host uid.
- **Single credential, one principal**: the same `PASSWORD` bearer authenticates chat (`pi-gateway`) and health sync, so every approval decision is attributed as `decided_by: "app"` — read it as "decided from the app", not a per-user id. The 90-day audit row records only a hash of IP + User-Agent per deciding device, never the values themselves. Per-user auth (and per-user attribution) arrives with per-user auth, not before.

### Approval gates + audit log (#313)

Sensitive tool calls pause for an explicit in-app approve/deny before they
execute. Enforcement lives in `cmd/mcp-gate`, the relay every `pi/mcp.json`
entry points through: reads and routine writes forward untouched, while money
writes, every `delete_*`/`archive_*`, destructive rewrites (`fix_*`) and
prunes that actually delete (`prune_old` with `dry_run=false`) hold up to 14
minutes for a decision, then run on approve or fail closed on deny/expiry —
Pi gets a tool error and the turn continues. Unknown tools and unknown
servers hold too (deny-by-default). Every proxied `tools/call` writes one
audit row with tool, redacted args, disposition (`allowed`/`approved`/
`denied`/`expired`), result and decider; arg keys are redacted by substring
and error text is pattern-scrubbed (300-rune cap), so treat `error` as
untrusted display text. Pending queue: `GET /api/approvals` (pass
`?status=pending`), decide with `POST /api/approvals/{id}/approve|deny`
(exactly once — races after expiry 409); history: `GET /api/audit`
(`?server=&tool=`). State is in Mongo (`approvals`, 15-minute TTL;
`audit_log`, 90-day TTL) and survives restarts; without Mongo both sides
fall back to memory that cannot meet — the relay denies gated calls
immediately (`relay_unavailable`) and the app banners the queue as degraded
instead of showing an undecidable list. The app surface (since 5.7.0) is the
**Approvals** sheet: pending cards with redacted args + approve/deny, a
badged entry with an in-turn banner while streaming, and the recent audit
trail below. Full design: `docs/adr/0002-approval-gates.md`.

Threat note — what this does and does not fix:

- Fixed: unattended money writes, deletes, prunes and rewrites now need a
  human; every proxied call is attributable for 90 days; deny leaves state
  unchanged (Pi gets a tool error, the turn continues).
- NOT fixed: Pi built-in tools (`bash` file writes, `read`/`write`/`edit`
  via codemode) never traverse MCP, so they are observed but not gated — a
  model told to `rm -rf` via bash is unimpeded. The agent still runs as root
  in the container, and the single bearer token still authenticates every
  route. Full sandboxing and per-connector OAuth vaults are out of scope
  (connectors own the vault).
- Residual: a held call occupies its turn until expiry; an approval shows
  tool + args + time but not which conversation asked.

### First connectors — Gmail, Calendar (#316, OAuth grants + MCP servers)

Two connectors with explicit read vs draft/write scopes, each
connecting/disconnecting independently from the app:

- Gmail: `search_gmail` / `read_gmail` / `list_messages` + `create_draft`
  (sends refused with "create a draft instead").
- Calendar: `list_events` / `get_event` + `create_event_draft` (an
  agent-side draft, sent nowhere; creates/invites refused the same way).

Connect flow: `POST /api/connectors/{id}/connect` returns an `auth_url`
(PKCE S256 + offline access + per-connector least-privilege scopes) for the
app to open in a browser; Google redirects to the single registered
`GET /api/connectors/callback?code=&state=` — one registered string with
two path shapes (bare direct, `/p/` via proxy); non-default ports are
variants of whichever path they attach to (default ports are not: `:443`
and `:80` are stripped by canonicalisation, so register the stripped form
— see `.env.example`). Product constraint, not just validation:
registrations must be public spellings (dotted FQDN or loopback) — LAN,
mDNS, and non-loopback-IP hosts fail closed here by design, because no
public Google console entry can hold them. Homelab setups must serve (and
register) a dotted name or loopback address: use a nip.io-style wildcard,
Tailscale funnel, or reverse proxy for a public name — or an SSH tunnel to
localhost for direct-only access. Bare `*.local`/LAN names are out of scope
by intent, not by omission. The bare spelling works direct on the pi
port and the `/p/` spelling through nginx, which strips `/p` before
forwarding (port included in the match — the `:8443` spelling and the
no-port spelling are different registrations). The callback bypasses
profile routing entirely, so any first segment serves — bare, `/p/`, or
`/p/<anything>/` — direct or proxied, while every other verb 404s a
miscased or cross-profile path. The server consumes the
single-use state, exchanges the code, seals both tokens with the envelope
key (AES-256-GCM, `CONNECTORS_ENCRYPTION_KEY`), and flips the row to
connected. Tokens land before the status flips — a connected-with-no-tokens
row is never servable. Without full OAuth config (client id/secret,
redirect URL, envelope key, durable store) connect answers 501.

The refusal is enforced in the relay before the approval check, audited as
`connector_draft_only`, and parks no approval card. Token placement: OAuth
refresh tokens live **server-side in Mongo** (`connectors` collection,
envelope-encrypted), never in the app and never pasted in chat — the API
returns status + scopes only, and disconnect clears the tokens (`$unset`
of the token columns on Mongo, whole-row delete on memory).
Already-fetched data (chat history) is not wiped on
disconnect; disconnect stops future access. Without Mongo the list still
answers from memory but nothing survives a restart and no token can be
stored. An expired row carries a `reconnect` hint instead of asking for a
secret.
Wire shape note: a row with no `updated_at` was never touched; an explicit
disconnect sets it. Same status, different shape — the difference is the
signal (explicit-off vs never-touched), so a future shape-normalisation
must not erase it. Caveat: a token write landing without a status write
(crash between the two writes) also carries `updated_at` — that row reads
as explicit-off but means interrupted setup, not an operator action; only
the disconnect path carries real human intent. Both backends backfill
display + `updated_at` on `SetTokens` (Mongo `$setOnInsert`, memory
display-default row), so the crash-window shape agrees on both. The
backfill is first-write-only (`updated_at` stays the first touch when
tokens rotate later): token freshness is not timestamped, so triage
reads the stamp as "touched at least once", never "touched now". `last_error` appears only on `expired` rows: connected
and disconnected rows never carry it (enforced on write, repaired on read).
The reconnect signal is `status` + the `reconnect` hint, not `last_error`:
`SetStatus(expired, "")` is a legal write, so an expired row can carry the
hint with no error text — the app must branch on the hint, and a
`last_error`-less expired row is mid-reconnect, not healthy. `connected_at`
appears only on `connected` rows with a valid stamp (a connected row without
one omits it fail-closed); the stored value is first-connected-at, retained
across disconnect/expire.
The MCP servers (`cmd/gmail`, `cmd/calendar`, wired in `pi/mcp.json`)
call the official Google APIs with the stored grant: decrypt, refresh on
expiry, seal the new pair back (lazy refresh-on-use, no background
worker). No grant means every tool that calls Google answers reconnect
guidance, never a secrets prompt. `create_event_draft` is the exception:
purely local, so it works grant-less — the draft is exactly what the
user needs while disconnected. It builds a validated draft the agent
presents to the user — actual creation waits for the approval-gate flow
that covers calendar writes.
Operator setup on a host with no domain: `scripts/connectors-google-setup.sh`
is an interactive wizard that walks the two things only a human can do —
joining a tailnet and creating the Google client — and derives the rest.
It publishes the callback over Tailscale Funnel (public TLS on 443 →
`http://127.0.0.1:8080`, the same nginx proxy), because Google accepts only
`https://` or a loopback `http://` and a bare-IP host can register neither.
Note the funnel publishes the whole proxy (chat + sync stay PASSWORD-gated);
the callback is the only new unauthenticated surface, so do not share the
`*.ts.net` URL.
It then writes `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`,
`GOOGLE_REDIRECT_URL`, and `CONNECTORS_ENCRYPTION_KEY` into the root
`.env` (or `$ENV_FILE` when overridden for a dry run) and re-renders
compose to prove they land in the rendered compose config. It never
restarts the stack — the values are inert until the pi container is
recreated (./scripts/hermes.sh restart recreates it; a plain container
restart without recreation will not pick up .env changes). It is safe
to re-run: existing values stay defaults, and an existing
envelope key is kept, since rotating it invalidates every stored grant.
Register the `/p/` spelling it prints (through the proxy `/p` is stripped
before pi sees it); the bare `https://<host>/api/...` form routes to
health-api instead.

Full design: `docs/adr/0009-connectors.md`, servers: `docs/adr/0010-connector-mcp-servers.md`.

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
