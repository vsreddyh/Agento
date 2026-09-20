"""MongoDB-only storage for the Miser MCP server.

Collections (see schema.py for validators/indexes/views):
  money_accounts     name (unique), type, balance, archived,
                     createdAt. NEVER expires.
  money_transactions date (YYYY-MM-DD), amount (>0), type
                     (income|expense|transfer), category, note, source,
                     accountId (required), sending_to (transfer only),
                     createdAt, expiresAt (TTL target, date + 90d).

INVARIANTS (load-bearing — do not bypass):
  1. Every mutation below runs in a multi-document transaction that writes
     the transaction doc(s) AND the corresponding $inc balance delta(s)
     atomically. Balances must survive TTL expiry of old docs, so they can
     only be maintained here — NEVER write these collections directly.
  2. sending_to is present iff type == transfer; from != to; both
     accounts exist and are unarchived. ($jsonSchema can't express this
     portably, so it is enforced here.)
"""
from __future__ import annotations

import datetime as dt
import os
import re

from bson import ObjectId

# Collection names + 90d retention window; DATE_RE enforces the YYYY-MM-DD contract.
ACCOUNTS = "money_accounts"
TRANSACTIONS = "money_transactions"
RETENTION_DAYS = 90
DATE_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")

ACCOUNT_TYPES = ["cash", "bank", "card", "wallet", "other"]
TX_TYPES = ["income", "expense", "transfer"]


# Domain error: caught by server.py and returned as {ok: False, error}.
class StoreError(ValueError):
    pass


# Time helpers: _expiry sets the TTL target (tx date + retention window).
def _utcnow() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc)


def _expiry(day: str, days: int = RETENTION_DAYS) -> dt.datetime:
    d = dt.date.fromisoformat(day)
    return dt.datetime(d.year, d.month, d.day,
                       tzinfo=dt.timezone.utc) + dt.timedelta(days=days)


# Delta map: income +amt, expense -amt, transfer -src/+dst (drives $inc updates).
def _signed_deltas(doc: dict) -> dict[str, float]:
    """Map account-id (str) -> balance delta implied by a transaction doc."""
    amt = float(doc["amount"])
    t = doc["type"]
    out: dict[str, float] = {}
    if t == "income":
        out[str(doc["accountId"])] = amt
    elif t == "expense":
        out[str(doc["accountId"])] = -amt
    elif t == "transfer":
        out[str(doc["accountId"])] = -amt
        out[str(doc["sending_to"])] = out.get(str(doc["sending_to"]), 0) + amt
    return out


# Store: MongoDB backend; all balance writes go through _transact for atomicity.
class Store:
    def __init__(self, uri: str, db_name: str = "hermes"):
        uri = (uri or "").strip()
        if not uri:
            raise ValueError("MONGODB_URI is not set — MongoDB is the only backend.")
        from pymongo import MongoClient
        self._client = MongoClient(uri, serverSelectionTimeoutMS=8000,
                                   retryWrites=True)
        db = self._client[db_name or "hermes"]
        self._accts = db[ACCOUNTS]
        self._txns = db[TRANSACTIONS]
        self._txn_supported: bool | None = None  # probed lazily via hello

    def _use_transactions(self) -> bool:
        """True when the deployment supports multi-doc transactions.

        Standalone mongod has no replica set, so transactions are rejected
        — fall back to plain writes. Only a positive result is cached: a
        negative one is re-probed on every call so a process that starts
        before the set is initiated upgrades to transactions automatically
        once it is up.
        """
        if self._txn_supported:
            return True
        try:
            hello = self._client.admin.command("hello")
        except Exception:  # noqa: BLE001 — server down; retry next call
            return False
        self._txn_supported = bool(hello.get("setName") or hello.get("msg") == "isdbgrid")
        if not self._txn_supported:
            print("[miser] MongoDB deployment has no replica set (yet) — "
                  "running balance writes without transactions "
                  "(single-doc atomicity only).")
        return self._txn_supported

    # ── transaction runner ────────────────────────────────
    # Retries the whole body on TransientTransactionError. On
    # UnknownTransactionCommitResult only the COMMIT is retried — the body
    # is never re-executed ($inc is not idempotent).
    # On deployments without transaction support (standalone mongod) the
    # body runs once with sess=None (pymongo accepts session=None).
    def _transact(self, fn, *args, **kwargs):
        from pymongo.errors import PyMongoError
        if not self._use_transactions():
            return fn(None, *args, **kwargs)
        attempts = 0
        while True:
            attempts += 1
            try:
                with self._client.start_session() as sess:
                    # with_transaction retries transient errors internally;
                    # the outer loop is a backstop.
                    return sess.with_transaction(lambda s: fn(s, *args, **kwargs))
            except PyMongoError as e:
                if e.has_error_label("UnknownTransactionCommitResult"):
                    raise  # commit ambiguous — do NOT re-run body ($inc not idempotent)
                if e.has_error_label("TransientTransactionError") and attempts < 5:
                    continue
                raise

    # ── accounts ──────────────────────────────────────────
    # create_account: unique name (index-backed); archived defaults to False.
    def create_account(self, name: str, type: str = "cash",
                       balance: float = 0) -> dict:
        from pymongo.errors import DuplicateKeyError
        name = (name or "").strip()
        if not name:
            raise StoreError("account name is required")
        if type not in ACCOUNT_TYPES:
            raise StoreError(f"account type must be one of {ACCOUNT_TYPES}")
        doc = {"name": name, "type": type,
               "balance": float(balance),
               "archived": False, "createdAt": _utcnow()}
        try:
            doc["_id"] = self._accts.insert_one(doc).inserted_id
        except DuplicateKeyError:
            raise StoreError(f"account '{name}' already exists")
        doc["_id"] = str(doc["_id"])
        return doc

    # list_accounts: alphabetical read; archived hidden unless requested.
    def list_accounts(self, include_archived: bool = False) -> list[dict]:
        filt = {} if include_archived else {"archived": False}
        rows = list(self._accts.find(filt, {"_id": 0}).sort("name", 1))
        return rows

    # archive_account: soft-delete; keeps history, blocks future writes via _resolve.
    def archive_account(self, name: str) -> bool:
        res = self._accts.update_one({"name": name}, {"$set": {"archived": True}})
        return res.matched_count > 0

    def _resolve(self, name_or_id: str, sess, *, for_write: bool = True) -> dict:
        """Resolve an account by name or _id string. Raises StoreError."""
        q: dict = {}
        try:
            q = {"_id": ObjectId(name_or_id)}
        except Exception:  # noqa: BLE001 — not an ObjectId, treat as name
            q = {"name": (name_or_id or "").strip()}
        acct = self._accts.find_one(q, session=sess)
        if not acct:
            raise StoreError(f"unknown account '{name_or_id}'")
        if for_write and acct.get("archived"):
            raise StoreError(f"account '{acct['name']}' is archived")
        return acct

    # get_balances: stored balances + total over active accounts only.
    def get_balances(self) -> dict:
        rows = list(self._accts.find({}, {"_id": 0}).sort("name", 1))
        total = sum(r.get("balance", 0) for r in rows if not r.get("archived"))
        return {"accounts": rows, "total": total}

    # ── transactions ──────────────────────────────────────
    # insert: validates date/amount/type, then atomically writes doc + $inc deltas.
    def insert(self, *, date: str, amount: float, type: str, category: str,
               account: str = "", sending_to: str = "",
               note: str = "", source: str = "mcp") -> str:
        from parse import CATEGORIES
        if not DATE_RE.match(date or ""):
            raise StoreError("date must be YYYY-MM-DD")
        amount = float(amount)
        if amount <= 0:
            raise StoreError("amount must be positive")
        if type not in TX_TYPES:
            raise StoreError(f"type must be one of {TX_TYPES}")
        category = (category or "other").lower().strip()
        if category not in CATEGORIES:
            category = "other"

        def _run(sess):
            # No default account: blank means the caller must choose.
            name = (account or "").strip()
            if not name:
                existing = [a["name"] for a in
                            self._accts.find({"archived": False}, {"name": 1},
                                             session=sess).sort("name", 1)]
                if not existing:
                    raise StoreError("no accounts found — please configure an "
                                     "account first (use create_account)")
                raise StoreError("account is required — available accounts: " +
                                 ", ".join(existing))
            src = self._resolve(name, sess)
            doc: dict = {"date": date, "amount": amount, "type": type,
                         "category": category, "note": (note or "")[:300],
                         "source": (source or "mcp")[:64],
                         "accountId": src["_id"],
                         "createdAt": _utcnow(), "expiresAt": _expiry(date)}
            if type == "transfer":
                if not sending_to:
                    raise StoreError("transfer requires sending_to")
                dst = self._resolve(sending_to, sess)
                if dst["_id"] == src["_id"]:
                    raise StoreError("transfer source and destination must differ")
                doc["sending_to"] = dst["_id"]
            elif sending_to:
                raise StoreError("sending_to is only valid for transfers")
            tid = self._txns.insert_one(doc, session=sess).inserted_id
            for aid, delta in _signed_deltas(doc).items():
                self._accts.update_one({"_id": ObjectId(aid)},
                                       {"$inc": {"balance": delta}}, session=sess)
            return str(tid)

        return self._transact(_run)

    def delete(self, filt: dict) -> int:
        """Delete by app-level filter; inverts balance deltas atomically."""
        if not filt:
            raise StoreError("refusing to delete without a filter")
        mongo_filt = self._to_mongo_filter(filt)

        def _run(sess):
            matched = list(self._txns.find(mongo_filt, session=sess))
            if not matched:
                return 0
            self._txns.delete_many({"_id": {"$in": [d["_id"] for d in matched]}},
                                   session=sess)
            agg: dict[str, float] = {}
            for d in matched:
                for aid, delta in _signed_deltas(d).items():
                    agg[aid] = agg.get(aid, 0) - delta  # inverse
            for aid, delta in agg.items():
                self._accts.update_one({"_id": ObjectId(aid)},
                                       {"$inc": {"balance": delta}}, session=sess)
            return len(matched)

        return self._transact(_run)

    # fix_last: adjusts newest tx by delta; scales each leg so transfers stay balanced.
    def fix_last(self, new_amount: float) -> bool:
        new_amount = float(new_amount)
        if new_amount <= 0:
            raise StoreError("amount must be positive")

        def _run(sess):
            last = self._txns.find_one(sort=[("_id", -1)], session=sess)
            if not last:
                return False
            delta = new_amount - float(last["amount"])
            if delta == 0:
                return True
            self._txns.update_one({"_id": last["_id"]},
                                  {"$set": {"amount": new_amount}}, session=sess)
            last["amount"] = new_amount
            # scale each side's delta proportionally via sign of original effect
            for aid, signed in _signed_deltas({**last, "amount": 1.0}).items():
                self._accts.update_one(
                    {"_id": ObjectId(aid)},
                    {"$inc": {"balance": signed * delta}}, session=sess)
            return True

        return self._transact(_run)

    # ── reads (no txn needed) ─────────────────────────────
    # query: date-range scan; account filter matches either side of transfers.
    def query(self, start: str, end: str, type: str | None = None,
              category: str | None = None, account: str | None = None) -> list[dict]:
        filt: dict = {"date": {"$gte": start, "$lte": end}}
        if type:
            filt["type"] = type
        if category:
            filt["category"] = category
        if account:
            acct = self._accts.find_one(
                {"$or": [{"name": account}, *self._oid_or_empty(account)]})
            if not acct:
                raise StoreError(f"unknown account '{account}'")
            filt["$or"] = [{"accountId": acct["_id"]},
                           {"sending_to": acct["_id"]}]
        rows = list(self._txns.find(filt, {"_id": 0}).sort("date", 1))
        for r in rows:
            r["accountId"] = str(r["accountId"])
            if "sending_to" in r:
                r["sending_to"] = str(r["sending_to"])
        return rows

    # summarize: income/expense/net + expense-only per-category ranking.
    def summarize(self, start: str, end: str,
                  account: str | None = None) -> dict:
        rows = self.query(start, end, account=account)
        income = sum(r["amount"] for r in rows if r["type"] == "income")
        expense = sum(r["amount"] for r in rows if r["type"] == "expense")
        by_cat: dict[str, float] = {}
        for r in rows:
            if r["type"] == "expense":
                by_cat[r["category"]] = by_cat.get(r["category"], 0) + r["amount"]
        top = sorted(by_cat.items(), key=lambda kv: kv[1], reverse=True)
        return {"income": income, "expense": expense, "net": income - expense,
                "count": len(rows), "by_category": top}

    # ── retention (TTL handles this natively; manual override) ──
    # Retention purges must NOT invert balances: TTL deletions bypass app
    # code entirely, so prune behaves identically (delete docs, keep the
    # absorbed balance). Only user-initiated delete() inverts.
    def prune(self, days: int = RETENTION_DAYS, dry_run: bool = False) -> int:
        cutoff = (dt.date.today() - dt.timedelta(days=days)).isoformat()
        filt = {"date": {"$lt": cutoff}}
        if dry_run:
            return self._txns.count_documents(filt)
        return self._txns.delete_many(filt).deleted_count

    # ── helpers ───────────────────────────────────────────
    # _oid_or_empty: lets name-or-id filters match _id without raising on plain names.
    @staticmethod
    def _oid_or_empty(s: str) -> list[dict]:
        try:
            return [{"_id": ObjectId(s)}]
        except Exception:  # noqa: BLE001
            return []

    def _to_mongo_filter(self, filt: dict) -> dict:
        """Translate app-level delete filters to Mongo filters.

        Supported keys: amount, category, date (+ $lt/$gt ops), account.
        """
        out: dict = {}
        for k, v in filt.items():
            if k == "account":
                acct = self._accts.find_one(
                    {"$or": [{"name": v}, *self._oid_or_empty(v)]})
                if not acct:
                    raise StoreError(f"unknown account '{v}'")
                out["$or"] = [{"accountId": acct["_id"]},
                              {"sending_to": acct["_id"]}]
            else:
                out[k] = v
        return out


# from_env: builds Store from MONGODB_URI/MONGODB_DB (single root .env).
def from_env() -> Store:
    return Store(uri=os.environ.get("MONGODB_URI", ""),
                 db_name=os.environ.get("MONGODB_DB", "hermes"))
