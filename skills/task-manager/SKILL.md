---
name: task-manager
description: "Personal task manager: create, list, complete, and roll recurring tasks. No status field — open means completedAt is null."
---

# Task Manager

Agent-side task tracking backed by the `task-manager` MCP server (MongoDB).
This is NOT the app's Projects tab (formerly Tasks) — that is an app-local
project list the agent cannot see. Everything here goes through MCP tools.

## Columns

- **name** (required), **description**, **due_date** (YYYY-MM-DD),
  **due_time** (HH:MM, needs a date), **estimated_minutes**,
  **repeat_rule** (free-form string, empty = one-shot).
- No status field. Open = not completed; done = `complete_task` called.
  Completed tasks vanish automatically 3 days later.

## The repeat contract (most important rule)

`repeat_rule` is NEVER interpreted by the server. It is your job:

1. When the user gives a repeating task, store their words verbatim in
   `repeat_rule` (e.g. "every 3rd Friday", "weekdays at 9am"). Never
   normalize to an enum.
2. When you call `complete_task` and the response contains `follow_up`
   with a `repeat_rule`, you MUST call `create_task` for the next
   occurrence: same name/description/estimated_minutes/repeat_rule, with
   the next due date/time that YOU compute from the rule and today.
3. If the rule is ambiguous ("regularly"), ask the user for the next due
   date instead of guessing.
4. One-shot tasks (empty rule) need nothing after completion.

## Everyday use

- Capture fast: `create_task` with just a name; fill in due/estimate when
  the user says them ("takes about an hour" → `estimated_minutes: 60`).
- Morning check: `list_tasks` with `overdue: true`, then state=open.
- Done for now but not finished: leave open. Only `complete_task` finishes.
- Mistake: `reopen_task`. Never `delete_task` to "undo" a completion.
