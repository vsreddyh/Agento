#!/usr/bin/env python3
"""Cookbook — reusable recipe library MCP server.

Storage: MongoDB (cookbook_ingredients, cookbook_recipes, cookbook_cook_log).
Permanent — never pruned. Run (stdio): pip install -r requirements.txt; python server.py
"""
from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from dotenv import load_dotenv

load_dotenv()

# MCP SDK compat: FastMCP (v1) was renamed MCPServer (v2) — accept either.
try:
    from mcp.server.fastmcp import FastMCP

    mcp = FastMCP("cookbook")
except ImportError:
    from mcp.server.mcpserver import MCPServer

    mcp = MCPServer("cookbook")

from cookbook.store import StoreError, from_env

# Shared store handle (MongoDB); tools below are thin wrappers returning {ok, ...}.
store = from_env()


# Uniform error envelope so MCP clients always get {ok: False, error}.
def _err(e: Exception) -> dict:
    return {"ok": False, "error": str(e)}


# add_ingredient: unique name enforced in store; note truncated to 300.
@mcp.tool()
def add_ingredient(name: str, note: str = "") -> dict:
    """Add an ingredient name (optional note). Names are unique."""
    try:
        return {"ok": True, "ingredient": store.add_ingredient(name, note)}
    except (StoreError, Exception) as e:
        return _err(e)


# list_ingredients: alphabetical read; empty search lists all (capped at 500).
@mcp.tool()
def list_ingredients(search: str = "") -> dict:
    """List ingredient names, optionally filtered by substring."""
    try:
        rows = store.list_ingredients(search)
        return {"ok": True, "count": len(rows), "ingredients": rows}
    except (StoreError, Exception) as e:
        return _err(e)


# delete_ingredient: refused when any recipe still references it (checked in store).
@mcp.tool()
def delete_ingredient(name_or_id: str) -> dict:
    """Delete an ingredient. Refused if any recipe still references it."""
    try:
        return {"ok": True, "deleted": store.delete_ingredient(name_or_id)}
    except (StoreError, Exception) as e:
        return _err(e)


# add_recipe: ingredient keys are name-or-id; per_serving requires all 5 macros.
@mcp.tool()
def add_recipe(name: str, ingredient_qtys: dict, per_serving: dict,
               servings: float = 1, note: str = "", tags: list = []) -> dict:
    """Save a recipe. ingredient_qtys maps name-or-id -> free qty string
    (e.g. {"salt": "2 spoons"}). per_serving needs kcal/protein_g/carbs_g/fat_g/fiber_g."""
    try:
        return {"ok": True, "recipe": store.add_recipe(
            name, ingredient_qtys, per_serving, servings, note, tags)}
    except (StoreError, Exception) as e:
        return _err(e)


# get_recipe: single lookup by dish name or id; None becomes unknown-recipe error.
@mcp.tool()
def get_recipe(name_or_id: str) -> dict:
    """Get one recipe by dish name or id."""
    try:
        r = store.get_recipe(name_or_id)
        if not r:
            return {"ok": False, "error": f"unknown recipe '{name_or_id}'"}
        return {"ok": True, "recipe": r}
    except (StoreError, Exception) as e:
        return _err(e)


# list_recipes: optional name/tag/ingredient filters (empty means no filter).
@mcp.tool()
def list_recipes(search: str = "", tag: str = "", ingredient: str = "") -> dict:
    """List recipes, optionally filtered by name substring, tag, or ingredient name."""
    try:
        rows = store.list_recipes(search, tag, ingredient)
        return {"ok": True, "count": len(rows), "recipes": rows}
    except (StoreError, Exception) as e:
        return _err(e)


# update_recipe: patch-only path; empty/zero args leave fields unchanged.
@mcp.tool()
def update_recipe(name_or_id: str, name: str = "", servings: float = 0,
                  per_serving: dict = {}, ingredient_qtys: dict = {},
                  note: str = "", tags: list = []) -> dict:
    """Patch a recipe (the ONLY way a recipe changes after a cook — call only
    when the user approves). Empty/zero args are left unchanged."""
    try:
        patch: dict = {}
        if name:
            patch["name"] = name
        if servings:
            patch["servings"] = servings
        if per_serving:
            patch["per_serving"] = per_serving
        if ingredient_qtys:
            patch["ingredient_qtys"] = ingredient_qtys
        if note:
            patch["note"] = note
        if tags:
            patch["tags"] = tags
        r = store.update_recipe(name_or_id, **patch)
        if not r:
            return {"ok": False, "error": f"unknown recipe '{name_or_id}'"}
        return {"ok": True, "recipe": r}
    except (StoreError, Exception) as e:
        return _err(e)


# delete_recipe: removes recipe plus its cook-log rows (counts returned).
@mcp.tool()
def delete_recipe(name_or_id: str) -> dict:
    """Delete a recipe plus its cook-log rows."""
    try:
        return {"ok": True, **store.delete_recipe(name_or_id)}
    except (StoreError, Exception) as e:
        return _err(e)


# scale_recipe: pure math read; qty strings are free text and returned as-is.
@mcp.tool()
def scale_recipe(name_or_id: str, servings: float) -> dict:
    """Scale macros to a target serving count. Pure math — no write.
    Qty strings are returned as-is (free text can't scale)."""
    try:
        return {"ok": True, **store.scale_recipe(name_or_id, servings)}
    except (StoreError, Exception) as e:
        return _err(e)


# log_cook: appends a cook-log row; never mutates the recipe itself.
@mcp.tool()
def log_cook(recipe: str, cooking_note: str = "", aftertaste_note: str = "",
             date: str = "") -> dict:
    """Log a cook: what differed (cooking_note) + what to improve (aftertaste_note).
    Never modifies the recipe — call update_recipe separately on approval."""
    try:
        return {"ok": True, "cook": store.log_cook(recipe, cooking_note, aftertaste_note, date)}
    except (StoreError, Exception) as e:
        return _err(e)


# list_cooks: newest-first cook history, optionally scoped to one recipe.
@mcp.tool()
def list_cooks(recipe: str = "", limit: int = 50) -> dict:
    """List cook-log rows, optionally for one recipe (newest first)."""
    try:
        rows = store.list_cooks(recipe, limit)
        return {"ok": True, "count": len(rows), "cooks": rows}
    except (StoreError, Exception) as e:
        return _err(e)


if __name__ == "__main__":
    mcp.run(transport="stdio")
