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
- `none` — one-shot. Do nothing.
- `custom` — YOU create the next occurrence: copy all fields verbatim, advance
  only `due_date` per the rule. Keep `due_time` unless the rule names a time.
- `exhausted`/`failed` — repeat stopped. Tell the user; do not silently recreate.

Ambiguous rule or empty `due_time` on an old task: ask the user, never guess.

## Skip vs complete

Work happened → `complete_task`. Work did NOT happen ("not tonight",
"tomorrow") → `skip_task` (marks `skipped: true`, still advances structured
repeats). Unsure → ask. Mistaken either → `reopen_task`, never `delete_task`.
