---
name: task-manager
description: "Personal task manager: create, list, complete, and roll recurring tasks. No status field — open means completedAt is null."
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

Structured XOR custom, never both:

- structured: `repeat_every` (1–28) + `repeat_unit` (days|weeks|months|years)
- custom: `repeat_custom: true` + `repeat_rule` = user's words verbatim
- neither: one-shot (prefer structured; use custom for exceptions, named
  weekdays, counts > 28)

On `complete_task`, read `rollover`:

- `created` — structured, server already made the next occurrence. Do nothing.
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
- "Not tonight" / "skip it" means the work did NOT happen: never `complete_task`
  it — leave it open, or push `due_date`/`due_time` via `update_task` when the
  user names another time. A completion is a claim the work is done, and
  completed rows vanish after 3 days, so a false one is erased, not corrected.

## Mistake

`reopen_task` — for a mistaken completion. Never `delete_task` to "undo" it.
