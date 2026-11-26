# Cookbook MCP — `mcps/cookbook/`

Recipe + ingredient books with notes. Permanent data — never pruned.

Collections: `cookbook_ingredients` (name unique + note ≤2000 bytes,
longer notes truncated on a UTF-8 boundary), `cookbook_recipes` (dish name
unique + note ≤2000 bytes, truncated, + `tags[]`, at most 20 tags of 40
bytes each, over-cap truncated). No quantities, servings, macros, or cook
log (#600); `cookbook_cook_log` stays in Atlas untouched but nothing reads
it. Deletes are idempotent (unknown names return `ok:true` with
`deleted:false`); lists cap at 500 ingredients / 200 recipes in name
order with no truncated flag (a full page may hide more — narrow with
search), and the recipe tag filter is an exact, case-sensitive match.

## Which arguments are required

The tool descriptions are the contract; this is the short form of it.

| Tool | Required | Optional (and what absent means) |
|---|---|---|
| `add_ingredient` | `name` | `note` |
| `get_ingredient` | `name_or_id` | — |
| `list_ingredients` | — | `search` (up to 500, name order) |
| `update_ingredient` | `name_or_id` | `name`, `note` — **empty args are left alone** |
| `delete_ingredient` | `name_or_id` | — (idempotent: unknown → `ok:true` `deleted:false`) |
| `add_recipe` | `name` | `note`, `tags` |
| `get_recipe` | `name_or_id` | — |
| `list_recipes` | — | `search`, `tag` (up to 200, name order; tag exact + case-sensitive) |
| `update_recipe` | `name_or_id` | `name`, `note`, `tags` — **empty args are left alone**; `tags` replaces the whole list, it is not merged |
| `delete_recipe` | `name_or_id` | — (idempotent: unknown → `ok:true` `deleted:false`) |

`update_*` are patch-shaped: only sent keys change; omitted keys are left
alone — resending a previously-read value is harmless. A whitespace-only
value counts as empty and is left unchanged too.

Name matching is exact and case-sensitive everywhere (`Salt` and `salt`
are distinct rows that can coexist); only `list_*` search is a
case-insensitive substring. Writes trim names/notes/tags
(whitespace-only counts as empty); renames to an existing name error.
Unknown get/update targets return `ok:false`; deletes are idempotent
(`ok:true` with `deleted:false`).

Run: `go run ./cmd/cookbook` (stdio), needs `MONGODB_URI`/`MONGODB_DB`.

The binary is built by `scripts/build-mcps.sh` into `bin/cookbook` and
registered in `opencode.json` (`mcp.servers["cookbook"]`), which is what
T3/opencode executes from the repo root.
