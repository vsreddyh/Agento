# Task Manager MCP Server

Personal task tracking for the user. The agent manages tasks over MCP;
the Agento Android app's Task Manager screen offers the same list with
full CRUD over `GET/POST/PATCH/DELETE /api/tasks` on health-api (same
store, same validation — see `cmd/health-api/main.go`).

## Tools

| Tool | Purpose |
|---|---|
| `create_task` | Create an open task. Required: name, description, due_date, due_time, estimated_minutes, parallelable. Optional: the four repeat keys (absent = one-shot) |
| `list_tasks` | List by state open/done/all, overdue-only, or search. Every argument is optional (state=open, limit=200) |
| `get_task` | Fetch one task by id |
| `update_task` | Edit fields. Only `id` is required — every other field is nil-safe, and empty repeat_rule clears the rule |
| `complete_task` | Mark done (3-day retention starts); echoes repeat_rule + follow-up nudge; always reports `rollover` |
| `skip_task` | Skip ONE occurrence without claiming it was done; 3-day retention starts too; advances a structured repeat |
| `reopen_task` | Reopen a completed **or skipped** task (cancels expiry; a skip returns to open, not to done) |
| `delete_task` | Permanently delete |

## Schema (MongoDB `hermes` DB)

**`tasks`** — `name`, `description`, `due_date` (YYYY-MM-DD), `due_time` (HH:MM,
requires a date), `estimated_minutes`, `parallelable`, `repeat_rule` (free-form,
verbatim), `completedAt` (null = open), `createdAt`, `expiresAt` (completed only =
completedAt + 3d, TTL target).

Note the distinction between *required on create* and *required in the document*: the six
fields `store.Create` rejects when absent are marked required in the tool schema, and are
plain stored values. Optionality is a property of the tool contract, not of the schema's
data model — `cmd/task-manager/schema_test.go` asserts the two agree per tool.

### Key invariants

- **No status field:** openness is `completedAt == null`. Never add one.
- **No server-side repeat logic:** `repeat_rule` is never parsed here.
  The agent interprets it and creates the next occurrence (see
  `pi/skills/task-manager/SKILL.md`); `complete_task` only echoes the rule
  back with a `follow_up` nudge.

## `complete_task` and the `rollover` field (#180)

Every completion reports WHY a next occurrence did or did not get created:

| `rollover` | Meaning | What the caller does |
|---|---|---|
| `created` | Structured cadence; `next` holds the new task | nothing |
| `none` | One-shot; there was never a next to make | nothing |
| `custom` | The condition is the caller's to interpret | create it (the `follow_up` says so) |
| `exhausted` | Structured, but no future date is computable | tell the user; the repeat stopped |
| `failed` | Minting errored — the repeat has stopped | tell the user, loudly |

This field exists because the outcome used to be inferred from `next == nil`,
which made `none`, `exhausted` and `failed` indistinguishable. A repeat that had
silently stopped recurring was reported as an ordinary completion, and the failure
existed only in a log line — which is how four legacy tasks stopped scheduling
before anyone noticed. `exhausted` and `failed` also carry `needs_attention`.

The nightly `retention` job runs `ReconcileRollover` for the same reason: rollover is a
side effect of `Complete`, so any writer that does not go through it stops a task
recurring with nothing to show for it. It reports and never repairs — a task that failed
to roll over usually failed because its stored data is wrong.

Each gap it prints leads with the field that caused it — "cannot roll over: due_time is
required (HH:MM)" — rather than the bare outcome, because the whole point of the report
is that a person has to go and fix something. `exhausted` is reserved for the one case it
means: the cadence has genuinely run past `MaxRollovers`. An unreadable `due_date` is
`failed`, because the remedy is to fix the field, not to re-enter a repeat that is intact.

#### What the reconciliation cannot see

A row whose repeat fields have drifted to an unreadable type is usually reported and named
as such, but **not always** — and the asymmetry is worth knowing before you rely on the
report:

- `repeat_unit` stored as a **number** (e.g. `7`) passes the query and IS reported, as a
  failure naming the wrong type — `$nin: ["", null]` matches a number, which then reads
  back as an empty unit.
- `repeat_every` stored as a **string** (e.g. `"5"`) is **silently missed**. MongoDB
  brackets comparison operators by BSON type, so `$gt: 0` does not match a string and the
  row never becomes a candidate. A repeat in that state has stopped recurring and this job
  will not say so.

That asymmetry is a property of the query, not an oversight in it: widening the filter to
match drifted types would also pull in rows that are not repeats at all. If a repeat you
expect to be recurring is missing from the report, check its stored types first — that is
the failure this caveat is here to prevent.

## `skip_task` — a skip is not a completion (#209)

There was no verb for "not doing this occurrence". An agent asked to skip five
chores called `complete_task` on all five, recording work that never happened as
done — and since completed rows are TTL-deleted after 3 days, the false record was
erased rather than corrected. The agent's own reasoning had read *"this is
ambiguous, I should ask"* and then guessed, because there was nothing else to call.

`skip_task(id, reason?)`:

- sets **`skippedAt`** so the record says skipped, not done. Every task response
  carries a derived `skipped` boolean, so "is it done?" and "was it done?" are
  different questions and a client reading only `completedAt` no longer conflates
  them;
- also sets `completedAt`, deliberately — the occurrence is *resolved* and must
  leave the open list, or the user is asked again tonight. Leaving it open is
  already what an open task is; conflating the two would put skipped chores back
  on tonight's list;
- **still advances a structured repeat**. Skipping an occurrence is not abandoning
  the series — `delete_task` is what abandons it.

`reason` is the user's words, verbatim and capped at 200 bytes on a rune boundary.

`reopen_task` clears the skip marker too, and returns a skipped task to **open**,
not to done.

Over HTTP: `POST /api/tasks/{id}/skip` with an optional `{"reason": "..."}`. The
body accepts **only** `reason`; an unknown key is a `422` naming the offending field.
A misspelt `{"reson": ...}` used to decode into an empty reason, leaving the user
believing they had recorded why with nothing recorded — the quiet disappearance a
skip exists to be distinguishable from. An empty body is still valid.
response keeps the task at the top level (so an old app still parses it) and adds
`skipReason`, `next` and `rollover`. `skipped` is not among them: it is derived
from `skippedAt` on **every** task response, so it reads `false` on anything
completed or open and only flips here.
- **Retention:** done tasks auto-delete 3 days after completion via TTL. **Skipped
  occurrences expire the same way** — `skip_task` resolves the occurrence, so it
  sets `expiresAt` exactly as `complete_task` does. A skip is not permanent
  history, and that is worth knowing before relying on it as a record.
  Open tasks never expire. `reopen_task` clears the expiry.

## Run

```bash
go run ./cmd/task-manager          # stdio transport (or go build -o task-manager ./cmd/task-manager)
```

`MONGODB_URI`/`MONGODB_DB` from the root `.env` (same as the other servers).

## Client config

```json
{
  "mcpServers": {
    "task-manager": {
      "command": "/usr/local/bin/task-manager",
      "args": []
    }
  }
}
```

## Files (Go, `internal/tasks/` + `cmd/task-manager/`)

- `cmd/task-manager/main.go` — MCP server + tool definitions
- `internal/tasks/store.go` — CRUD, completion/expiry, indexes
- `internal/tasks/store_test.go` — tests (`go test ./internal/tasks/`, needs `MONGODB_URI`)
