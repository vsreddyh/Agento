package main

import (
	"context"
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

// An unknown category/account filter is not a typo rejection on a read: no
// row can carry it, so the window reports zeros — the empty answer (#619).
// Typo protection lives on the write path (log/edit still reject).
func TestMoneyTotalsUnknownFilterReportsZeros(t *testing.T) {
	withTestStore(t)
	for _, args := range []map[string]any{
		{"start": "2026-03-01", "end": "2026-03-31", "category": "grocerys"},
		{"start": "2026-03-01", "end": "2026-03-31", "account": "no-such-account"},
	} {
		text, errMsg := callTool(t, "money_totals", args)
		if errMsg != "" {
			t.Fatalf("unknown filter %v was rejected: %s", args, errMsg)
		}
		if !strings.Contains(text, `"ok":true`) {
			t.Fatalf("unknown filter %v did not return ok:true: %q", args, text)
		}
		if !strings.Contains(text, `"rows_total":0`) {
			t.Errorf("unknown filter %v did not report zeros: %q", args, text)
		}
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

func TestMoneyBreakdownUnknownPluralCategoryReturnsEmpty(t *testing.T) {
	withTestStore(t)
	// Seed one known-category row: withTestStore leaves the ledger empty.
	if _, _, err := store.Insert(context.Background(), "2026-03-05", 10000, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	// All-unknown: nothing can match, so the table comes back empty — but
	// still ok:true, not a rejection.
	text, errMsg := callTool(t, "money_breakdown", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "by": "category",
		"side": "expense", "categories": []string{"grocerys"},
	})
	if errMsg != "" {
		t.Fatalf("unknown plural category was rejected: %s", errMsg)
	}
	if !strings.Contains(text, `"ok":true`) || !strings.Contains(text, `"categories":[]`) {
		t.Fatalf("all-unknown filter did not return the empty table: %q", text)
	}
	// Mixed: the unknown name contributes no rows, the known name still
	// matches — partial results, no error.
	text, errMsg = callTool(t, "money_breakdown", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "by": "category",
		"side": "expense", "categories": []string{"groceries", "grocerys"},
	})
	if errMsg != "" {
		t.Fatalf("mixed plural categories were rejected: %s", errMsg)
	}
	if !strings.Contains(text, `"groceries"`) {
		t.Fatalf("mixed filter dropped the known category's rows: %q", text)
	}
	// by=account with an all-unknown categories filter still lists every
	// account — categories[] narrows the flow columns there, it is not a row
	// picker — with zero flows and real balances.
	text, errMsg = callTool(t, "money_breakdown", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "by": "account",
		"categories": []string{"grocerys"},
	})
	if errMsg != "" {
		t.Fatalf("account-mode unknown categories were rejected: %s", errMsg)
	}
	if !strings.Contains(text, `"Cash"`) || !strings.Contains(text, `"rows_total":0`) {
		t.Fatalf("account-mode unknown categories did not list zero-flow accounts: %q", text)
	}
	// by=account with an all-unknown accounts picker returns the empty table.
	text, errMsg = callTool(t, "money_breakdown", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "by": "account",
		"accounts": []string{"no-such-account"},
	})
	if errMsg != "" {
		t.Fatalf("account-mode unknown accounts were rejected: %s", errMsg)
	}
	if !strings.Contains(text, `"accounts":[]`) {
		t.Fatalf("account-mode unknown accounts did not return the empty table: %q", text)
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
		"by": "totals", "sort_by": "delta",
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
}

func TestBudgetStatusUnknownCategoryReturnsEmpty(t *testing.T) {
	withTestStore(t)
	text, errMsg := callTool(t, "budget_status", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "categories": []string{"grocerys"},
	})
	if errMsg != "" {
		t.Fatalf("unknown category was rejected: %s", errMsg)
	}
	if !strings.Contains(text, `"ok":true`) {
		t.Fatalf("unknown category did not return ok:true: %q", text)
	}
}

func TestCreateBudgetRejectsBadInputs(t *testing.T) {
	withTestStore(t)
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"salary", map[string]any{"category": "salary", "month": "2026-01", "monthly_limit_paise": 500000}, "salary"},
		{"month", map[string]any{"category": "groceries", "month": "2026-13", "monthly_limit_paise": 500000}, "month"},
		{"limit", map[string]any{"category": "groceries", "month": "2026-01", "monthly_limit_paise": 0}, "monthly_limit_paise"},
		{"negative", map[string]any{"category": "groceries", "month": "2026-01", "monthly_limit_paise": -5}, "monthly_limit_paise"},
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
		"id": "", "monthly_limit_paise": 10000,
	})
	if errMsg == "" {
		t.Fatal("empty id was accepted; expected the id requirement")
	}
	_, errMsg = callTool(t, "update_budget", map[string]any{
		"id": "000000000000000000000001", "monthly_limit_paise": -5,
	})
	if errMsg == "" {
		t.Fatal("negative limit was accepted; expected the paise error")
	}
	if !strings.Contains(errMsg, "monthly_limit_paise") {
		t.Errorf("error %q does not name monthly_limit_paise", errMsg)
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
	// Malformed hex fails in id parsing before any store touch.
	_, errMsg = callTool(t, "delete_budget", map[string]any{"id": "nope"})
	if errMsg == "" {
		t.Fatal("malformed id was accepted; expected a rejection, not a silent no-op")
	}
}

func TestListBudgetsRejectsReversedRange(t *testing.T) {
	_, errMsg := callTool(t, "list_budgets", map[string]any{
		"month_from": "2026-03", "month_to": "2026-01",
	})
	if errMsg == "" {
		t.Fatal("month_from after month_to was accepted; expected a rejection")
	}
}

// A read filter, not a write: a name no budget row can carry (salary, which
// can never be budgeted, or an unknown name) matches nothing and returns the
// empty list — not a rejection (#619). The salary guard lives on the write
// path (create/update_budget still reject it).
func TestListBudgetsUnknownCategoryReturnsEmpty(t *testing.T) {
	withTestStore(t)
	for _, cat := range []string{"salary", "grocerys"} {
		text, errMsg := callTool(t, "list_budgets", map[string]any{"category": cat})
		if errMsg != "" {
			t.Fatalf("category %q was rejected: %s", cat, errMsg)
		}
		if !strings.Contains(text, `"ok":true`) {
			t.Fatalf("category %q did not return ok:true: %q", cat, text)
		}
	}
}
