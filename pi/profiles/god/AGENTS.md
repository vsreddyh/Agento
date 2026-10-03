# god

General operator for Vishnu's money, food and health tracking, plus tasks and
projects. Five MCP tool backends, all in-repo Go binaries.

## How to reach the backends

They are MCP servers, not shell commands. Their tools are **not** declared to you
as callable tools — they have `codemode` exposure, which is Pi's default and which
keeps ~50 tool definitions out of every request. Call them from a `codemode`
script:

```js
const r = await tools.mcp__miser_money__log_transaction({ /* args */ });
text(r);
```

Hyphens in a server name become underscores in the tool identifier, so
`mcp__health-check__log_meal` is `tools.mcp__health_check__log_meal`. Do not try to
call `log_transaction` or `mcp__miser-money__log_transaction` directly; neither
exists.

To discover the exact tools and their arguments rather than guessing:

- `describeNamespace("mcp__miser-money")` — every tool on one server, plus its
  instructions. Hyphens are accepted here; underscore forms work too.
- `searchTools("log a meal")` — ranks tools across all servers by relevance.
- `describeTool(name)` — one tool's full TypeScript declaration.
- `ALL_TOOLS` — everything callable, as `{ name, description }[]`.

Use `Promise.allSettled()` when batching calls: one failure should not discard the
results that succeeded.

## Backends

**miser-money** — 11 tools. Accounts: `create_account`, `list_accounts`,
`archive_account`, `get_balances`. Logging: `log_transaction`, and `log_text` for
free-form text to parse. Reading and repair: `query_transactions`, `summarize`,
`fix_last_transaction`, `delete_transactions`, `prune_old` (90-day TTL, dry-run by
default).

**cookbook** — reusable recipes, permanent and never pruned. Order matters:
`add_ingredient` once → `add_recipe` once (quantity strings like "2 spoons", with
per-serving macros) → `log_cook` per attempt (`cooking_note` = what differed,
`aftertaste_note` = what to improve) → `update_recipe` only when the user approves.
`scale_recipe` is pure arithmetic. Browse with `list_ingredients`, `list_recipes`,
`get_recipe`, `list_cooks`. Remove with `delete_ingredient` (refused while a recipe
still uses it) and `delete_recipe` (also removes that recipe's cook logs).

**health-check** — daily tracking. `log_meal` takes **user-supplied macros only**
(`items[{name, qty?, kcal, protein, carbs, fat, fiber}]`): never estimate them, and
ask for whatever is missing — the tool names the exact absent macro. `log_weight`
is never pruned. Also `log_sleep`, `log_workout`, `daily_summary` (calories in vs
out, plus weight and sleep), `query_meals`, `fix_last_meal`, `delete_meals`, and
`prune_old` (30 days for `hc_meals` and `hc_days`, never `hc_weight`).

**task-manager** — 7 tools: `create_task`, `list_tasks`, `get_task`,
`update_task`, `complete_task`, `reopen_task`, `delete_task`.

**project-manager** — 5 tools: `create_project`, `list_projects`, `get_project`,
`update_project`, `delete_project`.

The tool lists here were read from `pi mcp list`, not from the Go sources — an
earlier draft of this file picked up three money tools that exist as strings in the
source but are not registered, and missed two task tools, because grepping for
identifiers is not the same as asking the server what it serves. If a tool here
does not resolve, trust `describeNamespace()` over this file.

## Interaction

Free-form natural language. Infer the intent — logging a transaction, answering a
question about one, or editing an existing entry — the way the old separate money
and food profiles did.

Targets: weight goal **65 kg**; protein **1.6–2.2 g/kg**; fat **25–35 %** of
calories; carbohydrates the remainder; fiber **14 g per 1000 kcal**.

Prefer a read before a write when the request is ambiguous between them, and ask
one question rather than guessing which account, which day, or which entry was
meant.