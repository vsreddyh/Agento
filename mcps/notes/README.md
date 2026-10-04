# Notes MCP — `mcps/notes/`

Unscheduled things the user keeps: grocery list, packing list, a half-written
idea. Permanent data — never pruned. Shared across profiles, like money, health,
and cookbook.

Collection: `notes` (`title`, `body` markdown, `tags`, `pinned`, `createdAt`,
`updatedAt`). One document is one note; a list is markdown lines in `body`, not
one row per item.

## Which arguments are required

The tool descriptions are the contract; this is the short form, because two of
these are not guessable from the argument names.

| Tool | Required | Optional (and what absent means) |
|---|---|---|
| `create_note` | `title` | `body`, `tags`, `pinned` |
| `get_note` | `id` | — |
| `list_notes` | — | `tag`, `pinned_only`, `limit` (0 → 50) |
| `search_notes` | `text` | `limit` (0 → 20) |
| `update_note` | `id` | everything else — **an omitted arg is left alone; an arg sent as empty clears that field** |
| `delete_note` | `id` | — |

Notes are addressed by **id only**. Titles are not unique, so a title does not
resolve and the store refuses to pick a winner.

Two behaviours worth knowing before calling anything:

- `list_notes` returns summaries: the body is replaced by a `preview` (the first
  non-blank line), so a 200-note filter cannot flood the context window.
  `get_note` and `search_notes` return the body.
- `update_note` is patch-shaped and `body` **replaces**. Adding a line to a list
  means read-modify-write of the whole body; a partial body deletes the lines it
  omits. `title`, `body` and `tags` are pointers in the input struct, so *omitted*
  (leave alone) and *empty* (clear, on purpose) stay distinguishable — a plain
  `string` + `omitempty` would make `body=""` a silent no-op and the agent would
  report success on an edit that never happened.
- `search_notes` caps `limit` at 50, not 200: it returns full bodies, so the
  ceiling is on bytes in context, not on rows.

Run: `go run ./cmd/notes` (stdio), needs `MONGODB_URI`/`MONGODB_DB`.
In-container binary: `/usr/local/bin/notes`.