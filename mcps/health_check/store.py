"""MongoDB storage for the health-check MCP.

Collections:
  hc_meals    date, items [{name, qty?, kcal, protein, carbs, fat, fiber}], totals, createdAt
  hc_weight   date (unique), kg, createdAt — NEVER pruned
  hc_days     date (unique), steps, active_kcal, sleep_hours, workouts [{type, minutes, kcal}], updatedAt
"""
from __future__ import annotations

import datetime as dt
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from common.mongo import get_db
from common.validate import StoreError, check_day, check_macros, sum_totals, utcnow


# Store: binds hc_meals/hc_weight/hc_days + unique date indexes for upserts.
class Store:
    def __init__(self, uri: str = "", db_name: str = ""):
        if uri:
            os.environ["MONGODB_URI"] = uri
        if db_name:
            os.environ["MONGODB_DB"] = db_name
        db = get_db()
        self._meals = db["hc_meals"]
        self._weight = db["hc_weight"]
        self._days = db["hc_days"]
        self._weight.create_index("date", unique=True)
        self._days.create_index("date", unique=True)
        self._meals.create_index("date")

    # ── meals (cal-in) ──
    # _check_items: every item needs a name + all 5 macros; normalizes types/lengths.
    def _check_items(self, items: list) -> list[dict]:
        if not items:
            raise StoreError("log_meal needs at least one item with kcal/protein/carbs/fat/fiber")
        out = []
        for i, it in enumerate(items):
            if not isinstance(it, dict) or not (it.get("name") or "").strip():
                raise StoreError(f"item[{i}] needs a name")
            check_macros(it, ctx=f"item[{i}] '{it.get('name')}'")
            out.append({"name": it["name"].strip(),
                        "qty": str(it.get("qty", ""))[:100],
                        "kcal": float(it["kcal"]), "protein": float(it["protein"]),
                        "carbs": float(it["carbs"]), "fat": float(it["fat"]),
                        "fiber": float(it["fiber"])})
        return out

    # log_meal: validates day/items, stores precomputed totals for cheap reads.
    def log_meal(self, day: str, description: str, items: list) -> dict:
        day = check_day(day)
        items = self._check_items(items)
        doc = {"date": day, "items": items, "totals": sum_totals(items),
               "createdAt": utcnow()}
        doc["_id"] = str(self._meals.insert_one(doc).inserted_id)
        doc["createdAt"] = str(doc["createdAt"])
        return doc

    # query_meals: date-range read; ObjectIds/dates stringified for JSON clients.
    def query_meals(self, start: str, end: str) -> list[dict]:
        rows = list(self._meals.find(
            {"date": {"$gte": check_day(start), "$lte": check_day(end)}}).sort("date", 1).limit(500))
        for r in rows:
            r["_id"] = str(r["_id"])
            r["createdAt"] = str(r.get("createdAt", ""))
        return rows

    # fix_last_meal: correction path; rewrites newest meal's items + totals.
    def fix_last_meal(self, description: str, items: list) -> dict | None:
        last = list(self._meals.find().sort("createdAt", -1).limit(1))
        if not last:
            return None
        items = self._check_items(items)
        self._meals.update_one({"_id": last[0]["_id"]},
                               {"$set": {"items": items, "totals": sum_totals(items)}})
        return {"date": last[0]["date"], "totals": sum_totals(items), "items": items}

    # delete_meals: removes all meals for one day (no balance logic involved).
    def delete_meals(self, day: str) -> int:
        return self._meals.delete_many({"date": check_day(day)}).deleted_count

    # ── weight ──
    # log_weight: one upserted row per date; 0–500 kg plausibility guard.
    def log_weight(self, day: str, kg: float) -> dict:
        day = check_day(day)
        try:
            kg = float(kg)
        except (TypeError, ValueError):
            raise StoreError(f"weight must be a number, got '{kg}'")
        if kg <= 0 or kg > 500:
            raise StoreError(f"implausible weight {kg} kg")
        self._weight.update_one({"date": day},
                                {"$set": {"kg": kg, "createdAt": utcnow()}},
                                upsert=True)
        return {"date": day, "kg": kg}

    # ── days (cal-out + steps + sleep) ──
    # log_sleep: upserted sleep_hours on the day row; 0–24 h plausibility guard.
    def log_sleep(self, day: str, hours: float) -> dict:
        day = check_day(day)
        try:
            hours = float(hours)
        except (TypeError, ValueError):
            raise StoreError(f"sleep hours must be a number, got '{hours}'")
        if hours <= 0 or hours > 24:
            raise StoreError(f"implausible sleep {hours} h")
        self._days.update_one({"date": day},
                              {"$set": {"sleep_hours": hours, "updatedAt": utcnow()}},
                              upsert=True)
        return {"date": day, "sleep_hours": hours}

    # log_workout: $push appends to workouts (history preserved); minutes > 0, kcal >= 0.
    def log_workout(self, day: str, type: str, minutes: float, kcal: float = 0) -> dict:
        day = check_day(day)
        try:
            minutes = float(minutes)
        except (TypeError, ValueError):
            raise StoreError(f"workout minutes must be a number, got '{minutes}'")
        if minutes <= 0:
            raise StoreError("workout minutes must be > 0")
        try:
            kcal = float(kcal or 0)
        except (TypeError, ValueError):
            raise StoreError(f"workout kcal must be a number, got '{kcal}'")
        if kcal < 0:
            raise StoreError("workout kcal must be >= 0")
        w = {"type": (type or "workout")[:60], "minutes": minutes, "kcal": kcal}
        self._days.update_one({"date": day},
                              {"$push": {"workouts": w}, "$set": {"updatedAt": utcnow()}},
                              upsert=True)
        return {"date": day, **w}

    # ── reads ──
    # daily_summary: joins meals + weight + day row; cal_out = workouts + active_kcal.
    def daily_summary(self, day: str) -> dict:
        day = check_day(day)
        meals = list(self._meals.find({"date": day}, {"_id": 0}))
        totals = sum_totals([i for m in meals for i in m.get("items", [])])
        w = self._weight.find_one({"date": day}, {"_id": 0})
        d = self._days.find_one({"date": day}, {"_id": 0})
        wo = (d or {}).get("workouts", [])
        burn = round(sum(x.get("kcal", 0) for x in wo) + float((d or {}).get("active_kcal") or 0), 1)
        return {"date": day, "meals": len(meals), "totals": totals,
                "weight": w, "day": d, "workouts": wo,
                "cal_in": totals["kcal"], "cal_out": burn,
                "net": round(totals["kcal"] - burn, 1)}

    # prune: deletes meals/days older than cutoff; weight collection never touched.
    def prune(self, days: int = 30, dry_run: bool = True) -> dict:
        cutoff = (dt.date.today() - dt.timedelta(days=days)).isoformat()
        filt = {"date": {"$lt": cutoff}}
        counts = {"meals": self._meals.count_documents(filt),
                  "days": self._days.count_documents(filt)}
        if not dry_run:
            self._meals.delete_many(filt)
            self._days.delete_many(filt)
        return {"cutoff": cutoff, **counts}


# from_env: builds Store from MONGODB_URI/MONGODB_DB (single root .env).
def from_env() -> Store:
    return Store(os.environ.get("MONGODB_URI", ""),
                 os.environ.get("MONGODB_DB", "hermes"))
