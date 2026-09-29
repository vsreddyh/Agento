---
name: task-manager
description: "Personal task manager: create, list, complete, and roll recurring tasks. No status field — open means completedAt is null."
---

# Task Manager

Agent-side task tracking backed by the `task-manager` MCP server (MongoDB).
This is NOT the app's Projects tab — that is a separate project board the
agent manages through the `project-manager` MCP server (see
`skills/project-manager/SKILL.md`). Everything here goes through MCP tools.

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
2. When you call `complete_task` and the response contains `follow_up`,
   you MUST call `create_task` for the next occurrence, reusing the exact
   repeat keys the response gave you (`repeat_every` + `repeat_unit`, or
   `repeat_custom: true` + `repeat_rule`). Every field is copied verbatim —
   name/description/`due_time`/estimated_minutes/repeat/parallelable — and
   ONLY `due_date` advances, to the occurrence YOU compute from the rule
   and today. A "daily at 9am" task keeps `due_time: "09:00"`; a time that
   drifts between occurrences is a bug, not a recomputation. Change
   `due_time` only when the rule itself names a different time ("mornings
   at 6", "9am then 7pm").
3. If the rule is ambiguous ("regularly"), ask the user for the next due
   date instead of guessing. Same for an empty `due_time`: tasks created
   before times were required come back with `due_time: ""`, and
   `create_task` rejects that — ask the user for a time, never invent one.
4. One-shot tasks (empty rule) need nothing after completion.

## Everyday use

- Capture fast: collect name + description + due date/time + estimate +
  parallelable BEFORE calling `create_task` — it rejects missing fields.
  ("takes about an hour" → `estimated_minutes: 60`; "I can do it alongside
  X" → `parallelable: true`; no repeat mentioned → send no repeat keys.)
- Morning check: `list_tasks` with `overdue: true`, then state=open.
- Done for now but not finished: leave open. Only `complete_task` finishes.
- Mistake: `reopen_task`. Never `delete_task` to "undo" a completion.
