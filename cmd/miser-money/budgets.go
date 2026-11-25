package main

import (
	"context"
	"strings"

	"agento/internal/money"
)

// Budget tool handlers. Writes prompt (create/update via their own `ask`
// patterns, delete via `*delete*`); reads run free.

type createBudgetInput struct {
	Category          string `json:"category"`
	Month             string `json:"month"`
	MonthlyLimitPaise int64  `json:"monthly_limit_paise"`
}

func handleCreateBudget(ctx context.Context, in createBudgetInput) (map[string]any, error) {
	cat, err := store.CheckBudgetCategory(ctx, "create_budget", in.Category)
	if err != nil {
		return nil, err
	}
	mo, err := money.CheckBudgetMonth("create_budget", in.Month)
	if err != nil {
		return nil, err
	}
	if _, err := money.CheckBudgetLimit("create_budget", in.MonthlyLimitPaise); err != nil {
		return nil, err
	}
	b, err := store.CreateBudget(ctx, cat, mo, in.MonthlyLimitPaise)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true,
		"id": b.ID, "category": b.Category, "month": b.Month,
		"monthly_limit_paise": b.MonthlyLimitPaise,
	}, nil
}

type listBudgetsInput struct {
	MonthFrom string `json:"month_from,omitempty"`
	MonthTo   string `json:"month_to,omitempty"`
	Category  string `json:"category,omitempty"`
}

func handleListBudgets(ctx context.Context, in listBudgetsInput) (map[string]any, error) {
	mf := strings.TrimSpace(in.MonthFrom)
	mt := strings.TrimSpace(in.MonthTo)
	if mf != "" {
		if _, err := money.CheckBudgetMonth("list_budgets", mf); err != nil {
			return nil, err
		}
	}
	if mt != "" {
		if _, err := money.CheckBudgetMonth("list_budgets", mt); err != nil {
			return nil, err
		}
	}
	rows, err := store.ListBudgets(ctx, mf, mt, strings.TrimSpace(in.Category))
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, b := range rows {
		out = append(out, map[string]any{
			"id": b.ID, "category": b.Category, "month": b.Month,
			"monthly_limit_paise": b.MonthlyLimitPaise,
			"effective_now":       b.EffectiveNow,
			"createdAt":           b.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			"updatedAt":           b.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return map[string]any{"ok": true, "rows": out}, nil
}

type updateBudgetInput struct {
	ID                string `json:"id"`
	MonthlyLimitPaise int64  `json:"monthly_limit_paise"`
}

func handleUpdateBudget(ctx context.Context, in updateBudgetInput) (map[string]any, error) {
	if _, err := money.CheckBudgetLimit("update_budget", in.MonthlyLimitPaise); err != nil {
		return nil, err
	}
	b, err := store.UpdateBudget(ctx, in.ID, in.MonthlyLimitPaise)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true,
		"id": b.ID, "category": b.Category, "month": b.Month,
		"monthly_limit_paise": b.MonthlyLimitPaise,
	}, nil
}

type deleteBudgetInput struct {
	ID     string `json:"id"`
	DryRun *bool  `json:"dry_run,omitempty"`
}

func handleDeleteBudget(ctx context.Context, in deleteBudgetInput) (map[string]any, error) {
	dry := true
	if in.DryRun != nil {
		dry = *in.DryRun
	}
	res, err := store.DeleteBudget(ctx, in.ID, dry)
	if err != nil {
		return nil, err
	}
	r := res.Row
	base := map[string]any{
		"ok": true, "dry_run": res.DryRun,
		"id": r.ID, "category": r.Category, "month": r.Month,
		"monthly_limit_paise": r.MonthlyLimitPaise,
	}
	if res.Unbudgeted {
		base["falls_back_to_month"] = nil
		base["falls_back_to_limit_paise"] = int64(0)
		base["unbudgeted"] = true
	} else {
		base["falls_back_to_month"] = res.FallsBackToMonth
		base["falls_back_to_limit_paise"] = res.FallsBackToLimit
		base["unbudgeted"] = false
	}
	if !dry {
		base["deleted"] = res.Deleted
	}
	return base, nil
}

type budgetStatusInput struct {
	Start      string   `json:"start"`
	End        string   `json:"end"`
	Categories []string `json:"categories,omitempty"`
	SortBy     string   `json:"sort_by,omitempty"`
	Order      string   `json:"order,omitempty"`
}

func handleBudgetStatus(ctx context.Context, in budgetStatusInput) (map[string]any, error) {
	start, end, err := normalizeDayRange("budget_status", in.Start, in.End)
	if err != nil {
		return nil, err
	}
	cats, err := store.NormalizeCategories(ctx, "budget_status", in.Categories)
	if err != nil {
		return nil, err
	}
	sortBy, err := money.NormalizeSort("budget_status", in.SortBy, "remaining", []string{"remaining", "used", "spend", "limit", "name"})
	if err != nil {
		return nil, err
	}
	order, err := money.NormalizeOrder("budget_status", in.Order, "asc")
	if err != nil {
		return nil, err
	}
	res, err := store.BudgetStatus(ctx, start, end, cats, sortBy, order)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		rows = append(rows, map[string]any{
			"category": r.Category, "budgeted": r.Budgeted,
			"limit_paise": r.LimitPaise, "spend_paise": r.SpendPaise,
			"remaining_paise":       nilIfNilI64(r.RemainingPaise),
			"used_pct":              nilIfNilF64(r.UsedPct),
			"over_budget":           r.OverBudget,
			"projected_month_paise": nilIfNilI64(r.ProjectedMonthPaise),
			"projected_over_budget": nilIfNilBool(r.ProjectedOverBudget),
		})
	}
	return map[string]any{"ok": true,
		"start": start, "end": end, "days": resDays(start, end),
		"categories_applied": res.CategoriesApplied,
		"sort_by":            res.SortBy, "order": res.Order,
		"categories":                   rows,
		"total_limit_paise":            res.TotalLimitPaise,
		"total_spend_paise":            res.TotalSpendPaise,
		"total_remaining_paise":        res.TotalRemainingPaise,
		"total_used_pct":               nilIfNilF64(res.TotalUsedPct),
		"total_over_budget":            res.TotalOverBudget,
		"budgeted_categories":          res.BudgetedCategories,
		"total_unbudgeted_spend_paise": res.TotalUnbudgetedSpendPaise,
		"projection_basis":             res.ProjectionBasis,
	}, nil
}
