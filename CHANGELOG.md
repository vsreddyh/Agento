# Changelog

Human-written release notes. The `## [x.y.z]` section for the version in
`android/agento/VERSION` becomes the GitHub Release body (and the in-app
updater text) — every app PR that bumps `VERSION` must add its entry here,
or the release job fails.

Entries before 3.12.1 are partial (the changelog was introduced in 3.12.1);
see GitHub Releases for older notes.

## [Unreleased]

## [3.16.0]

- Task widget redo: rounded card with dark-mode surface, header divider,
  and a scrollable task list that fills any placement at/above the 3x2
  minimum — no row cap, no "+N more", every task reachable by scroll.

## [3.15.0]

- Task widget is dynamic: 3x2 minimum, resizable both ways, with the
  row count adapting to placement height (2 rows at 3x2, up to 6),
  a "+N more" overflow line, and due labels on wide placements.

## [3.14.1]

- Tools screen no longer lists each MCP server twice: derived server
  rows now show only when the gateway doesn't already list the server
  as its own toolset (4 servers showed as 8; 5 with project-manager).

## [3.14.0]

- Tools move out of Skills into their own sidebar section, with the
  same per-assistant picker, search, filters, and sorts.

## [3.13.1]

- Sidebar scrolls: the drawer and rail no longer cut off entries on
  short screens.

## [3.13.0]

- Projects are now a shared server board: the app's Projects tab reads
  and writes the `projects` collection over `/api/projects`, and the
  assistant manages the same rows over the new `project-manager` MCP
  (Todo/Ongoing/Paused/Done, permanent). Local `tasks.json` imports once
  on first launch, then is retired.

## [3.12.1]

- Update notes render as Markdown (no raw `#` markers) and come from a
  human-written changelog — no more auto-generated commit hashes.

## [3.7.0]

- Conversations show which tools and skills were used: a live `Using …`
  indicator while the reply streams, and a per-reply `Tools:` / `Skills:`
  line that persists with history and exports.

## [3.6.1]

- Renamed the Portfolio section to Resume and Portfolio (labels only).

## [3.6.0]

- Skills screen split into Skills (Default vs Custom) and Tools (Default
  vs Custom MCP), each with search, origin filters, and sorts.

## [3.5.3]

- Chat header subtitle ellipsize; message input rides above the keyboard.
