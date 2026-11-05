# Reports MCP — `mcps/reports/`

The ∃-Mail writer: agents file reports the user reads in the app's inbox.
Read-only inbox — not email: no compose, no reply, no thread, no send, no
external addresses.

Collection: `reports` (`title`, `body_md` markdown+LaTeX, `source`,
`read`, `starred`, `createdAt`). One document is one filed report.
Conditional 7-day retention, NOT permanent: read+unstarred older than 7 days
is pruned; unread and starred survive regardless of age.

## Which arguments are required

The tool descriptions are the contract; this is the short form.

| Tool | Required | Optional (and what absent means) |
|---|---|---|
| `create_report` | `title` | `body_md`, `source` |
| `get_report` | `id` | — |
| `list_reports` | — | `limit` (0 → 50) |

Reports are addressed by **id only**. Titles are not unique, so a title does
not resolve.

There is no update tool and no delete tool, on purpose: the reader's verbs
(open-marks-read, star, delete-with-confirm) belong to the app's
PATCH/DELETE over health-api, not to the filing path. A filed report's
content is immutable — re-filing a corrected report is a new report.

- `body_md` is markdown with inline (`$..$`) and block (`$$..$$`) LaTeX math.
  It is stored verbatim and compiled **server-side at read time**
  (`internal/reports`, vendored KaTeX under pure-Go goja — no node, no system
  dependency, offline). `$` inside code spans stays literal; unclosed `$`
  stays literal; currency ("costs $5") never parses as math.
- `list_reports` returns summaries: the body is replaced by a `preview` (the
  first non-empty, non-heading line), newest first. `get_report` returns the
  body.
- `source` is display-only (which skill/agent filed it) — it drives no
  filtering in v1.
- Over-cap input is still stored, but the response carries
  `truncated: {title, body_md, source}` naming which fields lost bytes.
