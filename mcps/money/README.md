# Miser — Multi-Account Money MCP Server

MCP server port of the retired money bot profile,
extended to **multiple accounts** with **atomic stored balances**.

## Tools

| Tool | Purpose |
|---|---|
| `create_account` | Create an account with starting balance. Required: name. Optional: type (defaults to cash), balance |
| `list_accounts` | List accounts with stored balances. Optional: include_archived |
| `archive_account` | Soft-delete (history stays, blocked from new writes). Required: name |
| `get_balances` | Per-account balances + total across active accounts. Takes no arguments |
| `log_transaction` | Log income / expense / transfer. Required: type, amount, account. Optional: category (an empty or unrecognised one is filed under `other`), sending_to (required by the store for a transfer, rejected otherwise), note, date (defaults to today) |
| `log_text` | Log from free-form text (`spent 300 on groceries`). Required: text. Optional: account override |
| `query_transactions` | List in a date range. Required: start, end — the store does not validate them, so absent bounds return nothing silently. Optional: type/category/account filter |
| `summarize` | Income, expense, net + per-category breakdown. Give a period phrase **or** an explicit start/end pair; optional account |
| `fix_last_transaction` | Correct the most recent entry (balances adjusted). Required: amount |
| `delete_transactions` | Delete by amount/category/date/account (balances inverted). At least one filter is required, so no single argument is marked required in the schema |
| `prune_old` | Immediate 90-day purge (TTL does this natively; manual override). Optional: days (0 means 90), dry_run (absent means true) |

## Schema (MongoDB `hermes` DB)

**`money_accounts`** — `name` (unique), `type`, `balance`
(stored running balance), `archived`, `createdAt`. Never expires.

**`money_transactions`** — `date` (YYYY-MM-DD), `amount` (>0), `type`
(income|expense|transfer), `category`, `note`, `source`, `accountId`
(required), `sending_to` (transfers only: receiving account), `createdAt`, `expiresAt`
(= date + 90d, TTL target).

Strict `$jsonSchema` validators + indexes (TTL, account/date, category/date)
+ views (`money_monthly_summary`, `money_category_breakdown`, `money_balances`).
See `schema.py`.

### Key invariants

- **Atomicity:** every mutation runs in a multi-document transaction that
  writes the doc(s) *and* the `$inc` balance delta(s) together. Never write
  these collections directly — only via `store.py`.
- **Balances survive expiry:** retention purges (TTL or `prune_old`) delete
  docs *without* touching balances. Only user-initiated `delete` inverts.
  Balance = all activity ever absorbed (not just the
  surviving 90-day window).
- **Transfers** are a single doc (`accountId` = from, `sending_to` = to);
  enforced in `store.py`: both accounts exist, unarchived, from ≠ to.

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
- `internal/money/parse.go` — free-form text parser
- `internal/money/money_test.go` — tests (`go test ./internal/money/`, needs `MONGODB_URI`)
