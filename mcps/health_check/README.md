# Health-check MCP — `mcps/health_check/`

Cal-in + cal-out + weight. Minimal schemas (old `food_*` abandoned, no migration):

- `hc_meals`: `date`, `items[{name, qty?, kcal, protein, carbs, fat, fiber}]`,
  `totals` (computed), `createdAt`. Agent parses text → MCP validates, naming
  any missing macro field.
- `hc_weight`: `date` (unique), `kg`, `createdAt`. Never pruned.
- `hc_days`: `date` (unique), `steps`, `active_kcal`, `sleep_hours`,
  `workouts[{type, minutes, kcal}]`, `updatedAt`. One doc per date — MCP and
  health-api write the same shape (no field drift).

## Which arguments are required

| Tool | Required | Optional (and what absent means) |
|---|---|---|
| `log_meal` | `items` | `description`, `date` (absent → today) |
| `fix_last_meal` | `items` | `description` |
| `query_meals` | `start`, `end` | — (both go through `mustDay`, which rejects `""`) |
| `delete_meals` | — | `date` (absent → today) |
| `log_weight` | `kg` | `date` (absent → today) |
| `log_sleep` | `hours` | `date` (absent → today) |
| `log_workout` | `minutes` | `type` (absent → `"workout"`), `kcal`, `date` |
| `daily_summary` | — | `date` (absent → today) |
| `prune_old` | — | `days` (0 → 30), `dry_run` (absent → true) |

Note the two `"date"` behaviours in the same server: `mustDay` rejects an empty string and
`dayOf` fills it in, so which one a tool uses decides whether its date is required.

`log_meal.description` and `fix_last_meal.description` are accepted by the store and
**never written to the document**. They are still in the tool schema, because the SDK
emits `additionalProperties: false` and removing the field would break every existing
caller. Storing them is an open change to the meal document's shape.

Run: `go run ./cmd/health-check` (stdio), needs `MONGODB_URI`/`MONGODB_DB`.
In-container binary: `/usr/local/bin/health-check`.
