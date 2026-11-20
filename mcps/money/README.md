# Miser — Multi-Account Money MCP Server

MCP server port of the retired money bot profile,
extended to **multiple accounts** with **atomic stored balances**.

## Tools

| Tool | Purpose |
|---|---|
| `create_account` | Create an account with starting balance. Required: name (trimmed, case-sensitive — `Cash` and `cash` are different accounts). Optional: type (defaults to cash; case-insensitive + trimmed, so `Bank` stores `bank` like transaction types), balance (opening balance in the same call, defaults to 0) |
| `list_accounts` | List accounts with stored balances **and ids** — transaction rows reference accounts by id, so this is the id→name map. Optional: include_archived |
| `archive_account` | Soft-delete (history stays queryable; blocked from new `log_transaction` writes — `delete`/`fix_last` can still move its balance). `name` accepts a name or id hex (trimmed, so an `_id` from `list_accounts` round-trips); the stored name is echoed back, never the raw input. The name stays taken. Required: `name` (put the hex here for id) |
| `unarchive_account` | Restore an archived account so it accepts writes again (inverse of `archive_account`). `name` accepts a name or id hex (trimmed); the stored name is echoed back. Required: `name` (put the hex here for id) |
| `get_balances` | Per-account balances (including archived rows) + total across active accounts only — `total` is NOT the sum of `accounts[].balance` once anything is archived, unlike `list_accounts` which hides archived by default. Takes no arguments |
| `log_transaction` | Log income / expense / transfer. Required: type, amount (> 0), account (account names are case-sensitive exact-match, unlike type/category — `cash` does not find `Cash`). Date must be a real `YYYY-MM-DD` calendar date (padded dates trimmed; `2026-02-30` rejected like query/summarize — it used to store then TTL-purge immediately while the balance stayed). Optional: category (blank = `other`; an unknown name is **rejected**, not silently refiled), sending_to (required by the store for a transfer, rejected otherwise), note (capped at 300 bytes, cut on a UTF-8 boundary — long text reads back truncated, not rejected), date (defaults to today), idempotency_key (trimmed, max 200 bytes — a UUID is 36; longer is rejected; a retry returns the original id with `created: false` instead of double-logging; the same key with a different date/amount/type/category/account/sending_to is **rejected** — use a new key for a new request). Returns the stored `category`/`account`/`note` (normalized) plus `sending_to`/`sending_to_account` for transfers; if the post-write re-read fails, `ok` stays true with `readback_error` |
| `query_transactions` | List in a date range. Required: start, end — both must be real `YYYY-MM-DD` dates or the call is rejected. Optional: type/category/account filter (type/category are lowercased+trimmed like writes, so `Groceries` finds `groceries`, but an unknown value is **rejected** like writes instead of matching nothing; account matches EITHER side of a transfer, so a transfer appears under both sender and receiver). Returns each row with its hex `_id`, so a queried row can be deleted by `id` |
| `summarize` | Income, expense, net + per-category breakdown (transfers are counted in `count` but excluded from income/expense/net; `by_category` is expense-only, so income categories never rank). Give a period phrase **or** an explicit start/end pair — half a pair is rejected rather than ignored, neither means this month. Supported phrases: today, yesterday, this week, last month (spaceless `lastmonth` works), month names with optional 1000-2999 year (June, June 2025, June 2100), this month / so far / month — matched on whole words, so `maybe`/`mayhem`/`todays`/`monthly` do not hit a branch. Anything else means this month too (`2025`, `last year` and `banana` all return this month, not an error). Optional: account (either side of a transfer, like query) |
| `fix_last_transaction` | Correct the most recent entry by insertion order, not date (amount must be > 0; balances adjusted; returns the fixed row's `id` + `date` so the read-back-by-id rule holds). Required: amount (> 0) |
| `delete_transactions` | Delete by `id` (a non-existent id is **rejected**, not a silent 0), or by amount (> 0; an explicit 0 is rejected, not a wildcard)/type/category/date/account (balances inverted; account matches EITHER side of a transfer — check `dry_run` first, since a receiver-side transfer is included; unknown type/category **rejected** like writes, not a silent `would_delete: 0`). Date filter trimmed + calendar-validated like writes/reads. `id` cannot be combined with the filters. `dry_run` defaults to **true** and only reports the matching rows; pass `dry_run=false` to delete |
| `prune_old` | Immediate purge, defaulting to the 90-day retention window (TTL does this natively; manual override). Does NOT touch balances — unlike `delete`, which inverts them. Optional: days (any `<= 0` means 90, including negatives — never an error), dry_run (absent means true) |

## Schema (MongoDB `hermes` DB)

**`money_accounts`** — `name` (unique), `type`, `balance`
(stored running balance), `archived`, `createdAt`. Never expires.

**`money_transactions`** — `date` (YYYY-MM-DD), `amount` (>0), `type`
(income|expense|transfer), `category`, `note`, `source`, `accountId`
(required), `sending_to` (transfers only: receiving account), `idempotencyKey`
(optional, unique + sparse), `createdAt`, `expiresAt`
(= date + 90d, TTL target).

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
| `query_transactions` with padded bounds (`" 2026-01-01 "`) | accepted, and the **trimmed** value is what reaches the store — validation trims while the filter compares raw strings |
| `summarize` with `start` but no `end` (or the reverse) | rejected — used to fall through to the period branch and summarise a window nobody asked for |
| `summarize` with neither bound | fine — the period phrase applies, defaulting to `this month` |
| `summarize` with both an explicit pair and a `period` | the explicit pair wins; stated in the tool description rather than left to be discovered |
| `summarize` with a whitespace-only bound | treated as absent, not as half a pair |

`store.Prune` re-defaults any `days <= 0` to the retention window, so a negative `prune_old.days`
does not reach the delete with a nonsense cutoff.

### Key invariants

- **Atomicity:** every mutation runs in a multi-document transaction that
  writes the doc(s) *and* the `$inc` balance delta(s) together. Never write
  these collections directly — only via `store.go`.
- **Balances survive expiry:** retention purges (TTL or `prune_old`) delete
  docs *without* touching balances. Only user-initiated `delete` inverts.
  Balance = all activity ever absorbed (not just the
  surviving 90-day window).
- **Transfers** are a single doc (`accountId` = from, `sending_to` = to);
  enforced in `store.go`: both accounts exist, unarchived, from ≠ to.

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
