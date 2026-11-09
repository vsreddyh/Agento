# ADR-0002: Approval gates via an MCP relay, audit in Mongo

- Status: superseded in substance by ADR-0011 (2026-11) for the
  process-isolation claim, by the T3 migration (#487, 2026-11) for the
  hold flow itself, and by the pure removal (#503, 2026-11) for the
  wrapper and audit trail: holds are gone entirely — no park, no poll,
  no wait, no audit rows.
- Superseded by: [ADR-0012](0012-t3-opencode-migration.md) (#496, 2026-11) as it relates to the removed Pi surfaces; kept on disk as history, not rewritten.
- Scope note (#503, 2026-11): nothing below this header describes live
  code. The relay, the hold flow, the in-process gate, the policy table,
  the `approvals`/`audit_log` collections and the sweep are all deleted;
  present-tense sentences in the sections below ("the wrapper executes",
  "the gate denies", "servers open the same collections") are relay-era
  record, not runbooks. The live gate is the `opencode.json` permission
  prompts plus `mcp.AddTool` in each server main — no audit row is
  written anywhere.
- Issue: #313

## Context

One shared `PASSWORD` authenticates everything, the agent runs as root in
the pi container, and no tool call asks first: money writes, deletes and
future external sends execute the moment the model calls them. Pi offers no
pre-execution hook — its tool events are post-hoc notifications, the MCP
servers run as Pi-spawned stdio subprocesses, and the gateway only relays.
"Confirm first" existed as prompt text in `AGENTS.md`, which is instruction,
not enforcement.

## Decision

> Relay-era record: ADR-0011 Stage 2 deleted the `mcp-gate` relay — enforcement
> became an in-process library inside each server (`internal/approvalgate`).
> The pump / framing / batch / teardown-join mechanics below describe the
> relay as built. T3-era (#487) then removed the hold flow the relay had
> established: no park, no poll, audit every known call including reads.
> Pure removal (#503) then deleted the wrapper and audit trail entirely:
> the file below no longer exists. Read the current `opencode.json`
> permission rules for the live gate — the harness prompts, the servers
> execute at once, no audit row is written.

### Enforce in a relay, not in Pi or the gateway

(Relay-era: this section describes the deleted relay. The enforcement
point was `internal/approvalgate.AddTool` in each server main until #503
deleted it; the live gate is `mcp.AddTool` plus the `opencode.json`
permission prompts.)

`cmd/mcp-gate` sits between Pi and each MCP server (`pi/mcp.json` points
every server at it; it spawns the real binary). It relays newline-delimited
JSON-RPC both ways:

- reads and routine writes forward untouched (same bytes, same ids);
- a sensitive `tools/call` holds: the relay records a pending approval and
  waits up to 14 minutes for a decision, then forwards on approve or answers
  Pi with a tool error on deny/expiry — the turn continues, nothing is
  killed;
- every `tools/call` is audit-logged, allowed or not, with redacted arguments.
  The guarantee is scoped to tool calls, not every frame: `initialize`,
  `tools/list` and other protocol traffic forward without a row, and JSON-RPC
  batches (a top-level array) are out of contract — MCP removed batching and
  Pi/the SDK do not send them, so they forward untouched, unpaired and
  unaudited rather than being half-parsed.

A held call blocks only itself; other traffic keeps flowing. Everything fails
closed: store outage, timeout, unknown tool, unknown server — all resolve to
"not executed". A framing fault (an oversized line) is dropped and logged, not
treated as a dead connection: the reader drains to the next boundary and the
pair keeps serving. An oversize drop is reply-less by design — the line is
larger than the cap precisely because it could not be parsed as a frame, so
there is no id to answer, and Pi's own call timeout is the backstop. Such a
drop is also the one `tools/call`-shaped frame with no audit row: it could not
be parsed, so it has no tool name or id to record, and inventing one would
pollute the tool facet more than the log line helps. A SIGTERM
or SIGINT drives the same teardown as a Pi hangup: pumps stop, holds expire,
and the join writes their audit rows before exit.

An MCP proxy was chosen over the alternatives because there is nowhere else
to stand: Pi cannot pause a turn for approval, and the gateway only sees
tools after they ran (killing the turn then would be closing the barn door).
The relay spoke no MCP semantics beyond framing — ids round-tripped
byte-identical, batches and server-initiated requests passed through — so it
could not misroute a call it did not understand. (The in-process gate keeps
the same property from the other side: it is handed a decoded call and only
decides, never rewrites it.)

### What gates, what runs free

Deny-by-default for money and deletes, exactly as the issue scopes it:

- gate: all `miser-money` writes, every `delete_*`/`archive_*`, destructive
  rewrites (`fix_*`), and prunes that actually delete (`prune_old` with
  `dry_run=false`; pre-#487 a counting dry run ran free — post-#487 it
  prompts in T3 like a real one, since patterns cannot see arguments);
- free: reads and routine creates/updates (`log_meal`, `create_task`,
  `add_recipe`, `log_cook`, ...).

Gating every write would make the agent ask permission to take notes —
approval fatigue, not safety. The table was complete by contract; the live
pin is now `TestEveryToolClassified` in `opencode_perm_test.go`, which fails
when a registered tool is neither ask-covered nor explicitly free. Reads
never gated. Pre-#487 unknown servers held; post-#487 they denied at once.
Unknown tools were rejected by the SDK before the gate ever ran
(see ADR-0011 Stage 2).

A gated `tools/call` **notification** (no `id`) was denied inline instead of
parked: nothing was waiting for it, so an approval card would promise a
released turn that did not exist. That path is unreachable now — the SDK
never routes notifications to handlers, so the in-process gate only ever
saw a decoded call with a reply id. #503 deleted the gate outright:
servers now execute at once via `mcp.AddTool` and no denial/audit path
remains.

### State in Mongo, memory fallback

(Deleted storage: the `approvals` (15-minute TTL) and `audit_log` (90-day
TTL) collections were dropped from Atlas in #503 and the
`internal/approvals` package is gone — no server opens these collections,
and `pi mcp list` (with its gate boot probe) went with the pi container in
#494. Everything below describes the relay-era store contract, not a live
database.)

`approvals` (15-minute TTL) and `audit_log` (90-day TTL) in the shared
database, through `internal/approvals` — the same Store-interface +
memory-fallback shape as runs. The servers and the gateway open the same
collections from their own processes. That sharing has one hard edge worth
stating plainly: a *decision* needs a store both processes see. Without Mongo
every server except memory and browser exits (log.Fatalf) and the entrypoint
`pi mcp list` gate refuses boot — memory and browser fall back to unshared
in-process stores. Pre-#487 their gated calls denied immediately
(`gate_unavailable`, error-level log naming the server, denied audit row)
instead of burning the full hold — same verdict as expiry, minutes earlier.
Post-#487 calls run with an ephemeral audit trail that dies with the
process. The cross-restart record
is the convenience; the cross-process record is the requirement. A server
restart expires its own pending rows (`MarkServerInterrupted`, scoped by
server) and the gateway's global `MarkInterrupted` deletes expired ones.

Expiry is visible immediately even though TTL deletion lags: both list paths
filter `expiresAt > now`, matching the atomic decide boundary, so a row that
timed out can never be shown as pending and then 409 on decide. Because
`TimeoutApproval` marks `status=expired` without extending `expiresAt`,
`?status=expired` is only observable for the rest of that original 15-minute
window. That is deliberate: the approvals collection is the live queue, and
the **audit log is the durable history** — relay-era, every expiry wrote an
`expired` audit row, retained for 90 days, and that was the record to query.
Post-#487 there are no live holds to expire: every expiry is a startup
sweep (status flip only, no per-row audit — the count log is the record),
so sweep-expired rows leave no audit row behind.

### The app decides, never with secrets

(Deleted surface: `GET /api/approvals`, `POST .../approve|deny` and
`GET /api/audit` have zero handlers — the app approval UI went with #487
and the store behind these routes went with #503. A 404 from health-api
today means no such route exists, not an "expired row". Everything below
is relay-era contract record, not a live API.)

`GET /api/approvals` (pending list), `POST .../approve|deny` (decision lands
exactly once; races after expiry 409), `GET /api/audit` (trail). Approvals
carry tool + redacted args + time — no credentials, no run linkage (the gate
cannot know the conversation; Pi spawns servers once, not per turn). The app
polls; a decision is a verb on an id, never a pasted body. `decided_by` is the
constant `"app"`: this deployment has one shared `PASSWORD`, so there is
exactly one principal to attribute a decision to. Per-user attribution arrives
with per-user auth, and until then the field should be read as "decided from
the app", not as an audit-grade user id.

The pending list is global by design: approvals are one queue across all
servers, so `GET /api/approvals` filters by status only and a caller that wants
one server narrows client-side. The audit trail, by contrast, is long and
filterable (`?server=&tool=`), because history is read by cause. A server
filter on the pending list would only matter if the queue were ever sharded.
The pending poll must pass `?status=pending`: without it the route returns all
live rows, and both expired-but-not-yet-TTL-swept and already-decided rows stay
live for the rest of their 15-minute window (expiry is lazy; the audit trail is
the history). An unfiltered list is therefore a window, never a live queue.
Post-#487 the list only ever shows pre-migration rows.

Expiry is evaluated at one instant per operation. Reads and lists use the
store's own clock; a **decision** uses the caller's decision instant (`at`),
because the row is being decided at that moment and both processes on this
host call it with `time.Now()`. The two must not be mixed inside one
operation, or a row could read as pending at one instant and be rejected as
expired at the next.

Two response-code contracts the app relies on. Reading an expired row is a
**404** — it is no longer in the live queue — while deciding that same row is a
**409**: the decision arrived after the hold ended. The 409 means "too late",
not "someone else decided", and the app refreshes rather than attributing an
action to another user. In the audit trail, `disposition=allowed` **or `approved`** with a
non-empty `error` means the tool ran and returned an error: `error != ""` is
the tool-level failure signal, so a reader can separate "ran and failed" from
"never ran" without parsing the payload.

(Relay-era: the relay swept at startup and converged through its teardown
join. Post-Stage-2 each server runs the same per-server sweep itself.)

A server that restarts expires its **own server's** pending rows at startup
(`MarkServerInterrupted`, called in each `main` before serving): it owns
that server's queue, so those rows belonged to the process that died, and
leaving them pending would show the app a card whose approval could never
forward (pre-#487; post-#487 only pre-migration rows can be pending, so no
live hold is ever at risk). The gateway's global `MarkInterrupted` still runs at its own
startup for the whole collection. There is no teardown join anymore: the
hold blocked the handler goroutine, and `ctx.Done` expired it with audit;
post-#487 a dead context denies before anything executes instead. The
sweep covers what a SIGKILL left behind.

The sweep writes no per-row audit entry (the count log is the record),
because the rows it finds were owned by a process that died before it could
record anything.

The sweep assumes this deployment's one-profile contract (one server
process per id). If two profiles ever shared a server name, a restarting
server would expire the other's rows — pre-#487, a live hold killed and
an approve that forwards nowhere. Supporting that would need a per-process instance token on each
row and a liveness check, not a broader server-scoped sweep.

A `gate_`-prefixed error marked a decision the gate made itself, not one a
tool or a human made: `gate_unavailable` (unshared store) and `gate_store`
(store unavailable) — both pre-#487 strings with no producer anymore. The relay-era `relay_protocol` / `relay_notification` /
`relay_store` / `relay_closing` / `relay_evicted` errors — and the synthetic
`(malformed)` tool for unparseable frames — no longer exist: without the
relay there is no framing to fault, no notification path (the SDK never
routes notifications to handlers), and no pending table to evict from. Pre-#487
a `denied` **without** a prefix meant a human said no; post-#487 `denied`
is overwhelmingly gate refusals (draft-only, unknown tool, cancelled
context), and a human tap survives only in pre-migration rows.

Error scrubbing is pattern-based, not exhaustive: `args` are redacted by key,
while `error` is free text and is only masked where it matches a
credential-shaped pattern (plus a 300-rune cap). A server that echoes a bare
secret with no recognizable prefix can still land it in a row, so the app must
treat `error` as untrusted display text, not as a safe field. Arguments are
redacted by key *substring*, so a future display field like `author` (or
`monkey`) renders as `***`: redacted args are display-only and must never be
read back as data. Known gaps in
that pattern set are short bare values (`token is abc`), an unquoted
multi-word value (`token=correct horse battery staple` masks only the first
word), and secrets with no key or scheme marker at all; quoted multi-word
values are covered. Value scrubbing cannot be complete, and the 300-rune cap
is the only hard bound. Secrets hygiene from the
issue stands: passwords/2FA never go through chat; the take-over path
belongs to #312.

### Threat note (what this does and does not fix)

(Partially stale: the T3 permission prompt is the live enforcement for
attended turns. The audit-log attribution, Pi-built-in-tools, root-in-
container and connector-vault sentences describe removed systems — #503
deleted the audit trail, Pi and the containerised agent are gone with
#494, and connectors were removed in #495.)

- Fixed: attended money writes, deletes, prunes and rewrites now need an
  explicit human decision (pre-#487: in-app tap; post-#487: T3 permission
  prompt — attended turns only; the retired scheduler's unattended fires
  had no prompter, prompt text was their only enforcement — the scheduler
  was deleted in #490); every executed call is attributable in the audit
  log for 90 days short of an audit-write outage; deny leaves state
  unchanged (the turn's harness gets a tool error and
  the turn continues).
- NOT fixed: Pi built-in tools (`bash` file writes, `read`/`write`/`edit`
  via codemode) never traverse MCP, so they are observed (tool frames) but
  not gated — a model told to `rm -rf` via bash is unimpeded. The agent
  still runs as root in the container, and the single bearer token still
  authenticates every route. Full sandboxing and per-connector OAuth vaults
  are out of scope (connectors issue owns the vault).
- Residual: pre-#487 a 14-minute held call occupied its turn until expiry;
  post-#487 nothing parks, so a stuck sensitive write is a harness prompt,
  never a hold. Approvals show tool + args + time but not which conversation asked.

## Consequences

(Historical record — the relay-era outcome below was superseded first by
#487 (holds became harness prompts) and then by #503, which deleted the
in-process gate, the policy table and the audit trail outright: servers
register tools directly with `mcp.AddTool`, no policy row or approvals
surface exists anywhere.)

- `pi/mcp.json` points straight at each server binary (ADR-0011 Stage 2
  deleted the `mcp-gate` relay; enforcement is in-process now); the entrypoint
  `pi mcp list` gate probes the servers directly, unchanged.
- The app needs an approvals surface (pending cards + approve/deny) and an
  audit screen — second PR in this stack.
- Adding an MCP tool now requires a policy row, enforced by test.
