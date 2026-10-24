# ADR-0010: Connector bridge to the official Google Workspace MCP servers

- Status: accepted (2026-10, first connectors)
- Issue: #316 (follow-up to ADR-0009, stacked on #367)

## Context

ADR-0009 landed the OAuth grant pipeline (connect URL → callback →
sealed tokens → connected) but no MCP servers. The first attempt built
first-party Go servers against the Gmail/Calendar REST APIs — then Google
published official remote MCP servers (`gmailmcp.googleapis.com`,
`calendarmcp.googleapis.com`, Developer Preview), and the custom servers
were deleted in favour of them: the official servers are the API Google
will extend, and matching their tool names keeps the agent on documented
ground.

## Decision

One stdio bridge (`cmd/mcp-bridge`, `--server gmail|calendar`) behind
mcp-gate like every server. It speaks MCP over stdio to Pi and forwards
tool calls to the official endpoints over Streamable HTTP with the
vault's access token attached — so the in-app connect flow stays the
single consent and the sealed-token vault stays the single credential
home (option A; Pi-native OAuth would orphan the vault for a second
token home and a headless sign-in ceremony).

- Gmail exposes the official read/draft subset: `search_threads`,
  `get_thread`, `get_message`, `list_drafts`, `list_labels`,
  `create_draft`. Label writes are filtered at the bridge — the official
  server has no send tool, so Gmail is draft-only by construction.
- Calendar exposes `list_events`, `get_event`, `search_events`,
  `suggest_time`, `list_calendars` plus bridge-local
  `create_event_draft` (validated, sent nowhere). The official server's
  real writes (`create/update/delete/respond`) are filtered at the bridge
  AND refused at the relay, so the token cannot write even if both
  checks vanished.
- Tool specs are proxied from the official `tools/list` at runtime (no
  static copies to drift); only names are filtered. A write tool Google
  adds that the bridge newly exposes without a rule holds for approval
  (fail-closed) until classified — the sync obligation from #367 now
  covers Google's changelogs, not ours.
- Auth is refresh-on-use with persist-back (`connectors.GrantTokenSource`:
  decrypt → refresh on expiry → seal the new pair). No refresh worker.
  401/403 from the official endpoint is a reconnect error, never provider
  text. A failed persist-back write fails the call rather than running
  on a grant the store no longer matches.

### Grant scopes match the official servers, not a hand-rolled client

`GoogleScopes(Calendar)` carries the official triple
(list + freebusy + events read): fewer scopes 403 inside proxied calls,
more scopes are consent without purpose. Gmail's pair already matches.
The OAuth client must carry Google's "AI-powered agent" flag (separate
client if the existing one serves non-agent features); the redirect URI
stays ours.

## Operator prerequisites (human steps, VPS + Cloud console)

1. Join the Google Workspace Developer Preview Program.
2. Enable `gmailmcp.googleapis.com` + `calendarmcp.googleapis.com`
   (plus `gmail`/`calendar-json` APIs) in the Cloud project.
3. Flag the OAuth client for AI-agent use (or mint a separate one and
   rotate the four `GOOGLE_*` values together).
4. Live verification: with a real grant connected, one `curl` with a
   vault access token against `.../mcp/v1` (`tools/list`) must 200 —
   Google rejecting third-party-client tokens would 401 here and send
   the design back to Pi-native OAuth. This is unverified in CI (no
   grant, no Preview membership); it is a merge gate done by hand.

## Consequences

- `cmd/mcp-bridge` owns the stdio↔HTTP translation (SSE framing,
  bearer attach, write-filter, local draft). Google owns the API
  surface; the bridge owns the glue (~spec drift lives here).
- `gatedTools`/`IsDraftOnlySend` name official tool names exactly;
  `TestPolicyTableIsComplete` pins the exposed sets.
- Google mandates prompt-injection screening for these servers: ours is
  the relay's existing Redact/ScrubError + approval gates, documented
  rather than outsourced to Model Armor.
- Calendar writes still have no path; app Connect screen and per-grant
  audit rows stay follow-ups.

## What this does not fix

- If Google rejects vault-minted tokens at the MCP endpoint (audience /
  client binding), the bridge 401s on every call and the design falls
  back to Pi-native OAuth — see the verification step above.
