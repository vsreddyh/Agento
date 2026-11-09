# Project Manager MCP Server

Project board for the user. The agent manages projects over MCP; the
Agento Android app's Projects screen offers the same list with full CRUD
over `GET/POST/PATCH/DELETE /api/projects` on health-api (same store,
same validation — see `cmd/health-api/main.go`).

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
Blocked | Paused | Done), `note`, `createdAt`, `updatedAt` (bumped on every edit).

### Key invariants

- **Fixed statuses:** only Todo/Ongoing/Testing/Blocked/Paused/Done (case-insensitive on
  write, stored canonical). Blank status defaults to Todo.
- **Permanent:** no TTL, no retention pruning. Done means finished.
- **Shared board:** the app's Projects tab reads/writes the same rows
  (see `.opencode/skills/project-manager/SKILL.md`); local `tasks.json` is gone.

## Run

```bash
go run ./cmd/project-manager      # stdio transport (or go build -o project-manager ./cmd/project-manager)
```

The binary is built by `scripts/build-mcps.sh` into `bin/project-manager` and
registered in `opencode.json` (`mcp.servers["project-manager"]`), which is what
T3/opencode executes from the repo root.
