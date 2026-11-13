# ADR-0013: Google Workspace (Gmail) is an external read-only MCP server

- Status: accepted (#515, 2026-11)
- Issue: #515 (`needs-decision` until the five choices below were made)

## Context

The thin client and the #494/#495 removal left the agent with six Mongo-backed
Go servers and no mail surface: `gmail`/`calendar` were first-party Go servers
wired to a connector grant store, and they went with the connector era. #515
asked for real mail as a *tool* again, not `gh`/shell work.

There is no usable first-party reference server (Google publishes none, the
old Gmail reference server is gone from `modelcontextprotocol/servers`), and
every high-star mail *client* is a whole platform. The narrowest credible
candidate was `taylorwilsdon/google_workspace_mcp` (MIT, Python, `uvx
workspace-mcp`, ships a Dockerfile).

The issue was blocked on five decisions. They are now:

1. **Account** — one mailbox, configured by `USER_GOOGLE_EMAIL`; nothing in
   code assumes which address.
2. **OAuth client** — a **Desktop app** client (human-created). A desktop
   client accepts any loopback redirect, so `http://localhost:<port>/oauth2callback`
   (port 8000 unless `WORKSPACE_MCP_PORT` says otherwise) needs no registration in
   GCP; a Web-app client would need an exact URI and
   is the wrong shape for a single headless user.
3. **Install** — a repo-local venv at `.workspace-mcp/venv` (git-ignored),
   pinned version, behind `scripts/workspace-mcp.sh`. Not `uvx` (a package
   manager `init` does not install and cannot pin), not a compose service
   (stdio is one client per process; the stack is deliberately three
   containers, and a fourth would need its own port/token story).
4. **Headless consent** — SSH-forward the callback port from the laptop; the
   Authorization URL comes back in the tool result. Written up in
   `documentation.md`.
5. **Transport** — stdio, matching every other server in `opencode.json`.

## Decision

`workspace-mcp` runs as a **seventh `mcp.servers` entry**, launched by
`scripts/workspace-mcp.sh` with a fixed surface:

```
--transport stdio --single-user --read-only --tools gmail --tool-tier core
```

Three tools result — `search_gmail_messages`, `get_gmail_message_content`,
`get_gmail_messages_content_batch` — requesting only `gmail.readonly`,
`openid`, `email` and `profile`. No send, delete, draft or label tool is
merely *ungated*: none is registered, so there is no `ask` rule to add and no
argument the harness cannot see.

Credentials: client id/secret and the account in the root `.env`; refresh
tokens in `.workspace-mcp/credentials/` (0700); both git-ignored. Nothing in
the venv or the cache is reconstructable from git, matching the rule for
other runtime state.

## Consequences

- Importing mail does **not** resurrect ADR-0009/ADR-0010: no connector grant
  store, no `/api/connectors`, no first-party `internal/gmail`. The app still
  cannot read mail; only the agent can, and only reads.
- `TestWorkspaceMCPSurfaceStaysReadOnly` (`opencode_perm_test.go`) pins the
  launcher argv, because `TestEveryToolClassified` only enumerates `cmd/*`
  servers and cannot see a Python tool list. Enabling any write tool now
  requires editing the launcher, the `ask` set and that test in one change.
- The OAuth consent screen stays in **Testing** (avoids Google's
  verification/CASA assessment for a restricted scope), so refresh tokens
  expire after 7 days and re-consent is a routine, documented operation.
- `init` installs the venv best-effort: the container stack does not depend on
  it, and an offline host still gets a working stack plus a loud message.
- Scope stayed Gmail. Calendar/Drive would be `--tools calendar drive` (or a
  wider tier) plus their own decisions about writes; this ADR does not cover
  them.
