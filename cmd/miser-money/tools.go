package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerAnalysisTools attaches the seven read-only analysis tools. It lives
// in this file (not main.go) for size, but every registration below uses a
// literal tool name so the permission test's enumeration keeps seeing them.
func registerAnalysisTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "money_totals",
		Description: "Period profit-and-loss for a date window (flat scalars, no table). Required: start, end (YYYY-MM-DD inclusive, like query_transactions — empty or reversed bounds are rejected, not silently empty). Optional: category, account (single; unknown names match nothing, so the window reports zeros — the empty answer, not a rejection; blank/whitespace-only means absent, unfiltered). Reports income/expense/net in paise, savings_rate_pct (null when no income), daily averages, and transfer count/volume. Transfers are never income or expense. An empty window returns zeros, not an error."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in moneyTotalsInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleMoneyTotals(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "money_breakdown",
		Description: "One breakdown table, selected by required by=category|account. by=category ranks one side (side expense|income required) with amount/count/share/avg per category; shares sum to 100.00 over the returned rows. by=account reports per-account opening/inflows/outflows/net/closing plus leg splits (side is accepted and ignored when valid — a typo'd side fails like category mode) — opening is always the real pre-window balance and closing the real window-end balance. Optional: categories[] (normalized, deduped) narrows which window rows contribute to the flow columns — each name matches rows carrying it and a name matching nothing contributes no rows. In by=category mode an all-unknown filter returns the empty table while a mixed one still returns the known names' rows (blank/whitespace-only entries count as absent); in by=account mode the account list always returns when no accounts[] filter is sent (unknown categories zero the flow columns but real balances still show — categories[] is not a row picker there, only accounts[] is) — in account mode it is NOT a row picker (accounts with zero matching flows still appear with rows_total 0 and their full real closing, and closing need not equal opening+net on a filtered table); accounts[] (scope in category mode, row picker in account mode; case-sensitive exact; blank/whitespace-only entries count as absent; an unknown picker name matches nothing — in account mode an all-unknown picker returns the empty table), sort_by (category mode: amount|name|count default amount; account mode: closing|name|net|inflows|outflows default closing), order asc|desc default desc. Sort args are validated in the handler only (no schema enum — the valid sets depend on the mode) and match case-insensitively after trimming; blank means the defaults (no schema enum rejects it — unlike query_transactions, where a blank must be omitted). Footers total the returned rows, not the window — a filtered call's flow total IS the subset's sum, while total_opening/closing are real-balance sums. Transfers never appear in category mode; in account mode they are transfer_in/out legs. Archived accounts with activity appear flagged."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in moneyBreakdownInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleMoneyBreakdown(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "money_top_expenses",
		Description: "Largest expense rows in a window (one bounded list, never a dump). Required: start, end. Optional: category, account (single; an unknown name matches no rows, so total_matching reads 0 — not an error; blank/whitespace-only means absent, unfiltered), limit 1..20 default 5, sort_by amount|date default amount, order asc|desc default desc. Sort args use a fixed set, validated in the handler only (no schema enum), and match case-insensitively after trimming; blank means the defaults (no schema enum rejects it — unlike query_transactions, where a blank must be omitted). Default ordering is amount desc, then date asc, then id asc (deterministic, including ties at the limit); sort_by/order change the primary key while ties still break by date then id. Each row carries id/date/amount_paise/category/account/note so it can go straight into edit_transaction/delete_transactions. total_matching counts every expense; expenses holds at most limit."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in moneyTopExpensesInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleMoneyTopExpenses(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "money_series",
		Description: "Bucketed trend for a window: one row per day, week (Monday-start, keyed by Monday date) or month. Required: start, end, bucket day|week|month. Optional: category, account (single; narrow values, not the grid — empty buckets stay; an unknown name matches no rows, so every bucket reads zero — not an error; blank/whitespace-only means absent, unfiltered), order asc|desc default asc (order only — there is no sort_by; a sort_by argument is ignored, not applied). Order is validated in the handler only (no schema enum) and matches case-insensitively after trimming; blank means the defaults (no schema enum rejects it — unlike query_transactions, where a blank must be omitted). Zero-filled: buckets with no rows appear as zeros. From/to clip to the window; days counts clipped days. Day grids over 500 buckets are rejected (use week/month). Carries no period totals — that is money_totals' answer."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in moneySeriesInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleMoneySeries(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "money_compare",
		Description: "Compare two windows A and B (overlap and unequal lengths allowed). Required: start_a, end_a, start_b, end_b, by=totals|categories. by=totals returns a fixed three-row table (income, expense, net) with A/B paise, delta (B minus A), delta_pct (null when A is zero and the delta moved; 0.00 when both sides are zero) and direction. by=categories attributes the change per expense category with shares and counts, ordered by absolute delta desc by default (ties alphabetical either way — desc flips the value compare, never the name order). Sort args (categories mode: delta|name|b_amount, default delta, order asc|desc default desc; totals mode has a fixed row order) are validated in the handler only (no schema enum) and match case-insensitively after trimming; blank means the defaults (no schema enum rejects it — unlike query_transactions, where a blank must be omitted). Categories mode takes optional categories[] (row picker) and singular account scope; totals mode takes singular category/account (blank/whitespace-only means absent, unfiltered) and rejects non-blank sort args and non-blank categories[] (fixed income, expense, net row order — whitespace-only counts as absent, like everywhere else; unlike money_series, which silently ignores a stray sort_by). Each name matches rows carrying it: an all-unknown filter reports zeros/empty for that side, while a mixed one still attributes the known names' rows — never an error. Per-period validation errors name the period."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in moneyCompareInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleMoneyCompare(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "money_recurring",
		Description: "Detect repeating money movements (subscriptions, salary) in a window. Groups income+expense rows by category+type and clusters near-equal amounts (amount_tolerance_pct 0..25 default 0 = exact; tolerance is ±percent around the cluster median). Only clusters with count >= min_occurrences (2..24 default 3) become candidates. Each candidate carries cadence (weekly/fortnightly/monthly/quarterly from median interval, else irregular), jitter, confidence (jitter-based), next_expected_date (null when irregular), and account summary — never occurrence rows (fetch those via query_transactions by id). Optional category/account narrow the rows; an unknown name matches nothing (empty candidates), not an error; blank/whitespace-only means absent, unfiltered. Bounded by limit (1..50 default 10) with total_candidates/truncated. Sort: sort_by next|amount|count default next, order asc|desc default asc (fixed sets, validated in the handler only — no schema enum; case-insensitive after trimming; ties alphabetical either way — desc flips the value compare, never the name order, and next keeps nulls last); blank means the defaults (no schema enum rejects it — unlike query_transactions, where a blank must be omitted)). Transfers are never detected."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in moneyRecurringInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleMoneyRecurring(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "money_anomalies",
		Description: "Flag the unusual: scope=bucket finds outlier days/weeks/months (bucket day|week|month required in this scope) against the median bucket (zero-spend buckets are never below-outliers — a quiet day is normal, not anomalous); scope=row finds single expense rows that dwarf their category median (with row ids for edit/delete). Required: start, end, scope bucket|row. Optional: category, account (single; an unknown name matches nothing — empty result, not an error; blank/whitespace-only means absent, unfiltered), sensitivity low|medium|high default medium (ratios 2.00/1.50/1.25), min_amount_paise default 10000 (floor on the delta), sort_by/order (bucket scope: delta|expense default delta desc; row scope: amount|ratio default amount desc; bucket is forbidden when scope=row). Sort args are validated in the handler only (no schema enum — the valid sets depend on the scope) and match case-insensitively after trimming; blank means the defaults (no schema enum rejects it — unlike query_transactions, where a blank must be omitted). Income and transfers never appear as row outliers. Too little history is an insufficient baseline, not an error."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in moneyAnomaliesInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleMoneyAnomalies(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})
}

// registerBudgetTools attaches budget_status, list_budgets and the three
// budget writes. Writes prompt (create/update via their own ask patterns,
// delete via *delete*); reads run free.
func registerBudgetTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "budget_status",
		Description: "Spend vs budget for a window: one row per category with prorated limit, spend, remaining, used_pct, over_budget, plus current-month projection when the window overlaps it. Required: start, end. Optional: categories[] (row picker; each name picks rows carrying it — an all-unknown filter returns the empty table while a mixed one still returns the known categories' rows — not an error; blank/whitespace-only entries count as absent), sort_by remaining|used|spend|limit|name default remaining, order asc|desc default asc. Sort args use a fixed set, validated in the handler only (no schema enum) and match case-insensitively after trimming; blank means the defaults (no schema enum rejects it — unlike query_transactions, where a blank must be omitted). The footer totals budgeted rows only (total_limit/spend/remaining/used/over_budget) — unbudgeted categories share the table with budgeted:false and their spend is reported separately as total_unbudgeted_spend_paise, so adding a spend column with unbudgeted rows present exceeds total_spend. Partial months prorate per covered day. Unbudgeted categories share the table with budgeted:false. Income and transfers never consume a budget, and salary spend is excluded from the table entirely — requesting salary returns the empty table, like an unknown name. Projection uses the whole calendar month's spend (projection_basis calendar_month)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in budgetStatusInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleBudgetStatus(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_budgets",
		Description: "List budget rows ordered by category ascending then month descending, with effective_now flagging at most one row per category as the row in force for the current IST month (zero flagged when the category has only future rows). Optional filters: month_from, month_to (YYYY-MM, open-ended; blank/whitespace-only means absent), category (blank/whitespace-only means all budgets; a name no row can carry — unknown, or salary which can never be budgeted — matches nothing and returns the empty list, not an error). No budgets is an empty success."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listBudgetsInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleListBudgets(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "create_budget",
		Description: "Create a monthly category budget (dated limit: applies from its month until a newer row supersedes it). Required: category (not salary), month YYYY-MM (future months allowed), monthly_limit_paise integer > 0. A duplicate category+month is rejected naming the existing id — change limits with update_budget."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createBudgetInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleCreateBudget(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_budget",
		Description: "Change one budget row's limit in place (category and month are identity — fix a typo there with delete + create). Required: id, monthly_limit_paise. A no-op update is rejected; updatedAt moves past createdAt."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateBudgetInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleUpdateBudget(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_budget",
		Description: "Delete one budget version. dry_run=true (default) reports the row and what would govern its month afterwards (falls_back_to_month/limit, or unbudgeted:true when the deleted month would have no budget — later surviving versions still govern their own months); nothing is deleted. Pass dry_run=false to delete. Unknown ids are rejected in both modes."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteBudgetInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := handleDeleteBudget(ctx, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})
}
