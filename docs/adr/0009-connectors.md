# ADR-0009: First connectors (Gmail, Calendar, Drive)

- Status: accepted (2026-10, v1 foundation)
- Issue: #316

## Context

Health Connect sensor sync is the only integration. Grok/Dots/Claude all
reach user data stores; Agento cannot read an email, check a calendar, or
touch a Drive doc. The auth model is one shared PASSWORD with no
per-connector OAuth and no read vs write scopes.

## Decision

### v1 scope: three connectors, draft-only where it matters

- Gmail: search/read + draft. Sends refused outright.
- Calendar: list/upcoming + draft event. Creates/invites refused outright.
- Drive: read/list + save agent-produced file. Deletes gate (backstop).
  Ungated-save scope: create-only stays free; if the wired server's save can
  overwrite an existing file by id/path, that is a destructive write and the
  follow-up must gate it (`save_file: true`) — overwrite slips between the
  free-read and gated-delete rules otherwise. Confirm against the real tool
  semantics when the connector MCP servers land.

Draft-only is a refusal, not a gate: the relay (`cmd/mcp-gate`) answers Pi
with `connectors.RefusalMessage` ("create a draft instead") and audits a
`connector_draft_only` deny. No approval card is parked for a refused send.
The `approvals.gatedTools` rows for the same tools are `true` as a backstop
for any caller that bypasses the relay check: a send must never run free.

Reads and Drive saves are intentionally ungated (`false`): reads never gate
anywhere in this stack (ADR-0002 — gating search would make the agent ask
permission to look, which is approval fatigue, not safety), and a Drive save
is the routine-create side of the same rule (like `log_meal` or
`create_task`: the agent writing at the user's direction, not moving money
or deleting). The dangerous directions — sends, invites, deletes — refuse
or gate. If exfiltration-via-read becomes a concern, it belongs to a
scoping/per-connector-approval pass, not to gating search in v1.

`Disconnect` upserts rather than no-ops on never-connected ids: List
synthesises disconnected rows anyway, so the stored row just makes the rest
state explicit. Harmless at three rows; creating rows is accepted.

### Token placement

OAuth refresh tokens live **server-side in Mongo** (`connectors`
collection, sibling to `approvals`/`runs`/`memory`), never in the app and
never pasted in chat. Reads project only the display shape
(`_id/displayName/scopes/status/lastError/connectedAt/updatedAt`); the
`refreshToken`/`accessToken` columns are unset on disconnect and never
selected into API responses. Without Mongo both sides fall back to memory
that cannot meet — same contract as approvals: the list still answers, but
nothing survives a restart and no token can be stored.

Disconnect stops future access (token columns `$unset`). Already-fetched
data — chat history, saved files — is NOT wiped: disconnect stops the tap,
it does not rewrite the past (cf. Dots delete-the-Dot semantics, explicitly
not adopted for history).

### Surfaces

- `GET /api/connectors` — three rows, disconnected synthesised when no
  document exists (a fresh deploy lists three disconnected connectors, not
  an empty list).
- `POST /api/connectors/{id}/disconnect` — clears tokens server-side.
- `POST /api/connectors/{id}/connect` — v1 answers **501**: OAuth lands in
  the follow-up. The app treats it as "needs setup", never as a prompt to
  paste a secret. Expired rows carry a `reconnect` hint with the same
  no-secrets wording — but no v1 writer sets expired (only Disconnect and the
  501 connect exist; SetStatus has no route), so the hint path is wired and
  covered but dormant until the OAuth follow-up drives it.
- `@-mention` in-chat attach and the real OAuth exchange are follow-ups, not
  this PR. The relay refusal and the list/disconnect shapes are what they
  build on.

### What this does and does not fix

- Fixed: per-connector status/scopes, independent disconnect, send paths
  that cannot run, a documented token home, and an expired state that
  prompts a reconnect instead of a password in chat.
- NOT fixed: no real Google OAuth yet (connect 501s), no connector MCP
  servers wired in `pi/mcp.json`, no app Connect screen, no per-user auth
  (single PASSWORD still authenticates everything), Pi built-in tools still
  ungated by the relay.

## Consequences

- `internal/connectors` owns ids/scopes/status/Store (memory + Mongo,
  `FromEnv`, display-only projection).
- `internal/gateway` serves the three routes; `cmd/pi-gateway` wires the
  store with the same memory fallback + index pattern as the siblings.
- `cmd/mcp-gate` refuses draft-only sends before the approval check.
- App Connect UI, OAuth exchange, and `mcp.json` wiring arrive stacked on
  top — this PR is the shape they fill in.
