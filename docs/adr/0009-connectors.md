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

Encryption: envelope is implemented (AES-256-GCM, random nonce per
value, base64(nonce||ciphertext) in Mongo). The key lives in
CONNECTORS_ENCRYPTION_KEY (64 hex chars, server-side only); without it the
OAuth config refuses to build and connect 501s — tokens can never land
unencrypted by omission. Atlas at-rest is the second layer, not the cover
story.

Write confirmation: the confirming read-after-write runs on a fresh budget,
but if it fails after a landed write the verb still 502s while the state
landed. That is retry-safe (a second verb succeeds idempotently), not a
divergence — worth stating so triage doesn't read it as one.

Disconnect stops future access (token columns `$unset`) with no audit row: the 90-day audit trail covers relayed tool calls, not connector lifecycle, so `updated_at` + container logs (`disconnected (tokens cleared)`, transition-only) are the intended "who turned it off and when" trail in v1. The OAuth follow-up — which writes real grants on these same verbs — must add grant lifecycle to the audit trail rather than copy this gap. Already-fetched
data — chat history, saved files — is NOT wiped: disconnect stops the tap,
it does not rewrite the past (cf. Dots delete-the-Dot semantics, explicitly
not adopted for history).

Expired retains the refresh token: expiry means the access token aged out,
not that the grant is known-bad, and the follow-up's silent-refresh attempt
needs the refresh to try with. The reader must never use an expired row's
tokens for data access — only the refresh attempt may. Tokens live until
disconnect/revoke with no TTL (unlike approvals 15m / audit 90d): a future
retention pass must not add a TTL to this collection.

### Surfaces

- `GET /api/connectors` — three rows, disconnected synthesised when no
  document exists (a fresh deploy lists three disconnected connectors, not
  an empty list).
- `POST /api/connectors/{id}/disconnect` — clears tokens server-side.
- `POST /api/connectors/{id}/connect` — mints a one-time state+PKCE pair,
  records the pending attempt (10-minute TTL), and returns the Google
  consent `auth_url`. Without full OAuth config answers **501** like the v1
  foundation. The app opens the URL in a browser and polls; it never sees a
  code, verifier, or token.
- `GET /api/connectors/callback?code=&state=` — single registered redirect
  URI for all three connectors (the connector travels inside the state row,
  not the path). Consumes the state (single-use: no code exchanged twice),
  exchanges server-side, seals tokens with the envelope key, flips the row
  to connected. Tokens land before the status flips — a
  connected-with-no-tokens row is never servable.
- `@-mention` in-chat attach, connector MCP servers, and the app Connect
  screen are follow-ups, not this PR. The relay refusal, list/disconnect
  shapes, and now the grant pipeline are what they build on.

### What this does and does not fix

- Fixed: per-connector status/scopes, independent disconnect, send paths
  that cannot run, a documented token home, an expired state that prompts a
  reconnect instead of a password in chat, and now the real OAuth grant
  pipeline (connect URL → single callback → exchange → sealed tokens →
  connected), with PKCE, single-use states, and tokens-before-status
  ordering.
- NOT fixed: no connector MCP servers wired in `pi/mcp.json` yet (the agent
  still cannot read mail/calendar/Drive — the grant pipeline lands first so
  servers never touch an unsealed secret), no app Connect screen (Settings
  opens the auth_url from the API today), no per-user auth (single PASSWORD
  still authenticates everything), Pi built-in tools still ungated by the
  relay.

## Consequences

- `internal/connectors` owns ids/scopes/status/Store (memory + Mongo,
  `FromEnv`, display-only projection).
- `internal/gateway` serves the three routes; `cmd/pi-gateway` wires the
  store with the same memory fallback + index pattern as the siblings.
- `cmd/mcp-gate` refuses draft-only sends before the approval check.
- App Connect UI, OAuth exchange, and `mcp.json` wiring arrive stacked on
  top — this PR is the shape they fill in.
