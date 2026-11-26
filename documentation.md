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

<!-- #584 TEMP-REMOVE (∃-Mail): architecture diagram below omits the reports MCP; restore `,reports`. -->
```
                                        Remote MongoDB Atlas
                                                  ▲
                                                  │
  Agento (Android) ──► proxy (:8080 compose default) ──► /api/* ──► health-api ──┘
                                                          /health ──┘
  T3/opencode (host) ──► scripts/mcp-run.sh <server> ──► bin/{task-manager,
                              project-manager,miser-money,cookbook,
                              health-check} ──► MongoDB Atlas
                       └─► scripts/workspace-mcp.sh (venv, Gmail: read + labels)

  Both launchers read the root .env themselves (T3 supplies no configuration
  of its own, though an inherited shell variable still passes through).

  Retention ─────────────► one-shot container (cron 03:00 / on start)
```

- **health-api**: Go service (`cmd/health-api/main.go`, net/http) on container port `:8000` (compose default host publish `:8001`, `HEALTH_HOST_PORT` remaps the host side). Serves sync, tasks, projects and files<!-- #584 TEMP-REMOVE (∃-Mail): , and ∃-Mail reports -->, and writes them to MongoDB.
- **proxy**: nginx single entrypoint (`:8080` compose default, `APP_PORT` remaps; `docker/proxy/nginx.conf`) — routes `/api/*` and `/health` → health-api. The app's one Server URL points here.
- **retention**: one-shot retention job executing the `retention` Go binary (`cmd/retention/main.go`) via cron or on stack start.
- **Development Isolation**: all database operations go to the Atlas `MONGODB_URI` — point dev checkouts at a separate database to keep prod data untouched (see [Development Mode Isolation](#development-mode-isolation) for which of an exported variable or `.env` wins, per entry point).

---

## Agent Surface (T3/opencode)

The agent runs on the host, not in a container. T3 discovers `opencode.json` by walking up from its working directory, and the relative `./scripts/...` commands in `mcp.servers` resolve against that same directory, so **the repo root must be the cwd** — start T3 anywhere else and no server starts. Once a launcher is running, its own `.env`/`bin/` lookup no longer depends on the cwd: each one resolves the checkout from its own path.

- Build the servers with `./scripts/build-mcps.sh` (static `CGO_ENABLED=0` binaries into git-ignored `bin/`). Kept servers: `task-manager`, `project-manager`, `miser-money`, `cookbook`, `health-check`<!-- #584 TEMP-REMOVE (∃-Mail): , `reports` -->. A launcher refuses to exec a stale `bin/<server>` — one older than the non-test files under `cmd/<server>`, its own `internal/<store>` package, or the shared `internal/*` libs that server imports (exact per-server mapping in `scripts/mcp-run.sh`, pinned by `TestMCPWatchSetsMatchGoDeps`), or `go.mod`/`go.sum` (git HEAD backs the mtime check with the same test exclusion, so deletions trip it but test-only commits don't — merge-brought deletions included via `-m --full-history`) — naming the newer file and the rebuild command: pulling and restarting T3 does **not** rebuild `bin/`, so a stale server keeps serving its old tool list (#557). Rebuild after every merge touching those paths, then restart T3.
- One server is not built from this repo: `workspace-mcp` (Google Workspace/Gmail: read + label changes) runs from a git-ignored Python venv via [`scripts/workspace-mcp.sh`](scripts/workspace-mcp.sh). Install it with `./scripts/build-workspace-mcp.sh` — or `./scripts/hermes.sh init`, which does it too. See [Google Workspace (Gmail)](#google-workspace-gmail) below.
- **Each server loads its own environment from the root `.env`** — T3 passes no configuration of its own (it runs from an empty systemd unit; anything it inherited still passes through the launchers, which is why a `.env` value only fills a gap). Every `mcp.servers` entry runs a launcher ([`scripts/mcp-run.sh <server>`](scripts/mcp-run.sh) for the five Go servers, `scripts/workspace-mcp.sh` for Gmail), and the launcher fills only the keys its server needs (`MONGODB_URI`, `MONGODB_DB`; the three Google credentials plus `WORKSPACE_MCP_PORT` and `WORKSPACE_MCP_CREDENTIALS_DIR` for Gmail) through the shared loader in `scripts/lib/common.sh`: parsed, never `source .env` (a pasted Atlas URI carries `&` query params and a secret may hold `$`/backticks, which bare sourcing mangles or executes). In the launchers a variable already exported in the environment wins, so pointing a session at a throwaway Atlas database works — note that `./scripts/hermes.sh`, `./scripts/retention.sh` and `./scripts/sysmon.sh` are the other way round (they load with `load_root_env`, where the file wins), so exporting a throwaway `MONGODB_URI` does not redirect compose, a retention run or sysmon. `.env` holds more than the servers need — `PASSWORD`, the provider key and the compose ports stay there too; the launchers simply do not read them, so a server only sees such a value if it was inherited from whatever started T3 (start T3 from a clean shell if that matters). Do **not** reintroduce `{env:VAR}` in `opencode.json`: under T3's empty systemd environment it expands to `""`, which a launcher then treats as unset and fills anyway, so it buys nothing for the keys a launcher covers — and for a key no launcher covers the empty value is indistinguishable from unset: a required key fails loudly, an optional one silently takes its default (`MONGODB_DB`→`hermes` in the Go servers, the callback port→`8000` in the launcher), which is the quiet failure to watch for. `TestMCPServersLoadTheirOwnEnv` pins this.

- No secret is written into git; `.env` stays git-ignored.
- The persona and shared skills live in `.opencode/chat.md` and `.opencode/skills/`; `opencode.json`'s `instructions` entry loads the former.

### Google Workspace (Gmail)

The one non-Go MCP server (#515): [taylorwilsdon/google_workspace_mcp](https://github.com/taylorwilsdon/google_workspace_mcp) (MIT), pinned in `scripts/build-workspace-mcp.sh` and installed into the git-ignored `.workspace-mcp/venv`. `scripts/workspace-mcp.sh` launches it over stdio with a deliberately fixed, narrow surface:

```
--single-user --permissions gmail:organize --tool-tier complete
--disabled-tools send_gmail_message draft_gmail_message manage_gmail_label manage_gmail_filter
```

`--permissions gmail:organize` adds exactly three Gmail scopes — `gmail.readonly`, `gmail.labels` and `gmail.modify` — on top of the base `openid`/`email`/`profile` ones, which is the least that can mark a message read (`gmail.readonly` cannot). The verified tool list is eight read/utility tools (`search_gmail_messages`, `get_gmail_message_content`, `get_gmail_messages_content_batch`, `get_gmail_thread_content`, `get_gmail_threads_content_batch`, `get_gmail_attachment_content`, `list_gmail_labels`, `start_google_auth`) plus **exactly two write tools**, both label modifications:

- `modify_gmail_message_labels` — add/remove labels on one message
- `batch_modify_gmail_message_labels` — the same for up to 1000 message ids per call, or per conversation via `thread_ids`, with a read-back that reports what actually changed

What that means in practice: **mark read/unread** (`remove_label_ids: ["UNREAD"]`), **archive** (`remove INBOX`), **move to trash** (`add TRASH`), apply or clear custom labels. What does not exist here: send, draft, filter management, label management and permanent delete. Be precise about **why** each is absent, because the scopes are not the barrier for most of them:

| Tool | Held back by |
| --- | --- |
| `send_gmail_message`, `draft_gmail_message` | `--disabled-tools` — Google accepts `gmail.modify` for `users.messages.send` and `users.drafts.create`, so the scope *does* permit them |
| `manage_gmail_label` | `--disabled-tools` — `gmail.labels` comes with `organize` |
| `manage_gmail_filter` | `--disabled-tools` plus the un-requested settings scope |
| `list_gmail_filters` | not in the block list — it is held back by the un-requested settings scope, which is why it never registers at this permission level |
| permanent delete | the missing full-mail scope (`mail.google.com`) — no tool requests it |

So the disabled list is load-bearing, not belt-and-braces: `TestWorkspaceMCPWriteSurfaceStaysNarrow` pins it for exactly that reason.

Both write tools are `ask`-gated in `opencode.json` (`*modify_gmail_message_labels*`, `*batch_modify_gmail_message_labels*`), so every mail mutation prompts with its scope before it runs. `TestWorkspaceMCPWriteSurfaceStaysNarrow` (`opencode_perm_test.go`) pins the flags, the disabled list and the two gated tools; widening the surface means editing the launcher, the `ask` rules and that test in the same change. This is a real mailbox surface.

**Where the credentials live**

| What | Where |
| --- | --- |
| OAuth client id + secret | root `.env` (`GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET`) — git-ignored |
| Account the agent reads | root `.env` (`USER_GOOGLE_EMAIL`), also sent as the consent `login_hint` |
| Refresh-token cache | `.workspace-mcp/credentials/` (mode 0700, git-ignored) |
| Venv | `.workspace-mcp/venv/` (git-ignored) |

Nothing in that table is committed: `clean` never touches `.workspace-mcp/`, and a fresh clone does not contain it. It is runtime state, recreated by `./scripts/build-workspace-mcp.sh` plus one consent.

**One-time Google Cloud setup (human-only)**

1. Create a Google Cloud project, enable the **Gmail API** (nothing else is in scope).
2. Configure the OAuth consent screen and create an OAuth client of type **Desktop app**. A desktop client accepts any loopback redirect, so `http://localhost:<port>/oauth2callback` needs no registration (`8000` by default, or your `WORKSPACE_MCP_PORT`) — do not pick "Web application", which would require an exact URI. Leave **Use this client for an AI-powered agent** unchecked: it is a self-declaration that grants no scopes and changes no consent, and it only gates Google's *hosted* Workspace MCP endpoints — this server calls the Gmail REST API with your own client, so it is not required. Tick it on a **separate** client only if you later want Workspace-admin agent policies or plan to use Google's hosted MCP servers; the flag cannot be changed on an existing client after Nov 12, 2026.
3. Leave the publishing status on **Testing** and add the account as a test user. That avoids Google's verification/CASA assessment for the restricted Gmail scopes it asks for (`gmail.readonly` and `gmail.modify`, both restricted; `gmail.labels` is not), at the cost of a **7-day refresh-token lifetime** — expect to re-consent roughly weekly (below). Publishing the app instead means the full assessment.
4. Put the client id + secret and `USER_GOOGLE_EMAIL` into the root `.env` — or export any of the three for a one-off test, since for the launchers an exported value wins over the file — then reload the MCP servers (or restart T3/opencode): the launcher parses `.env` when it spawns, so a running server keeps the values it started with.

**Headless first-run consent**

The callback listener binds loopback on the host — `127.0.0.1:8000` by default, or `127.0.0.1:$WORKSPACE_MCP_PORT` when that is set — so a laptop browser reaches it through an SSH forward:

1. `ssh -L 8000:127.0.0.1:8000 <host>` and keep that session open — both numbers are the callback port, so substitute `$WORKSPACE_MCP_PORT` on both sides if it is set. The remote side is spelled `127.0.0.1`, not `localhost`: the listener is IPv4 loopback, and a remote `localhost` that resolves to `::1` first would forward to `[::1]:8000` and be refused.
2. Ask the agent for mail (e.g. "search my unread mail"). The tool call does not fail hard: it returns an **Authorization URL** for that account.
3. Open the URL on the laptop, sign in as `USER_GOOGLE_EMAIL`, approve. The browser is redirected to `http://localhost:<port>/oauth2callback`, the forward hands it to the host, and the page reports success.
4. Repeat the request. The token is now cached; later calls neither prompt nor print a URL.

The callback listener lives inside the spawned server process, so finish consent **while that T3 session is running** — if the process restarted in between, just make the request again for a fresh URL. `WORKSPACE_MCP_PORT` changes the port: set it in the root `.env` or export it (an exported value wins, like the credentials), then reload the MCP servers (or restart T3/opencode) — a launcher reads its inputs once, when it spawns — and forward the new port. That port is the **only** knob: the launcher assigns `GOOGLE_OAUTH_REDIRECT_URI` from it outright, so an inherited or exported redirect URI (say, one left over from a registered GCP URI) is discarded rather than honoured.

**Re-consent** — after the 7-day expiry, or to switch mailbox: delete the cache and respawn the MCP servers (reload, or restart T3/opencode), then repeat the four steps above (change `USER_GOOGLE_EMAIL` first if the account is changing). The cache is `.workspace-mcp/credentials/` unless you set `WORKSPACE_MCP_CREDENTIALS_DIR` — in `.env` (the launcher reads it, like the port) or in the environment; give a plain absolute path, since a parsed `.env` expands nothing (the launcher refuses any value containing `~` or `$`, and refuses a relative path, instead of creating a literal `~` directory or a cwd-dependent cache). Delete where you pointed it:

```bash
# from the repo root (the source path is relative); $REPO comes from the loader
source scripts/lib/common.sh
load_root_env_fill WORKSPACE_MCP_CREDENTIALS_DIR
trim_env_keys WORKSPACE_MCP_CREDENTIALS_DIR   # same normalisation as the launcher
rm -f "${WORKSPACE_MCP_CREDENTIALS_DIR:-$REPO/.workspace-mcp/credentials}/"*.json
```

(`$REPO` is the checkout root: the loader resolves it from *its own* path unless `REPO` is already exported, while the launchers always set it from theirs — so run the snippet from the repo root and do not rely on an exported `REPO`; the `source` line is relative too. `load_root_env_fill`, not `load_root_env`, so an exported override wins — but only one visible to this shell: a `fill` sees its own environment, so if the launcher gets `WORKSPACE_MCP_CREDENTIALS_DIR` from an export rather than from `.env`, export it here too or the `rm` deletes a different directory and re-consent loops.)

**Check it works**: the launcher parses the root `.env` itself, so there is no pre-load step — run it bare:

```bash
./scripts/workspace-mcp.sh
```

It refuses a malformed `WORKSPACE_MCP_PORT` first, then names whichever of the three credentials (`GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET`, `USER_GOOGLE_EMAIL`) is missing or still the `.env.example` placeholder, and finally checks the token-cache directory (absolute, with no `~`/`$`). With all of those satisfied it opens stdio and waits for a client (that is the point — there is no mail to print and no consent URL until an MCP client calls a tool), so the real proof is a read through T3: ask for mail, complete consent once, ask again. `opencode mcp list` shows the server once the MCP servers have respawned (reload, or restart T3/opencode) — an edit to `.env` needs a respawn because the launcher reads it at spawn time.

The bind is loopback because the launcher scrubs every env-only knob that could widen it — `WORKSPACE_MCP_HOST`/`WORKSPACE_MCP_BASE_URI`/`WORKSPACE_EXTERNAL_URL`, the extra unauthenticated listener `WORKSPACE_MCP_HTTP_PORT`, and the five tool-surface variables `WORKSPACE_MCP_TOOLS`/`WORKSPACE_MCP_TOOL_TIER`/`WORKSPACE_MCP_READ_ONLY`/`WORKSPACE_MCP_PERMISSIONS`/`WORKSPACE_MCP_DISABLED_TOOLS`. For `PERMISSIONS`/`TOOL_TIER`/`DISABLED_TOOLS` the flags below are what take effect and the unset keeps the two out of conflict; `TOOLS` and `READ_ONLY` are ignored by 2.4.2 while `--permissions` is present (it warns and drops them), and the unset means they are never considered at all. The host/URL knobs are the ones with no flag and no permission-mode protection, so their unset is load-bearing (the CLI flags above win where they exist, but the host/URL/HTTP-port are env-only); setting any of them outside the file has no effect. Verified against 2.4.2 at the default port with `PORT=9999` in the environment, which still yields `127.0.0.1:8000` and a redirect of `http://localhost:8000/oauth2callback` (with `WORKSPACE_MCP_PORT=9000` both become 9000 — `PORT` never wins).

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
5. Installs the Google Workspace/Gmail MCP server into `.workspace-mcp/venv` (`./scripts/build-workspace-mcp.sh`). A failure warns instead of aborting — the container stack does not need it.

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
| `money_accounts` | Miser-money | `name` (unique), `type`, `openingBalance` (int64 paise seed, seeded at creation — read back as `balance_paise`; changeable only via `edit_account`, which moves the balance by exactly the delta with no transaction row), `archived`, `createdAt` — **permanent**. An archived name stays taken; `unarchive_account` restores it |
| `money_transactions` | Miser-money | `date` (YYYY-MM-DD, real calendar date — `2026-02-30` rejected), `amount` (int64 paise, >0 — read back as `amount_paise`; the surface takes integers only, so fractional money fails schema validation), `type` (income\|expense\|transfer), `category` (must exist in `money_categories`; unknown names are rejected), `note` (capped at 300 bytes, UTF-8-safe cut; `null` means no change on edit, `""` clears it), `source`, `accountId` (required), `sending_to` (transfers only), `idempotencyKey` (optional, UNIQUE+SPARSE, trimmed, max 200 bytes — a retried log returns the original row instead of writing a second — unless the row was soft-deleted, in which case even the same payload is rejected naming `restore_transactions` — but the same key with a different payload is always **rejected** as a mismatch first (restore cannot fix that, use a new key)), `createdAt`, `deletedAt` (soft-delete flag; excluded from every ledger read — query/match/analyses/balances/budget spend; the by-id verbs edit/restore/delete/idempotency-replay check the flag explicitly to reject or restore) — rows are **soft-deleted**: `delete_transactions` flags them (recoverable via `restore_transactions`), and the `ttl_deleted_at` index removes them 10 days after flagging |
| `money_budgets` | Miser-money | `category` (must exist in `money_categories` except `salary` — income is not budgeted), `month` (`YYYY-MM`, effective from this month), `monthlyLimit` (int64 paise, > 0 — written as `monthly_limit_paise`, read back as `monthly_limit_paise`), `createdAt`, `updatedAt` — **permanent**. Unique on (`category`, `month`); only `delete_budget` removes a row |
| `money_categories` | Miser-money | `name` (unique, trimmed, lowercased) — **permanent**. Seeded with the ten defaults; `other` is the reserved blank fallback and cannot be renamed, renamed to, or deleted. Renames rewrite `money_transactions` and `money_budgets`; deletes are refused while rows reference the category unless `reassign_to` moves them |
| `hc_meals` | Health-check | `date` (YYYY-MM-DD), `items[{name, qty?, kcal, protein, carbs, fat, fiber}]`, `totals` (computed), `createdAt` |
| `hc_days` | Health-check / Health Sync | `date` (YYYY-MM-DD, unique), `steps` (int), `active_kcal` (float), `sleep_hours` (float), `workouts[{type, minutes, kcal}]`, `updatedAt` |
| `hc_weight` | Health-check | `date` (YYYY-MM-DD, unique), `kg` (float) — **exempt from retention pruning** |
| `cookbook_ingredients` | Cookbook | `name` (unique), `note` (≤2000 bytes, UTF-8-safe cut), `createdAt`, `updatedAt` — **permanent** |
| `cookbook_recipes` | Cookbook | `name` (unique), `note` (≤2000 bytes, UTF-8-safe cut), `tags[]` (≤20 entries, ≤40 bytes each), `createdAt`, `updatedAt` — **permanent** |
| `cookbook_cook_log` | Cookbook (retired #600) | pre-trim rows stay in Atlas untouched; nothing reads or writes them — **permanent** |
| `tasks` | Task-manager | `idempotency_key` (client key, UNIQUE+SPARSE — makes create idempotent, #176), `source` (app vs MCP), `name`, `description`, `due_date`, `due_time`, `estimated_minutes`, `repeat_rule` (verbatim) — no status field; `completedAt` null = open, done tasks TTL 3d via `expiresAt`; `skippedAt` + derived `skipped` on occurrences skipped via the skip verb (#287) — still read as "not done" |
| `tasks_changes` | Task-manager | Single document (`_id: "revision"`) holding the collection-wide change counter every task write advances — create, update, complete (plus its rollover successor), skip, reopen, delete — from **both** writers, because the marker is bumped by the store itself (#178). Served read-only as `GET /api/tasks/changes?since=`; a collection that has never been written reports revision 0. **Permanent** |
| `task_mutations` | Task-manager | The change log (#176): one row per mutation — `op`, `task_id`, `source`, `detail`, `at` — from **both** writers. Served read-only as `GET /api/tasks/mutations?task_id=&op=&since=&limit=` and as the `task_history` MCP tool (#350). It outlives its subject on purpose (done tasks are TTL-deleted after 3 days). Pruned at 90 days. Indexed `{task_id, at}` and `{at, _id}` |
| `projects` | Project-manager | `name`, `status` (Todo\|Ongoing\|Testing\|Paused\|Done), `note`, `position` (manual board order), `snoozeAt` (RFC3339 instant, Paused only, must be strictly >60s ahead), `snoozeFiredAt` (the snooze instant the server ticker fired), `createdAt`, `updatedAt` — **permanent**, shared with the app's Projects tab. health-api wakes a due snooze to Ongoing and clears `snoozeAt` one minute early; no client schedules it |
| `counters` | Project-manager | Single doc `_id: "projects_position"` holding `seq`, the atomic source of `projects.position` (`findOneAndUpdate` `$inc`, seeded from the max in `EnsureSchema`). Inspect with `mongo get counters`; back up and restore with `projects` — a counter restored behind the max reissues an existing position |
<!-- #584 TEMP-REMOVE (∃-Mail): reports collection row.
| `reports` | Reports (∃-Mail, #365) | `title`, `body_md` (markdown with `$..$`/`$$..$$` LaTeX, compiled server-side on read), `source`, `read`, `starred`, `createdAt` — **7-day conditional retention**: pruned when `createdAt < today - 7d AND read AND NOT starred` |
-->

### Database Helper CLI
Operators use the `mongo` Go CLI (`cmd/mongo/main.go`) directly:

```bash
go run ./cmd/mongo count money_transactions '{"type":"expense"}'
go run ./cmd/mongo insert hc_weight '{"date":"2026-08-08","kg":63.2}'
go run ./cmd/mongo upsert hc_days '{"date":"2026-08-08"}' '{"steps":8452}'
# update rewrites every match (UpdateMany $set — plain fields, not a full
# update doc). The project Blocked migration, then its verify:
go run ./cmd/mongo update projects '{"status":"Blocked"}' '{"status":"Paused"}'
go run ./cmd/mongo count projects '{"status":"Blocked"}' # must be {"count":0}
```

---

## Data Retention & Lifecycle

Automated data pruning is executed by the `retention` Go binary (`cmd/retention/main.go`):

| Target | Retention Window | Action |
|---|---|---|
| `money_transactions` | Flagged 10 days | **Soft delete** — `delete_transactions` flags rows with `deletedAt` (recoverable via `restore_transactions`); the `ttl_deleted_at` index removes flagged rows 10 days after flagging. Unflagged rows are permanent; the retention job itself is a no-op for money |
| `money_budgets` | Permanent | **Never expires** — rows live until `delete_budget` removes them; money retention is a no-op |
| `hc_meals` | > 30 days | Pruned when `date < today - 30d` |
| `hc_days` | > 30 days | Pruned when `date < today - 30d` |
| `hc_weight` | Permanent | **Never pruned** |
| `cookbook_*` | Permanent | **Never pruned** |
<!-- #584 TEMP-REMOVE (∃-Mail): reports retention row.
| `reports` (#365) | > 7 days, conditional | Pruned when `createdAt < today - 7d AND read AND NOT starred` — unread/starred never pruned |
-->
| `task_mutations` | > 90 days | Pruned when `at < today - 90d` — the task audit log (#176) |
| `tasks_changes` | Permanent | **Never pruned** — one single-document counter, not a log (#178) |
| `tasks` (repeats) | n/a | **Reconciled, not pruned**: completed structured repeats with no successor are reported (#180) |

Run manual dry-runs via:
```bash
./scripts/retention.sh --dry-run
```

### Operator-set, not app settings (#355)

The retention windows above and the agent's personality and instructions
(`.opencode/chat.md`, `.opencode/skills/`) are operator-set by design: they live
in the repo / deployment, not in the app. The app's Settings → Behaviour section
owns the device-local knobs (reminder lead time, notifications) — everything
with an obvious default that the device can apply itself. Health sync cadence
lives with the Sync card in Settings → Health sync (#588).

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
| `/api/projects`, `POST /api/projects/reorder`, `POST /api/projects/move` | Project board CRUD + manual order (full-section rewrite, single-row move). health-api also runs the snooze ticker: every 30s it flips due `Paused` rows (`snoozeAt - 1m`) to `Ongoing` and clears `snoozeAt`, so the transition is server-owned and shared by every client |
| `/api/files`, `/api/files/upload`, `/api/files/download`, `DELETE /api/files/delete?path=<relative file>` | The Files tab shelf over `exports/` (delete removes one file; folders, escapes and missing rows are named refusals) |
<!-- #584 TEMP-REMOVE (∃-Mail): reports route row.
| `/api/reports` | ∃-Mail inbox (markdown + LaTeX) |
-->
| `/api/money/balances`, `/api/money/transactions` | Money read surface for the app (derived per-account `balance_paise` + total; paged `amount_paise` rows, `order=asc` oldest-first default, `desc` newest-first). `start`/`end` (YYYY-MM-DD) are required; `limit` defaults 100, clamped to 500 (an explicit `limit=0` is rejected — unlike MCP); optional `account` (name or id hex, matches either side of a transfer); `total` counts the window (not the balances total, which has no date cap); page with `offset` (default 0) = `next_offset` until `truncated` is false (`next_offset` is null when done — never send `offset=null`) |
| `/health`, `/health/` | Liveness probe (unauthenticated) |

```bash
curl http://<host>:8080/api/tasks/changes -H "Authorization: Bearer <PASSWORD>"
```

---

## Development Mode Isolation

There is no local MongoDB service — every environment (dev included) uses the Atlas `MONGODB_URI`. Isolate development by pointing `MONGODB_URI`/`MONGODB_DB` at a separate throwaway database, but mind **which direction wins**, because it differs per entry point:

- **MCP servers** (`scripts/mcp-run.sh`, `scripts/workspace-mcp.sh`) load with `load_root_env_fill`: a value already **exported** in the environment wins over the file. So `export MONGODB_DB=agento_dev` in the shell that starts T3 redirects the agent.
- **Compose and the orchestrator scripts** (`scripts/hermes.sh`, `scripts/retention.sh`, `scripts/sysmon.sh`) load with `load_root_env`, where the **file wins**. Exporting a throwaway `MONGODB_URI` there does nothing — edit `.env` (or point a separate checkout at a separate `.env`).

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

### Approval prompts (#313, #487, #503)

Sensitive tool calls are approved by the T3/opencode permission prompts configured in `opencode.json` (pinned by `opencode_perm_test.go`) — the old in-app approval queue and the in-process approval gate are both gone (#503 removed `internal/approvals` and `internal/approvalgate` outright: no wrapper, no audit rows, no `approvals`/`audit_log` collections).

Upgrading with an existing database (e.g. a separate dev database): deleting the code does not delete existing collections or their TTL indexes — drop the retired ones by hand (`go run ./cmd/mongo drop approvals`, `go run ./cmd/mongo drop audit_log`). Fresh databases simply never create them.

- Money writes, account lifecycle (`create_account`/`archive_account`), every `delete_*`, destructive rewrites (`fix_last_*`), and prunes prompt first; reads and routine writes run free. (The harness cannot see arguments, so even a counting prune with `dry_run=true` prompts.)
- Error text in the Health Sync response (`POST /api/health/sync`) is pattern-scrubbed (128-rune cap on the `detail` envelope; 300 is the scrub's internal bound). Other endpoints echo decode errors raw — treat every `detail` string as untrusted display text regardless. (MCP tool handlers return their errors raw — scrubbing happens at the HTTP envelope, not in the servers.)

---

## Extending the Stack

### Adding an MCP server
1. Add `cmd/<name>/` and `internal/<name>/` (stdio MCP server, Mongo-backed).
2. Add the server to `SERVERS` in `scripts/build-mcps.sh`, and to `mcp.servers` in `opencode.json` as `"command": ["./scripts/mcp-run.sh", "<name>"]` — **no `environment` block**: `mcp-run.sh` fills `MONGODB_URI`/`MONGODB_DB` from the root `.env`. A server that needs any other key gets its own launcher script (the way `scripts/workspace-mcp.sh` does it), not a wider list in the shared `mcp-run.sh` — `TestMCPServersLoadTheirOwnEnv` pins that list to exactly those two keys, because one wrapper serves all five servers. `{env:VAR}` expands to `""` under T3's empty systemd environment and the same test rejects it.
3. Run `./scripts/build-mcps.sh`, then confirm with `opencode mcp list` from the repo root.

An external (non-Go) server follows the `workspace-mcp` pattern instead: an installer script that pins the upstream version into a git-ignored directory, a launcher script named in `mcp.servers`, its credentials in the root `.env`, and — if it can write anything — `ask` rules plus a pin like `TestWorkspaceMCPWriteSurfaceStaysNarrow`.

### Adding a skill
Put the skill directory containing `SKILL.md` under `.opencode/skills/<name>/` and commit it. T3/opencode discovers that tree; `opencode.json` only needs the `instructions` entry that loads the persona.
