---
name: task-manager
description: "Personal task manager: create, list, complete, skip, and roll recurring tasks. No status field — open means completedAt is null."
---

# Task Manager

Agent-side tracking via the `task-manager` MCP server (MongoDB). Not the app's
Projects tab (see `project-manager` skill).

## Columns

`name`, `description`, `due_date` (YYYY-MM-DD), `due_time` (HH:MM),
`estimated_minutes`, `parallelable` — ALL required on `create_task`. Repeat is
the only optional part (all repeat keys zero/empty = one-shot). No status
field; completed tasks vanish after 3 days.

## Repeat contract

One of three shapes, never combined:

- structured interval: `repeat_every` (1–28) + `repeat_unit` (days|weeks|months|years)
- structured weekly days: `repeat_days` = array of weekdays (`mon`…`sun`),
  always WEEKLY, and `due_date` MUST be one of them ("gym Mon/Wed/Fri" →
  `repeat_days: ["mon","wed","fri"]`)
- custom: `repeat_custom: true` + `repeat_rule` = user's words verbatim
- none of them: one-shot

Switching shapes means zeroing the old one: to move a task to days, send
`repeat_every: 0`, `repeat_unit: ""` alongside `repeat_days`, and vice versa.
Prefer BOTH structured shapes — the server rolls those over itself. Reserve
custom for exceptions and counts above 28.

On `complete_task`, read `rollover`:

- `created` — structured (interval or selected days), server already made the
  next occurrence. Do nothing.
  (Month ends clamp and stay: 31st → Feb 28th stays there, not drift to correct.)
- `none` — one-shot. Do nothing.
- `custom` — YOU create the next occurrence: copy all fields verbatim, advance
  only `due_date` per the rule. Keep `due_time` unless the rule names a time.
- `exhausted`/`failed` — repeat stopped (see `needs_attention`). Tell the user; do not silently recreate.

Ambiguous rule: ask the user for the next due date, never guess. Empty
`due_time` (`""` on old tasks): ask for a time, never invent one — `create_task`
rejects it.

## Everyday use

- Capture fast: collect name + description + due date/time + estimate +
  parallelable BEFORE calling `create_task` — it rejects missing fields.
  ("takes about an hour" → `estimated_minutes: 60`; "I can do it alongside
  X" → `parallelable: true`; no repeat mentioned → send no repeat keys.)
- Morning check: `list_tasks` with `overdue: true`, then state=open.
- Done for now but not finished: leave open. Only `complete_task` finishes.
- "Not tonight" / "skip it" means the work did NOT happen: call `skip_task`
  (never `complete_task` — a completion claims the work happened, and
  completed rows vanish after 3 days, so a false one is erased, not
  corrected). A structured repeat rolls over server-side like a completion;
  a custom repeat is yours to recreate from the response's `follow_up`.

## Mistake

`reopen_task` — for a mistaken completion or skip. Never `delete_task` to "undo" it.
