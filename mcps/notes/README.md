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
| `update_note` | `id` | everything else — **an omitted arg is left alone; `body`/`tags` sent as empty clear that field, but an empty `title` is rejected** |
| `delete_note` | `id` | — |

Notes are addressed by **id only**. Titles are not unique, so a title does not
resolve and the store refuses to pick a winner.

`search_notes` matches with an unindexed `$regex` (correctly `QuoteMeta`d) over title
and body. That is a collection scan — irrelevant at the size this runs at, and the
first thing to change if notes ever grows into thousands of rows, which is what an
Atlas text index would be for.

- `search_notes` matches title, body **and tags**, so a word the user remembers
  finds the note even when it is only a tag. Tags are lowercased on write and the
  query is lowercased to match.
- `tags` is always a `[]string` in a response, whichever call produced it. A doc
  read back from Mongo decodes its BSON array as `bson.A`; without normalising in
  `docOut`, a Go caller asserting `.([]string)` passes on the create response and
  panics on the get.
- Over-cap input is still stored, but the response carries `truncated: {title,
  body}` naming which fields lost bytes. A 25 KB paste stored as 20 KB and
  answered `ok:true` is data loss reported as success.

Four behaviours worth knowing before calling anything:

- `list_notes` returns summaries: the body is replaced by a `preview` (the first
  line that is not blank and not an ATX heading — `#` followed by a space or
  end of line; the title already carries the heading), so a 200-note filter
  cannot flood the context window. `#milk` is content, not a heading. `get_note` and
  `search_notes` return the body.
- `update_note` with nothing to change writes nothing, so `updatedAt` is not bumped
  and the note does not jump to the top of every list.
- `delete_note` of an unknown id returns `ok:false`, like `get_note` and
  `update_note`. Deletion itself is idempotent — the store reports
  "nothing was deleted" rather than treating it as a fault.
- `update_note` is patch-shaped and `body` **replaces**. Adding a line to a list
  means read-modify-write of the whole body; a partial body deletes the lines it
  omits. `title`, `body` and `tags` are pointers in the input struct, so *omitted*
  (leave alone) and *empty* (clear, on purpose) stay distinguishable — a plain
  `string` + `omitempty` would make `body=""` a silent no-op and the agent would
  report success on an edit that never happened. `title` is the one field where
  empty is refused rather than honoured: a note with no title cannot be found or
  listed meaningfully, so the store rejects it instead of storing one.
- `search_notes` caps `limit` at 50, not 200: it returns full bodies, so the
  ceiling is on bytes in context, not on rows.

Run: `go run ./cmd/notes` (stdio), needs `MONGODB_URI`/`MONGODB_DB`.
In-container binary: `/usr/local/bin/notes`.