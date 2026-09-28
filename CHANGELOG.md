# Changelog

Human-written release notes. The `## [x.y.z]` section for the version in
`android/agento/VERSION` becomes the GitHub Release body (and the in-app
updater text) — every app PR that bumps `VERSION` must add its entry here,
or the release job fails.

Entries before 3.12.1 are partial (the changelog was introduced in 3.12.1);
see GitHub Releases for older notes.

## [Unreleased]

## [3.20.0]

- Skills/Tools UI: rows tap to expand the full description and complete
  tool list, each assistant gets a totals header (skills / toolsets /
  tools / MCP servers) as a context-load proxy, explicitly-off toolsets
  stay visible with an Off badge, and an explicit `configured: false`
  adds a "Not configured" badge (invisible otherwise).
- All displayed times are pinned to IST: chat/thread/project stamps, task
  today-comparisons, due-reminder parse/compare, and the Settings last-sync
  stamp (previously raw UTC ISO) render through one `Asia/Kolkata` zone.

## [3.19.0]

- Chat feel and control: auto-scroll pins to the latest message (a manual
  scroll-up unpins and reveals Jump-to-latest), Regenerate resends without
  the last reply, stopped replies are marked with Continue + Regenerate,
  and the composer stays live mid-stream with Queue send.
- Message substance: assistant text is selectable, reasoning traces render
  in a collapsible Thinking section, every reply carries its serving model
  in the meta line, and user messages support edit-and-resubmit (Replace
  with drop-turns confirmation, Fork to a new thread).
- Deferred: code-block copy buttons (needs a version-pinned mikepenz
  components override) and the TalkBack/keyboard/reduced-motion pass.

## [3.18.0]

- Usage display: every assistant reply carries its token report
  (`~1.2k tok`, persisted per message), and each thread shows a header
  with the thread total, current context size, counted turns, and
  replies without reports. The header reconciles against the gateway
  session total (the multi-device truth) and notes the first-turn
  baseline (SOUL + skills + tools). Settings totals gain a cached-tokens
  row. No cost UI — the gateway reports no real pricing yet.

## [3.17.1]

- Chat reuses one stable gateway session per conversation thread instead
  of minting a session per turn, so server-side titles, token/cost
  accounting, and compression read per conversation. Tool/skill chips
  still backfill correctly: the post-turn fetch slices to the last turn
  only.

## [3.17.0]

- Widget completes tasks inline: a ring button on every row finishes the
  task without opening the app, row taps deep-link into the detail sheet,
  and the header cycles Open/Done/All per placement. Display settings
  (comfortable/compact density, due line) live behind a tap on the title.
- Task Manager groups rows by day (Overdue/Today/Tomorrow/This week/
  Later/No due date) behind a Day groups chip, and a bell toggles
  due-time reminders that fire a notification deep-linking to the task.

## [3.16.1]

- Task widget no longer risks a load error on slow/cold starts: the
  collection factory's fallback fetch is capped at 10s and never throws
  (it runs on the AppWidget binder thread, where the shared 30s
  timeout could stall the host bind).

## [3.16.0]

- Task widget redo: rounded card with dark-mode surface, header divider,
  and a scrollable task list that fills any placement at/above the 3x2
  minimum — no row cap, no "+N more", every task reachable by scroll.
- Task Manager redo: compact rows (checkbox + name + one due line,
  overdue in red) with a detail bottom sheet holding the full
  description, due, estimate, repeat, and history plus
  Complete/Reopen, Edit, and Delete. Search across name/details,
  sort by due date / name / newest / estimate, and a result count.

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
