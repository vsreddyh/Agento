# Task Manager MCP Server

Personal task tracking for the user. The agent manages tasks over MCP;
the Agento Android app's Task Manager screen offers the same list with
full CRUD over `GET/POST/PATCH/DELETE /api/tasks` on health-api (same
store, same validation — see `cmd/health-api/main.go`).

## Tools

| Tool | Purpose |
|---|---|
| `create_task` | Create an open task (name required; due/estimate/repeat optional) |
| `list_tasks` | List by state open/done/all, overdue-only, or search |
| `get_task` | Fetch one task by id |
| `update_task` | Edit fields (nil-safe; empty repeat_rule clears the rule) |
| `complete_task` | Mark done (3-day retention starts); echoes repeat_rule + follow-up nudge |
| `reopen_task` | Reopen a completed task (cancels expiry) |
| `delete_task` | Permanently delete |

## Schema (MongoDB `hermes` DB)

**`tasks`** — `name` (required), `description`, `due_date` (YYYY-MM-DD),
`due_time` (HH:MM, requires a date), `estimated_minutes`, `repeat_rule`
(free-form, verbatim), `completedAt` (null = open), `createdAt`,
`expiresAt` (completed only = completedAt + 3d, TTL target).

### Key invariants

- **No status field:** openness is `completedAt == null`. Never add one.
- **No server-side repeat logic:** `repeat_rule` is never parsed here.
  The agent interprets it and creates the next occurrence (see
  `skills/task-manager/SKILL.md`); `complete_task` only echoes the rule
  back with a `follow_up` nudge.
- **Retention:** done tasks auto-delete 3 days after completion via TTL.
  Open tasks never expire. `reopen_task` clears the expiry.

## Run

```bash
go run ./cmd/task-manager          # stdio transport (or go build -o task-manager ./cmd/task-manager)
```

`MONGODB_URI`/`MONGODB_DB` from the root `.env` (same as the other servers).

## Client config

```json
{
  "mcpServers": {
    "task-manager": {
      "command": "/usr/local/bin/task-manager",
      "args": []
    }
  }
}
```

## Files (Go, `internal/tasks/` + `cmd/task-manager/`)

- `cmd/task-manager/main.go` — MCP server + tool definitions
- `internal/tasks/store.go` — CRUD, completion/expiry, indexes
- `internal/tasks/store_test.go` — tests (`go test ./internal/tasks/`, needs `MONGODB_URI`)
