# ADR-0014: Gmail write surface is label changes only

- Status: accepted (2026-11-15)
- Supersedes part of: ADR-0013 (which made the server read-only)

## Context

ADR-0013 pinned `workspace-mcp` to `--read-only --tools gmail --tool-tier
core`: three read tools, `gmail.readonly`, no mail mutation of any kind. The
push for that was "no write surface means nothing to gate".

In practice the missing verb was one: **mark a message read**. That is a label
change (`remove_label_ids: ["UNREAD"]`), and there is no Gmail scope narrower
than `gmail.modify` that permits it — `gmail.readonly` cannot, and Google has
no "read + mark-read" scope. So the choice was a binary one: either the agent
can never mark mail read, or it takes `gmail.modify`, which also permits
archive, trash and custom labels.

## Decision

1. **Scope: `--permissions gmail:organize`.** Upstream's level expands to
   `gmail.readonly` + `gmail.labels` + `gmail.modify` — the least that can
   mark a message read. No compose, send, settings or `mail.google.com`.
2. **Tools: `--tool-tier complete` plus an explicit block list.**
   `--disabled-tools send_gmail_message draft_gmail_message manage_gmail_label
   manage_gmail_filter` is passed last (its `nargs="+"`), so the registered
   surface is eight read/utility tools plus exactly two writes:
   `modify_gmail_message_labels` and `batch_modify_gmail_message_labels`.
   Verified over MCP stdio against the pinned upstream version.
3. **Both writes are `ask`-gated** in `opencode.json`
   (`*modify_gmail_message_labels*`, `*batch_modify_gmail_message_labels*`),
   and `TestWorkspaceMCPWriteSurfaceStaysNarrow` pins the argv, the disabled
   list, the absence of `--read-only`, and the presence of both `ask` patterns
   (`workspaceMCPWriteTools` in `opencode_perm_test.go`).

Defence in depth is deliberate, with one honest gap: the token carries no
scope that could send, draft, manage filters or permanently delete, so those
stay unreachable even if a future upstream version renamed a tool out of the
block list. Label *management* is the exception — `gmail.labels` comes with
`organize` — so `manage_gmail_label` is held back by the block list alone.

## Consequences

- **Consent must be upgraded** whenever the scope set changes: a token minted
  under ADR-0013 has no `gmail.modify`, so the first write call returns an
  Authorization URL instead of acting. The headless flow is unchanged
  (documentation.md → "Google Workspace (Gmail)").
- `gmail.modify` is label-level, not item-level: trash is recoverable, but the
  agent can now archive or trash mail without a human typing anything except
  the harness prompt. Treat an `ask` prompt for a batch as a delete-shaped
  decision and state the count and query.
- Read-only is gone as a *claim*; the ADR-0013 language ("only reads", "no
  write/send/delete surface at all") no longer holds and is superseded here.
  The scopes are still narrow enough that send/draft/filters cannot be
  reached without a further decision, which would be ADR-0015.
- The 7-day Testing-mode refresh-token expiry is unchanged, so re-consent
  cadence stays weekly.
