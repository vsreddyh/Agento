# Changelog

Human-written release notes. The `## [x.y.z]` section for the version in
`android/agento/VERSION` becomes the GitHub Release body (and the in-app
updater text) — every app PR that bumps `VERSION` must add its entry here,
or the release job fails.

Entries before 3.12.1 are partial (the changelog was introduced in 3.12.1);
see GitHub Releases for older notes.

## [4.4.4]

- DIAGNOSTIC: the widget style ladder is now C, D1–D5 and E, where each
  D step adds exactly one element to the chrome C proved renders (rows,
  divider, empty-state text, header toggle, title-to-settings tap), so
  the first failing step names the culprit on issue #137.

## [4.4.3]

- Task widget settings (view, density, due line) move into the widget's
  settings screen, now reachable from the Task Manager — the widget's own
  header controls never rendered on some launchers (#137), and the view
  name shows in the header title instead of a toggle button.

## [4.4.2]

- FIX (attempt) for the task widget load error (issue #137): the widget
  is rebuilt on the exact chrome the bisect proved renders (header text
  + refresh, concrete backgrounds), with only the row container or
  scrollable list added. The divider, header view toggle,
  title-to-settings tap and empty-state tap are gone — none of them ever
  rendered on the affected launcher. View switching and widget settings
  now live in the Task Manager screen.

## [4.4.1]

- FIX: task widget load error on some launchers (issue #137) — the
  header's view toggle is a TextView instead of a platform Button,
  whose default style resolves theme attributes against the host's
  theme when a widget is inflated. The bisect (styles A–E) showed the
  working styles contained no Button; this was the difference.

## [4.4.0]

- DIAGNOSTIC: task widget display settings gain a style ladder (A–E)
  that swaps progressively simpler widget layouts — one text label up
  to the full scrolling widget — to identify which piece a launcher
  rejects (issue #137). Default is the full widget; the styles are
  removed once the cause is found.

## [4.3.1]

- FIX: task widget load error on some launchers (issue #137) — widget
  text colors are now concrete day/night values instead of
  `?android:attr/textColor*` theme attributes, which a host that
  inflates widget layouts with a bare theme can fail to resolve.

## [4.3.0]

- Task widget: the complete-ring drawable uses a concrete day/night
  color instead of a theme attribute, and direct collection rows carry
  explicit per-row taps instead of the mutable template + fill-ins —
  both were host-sensitive pieces of the widget load path (issue #137).
- Widget display settings gain a "Scrolling list" toggle: off renders
  plain rows with no collection, template, or service bind, which
  isolates a collection-binding failure from the rest of the widget.
  Off by default while #137 is open; the collection path is unchanged
  and is the intended end state.
- Widget diagnostics (Settings → About) capture system-wide log
  alongside our own — a host-side widget failure is thrown in the
  launcher's process and is invisible in ours. Reports are capped,
  filtered to widget signatures, and secret-redacted; the report can be
  saved to a file for upload, and copy carries the same content.
  Adds a best-effort "Clear system log" action.

## [4.2.4]

- FIX: task widget load error on some launchers (issue #137) — direct
  collection rows now carry explicit per-row taps instead of the
  mutable template + fill-ins. Same taps, zero functional loss.

## [4.2.3]

- FIX: task widget load error on some launchers (issue #137) — the row
  complete-ring drawable used a theme attribute that fails to resolve
  in certain widget hosts; it now uses a concrete day/night color.

## [4.2.2]

- Task UI for narrow screens: detail sheet stacks label above value so
  nothing wraps mid-word, and the task dialog is a single scrolling
  column with section headers, quick chips (Today/Tomorrow, minutes,
  repeat presets), and a Save hint until all required fields are set.

## [4.2.1]

- FIX: task widget on Android 16 — service-backed collections broke
  there, so API 31+ devices now get rows directly via
  RemoteCollectionItems (no service bind); older devices keep the
  legacy factory path.

## [4.2.0]

- Widget diagnostics can be saved to a file and shared for upload
  (Settings → About), with a larger redacted log slice than the
  clipboard variant. Factory throwables are recorded and crash log
  lines surface first, so an on-device widget crash names itself
  without adb.

## [4.1.0]

- Widget hardening: one bad placement can no longer abort the initial
  render for the rest, and Settings → About gains a Widget diagnostics
  card (placement state plus one-tap copy of recent device log, so
  widget failures are debuggable without adb).

## [4.0.0]

- BREAKING: tasks require every field except Repeats — name, details,
  due date + time, estimate, and the parallel tickbox. The agent and app
  collect them all up front; old versions that create near-empty tasks
  get a plain error instead. Repeat-less one-shot tasks still work.
- Task due notifications carry the task: friendly due line + estimate in
  the collapsed text, description + due + estimate + repeat in the
  expanded view, and a Done action that completes inline (reuses the
  widget trampoline — no app open).
- Tasks gain a "Can run in parallel" tickbox (list badge, detail line,
  reminder text, agent tools) marking tasks that can run alongside
  other tasks.
- New/edit task dialog: due date and time are picked (calendar + clock,
  Today/Tomorrow shortcuts), not typed. Completing a task opens a
  prefilled draft with the date cleared, so recreating means picking a
  fresh date.

## [3.20.1]

- Task Manager bug fix: open tasks no longer render as completed — JSON
  `completedAt: null` was coerced to the string `"null"` by `optString`
  (all string fields now parse through a null-guarded helper), which also
  restores the Overdue/Today date grouping.
- Task Manager looks: flat rows with dividers instead of Card-per-row,
  friendly due lines (`Today, 09:00`, `Tomorrow`, `Mon 29 Sep`),
  section headers with counts, dim + strikethrough for done rows.
- Task widget: the error view names the failure reason and taps to retry,
  and one wedged placement can no longer abort the rest.

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
