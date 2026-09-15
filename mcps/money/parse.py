"""Free-form natural-language parser for the money bot.

Ported from the Hermes `money` profile contract
(profiles/master/profiles/money/SOUL.md, retired with the Discord cutover):

    spent 300 on groceries
    got 5000 salary
    what did I spend this month?
    how much on eating out in June?
    biggest expense categories this month?
    remove the 300 groceries entry
    fix that to 350

No LLM needed — regex + keyword rules, deterministic.
"""
from __future__ import annotations

import calendar
import datetime as dt
import re

CATEGORIES = [
    "groceries", "eating out", "transport", "bills", "rent",
    "shopping", "health", "fun", "salary", "other",
]

_CATEGORY_HINTS = {
    "groceries": ["grocer", "vegetable", "veggie", "fruit", "milk", "kirana", "supermarket"],
    "eating out": ["restaurant", "food", "lunch", "dinner", "breakfast", "cafe", "coffee",
                   "pizza", "burger", "swiggy", "zomato", "eating out", "dining"],
    "transport": ["uber", "ola", "taxi", "cab", "bus", "train", "metro", "fuel", "petrol",
                  "diesel", "parking", "flight", "transport", "travel"],
    "bills": ["electric", "water", "internet", "wifi", "phone", "mobile", "recharge",
              "gas bill", "utility", "bill"],
    "rent": ["rent", "lease", "landlord"],
    "shopping": ["amazon", "flipkart", "cloth", "shirt", "shoe", "dress", "shopping", "myntra"],
    "health": ["doctor", "hospital", "medic", "pharma", "gym", "health"],
    "fun": ["movie", "game", "party", "concert", "netflix", "fun", "entertainment"],
    "salary": ["salary", "paycheck", "wage", "income", "credited", "salary credit"],
}

_AMOUNT_RE = re.compile(r"(?:₹|rs\.?\s?|\$)?\s?(\d+(?:,\d+)*(?:\.\d{1,2})?)", re.I)

INCOME_WORDS = ["got", "received", "income", "salary", "credited", "earned", "pay", "deposit"]
EXPENSE_WORDS = ["spent", "paid", "bought", "purchase", "expense", "cost", "spend"]

MONTHS = {m.lower(): i for i, m in enumerate(calendar.month_name) if m}


def normalize_category(text: str) -> str:
    t = text.lower()
    for cat in CATEGORIES:
        if cat in t:
            return cat
    for cat, hints in _CATEGORY_HINTS.items():
        if any(h in t for h in hints):
            return cat
    return "other"


def extract_amount(text: str) -> float | None:
    m = _AMOUNT_RE.search(text.replace(",", ""))
    if not m:
        return None
    try:
        return float(m.group(1))
    except ValueError:
        return None


def _month_range(year: int, month: int) -> tuple[str, str]:
    first = dt.date(year, month, 1)
    last = dt.date(year, month, calendar.monthrange(year, month)[1])
    return first.isoformat(), last.isoformat()


def resolve_period(text: str, today: dt.date | None = None) -> tuple[str, str, str]:
    """Return (start_YYYY-MM-DD, end_YYYY-MM-DD, label)."""
    today = today or dt.date.today()
    t = text.lower()
    if "today" in t:
        s = today.isoformat()
        return s, s, "today"
    if "yesterday" in t:
        y = (today - dt.timedelta(days=1)).isoformat()
        return y, y, "yesterday"
    if "this week" in t or "thisweek" in t:
        start = today - dt.timedelta(days=today.weekday())
        return start.isoformat(), today.isoformat(), "this week"
    if "last month" in t:
        first_this = today.replace(day=1)
        last_prev = first_this - dt.timedelta(days=1)
        s, e = _month_range(last_prev.year, last_prev.month)
        return s, e, last_prev.strftime("%B %Y")
    m = re.search(r"(january|february|march|april|may|june|july|august|september|october|november|december)", t)
    if m:
        month = MONTHS[m.group(1)]
        year = today.year
        ym = re.search(r"(19|20)\d{2}", t)
        if ym:
            year = int(ym.group(0))
        elif month > today.month:
            year -= 1  # "June" in January means last June
        s, e = _month_range(year, month)
        return s, e, f"{calendar.month_name[month]} {year}"
    if "this month" in t or "thismonth" in t or "so far" in t or "month" in t:
        s, e = _month_range(today.year, today.month)
        return s, e, today.strftime("%B %Y")
    # default: current month
    s, e = _month_range(today.year, today.month)
    return s, e, today.strftime("%B %Y")


def classify(text: str) -> dict:
    """Classify free-form text into an action dict.

    Actions: log_income | log_expense | log_transfer | query | delete | fix | help | unknown
    """
    t = text.strip()
    low = t.lower()

    if low in ("help", "!help", "commands"):
        return {"action": "help"}

    # explicit ! commands take precedence
    if low.startswith("!"):
        return classify(t[1:])

    # delete / remove
    if re.search(r"\b(remove|delete|undo)\b", low):
        return {"action": "delete", "amount": extract_amount(t),
                "category": normalize_category(t), "raw": t}
    # fix / correct last entry
    if re.search(r"\b(fix|correct|change|update)\b.*\b(to|as)\b", low) or low.startswith("fix "):
        return {"action": "fix", "amount": extract_amount(t), "raw": t}

    # questions
    if (low.startswith(("what", "how", "total", "show", "report", "summary", "breakdown", "biggest", "top"))
            or "?" in t or ("spend" in low and ("month" in low or "total" in low or "much" in low))):
        start, end, label = resolve_period(t)
        tx_type = "income" if "income" in low or "earn" in low else None
        if "income" not in low and ("spend" in low or "spent" in low or "expense" in low):
            tx_type = "expense"
        return {"action": "query", "start": start, "end": end, "label": label,
                "type": tx_type, "category": normalize_category(t) if " on " in low or " for " in low else None,
                "raw": t}

    amount = extract_amount(t)
    # transfers ("transfer 2000 to Cash", "move 500 to savings")
    if re.search(r"\b(transfer|move|send)\b", low) and amount is not None:
        dest = None
        m = re.search(r"\bto\s+([A-Za-z][\w ]*)", t)
        if m:
            dest = m.group(1).strip().rstrip(" .!")
        return {"action": "log_transfer", "amount": amount,
                "sending_to": dest, "raw": t}
    if amount is not None:
        is_income = any(w in low for w in INCOME_WORDS)
        is_expense = any(w in low for w in EXPENSE_WORDS)
        tx_type = "income" if is_income and not is_expense else "expense"
        # "monthly salary credited" with no digits -> amount unknown
        return {"action": f"log_{tx_type}", "amount": amount,
                "category": normalize_category(t), "raw": t}

    # income phrase without amount (e.g. "monthly salary credited")
    if any(w in low for w in ["salary credited", "salary credit", "income"]):
        return {"action": "log_income", "amount": None,
                "category": normalize_category(t), "raw": t}

    return {"action": "unknown", "raw": t}
