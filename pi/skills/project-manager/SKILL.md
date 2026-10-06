---
name: project-manager
description: "Project board: create, list, update, and delete projects with Todo/Ongoing/Paused/Done statuses. Shared with the app's Projects tab."
---

# Project Manager

Agent-side board via the `project-manager` MCP server (MongoDB). Same list as
the app's Projects tab. Permanent: no TTL, no pruning.

- `name` required; `status` Todo|Ongoing|Paused|Done (default Todo); `note` free-form.
- Advance with `update_project`. Done means finished, not deleted.
- Never `delete_project` to "finish" — set status Done.
