package main

import (
	"context"
	"fmt"
	"strings"

	"agento/internal/money"
)

// This file holds the input types and handlers for the seven read-only
// money analysis tools. The AddTool calls live in tools.go
// (registerAnalysisTools, called from main.go's registerTools) — handlers
// here, registration there. Input types are named (not inline) so
// schema_test.go can infer the published schema from the same types AddTool
// publishes. The permission test enumerates registrations across every
// non-test Go file in each cmd dir, so a tool registered in a new file is
// still classified.

// normalizeAccountList trims, drops blanks and dedupes an account filter.
// Accounts are case-sensitive exact names (unlike categories), so values are
// never lowercased. Existence is not checked: an unknown name resolves to the
// nil id downstream and matches no row, so the read returns the empty table
// (#619) instead of a rejection.
func normalizeAccountList(tool string, in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		t := strings.TrimSpace(v)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func nilIfNilI64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nilIfNilF64(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nilIfNilBool(v *bool) any {
	if v == nil {
		return nil
	}
	return *v
}

type moneyTotalsInput struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Category string `json:"category,omitempty"`
	Account  string `json:"account,omitempty"`
}

func handleMoneyTotals(ctx context.Context, in moneyTotalsInput) (map[string]any, error) {
	start, end, err := normalizeDayRange("money_totals", in.Start, in.End)
	if err != nil {
		return nil, err
	}
	cat := money.NormalizeCategory(in.Category)
	var catP, acctP *string
	if cat != "" {
		catP = &cat
	}
	if a := strings.TrimSpace(in.Account); a != "" {
		acctP = &a
	}
	t, err := store.PeriodTotals(ctx, start, end, catP, acctP)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true,
		"start": start, "end": end, "days": t.Days,
		"category": nilIfEmpty(cat), "account": nilIfEmpty(in.Account),
		"rows_total": t.RowsTotal, "rows_income": t.RowsIncome,
		"rows_expense": t.RowsExpense, "rows_transfer": t.RowsTransfer,
		"income_paise": t.IncomePaise, "expense_paise": t.ExpensePaise,
		"net_paise": t.NetPaise, "savings_rate_pct": nilIfNilF64(t.SavingsRatePct),
		"avg_daily_expense_paise": t.AvgDailyExpensePaise,
		"avg_daily_income_paise":  t.AvgDailyIncomePaise,
		"transfer_count":          t.TransferCount,
		"transfer_volume_paise":   t.TransferVolumePaise,
	}, nil
}

type moneyBreakdownInput struct {
	Start      string   `json:"start"`
	End        string   `json:"end"`
	By         string   `json:"by"`
	Side       string   `json:"side,omitempty"`
	Categories []string `json:"categories,omitempty"`
	Accounts   []string `json:"accounts,omitempty"`
	SortBy     string   `json:"sort_by,omitempty"`
	Order      string   `json:"order,omitempty"`
}

func handleMoneyBreakdown(ctx context.Context, in moneyBreakdownInput) (map[string]any, error) {
	start, end, err := normalizeDayRange("money_breakdown", in.Start, in.End)
	if err != nil {
		return nil, err
	}
	by := strings.ToLower(strings.TrimSpace(in.By))
	if by == "" {
		return nil, fmt.Errorf("money_breakdown by is required: category or account")
	}
	if by != "category" && by != "account" {
		return nil, fmt.Errorf("money_breakdown by must be category or account — got %q (trends live in money_series)", in.By)
	}
	cats := money.NormalizeCategories(in.Categories)
	accts := normalizeAccountList("money_breakdown", in.Accounts)
	if by == "category" {
		side, err := money.NormalizeSide("money_breakdown", in.Side)
		if err != nil {
			return nil, err
		}
		sortBy, err := money.NormalizeSort("money_breakdown", in.SortBy, "amount", []string{"amount", "name", "count"})
		if err != nil {
			return nil, err
		}
		order, err := money.NormalizeOrder("money_breakdown", in.Order, "desc")
		if err != nil {
			return nil, err
		}
		res, err := store.BreakdownByCategory(ctx, start, end, side, cats, accts, sortBy, order)
		if err != nil {
			return nil, err
		}
		rows := make([]any, 0, len(res.Rows))
		for _, g := range res.Rows {
			rows = append(rows, map[string]any{
				"category": g.Category, "amount_paise": g.AmountPaise,
				"count": g.Count, "share_pct": g.SharePct, "avg_paise": g.AvgPaise,
			})
		}
		return map[string]any{"ok": true,
			"start": start, "end": end, "days": resDays(start, end),
			"by": "category", "side": side,
			"categories_applied": res.CategoriesApplied, "accounts_applied": res.AccountsApplied,
			"sort_by": res.SortBy, "order": res.Order,
			"categories":         rows,
			"total_amount_paise": res.TotalAmountPaise, "total_count": res.TotalCount,
		}, nil
	}
	// by=account: side is accepted and ignored — the table covers all legs.
	sortBy, err := money.NormalizeSort("money_breakdown", in.SortBy, "closing", []string{"closing", "name", "net", "inflows", "outflows"})
	if err != nil {
		return nil, err
	}
	order, err := money.NormalizeOrder("money_breakdown", in.Order, "desc")
	if err != nil {
		return nil, err
	}
	res, err := store.AccountFlow(ctx, start, end, cats, accts, sortBy, order)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(res.Rows))
	for _, a := range res.Rows {
		rows = append(rows, map[string]any{
			"id": a.ID, "name": a.Name, "archived": a.Archived,
			"opening_balance_paise": a.OpeningPaise,
			"inflows_paise":         a.InflowsPaise, "outflows_paise": a.OutflowsPaise,
			"net_paise": a.NetPaise, "closing_balance_paise": a.ClosingPaise,
			"income_paise": a.IncomePaise, "expense_paise": a.ExpensePaise,
			"transfer_in_paise": a.TransferInPaise, "transfer_out_paise": a.TransferOutPaise,
			"rows_total": a.RowsTotal,
		})
	}
	return map[string]any{"ok": true,
		"start": start, "end": end, "days": resDays(start, end),
		"by":                 "account",
		"categories_applied": res.CategoriesApplied, "accounts_applied": res.AccountsApplied,
		"sort_by": res.SortBy, "order": res.Order,
		"accounts":                    rows,
		"total_opening_paise":         res.TotalOpeningPaise,
		"total_inflows_paise":         res.TotalInflowsPaise,
		"total_outflows_paise":        res.TotalOutflowsPaise,
		"total_net_paise":             res.TotalNetPaise,
		"total_closing_paise":         res.TotalClosingPaise,
		"total_active_closing_paise":  res.TotalActiveClosingPaise,
		"total_income_paise":          res.TotalIncomePaise,
		"total_expense_paise":         res.TotalExpensePaise,
		"total_transfer_volume_paise": res.TotalTransferVolumePaise,
	}, nil
}

func resDays(start, end string) int {
	d, err := money.DaysBetween(start, end)
	if err != nil {
		return 0
	}
	return d
}

type moneyTopExpensesInput struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Category string `json:"category,omitempty"`
	Account  string `json:"account,omitempty"`
	Limit    *int   `json:"limit,omitempty"`
	SortBy   string `json:"sort_by,omitempty"`
	Order    string `json:"order,omitempty"`
}

func handleMoneyTopExpenses(ctx context.Context, in moneyTopExpensesInput) (map[string]any, error) {
	start, end, err := normalizeDayRange("money_top_expenses", in.Start, in.End)
	if err != nil {
		return nil, err
	}
	cat := money.NormalizeCategory(in.Category)
	var catP, acctP *string
	if cat != "" {
		catP = &cat
	}
	if a := strings.TrimSpace(in.Account); a != "" {
		acctP = &a
	}
	limit := 5
	if in.Limit != nil {
		limit = *in.Limit
	}
	if limit < 1 || limit > 20 {
		return nil, fmt.Errorf("money_top_expenses limit must be between 1 and 20 — got %d", limit)
	}
	sortBy, err := money.NormalizeSort("money_top_expenses", in.SortBy, "amount", []string{"amount", "date"})
	if err != nil {
		return nil, err
	}
	order, err := money.NormalizeOrder("money_top_expenses", in.Order, "desc")
	if err != nil {
		return nil, err
	}
	res, err := store.TopExpenses(ctx, start, end, catP, acctP, limit, sortBy, order)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		rows = append(rows, map[string]any{
			"id": r.ID, "date": r.Date, "amount_paise": r.AmountPaise,
			"category": r.Category, "account": r.Account, "note": r.Note,
		})
	}
	return map[string]any{"ok": true,
		"start": start, "end": end, "days": resDays(start, end),
		"category": nilIfEmpty(cat), "account": nilIfEmpty(in.Account),
		"limit_applied": res.LimitApplied, "total_matching": res.TotalMatching,
		"sort_by": res.SortBy, "order": res.Order,
		"expenses": rows,
	}, nil
}

type moneySeriesInput struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Bucket   string `json:"bucket"`
	Category string `json:"category,omitempty"`
	Account  string `json:"account,omitempty"`
	Order    string `json:"order,omitempty"`
}

func handleMoneySeries(ctx context.Context, in moneySeriesInput) (map[string]any, error) {
	start, end, err := normalizeDayRange("money_series", in.Start, in.End)
	if err != nil {
		return nil, err
	}
	bucket, err := money.NormalizeBucket("money_series", in.Bucket)
	if err != nil {
		return nil, err
	}
	cat := money.NormalizeCategory(in.Category)
	var catP, acctP *string
	if cat != "" {
		catP = &cat
	}
	if a := strings.TrimSpace(in.Account); a != "" {
		acctP = &a
	}
	order, err := money.NormalizeOrder("money_series", in.Order, "asc")
	if err != nil {
		return nil, err
	}
	buckets, err := store.Series(ctx, start, end, bucket, catP, acctP, order)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(buckets))
	for _, b := range buckets {
		rows = append(rows, map[string]any{
			"key": b.Key, "from": b.From, "to": b.To, "days": b.Days,
			"rows_total": b.RowsTotal, "rows_income": b.RowsIncome,
			"rows_expense": b.RowsExpense, "rows_transfer": b.RowsTransfer,
			"income_paise": b.IncomePaise, "expense_paise": b.ExpensePaise,
			"net_paise": b.NetPaise, "avg_daily_expense_paise": b.AvgDailyExpensePaise,
			"transfer_count": b.TransferCount, "transfer_volume_paise": b.TransferVolumePaise,
		})
	}
	return map[string]any{"ok": true,
		"start": start, "end": end, "days": resDays(start, end),
		"bucket": bucket, "bucket_count": len(buckets), "order": order,
		"category": nilIfEmpty(cat), "account": nilIfEmpty(in.Account),
		"buckets": rows,
	}, nil
}

type moneyCompareInput struct {
	StartA     string   `json:"start_a"`
	EndA       string   `json:"end_a"`
	StartB     string   `json:"start_b"`
	EndB       string   `json:"end_b"`
	By         string   `json:"by"`
	Category   string   `json:"category,omitempty"`
	Categories []string `json:"categories,omitempty"`
	Account    string   `json:"account,omitempty"`
	SortBy     string   `json:"sort_by,omitempty"`
	Order      string   `json:"order,omitempty"`
}

func handleMoneyCompare(ctx context.Context, in moneyCompareInput) (map[string]any, error) {
	startA, endA, err := normalizeDayRange("money_compare period A", in.StartA, in.EndA)
	if err != nil {
		return nil, err
	}
	startB, endB, err := normalizeDayRange("money_compare period B", in.StartB, in.EndB)
	if err != nil {
		return nil, err
	}
	by := strings.ToLower(strings.TrimSpace(in.By))
	if by == "" {
		return nil, fmt.Errorf("money_compare by is required: totals or categories")
	}
	if by != "totals" && by != "categories" {
		return nil, fmt.Errorf("money_compare by must be totals or categories — got %q", in.By)
	}
	var acctP *string
	if a := strings.TrimSpace(in.Account); a != "" {
		acctP = &a
	}
	base := map[string]any{"ok": true,
		"start_a": startA, "end_a": endA, "days_a": resDays(startA, endA),
		"start_b": startB, "end_b": endB, "days_b": resDays(startB, endB),
		"by": by,
	}
	if by == "totals" {
		if len(in.Categories) > 0 {
			return nil, fmt.Errorf("money_compare categories[] is only valid when by=categories — pass one category or switch mode")
		}
		if strings.TrimSpace(in.SortBy) != "" || strings.TrimSpace(in.Order) != "" {
			return nil, fmt.Errorf("money_compare totals mode has a fixed row order (income, expense, net) — no sort arguments")
		}
		cat := money.NormalizeCategory(in.Category)
		var catP *string
		if cat != "" {
			catP = &cat
		}
		deltas, err := store.CompareTotals(ctx, startA, endA, startB, endB, catP, acctP)
		if err != nil {
			return nil, err
		}
		rows := make([]any, 0, len(deltas))
		for _, d := range deltas {
			rows = append(rows, map[string]any{
				"metric": d.Metric, "a_paise": d.APaise, "b_paise": d.BPaise,
				"delta_paise": d.DeltaPaise, "delta_pct": nilIfNilF64(d.DeltaPct),
				"direction": d.Direction,
			})
		}
		base["metrics"] = rows
		return base, nil
	}
	if strings.TrimSpace(in.Category) != "" {
		return nil, fmt.Errorf("money_compare category is only valid when by=totals — use categories[] in categories mode")
	}
	cats := money.NormalizeCategories(in.Categories)
	sortBy, err := money.NormalizeSort("money_compare", in.SortBy, "delta", []string{"delta", "name", "b_amount"})
	if err != nil {
		return nil, err
	}
	order, err := money.NormalizeOrder("money_compare", in.Order, "desc")
	if err != nil {
		return nil, err
	}
	deltas, err := store.CompareCategories(ctx, startA, endA, startB, endB, cats, acctP, sortBy, order)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(deltas))
	for _, d := range deltas {
		rows = append(rows, map[string]any{
			"category": d.Category, "a_paise": d.APaise, "b_paise": d.BPaise,
			"delta_paise": d.DeltaPaise, "delta_pct": nilIfNilF64(d.DeltaPct),
			"direction": d.Direction, "a_share_pct": d.ASharePct, "b_share_pct": d.BSharePct,
			"a_count": d.ACount, "b_count": d.BCount,
		})
	}
	base["categories_applied"] = cats
	if acctP != nil {
		base["account"] = *acctP
	} else {
		base["account"] = nil
	}
	base["sort_by"] = sortBy
	base["order"] = order
	base["categories"] = rows
	return base, nil
}

type moneyRecurringInput struct {
	Start              string  `json:"start"`
	End                string  `json:"end"`
	Category           string  `json:"category,omitempty"`
	Account            string  `json:"account,omitempty"`
	MinOccurrences     *int    `json:"min_occurrences,omitempty"`
	AmountTolerancePct float64 `json:"amount_tolerance_pct,omitempty"`
	Limit              *int    `json:"limit,omitempty"`
	SortBy             string  `json:"sort_by,omitempty"`
	Order              string  `json:"order,omitempty"`
}

func handleMoneyRecurring(ctx context.Context, in moneyRecurringInput) (map[string]any, error) {
	start, end, err := normalizeDayRange("money_recurring", in.Start, in.End)
	if err != nil {
		return nil, err
	}
	cat := money.NormalizeCategory(in.Category)
	var catP, acctP *string
	if cat != "" {
		catP = &cat
	}
	if a := strings.TrimSpace(in.Account); a != "" {
		acctP = &a
	}
	minOcc := 3
	if in.MinOccurrences != nil {
		minOcc = *in.MinOccurrences
	}
	if minOcc < 2 || minOcc > 24 {
		return nil, fmt.Errorf("money_recurring min_occurrences must be between 2 and 24 — got %d", minOcc)
	}
	if in.AmountTolerancePct < 0 || in.AmountTolerancePct > 25 {
		return nil, fmt.Errorf("money_recurring amount_tolerance_pct must be between 0 and 25 — got %v", in.AmountTolerancePct)
	}
	limit := 10
	if in.Limit != nil {
		limit = *in.Limit
	}
	if limit < 1 || limit > 50 {
		return nil, fmt.Errorf("money_recurring limit must be between 1 and 50 — got %d", limit)
	}
	sortBy, err := money.NormalizeSort("money_recurring", in.SortBy, "next", []string{"next", "amount", "count"})
	if err != nil {
		return nil, err
	}
	order, err := money.NormalizeOrder("money_recurring", in.Order, "asc")
	if err != nil {
		return nil, err
	}
	cands, total, err := store.DetectRecurring(ctx, start, end, catP, acctP, minOcc, in.AmountTolerancePct, limit, sortBy, order)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(cands))
	for _, c := range cands {
		rows = append(rows, map[string]any{
			"category": c.Category, "type": c.Type, "amount_paise": c.AmountPaise,
			"count": c.Count, "first_date": c.FirstDate, "last_date": c.LastDate,
			"median_interval_days": c.MedianIntervalDays,
			"interval_jitter_days": c.IntervalJitterDays,
			"cadence":              c.Cadence, "next_expected_date": nilIfEmpty(c.NextExpectedDate),
			"confidence": c.Confidence, "account": c.Account, "account_count": c.AccountCount,
		})
	}
	return map[string]any{"ok": true,
		"start": start, "end": end, "days": resDays(start, end),
		"category": nilIfEmpty(cat), "account": nilIfEmpty(in.Account),
		"min_occurrences": minOcc, "amount_tolerance_pct": in.AmountTolerancePct,
		"limit_applied": limit, "sort_by": sortBy, "order": order,
		"total_candidates": total, "truncated": total > len(cands),
		"candidates": rows,
	}, nil
}

func nilIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.TrimSpace(s)
}

type moneyAnomaliesInput struct {
	Start          string `json:"start"`
	End            string `json:"end"`
	Scope          string `json:"scope"`
	Category       string `json:"category,omitempty"`
	Account        string `json:"account,omitempty"`
	Sensitivity    string `json:"sensitivity,omitempty"`
	MinAmountPaise *int64 `json:"min_amount_paise,omitempty"`
	Bucket         string `json:"bucket,omitempty"`
	SortBy         string `json:"sort_by,omitempty"`
	Order          string `json:"order,omitempty"`
}

func handleMoneyAnomalies(ctx context.Context, in moneyAnomaliesInput) (map[string]any, error) {
	start, end, err := normalizeDayRange("money_anomalies", in.Start, in.End)
	if err != nil {
		return nil, err
	}
	scope := strings.ToLower(strings.TrimSpace(in.Scope))
	if scope == "" {
		return nil, fmt.Errorf("money_anomalies scope is required: bucket or row")
	}
	if scope != "bucket" && scope != "row" {
		return nil, fmt.Errorf("money_anomalies scope must be bucket or row — got %q", in.Scope)
	}
	cat := money.NormalizeCategory(in.Category)
	var catP, acctP *string
	if cat != "" {
		catP = &cat
	}
	if a := strings.TrimSpace(in.Account); a != "" {
		acctP = &a
	}
	sens, err := money.NormalizeSensitivity("money_anomalies", in.Sensitivity)
	if err != nil {
		return nil, err
	}
	minPaise := int64(10000)
	if in.MinAmountPaise != nil {
		minPaise = *in.MinAmountPaise
	}
	if minPaise < 0 {
		return nil, fmt.Errorf("money_anomalies min_amount_paise must be >= 0 — got %d", minPaise)
	}
	base := map[string]any{"ok": true,
		"start": start, "end": end, "days": resDays(start, end),
		"scope": scope, "sensitivity_applied": sens,
		"ratio_applied": money.SensitivityRatio(sens), "min_amount_paise": minPaise,
		"category": nilIfEmpty(cat), "account": nilIfEmpty(in.Account),
	}
	if scope == "bucket" {
		bucket, err := money.NormalizeBucket("money_anomalies", in.Bucket)
		if err != nil {
			return nil, fmt.Errorf("money_anomalies %v", err)
		}
		sortBy, err := money.NormalizeSort("money_anomalies", in.SortBy, "delta", []string{"delta", "expense"})
		if err != nil {
			return nil, err
		}
		order, err := money.NormalizeOrder("money_anomalies", in.Order, "desc")
		if err != nil {
			return nil, err
		}
		outliers, baseline, considered, insufficient, err := store.BucketAnomalies(ctx, start, end, bucket, catP, acctP, sens, minPaise, sortBy, order)
		if err != nil {
			return nil, err
		}
		rows := make([]any, 0, len(outliers))
		for _, o := range outliers {
			rows = append(rows, map[string]any{
				"key": o.Key, "from": o.From, "to": o.To, "days": o.Days,
				"expense_paise": o.ExpensePaise, "median_paise": o.MedianPaise,
				"delta_paise": o.DeltaPaise, "direction": o.Direction,
			})
		}
		base["bucket"] = bucket
		base["sort_by"] = sortBy
		base["order"] = order
		base["baseline_median_paise"] = baseline.ExpensePaise
		base["insufficient_baseline"] = insufficient
		base["buckets_considered"] = considered
		if insufficient {
			base["reason"] = "fewer than 3 buckets in the window — no baseline to compare against"
		} else {
			base["reason"] = nil
		}
		base["outliers"] = rows
		return base, nil
	}
	if strings.TrimSpace(in.Bucket) != "" {
		return nil, fmt.Errorf("money_anomalies bucket is only valid when scope=bucket")
	}
	sortBy, err := money.NormalizeSort("money_anomalies", in.SortBy, "amount", []string{"amount", "ratio"})
	if err != nil {
		return nil, err
	}
	order, err := money.NormalizeOrder("money_anomalies", in.Order, "desc")
	if err != nil {
		return nil, err
	}
	outliers, evaluated, skipped, err := store.RowAnomalies(ctx, start, end, catP, acctP, sens, minPaise, sortBy, order)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(outliers))
	for _, o := range outliers {
		rows = append(rows, map[string]any{
			"id": o.ID, "date": o.Date, "amount_paise": o.AmountPaise,
			"category": o.Category, "account": o.Account, "note": o.Note,
			"category_median_paise": o.CategoryMedianPaise,
			"ratio_pct":             o.RatioPct, "direction": o.Direction,
		})
	}
	base["sort_by"] = sortBy
	base["order"] = order
	base["categories_evaluated"] = evaluated
	base["categories_skipped"] = skipped
	base["outliers"] = rows
	return base, nil
}
