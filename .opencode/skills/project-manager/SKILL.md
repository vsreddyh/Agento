---
name: project-manager
description: "Project board: create, list, update, and delete projects with Todo/Ongoing/Testing/Paused/Done statuses. Shared with the app's Projects tab."
---

# Project Manager

Agent-side board via the `project-manager` MCP server (MongoDB). Same list as
the app's Projects tab. Permanent: no TTL, no pruning.

- `name` required; `status` Todo|Ongoing|Testing|Paused|Done (default Todo); `note` free-form.
- **Snooze (Paused only):** `snooze_at` is an RFC3339 date **and** time, must be
  in the future, and only sticks while the project is Paused. `""` clears it;
  omitting it leaves it untouched. The server (health-api, not a phone) wakes
  the project to Ongoing one minute before `snooze_at` and clears the field —
  never move a snoozed project yourself, and never treat a passed `snoozeAt`
  as still armed. The app notifies the user from its own per-device cache.
- The board has a manual order the user controls on the phone. `create_project`
  appends to the end of its status section; `list_projects` returns the same
  status-grouped order the app shows (status, then manual position, then
  newest). There is no reorder tool — never try to fake it by editing projects.
- Advance with `update_project`. A status change moves the project to the
  end of its new section. Done means finished, not deleted.
- Never `delete_project` to "finish" — set status Done.
