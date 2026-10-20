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
- every call is audit-logged, allowed or not, with redacted arguments.

A held call blocks only itself; other traffic keeps flowing. Everything fails
closed: store outage, timeout, unknown tool, unknown server — all resolve to
"not executed".

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

Two response-code contracts the app relies on. Reading an expired row is a
**404** — it is no longer in the live queue — while deciding that same row is a
**409**: the decision arrived after the hold ended. The 409 means "too late",
not "someone else decided", and the app refreshes rather than attributing an
action to another user. In the audit trail, `disposition=allowed` with a
non-empty `error` means the tool ran and returned an error: `error != ""` is
the tool-level failure signal, so a reader can separate "ran and failed" from
"never ran" without parsing the payload. Secrets hygiene from the
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
