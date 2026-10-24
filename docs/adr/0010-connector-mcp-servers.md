# ADR-0010: Connector MCP servers (Gmail, Calendar)

- Status: accepted (2026-10, first connectors)
- Issue: #316 (follow-up to ADR-0009, stacked on #367)

## Context

ADR-0009 landed the OAuth grant pipeline (connect URL → callback →
sealed tokens → connected) but no MCP servers: the agent still could not
read mail or calendar. The servers had to be first-party, not external:
the grant vault (sealed `TokenRow`s in Mongo, envelope key server-side)
is unreadable to a third-party server, which would bring its own OAuth,
its own token storage, and its own tool names — orphaning the consent
flow and the draft-only refusal the relay enforces by tool name.

## Decision

Two stdio MCP servers behind mcp-gate like every other (`cmd/gmail`,
`cmd/calendar`, wired in `pi/mcp.json`), calling the official Google
Gmail and Calendar APIs with the stored grant:

- gmail: `search_gmail` / `read_gmail` / `list_messages` (read-only),
  `create_draft` (compose scope). `send_message` / `send_gmail` are
  registered only so the relay refuses them with "create a draft instead"
  instead of answering an unknown tool.
- calendar: `list_events` / `get_event` (readonly),
  `create_event_draft` (agent-side object: validated, sent nowhere — the
  grant is readonly, so this tool works on exactly the grant the connect
  flow mints). `create_event` / `send_invite` are registered refusals,
  same shape as gmail sends.

Tool names match `approvals.gatedTools` and `connectors.IsDraftOnlySend`
exactly: a renamed tool would miss the refusal and fall through to the
gated backstop (fail-closed, but the wrong UX). The scope-correspondence
obligation from #367 extends here — if a server needs a broader Google
scope, the OAuth table, the tool policy, and this ADR move together.

### Auth: refresh-on-use, persist-back

No background refresher. Each server builds its Google client from
`connectors.GrantClient`: decrypt the stored grant, refresh on expiry,
seal the new pair back into the store before serving. A failed refresh is
a reconnect error ("reconnect in Settings"), never a secrets prompt.
Persist-back failure fails the call rather than running on a grant the
store no longer matches (rotation could strand the next process).

No grant (disconnected/expired, or a fresh deploy that never connected)
fails closed the same way: the server starts (Mongo + client creds are
all it needs at boot) but every tool answers reconnect guidance.

`ServerOAuthConfig` needs only `GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET`
+ envelope key — never the redirect URL, which servers never mint.

## Consequences

- `internal/gmail`, `internal/calendar` own the API clients;
  `cmd/gmail`, `cmd/calendar` own the MCP surface. Both follow the
  notes-server shape (typed inputs, schema tests, `fail`/`result`).
- Reads stay ungated (`false`); sends refuse (`IsDraftOnlySend`); the
  gated `true` rows stay as the backstop for relay bypass.
- Calendar writes still have no path: `create_event_draft` presents, the
  agent does not create. Actual creation waits for the approval-gate
  flow that covers calendar writes (still a follow-up, like the app
  Connect screen and per-grant audit rows).
