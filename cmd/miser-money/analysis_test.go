package main

import (
	"strings"
	"testing"
)

// Validation wiring for the analysis and budget tools: every rejection below
// happens before any store touch (`store` stays nil), proving the handler
// enforces it rather than relying on the database to fail.

func TestMoneyTotalsRejectsBadWindow(t *testing.T) {
	_, errMsg := callTool(t, "money_totals", map[string]any{
		"start": "", "end": "2026-03-31",
	})
	if errMsg == "" {
		t.Fatal("empty start was accepted; expected the window rejection")
	}
	if !strings.Contains(errMsg, "money_totals") {
		t.Errorf("error %q does not name the tool", errMsg)
	}
}

func TestMoneyTotalsRejectsUnknownCategory(t *testing.T) {
	_, errMsg := callTool(t, "money_totals", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "category": "grocerys",
	})
	if errMsg == "" {
		t.Fatal("unknown category was accepted; expected a rejection, not a silent zero")
	}
	if !strings.Contains(errMsg, "grocerys") {
		t.Errorf("error %q does not name the offending category", errMsg)
	}
}

func TestMoneyBreakdownRejectsBadMode(t *testing.T) {
	for _, args := range []map[string]any{
		{"start": "2026-03-01", "end": "2026-03-31", "by": ""},
		{"start": "2026-03-01", "end": "2026-03-31", "by": "bucket"},
	} {
		_, errMsg := callTool(t, "money_breakdown", args)
		if errMsg == "" {
			t.Fatalf("by=%v was accepted; expected a rejection", args["by"])
		}
		if !strings.Contains(errMsg, "money_breakdown") {
			t.Errorf("error %q does not name the tool", errMsg)
		}
	}
}

func TestMoneyBreakdownRequiresSideInCategoryMode(t *testing.T) {
	_, errMsg := callTool(t, "money_breakdown", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "by": "category",
	})
	if errMsg == "" {
		t.Fatal("category mode without side was accepted; expected the side requirement")
	}
	if !strings.Contains(errMsg, "side") {
		t.Errorf("error %q does not name the missing side", errMsg)
	}
}

func TestMoneyBreakdownRejectsUnknownPluralCategory(t *testing.T) {
	_, errMsg := callTool(t, "money_breakdown", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "by": "category",
		"side": "expense", "categories": []string{"groceries", "grocerys"},
	})
	if errMsg == "" {
		t.Fatal("unknown plural category was accepted; the whole call must fail")
	}
	if !strings.Contains(errMsg, "grocerys") {
		t.Errorf("error %q does not name the offending element", errMsg)
	}
}

func TestMoneyBreakdownRejectsBadSort(t *testing.T) {
	_, errMsg := callTool(t, "money_breakdown", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "by": "category",
		"side": "expense", "sort_by": "colour",
	})
	if errMsg == "" {
		t.Fatal("unknown sort_by was accepted; expected the enum error")
	}
	_, errMsg = callTool(t, "money_breakdown", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "by": "category",
		"side": "expense", "order": "sideways",
	})
	if errMsg == "" {
		t.Fatal("unknown order was accepted; expected the enum error")
	}
}

func TestMoneyTopExpensesRejectsBadLimit(t *testing.T) {
	for _, limit := range []int{0, 21} {
		_, errMsg := callTool(t, "money_top_expenses", map[string]any{
			"start": "2026-03-01", "end": "2026-03-31", "limit": limit,
		})
		if errMsg == "" {
			t.Fatalf("limit=%d was accepted; expected the 1..20 error", limit)
		}
		if !strings.Contains(errMsg, "money_top_expenses") {
			t.Errorf("error %q does not name the tool", errMsg)
		}
	}
}

func TestMoneySeriesRejectsBadBucket(t *testing.T) {
	for _, bucket := range []string{"", "quarter"} {
		_, errMsg := callTool(t, "money_series", map[string]any{
			"start": "2026-03-01", "end": "2026-03-31", "bucket": bucket,
		})
		if errMsg == "" {
			t.Fatalf("bucket=%q was accepted; expected a rejection", bucket)
		}
	}
}

func TestMoneyCompareNamesItsPeriod(t *testing.T) {
	_, errMsg := callTool(t, "money_compare", map[string]any{
		"start_a": "2026-03-31", "end_a": "2026-03-01",
		"start_b": "2026-03-01", "end_b": "2026-03-31", "by": "totals",
	})
	if errMsg == "" {
		t.Fatal("reversed period A was accepted; expected a period-A error")
	}
	if !strings.Contains(errMsg, "period A") {
		t.Errorf("error %q does not name period A", errMsg)
	}
}

func TestMoneyCompareRejectsModeMixups(t *testing.T) {
	_, errMsg := callTool(t, "money_compare", map[string]any{
		"start_a": "2026-02-01", "end_a": "2026-02-28",
		"start_b": "2026-03-01", "end_b": "2026-03-31",
		"by":      "totals", "sort_by": "delta",
	})
	if errMsg == "" {
		t.Fatal("sort_by in totals mode was accepted; the order is fixed")
	}
	_, errMsg = callTool(t, "money_compare", map[string]any{
		"start_a": "2026-02-01", "end_a": "2026-02-28",
		"start_b": "2026-03-01", "end_b": "2026-03-31",
		"by": "categories", "category": "groceries",
	})
	if errMsg == "" {
		t.Fatal("singular category in categories mode was accepted; use categories[]")
	}
}

func TestMoneyRecurringRejectsBadInputs(t *testing.T) {
	cases := []map[string]any{
		{"start": "2026-01-01", "end": "2026-06-30", "min_occurrences": 1},
		{"start": "2026-01-01", "end": "2026-06-30", "amount_tolerance_pct": 26},
		{"start": "2026-01-01", "end": "2026-06-30", "limit": 0},
	}
	for _, args := range cases {
		_, errMsg := callTool(t, "money_recurring", args)
		if errMsg == "" {
			t.Fatalf("%v was accepted; expected a rejection", args)
		}
		if !strings.Contains(errMsg, "money_recurring") {
			t.Errorf("error %q does not name the tool", errMsg)
		}
	}
}

func TestMoneyAnomaliesRejectsBadScope(t *testing.T) {
	_, errMsg := callTool(t, "money_anomalies", map[string]any{
		"start": "2026-01-01", "end": "2026-06-30", "scope": "",
	})
	if errMsg == "" {
		t.Fatal("empty scope was accepted; expected the scope requirement")
	}
	_, errMsg = callTool(t, "money_anomalies", map[string]any{
		"start": "2026-01-01", "end": "2026-06-30", "scope": "bucket",
	})
	if errMsg == "" {
		t.Fatal("bucket scope without bucket was accepted; expected the bucket requirement")
	}
	_, errMsg = callTool(t, "money_anomalies", map[string]any{
		"start": "2026-01-01", "end": "2026-06-30", "scope": "row", "bucket": "month",
	})
	if errMsg == "" {
		t.Fatal("bucket with row scope was accepted; expected a rejection")
	}
	_, errMsg = callTool(t, "money_anomalies", map[string]any{
		"start": "2026-01-01", "end": "2026-06-30", "scope": "row",
		"sensitivity": "max",
	})
	if errMsg == "" {
		t.Fatal("bad sensitivity was accepted; expected the enum error")
	}
	_, errMsg = callTool(t, "money_anomalies", map[string]any{
		"start": "2026-01-01", "end": "2026-06-30", "scope": "row",
		"min_amount_paise": -1,
	})
	if errMsg == "" {
		t.Fatal("negative min_amount_paise was accepted; expected a rejection")
	}
}

func TestBudgetStatusRejectsBadInputs(t *testing.T) {
	_, errMsg := callTool(t, "budget_status", map[string]any{
		"start": "", "end": "2026-03-31",
	})
	if errMsg == "" {
		t.Fatal("empty start was accepted; expected the window rejection")
	}
	_, errMsg = callTool(t, "budget_status", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "sort_by": "colour",
	})
	if errMsg == "" {
		t.Fatal("unknown sort_by was accepted; expected the enum error")
	}
	_, errMsg = callTool(t, "budget_status", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "categories": []string{"grocerys"},
	})
	if errMsg == "" {
		t.Fatal("unknown category was accepted; expected a rejection naming it")
	}
}

func TestCreateBudgetRejectsBadInputs(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"salary", map[string]any{"category": "salary", "month": "2026-01", "monthly_limit": 5000.0}, "salary"},
		{"month", map[string]any{"category": "groceries", "month": "2026-13", "monthly_limit": 5000.0}, "month"},
		{"limit", map[string]any{"category": "groceries", "month": "2026-01", "monthly_limit": 0.0}, "monthly_limit"},
		{"paise", map[string]any{"category": "groceries", "month": "2026-01", "monthly_limit": 10.001}, "exact paise"},
	}
	for _, c := range cases {
		_, errMsg := callTool(t, "create_budget", c.args)
		if errMsg == "" {
			t.Fatalf("%s was accepted; expected a rejection", c.name)
		}
		if !strings.Contains(errMsg, c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, errMsg, c.want)
		}
	}
}

func TestUpdateBudgetRejectsBadInputs(t *testing.T) {
	_, errMsg := callTool(t, "update_budget", map[string]any{
		"id": "", "monthly_limit": 100.0,
	})
	if errMsg == "" {
		t.Fatal("empty id was accepted; expected the id requirement")
	}
	_, errMsg = callTool(t, "update_budget", map[string]any{
		"id": "000000000000000000000001", "monthly_limit": 9.999,
	})
	if errMsg == "" {
		t.Fatal("inexact limit was accepted; expected the paise error")
	}
	if !strings.Contains(errMsg, "exact paise") {
		t.Errorf("error %q does not demand exact paise", errMsg)
	}
}

func TestDeleteBudgetRejectsEmptyID(t *testing.T) {
	_, errMsg := callTool(t, "delete_budget", map[string]any{"id": ""})
	if errMsg == "" {
		t.Fatal("empty id was accepted; expected the id requirement")
	}
	if !strings.Contains(errMsg, "delete_budget") {
		t.Errorf("error %q does not name the tool", errMsg)
	}
}

func TestListBudgetsRejectsReversedRange(t *testing.T) {
	_, errMsg := callTool(t, "list_budgets", map[string]any{
		"month_from": "2026-03", "month_to": "2026-01",
	})
	if errMsg == "" {
		t.Fatal("month_from after month_to was accepted; expected a rejection")
	}
	_, errMsg = callTool(t, "list_budgets", map[string]any{"category": "salary"})
	if errMsg == "" {
		t.Fatal("salary was accepted; income cannot be budgeted")
	}
}
