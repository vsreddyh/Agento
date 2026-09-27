# Project Manager MCP Server

Project board for the user. The agent manages projects over MCP; the
Agento Android app's Projects screen offers the same list with full CRUD
over `GET/POST/PATCH/DELETE /api/projects` on health-api (same store,
same validation — see `cmd/health-api/main.go`).

## Tools

| Tool | Purpose |
|---|---|
| `create_project` | Create a project (name required; status defaults to Todo) |
| `list_projects` | List by status Todo/Ongoing/Paused/Done/all, or search name/note |
| `get_project` | Fetch one project by id |
| `update_project` | Edit name/status/note (only sent keys change) |
| `delete_project` | Permanently delete |

## Schema (MongoDB `hermes` DB)

**`projects`** — `name` (required), `status` (Todo | Ongoing | Paused |
Done), `note`, `createdAt`, `updatedAt` (bumped on every edit).

### Key invariants

- **Fixed statuses:** only Todo/Ongoing/Paused/Done (case-insensitive on
  write, stored canonical). Blank status defaults to Todo.
- **Permanent:** no TTL, no retention pruning. Done means finished.
- **Shared board:** the app's Projects tab reads/writes the same rows
  (see `skills/project-manager/SKILL.md`); local `tasks.json` is gone.

## Run

```bash
go run ./cmd/project-manager      # stdio transport (or go build -o project-manager ./cmd/project-manager)
```

The binary is baked into the bot image (`test/Dockerfile`) and registered
on the god profile (`gateway/config.yaml.template`).
