# ADR-0002: Approval gates via an MCP relay, audit in Mongo

- Status: accepted (2026-10)
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

### Enforce in a relay, not in Pi or the gateway

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
The relay speaks no MCP semantics beyond framing — ids round-trip
byte-identical, batches and server-initiated requests pass through — so it
cannot misroute a call it does not understand.

### What gates, what runs free

Deny-by-default for money and deletes, exactly as the issue scopes it:

- gate: all `miser-money` writes, every `delete_*`/`archive_*`, destructive
  rewrites (`fix_*`), and prunes that actually delete (`prune_old` with
  `dry_run=false`; a counting dry run runs free);
- free: reads and routine creates/updates (`log_meal`, `create_task`,
  `add_recipe`, `log_cook`, ...).

Gating every write would make the agent ask permission to take notes —
approval fatigue, not safety. The table is complete by contract
(`TestPolicyTableIsComplete` fails when a server gains an unclassified
tool), and reads never gate. Unknown servers and unknown tools hold.

A gated `tools/call` **notification** (no `id`) is denied inline instead of
parked: nothing is waiting for it, so an approval card would promise a
released turn that does not exist. The deny is audited like any other, with
the reason stated. Allowed notifications forward normally and audit at
forward time, since there is no response to observe.

### State in Mongo, memory fallback

`approvals` (15-minute TTL) and `audit_log` (90-day TTL) in the shared
database, through `internal/approvals` — the same Store-interface +
memory-fallback shape as runs. The relay and the gateway open the same
collections from their own processes and fall back independently. That
independence has one hard edge worth stating plainly: a *decision* needs a
store both processes see. Without Mongo the relay and the gateway hold
separate memories, so every gated call waits out its hold and expires — by
design, loudly (error-level log naming the server, expired audit row). The
cross-restart record is the convenience; the cross-process record is the
requirement. A restart expires pending rows (nothing will ever decide them)
and deletes expired ones.

Expiry is visible immediately even though TTL deletion lags: both list paths
filter `expiresAt > now`, matching the atomic decide boundary, so a row that
timed out can never be shown as pending and then 409 on decide. Because
`TimeoutApproval` marks `status=expired` without extending `expiresAt`,
`?status=expired` is only observable for the rest of that original 15-minute
window. That is deliberate: the approvals collection is the live queue, and
the **audit log is the durable history** — every expiry writes an `expired`
audit row, retained for 90 days, and that is the record to query.

### The app decides, never with secrets

`GET /api/approvals` (pending list), `POST .../approve|deny` (decision lands
exactly once; races after expiry 409), `GET /api/audit` (trail). Approvals
carry tool + redacted args + time — no credentials, no run linkage (the relay
cannot know the conversation; Pi spawns servers once, not per turn). The app
polls; a decision is a verb on an id, never a pasted body. `decided_by` is the
constant `"app"`: this deployment has one shared `PASSWORD`, so there is
exactly one principal to attribute a decision to. Per-user attribution arrives
with per-user auth, and until then the field should be read as "decided from
the app", not as an audit-grade user id.

The pending list is global by design: approvals are one queue across all six
relays, so `GET /api/approvals` filters by status only and a caller that wants
one server narrows client-side. The audit trail, by contrast, is long and
filterable (`?server=&tool=`), because history is read by cause. A server
filter on the pending list would only matter if the queue were ever sharded.
The pending poll must pass `?status=pending`: without it the route returns all
live rows, and an expired-but-not-yet-TTL-swept row is still live for the rest
of its 15-minute window (expiry is lazy; the audit trail is the history).

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

A relay that restarts expires its **own server's** pending rows at startup
(`MarkServerInterrupted`): one relay owns one server, so those rows belonged
to the process that died, and leaving them pending would show the app a card
whose approval could never forward. The gateway's global `MarkInterrupted`
still runs at its own startup for the whole collection.

The sweep is queue hygiene, not a decision: it writes no per-row audit entry
(the count log is the record), because the rows it finds were owned by a
process that died before it could record anything. The teardown path is where
an abandoned hold gets its own `expired` row; the sweep covers what a SIGKILL
left behind.

The sweep assumes this deployment's one-profile contract (one relay per
server). If two profiles ever shared a server name, a restarting relay would
expire the other's live hold — an approve that forwards nowhere. Supporting
that would need a per-relay instance token on each row and a liveness check,
not a broader server-scoped sweep.

A `relay_`-prefixed error marks a decision the relay made itself, not one a
tool or a human made: `relay_protocol` (unparseable `tools/call`),
`relay_notification` (gated call with no reply id), `relay_store` (store
unavailable), `relay_closing` (a gated call arrived after teardown began) and
`relay_evicted` (unanswered row left the pending table at its cap). They never reached the tool, so a tool-failure filter must exclude the
prefix — and `denied` **without** the prefix means a human said no. The same
convention applies to the synthetic tool name `(malformed)`: it is the audit
entry for a frame the relay could not parse, and the app may show it as such
rather than as a real tool.

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

- Fixed: unattended money writes, deletes, prunes and rewrites now need an
  explicit human decision; every proxied call is attributable in the audit
  log for 90 days; deny leaves state unchanged (Pi gets a tool error and
  the turn continues).
- NOT fixed: Pi built-in tools (`bash` file writes, `read`/`write`/`edit`
  via codemode) never traverse MCP, so they are observed (tool frames) but
  not gated — a model told to `rm -rf` via bash is unimpeded. The agent
  still runs as root in the container, and the single bearer token still
  authenticates every route. Full sandboxing and per-connector OAuth vaults
  are out of scope (connectors issue owns the vault).
- Residual: a 14-minute held call occupies its turn until expiry; approvals
  show tool + args + time but not which conversation asked.

## Consequences

- `pi/mcp.json` points every server through `mcp-gate` (built in the image
  alongside the servers); the entrypoint `pi mcp list` gate passes through
  it unchanged.
- The app needs an approvals surface (pending cards + approve/deny) and an
  audit screen — second PR in this stack.
- Adding an MCP tool now requires a policy row, enforced by test.
