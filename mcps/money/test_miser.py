"""Tests for the Miser MCP server (parse + store).

Store tests run against live Atlas in a throwaway `miser_test` database
(dropped after each test). Needs MONGODB_URI in env / .env.
"""
import concurrent.futures
import datetime as dt
import os

import pytest
from dotenv import load_dotenv

load_dotenv()

from parse import classify, normalize_category, resolve_period
from store import Store, StoreError

TEST_DB = "miser_test"


@pytest.fixture()
def st():
    uri = os.environ.get("MONGODB_URI", "").strip()
    if not uri:
        pytest.skip("MONGODB_URI not set")
    import schema
    from pymongo import MongoClient
    client = MongoClient(uri, serverSelectionTimeoutMS=8000)
    db = client[TEST_DB]
    client.drop_database(TEST_DB)
    schema.ensure_collection(db, "money_accounts", schema.ACCOUNTS_VALIDATOR,
                             False, lambda *a: None)
    schema.ensure_collection(db, "money_transactions", schema.TRANSACTIONS_VALIDATOR,
                             False, lambda *a: None)
    schema.ensure_indexes(db, False, lambda *a: None)
    s = Store(uri=uri, db_name=TEST_DB)
    s.create_account("Cash", "cash", 0)
    yield s
    client.drop_database(TEST_DB)
    client.close()


def balances(s):
    return {a["name"]: a["balance"] for a in s.get_balances()["accounts"]}


# ── parse (no DB) ─────────────────────────────────────────
def test_classify_log():
    r = classify("spent 300 on groceries")
    assert r["action"] == "log_expense" and r["amount"] == 300
    assert r["category"] == "groceries", r
    r = classify("got 5000 salary")
    assert r["action"] == "log_income" and r["amount"] == 5000, r


def test_classify_query():
    r = classify("what did I spend this month?")
    assert r["action"] == "query" and r["type"] == "expense", r
    r = classify("how much on eating out in June?")
    assert r["action"] == "query", r
    assert r["start"].endswith("-06-01"), r


def test_classify_edit():
    assert classify("remove the 300 groceries entry")["action"] == "delete"
    r = classify("fix that to 350")
    assert r["action"] == "fix" and r["amount"] == 350, r


def test_periods():
    today = dt.date(2026, 9, 5)
    s, e, _ = resolve_period("this month", today)
    assert (s, e) == ("2026-09-01", "2026-09-30"), (s, e)
    s, e, _ = resolve_period("last month", today)
    assert (s, e) == ("2026-08-01", "2026-08-31"), (s, e)


def test_categories():
    assert normalize_category("Uber ride") == "transport"
    assert normalize_category("Netflix") == "fun"
    assert normalize_category("blah blah") == "other"


# ── accounts ──────────────────────────────────────────────
def test_accounts_crud(st):
    st.create_account("HDFC", "bank", 1000)
    names = [a["name"] for a in st.list_accounts()]
    assert {"Cash", "HDFC"} <= set(names)
    with pytest.raises(StoreError):
        st.create_account("HDFC")  # duplicate
    with pytest.raises(StoreError):
        st.create_account("X", "spaceship")  # bad type
    assert st.archive_account("HDFC") is True
    assert "HDFC" not in [a["name"] for a in st.list_accounts()]
    assert "HDFC" in [a["name"] for a in st.list_accounts(include_archived=True)]


# ── mutations move balances atomically ────────────────────
def test_insert_balances(st):
    st.create_account("HDFC", "bank", 1000)
    st.insert(date="2026-09-01", amount=300, type="expense",
              category="groceries", account="HDFC")
    st.insert(date="2026-09-02", amount=5000, type="income",
              category="salary", account="HDFC")
    b = balances(st)
    assert b["HDFC"] == 1000 - 300 + 5000, b
    assert b["Cash"] == 0


def test_transfer_balances(st):
    st.create_account("HDFC", "bank", 1000)
    st.insert(date="2026-09-01", amount=200, type="transfer",
              category="other", account="HDFC", sending_to="Cash")
    b = balances(st)
    assert b["HDFC"] == 800 and b["Cash"] == 200, b
    s = st.summarize("2026-09-01", "2026-09-30")
    assert s["income"] == 0 and s["expense"] == 0 and s["count"] == 1


def test_transfer_validation(st):
    st.create_account("HDFC", "bank", 1000)
    with pytest.raises(StoreError):
        st.insert(date="2026-09-01", amount=100, type="transfer",
                  category="other", account="HDFC", sending_to="HDFC")
    with pytest.raises(StoreError):
        st.insert(date="2026-09-01", amount=100, type="transfer",
                  category="other", account="HDFC")  # no sending_to
    with pytest.raises(StoreError):
        st.insert(date="2026-09-01", amount=100, type="transfer",
                  category="other", account="HDFC", sending_to="Nope")
    with pytest.raises(StoreError):
        st.insert(date="2026-09-01", amount=100, type="expense",
                  category="other", account="HDFC", sending_to="Cash")
    st.archive_account("Cash")
    with pytest.raises(StoreError):
        st.insert(date="2026-09-01", amount=100, type="transfer",
                  category="other", account="HDFC", sending_to="Cash")
    # nothing was written by any failed attempt
    assert st.query("2026-01-01", "2026-12-31") == []
    assert balances(st)["HDFC"] == 1000


def test_blank_account_requires_setup(st):
    # No accounts at all → configure-first error.
    st._accts.delete_many({})
    with pytest.raises(StoreError, match="no accounts found"):
        st.insert(date="2026-09-01", amount=100, type="expense",
                  category="other", account="")
    # Accounts exist but none named → must pick one explicitly.
    st.create_account("Cash", "cash", 0)
    with pytest.raises(StoreError, match="account is required"):
        st.insert(date="2026-09-01", amount=100, type="expense",
                  category="other", account="")
    # nothing was written by either failed attempt
    assert st.query("2026-01-01", "2026-12-31") == []


def test_delete_inverts_balances(st):
    st.create_account("HDFC", "bank", 1000)
    st.insert(date="2026-09-01", amount=200, type="transfer",
              category="other", account="HDFC", sending_to="Cash")
    assert st.delete({"category": "other"}) == 1
    b = balances(st)
    assert b["HDFC"] == 1000 and b["Cash"] == 0, b


def test_fix_last_adjusts_balances(st):
    st.insert(date="2026-09-01", amount=300, type="expense",
              category="groceries", account="Cash")
    assert st.fix_last(350) is True
    assert balances(st)["Cash"] == -350
    assert st.summarize("2026-09-01", "2026-09-30")["expense"] == 350


def test_concurrent_inserts(st):
    st.create_account("HDFC", "bank", 0)
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as ex:
        futs = [ex.submit(st.insert, date="2026-09-01", amount=10,
                          type="income", category="salary", account="HDFC")
                for _ in range(20)]
        for f in futs:
            f.result()
    assert balances(st)["HDFC"] == 200


def test_reconciliation_after_prune(st):
    """Balances stay correct even as old docs expire (stored, not recomputed)."""
    st.create_account("HDFC", "bank", 1000)
    st.insert(date="2020-01-01", amount=100, type="expense",
              category="other", account="HDFC")
    assert balances(st)["HDFC"] == 900
    assert st.prune(days=90) == 1
    assert balances(st)["HDFC"] == 900  # balance survived expiry
    assert st.query("2020-01-01", "2020-12-31") == []


def test_validator_rejects_bad_docs(st):
    from pymongo.errors import WriteError
    with pytest.raises(WriteError):
        st._txns.insert_one({"date": "yesterday", "amount": -5, "type": "lottery",
                             "category": "yachts", "accountId": "nope",
                             "junk": True})
