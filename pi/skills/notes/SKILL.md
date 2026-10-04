---
name: notes
description: "Shared markdown notes: grocery lists, packing lists, anything kept but not scheduled. MongoDB-backed, permanent, never pruned."
---

# Notes

Unscheduled things the user keeps: grocery list, packing list, a half-written
idea. One collection (`notes`), permanent — like money, health, and cookbook, and
shared across profiles, because a shopping list belongs to the user rather than to
whichever profile was asked.

Everything goes through MCP tools (`notes`), never through MongoDB directly.

## Which tool, which store

One collection is three different things with different shapes:

| Kind | Where it goes |
|---|---|
| Needs a due date, recurs, has an estimate | `task-manager` |
| A list to consume — groceries, packing, "things to try" | `notes` |
| Something to say once and keep | `notes` |

If the user says "remind me to buy milk", that is a task. If they say "add milk to
the list", it is a note. Do not create a task with no due date to hold a list.

## Arguments

`title` is required on `create_note`; everything else is optional.

| Tool | Required | Optional (and what absent means) |
|---|---|---|
| `create_note` | `title` | `body`, `tags`, `pinned` |
| `get_note` | `id` | — |
| `list_notes` | — | `tag`, `pinned_only`, `limit` (0 → 50) |
| `search_notes` | `text` | `limit` (0 → 20) |
| `update_note` | `id` | `title`, `body`, `tags`, `pinned` — **absent args are left alone** |
| `delete_note` | `id` | — |

Notes are addressed by **id only**. Titles are not unique — two notes called
"list" is normal — so a title will not resolve and the store refuses to guess.

## Bodies are markdown, and they are a list

One item per line:

```
- milk
- eggs
- bread
```

Two consequences the tool descriptions cannot enforce for you:

- `list_notes` returns summaries **without** bodies. To read or edit a list,
  `get_note` first.
- `update_note` **replaces** the body. Adding one item means `get_note`, append
  the line, and send the whole body back. Sending a partial body silently deletes
  every line you left out. Never reconstruct a body from a `list_notes` summary —
  that is how a list gets wiped.

## Editing a list the user is talking about

"I got milk" → `get_note`, drop that one line, `update_note` with the rest
verbatim. If more than one note could be the list, `search_notes` for the item and
ask which one rather than guessing.

Deleting a note is irreversible — confirm first, and never as a way to "tidy".

Run: `go run ./cmd/notes` (stdio) for a local check. In the running stack the
server is `/usr/local/bin/notes`, started by Pi from the agent config; do not
launch it yourself.
