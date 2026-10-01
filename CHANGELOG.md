# Changelog

Human-written release notes. The `## [x.y.z]` section for the version in
`android/agento/VERSION` becomes the GitHub Release body (and the in-app
updater text) — every app PR that bumps `VERSION` must add its entry here,
or the release job fails.

Entries before 3.12.1 are partial (the changelog was introduced in 3.12.1);
see GitHub Releases for older notes.

## [4.12.1]

- **Task domain moved out of MainActivity** (#163, first of four moves).
  The filter/sort/bucket rules, the editor draft and its repeat validation —
  everything with no Compose in it — now live in `TaskDomain.kt`, moved
  verbatim with only `private` widened to `internal`. No behaviour changes;
  the next moves are the task UI, then the settings UI.

## [4.12.0]

- **The task widget now refreshes on its own, every 30 minutes.** It used to
  show whatever was true when it was last pushed, and the only pushes came
  from a task being changed *in the app*. So a task created or completed by
  the assistant — the entire reason the list is shared — reached the home
  screen only when the app was next opened and something else was touched.
  The widget now goes stale within half an hour rather than indefinitely,
  which is when it matters: when you are not looking at the app.
- The same periodic run re-derives the reminder alarms, so reminders for
  tasks created elsewhere are armed without waiting for you to open the app.
- It is its own scheduled job rather than something the hourly health sync
  also does. Health sync runs behind a foreground notification and retries
  with backoff when it fails; coupling the task list to it would let a
  failed health sync quietly starve the widget.
- Costs nothing when unused: both refreshes return immediately if reminders
  are off and no widget is placed.

## [4.11.2]

- **The start-time rule is defined once.** "A task starts `estimated_minutes`
  before it is due, and a zero estimate has no start of its own" lived twice —
  once in the task model and once re-derived in the reminder engine, with a
  comment in each file pointing at the other as the only thing keeping them
  in step. Both now call one function. No behaviour changes: the list still
  sorts and groups with the deadline as a zero-estimate task's start, and a
  zero-estimate task still gets no "Start now" alert.

## [4.11.1]

- **The reminder budget is no longer silent.** Past the cap, reminders were
  dropped and the alarms that were already set were cancelled, with nothing
  anywhere saying so — a task in your list that simply never alerts. What the
  last refresh had to leave out is now reported in Settings -> Widget
  diagnostics, in the exported report, and in the clipboard copy.
- **Alerts outrank nags when the budget runs out.** The cap used to keep the
  nearest fire times, which let a task due in a week take away every kind of
  alert from a task due tomorrow. A start or due alert now ranks ahead of a
  repeating overdue nag, each group nearest-first: the right thing to lose is
  the nag, because its task is still listed and still shown as overdue.
- **Live nags now count against the budget.** They used to sit outside the cap
  on the grounds that cancelling a running nag is worse than exceeding the
  cap, so the nag set grew without bound and the cap only ever applied to new
  work. They compete now, ordered by how long each task has been overdue.
- **The cap rose from 90 to 200.** It is a self-imposed guard, not a platform
  limit — Samsung's documented ceiling is 500 alarms per app — so 200 is a
  floor for correctness rather than a limit on capacity. At three points per
  task that is about 65 timed tasks, past the point where the list itself
  wants paging.

## [4.11.0]

- **Duplicate** on a task's detail sheet: one tap copies it 1:1 into a new
  open task — name, details, due date and time, estimate, the entire repeat
  and the parallel flag, all carried over verbatim. Nothing is adjusted on
  the way through: a copy is a starting point to edit, not a rescheduled
  twin, so a past due date stays past and a cadence comes across as the
  same cadence. It sits beside Delete rather than crowding the primary row,
  and a task missing anything the app itself requires says exactly which
  field — without closing the task, so the thing that needed fixing is
  still on screen.

## [4.10.0]

- **Four reminder types, as specified** (issue #160), each with its own
  message instead of one generic alert:
  - **5 minutes before the start time** — "Starting in 5 minutes · starts
    Today, 07:30"
  - **At the start time** — "Start now · due Today, 09:00"
  - **At the due time** — "Due now"
  - **Every 15 minutes past due** — "Overdue since Today, 09:00"

  Each line names the time that is actionable at that moment, which is why
  they are written separately rather than composed from one suffix.
- A reminder's expanded view now opens whenever it would add something, not
  only when the task has a description — so a task with no description still
  shows when it starts and when it is due.
- **Notifications stopped echoing the whole task.** The expanded view was
  repeating the due line, the start line, the estimate, the repeat rule and
  the parallel flag — six lines of a record already visible in the app. It
  is now the message, the task's own description, and the window it has to
  happen in. The locked-screen line names the moment and the one time that
  matters.
- The "5 minutes before due" heads-up is replaced by "at the due time", per
  the four types above, and the 5-minute lead now sits before the *start*.
- A task with no estimate gets the due alert only: there is no start time
  for the 5-minute warning to lead into, and a "Start now" alert arriving
  at the due moment would say the wrong thing.
- Reminders that would land within two minutes of each other collapse, and
  the **later** one wins: a one-minute estimate no longer fires "Start now"
  and "Due now" a minute apart, and it is "Start now" that goes, so a task
  that has just come due is never the alert that gets dropped.
- The alarm budget rose from 60 to 90, because a task now arms up to three
  points rather than two. See issue #165 for why that cap matters more than
  its size suggests.

## [4.9.0]

- New **Current** section at the top of the task list: tasks whose start
  time (`due time − estimate`) has arrived but whose due time has not — the
  window in which the work is actually meant to happen, and the moment the
  "Start now" reminder fires.
- **The horizon groups are measured to the start time**, not the due time,
  which is what a list like this is for: a task due in three hours with a
  one-hour estimate is something to start in the next hour or two, not in
  three. This matches the reminder engine's own rule, including the
  zero-estimate fallback to the due time.
- A task now belongs to the day it **starts** on, not the day it is due:
  due tomorrow at 00:30 with a one-hour estimate starts tonight, so it
  shows under tonight and becomes **Current** the moment it should begin.
  Rows predating mandatory due times — 11 of 59 when this was written —
  keep their day group (a today one lands in Later today, since nothing is
  known about when), and their detail sheet says the start needs a due time
  rather than echoing one.
- **Everything sorts by start time** (the default order; the due time
  breaks ties). Tasks with no usable due time sort last rather than first —
  an undated task is not the most urgent thing you have.
- **Rows and the detail sheet now show the start and the due time** —
  "Today, 07:00 → 09:00" — instead of the estimate, always anchored on the
  start's day, which is the day the row is grouped under. A task crossing
  midnight (due 00:30, 1h estimate) therefore reads "Today, 23:30 → 00:30"
  rather than restating the deadline's own day and contradicting the group
  above it; the detail sheet still spells that day out under "Due". A zero
  estimate has no start of its own, so the start is left out rather than
  printed twice. The estimate is now purely an input:
  it still decides when the start moment and the "Start now" reminder fall,
  it is just no longer a number you have to read.
- The home-screen widget row and reminder notifications carry the start and
  due times in place of the estimate, like the task list. The estimate is
  now shown in exactly one place: the editor, which is also where it is
  stored.
- Corrected the 2-3 hour group's label, which shipped as "Next 1-3 hours"
  and so appeared to overlap "Next hour". Every group name now matches the
  range it holds.

## [4.8.0]

- The task list now groups by **how long is left** instead of showing one
  undifferentiated "Today" pile. Today is split into **This hour**, **Next
  hour**, **Next 1-3 hours**, **Next 3-6 hours**, **Next 6-12 hours** and
  **Later today**; days from tomorrow on keep their plain day groups, where
  a time-of-day split adds nothing. The horizons stop at the end of today on
  purpose — a task due in three hours is not "tomorrow", and hiding it there
  is what makes a list untrustworthy.
- A task due earlier today now counts as **overdue** (red row, Overdue
  group) instead of sitting in "Today" as if it were still ahead. The
  reminder engine has been nagging those every 15 minutes all along, so the
  list and the alerts now agree.
- The grouping toggle reads **Group by time**, and a minute ticker re-buckets
  the list as the clock moves, so a task walks from "This hour" into "Next
  hour" on its own instead of only on the next refresh.
- The due quick-picks are out of the task editor: the date and time pickers
  are the only way to set a due moment.

## [4.7.1]

- Due shortcuts became horizons — **Next hour**, **3 hours**, **8 hours**,
  **Later today** — each setting the date *and* time together, with "Today"
  and "Tomorrow" removed (they filled a date and left the mandatory time to
  be picked by hand, so each was a dead end that looked like an answer).
  Superseded by 4.8.0, which moves those horizons out of the editor and into
  the task list as groups, and refines them into six time buckets.

## [4.7.0]

- Repeats that can be computed now roll themselves over. Completing a task
  with a structured cadence (`every 3 days`, `every month`) creates the next
  occurrence automatically — the date advances, the time and every other
  field carry over, and nobody is asked to pick a date the server can
  already work out. Completing it in the app just says which date landed.
  Custom conditions ("every 3rd Friday", "end of every month") still ask,
  because a rule with an exception in it is not something a server should
  interpret on its own.
  - Month ends clamp instead of rolling over: 31 Jan + 1 month is 28 Feb
    (29th in a leap year), not 3 March.
  - A task left overdue for months is advanced to the next *upcoming*
    occurrence, not into a new past-due one.
  - Completing a task answers exactly as before, with the new occurrence
    added under `next` — an older app build can still complete tasks
    against the new server.
- FIX: the voice widget's placement was listed in **Settings → Home-screen
  widget** as a second task widget, and could be configured as one. Widget
  ids are now cross-checked against their own provider before being shown,
  listed or configured, so only real task placements appear.

## [4.6.0]

- Task widget settings moved out of the Task Manager into **Settings →
  Home-screen widget**, its own subsection. It configures the home screen,
  not the task list, so it no longer sits under your task rows. Each
  placement is listed with what it currently shows plus a Configure
  button; with no widget placed you get instructions and an "Add the
  widget" button instead of a button that does nothing.
- Reminders now fire at the times you actually need them, and only for
  open tasks that have a due time:
  - **Start now** at `due time − estimated minutes` — the last moment you
    can begin and still finish on time.
  - **Due in 5 min**, five minutes before the due time.
  - **Overdue** — once the due time has passed, repeating every 15
    minutes until you complete, delete or reschedule the task.
  - A zero estimate gets no start nudge (there is nothing to start early
    for); when a task's start nudge and heads-up land on the same minute
    you get one alert, not two; and points already in the past are
    dropped, so a late-created task never fires a burst of stale alerts.
  - Each reminder is its own notification and is never bundled into a
    group, so simultaneous alerts — and one task's own reminders — stay
    readable instead of overwriting each other.
- The reminder channel description now spells out the three reminders
  and the 15-minute overdue repeat.
- The due quick-picks no longer stop at "Today", which left the mandatory
  time to be set by hand. They are now **reminder presets** — remind me in
  1h / 3h / 8h, or by the end of today — and each fills the date *and*
  time together. They are counted from the reminder, not the due time: the
  "start now" alert fires `estimated_minutes` before due, so the estimate
  is added to the offset and the due time lands that much later. "End of
  today" is wall-clock (23:59, with its heads-up 5 minutes before), and
  "Tomorrow" stays date-only, because "tomorrow at what time?" is worth
  asking.
- Repeats are now real values instead of a sentence: **every N
  days/weeks/months/years** (N = 1-28), or a **custom condition** for the
  rules that genuinely need words ("mon-fri only", "daily, skip
  Wednesdays", "end of every month"). The task editor replaces the
  free-text box and its four chips with a count, a unit, a custom tickbox
  and the custom text; a blank count means the task doesn't repeat.
  Existing tasks were migrated: plain cadences ("daily", "every 5 days")
  became structured repeats, and the rules carrying an exception ("mon-fri
  only", "daily, skip Wednesdays", "end of every month") kept their exact
  words as a custom condition.

## [4.5.1]

- FIX: recreating a completed task no longer throws away its due time
  (issue #148). Completing a task opens a prefilled draft with only the
  date blank — name, details, estimate, repeat rule, parallel flag and
  the time you set are all carried over. The draft says so in-line, and
  an explicit "clear due date" still drops the time with it.
- Agent-side repeat rollover (MCP `follow_up` + skill contract) now keeps
  `due_time` identical and advances only the date, instead of inviting a
  recomputed time to drift between occurrences.
- Tasks created before times were required keep reading back an empty
  time rather than an invented one, and become valid again on first edit;
  the skill tells the agent to ask for a time instead of guessing.

## [4.5.0]

- FIX: task widget load error on Android 16 / OxygenOS (issue #137) —
  root cause was a single decorative 1dp divider `View` in the widget
  layout; removing it makes the widget render again. The temporary
  diagnostic style ladder is gone: the widget is back to two modes,
  the scrolling list (default) and plain rows, with the view switched
  in its settings screen.
- Task widget rows carry explicit per-row taps instead of the mutable
  pending-intent template, and widget text colors are concrete
  day/night values rather than theme attributes.
- Widget settings screen rebuilt: live preview of the placement, grouped
  cards, segmented choices for the shown slice and row style, switches
  with explanations, and a sticky Cancel/Save bar.

## [4.4.4]

- FIX: the task widget's 1dp divider `View` is the element this launcher
  rejects outright — the style ladder isolated it (C and D1 render, D2
  fails, and D2 only adds that separator). Removed for good; the ladder
  continues with the empty-state text, header toggle and settings tap.

## [4.4.3]

- DIAGNOSTIC: each widget style step (D1-D5) now inflates its own
  layout, byte-identical in the header to the known-good style C and
  adding exactly one element — previously the later elements were
  merely GONE in a shared layout, so a failing step did not isolate
  its own element (issue #137).

## [4.4.2]

- FIX (attempt) for the task widget load error (issue #137): the widget
  is rebuilt on the exact chrome the bisect proved renders, with a
  diagnostic style ladder (C, D1-D5, E). Each D step has its own
  layout, byte-identical to C in the header and adding exactly one
  element — task rows, divider, empty-state text, header view toggle,
  title-to-settings tap — so the first failing step names the culprit. The platform Button is gone for good (its default style
  resolves theme attributes against the host's theme), as are the
  theme-attribute text colors; divider and toggle return only as
  diagnostic steps.
- Task widget settings (view, density, due line) move into the widget's
  settings screen, reachable from the Task Manager; the header shows the
  current view name.

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
