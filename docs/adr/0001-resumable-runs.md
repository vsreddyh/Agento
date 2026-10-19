# ADR-0001: Conversation runs are a sibling of cron jobs

- Status: accepted (2026-10)
- Issue: #311

## Context

A chat turn used to be the lifetime of one HTTP request. The handler held the
profile's turn lock, prompted Pi, and wrote SSE frames to the response. Killing
the app or dropping the network killed the turn with it. The app's recovery
story (#214) was to poll the sessions API after the fact and fetch whatever Pi
had recorded, which showed the answer but could not show progress, steer, or
stop anything.

The requirement is a run that survives app close, reattaches with its progress,
and can be steered and cancelled while it is in flight.

## Decision

### One run per profile, no queue

Pi holds one session per process, and `agentHandler.turnMu` already serialises
turns. A run therefore executes under that same lock; there is still exactly one
executing turn per profile.

A second prompt on a busy profile is not queued. Same conversation + same prompt
reattaches to the in-flight run (that is what the app retries after a drop). A
different prompt gets `429` with `X-Agento-Run-Id`, naming the run so the client
can reattach or cancel instead of guessing. A queue was rejected because it
would make the wait unbounded and invisible, which is worse than a refusal that
says what is running.

### State lives in MongoDB; execution does not

Runs are stored in the `agent_runs` collection through `internal/runs.Store`,
with an in-memory store used by tests and when Mongo is unreachable. The memory
store is a fallback for availability, not a second behaviour: both implement the
same interface and the same execution path.

What Mongo buys is surviving the process. What it cannot buy is execution: Pi
is a stdio child of the gateway, so a gateway restart ends every turn. At
startup `MarkInterrupted` flips stale `running` rows to `interrupted` and keeps
their partial output. This is recorded here so nobody later reads "resumable" as
"jobs resume after a deploy" — the *record* survives, the *work* does not.

### TTL is 7 days

A `expiresAt` field plus a Mongo TTL index (best-effort creation, logged on
failure) deletes finished runs after seven days. Long enough to reopen the app
the next morning and see what happened; short enough that run output does not
quietly become a second, unmanaged transcript archive. A run in flight refreshes
its expiry when it settles, so it cannot be deleted mid-turn.

### Sibling of `/api/jobs`, not a reuse of `cronjobs`

Cron jobs are fire-and-forget: identified by schedule and prompt, carrying a
short capped history of notifications. A run is one execution with an
observable lifecycle. Sharing the collection would mean one document shape doing
two jobs and `/api/jobs` growing a second meaning.

`internal/runs` and `internal/cronjobs` stay separate packages, and the routes
stay separate collections of handlers. `/api/runs` is read-mostly plus a small
set of verbs:

| route | purpose |
|---|---|
| `GET /api/runs` | list, filterable by `profile`, `conversation_id`, `status`, `limit` |
| `GET /api/runs/{id}` | one run's record |
| `GET /api/runs/{id}/events` | SSE: snapshot, then live frames |
| `POST /api/runs/{id}/steer` | queue an instruction into the turn |
| `POST /api/runs/{id}/cancel` | abort the turn, keep partial output |

Both `/api/runs` and `/{profile}/api/runs` spellings are served, matching
`/api/jobs` and the app's profile-path `base + "/p/chat"` construction.

### Reattach is snapshot plus live frames, not an event log

Every stream begins with one `{"agento_run": {...}}` frame carrying the run's
complete accumulated output, reasoning, tools, usage and status at sequence
`N`. Frames published after that snapshot have a higher sequence and are
relayed; everything at or below `N` is already inside it. This needs no stored
event log and no replay: a client that reattaches reconstructs its display from
the snapshot and continues from the live stream.

The reattach key is the conversation plus the full user turn — text and image
set. Same text with different images is a different turn (429, not a reattach).

An older client ignores the snapshot frame (unknown keys are skipped by the app)
and still sees ordinary chat chunks. A run that settled before the client
subscribed replays its final output as one ordinary delta chunk so that client
gets an answer rather than an empty stream.

Wire shapes for the app parser: the snapshot frame omits empty `output` /
`reasoning` (`omitempty` — absent means "nothing yet"), while the list/detail
records always emit their keys (`"output":""`, `"error":""`, `seq`,
timestamps). `tools` is always an array in both. Parse leniently: tolerate a
missing key wherever the other shape sends an empty value.

### Stop is a server-side cancel

Cancel marks the run, sends Pi's `abort` (via `Send`, because the abort reply
arrives only once the session is idle), keeps the text produced so far, and
ends the stream with an explicit `{"agento_run": {"status": "cancelled"}}`
frame. The app must call this endpoint, not just close its socket: closing the
socket is exactly the case this feature exists to make non-destructive.

## Consequences

- A turn now outlives its request: disconnecting mid-turn no longer stops it.
  A client that wants to stop work must cancel explicitly.
- One run at a time per profile remains the concurrency limit, and the 429 is
  informative rather than a bare refusal.
- `/api/runs` in the app must use the profile path (`/p/chat/api/runs`);
  unprefixed `/api/*` through the proxy belongs to health-api.
- Run output is capped on persistence (256 KiB output, 64 KiB reasoning, 4 KB
  prompt preview, 32 tool names, 100 runs per list). The live output buffer is
  uncapped for the duration of the run; live reasoning follows the same 64 KiB
  cap, filled partially rather than dropped at the boundary.
- The list route shows previews (2 KB output, 1 KB reasoning), not full turns;
  the detail route keeps everything.
- Persistence never blocks the turn: the executor offers each second's snapshot
  to a background loop without waiting, and the terminal write drains after it.
  The 429 names the profile's in-flight run even across conversations.
