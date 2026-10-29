# ADR-0009: First connectors (Gmail, Calendar)

- Status: accepted (2026-10, v1 foundation)
- Issue: #316

## Context

Health Connect sensor sync is the only integration. Grok/Dots/Claude all
reach user data stores; Agento cannot read an email or check a calendar.
The auth model is one shared PASSWORD with no
per-connector OAuth and no read vs write scopes.

## Decision

### v1 scope: two connectors, draft-only where it matters

- Gmail: search/read + draft. Sends refused outright.
- Calendar: list/upcoming + draft event. Creates/invites refused outright.

Draft-only is a refusal, not a gate: the relay (`cmd/mcp-gate`) answers Pi
with `connectors.RefusalMessage` ("create a draft instead") and audits a
`connector_draft_only` deny. No approval card is parked for a refused send.
The `approvals.gatedTools` rows for the same tools are `true` as a backstop
for any caller that bypasses the relay check: a send must never run free.

Reads are intentionally ungated (`false`): reads never gate
anywhere in this stack (ADR-0002 — gating search would make the agent ask
permission to look, which is approval fatigue, not safety).
The dangerous directions — sends, invites, deletes — refuse
or gate. If exfiltration-via-read becomes a concern, it belongs to a
scoping/per-connector-approval pass, not to gating search in v1.

`Disconnect` upserts rather than no-ops on never-connected ids: List
synthesises disconnected rows anyway, so the stored row just makes the rest
state explicit. Harmless at two rows; creating rows is accepted.

### Token placement

The PKCE verifier rests **plaintext** in `oauth_states` by necessity — the token
exchange must present the exact verifier, so it cannot be hashed at rest, and a
hash would add no security against anyone who can already read the row's sibling
columns. The mitigation is lifetime, not secrecy: 10-minute TTL, single-use
consume-on-read, and no API surface returns it (only the server that created it
ever reads it back). Treat a leaked verifier like a leaked pending state: it can
only complete a consent the user is already finishing.

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

Anonymous callback load is accepted, not overlooked: the unauthenticated `GET /api/connectors/callback` lets any bearer-less caller spend one indexed `FindOneAndDelete` on an attacker-chosen id. 128-bit single-use states make exchange-on-guess infeasible, so this is DB-load-only, never a grant risk — and a per-IP throttle would add more unauthenticated surface than it removes at this cardinality. Revisit if callback traffic ever stops looking like taps.

Disconnect stops future access (token columns `$unset`) with no audit row: the 90-day audit trail covers relayed tool calls, not connector lifecycle, so `updated_at` + container logs (`disconnected (tokens cleared)`, transition-only) are the intended "who turned it off and when" trail in v1. The OAuth follow-up — which writes real grants on these same verbs — must add grant lifecycle to the audit trail rather than copy this gap. Already-fetched
data — chat history — is NOT wiped: disconnect stops the tap,
it does not rewrite the past (cf. Dots delete-the-Dot semantics, explicitly
not adopted for history).

Disconnect during the seconds-long exchange window loses to the in-flight
callback — and that is accepted, not overlooked. The pending-state scrub
(`DeleteMany` on disconnect) kills attempts still awaiting consent, but a
callback that already consumed its state (`ConsumeState → Exchange →
SetTokens → SetStatus(connected)`) holds no row for the scrub to reap: a
disconnect landing mid-`Exchange` clears tokens, then the callback resumes
and flips the row back to connected. No epoch closes this without
conditional writes on both backends (a check-then-act re-read would only
narrow the window, not close it), and the failure is operator-visible and
self-healing — the app shows connected, one re-tap disconnects — so the
window is documented, not engineered away. Triage must read a resurrect as
this race, not a scrub regression.

Expired retains the refresh token: expiry means the access token aged out,
not that the grant is known-bad, and the follow-up's silent-refresh attempt
needs the refresh to try with. The reader must never use an expired row's
tokens for data access — only the refresh attempt may. Tokens live until
disconnect/revoke with no TTL (unlike approvals 15m / audit 90d): a future
retention pass must not add a TTL to this collection.

### Surfaces

- `GET /api/connectors` — two rows, disconnected synthesised when no
  document exists (a fresh deploy lists two disconnected connectors, not
  an empty list).
- `POST /api/connectors/{id}/disconnect` — clears tokens server-side.
- `POST /api/connectors/{id}/connect` — mints a one-time state+PKCE pair,
  records the pending attempt (10-minute TTL), and returns the Google
  consent `auth_url`. Without full OAuth config answers **501** like the v1
  foundation. The app opens the URL in a browser and polls; it never sees a
  code, verifier, or token.
- `GET /api/connectors/callback?code=&state=` — single registered redirect
  URI for both connectors (the connector travels inside the state row,
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
  still cannot read mail/calendar — the grant pipeline lands first so
  servers never touch an unsealed secret), no app Connect screen (Settings
  opens the auth_url from the API today), no per-user auth (single PASSWORD
  still authenticates everything), Pi built-in tools still ungated by the
  relay.

## Consequences

- `internal/connectors` owns ids/scopes/status/Store (memory + Mongo,
  `FromEnv`, display-only projection).
- `internal/gateway` serves the four routes (list, disconnect, connect,
  callback); `cmd/pi-gateway` wires the store with the same memory
  fallback + index pattern as the siblings.
- `cmd/mcp-gate` refuses draft-only sends before the approval check.
- App Connect UI and `mcp.json` wiring arrive stacked on top — this PR is
  the shape they fill in. Remaining follow-ups are exactly those two; the
  grant pipeline lands here (AuthURL → callback → exchange → sealed
  tokens → connected).
