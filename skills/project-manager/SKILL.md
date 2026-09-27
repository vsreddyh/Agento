---
name: project-manager
description: "Project board: create, list, update, and delete projects with Todo/Ongoing/Paused/Done statuses. Shared with the app's Projects tab."
---

# Project Manager

Agent-side project board backed by the `project-manager` MCP server
(MongoDB). This is the SAME list as the app's Projects tab — everything
here is visible in the app, and app edits are visible here.

## Columns

- **name** (required), **status** (Todo | Ongoing | Paused | Done,
  default Todo), **note** (free-form).
- Projects are permanent: no TTL, no retention pruning.

## Everyday use

- Capture fast: `create_project` with just a name; set status/note when
  the user says them.
- Board check: `list_projects` (default all), or filter by status.
- Advance work with `update_project` (status Todo → Ongoing → Paused →
  Done); Done means finished, not deleted.
- Never `delete_project` to "finish" something — set status Done.
