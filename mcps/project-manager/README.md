# Project Manager MCP Server

Project board for the user. The agent manages projects over MCP; the
Agento Android app's Projects screen offers the same list with full CRUD
over `GET/POST/PATCH/DELETE /api/projects` plus `POST /api/projects/reorder`
on health-api (same store, same validation — see `cmd/health-api/main.go`).
Reordering is an app-only verb: a project the agent creates appends to the
end of the board, it never needs to set the order itself.

## Tools

| Tool | Purpose |
|---|---|
| `create_project` | Create a project. Required: name. Optional: status (defaults to Todo), note |
| `list_projects` | List by status Todo/Ongoing/Testing/Blocked/Paused/Done/all, or search name/note (limit caps rows, default 200). Every argument is optional |
| `get_project` | Fetch one project by id |
| `update_project` | Edit name/status/note. Only `id` is required — every other field is nil-safe, and an empty update returns `noop: true` |
| `delete_project` | Permanently delete |

## Schema (MongoDB `hermes` DB)

**`projects`** — `name` (required), `status` (Todo | Ongoing | Testing |
Blocked | Paused | Done), `note`, `position`, `createdAt`, `updatedAt`
(bumped on every edit).

`position` is the manual board order (#519), ascending with gaps of 1000 so
a section can be re-slotted. New rows get `max(position) + 1000`, which the
app renders as the end of their status section; a status change re-slots the
row the same way, so it appends to the new section instead of inheriting the
old slot. `list_projects` returns the board order the phone shows: status
rank (Ongoing, Testing, Blocked, Paused, Todo, Done), then `position`, then
newest. Rows created before #519 have no `position` and sort after every
positioned row in their section.

### Key invariants

- **Fixed statuses:** only Todo/Ongoing/Testing/Blocked/Paused/Done (case-insensitive on
  write, stored canonical). Blank status defaults to Todo.
- **Permanent:** no TTL, no retention pruning. Done means finished.
- **Manual order:** `POST /api/projects/reorder` with `{"status":"Todo","ids":[…]}` rewrites one
  section's positions. `status` is required (no Todo default, unlike
  create/update), `ids` must list the **complete** section (a partial list
  would collide with omitted rows), every id must still carry that status,
  and reordering does not bump `updatedAt`.
- **Shared board:** the app's Projects tab reads/writes the same rows
  (see `.opencode/skills/project-manager/SKILL.md`); local `tasks.json` is gone.

## Run

```bash
go run ./cmd/project-manager      # stdio transport (or go build -o project-manager ./cmd/project-manager)
```

The binary is built by `scripts/build-mcps.sh` into `bin/project-manager` and
registered in `opencode.json` (`mcp.servers["project-manager"]`), which is what
T3/opencode executes from the repo root.
