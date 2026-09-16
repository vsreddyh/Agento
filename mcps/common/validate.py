"""Shared validators for Hermes MCPs."""
from __future__ import annotations

import datetime as dt
import re

# Shared contracts: DATE_RE enforces YYYY-MM-DD; MACRO_KEYS is the 5-macro set.
DATE_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")
MACRO_KEYS = ("kcal", "protein", "carbs", "fat", "fiber")


# Domain error: caught by servers and returned as {ok: False, error}.
class StoreError(ValueError):
    pass


# utcnow: timezone-aware timestamps so createdAt/updatedAt compare correctly.
def utcnow() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc)


# check_day: rejects bad format AND non-calendar dates (e.g. 2026-02-30).
def check_day(day: str) -> str:
    day = (day or "").strip()
    if not DATE_RE.match(day):
        raise StoreError(f"bad date '{day}' — use YYYY-MM-DD")
    try:
        dt.date.fromisoformat(day)
    except ValueError:
        raise StoreError(f"bad date '{day}' — not a real calendar date")
    return day


# check_macros: all 5 macros required, numeric, >= 0; names the missing field for the agent.
def check_macros(d: dict, ctx: str = "item") -> dict:
    for k in MACRO_KEYS:
        if k not in d:
            raise StoreError(f"{ctx} missing '{k}' — ask the user for it")
        try:
            v = float(d[k])
        except (TypeError, ValueError):
            raise StoreError(f"{ctx} field '{k}' must be a number")
        if v < 0:
            raise StoreError(f"{ctx} field '{k}' must be >= 0")
    return d


# sum_totals: aggregates item macros to 1-decimal totals for meal/day summaries.
def sum_totals(items: list[dict]) -> dict:
    return {k: round(sum(float(i.get(k, 0)) for i in items), 1) for k in MACRO_KEYS}
