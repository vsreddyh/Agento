#!/usr/bin/env python3
"""Data retention for the Hermes bots.

Runs inside the `retention` service of the live container stack (see
docker/docker-compose.yml) and natively via `scripts/retention.sh` (which wraps
`podman-compose run --rm retention`). Policy summary:

  story       git repo (workspace/portals)    — no-op
  resumes     git repo (workspace/resumes)    — no-op
  money       transactions autowiped when the oldest entry is >90 days old
  health-check hc_meals + hc_days pruned after 30 days; hc_weight is NEVER touched
  cookbook    cookbook_ingredients/recipes/cook_log are permanent — no-op

Only the remote MongoDB is touched. Connection comes from MONGODB_URI /
MONGODB_DB (injected via env by the compose file / root .env).
"""

import argparse
import datetime
import os

import pymongo


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="report what would be removed without deleting anything",
    )
    args = parser.parse_args()

    uri = os.environ.get("MONGODB_URI", "").strip()
    if not uri:
        print("[retention] MONGODB_URI not set — skipping.")
        return 0

    try:
        client = pymongo.MongoClient(uri, serverSelectionTimeoutMS=8000)
    except Exception as exc:  # noqa: BLE001
        print(f"[retention] failed to connect to MongoDB: {exc}")
        return 1

    db = client[os.environ.get("MONGODB_DB", "hermes").strip() or "hermes"]
    today = datetime.date.today()
    prefix = "[retention] DRY-RUN: " if args.dry_run else "[retention] "

    # ── money: wipe transactions older than 90 days ──────────────
    money_cutoff = (today - datetime.timedelta(days=90)).isoformat()
    money_count = db["money_transactions"].count_documents({"date": {"$lt": money_cutoff}})
    if not args.dry_run:
        db["money_transactions"].delete_many({"date": {"$lt": money_cutoff}})
    print(f"{prefix}money: would remove {money_count} transactions older than {money_cutoff}")

    # ── health-check: prune hc_meals + hc_days older than 30 days ───
    # hc_weight is intentionally NOT in this list — kept permanently.
    # cookbook_* collections are permanent — never touched.
    food_cutoff = (today - datetime.timedelta(days=30)).isoformat()
    for col in ("hc_meals", "hc_days"):
        count = db[col].count_documents({"date": {"$lt": food_cutoff}})
        if not args.dry_run:
            db[col].delete_many({"date": {"$lt": food_cutoff}})
        print(f"{prefix}health-check: would remove {count} from {col} older than {food_cutoff}")

    # ── story / resumes / cookbook: no-op ───────────────────────
    print("[retention] story/resumes/cookbook: no retention policy (git repos / permanent).")
    client.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
