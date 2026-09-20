#!/usr/bin/env python3
"""One-time (idempotent) MongoDB schema setup for the Miser MCP server.

Creates/updates, in the configured database:
  - money_accounts     (strict validator, unique name index; no seed —
                        the user creates accounts via create_account)
  - money_transactions (strict validator, TTL + query indexes)
  - views: money_monthly_summary, money_category_breakdown, money_balances

Usage:
  python schema.py --apply      # create / update everything
  python schema.py --dry-run    # print what would be done, change nothing

Safe to re-run: validators are applied via collMod when the collection
already exists, createIndex is idempotent, views are dropped + recreated.
"""
from __future__ import annotations

import argparse
import datetime as dt
import os
import sys

from dotenv import load_dotenv

load_dotenv()

# Enum contracts shared with store.py; RETENTION_DAYS drives the TTL index.
ACCOUNT_TYPES = ["cash", "bank", "card", "wallet", "other"]
TX_TYPES = ["income", "expense", "transfer"]
CATEGORIES = ["groceries", "eating out", "transport", "bills", "rent",
              "shopping", "health", "fun", "salary", "other"]
RETENTION_DAYS = 90

# Strict validators: reject malformed docs at the DB layer (app validates first).
ACCOUNTS_VALIDATOR = {
    "$jsonSchema": {
        "bsonType": "object",
        "required": ["name", "type"],
        "additionalProperties": False,
        "properties": {
            "_id": {},
            "name": {"bsonType": "string", "minLength": 1, "maxLength": 100},
            "type": {"enum": ACCOUNT_TYPES},
            "balance": {"bsonType": "number"},
            "archived": {"bsonType": "bool"},
            "createdAt": {"bsonType": "date"},
        },
    }
}

TRANSACTIONS_VALIDATOR = {
    "$jsonSchema": {
        "bsonType": "object",
        "required": ["date", "amount", "type", "category", "accountId"],
        "additionalProperties": False,
        "properties": {
            "_id": {},
            "date": {"bsonType": "string",
                     "pattern": r"^\d{4}-\d{2}-\d{2}$",
                     "description": "YYYY-MM-DD, lexicographic compare"},
            "amount": {"bsonType": "number", "minimum": 0, "exclusiveMinimum": True},
            "type": {"enum": TX_TYPES},
            "category": {"enum": CATEGORIES},
            "note": {"bsonType": "string", "maxLength": 300},
            "source": {"bsonType": "string", "maxLength": 64},
            "accountId": {"bsonType": "objectId"},
            "sending_to": {"bsonType": "objectId"},
            "createdAt": {"bsonType": "date"},
            "expiresAt": {"bsonType": "date"},
        },
    }
}

# Conditional rules ($jsonSchema if/then is version-fragile) are enforced
# in store.py instead: sending_to present iff type == transfer,
# accountId != sending_to, both accounts exist and are unarchived.

# Read-only aggregation views: monthly totals, expense breakdown, live balances.
VIEWS = {
    "money_monthly_summary": [
        {"$group": {
            "_id": {"month": {"$substr": ["$date", 0, 7]},
                    "accountId": "$accountId", "type": "$type"},
            "total": {"$sum": "$amount"}, "count": {"$sum": 1}}},
        {"$project": {"_id": 0, "month": "$_id.month", "accountId": "$_id.accountId",
                      "type": "$_id.type", "total": 1, "count": 1}},
        {"$sort": {"month": -1, "total": -1}},
    ],
    "money_category_breakdown": [
        {"$match": {"type": "expense"}},
        {"$group": {
            "_id": {"month": {"$substr": ["$date", 0, 7]},
                    "accountId": "$accountId", "category": "$category"},
            "total": {"$sum": "$amount"}, "count": {"$sum": 1}}},
        {"$project": {"_id": 0, "month": "$_id.month", "accountId": "$_id.accountId",
                      "category": "$_id.category", "total": 1, "count": 1}},
        {"$sort": {"month": -1, "total": -1}},
    ],
    "money_balances": [
        {"$project": {"_id": 0, "name": "$name", "type": "$type",
                      "balance": "$balance", "archived": "$archived"}},
        {"$sort": {"name": 1}},
    ],
}


# DB handle from the single root .env; exits early if MONGODB_URI is missing.
def get_db():
    uri = os.environ.get("MONGODB_URI", "").strip()
    if not uri:
        sys.exit("MONGODB_URI is not set — copy .env.example to .env first.")
    from pymongo import MongoClient
    return MongoClient(uri, serverSelectionTimeoutMS=8000)[
        os.environ.get("MONGODB_DB", "hermes").strip() or "hermes"]


# Idempotent collection setup: collMod existing, create + strict validator if new.
def ensure_collection(db, name, validator, dry_run, log):
    from pymongo.errors import CollectionInvalid
    if name in db.list_collection_names():
        log(f"collMod {name} (validator update)")
        if not dry_run:
            db.command({"collMod": name, "validator": validator,
                        "validationLevel": "strict", "validationAction": "error"})
    else:
        log(f"create {name} (strict validator)")
        if not dry_run:
            try:
                db.create_collection(name, validator=validator,
                                     validationLevel="strict", validationAction="error")
            except CollectionInvalid:
                pass  # raced — already exists


# Indexes: unique account name, TTL on expiresAt, query paths for reads/deletes.
def ensure_indexes(db, dry_run, log):
    specs = [
        ("money_accounts", [("name", 1)], {"unique": True, "name": "uniq_name"}),
        ("money_transactions", [("expiresAt", 1)],
         {"expireAfterSeconds": 0, "name": "ttl_expiresAt"}),
        ("money_transactions", [("accountId", 1), ("date", 1)], {"name": "acct_date"}),
        ("money_transactions", [("sending_to", 1), ("date", 1)],
         {"name": "sendingto_date", "sparse": True}),
        ("money_transactions", [("date", 1), ("type", 1)], {"name": "date_type"}),
        ("money_transactions", [("category", 1), ("date", 1)], {"name": "cat_date"}),
    ]
    from pymongo import ASCENDING
    for col, keys, kwargs in specs:
        keys = [(k, ASCENDING) for k, _ in keys]
        log(f"createIndex {col} {kwargs['name']}")
        if not dry_run:
            db[col].create_index(keys, **kwargs)


# Views are drop + recreate (no ALTER); safe to re-run.
def ensure_views(db, dry_run, log):
    sources = {"money_monthly_summary": "money_transactions",
               "money_category_breakdown": "money_transactions",
               "money_balances": "money_accounts"}
    for view, pipeline in VIEWS.items():
        log(f"recreate view {view} on {sources[view]}")
        if not dry_run:
            if view in db.list_collection_names():
                db[view].drop()
            db.create_collection(view, viewOn=sources[view], pipeline=pipeline)


# No seed account: inserts without an account fail with
# "no accounts found — please configure an account first" until the user
# creates one via create_account.


def migrate_renames(db, dry_run, log):
    """One-time migration: destAccountId -> sending_to, drop openingBalance."""
    n = 0 if dry_run else db["money_transactions"].update_many(
        {"destAccountId": {"$exists": True}},
        [{"$set": {"sending_to": "$destAccountId"}},
         {"$unset": "destAccountId"}]).modified_count
    log(f"migrate: renamed destAccountId -> sending_to on {n} transaction(s)")
    n = 0 if dry_run else db["money_accounts"].update_many(
        {"openingBalance": {"$exists": True}},
        [{"$unset": "openingBalance"}]).modified_count
    log(f"migrate: dropped openingBalance on {n} account(s)")
    for col, old in (("money_transactions", "destacct_date"),):
        idx = [i["name"] for i in db[col].list_indexes()]
        if old in idx:
            log(f"migrate: drop old index {col}.{old}")
            if not dry_run:
                db[col].drop_index(old)


# CLI entry: --apply mutates, --dry-run only logs; order matters (migrate before indexes).
def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    g = ap.add_mutually_exclusive_group(required=True)
    g.add_argument("--apply", action="store_true")
    g.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    db = get_db()
    log = print
    print(f"database: {db.name} (dry_run={args.dry_run})")
    ensure_collection(db, "money_accounts", ACCOUNTS_VALIDATOR, args.dry_run, log)
    ensure_collection(db, "money_transactions", TRANSACTIONS_VALIDATOR, args.dry_run, log)
    migrate_renames(db, args.dry_run, log)
    ensure_indexes(db, args.dry_run, log)
    ensure_views(db, args.dry_run, log)
    print("done.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
