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
| `exhausted` | Structured and intact, but past `MaxRollovers` | tell the user; re-enter the repeat |
| `failed` | Stored data won't carry forward, or minting errored | tell the user which field, loudly |

This field exists because the outcome used to be inferred from `next == nil`,
which made `none`, `exhausted` and `failed` indistinguishable. A repeat that had
silently stopped recurring was reported as an ordinary completion, and the failure
existed only in a log line — which is how four legacy tasks stopped scheduling
before anyone noticed. `exhausted` and `failed` also carry `needs_attention`.

`exhausted` deliberately covers ONLY the `MaxRollovers` ceiling. An unreadable
`due_date` used to land there too, and its remedy was "re-enter the repeat" — the wrong
advice for a repeat that was perfectly intact and merely had an unreadable field. It is
`failed` now, and names the field.

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


## HTTP hardening (#176)

`POST /api/tasks` accepts an **`Idempotency-Key`** header. The same key **with the
same body** returns the *original* task rather than creating a second one, answering
`200` with `Idempotent-Replay: true` instead of `201`.

The same key with a **different** body is a `422`, not a replay. The key is single
use per distinct request, and this matters more than it looks: answering `200` with
the original task would tell the caller it had created a task carrying the fields it
just sent, when the task it receives has none of them. A silent wrong answer is worse
than the duplicate the key exists to prevent. The mismatch is detected by a digest of
**every field the create reads** (`taskCreateFields` in the source), written in the
same insert as the key — so a retry that differs only in JSON key order or indentation
still replays, and adding an unrelated field to a newer client does not break an
existing key.

No count is given here on purpose. The list is enumerated in one place in the code, and
a number in prose is a second place to fall behind — `repeat_rule` was once missing
from the digest because it is consumed, validated and **stored**, so two creates
differing only in their custom repeat text ("3rd Friday" vs "end of month") hashed
alike and the second replayed the first's task. The prose said "nine" while the code
had ten, and would have said "ten" the next time a field was added. A test now walks
the list and fails if any field is left out of the digest.

An absent key is normalised to its zero value rather than skipped — the store reads
absent and zero identically, so a client that upgrades to sending `"repeat_rule": ""`
must not 422 its own retry. String values are likewise TrimSpaced for exactly the
fields the create path trims (`name`, `description`, `due_date`, `due_time`,
`repeat_unit`, and `repeat_rule`), because `" foo "` and `"foo"` create the *same* task
and a retry differing only in padding must replay.

`repeat_rule` is in that set because `Repeat.Normalize` trims the text for every create
path. It used to be trimmed by the MCP path and by the update path but **not** by HTTP
create, so the same custom condition was stored two different ways depending on which
door it came through — and it had been excluded from this digest precisely because create
stored it raw, an exclusion that was correct until the store changed. The rule now lives
in `Normalize`, which every path already calls, so a future caller cannot skip it.

The general rule the digest has to keep: **it must agree with the store about what
"the same request" means.** Every gap so far — `repeat_rule` missing, absent-vs-zero,
whitespace — was the digest and the store disagreeing about sameness, in one direction
or the other.

`idempotency_key` and `source` are returned on every task, deliberately: the key is
client-generated rather than secret, and echoing it is what lets a client correlate a
retry without keeping its own log; `source` is the only record of *which* caller wrote
a row, which matters because the app and the agent share one password.

`idempotency_fingerprint` is **not** returned. It is internal to this mechanism, and a
digest of the body the client just sent tells it nothing it does not already know. It
is read back from the raw stored document rather than from the rendered task, so hiding
it cannot switch off the mismatch check — a cosmetic edit must never be able to
disable a correctness guarantee.

The guarantee is enforced by a **unique sparse index**, not by a lookup before the
insert — a read-then-write has a window between the two, and the failure it
produces is the one this exists to prevent: a client retrying after a timeout,
whose first request is still in flight, reads "nothing with this key" and inserts a
duplicate. Two concurrent retries therefore produce one task, and the loser of the
race is handed the winner's row. Without the header, behaviour is unchanged.

Sparse matters as much as unique: most tasks are created without a key, and a
plain unique index would treat every missing value as the same key and reject all
but the first.

Two additions beside it:

- **Rate limiting** — a per-source token bucket (burst 40, ~2/s sustained) with a
  `429` and `Retry-After`. The API is network-reachable and authed by one shared
  password, so a stuck client retrying in a loop was an unbounded write load on a
  shared production MongoDB. The limiter wraps the mux, so a route added later
  cannot forget it.

  "Per-source" means the real client address, taken from `X-Real-IP` **only when the
  immediate peer is on a private network** — otherwise every request through the
  nginx proxy on `:8080` would share the proxy's single bucket, which is most of them.
  The trust condition is load-bearing rather than cautious: compose publishes
  health-api directly as `8001:8000`, so a client can reach it bypassing nginx and
  set `X-Real-IP` freely. Believing the header unconditionally would let anyone
  disable the limiter by rotating one header. `X-Forwarded-For` is a fallback and only
  its **last** hop is read, since nginx uses `$proxy_add_x_forwarded_for`, which
  appends to a client-supplied list and so leaves every earlier entry attacker-chosen.
- **A mutation log** (`task_mutations`) recording op, task, source and timestamp for
  every create/update/complete/skip/reopen/delete, from **both** callers: the HTTP API
  (`source` = the `X-Agento-Source` header, or `http`) and the MCP server
  (`source` = `mcp`). The agent and the app are the same caller as far as the server is
  concerned — one shared password, one collection — so nothing previously recorded *who*
  changed a task. Both paths are wired because the agent is a primary mutator: logging
  only HTTP would answer the question for the app and stay silent for the agent, which
  is exactly the case the log exists for. `X-Agento-Source` labels the caller — trusted
  only as a label, never for authorisation.

  **The log is not tamper-evident.** `source` is self-attested: anyone holding the shared
  password can claim any label. So this answers "who changed this, and when" well enough
  to explain an accident or a surprising change, and badly enough to be useless against
  someone acting deliberately. Closing that gap needs per-caller credentials rather than
  one shared password. Recorded here so the field is not later mistaken for an
  authenticated identity.

  A replayed create is **not** recorded: it changed nothing, so there is nothing to
  audit, and logging one would make the log claim a task was created once per retry —
  which defeats counting creates to answer "how many tasks exist". An update that wrote
  nothing (`PATCH` with no field the store will set) is likewise not recorded.

### What the nightly reconciliation cannot see

It scans for completed **structured** repeats, so a row whose repeat fields have
drifted to an unreadable type can be invisible to it. The case to know about:

- `repeat_unit` stored as a **number** (e.g. `7`) passes the query and IS reported, as a
  failure naming the wrong type — because `$nin: ["", null]` matches a number, which then
  reads back as an empty unit.
- `repeat_every` stored as a **string** (e.g. `"5"`) is **silently missed**. MongoDB
  brackets comparison operators by BSON type, so `$gt: 0` does not match a string and the
  row never becomes a candidate. A repeat in this state has stopped recurring and this
  job will not say so.

That asymmetry is a property of the query, not an oversight in it: widening the filter
to match drifted types would also pull in rows that are not repeats at all. If a repeat
you expect to be recurring is not in the nightly report, check its stored types first —
that is the failure this caveat is here to prevent. Writes are best-effort: a failed audit write
  never fails the mutation it was recording. Pruned at 90 days by `retention`.
  Indexed by `at` and `task_id`, declared in the same `EnsureSchema` as the task
  indexes — an earlier `EnsureAuditIndex` that nothing called meant both readers were
  collection-scanning a collection that only grows. The write does **not** inherit
  the request context, so a client that disconnects mid-request cannot drop the entry
  for a mutation that already committed; it is bounded at 5s independently.

  The limiter counts a source as `IP + hash(token)` so two real callers behind one
  address each get a full budget — but **only for a token that is actually
  configured**. The limiter runs before auth, so folding in whatever token was
  presented would let anyone evade the limit by rotating a bogus `Authorization`
  header. Unauthenticated traffic is keyed by address alone.
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
