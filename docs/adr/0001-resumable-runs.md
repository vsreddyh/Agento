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
| `GET /api/runs` | list, filterable by `profile`, `conversation_id`, `status`, `limit` (default 100, capped at 100; absent, empty or `0` means default) |
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
set — while the run is in flight. On the chat ingress the conversation arrives
as the `X-Hermes-Session-Id` header; every run route and filter calls it
`conversation_id` — same value, trimmed, same 256 bound. Runs without a conversation id are not
reattachable (there is nothing to key on); the app must always send one.
A same-prompt resend that lands after the
run settled starts a new run rather than replaying the terminal one: that is
the right semantic (a completed turn resent is a new turn), and the 429 path
only names in-flight runs. Two identical POSTs racing when idle can still 429
the loser when the winner has not begun yet — a retry then reattaches, so the
app retries a 429 that names its own prompt once before showing it. A 429 names
a run to watch, not a cue to retry POST on a timer: back off and follow the
named run's `/events` instead — `Retry-After: 5` is a hint, not a poll
interval. Same text
with different images is a different turn
(429, not a reattach).
Model, provider and reasoning effort are turn setup, not part of the key: a
model switch mid-turn reattaches to the running model's run rather than
starting a new one, and the snapshot carries the model actually running so the
client can see which one answered.

An older client ignores the snapshot frame (unknown keys are skipped by the app)
and still sees ordinary chat chunks. A run that settled before the client
subscribed replays its final output as one ordinary delta chunk so that client
gets an answer rather than an empty stream.

Wire shapes for the app parser: the snapshot frame omits empty `output` /
`reasoning` (`omitempty` — absent means "nothing yet"), while the list/detail
records always emit `id`, `profile`, `status`, `output`, `error`, `seq`,
`created_at`, `updated_at`, `finished_at` (`0` means "not finished") and
`tools`, and emit `conversation_id`, `prompt`, `provider`, `model`, `effort`,
`reasoning`, `usage` only when set. The snapshot carries `id`,
`profile`, `conversation_id`, `status` and `seq` on every frame when set (an
untracked run has no conversation to name, matching the non-reattachable rule
below) — no second round trip to learn ownership; the prompt preview lives
only in the record. A running snapshot omits `finished_at` (unset until the
run settles); the record always emits it with `0` meaning "not finished".
Parse leniently: tolerate a missing key wherever the other shape sends an
empty value.

Two accepted asymmetries for pre-`agento_run` clients: a `cancelled` run
replays as partial output + `[DONE]`, which an old client reads as a
completion — only the snapshot status tells them apart — while a buffered
(non-streaming) old client CAN tell them apart via the 409 `run_status`.
Same cancelled run, distinguishable buffered, indistinguishable streaming:
the app codes the streaming path against the snapshot status, never the
terminator. And a run named by a
429 may already be terminal when the client reattaches; the snapshot status is
authoritative, live frames are not promised.

Buffered (non-streaming) clients tell terminal states apart by `param` + HTTP
status, not message text: `cancelled` and `interrupted` are both 409 with
`param: "run_status"` — "the run was cancelled" vs "the gateway restarted
while this run was in flight; the partial output is preserved". Both strings
are stable contract; failures are 502 `upstream_error`.

Error frames end the stream with EOF and no `[DONE]`, the same contract as
the chat surface's `writeStreamError`: the app treats a closed stream without
a terminator as failure, on both the live and the reattached path — with one
exception. A stream dropped for backpressure (the server sheds a subscriber
that stops draining) also ends EOF mid-run with the run still `running`: that
is not failure, it is a cue to reattach. The snapshot on reattach carries
everything the dropped stream missed, so the rule is: EOF + run terminal =
failure, EOF + run running = reattach and continue.

Verb responses carry action states, not run states: `POST .../steer` answers
`{"ok":true,"status":"queued"}` and an accepted `POST .../cancel` answers
`{"ok":true,"status":"cancelling"}` — neither is a `runs.Status`, and the app
must not decode them as one. A cancel against an already-settled run answers
the run's real terminal status instead. Poll `GET /api/runs/{id}` or watch
the events stream for run states; read the verb responses only as
acknowledgements.

### Stop is a server-side cancel

Cancel marks the run, sends Pi's `abort` (via `Send`, because the abort reply
arrives only once the session is idle), keeps the text produced so far, and
ends the stream with an explicit `{"agento_run": {"status": "cancelled"}}`
frame. The app must call this endpoint, not just close its socket: closing the
socket is exactly the case this feature exists to make non-destructive.
Precedence is user-intent-wins: a turn that errors and is cancelled
concurrently records `cancelled` with no error text. The agent's failure is
dropped rather than merged — a failed turn the user happened to cancel reads
as a cancel, which is what the user asked for.

## Consequences

- A turn now outlives its request: disconnecting mid-turn no longer stops it.
  A client that wants to stop work must cancel explicitly.
- One run at a time per profile remains the concurrency limit, and the 429 is
  informative rather than a bare refusal.
- `/api/runs` in the app must use the profile path (`/p/chat/api/runs`);
  unprefixed `/api/*` through the proxy belongs to health-api.
- Run output is capped (256 KiB output, 64 KiB reasoning, 4 KB prompt
  preview, 32 tool names, 100 runs per list), live and stored alike. Live
  buffers fill partially rather than dropping whole chunks at the boundary;
  past the cap the snapshot carries the head while live deltas flow in full.
- The list route shows previews (2 KB output, 1 KB reasoning), not full turns;
  the detail route keeps everything. The `prompt` preview is the same 4 KB in
  both shapes; only output/reasoning shrink to previews. Previews carry the
  `… (truncated)` marker once the run settles, like every terminal shape;
  in-flight previews stay unmarked heads with more deltas still coming. A truncated value ends
  with the marker in every terminal shape — snapshot, list
  preview, detail record, replay delta, buffered body — but never mid-flight:
  while the turn runs, the head is incomplete by construction and more deltas
  follow, so a marker there would land mid-text. Over-cap answers are
  head-truncated everywhere, marker included: honest, and the same bytes the
  record keeps.
  Records expire after 7 days; the in-memory fallback (Mongo unreachable) instead caps at
  1000 runs with oldest-first eviction — a degraded path, not an archive.
- Persistence never blocks the turn: the executor offers each second's snapshot
  to a background loop without waiting, and the terminal write drains after it.
  The 429 names the profile's in-flight run even across conversations.
