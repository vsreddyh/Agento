"""Shared MongoDB helpers for Hermes MCPs (cookbook, health-check).

Env: MONGODB_URI (required), MONGODB_DB (default: hermes).
Client + db are cached per process.
"""
from __future__ import annotations

import os

# Process-cached client/db: one connection per MCP process, reused across tools.
_client = None
_db = None


# get_db: lazy singleton from MONGODB_URI/MONGODB_DB; raises when URI is missing.
def get_db():
    global _client, _db
    if _db is not None:
        return _db
    uri = os.environ.get("MONGODB_URI", "").strip()
    if not uri:
        raise ValueError("MONGODB_URI is not set.")
    from pymongo import MongoClient

    _client = MongoClient(uri, serverSelectionTimeoutMS=8000, retryWrites=True)
    db_name = os.environ.get("MONGODB_DB", "hermes").strip() or "hermes"
    _db = _client[db_name]
    return _db


# col: shortcut for get_db()[name] so stores avoid repeating the lookup.
def col(name: str):
    return get_db()[name]


# from_env_db_name: resolves the DB name with the same default/fallback as get_db.
def from_env_db_name() -> str:
    return os.environ.get("MONGODB_DB", "hermes").strip() or "hermes"
