"""MongoDB storage for the cookbook MCP.

Collections (permanent — never pruned):
  cookbook_ingredients  name (unique), note, createdAt
  cookbook_recipes      name (unique), servings, kcal/protein_g/carbs_g/fat_g/fiber_g
                        (per-serving), quantities [{ingredient_id, name, qty}],
                        note, tags, source, createdAt, updatedAt
  cookbook_cook_log     recipe_id, date, cooking_note, aftertaste_note, createdAt
"""
from __future__ import annotations

import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from bson import ObjectId
from bson.errors import InvalidId
from pymongo.errors import DuplicateKeyError

from common.mongo import get_db
from common.validate import StoreError, check_day, utcnow

# Macro contract: every recipe stores all 5 per-serving macros (validated on write).
RECIPE_MACROS = ("kcal", "protein_g", "carbs_g", "fat_g", "fiber_g")


# _oid: strict id parse; StoreError steers callers to fall back to name lookup.
def _oid(s: str) -> ObjectId:
    try:
        return ObjectId(s)
    except (InvalidId, TypeError):
        raise StoreError(f"bad id '{s}'")


# _doc_out: stringifies ObjectIds/dates so MCP JSON responses stay serializable.
def _doc_out(d: dict) -> dict:
    d = dict(d)
    d["_id"] = str(d["_id"])
    for k in ("createdAt", "updatedAt"):
        if k in d and d[k] is not None:
            d[k] = str(d[k])
    if "recipe_id" in d and d["recipe_id"] is not None:
        d["recipe_id"] = str(d["recipe_id"])
    return d


# Store: binds the 3 cookbook collections + unique/name indexes on first use.
class Store:
    def __init__(self, uri: str = "", db_name: str = ""):
        if uri:
            os.environ["MONGODB_URI"] = uri
        if db_name:
            os.environ["MONGODB_DB"] = db_name
        db = get_db()
        self._ings = db["cookbook_ingredients"]
        self._recipes = db["cookbook_recipes"]
        self._log = db["cookbook_cook_log"]
        self._ings.create_index("name", unique=True)
        self._recipes.create_index("name", unique=True)
        self._log.create_index([("recipe_id", 1), ("date", 1)])

    # ── ingredients ──
    # add_ingredient: unique name (index-backed); note truncated to 300.
    def add_ingredient(self, name: str, note: str = "") -> dict:
        name = (name or "").strip()
        if not name:
            raise StoreError("ingredient name is required")
        doc = {"name": name, "note": (note or "")[:300], "createdAt": utcnow()}
        try:
            doc["_id"] = self._ings.insert_one(doc).inserted_id
        except DuplicateKeyError:
            raise StoreError(f"ingredient '{name}' already exists")
        return _doc_out(doc)

    # list_ingredients: case-insensitive substring search; empty search lists all.
    def list_ingredients(self, search: str = "") -> list[dict]:
        filt = {"name": {"$regex": re.escape(search.strip()), "$options": "i"}} if search.strip() else {}
        return [_doc_out(r) for r in self._ings.find(filt).sort("name", 1).limit(500)]

    # delete_ingredient: id-or-name lookup; refused while any recipe references it.
    def delete_ingredient(self, name_or_id: str) -> bool:
        try:
            ing = self._ings.find_one({"_id": _oid(name_or_id)})
        except StoreError:
            ing = self._ings.find_one({"name": (name_or_id or "").strip()})
        if not ing:
            raise StoreError(f"unknown ingredient '{name_or_id}'")
        ref = self._recipes.find_one({"quantities.ingredient_id": str(ing["_id"])}, {"_id": 0, "name": 1})
        if ref:
            raise StoreError(f"ingredient '{ing['name']}' is used by recipe '{ref['name']}' — edit the recipe first")
        return self._ings.delete_one({"_id": ing["_id"]}).deleted_count > 0

    # ── recipes ──
    # _resolve_ings: maps name-or-id qty keys to {ingredient_id, name, qty}; all must exist.
    def _resolve_ings(self, qtys: dict) -> list[dict]:
        out = []
        for key, qty in (qtys or {}).items():
            try:
                ing = self._ings.find_one({"_id": _oid(key)})
            except StoreError:
                ing = self._ings.find_one({"name": (key or "").strip()})
            if not ing:
                raise StoreError(f"unknown ingredient '{key}' — add it with add_ingredient first")
            out.append({"ingredient_id": str(ing["_id"]), "name": ing["name"], "qty": str(qty or "")[:100]})
        if not out:
            raise StoreError("recipe needs at least one ingredient quantity")
        return out

    # _check_macros: all 5 macros required, numeric, >= 0, rounded to 1 decimal.
    def _check_macros(self, per_serving: dict) -> dict:
        if not isinstance(per_serving, dict):
            raise StoreError("per_serving must be a dict")
        out = {}
        for k in RECIPE_MACROS:
            if k not in per_serving:
                raise StoreError(f"per_serving missing '{k}' — ask the user for it")
            try:
                v = float(per_serving[k])
            except (TypeError, ValueError):
                raise StoreError(f"per_serving '{k}' must be a number")
            if v < 0:
                raise StoreError(f"per_serving '{k}' must be >= 0")
            out[k] = round(v, 1)
        return out

    # add_recipe: validates servings/macros/ingredients; tags capped at 20x40 chars.
    def add_recipe(self, name: str, ingredient_qtys: dict, per_serving: dict,
                   servings: float = 1, note: str = "", tags: list | None = None,
                   source: str = "mcp") -> dict:
        name = (name or "").strip()
        if not name:
            raise StoreError("recipe name is required")
        try:
            servings = float(servings or 0)
        except (TypeError, ValueError):
            raise StoreError(f"servings must be a number, got '{servings}'")
        if servings <= 0:
            raise StoreError("servings must be > 0")
        doc = {"name": name, "servings": servings,
               **self._check_macros(per_serving or {}),
               "quantities": self._resolve_ings(ingredient_qtys or {}),
               "note": (note or "")[:300], "tags": [str(t)[:40] for t in (tags or [])][:20],
               "source": (source or "mcp")[:64], "createdAt": utcnow(), "updatedAt": utcnow()}
        try:
            doc["_id"] = self._recipes.insert_one(doc).inserted_id
        except DuplicateKeyError:
            raise StoreError(f"recipe '{name}' already exists")
        return _doc_out(doc)

    # get_recipe: id-or-name lookup; None when unknown (callers turn it into an error).
    def get_recipe(self, name_or_id: str) -> dict | None:
        try:
            r = self._recipes.find_one({"_id": _oid(name_or_id)})
        except StoreError:
            r = self._recipes.find_one({"name": (name_or_id or "").strip()})
        return _doc_out(r) if r else None

    # list_recipes: optional name/tag/ingredient filters combined with AND.
    def list_recipes(self, search: str = "", tag: str = "", ingredient: str = "") -> list[dict]:
        filt: dict = {}
        if search.strip():
            filt["name"] = {"$regex": re.escape(search.strip()), "$options": "i"}
        if tag.strip():
            filt["tags"] = tag.strip()
        if ingredient.strip():
            filt["quantities.name"] = {"$regex": re.escape(ingredient.strip()), "$options": "i"}
        return [_doc_out(r) for r in self._recipes.find(filt).sort("name", 1).limit(200)]

    # update_recipe: partial patch; only provided keys change, updatedAt always bumped.
    def update_recipe(self, name_or_id: str, **patch) -> dict | None:
        try:
            filt = {"_id": _oid(name_or_id)}
        except StoreError:
            filt = {"name": (name_or_id or "").strip()}
        r = self._recipes.find_one(filt)
        if not r:
            return None
        upd: dict = {"updatedAt": utcnow()}
        if "name" in patch and patch["name"]:
            upd["name"] = patch["name"].strip()
        if "servings" in patch and patch["servings"] is not None:
            s = float(patch["servings"])
            if s <= 0:
                raise StoreError("servings must be > 0")
            upd["servings"] = s
        if "per_serving" in patch and patch["per_serving"] is not None:
            upd.update(self._check_macros(patch["per_serving"]))
        if "ingredient_qtys" in patch and patch["ingredient_qtys"] is not None:
            upd["quantities"] = self._resolve_ings(patch["ingredient_qtys"])
        for k in ("note", "tags", "source"):
            if k in patch and patch[k] is not None:
                if k == "note":
                    upd[k] = str(patch[k])[:300]
                elif k == "tags":
                    upd[k] = [str(t)[:40] for t in patch[k]][:20]
                else:
                    upd[k] = str(patch[k])[:64]
        try:
            self._recipes.update_one({"_id": r["_id"]}, {"$set": upd})
        except DuplicateKeyError:
            raise StoreError(f"recipe '{upd.get('name')}' already exists")
        return self.get_recipe(str(r["_id"]))

    # delete_recipe: removes recipe + its cook-log rows so no orphan logs remain.
    def delete_recipe(self, name_or_id: str) -> dict:
        r = self.get_recipe(name_or_id)
        if not r:
            return {"deleted": False, "cook_logs": 0}
        oid = _oid(r["_id"])
        logs = self._log.delete_many({"recipe_id": oid}).deleted_count
        self._recipes.delete_one({"_id": oid})
        return {"deleted": True, "cook_logs": logs}

    # scale_recipe: pure math (factor = target/base); free-text qtys never scaled.
    def scale_recipe(self, name_or_id: str, servings: float) -> dict:
        r = self.get_recipe(name_or_id)
        if not r:
            raise StoreError(f"unknown recipe '{name_or_id}'")
        try:
            servings = float(servings)
        except (TypeError, ValueError):
            raise StoreError(f"servings must be a number, got '{servings}'")
        if servings <= 0:
            raise StoreError("servings must be > 0")
        f = servings / float(r["servings"])
        scaled = {k: round(float(r[k]) * f, 1) for k in RECIPE_MACROS}
        total = {k: round(float(r[k]) * servings, 1) for k in RECIPE_MACROS}
        return {"name": r["name"], "base_servings": r["servings"],
                "target_servings": servings, "factor": round(f, 3),
                "per_serving": {k: float(r[k]) for k in RECIPE_MACROS},
                "scaled_per_serving": scaled, "total": total,
                "quantities": r["quantities"],
                "qty_note": "qty strings are free text (e.g. '2 spoons') — not scaled"}

    # ── cook log ──
    # log_cook: appends an observation row; recipe itself is never modified here.
    def log_cook(self, recipe: str, cooking_note: str = "", aftertaste_note: str = "",
                 date: str = "") -> dict:
        import datetime as dti

        r = self.get_recipe(recipe)
        if not r:
            raise StoreError(f"unknown recipe '{recipe}'")
        day = check_day(date) if (date or "").strip() else dti.date.today().isoformat()
        doc = {"recipe_id": _oid(r["_id"]), "recipe_name": r["name"], "date": day,
               "cooking_note": (cooking_note or "")[:500],
               "aftertaste_note": (aftertaste_note or "")[:500], "createdAt": utcnow()}
        doc["_id"] = self._log.insert_one(doc).inserted_id
        return _doc_out(doc)

    # list_cooks: newest-first history; limit clamped to 1..200.
    def list_cooks(self, recipe: str = "", limit: int = 50) -> list[dict]:
        filt: dict = {}
        if (recipe or "").strip():
            r = self.get_recipe(recipe.strip())
            if not r:
                raise StoreError(f"unknown recipe '{recipe}'")
            filt["recipe_id"] = _oid(r["_id"])
        rows = list(self._log.find(filt).sort("date", -1).limit(max(1, min(int(limit or 50), 200))))
        return [_doc_out(x) for x in rows]


# from_env: builds Store from MONGODB_URI/MONGODB_DB (single root .env).
def from_env() -> Store:
    return Store(os.environ.get("MONGODB_URI", ""),
                 os.environ.get("MONGODB_DB", "hermes"))
