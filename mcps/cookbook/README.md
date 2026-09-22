# Cookbook MCP — `mcps/cookbook/`

Reusable recipe library. Permanent data — never pruned.

Collections: `cookbook_ingredients` (name unique + optional note),
`cookbook_recipes` (dish name unique, per-serving `kcal/protein_g/carbs_g/fat_g/fiber_g`,
`quantities: [{ingredient_id, name, qty}]` where `qty` is free text like `"2 spoons"`),
`cookbook_cook_log` (`recipe_id + date + cooking_note + aftertaste_note`).

Flow: `add_ingredient` once → `add_recipe` once → `log_cook` per attempt →
`update_recipe` only when the user approves. `scale_recipe` is pure math.

Run: `go run ./cmd/cookbook` (stdio), needs `MONGODB_URI`/`MONGODB_DB`.
In-container binary: `/usr/local/bin/cookbook`.
