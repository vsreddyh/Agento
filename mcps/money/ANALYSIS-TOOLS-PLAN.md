# Miser-money analysis tools — tool plan (Gherkin)

Status: implemented on branch `feat/miser-money-analysis-tools` (this file is
the as-built spec; scenarios marked below are covered by handler tests,
pure-function unit tests, and DB integration tests).
Scope: twelve new tools — nine read-only (seven analysis plus
`budget_status` and `list_budgets`; four of the seven mode-dispatched) and
three budget writes on one new collection. The existing nine tools are
unchanged.

## The design rule

Two extremes are rejected:

- **many tiny tools** — every question needs its own entry point, and a
  compound answer costs several calls the agent has to sequence;
- **one large tool returning everything** — the caller pays for sections it
  did not ask about.

The middle ground: a tool may offer modes, but **the mode is a required
argument, and a call computes and returns exactly one mode's single answer**.
No mode ever returns another mode's table. The caller declares what it cares
about in one argument, makes one call, and gets one table (or one flat scalar
block) with no dead weight.

Additional invariants, carried over from the earlier review:

- At most **one array** per response. Everything else is a flat top-level
  scalar. No nested `window{}`, `totals{}`, `rows{}`, `method{}`,
  `baseline{}`; no `months[]` or `occurrences[]` inside a row.
- Applied values are echoed flat (`side`, `by`, `bucket`,
  `sensitivity_applied`, `bucket_ratio_applied`, `sort_by`, `order`,
  `categories_applied`, `accounts_applied`, ...), never as a bundle.
- The discriminator is validated: an unknown `by`/`scope` is rejected, and a
  mode-specific argument missing in the mode that needs it is rejected by
  name (`side` is required when `by=category`).
- Filters narrow, then footers follow: whenever a table carries a footer,
  the footer is computed over the **returned (filtered, sorted) rows**, not
  the whole window. A subset question ("groceries + rent") is answered by
  the filtered footer — no second call, no client-side sum.
- Sort never invents rows: it only reorders the filtered set. Ties always
  break deterministically (date, id, name), so a sort is stable across calls.

Trade-off accepted: a compound question ("what did I spend and on what?") is
two calls. That is the price of never returning an unasked section; there is
no single call that returns both without being the buffet we ruled out.

Decisions taken (from the design Q&A):

| Decision | Choice |
|---|---|
| Tool count | merged by question family, discriminated by a required mode |
| Output shape | one table max, flat fields, no bundles |
| Row picking | plural `categories[]` on breakdown/compare-categories/budget_status, plural `accounts[]` on breakdown; everywhere else singular `category`/`account` stays |
| Ordering | caller-chosen `sort_by` + `order` per table, small per-tool enum, deterministic default and deterministic tie-breaks |
| Subset scalars | no separate scalar tool: the filtered table footer IS the subset compute (over returned rows, not the window) |
| Aggregation | MongoDB `$group`/`$median` where available; Go for bucketing, clustering, proration, rounding |
| Windows | `start`/`end` required on every windowed tool (`start_a`/`end_a`/`start_b`/`end_b` for `money_compare`); `list_budgets` is unwindowed — no presets, no hidden "this month" |
| Budgets | `money_budgets` keyed `(category, month)`, versioned limits, CRUD |
| Units | ledger keeps rupees floats; analysis aggregates are integer paise |

## Tool catalogue

| # | Tool | Modes | Returns | Kind |
|---|---|---|---|---|
| 10 | `money_totals` | — | flat scalars | read |
| 11 | `money_breakdown` | `by=category\|account` | one table + footer | read |
| 12 | `money_top_expenses` | — | one table | read |
| 13 | `money_series` | `bucket=day\|week\|month` | one table | read |
| 14 | `money_compare` | `by=totals\|categories` | one table | read |
| 15 | `money_recurring` | — | one table | read |
| 16 | `money_anomalies` | `scope=bucket\|row` | one table | read |
| 17 | `budget_status` | — | one table + footer | read |
| 18 | `list_budgets` | — | one table | read |
| 19 | `create_budget` | — | flat row scalars | write (`ask`) |
| 20 | `update_budget` | — | flat row scalars | write (`ask`) |
| 21 | `delete_budget` | — | flat scalars | destructive (`*delete*`) |

Final surface: 21 tools (12 new). Permission patterns:
`*create_budget*`, `*update_budget*`, plus the existing `*delete*`.

---

## Part 0 — Shared contract

```gherkin
Feature: Analysis money conventions
  Background:
    Given amounts are stored as int64 paise
    And analysis aggregates are integer fields ending in _paise
    And every window is calendar days in IST

  Scenario: Window bounds are validated once, like query_transactions
    Given start "2026-03-01" and end "2026-03-31"
    Then the applied window is start 2026-03-01, end 2026-03-31, days 31
    And filtering is date >= start and date <= end, inclusive

  Scenario Outline: Bad windows are rejected, never silently empty
    When a windowed tool is called with <start> and <end>
    Then the call fails with <error>
    And no rows or buckets are returned

    Examples:
      | start        | end          | error |
      | ""           | "2026-03-31" | start is required and must be YYYY-MM-DD |
      | "2026-03-31" | "2026-03-01" | start 2026-03-31 is after end 2026-03-01 |
      | "2026-02-30" | "2026-03-31" | not a real calendar date |
      | "2026-3-01"  | "2026-03-31" | not YYYY-MM-DD |

  Scenario: Padded bounds are trimmed, not rejected
    Given start " 2026-03-01 " and end " 2026-03-31 "
    Then the filter uses and echoes the trimmed values

  Scenario: A window with no rows is a real answer
    Then ok is true, every paise field is 0, every table is []

  Scenario: Transfers are never income or expense
    Given a transfer of 5000.00 from HDFC Checking to Cash inside the window
    Then income_paise and expense_paise exclude it
    And it appears only as transfer fields or an account leg

  Scenario: Future-dated and old rows follow the window
    Then a window covering 2019-01-01..2030-12-31 includes both extremes
    And 2026-03-01..2026-03-31 includes neither

  Scenario: Archived accounts still contribute history
    Given an archived account with a row inside the window
    Then account mode includes it with archived true
    And a filter naming it works

  Scenario: Unknown filter values are rejected, not zero-filled
    When category is "grocerys" or account is "cash" (stored "Cash")
    Then the call fails naming the unknown value

  Scenario: Plural filters select rows, then the footer follows the selection
    Given categories ["groceries", "bills"] on a category table
    Then only those two rows are returned
    And total_amount_paise equals their sum, not the window total
    And share_pct is recomputed over the returned rows and sums to 100.00

  Scenario: Plural filter elements are normalized like singulars
    Then " Groceries " matches groceries and duplicates collapse to one entry
    And categories_applied echoes the normalized list
    And an unknown element fails the whole call, naming the element

  Scenario: Sort reorders, never refilters
    Given sort_by "name" and order "asc" on a category table
    Then rows are ordered by category ascending
    And the footer values are unchanged from the default sort
    And ties break by the tool's documented tie-break (amount, date, id)

  Scenario: Unknown sort values are rejected
    When sort_by is "colour" or order is "sideways"
    Then the call fails naming the accepted values
    And the default sort is stated in the error

  Scenario: Analysis reflects edits and deletes immediately
    Given money_totals reported expense_paise 827025
    When a row is deleted with delete_transactions
    Then the next money_totals reports the reduced total
    And no cache or stored aggregate is consulted

  Scenario: Percentages and averages round half-up
    Then 26678.2258 paise is returned as 26678
    And 89.85245 percent is returned as 89.85

  Scenario: Responses are flat, with at most one table
    Then no response contains a nested object
    Then no response contains more than one array
    And window values are flat start, end and days fields
    And applied values are flat, never a method object
    And a row never nests a further array

  Scenario Outline: The mode is required and validated
    When a mode-dispatched tool is called with <mode>
    Then the result is <result>

    Examples:
      | mode      | result |
      | ""        | rejected: the mode argument is required |
      | "quarter" | rejected, naming the accepted modes |
      | "CATEGORY"| accepted, lowercased and trimmed |
```

Unit detail: `*_paise` fields are integer money; percentage fields
(`savings_rate_pct`, `share_pct`, `used_pct`, `delta_pct`) are display values
rounded to 2 decimals. The two row lists that mirror ledger rows —
`money_top_expenses` entries and `money_anomalies(scope=row)` outliers —
carry both `amount` (rupees) and `amount_paise` (exact), so a returned row can
be fed straight back into `edit_transaction`/`delete_transactions`.

Shared fixture: accounts Cash (opening 1000.00), HDFC Checking (opening
50000.00), Equity Card (opening 0.00), archived Old Wallet (opening 250.00);
February 2026 rows (pre-start activity for the archived account only):
a 300.00 health expense on Old Wallet (02-05) and a 250.00 transfer from
HDFC Checking to Old Wallet (02-10). The transfer's source matters: HDFC's
March opening is its seed minus that February leg (49750.00), which is
exactly what the account_flow scenarios assert.
March 2026 rows:

| date | type | amount | category | account | sending_to |
|---|---|---|---|---|---|
| 03-01 | income | 80000.00 | salary | HDFC Checking | |
| 03-02 | expense | 2500.50 | groceries | HDFC Checking | |
| 03-03 | expense | 450.00 | eating out | Equity Card | |
| 03-05 | transfer | 5000.00 | other | HDFC Checking | Cash |
| 03-08 | expense | 120.75 | transport | Cash | |
| 03-12 | expense | 1800.00 | groceries | Equity Card | |
| 03-20 | expense | 999.00 | fun | HDFC Checking | |
| 03-28 | expense | 2400.00 | bills | HDFC Checking | |
| 03-31 | income | 1500.00 | other | Cash | |

Derived once and reused: income 8,150,000 paise; expense 827,025; net
7,322,975; transfer volume 500,000; transfer count 1; rows 9/2/6/1.

---

## Part 1 — Analyses

### 1. `money_totals`

Question: "how did this period go?" Answer: flat scalars, no table, no mode.

Input: `start`, `end` (required); optional `category`, `account`.

```gherkin
Feature: Period totals
  Scenario: The scalar block is complete and derived
    When money_totals is called for 2026-03-01..2026-03-31
    Then start is 2026-03-01, end is 2026-03-31, days is 31
    And rows_total is 9, rows_income is 2, rows_expense is 6, rows_transfer is 1
    And income_paise is 8150000
    And expense_paise is 827025
    And net_paise is 7322975
    And savings_rate_pct is 89.85
    And avg_daily_expense_paise is 26678
    And avg_daily_income_paise is 262903
    And transfer_count is 1
    And transfer_volume_paise is 500000

  Scenario: It returns one answer and no table
    Then the response has no array at all

  Scenario: An expense-only window has no savings rate
    Then savings_rate_pct is null
    And net_paise is negative by exactly expense_paise

  Scenario: An empty window is zeros, not an error
    Then ok is true and every paise field is 0

  Scenario: A category filter narrows every field
    When category is "groceries"
    Then income_paise is 0, expense_paise is 430050 and rows_total is 2

  Scenario Outline: Filters are validated, not coerced
    When money_totals is called with <argument>
    Then the call fails with <error>

    Examples:
      | argument                       | error |
      | category "grocerys"            | category "grocerys" does not exist |
      | account "cash" (stored "Cash") | unknown account 'cash' |
```

The response is the scalar block plus flat echoes: `start`, `end`, `days`,
`category`, `account` (null when absent).

### 2. `money_breakdown`

Question: "where did it go / how did each account move?" Answer: one table,
selected by a required `by` mode.

Input: `start`, `end`, `by` (`category|account`, required);
`side` (`expense|income`) required **only** when `by=category`;
optional `categories[]` (row picker, both modes), `accounts[]` (scope in
category mode, row picker in account mode), `sort_by`, `order`.

Sort enums: category mode `amount|name|count` (default `amount`);
account mode `closing|name|net|inflows|outflows` (default `closing`);
`order` `asc|desc` (default `desc` in both modes).

```gherkin
Feature: Breakdown by one dimension
  Scenario: Category mode ranks one side with shares
    When money_breakdown is called with by "category" and side "expense"
    Then total_amount_paise is 827025 and total_count is 6
    And categories[0] is groceries: amount_paise 430050, count 2,
      share_pct 52.00, avg_paise 215025
    And categories[1] is bills: amount_paise 240000, share_pct 29.02
    And the last row is transport: amount_paise 12075, share_pct 1.46
    And the share_pct values sum to 100.00

  Scenario: Income mode is the same table, income rows only
    When side is "income"
    Then categories[0] is salary: amount_paise 8000000, share_pct 98.16

  Scenario: Account mode is the same tool, a different table
    Given the shared February rows (HDFC Checking sent 250.00 to Old Wallet)
    When money_breakdown is called with by "account"
    Then accounts[0] is HDFC Checking: opening 4975000, inflows 8000000,
      outflows 1089950, net 6910050, closing 11885050
    And Cash is opening 100000, inflows 650000, outflows 12075,
      closing 737925
    And Equity Card closing is -225000
    And Old Wallet is opening 20000, closing 20000, archived true
    And total_opening_paise is 5095000
    And total_closing_paise is 12417975
    And total_active_closing_paise is 12397975
    And rows are ordered by closing_balance_paise descending, then name
    And side is ignored and not required in this mode

  Scenario: Account mode includes archived accounts with activity
    Then Old Wallet appears with archived true and closing 20000
    And total_active_closing_paise excludes it

  Scenario: The account filter is exact and plural
    Then accounts ["Cash"] returns one row
    And accounts ["cash"] is rejected as unknown
    And a hex id returns the same account
    And accounts ["Cash", "Cash", " Equity Card "] dedupes to two rows
    And categories ["groceries", "bills"] in category mode returns two rows
      with the footer over those two rows only

  Scenario: Category-mode sort is caller-chosen
    When sort_by is "name" and order is "asc"
    Then rows are ordered by category ascending
    And total_amount_paise is unchanged from the default sort

  Scenario: Account-mode sort is caller-chosen
    When sort_by is "name" and order is "asc"
    Then rows are ordered by name ascending
    And total_closing_paise is unchanged from the default sort

  Scenario: Ties break deterministically
    Then equal category amounts are ordered by category name
    And equal closing balances are ordered by account name

  Scenario: The footer is flat, not a totals object
    Then category mode reports total_amount_paise and total_count
    And account mode reports total_closing_paise and
      total_active_closing_paise
    And there is no nested totals object

  Scenario Outline: Mode-specific arguments are validated by name
    When money_breakdown is called with <argument>
    Then the call fails with <error>

    Examples:
      | argument                | error |
      | by ""                   | by is required: category or account |
      | by "bucket"             | by must be category or account; use money_series |
      | by "category", side ""  | side is required when by=category |
      | by "category", side "transfer" | side must be expense or income |
      | by "sideways"           | rejected as an unknown mode, naming category and account |
      | sort_by "colour"        | sort_by must be amount, name or count (category mode) |
      | order "sideways"        | order must be asc or desc |

  Scenario: No second table is returned
    Then category mode returns no accounts array
    And account mode returns no categories array
```

Category row: `category`, `amount_paise`, `count`, `share_pct`, `avg_paise`.
Account row: `id`, `name`, `archived`, `opening_balance_paise`,
`inflows_paise`, `outflows_paise`, `net_paise`, `closing_balance_paise`,
`income_paise`, `expense_paise`, `transfer_in_paise`, `transfer_out_paise`,
`rows_total`.
Flat scalars (both modes): `start`, `end`, `days`, `by`,
`categories_applied`, `accounts_applied`, `sort_by`, `order`, plus the
mode's footer. Category mode additionally echoes `side`; account mode
omits it (`side` is accepted and ignored there, never echoed).

### 3. `money_top_expenses`

Question: "what were the biggest hits?" Answer: one bounded row list.

Input: `start`, `end` (required); optional `category`, `account`; `limit`
1..20, default 5; `sort_by` `amount|date` (default `amount`), `order`
`asc|desc` (default `desc`).

```gherkin
Feature: Largest expenses
  Scenario: Default limit is five and truncation is reported
    When money_top_expenses is called for the fixture window
    Then expenses has 5 entries and total_matching is 6
    And expenses[0] is the 2500.50 groceries row
    And expenses[1] is the 2400.00 bills row
    And expenses[2] is the 1800.00 groceries row

  Scenario: Sort is caller-chosen
    When sort_by is "date" and order is "asc"
    Then expenses[0] is the earliest expense in the window
    And total_matching is unchanged

  Scenario Outline: Sort values are validated
    When money_top_expenses is called with <argument>
    Then the call fails with <error>

    Examples:
      | argument       | error |
      | sort_by "name" | sort_by must be amount or date |
      | order "up"     | order must be asc or desc |

  Scenario: Each row is usable for edit or delete by id
    Then every entry has id, date, amount, amount_paise, category, account, note

  Scenario: Ordering is deterministic
    Then rows are ordered by amount_paise descending, then date ascending,
      then id ascending

  Scenario: Ties at the limit cut deterministically
    Given three rows tie for the fifth slot
    Then the earliest date wins, then the smallest id

  Scenario Outline: The limit is validated, not clamped
    When money_top_expenses is called with limit <limit>
    Then the result is <result>

    Examples:
      | limit | result |
      | 1     | one row |
      | 21    | rejected: limit must be between 1 and 20 |
      | 0     | rejected, because 0 asks for an answer with no rows |

  Scenario: An empty window returns an empty list
    Then expenses is [] and total_matching is 0
```

Row: `id`, `date`, `amount`, `amount_paise`, `category`, `account`, `note`.
Flat scalars: `start`, `end`, `days`, `category`, `account`, `limit_applied`,
`total_matching`, `sort_by`, `order`.

### 4. `money_series`

Question: "what does the trend look like?" Answer: one bucket table. The
bucket size is itself the required mode.

Input: `start`, `end`, `bucket` (`day|week|month`, required); optional
`category`, `account`; `order` `asc|desc` (default `asc`).

```gherkin
Feature: Bucketed series
  Background:
    Given the window 2026-01-01..2026-03-31
    And February has one 300.00 health expense and one 250.00 transfer
    And January has no rows

  Scenario: Month buckets are zero-filled
    When money_series is called with bucket "month"
    Then bucket is "month" and bucket_count is 3
    And buckets are 2026-01, 2026-02, 2026-03 in order
    And 2026-01 has expense_paise 0, rows_total 0, days 31
    And 2026-02 has expense_paise 30000, transfer_volume_paise 25000
    And 2026-03 has expense_paise 827025, income_paise 8150000

  Scenario: The series carries no period totals
    Then there is no total field of any kind
    And a caller wanting the period total calls money_totals

  Scenario: Week buckets start Monday and clip to the window
    Given the window 2026-03-01..2026-03-31 (2026-03-01 is a Sunday)
    When bucket is "week"
    Then buckets[0] is key 2026-02-23, from 2026-03-01, days 1
    And buckets[1] is key 2026-03-02, to 2026-03-08, days 7
    And the last bucket is key 2026-03-30 with days 2

  Scenario: A filter narrows values, not the grid
    When category is "groceries"
    Then every bucket still appears, including empty ones

  Scenario: Order flips the grid without changing values
    When order is "desc"
    Then buckets are 2026-03, 2026-02, 2026-01 in order
    And 2026-03 still has expense_paise 827025

  Scenario: Day buckets refuse to truncate silently
    When called for 2025-01-01..2026-12-31 with bucket "day"
    Then the call fails: 730 buckets exceeds the 500-bucket limit
    And no partial series is returned

  Scenario Outline: Bucket is normalized, unknown values rejected
    When bucket is <bucket>
    Then the result is <result>

    Examples:
      | bucket    | result |
      | "Month"   | a month series |
      | "quarter" | rejected, naming day, week, month |
      | ""        | rejected, bucket is required |
```

Row: `key`, `from`, `to`, `days`, `rows_total`, `rows_income`,
`rows_expense`, `rows_transfer`, `income_paise`, `expense_paise`,
`net_paise`, `avg_daily_expense_paise`, `transfer_count`,
`transfer_volume_paise`.
Flat scalars: `start`, `end`, `days`, `bucket`, `bucket_count`, `order`,
`category`, `account`.

### 5. `money_compare`

Question: "is B up or down versus A, and what drove it?" Answer: one table,
selected by `by`.

Input: `start_a`, `end_a`, `start_b`, `end_b` (required); `by`
(`totals|categories`, required); totals mode: optional singular `category`,
`account`; categories mode: optional `categories[]` (row picker) and singular
`account` (scope); categories mode also takes `sort_by`
`delta|name|b_amount` (default `delta`, meaning absolute delta_paise
descending) and `order` `asc|desc` (default `desc`). Totals mode has a fixed
three-row order (income, expense, net) and takes no sort arguments.

```gherkin
Feature: Compare two windows
  Background:
    Given period A is 2026-02-01..2026-02-28: one 300.00 health expense
    And period B is 2026-03-01..2026-03-31: the shared March fixture
    And overlap and unequal lengths are allowed

  Scenario: Totals mode is a three-row metric table
    When money_compare is called with by "totals"
    Then metrics has three rows: income, expense, net
    And the expense row is a_paise 30000, b_paise 827025,
      delta_paise 797025, delta_pct 2656.75, direction "up"
    And the income row is a_paise 0, b_paise 8150000, delta_pct null,
      direction "up"
    And days_a is 28 and days_b is 31

  Scenario: Categories mode attributes the change
    When money_compare is called with by "categories"
    Then health is a_paise 30000, b_paise 0, delta_paise -30000,
      direction "down"
    And groceries is a_paise 0, b_paise 430050, delta_paise 430050,
      delta_pct null, direction "up"
    And rows are ordered by absolute delta_paise descending

  Scenario: Categories mode accepts a row picker and a sort
    When categories is ["groceries", "bills"]
    Then only those two categories appear
    When sort_by is "name" and order is "asc"
    Then rows are ordered by category ascending

  Scenario: Identical windows are flat, not null
    Then every delta_paise is 0, every direction is "flat"
    And every delta_pct is 0.00 (both sides zero is a 0 delta, not null)

  Scenario: A zero baseline yields null percent — unless nothing moved
    Then delta_pct is null when a_paise is 0 and the delta is non-zero
    And delta_pct is 0.00 when both sides are 0

  Scenario: One mode, one table
    Then totals mode returns no categories array
    And categories mode returns no metrics array
    And no period totals block is returned in either mode

  Scenario Outline: Each period is validated independently and by name
    When money_compare is called with <bad input>
    Then the call fails with <error>

    Examples:
      | bad input           | error |
      | start_a after end_a | period A: start ... is after end ... |
      | end_b "2026-02-30"  | period B: not a real calendar date |
      | by ""               | by is required: totals or categories |
      | by "accounts"       | by must be totals or categories |
```

Totals row: `metric` (`income|expense|net`), `a_paise`, `b_paise`,
`delta_paise`, `delta_pct`, `direction`. Categories row: `category`,
`a_paise`, `b_paise`, `delta_paise`, `delta_pct`, `direction`, `a_share_pct`,
`b_share_pct`, `a_count`, `b_count`.
Flat scalars (both modes): `start_a`, `end_a`, `days_a`, `start_b`,
`end_b`, `days_b`, `by`. Categories mode additionally echoes
`categories_applied`, `account`, `sort_by`, `order`; totals mode omits
them (fixed three-row order, no sort args, `categories[]` rejected).

### 6. `money_recurring`

Question: "what repeats?" Answer: one candidate table, scalars only.

Input: `start`, `end` (required); optional `category`, `account`;
`min_occurrences` 2..24 (default 3); `amount_tolerance_pct` 0..25 (default 0);
`limit` 1..50 (default 10); `sort_by` `next|amount|count` (default `next`,
nulls last) and `order` `asc|desc` (default `asc`).

```gherkin
Feature: Detect recurring movements
  Background:
    Given the window 2026-01-01..2026-06-30
    And bills 2400.00 on 2026-01-05, 2026-02-05, 2026-03-05
    And groceries 500.00 on 2026-03-01, 2026-03-08, 2026-03-15
    And salary 80000.00 monthly from 2026-01-01 to 2026-06-01
    And no transfers in the window

  Scenario: Cadence comes from the median interval
    Then bills has cadence "monthly", count 3, jitter 0, confidence "high",
      next_expected_date 2026-04-05
    And groceries has cadence "weekly", next_expected_date 2026-03-22
    And salary has cadence "monthly", count 6, confidence "high"

  Scenario: Rows carry scalars only, never their occurrences
    Then a candidate has category, type, amount_paise, count, first_date,
      last_date, median_interval_days, interval_jitter_days, cadence,
      next_expected_date, confidence, account, account_count
    And there is no occurrences array
    And each candidate's rows are fetchable from query_transactions by id

  Scenario: The list is bounded and truncation is reported
    Then at most limit candidates are returned, ordered by
      next_expected_date ascending (nulls last), then category
    And total_candidates and truncated describe what was cut

  Scenario: Sort is caller-chosen
    When sort_by is "amount" and order is "desc"
    Then candidates[0] is the salary candidate
    And total_candidates is unchanged

  Scenario: Exact matching is the default
    Given bills 1200.00, 1320.00 and 1250.00 in three consecutive months
    When amount_tolerance_pct is 0
    Then no candidate is returned

  Scenario: Tolerance clusters around a median
    When amount_tolerance_pct is 10
    Then one bills candidate has amount_paise 125000 and count 3

  Scenario: Too few occurrences is not a candidate
    Then two bills rows with min_occurrences 3 produce []

  Scenario: Irregular spacing is labelled, not hidden
    Given three fun rows 45, 50 and 60 days apart
    Then cadence is "irregular", confidence "low", next_expected_date null

  Scenario: Confidence is jitter-based
    Then high requires count >= min_occurrences and jitter <= 3 days
    And medium requires jitter <= 7 days
    And low covers the rest

  Scenario: Income is detected, transfers are not
    Then salary appears as type "income"
    And a repeated 5000.00 transfer never appears

  Scenario Outline: Inputs are validated explicitly
    When money_recurring is called with <argument>
    Then the call fails with <error>

    Examples:
      | argument                | error |
      | min_occurrences 1       | min_occurrences must be between 2 and 24 |
      | amount_tolerance_pct 26 | amount_tolerance_pct must be between 0 and 25 |
      | limit 0                 | limit must be between 1 and 50 |
      | sort_by "colour"        | sort_by must be next, amount or count |
      | order "sideways"        | order must be asc or desc |
```

Candidate row scalars are listed in the recurring scenario above.
Flat scalars: `start`, `end`, `days`, `category`, `account`,
`min_occurrences`, `amount_tolerance_pct`, `limit_applied`, `sort_by`,
`order`, `total_candidates`, `truncated`.

Cadence bands from `median_interval_days`: 5–9 weekly, 12–17 fortnightly,
25–35 monthly, 80–100 quarterly, otherwise irregular. `next_expected_date` =
`last_date + median_interval_days`, null when irregular. Jitter = max
absolute deviation of consecutive intervals from the median. Grouping is
greedy by date within category+type, joining the first cluster whose median is
within ±tolerance%; single-row clusters drop. Two equal subscriptions in one
category can merge — documented, narrowable by `account`.

### 7. `money_anomalies`

Question: "what was unusual?" Answer: one outlier table, selected by `scope`.

Input: `start`, `end`, `scope` (`bucket|row`, required); optional
`category`, `account`; `sensitivity` (`low|medium|high`, default medium);
`min_amount_paise` (default 10000); `bucket` (`day|week|month`, required only
when `scope=bucket`); `sort_by` and `order` — bucket scope:
`delta|expense` (default `delta`, absolute delta_paise) with default `desc`;
row scope: `amount|ratio` (default `amount`) with default `desc`.

```gherkin
Feature: Anomalies
  Scenario: Bucket scope flags unusual periods
    Given six monthly buckets with expense 9500.00, 10000.00, 10000.00,
      10200.00, 10500.00 and 45000.00
    When money_anomalies is called with scope "bucket" and bucket "month"
    Then outliers has one entry for the 45000.00 month
    And it carries expense_paise 4500000, median_paise 1010000,
      delta_paise 3490000, direction "above"
    And baseline_median_paise is 1010000 and buckets_considered is 6

  Scenario: Row scope flags unusual transactions
    Given groceries rows 2500.50, 1800.00 and 12000.00 in the window
    When money_anomalies is called with scope "row"
    Then outliers has the 12000.00 row with category_median_paise 250050,
      ratio_pct 479.90, direction "above"
    And the row carries its id for edit or delete

  Scenario: Each scope returns only its own table
    Then bucket scope returns no transaction rows
    And row scope returns no bucket rows
    And neither scope returns a baselines array

  Scenario: Thresholds are flat and applied
    Then sensitivity_applied is "medium", ratio_applied is 1.50
      (sensitivity-driven: 2.00 low, 1.50 medium, 1.25 high — same in both
      scopes), and min_amount_paise is echoed
    And an entry requires amount > ratio x median and delta >= min_amount_paise

  Scenario: Too little history is not an error
    Then bucket scope with fewer than 3 buckets sets insufficient_baseline
      true and returns []
    And buckets_considered is the actual bucket count (e.g. 2), not 0
    And row scope with a category under 3 rows skips it and reports
      categories_evaluated and categories_skipped

  Scenario Outline: Mode-specific arguments are validated by name
    When money_anomalies is called with <argument>
    Then the call fails with <error>

    Examples:
      | argument                     | error |
      | scope ""                     | scope is required: bucket or row |
      | scope "bucket", bucket ""    | bucket is required when scope=bucket |
      | scope "bucket", bucket "quarter" | bucket must be day, week or month |
      | sensitivity "max"            | sensitivity must be low, medium or high |
      | min_amount_paise -1          | min_amount_paise must be >= 0 |

  Scenario: Sort is caller-chosen within each scope
    When scope is "row", sort_by is "ratio" and order is "desc"
    Then outliers are ordered by ratio_pct descending
    And the same rows are returned as the default sort

  Scenario: Income and transfers never appear as row outliers
    Then salary and transfer rows are absent from row scope
```

Bucket row: `key`, `from`, `to`, `days`, `expense_paise`, `median_paise`,
`delta_paise`, `direction`. Row outlier:
`id`, `date`, `amount`, `amount_paise`, `category`, `account`, `note`,
`category_median_paise`, `ratio_pct`, `direction`. Flat scalars (both
scopes): `start`, `end`, `days`, `scope`, `category`, `account`, `sort_by`,
`order`, `sensitivity_applied`, `ratio_applied`, `min_amount_paise`. Bucket scope
additionally echoes `bucket`, `baseline_median_paise`,
`buckets_considered`, `insufficient_baseline`, `reason`. Row scope
additionally echoes `categories_evaluated`, `categories_skipped`, and
never returns `bucket` or any `baseline_*` field (a passed `bucket` is
rejected in row scope).

---

## Part 2 — Budgets

### 2.1 Data model

New collection `money_budgets`, permanent (no TTL; the `money` retention step
stays a no-op):

| Field | Type | Rules |
|---|---|---|
| `category` | string | fixed category except `salary` (income is not budgeted) |
| `month` | string | `YYYY-MM`, real month, effective **from** this month |
| `monthlyLimit` | int64 paise | > 0, exact paise (input rupees, <= 2 decimals) |
| `createdAt` | instant | UTC, set on create |
| `updatedAt` | instant | UTC, set on create and every update |

Index: `uniq_budget_category_month` on `(category, month)`, unique.

Resolution: for month `m`, a category's limit is the `monthlyLimit` of the row
with the greatest `month <= m`; no row means unbudgeted for `m` (not an
error). A new month row preserves history, while a same-month correction via
`update_budget` mutates that month's row in place.

Proration: for each category and each month the window touches,
`round_half_up(monthlyLimit(month) x days_covered / days_in_month)`, summed
over months. Spend is expense-only; transfers excluded.

### 2.2 `budget_status`

Question: "am I over budget?" Answer: one table with one row per category,
plus its flat footer. Month detail is a one-month window, not a nested array.

Input: `start`, `end` (required); optional `categories[]` (row picker,
default all); `sort_by` `remaining|used|spend|limit|name` (default
`remaining`, smallest remaining first — most over budget first) and `order`
`asc|desc` (default `asc`).

```gherkin
Feature: Budget status
  Background:
    Given groceries rows 2026-01 at 5000.00 and 2026-03 at 6000.00
    And March groceries expenses 2500.50 and 1800.00
    And fun has a March expense of 1001.00 with no budget row

  Scenario: A full month uses that month's limit
    When budget_status is called for 2026-03-01..2026-03-31
    Then the groceries row has limit_paise 600000, spend_paise 430050,
      remaining_paise 169950, used_pct 71.68, over_budget false

  Scenario: A partial month prorates per covered day
    When budget_status is called for 2026-02-16..2026-03-15
    Then the groceries row has limit_paise 522466
    And that is 232143 paise for 13/28 of February plus 290323 for 15/31 of March
    And spend_paise is 430050

  Scenario: Unbudgeted categories share the table
    Then fun appears with budgeted false, limit_paise 0, spend_paise 100100,
      remaining_paise null, used_pct null, over_budget false
    And total_spend_paise counts budgeted categories only (430050)
    And total_unbudgeted_spend_paise is 100100

  Scenario: Exactly on budget is not over
    Given limit 4300.50 and spend 4300.50
    Then over_budget false, remaining 0, used_pct 100.00

  Scenario: Over budget is exact
    Given spend 430050 against limit 430000
    Then over_budget true and remaining_paise -50

  Scenario: The current month carries a projection
    Given the window includes the current IST month and today is the 15th
    Then projected_month_paise is spend-so-far x days_in_month / 15
    And projection_basis is "calendar_month": the projection uses the whole
      calendar month's expenses even if the window starts mid-month

  Scenario: A past window carries no projection
    Then projected_month_paise and projected_over_budget are null

  Scenario: No budgets still reports spend
    Then every row has budgeted false and limit_paise 0
    And total_limit_paise is 0

  Scenario: Income and transfers never consume a budget
    Then salary and transfer rows do not change spend_paise

  Scenario: A row picker narrows the table and its footer
    When categories is ["groceries"]
    Then only the groceries row remains
    And total_spend_paise equals the groceries spend_paise

  Scenario: Sort is caller-chosen
    When sort_by is "name" and order is "asc"
    Then rows are ordered by category ascending
    And the footer values are unchanged
```

Row: `category`, `budgeted`, `limit_paise`, `spend_paise`, `remaining_paise`,
`used_pct`, `over_budget`, `projected_month_paise`, `projected_over_budget`.
Footer: `total_limit_paise`, `total_spend_paise`, `total_remaining_paise`,
`total_used_pct`, `total_over_budget`, `budgeted_categories`,
`total_unbudgeted_spend_paise`, `projection_basis`.
Flat scalars: `start`, `end`, `days`, `categories_applied`, `sort_by`,
`order`.

### 2.3 `list_budgets`

```gherkin
Feature: List budget rows
  Scenario: One table, ordered, effective row flagged
    Then rows are ordered by category ascending, then month descending
    And at most one row per category has effective_now true for the current
      IST month (the greatest month <= today's month; zero when the category
      has only future rows)
    And a category whose only row is in the future has no effective_now row
    And there is intentionally no sort_by/order on this tool: it is a config
      listing with one canonical order, not a ranked answer

  Scenario: Filters are optional and validated
    Then month_from alone means from that month onward
    And month_to alone means up to that month
    And month_from after month_to is rejected
    And category "salary" is rejected as income
    And category "grocerys" is rejected as unknown

  Scenario: No budgets is an empty success
    Then ok is true and rows is []
```

Row: `id`, `category`, `month`, `monthly_limit`, `monthly_limit_paise`,
`effective_now`, `createdAt`, `updatedAt`.

### 2.4 `create_budget`

```gherkin
Feature: Create a monthly category budget
  Scenario: Create the first limit
    When create_budget is called with "groceries", 5000.00, "2026-01"
    Then ok is true and the row is read back by id
    And id, category, month, monthly_limit and monthly_limit_paise are flat
      top-level fields

  Scenario: A later row supersedes, an earlier one does not
    Given groceries rows at 2026-01 (5000.00) and 2026-03 (6000.00)
    Then 2026-02 resolves to 500000 and 2026-03 to 600000
    And 2026-12 resolves to 600000

  Scenario: Salary cannot be budgeted
    Then category "salary" fails: salary is income and cannot be budgeted

  Scenario Outline: Invalid budgets are rejected
    When create_budget is called with <argument>
    Then the call fails with <error>

    Examples:
      | argument             | error |
      | category "grocerys"  | category "grocerys" does not exist |
      | monthly_limit 0      | monthly_limit must be > 0 |
      | monthly_limit 10.001 | monthly_limit must be an exact paise value |
      | month "2026-13"      | month must be a real YYYY-MM month |
      | month "2026-1"       | month must be YYYY-MM |
      | month ""             | month is required |

  Scenario: A duplicate month is rejected, not overwritten
    Then create_budget for groceries 2026-03 fails naming the existing id
      and pointing at update_budget
    And the existing row is unchanged

  Scenario: Future months are legitimate
    Then a budget at 2027-01 is stored and applies from 2027-01
```

### 2.5 `update_budget`

```gherkin
Feature: Correct a budget limit
  Scenario: The limit changes in place
    Given a row for groceries 2026-03 at 6000.00
    When update_budget sets 6500.00
    Then the row is returned read back by id, flat, with
      monthly_limit_paise 650000
    And updatedAt is newer than createdAt

  Scenario: A no-op is rejected
    Then updating to the stored limit fails: monthly_limit is unchanged

  Scenario Outline: Invalid updates are rejected
    When update_budget is called with <argument>
    Then the call fails with <error>

    Examples:
      | argument            | error |
      | id ""               | id is required |
      | unknown id          | unknown budget id '<id>' |
      | monthly_limit 0     | monthly_limit must be > 0 |
      | monthly_limit 9.999 | monthly_limit must be an exact paise value |
```

### 2.6 `delete_budget`

```gherkin
Feature: Delete one budget version
  Scenario: Dry run is the default and names the fallback
    Given groceries rows at 2026-01 (5000.00) and 2026-03 (6000.00)
    When delete_budget targets the 2026-03 id
    Then dry_run is true and the row's fields are returned flat
    And falls_back_to_month is 2026-01
    And falls_back_to_limit_paise is 500000
    And nothing is deleted

  Scenario: Confirmed delete removes only that version
    Then dry_run false deletes 1 row
    And 2026-03 resolves to 500000 again

  Scenario: Deleting a non-latest version scopes the fallback to its month
    Given groceries rows at 2026-01 (5000.00) and 2026-03 (6000.00)
    When delete_budget dry-runs the 2026-01 id
    Then falls_back_to_month is null, falls_back_to_limit_paise is 0 and
      unbudgeted is true — for the deleted 2026-01 slot only
    And 2026-04 still resolves to 600000 through the surviving 2026-03 row

  Scenario: Deleting the only version leaves the category unbudgeted
    Then falls_back_to_month is null, falls_back_to_limit_paise is 0 and
      unbudgeted is true

  Scenario: Unknown ids are rejected in both modes
    Then the call fails with unknown budget id '<id>'
```

---

## Part 3 — Implementation notes

- Shared sub-functions in `internal/money/analysis.go` (window scan,
  type/category grouping, account legs, bucketizing, median, rounding) and
  `internal/money/budgets.go` (resolution, proration). Each tool is a thin
  dispatcher: validate the mode, run only that mode's aggregation, assemble
  one flat response.
- Files: `cmd/miser-money/analysis.go` and `cmd/miser-money/budgets.go` hold
  the input types and `register*Tools`; `registerTools` calls both. Named
  input types keep `schema_test.go` able to infer the published contract.
- MongoDB: `$match` on date (+ type/category/account) then `$group` by the
  selected dimension only. `money_breakdown(by=account)` uses three groups
  (pre-start legs by accountId, pre-start by sending_to, in-window by both)
  plus account seeds. `$median` in `$group` where the server is 7.0+, with a
  sort-and-pick fallback in Go. Date bucketing, zero-fill, clustering,
  proration and half-up rounding are Go-side because dates are strings and
  the rules must be exact and unit-testable.
- Mode dispatch computes one table, not all tables: `by=account` never runs
  the category aggregation, and `scope=row` never builds buckets.
- Filtering happens before aggregation (`$match` narrows the scan), sorting
  after (in Go, over the filtered rows); the footer is always summed from
  the returned rows so a subset footer never silently reports window totals.
- No stored aggregates and no cache: every response is recomputed from the
  ledger, so edit/delete are visible on the next call.
- Budget writes are single-document writes, like every other money mutation.
- `EnsureSchema` creates `uniq_budget_category_month`; money retention stays a
  no-op, so budgets are permanent.
- Tool descriptions state: transfers excluded from income/expense, units,
  rounding, ordering (default sort + tie-breaks), empty-window behavior,
  one-table rule, mode-specific required arguments, plural-filter semantics
  (normalize, dedupe, footer-over-returned-rows), and defaults (`limit`,
  `min_occurrences`, `amount_tolerance_pct`, `sensitivity`,
  `min_amount_paise`, `dry_run`, `sort_by`/`order`). No default ever applies
  to `start`/`end`/`by`/`bucket`/`scope`.

## Part 4 — Test and docs checklist

Tests:

- `cmd/miser-money/schema_test.go`: required sets for the 12 new input types
  (`start`/`end` on every windowed tool — `start_a`/`end_a`/`start_b`/`end_b`
  for `money_compare`, which has no `start`/`end`; `by` on breakdown/compare; `bucket`
  on series; `scope` on anomalies; `category`/`month`/`monthly_limit` on
  budgets; `get_balances` stays no-arg); optional-field documentation.
- Handler tests without MongoDB: window validation; mode validation; the
  mode-specific requirement errors (`side` with `by=category`, `bucket` with
  `scope=bucket`); `sort_by`/`order` enums per tool; plural-filter
  normalization (trim, lowercase, dedupe) and unknown-element rejection;
  bounds on limit/min_occurrences/tolerance; month validation; unknown
  category/account.
- Store tests (skip without `MONGODB_URI`): each mode against the shared
  fixture; cross-tool invariants — `money_totals.expense_paise ==
  money_breakdown(by=category,side=expense).total_amount_paise == budgeted
  spend`, `money_breakdown(by=account).total_active_closing_paise ==
  get_balances.total`, transfer legs net to zero, series buckets sum to
  period totals per field; subset checks — filtered footer equals the
  unfiltered table restricted to those rows; sort checks — every `sort_by`
  returns the same row set in the requested order with an unchanged footer.
- Shape tests: at most one array; no nested object; no row contains an array;
  each mode returns only its own table; applied values flat.
- Determinism tests for every tie-break (amount, date, id, category name,
  absolute delta, closing balance).
- Proration and rounding vectors (522466 = 232143 + 290323).
- `opencode_perm_test.go`: add the nine read tools to `freeTools`; add
  `*create_budget*` and `*update_budget*` to `opencode.json` and
  `expectedAsk`; `TestEveryToolClassified` must pass with 21 tools.
- No Android CI involvement; no `VERSION` bump (non-app change).

Docs in the same change:

- `mcps/money/README.md`: tool table (+12), `money_budgets` schema,
  resolution and proration rules, replace the now-false "Period totals (no
  summarize tool)" section, retention note.
- `documentation.md`: `money_budgets` schema row, retention row (permanent),
  money tool list.
- `.opencode/chat.md`: miser-money count 9 -> 21, read/write split, add
  `create_budget`/`update_budget` to the `ask`-gated writes and
  `delete_budget` to the destructive list.
- `AGENTS.md`: no edit unless it starts enumerating money tools.

## Part 5 — Open decisions to confirm

1. **Mode dispatch** (the middle ground): `money_breakdown(by=...)`,
   `money_compare(by=...)`, `money_anomalies(scope=...)` each require a mode
   and return only that mode's table. Merging instead of separate tools is
   what keeps the tool list at 21 without any response carrying an unused
   section.
2. **Paise aggregates**; `amount` rupees only on row-shaped objects.
3. **Budget model**: dated versions keyed `(category, month)`, resolved by the
   greatest month <= target.
4. **Partial-month proration** per covered day, rounded per month then summed.
5. **Recurring default**: exact amounts (`amount_tolerance_pct 0`), no
   occurrence rows in the response.
6. **Budget writes prompt**; `delete_budget` uses `*delete*`.

## Out of scope

- No multi-currency or FX.
- No forecasts beyond the current-month budget projection.
- No merchant field: recurrence groups by category + amount.
- No new transaction write paths, no stored totals, no caching.
- Stacked PRs are possible (analyses, then budgets); the budgets PR targets
  the analyses PR's branch.
