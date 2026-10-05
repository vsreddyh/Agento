---
name: task-manager
description: "Personal task manager: create, list, complete, and roll recurring tasks. No status field — open means completedAt is null."
---

# Task Manager

Agent-side task tracking backed by the `task-manager` MCP server (MongoDB).
This is NOT the app's Projects tab — that is a separate project board the
agent manages through the `project-manager` MCP server (see
`pi/skills/project-manager/SKILL.md`). Everything here goes through MCP tools.

## Columns

- **name**, **description**, **due_date** (YYYY-MM-DD), **due_time**
  (HH:MM), **estimated_minutes** (>= 0), **parallelable** (true = can run
  alongside other tasks) — ALL required on `create_task`.
  The **repeat** is the only optional part (all four keys zero/empty =
  one-shot).
- No status field. Open = not completed; done = `complete_task` called.
  Completed tasks vanish automatically 3 days later.

## The repeat contract (most important rule)

A repeat is EITHER **structured** OR **custom** — never both, the server
rejects the mix:

- structured: `repeat_every` (1-28) + `repeat_unit`
  (`days`|`weeks`|`months`|`years`);
- custom: `repeat_custom: true` + `repeat_rule` = the user's words
  verbatim ("every 3rd Friday", "weekdays at 9am");
- neither: one-shot.

1. **Prefer structured.** "every 2 weeks" is `repeat_every: 2,
   repeat_unit: "weeks"` — a cadence you can reason about, and the app
   shows it as "Every 2 weeks". Fall back to a custom condition only when
   the rule has a shape those fields cannot hold (exceptions, named
   weekdays, "end of month"). Never flatten a rule that carries an
   exception into a plain cadence: "daily, skip Wednesdays" is a custom
   condition, not "every day". A count above 28 is a custom condition too.
2. When you call `complete_task`, read what comes back. Every completion reports
   `rollover`, so you never have to infer the outcome from a missing field:
   - `created` (with the new task in `next`) — the repeat was STRUCTURED, the
     server already created the next occurrence, and you must NOT create
     another. (Month ends clamp and then keep that day: a task due on the 31st
     that rolls into February comes back on the 28th, and stays there — that
     is intended, not drift to correct.)
   - `none` — a one-shot. Nothing further.
   - `custom` — the repeat is a CUSTOM condition, so you MUST call
     `create_task` for the next occurrence, reusing the exact repeat keys
     the response gave you (`repeat_custom: true` + `repeat_rule`). Copy
     every field verbatim — name/description/`due_time`/
     estimated_minutes/repeat/parallelable — and advance ONLY `due_date`,
     to the occurrence YOU compute from the words and today. A "daily at
     9am" custom rule keeps `due_time: "09:00"`; a time that drifts
     between occurrences is a bug, not a recomputation. Change `due_time`
     only when the rule itself names a different time ("mornings at 6",
     "9am then 7pm").
   - `exhausted` or `failed` — **the task repeats but its next occurrence was
     NOT created, so it has stopped recurring.** The response carries
     `needs_attention`. TELL THE USER in your reply, with the task name and
     what went wrong. Do not report this as a plain completion, and do not
     silently create the next task yourself unless the user asks — the
     stored data is usually why it failed, so it needs a human to look at.
3. If the rule is ambiguous ("regularly"), ask the user for the next due
   date instead of guessing. Same for an empty `due_time`: tasks created
   before times were required come back with `due_time: ""`, and
   `create_task` rejects that — ask the user for a time, never invent one.
4. One-shot tasks (empty rule) need nothing after completion — `rollover: none`.

## Everyday use

- Capture fast: collect name + description + due date/time + estimate +
  parallelable BEFORE calling `create_task` — it rejects missing fields.
  ("takes about an hour" → `estimated_minutes: 60`; "I can do it alongside
  X" → `parallelable: true`; no repeat mentioned → send no repeat keys.)
- Morning check: `list_tasks` with `overdue: true`, then state=open.
- Done for now but not finished: leave open. Only `complete_task` finishes, and
  `skip_task` skips. See the skip rule below.

## Skip is a verb, not a completion

**`complete_task` records that work HAPPENED. If it did not happen, use
`skip_task`** — "not doing this tonight", "out of time", "doing it tomorrow",
"skipping the skippable ones".

Why this matters enough to have its own tool: a completion is a claim that the
work is done. Recording a skip as a completion writes a false record — and
completed tasks are deleted after 3 days, so the false history is **erased rather
than corrected**, and the user never finds out. This has already happened: five
chores were marked complete for an evening in which none of them was done.

| The user says | Call |
|---|---|
| "done", "finished it", "did it" | `complete_task` |
| "skip it", "not tonight", "doing it tomorrow", "no time" | `skip_task` |
| "still need to do it" | nothing — leave it open |

`skip_task` does three things, all of which `complete_task` does not:

- marks the record **skipped**, not done (`skipped: true`), so the history is
  honest;
- still takes it off tonight's open list, so the user is not asked again;
- still advances a **structured** repeat — skipping tonight is not skipping the
  habit. `delete_task` is what abandons a series.

If you are ever unsure whether the work happened, **ask**. A wrong completion is
worse than an extra question, and it disappears in three days.

## Mistake

`reopen_task` — for both a mistaken completion and a mistaken skip. Never
`delete_task` to "undo" either.
