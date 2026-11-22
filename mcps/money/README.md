# Miser — Multi-Account Money MCP Server

MCP server port of the retired money bot profile,
extended to **multiple accounts** with **derived balances** (no stored running total — balances are computed from the ledger on every read).

## Tools

| Tool | Purpose |
|---|---|
| `create_account` | Create an account with starting balance. Required: name (trimmed, case-sensitive — `Cash` and `cash` are different accounts). Optional: type (defaults to cash, empty or whitespace-only; case-insensitive + trimmed, so `Bank` stores `bank` like transaction types), balance (opening balance seed in the same call, defaults to 0, must be exact paise like every amount; stored once, never updated) |
| `list_accounts` | List accounts with derived balances **and ids** — hides archived by default; pass `include_archived=true` to see them. Transaction rows reference accounts by id, so this is the id→name map. Optional: include_archived |
| `archive_account` | Soft-delete (history stays queryable; blocked from new `log_transaction` writes — `delete`/`edit` still apply to its rows). `name` accepts a name or id hex (trimmed, so an `_id` from `list_accounts` round-trips); the stored name is echoed back, never the raw input. The name stays taken. Required: `name` (put the hex here for id) |
| `unarchive_account` | Restore an archived account so it accepts writes again (inverse of `archive_account`). `name` accepts a name or id hex (trimmed); the stored name is echoed back. Required: `name` (put the hex here for id) |
| `get_balances` | Per-account balances (including archived rows) + total across active accounts only — `total` is NOT the sum of `accounts[].balance` once anything is archived, unlike `list_accounts` which hides archived by default. Takes no arguments |
| `log_transaction` | Log income / expense / transfer. Required: type, amount (exact paise: > 0, at most 2 decimals — stored as int64), account (account names are case-sensitive exact-match, unlike type/category — `cash` does not find `Cash`). Date must be a real `YYYY-MM-DD` calendar date (padded dates trimmed; `2026-02-30` rejected like query). Rows never expire, so any real date stores with immediate balance effect and stays until deleted, even a far-future one (visible only to explicit start/end queries covering it, or to a by-id fetch)). Optional: category (blank or whitespace-only = `other`; an unknown name is **rejected**, not silently refiled), sending_to (required by the store for a transfer, name or id hex like account, must differ from the source, and forbidden on non-transfers except whitespace-only (counts as absent), rejected otherwise), note (capped at 300 bytes, cut on a UTF-8 boundary — long text reads back truncated, not rejected), date (defaults to today in IST; empty or whitespace-only also means today, unlike query's rejected empty bounds in range mode), idempotency_key (trimmed, max 200 bytes — a UUID is 36; longer is rejected; blank/whitespace-only means no key, each call logs anew; a retry returns the original id with `created: false` instead of double-logging; the same key with a different date/amount/type/category/account/sending_to is **rejected** — use a new key for a new request; `note` differences are ignored on replay (first write wins, a correction needs a new key; there is no `source` field — it is always `log_transaction` via MCP). Returns the stored `category`/`account`/`note` (normalized) plus `sending_to`/`sending_to_account` for transfers; if the post-write re-read fails, `ok` stays true with `readback_error` |
| `query_transactions` | Fetch one transaction by id, or list in a date range. Give an id OR a start/end pair — an id addresses one row, so it cannot be combined with any other argument except the zero-valued paging defaults (an unknown id is rejected, not a silent empty page). Without an id both bounds are required; paging below is range-mode only: start, end — both must be real `YYYY-MM-DD` dates or the call is rejected (padded bounds trimmed, like log/delete). Bounds are plain calendar-day strings with no timezone conversion — pass IST dates to match how rows are stored. Optional: type/category/account filter (type/category are lowercased+trimmed like writes, so `Groceries` finds `groceries`, but an unknown value is **rejected** like writes instead of matching nothing; account (case-sensitive exact name or id hex, unlike type/category; matches EITHER side of a transfer, so a transfer appears under both sender and receiver). Paged: `limit` defaults to 100, clamped to 500; `offset` defaults to 0. There is no unlimited `query_transactions` page on MCP (delete dry-runs stay unbounded by design): 0/negative `limit` also means 100, unlimited is store-internal only. The response carries `count` (rows in this page — unlike `total`, which counts all matches), the applied `limit`, `total`, `truncated` and `next_offset` so a short page is never mistaken for the end: to read everything, repeat with `offset = next_offset` until `truncated` is false (`next_offset` is null then). Rows come back date-ascending with an `_id` tiebreak (oldest first; deterministic order, but not a snapshot — a concurrent log between page fetches can shift rows across page boundaries). Returns each row with its hex `_id`, so a queried row can be deleted or edited by `id` |
| `delete_transactions` | Delete by `id` (a non-existent id is **rejected**, not a silent 0), or by amount (exact paise: > 0, at most 2 decimals; an explicit 0 or >2-decimal value is rejected, not a wildcard)/type/category/date/account (removed rows stop contributing to balances; account takes a case-sensitive exact name or id hex like query (unlike type/category) and matches EITHER side of a transfer — check `dry_run` first, since a receiver-side transfer is included; whitespace-only means absent; unknown type/category **rejected** like writes, not a silent `would_delete: 0`). Date filter trimmed + calendar-validated like writes/reads; whitespace-only filters mean absent; dates are plain calendar-day strings with no timezone conversion — pass IST dates. `id` cannot be combined with the filters. `dry_run` defaults to **true** and only reports the matching rows (uncapped by design — a dry run must show everything it would delete); pass `dry_run=false` to delete |
| `edit_transaction` | Edit one transaction by id (the row changes in place; balances derive from rows, so no separate rebalance step exists). Required: id (missing rejected, not a silent 0). Optional: amount (exact paise: > 0, at most 2 decimals), type/category (validated like writes, except blank means no change — pass explicit `other` to reset the category), account/sending_to (case-sensitive exact name or id hex like writes, unlike type/category; transfer requires sending_to when the result would otherwise have no destination (converting a non-transfer into one) — edits on an existing transfer keep its legs unless a new one is passed, and it must differ; forbidden on non-transfers except whitespace-only (counts as absent); moving a leg onto an archived account rejects, but resupplying the leg already on the row does not — copying `account` from a query back into the edit keeps working — even when converting away from a transfer, where the resupplied destination is ignored and the leg drops), date (real calendar date, IST day like writes when supplied — empty/whitespace means no change, unlike log where it means today; changes the row's date — rows never expire, so any past date stays put), note (capped at 300 bytes; blank means leave alone, cannot be cleared). Absent/whitespace-only means no change (except converting to transfer still requires `sending_to`); at least one field must change. Returns the edited row read back by id; if the re-read fails, `ok` stays true with `readback_error` (like log_transaction) |

## Schema (MongoDB `hermes` DB)

**`money_accounts`** — `name` (unique), `type`, `openingBalance` (int64 paise, immutable seed set once at creation), `archived`, `createdAt`. Never expires.

**`money_transactions`** — `date` (YYYY-MM-DD), `amount` (int64 paise, >0),
`type` (income|expense|transfer), `category`, `note`, `source`, `accountId`
(required), `sending_to` (transfers only: receiving account), `idempotencyKey`
(optional, unique + sparse), `createdAt`. Rows never expire: there is no
`expiresAt` field and no TTL index — a row lives until `delete_transactions`
removes it.

Indexes: `uniq_name` (accounts), `uniq_idempotency`,
`acct_date`, `sendingto_date`, `date_type`, `cat_date`. No `$jsonSchema`
validators and no views — validation lives in the store.

## Rejected input

`query_transactions` validates its date arguments in the handler, because
`required` in the input schema is a client-side hint and a caller can still send
`"start": ""` and pass validation:

| input | result |
|---|---|
| `query_transactions` with empty or non-`YYYY-MM-DD` bounds | rejected — used to match zero rows and return them as a real answer |
| `query_transactions` with `start` after `end` | rejected — two valid dates, empty range, used to return zero rows as if that were the answer |
| `query_transactions` with padded bounds (`" 2026-01-01 "`) | accepted, and the **trimmed** value is what reaches the store and the filter — validation and filtering both trim, so padding is harmless |
| `query_transactions` with `offset: -1` in range mode | rejected — a negative skip is a typo, not page 0 (validated in handler and store; non-zero paging args cannot be combined with `id` (zeros are the unset defaults)) |
| `query_transactions` with `limit: 0` (or negative) in range mode | not unlimited: the MCP default of 100 applies (check the echoed `limit` — expect 100); unlimited is store-internal only (by-id mode echoes `limit: 1` for its single row) |
| `query_transactions` with `limit: 9999` in range mode | clamped to 500 — check the echoed `limit` (and `count` vs `total`): large pages still come back paged, with `total`/`truncated`/`next_offset` saying so |
| `edit_transaction` with a missing id | rejected like delete by id, not a silent no-op |
| `edit_transaction` with no edit fields | rejected — setting nothing is a no-op the caller almost certainly did not mean |
| `edit_transaction` with `amount: 0` (or inexact) | rejected like writes — 0 is not a correction, inexact would match paise it never stored |

There is no expiry and no purge tool: rows live until `delete_transactions`
removes them (which inverts their balance legs). Nothing deletes a row
outside `delete_transactions`: a hand `DeleteMany` skips the `$inc` legs
that tool applies, so the stored running total diverges with no error. A
mistaken row is corrected with `edit_transaction` or removed with
`delete_transactions`, never by hand.

### Period totals (no `summarize` tool)

There is no server-side total. `get_balances` is a lifetime stored running
total — it is the wrong window for a period — and `query_transactions.total`
counts rows, not money. To total a period, paginate the whole range and sum
client-side:

- repeat `query_transactions` with `offset = next_offset` until `truncated`
  is false;
- sum in paise (`round(amount * 100)` per row), not in rendered rupee
  floats, so the total stays integer-exact;
- exclude `transfer` rows from income/expense/net (count them separately);
- rank per-category over expenses only.

### Key invariants

- **Single writes, derived reads:** every mutation is one document write —
  there is no second write that could half-land, so there is nothing to
  transact. Writes still go only through `store.go`: it validates (exact
  paise, known type/category, real calendar dates, transfer rules) and a
  hand-written row bypasses all of that.
- **No expiry:** rows live until deleted. A balance is the account's opening
  seed plus the signed sum of its legs — computed on read, never stored — so
  deleting or editing a row moves the totals with no separate rebalance step.
  Balance = all activity ever absorbed.
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
- `internal/money/parse.go` — IST day helper + categories
- `internal/money/money_test.go` — tests (`go test ./internal/money/`, needs `MONGODB_URI`)
