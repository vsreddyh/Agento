# ADR-0012: Agent runs on the host in T3/opencode, not in a Pi container

- Status: accepted (#496, 2026-11)
- Issue: #457 (phase 4: #496; phase 3 was #494 + #495)

## Context

The Pi-era stack ran the agent inside the deployment: a `pi` container
with per-profile checkouts, a `pi-gateway` HTTP surface (chat turns over
SSE at `/p/*`, resumable runs, model options), gateway packages
(`internal/gateway|pi|runs|teach|skills`), in-process approval holds with
an in-app approval queue, user-taught skills, eleven MCP servers
(`task-manager`, `project-manager`, `miser-money`, `cookbook`,
`health-check`, `reports`, plus `notes`, `memory`, `browser`, `gmail`,
`calendar`), Google OAuth connectors, and app screens built on all of it
(Chat, Skills, Tools, Teach, Memory, Runs, Approvals, Questions,
Connectors, model pickers, live voice).

That shape is gone, removed bottom-up across #487–#495:

- #487: approval holds replaced by T3/opencode permission prompts; the
  in-process gate executes at once with best-effort audit.
- #490: cron jobs deleted; retention runs on host cron + stack start.
- #493: thin Android client (8.x) — chat-era app screens removed.
- #494: `pi` container, `pi-gateway`, gateway packages, chat/resumable-run
  surface deleted; T3/opencode runs on the host.
- #495: `notes`, `memory`, `browser`, `gmail`, `calendar` servers,
  connector grant store and `/api/connectors` routes deleted; their Atlas
  collections dropped.

## Decision

The agent is a host tool, not a service. T3/opencode runs from the repo
root, spawns the six kept MCP servers as local binaries
(`task-manager`, `project-manager`, `miser-money`, `cookbook`,
`health-check`, `reports` via `scripts/build-mcps.sh` → `bin/`, wired in
`opencode.json`), and reads/writes the same Atlas database as the phone.
The container stack is `health-api` + nginx proxy + one-shot `retention`
only, defined in the single `docker/docker-compose.yml`.

Docs describe this stack (`README.md` quick start,
`documentation.md` deep dive, `AGENTS.md` process rules, `vps.md`
sizing/ports). History stays where it belongs: the Pi-era ADRs below are
marked superseded and kept on disk, not rewritten.

## Supersedes (removed surfaces only)

- ADR-0001 (resumable runs): runs died with the gateway — no run
  survives anything, nothing reattaches.
- ADR-0002 (approval gates): holds are gone — confirmation is a harness
  prompt, the gate executes at once with an audit row.
- ADR-0003 (per-profile memory): the Memory screen and the
  `memory_items`/`memory_settings` store are gone with the memory server.
- ADR-0004 (taught skills): Skills/Teach screens and the save-review-run
  loop are gone with the app chat era.
- ADR-0005 (browser tasks): the browser server is gone — no navigation
  runs, so no prompt-per-navigation fires.
- ADR-0006 (live call mode): voice/live died with chat — the thin client
  is text/sync only.
- ADR-0007 (chat attachments): the composer they attached to is gone.
- ADR-0009 / ADR-0010 (connectors + connector MCP servers): grants,
  OAuth routes and the Gmail/Calendar servers are removed; no connector
  docs, env vars or collections remain.
- ADR-0011 (connection footprint): the Pi stack it measures and plans
  for (pi container, mcp-gate relays, pi-gateway stores) is gone —
  superseded in full, including the staged plan.

## Consequences

- `README.md`, `documentation.md`, `AGENTS.md` and `vps.md` document
  only services that exist: proxy `:8080`, health-api `:8001`, retention,
  Atlas, host-run T3. Port `:8643` and the `/p/*` surface appear nowhere
  except this ADR.
- The six kept servers are the complete agent surface; adding one
  follows `documentation.md` ("Extending the Stack").
- Current ADR-0008 (bulk-batch atomicity) stands as written.
