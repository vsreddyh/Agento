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
| `update_note` | `id` | everything else — **absent args are left alone** |
| `delete_note` | `id` | — |

Notes are addressed by **id only**. Titles are not unique, so a title does not
resolve and the store refuses to pick a winner.

Two behaviours worth knowing before calling anything:

- `list_notes` projects out `body` and returns a `preview` (the first non-blank
  line) instead, so a 200-note filter cannot flood the context window. `get_note`
  and `search_notes` return the body.
- `update_note` is patch-shaped and `body` **replaces**. Adding a line to a list
  means read-modify-write of the whole body; a partial body deletes the lines it
  omits. `pinned` is a `*bool` so `false` is distinguishable from absent —
  unpinning is a real edit that `omitempty` on a plain `bool` would swallow.

Run: `go run ./cmd/notes` (stdio), needs `MONGODB_URI`/`MONGODB_DB`.
In-container binary: `/usr/local/bin/notes`.