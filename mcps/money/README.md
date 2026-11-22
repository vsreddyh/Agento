# Miser — Multi-Account Money MCP Server

MCP server port of the retired money bot profile,
extended to **multiple accounts** with **atomic stored balances**.

## Tools

| Tool | Purpose |
|---|---|
| `create_account` | Create an account with starting balance. Required: name (trimmed, case-sensitive — `Cash` and `cash` are different accounts). Optional: type (defaults to cash, empty or whitespace-only; case-insensitive + trimmed, so `Bank` stores `bank` like transaction types), balance (opening balance in the same call, defaults to 0, must be exact paise like every amount) |
| `list_accounts` | List accounts with stored balances **and ids** — hides archived by default; pass `include_archived=true` to see them. Transaction rows reference accounts by id, so this is the id→name map. Optional: include_archived |
| `archive_account` | Soft-delete (history stays queryable; blocked from new `log_transaction` writes — `delete`/`edit` can still move its balance). `name` accepts a name or id hex (trimmed, so an `_id` from `list_accounts` round-trips); the stored name is echoed back, never the raw input. The name stays taken. Required: `name` (put the hex here for id) |
| `unarchive_account` | Restore an archived account so it accepts writes again (inverse of `archive_account`). `name` accepts a name or id hex (trimmed); the stored name is echoed back. Required: `name` (put the hex here for id) |
| `get_balances` | Per-account balances (including archived rows) + total across active accounts only — `total` is NOT the sum of `accounts[].balance` once anything is archived, unlike `list_accounts` which hides archived by default. Takes no arguments |
| `log_transaction` | Log income / expense / transfer. Required: type, amount (exact paise: > 0, at most 2 decimals — stored as int64), account (account names are case-sensitive exact-match, unlike type/category — `cash` does not find `Cash`). Date must be a real `YYYY-MM-DD` calendar date (padded dates trimmed; `2026-02-30` rejected like query/summarize — it used to store then TTL-purge immediately while the balance stayed). Any real date stores with immediate balance effect, even a far-future one hidden from relative period-phrase summaries until then (a month+year phrase or explicit start/end shows future rows immediately) or a far-past one the TTL purges quickly while the balance stays. Optional: category (blank or whitespace-only = `other`; an unknown name is **rejected**, not silently refiled), sending_to (required by the store for a transfer, name or id hex like account, must differ from the source, and forbidden on non-transfers except whitespace-only (counts as absent), rejected otherwise), note (capped at 300 bytes, cut on a UTF-8 boundary — long text reads back truncated, not rejected), date (defaults to today in IST; empty or whitespace-only also means today, unlike query's rejected empty bounds), idempotency_key (trimmed, max 200 bytes — a UUID is 36; longer is rejected; blank/whitespace-only means no key, each call logs anew; a retry returns the original id with `created: false` instead of double-logging; the same key with a different date/amount/type/category/account/sending_to is **rejected** — use a new key for a new request; `note` differences are ignored on replay (first write wins, a correction needs a new key; there is no `source` field — it is always `log_transaction` via MCP). Returns the stored `category`/`account`/`note` (normalized) plus `sending_to`/`sending_to_account` for transfers; if the post-write re-read fails, `ok` stays true with `readback_error` |
| `query_transactions` | List in a date range. Required: start, end — both must be real `YYYY-MM-DD` dates or the call is rejected (padded bounds trimmed, like log/delete). Bounds are plain calendar-day strings with no timezone conversion — pass IST dates to match how rows are stored. Optional: type/category/account filter (type/category are lowercased+trimmed like writes, so `Groceries` finds `groceries`, but an unknown value is **rejected** like writes instead of matching nothing; account (case-sensitive exact name or id hex, unlike type/category; matches EITHER side of a transfer, so a transfer appears under both sender and receiver). Paged: `limit` defaults to 100, clamped to 500; `offset` defaults to 0. There is no unlimited `query_transactions` page on MCP (summarize and delete dry-runs stay unbounded by design): 0/negative `limit` also means 100, unlimited is store-internal only. The response carries `count` (rows in this page — unlike `total`, which counts all matches), the applied `limit`, `total`, `truncated` and `next_offset` so a short page is never mistaken for the end: to read everything, repeat with `offset = next_offset` until `truncated` is false (`next_offset` is null then). Rows come back date-ascending with an `_id` tiebreak (oldest first; deterministic order, but not a snapshot — a concurrent log or TTL expiry between page fetches can shift rows across page boundaries). Returns each row with its hex `_id`, so a queried row can be deleted or edited by `id` |
| `summarize` | Income, expense, net + per-category breakdown (transfers are counted in `count` but excluded from income/expense/net; `by_category` is expense-only, so income categories never rank). Give a period phrase **or** an explicit start/end pair — half a pair is rejected rather than ignored, and a reversed pair is rejected like query, neither means this month (bounds are trimmed, so padded values work; whitespace-only counts as absent, like account). Period phrases are evaluated in IST, like the default log date; an explicit start/end pair is used verbatim (no conversion). Supported phrases: today, yesterday, this week (Monday-first), last month (spaceless `thisweek`/`thismonth`/`lastmonth` all work), month names with optional 1000-2999 year (June, June 2025, June 2100; a bare month means the most recent occurrence, so December in November 2026 is Dec 2025; a year outside 1000-2999 is ignored and the recency rule applies), this month / so far / month — matched on whole words, so `maybe`/`mayhem`/`todays`/`monthly` do not hit a branch. Anything else means this month too (`2025`, `last year` and `banana` all return this month, not an error). Optional: account (case-sensitive exact name or id hex, unlike type/category; whitespace-only means absent, like bounds; either side of a transfer, like query) |
| `delete_transactions` | Delete by `id` (a non-existent id is **rejected**, not a silent 0), or by amount (exact paise: > 0, at most 2 decimals; an explicit 0 or >2-decimal value is rejected, not a wildcard)/type/category/date/account (balances inverted; account takes a case-sensitive exact name or id hex like query (unlike type/category) and matches EITHER side of a transfer — check `dry_run` first, since a receiver-side transfer is included; whitespace-only means absent, like summarize; unknown type/category **rejected** like writes, not a silent `would_delete: 0`). Date filter trimmed + calendar-validated like writes/reads; whitespace-only filters mean absent, like summarize bounds; dates are plain calendar-day strings with no timezone conversion — pass IST dates. `id` cannot be combined with the filters. `dry_run` defaults to **true** and only reports the matching rows (uncapped by design — a dry run must show everything it would delete); pass `dry_run=false` to delete |
| `edit_transaction` | Edit one transaction by id (balances rebalanced atomically — old legs reversed, new legs applied). Required: id (missing rejected, not a silent 0). Optional: amount (exact paise: > 0, at most 2 decimals), type/category (validated like writes, except blank means no change — pass explicit `other` to reset the category), account/sending_to (transfer requires sending_to when the result would otherwise have no destination (converting a non-transfer into one) — edits on an existing transfer keep its legs unless a new one is passed, and it must differ; forbidden on non-transfers except whitespace-only (counts as absent); moving a leg onto an archived account rejects, but resupplying the leg already on the row does not — copying `account` from a query back into the edit keeps working), date (real calendar date, IST day like writes when supplied — empty/whitespace means no change, unlike log where it means today; moves expiresAt, so a far-past date purges quickly via TTL while the rebalanced delta stays), note (capped at 300 bytes; blank means leave alone, cannot be cleared). Absent/whitespace-only means no change; at least one field must change. Returns the edited row read back by id |
| `prune_old` | Immediate purge, defaulting to the 90-day retention window (TTL does this natively; manual override). Cutoff uses the same UTC frame as TTL (not IST like user-visible dates); boundary note: prune keeps `date == cutoff` while TTL expires it at 00:00 UTC today, so dry-run counts can trail TTL by a day. Does NOT touch balances — unlike `delete`, which inverts the legs, and `edit_transaction`, which reverses/applies them. Optional: days (any `<= 0` means 90, including negatives — never an error), dry_run (absent means true) |

## Schema (MongoDB `hermes` DB)

**`money_accounts`** — `name` (unique), `type`, `balance` (int64 paise,
stored running balance), `archived`, `createdAt`. Never expires.

**`money_transactions`** — `date` (YYYY-MM-DD), `amount` (int64 paise, >0),
`type` (income|expense|transfer), `category`, `note`, `source`, `accountId`
(required), `sending_to` (transfers only: receiving account), `idempotencyKey`
(optional, unique + sparse), `createdAt`, `expiresAt`
(= UTC midnight of date + 90d, ≈05:30 IST — not IST midnight; TTL target).

Indexes: `uniq_name` (accounts), `ttl_expiresAt`, `uniq_idempotency`,
`acct_date`, `sendingto_date`, `date_type`, `cat_date`. No `$jsonSchema`
validators and no views — validation lives in the store.

## Rejected input

`query_transactions` and `summarize` validate their date arguments in the handler, because
`required` in the input schema is a client-side hint and a caller can still send
`"start": ""` and pass validation:

| input | result |
|---|---|
| `query_transactions` with empty or non-`YYYY-MM-DD` bounds | rejected — used to match zero rows and return them as a real answer |
| `query_transactions` with `start` after `end` | rejected — two valid dates, empty range, used to return zero rows as if that were the answer |
| `query_transactions` with padded bounds (`" 2026-01-01 "`) | accepted, and the **trimmed** value is what reaches the store and the filter — validation and filtering both trim, so padding is harmless |
| `query_transactions` with `offset: -1` | rejected — a negative skip is a typo, not page 0 (validated in handler and store) |
| `query_transactions` with `limit: 0` (or negative) | not unlimited: the MCP default of 100 applies (check the echoed `limit` — expect 100); unlimited is store-internal only |
| `query_transactions` with `limit: 9999` | clamped to 500 — check the echoed `limit` (and `count` vs `total`): large pages still come back paged, with `total`/`truncated`/`next_offset` saying so |
| `summarize` with `start` but no `end` (or the reverse) | rejected — used to fall through to the period branch and summarise a window nobody asked for |
| `summarize` with `start` after `end` | rejected, like `query_transactions` — two valid dates in the wrong order, documented on the tool rather than left to be discovered |
| `summarize` with neither bound | fine — the period phrase applies, defaulting to `this month` |
| `summarize` with both an explicit pair and a `period` | the explicit pair wins; stated in the tool description rather than left to be discovered |
| `summarize` with a whitespace-only bound | treated as absent, not as half a pair |
| `edit_transaction` with a missing id | rejected like delete by id, not a silent no-op |
| `edit_transaction` with no edit fields | rejected — setting nothing is a no-op the caller almost certainly did not mean |
| `edit_transaction` with `amount: 0` (or inexact) | rejected like writes — 0 is not a correction, inexact would match paise it never stored |

`store.Prune` re-defaults any `days <= 0` to the retention window, so a negative `prune_old.days`
does not reach the delete with a nonsense cutoff.

### Key invariants

- **Atomicity:** every mutation runs in a multi-document transaction that
  writes the doc(s) *and* the `$inc` balance delta(s) together. Never write
  these collections directly — only via `store.go`.
- **Balances survive expiry:** retention purges (TTL or `prune_old`) delete
  docs *without* touching balances. Only a user-initiated rewrite of an
  existing row moves a balance: `delete` inverts the legs, `edit_transaction`
  reverses the old legs and applies the new ones.
  Balance = all activity ever absorbed (not just the
  surviving 90-day window).
- **Transfers** are a single doc (`accountId` = from, `sending_to` = to);
  enforced in `store.go` at write time: both accounts exist and are
  unarchived, from ≠ to. A row keeps its legs if an account is archived
  later — that row stays readable, deletable, and editable in place.

## Run

```bash
cp .env.example .env   # MONGODB_URI required
go run ./cmd/miser-money          # stdio transport (or go build -o miser-money ./cmd/miser-money)
```

## Client config

```json
{
  "mcpServers": {
    "miser-money": {
      "command": "/usr/local/bin/miser-money",
      "args": []
    }
  }
}
```

## Files (Go, `internal/money/` + `cmd/miser-money/`)

- `cmd/miser-money/main.go` — MCP server + tool definitions
- `internal/money/store.go` — accounts, atomic transactions, stored balances
- `internal/money/parse.go` — period resolution (`this month`, `June`, …)
- `internal/money/money_test.go` — tests (`go test ./internal/money/`, needs `MONGODB_URI`)
