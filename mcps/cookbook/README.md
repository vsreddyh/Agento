# Cookbook MCP — `mcps/cookbook/`

Reusable recipe library. Permanent data — never pruned.

Collections: `cookbook_ingredients` (name unique + optional note),
`cookbook_recipes` (dish name unique, per-serving `kcal/protein_g/carbs_g/fat_g/fiber_g`,
`quantities: [{ingredient_id, name, qty}]` where `qty` is free text like `"2 spoons"`),
`cookbook_cook_log` (`recipe_id + date + cooking_note + aftertaste_note`).

Flow: `add_ingredient` once → `add_recipe` once → `log_cook` per attempt →
`update_recipe` only when the user approves. `scale_recipe` is pure math.

## Which arguments are required

The tool descriptions are the contract; this is the short form of it, because three of
these are not guessable from the argument names.

| Tool | Required | Optional (and what absent means) |
|---|---|---|
| `add_ingredient` | `name` | `note` |
| `list_ingredients` | — | `search` |
| `delete_ingredient` | `name_or_id` | — |
| `add_recipe` | `name`, `ingredient_qtys`, `per_serving` | `servings` (0 → 1), `note`, `tags` |
| `get_recipe` | `name_or_id` | — |
| `list_recipes` | — | `search`, `tag`, `ingredient` |
| `update_recipe` | `name_or_id` | everything else — **empty/zero args are left alone** |
| `delete_recipe` | `name_or_id` | — |
| `scale_recipe` | `name_or_id`, `servings` | — (nothing defaults `servings` here) |
| `log_cook` | `recipe` | `cooking_note`, `aftertaste_note`, `date` (absent → today) |
| `list_cooks` | — | `recipe`, `limit` (0 → 50) |

`update_recipe` is patch-shaped and is the only way a recipe changes after a cook, so
send only the keys you mean to change — a resend of fields you read earlier is how an
approved edit becomes a whole-record overwrite.

Run: `go run ./cmd/cookbook` (stdio), needs `MONGODB_URI`/`MONGODB_DB`.
In-container binary: `/usr/local/bin/cookbook`.
