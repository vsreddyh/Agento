# Health-check MCP — `mcps/health_check/`

Flat food log + weight + sleep + workouts. All permanent: no TTL index, no
prune tool, retention is a no-op for all five collections.

- `hc_food_log`: `datetime` (Date, =now if absent), `day` (YYYY-MM-DD IST,
  indexed), `what` + `what_lc` (lowercased copy for exact-match filter),
  `qty` free-text, `kcal/protein/carbs/fat/fiber` (optional 1-decimal, >= 0,
  absent means unknown; absurd values fail: kcal>100000, others>10000),
  `note?`, `createdAt`. One row = one thing eaten. No meals, no totals.
- `hc_weight`: `date` (unique), `kg` (0 < kg <= 500), `createdAt` (set once on insert, never rewritten by re-logs). Re-logging overwrites `kg` only.
- `hc_days`: `date` (unique), `steps` (0–1000000), `active_kcal` (0–100000),
  `updatedAt` (always Date) — scalars only, written through the single
  `RecordSync` path the MCP and `health-api/sync` share.
- `hc_sleep`: `day` (indexed), `start?` (Date), `minutes` (whole, 1-1440), `hash` (content
  dedup, unique), `createdAt`. Sleep hours are always derived from sessions.
- `hc_workouts`: `day` (indexed), `type` (lowercased on write, empty → `other`,
  max 60 runes), `minutes` (whole), `kcal?` (absent means unknown, never
  stored as 0), `hash` (content dedup, unique), `createdAt`.

Dedup hashes live on top-level docs with scalar unique indexes: unique
multikey indexes do not enforce on this cluster (probed), so sessions and
workouts are collections, not subdocs. A re-sent sync skips quietly;
a manual double-log returns a friendly duplicate error. Exact-duplicate
same-day repeats are unrepresentable by design (double-submit protection):
two identical naps or two identical runs in one day collide on purpose —
vary any field, or delete and re-log, to record both.

## Tools (17)

Food (id-keyed, full CRUD): `log_food(what, qty?, macros?, note?, datetime?)`,
`list_food(start, end, sort_by?, sort?, what?)`, `get_food(id)`,
`correct_food(id, patch…, clear_macros?)` (ask), `delete_food(id, dry_run=true)` (ask).
Weight (date-keyed, upsert is the correction): `log_weight(kg, date?)`,
`list_weight(…)`, `delete_weight(date, dry_run=true)` (ask).
Sleep (session-keyed): `log_sleep(hours, date?)`, `list_sleep(…)`,
`correct_sleep(id, minutes|hours — one of the two, both-set fails)` (ask),
`delete_sleep(id, dry_run=true)` (ask). Workouts (entry-keyed):
`log_workout(type?, minutes, kcal?, date?)` (empty type → `other`),
`list_workouts(start, end, sort_by?, sort?, type?)`, `correct_workout(id, …)`
(ask), `delete_workout(id, dry_run=true)` (ask).
Read path: `daily_summary(date?)` — food-derived cal-in with per-macro
counts (`macro_counts`), plus `workout_kcal` + `active_kcal` as separate
cal-out lines (no single net: Health Connect active calories already
include exercise).

Sort is two enums on every `list_*`: `sort_by` names only that list's
fields (`list_food`: `datetime,kcal`; `list_weight`: `date,kg`;
`list_sleep`: `day,sleep_hours`; `list_workouts`: `day,minutes,kcal`),
`sort` is `asc|desc`. Unknown values return errors on both layers (schema
first, then the store's `fail()` naming the allowed set); the applied pair
is echoed along with `count`/`total`/`truncated` (cap 500: rows for the
flat lists, day-docs for the grouped ones — a returned day always carries
all of its sessions/workouts; a truncated page means narrow the dates).
Value sorts (`sleep_hours`/`minutes`/`kcal`) apply within the returned page,
and `kcal` sorts put unknown-macro rows first in asc (absent means unknown,
never zero). Filters are exact match on lowered text and return the empty
page when nothing matches; bad dates/ids return errors.

Run: `go run ./cmd/health-check` (stdio), needs `MONGODB_URI`/`MONGODB_DB`.
Built binary: `bin/health-check` (via `./scripts/build-mcps.sh`), launched by
T3 through `./scripts/mcp-run.sh health-check` — there is no health-check
container in the stack, so never `exec` into one for it.

Upgrading from the meals era is gated: `EnsureSchema` refuses to boot with
a non-empty frozen `hc_meals` (back it up or wipe it with `mongo delete
hc_meals '{}'`); an empty one is dropped quietly. "Permanent" below means no
TTL/prune going forward, not that the pre-v2 shape is preserved.
