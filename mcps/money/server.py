#!/usr/bin/env python3
"""Miser — multi-account money-management MCP server.

Storage: MongoDB (`money_accounts` + `money_transactions` in the shared
`hermes` DB). Every balance-changing write runs in a multi-document
transaction (doc + $inc deltas atomically) — see store.py invariants.

Run (stdio, for MCP clients):
  pip install -r requirements.txt
  python schema.py --apply   # one-time DB setup
  python server.py
"""
from __future__ import annotations

import datetime as dt
import os

from dotenv import load_dotenv

from parse import CATEGORIES, classify, resolve_period
from store import StoreError, from_env

load_dotenv()

# MCP SDK compat: FastMCP (v1) was renamed MCPServer (v2) — accept either.
try:  # MCP SDK v1
    from mcp.server.fastmcp import FastMCP

    mcp = FastMCP("miser-money")
except ImportError:  # MCP SDK v2 — FastMCP renamed to MCPServer
    from mcp.server.mcpserver import MCPServer

    mcp = MCPServer("miser-money")

# Shared store handle (MongoDB); tools below are thin wrappers returning {ok, ...}.
store = from_env()


# Uniform error envelope so MCP clients always get {ok: False, error}.
def _err(e: Exception) -> dict:
    return {"ok": False, "error": str(e)}


# ── accounts ──────────────────────────────────────────────
# create_account: unique name enforced in store; type/balance validated there.
@mcp.tool()
def create_account(name: str, type: str = "cash",
                   balance: float = 0) -> dict:
    """Create a money account (e.g. Cash, HDFC Checking, HDFC Credit Card).

    Args:
        name: Unique account name.
        type: cash | bank | card | wallet | other.
        balance: Starting balance.
    """
    try:
        acct = store.create_account(name, type, balance)
        return {"ok": True, "account": acct}
    except (StoreError, Exception) as e:  # noqa: BLE001
        return _err(e)


# list_accounts: read-only; archived hidden unless explicitly requested.
@mcp.tool()
def list_accounts(include_archived: bool = False) -> dict:
    """List accounts with their current (stored) balances."""
    try:
        return {"ok": True, "accounts": store.list_accounts(include_archived)}
    except (StoreError, Exception) as e:  # noqa: BLE001
        return _err(e)


# archive_account: soft-delete; history stays queryable, new writes blocked.
@mcp.tool()
def archive_account(name: str) -> dict:
    """Soft-delete an account (history stays queryable; blocked from new writes)."""
    try:
        if store.archive_account(name):
            return {"ok": True, "archived": name}
        return {"ok": False, "error": f"unknown account '{name}'"}
    except (StoreError, Exception) as e:  # noqa: BLE001
        return _err(e)


# get_balances: stored balances (maintained transactionally) + active total.
@mcp.tool()
def get_balances() -> dict:
    """Current per-account balances plus total across active accounts."""
    try:
        out = store.get_balances()
        return {"ok": True, **out}
    except (StoreError, Exception) as e:  # noqa: BLE001
        return _err(e)


# ── transactions ──────────────────────────────────────────
# log_transaction: structured write; defaults date to today, note truncated to 300.
@mcp.tool()
def log_transaction(type: str, amount: float, category: str = "other",
                    account: str = "", sending_to: str = "",
                    note: str = "", date: str = "") -> dict:
    """Log an income, expense, or transfer (atomic with balance update).

    Args:
        type: 'income', 'expense', or 'transfer'.
        amount: Positive amount.
        category: One of groceries, eating out, transport, bills, rent,
            shopping, health, fun, salary, other.
        account: Source account name (required; no default — blank fails
            with "no accounts found" or "account is required").
        sending_to: Required for transfers (receiving account name).
        note: Free-text description.
        date: YYYY-MM-DD, defaults to today.
    """
    type = (type or "").lower().strip()
    try:
        day = (date or "").strip() or dt.date.today().isoformat()
        tid = store.insert(date=day, amount=float(amount), type=type,
                           category=category, account=account,
                           sending_to=sending_to,
                           note=note[:300], source="log_transaction")
        return {"ok": True, "id": tid, "date": day, "type": type,
                "amount": float(amount)}
    except (StoreError, Exception) as e:  # noqa: BLE001
        return _err(e)


# log_text: free-form entry point; delegates parsing to parse.classify.
@mcp.tool()
def log_text(text: str, account: str = "") -> dict:
    """Log from free-form text ('spent 300 on groceries', 'got 5000 salary',
    'transfer 2000 to Cash'). Optional account override; blank requires an
    existing account and fails with "no accounts found" when none exists.
    """
    r = classify(text)
    if r["action"] in ("log_income", "log_expense"):
        if r.get("amount") is None:
            return {"ok": False, "error": "No amount found — ask the user how much."}
        tx_type = "income" if r["action"] == "log_income" else "expense"
        try:
            tid = store.insert(date=dt.date.today().isoformat(), amount=r["amount"],
                               type=tx_type, category=r["category"], account=account,
                               note=text[:300], source="log_text")
            return {"ok": True, "id": tid, "type": tx_type,
                    "amount": r["amount"], "category": r["category"]}
        except (StoreError, Exception) as e:  # noqa: BLE001
            return _err(e)
    if r["action"] == "log_transfer":
        if r.get("amount") is None:
            return {"ok": False, "error": "No amount found — ask the user how much."}
        dest = r.get("sending_to") or ""
        if not dest:
            return {"ok": False, "error": "No destination account found — ask the user where to transfer to."}
        try:
            tid = store.insert(date=dt.date.today().isoformat(), amount=r["amount"],
                               type="transfer", category="other", account=account,
                               sending_to=dest, note=text[:300], source="log_text")
            return {"ok": True, "id": tid, "type": "transfer",
                    "amount": r["amount"], "sending_to": dest}
        except (StoreError, Exception) as e:  # noqa: BLE001
            return _err(e)
    return {"ok": False, "action": r["action"],
            "error": f"Not a loggable statement (classified as '{r['action']}')."}


# query_transactions: date-range read; empty filters mean "no filter".
@mcp.tool()
def query_transactions(start: str, end: str, type: str = "",
                       category: str = "", account: str = "") -> dict:
    """List transactions between start and end dates (YYYY-MM-DD inclusive).
    Optional filters: type, category, account (matches either side of transfers).
    """
    try:
        rows = store.query(start, end, type=type or None,
                           category=category or None, account=account or None)
        return {"ok": True, "count": len(rows), "transactions": rows}
    except (StoreError, Exception) as e:  # noqa: BLE001
        return _err(e)


# summarize: income/expense/net + per-category; period phrase or explicit range.
@mcp.tool()
def summarize(period: str = "this month", start: str = "",
              end: str = "", account: str = "") -> dict:
    """Summarize income, expenses, net total and per-category breakdown.
    Optional account filter. Period phrase or explicit start/end dates.
    """
    try:
        if start and end:
            label = f"{start} to {end}"
        else:
            start, end, label = resolve_period(period or "this month")
        s = store.summarize(start, end, account=account or None)
        return {"ok": True, "label": label, "start": start, "end": end,
                "income": s["income"], "expense": s["expense"], "net": s["net"],
                "count": s["count"],
                "by_category": [{"category": c, "total": t} for c, t in s["by_category"]]}
    except (StoreError, Exception) as e:  # noqa: BLE001
        return _err(e)


# fix_last_transaction: corrects most-recent amount; balances adjusted atomically.
@mcp.tool()
def fix_last_transaction(amount: float) -> dict:
    """Correct the most recent transaction's amount (balances adjusted atomically)."""
    try:
        if store.fix_last(float(amount)):
            return {"ok": True, "amount": float(amount)}
        return {"ok": False, "error": "No transactions to fix."}
    except (StoreError, Exception) as e:  # noqa: BLE001
        return _err(e)


# delete_transactions: filtered delete with balance inversion; requires ≥1 filter.
@mcp.tool()
def delete_transactions(amount: float = 0, category: str = "",
                        date: str = "", account: str = "") -> dict:
    """Delete matching transactions (balances inverted atomically).
    At least one filter required."""
    filt: dict = {}
    if amount:
        filt["amount"] = float(amount)
    if category:
        filt["category"] = category
    if date:
        filt["date"] = date
    if account:
        filt["account"] = account
    if not filt:
        return {"ok": False,
                "error": "Provide at least one of amount, category, date, account."}
    try:
        return {"ok": True, "deleted": store.delete(filt)}
    except (StoreError, Exception) as e:  # noqa: BLE001
        return _err(e)


# prune_old: manual 90d purge (TTL does this in background); dry_run only reports.
@mcp.tool()
def prune_old(days: int = 90, dry_run: bool = True) -> dict:
    """Immediate 90-day purge (TTL handles this natively in the background;
    this is the manual/immediate override; balances stay correct).
    dry_run=true (default) only reports what would be removed.
    """
    try:
        n = store.prune(days=days, dry_run=dry_run)
        return {"ok": True, "dry_run": dry_run,
                "would_remove": n if dry_run else 0,
                "removed": 0 if dry_run else n}
    except (StoreError, Exception) as e:  # noqa: BLE001
        return _err(e)


if __name__ == "__main__":
    mcp.run(transport="stdio")
