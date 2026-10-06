---
name: notes
description: "Shared markdown notes: grocery lists, packing lists, anything kept but not scheduled. MongoDB-backed, permanent, never pruned."
---

# Notes

Unscheduled things the user keeps. One `notes` collection, permanent, via MCP
tools only — never MongoDB directly. Needs a due date → `task-manager`, not here.

## Tools

`create_note(title, body?, tags?, pinned?)`, `get_note(id)`,
`list_notes(tag?, pinned_only?, limit?)` (summaries, no bodies),
`search_notes(text)` (title + body + tags), `update_note(id, ...)` (omitted
args untouched; `body=""` clears), `delete_note(id)` (confirm first).

Notes are addressed by **id only** — titles are not unique.

## Bodies

One item per line (`- milk`). `update_note` **replaces** the body: `get_note`
first, append/remove lines, send the whole body back. Never rebuild a body
from a `list_notes` summary. If the response says `truncated`, tell the user
the tail was cut.
