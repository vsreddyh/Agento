# Cookbook MCP — `mcps/cookbook/`

Recipe + ingredient books with notes. Permanent data — never pruned.

Collections: `cookbook_ingredients` (name unique + note ≤2000, longer notes
truncated), `cookbook_recipes` (dish name unique + note ≤2000, truncated, +
`tags[]`, at most 20 tags of 40 chars each, over-cap truncated). No
quantities, servings, macros, or cook log (#600); `cookbook_cook_log`
stays in Atlas untouched but nothing reads it.

## Which arguments are required

The tool descriptions are the contract; this is the short form of it.

| Tool | Required | Optional (and what absent means) |
|---|---|---|
| `add_ingredient` | `name` | `note` |
| `get_ingredient` | `name_or_id` | — |
| `list_ingredients` | — | `search` |
| `update_ingredient` | `name_or_id` | `name`, `note` — **empty args are left alone** |
| `delete_ingredient` | `name_or_id` | — |
| `add_recipe` | `name` | `note`, `tags` |
| `get_recipe` | `name_or_id` | — |
| `list_recipes` | — | `search`, `tag` |
| `update_recipe` | `name_or_id` | `name`, `note`, `tags` — **empty/zero args are left alone** |
| `delete_recipe` | `name_or_id` | — |

`update_*` are patch-shaped: send only the keys you mean to change — a
resend of fields you read earlier is how an approved edit becomes a
whole-record overwrite.

Run: `go run ./cmd/cookbook` (stdio), needs `MONGODB_URI`/`MONGODB_DB`.
In-container binary: `/usr/local/bin/cookbook`.
