#!/usr/bin/env python3
"""Remote-MongoDB helper for the Hermes bots (money, health-check, cookbook).

Usage:
  mongo.py get <collection> [filter-json]
  mongo.py count <collection> [filter-json]
  mongo.py insert <collection> <doc-json>
  mongo.py insert-many <collection> <docs-json-array>
  mongo.py upsert <collection> <filter-json> <update-json>
  mongo.py delete <collection> <filter-json>
  mongo.py drop <collection>
  mongo.py aggregate <collection> <pipeline-json>

Environment:
  MONGODB_URI   connection string (required, e.g. mongodb+srv://...)
  MONGODB_DB    database name (default: hermes)

Collection conventions (stable — retention.sh depends on them):
  money_transactions                 money bot
  hc_meals / hc_days / hc_weight     health-check bot (weight is NEVER pruned)
  cookbook_ingredients /
  cookbook_recipes / cookbook_cook_log
                                     cookbook bot (permanent, never pruned)

Date convention: store a `date` field as "YYYY-MM-DD" or a full ISO-8601
string. Both compare lexicographically, which is what retention.sh relies on.
"""

from __future__ import annotations

import argparse
import json
import os
import sys

try:
    from pymongo import MongoClient
except ImportError:
    print("pymongo is not installed — run `pip3 install pymongo` or hermes.sh init", file=sys.stderr)
    sys.exit(1)


# get_db: connects from MONGODB_URI/MONGODB_DB; exits 2 when URI is missing.
def get_db():
    uri = os.environ.get("MONGODB_URI", "").strip()
    if not uri:
        print("MONGODB_URI is not set", file=sys.stderr)
        sys.exit(2)
    db_name = os.environ.get("MONGODB_DB", "hermes").strip() or "hermes"
    client = MongoClient(uri, serverSelectionTimeoutMS=8000)
    return client[db_name]


# load_json: parses a CLI JSON arg; exits 3 on malformed input.
def load_json(raw: str) -> dict:
    try:
        return json.loads(raw)
    except json.JSONDecodeError as e:
        print(f"invalid JSON: {e}", file=sys.stderr)
        sys.exit(3)


# CLI dispatch: one collection + JSON args; ObjectIds stringified for shell-friendly output.
def main() -> int:
    p = argparse.ArgumentParser(prog="mongo.py")
    p.add_argument("command", choices=[
        "get", "count", "insert", "insert-many", "upsert", "delete", "drop", "aggregate",
    ])
    p.add_argument("collection")
    p.add_argument("args", nargs="*", help="JSON arguments (filter/doc/pipeline)")

    ns = p.parse_args()
    db = get_db()
    col = db[ns.collection]

    # Reads: get lists docs (capped by MONGO_LIMIT), count returns only the number.
    if ns.command in ("get", "count"):
        filt = load_json(ns.args[0]) if ns.args else {}
        if ns.command == "count":
            print(json.dumps({"count": col.count_documents(filt)}))
        else:
            limit = int(os.environ.get("MONGO_LIMIT", "1000"))
            docs = list(col.find(filt).limit(limit))
            for d in docs:
                d["_id"] = str(d["_id"])
            print(json.dumps(docs, default=str))

    # Writes: single/batch insert return the new id(s) for scripting.
    elif ns.command == "insert":
        if len(ns.args) < 1:
            p.error("insert requires a doc JSON")
        res = col.insert_one(load_json(ns.args[0]))
        print(json.dumps({"inserted_id": str(res.inserted_id)}))

    elif ns.command == "insert-many":
        if len(ns.args) < 1:
            p.error("insert-many requires a JSON array of docs")
        docs = load_json(ns.args[0])
        if not isinstance(docs, list):
            p.error("insert-many expects a JSON array")
        res = col.insert_many(docs)
        print(json.dumps({"inserted_ids": [str(i) for i in res.inserted_ids]}))

    # Upsert: $set-only update so callers never need full update operators.
    elif ns.command == "upsert":
        if len(ns.args) < 2:
            p.error("upsert requires <filter-json> <update-json>")
        filt, upd = load_json(ns.args[0]), load_json(ns.args[1])
        res = col.update_one(filt, {"$set": upd}, upsert=True)
        print(json.dumps({"matched": res.matched_count, "upserted": str(res.upserted_id)}))

    # Destructive ops: delete_many by filter, drop for whole collections.
    elif ns.command == "delete":
        if len(ns.args) < 1:
            p.error("delete requires a filter JSON")
        res = col.delete_many(load_json(ns.args[0]))
        print(json.dumps({"deleted": res.deleted_count}))

    elif ns.command == "drop":
        col.drop()
        print(json.dumps({"dropped": ns.collection}))

    # Aggregate: runs a raw pipeline JSON array for ad-hoc summaries/debugging.
    elif ns.command == "aggregate":
        if len(ns.args) < 1:
            p.error("aggregate requires a pipeline JSON array")
        pipe = load_json(ns.args[0])
        if not isinstance(pipe, list):
            p.error("aggregate expects a pipeline JSON array")
        docs = list(col.aggregate(pipe))
        for d in docs:
            if "_id" in d and not isinstance(d["_id"], (str, int, float)):
                d["_id"] = str(d["_id"])
        print(json.dumps(docs, default=str))

    return 0


if __name__ == "__main__":
    sys.exit(main())
