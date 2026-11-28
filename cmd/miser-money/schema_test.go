package main

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"agento/internal/mcpschema"
	"agento/internal/money"

	"github.com/google/jsonschema-go/jsonschema"
)

// Fields without `omitempty` are advertised as required. The required set below is drawn
// from store validation, from the tool descriptions, and — for `query_transactions`'
// range mode — from the fact that absent date bounds produce a silently empty result
// rather than an error, which is a stronger reason to require a field than a
// rejection is.
//
// Two tools deliberately require nothing: `delete_transactions` takes
// "at least one filter" arguments and `query_transactions` takes "an id OR a
// start/end pair", which no field can express without forbidding a
// legitimate combination. Both constraints are enforced server-side and
// documented in the type comment.
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		// store.CreateAccount rejects an empty name; the handler defaults type to cash.
		{"create_account", createAccountInput{}, []string{"name"}},
		{"edit_account", editAccountInput{}, []string{"name"}},
		{"create_category", createCategoryInput{}, []string{"name"}},
		{"list_categories", listCategoriesInput{}, nil},
		{"update_category", updateCategoryInput{}, []string{"name", "new_name"}},
		{"delete_category", deleteCategoryInput{}, []string{"name"}},
		{"list_accounts", listAccountsInput{}, nil},
		{"archive_account", archiveAccountInput{}, []string{"name"}},
		{"unarchive_account", unarchiveAccountInput{}, []string{"name"}},
		{"get_balances", getBalancesInput{}, nil},
		// store.Insert rejects amount <= 0, an unknown type, and an unknown
		// category; `category` is NOT in this list because *blank* still means
		// "other", so the field stays optional. `account` is rejected with a
		// list of valid names. date is filled in by the handler, and sending_to
		// is required by the store only for a transfer.
		{"log_transaction", logTransactionInput{},
			[]string{"type", "amount_paise", "account"}},
		// Conditionally required: an id OR a start/end pair. Absent range
		// bounds are silently wrong (empty result, not an error), so range
		// mode needs both — but no single field can be marked required
		// without forbidding by-id mode. Enforced server-side.
		{"query_transactions", queryTransactionsInput{}, nil},
		{"edit_transaction", editTransactionInput{}, []string{"id"}},
		{"restore_transactions", restoreTransactionInput{}, []string{"id"}},
		// "At least one filter required", so no single field can be.
		{"delete_transactions", deleteTransactionsInput{}, nil},
		// Analysis tools: windows are required because absent bounds are
		// silently wrong (empty result, not an error). Mode discriminators
		// (by/bucket/scope) are required because the tool cannot know which
		// table to compute without them. Conditionally-required fields
		// (side with by=category, bucket with scope=bucket) stay optional
		// in the schema and are enforced server-side with a named error.
		{"money_totals", moneyTotalsInput{}, []string{"start", "end"}},
		{"money_breakdown", moneyBreakdownInput{}, []string{"start", "end", "by"}},
		{"money_top_expenses", moneyTopExpensesInput{}, []string{"start", "end"}},
		{"money_series", moneySeriesInput{}, []string{"start", "end", "bucket"}},
		{"money_compare", moneyCompareInput{},
			[]string{"start_a", "end_a", "start_b", "end_b", "by"}},
		{"money_recurring", moneyRecurringInput{}, []string{"start", "end"}},
		{"money_anomalies", moneyAnomaliesInput{}, []string{"start", "end", "scope"}},
		{"budget_status", budgetStatusInput{}, []string{"start", "end"}},
		{"list_budgets", listBudgetsInput{}, nil},
		{"create_budget", createBudgetInput{}, []string{"category", "month", "monthly_limit_paise"}},
		{"update_budget", updateBudgetInput{}, []string{"id", "monthly_limit_paise"}},
		{"delete_budget", deleteBudgetInput{}, []string{"id"}},
	}

	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			schema := schemaFor(t, c.input)
			got := slices.Clone(schema.Required)
			slices.Sort(got)
			want := slices.Clone(c.required)
			slices.Sort(want)

			if !slices.Equal(got, want) {
				t.Errorf("required = %v, want %v", got, want)
			}
			for _, name := range want {
				prop, ok := schema.Properties[name]
				if !ok {
					t.Errorf("required field %q has no property in the schema", name)
					continue
				}
				// A pointer field is emitted as a union — `["null","boolean"]` — so
				// Type is empty and Types is populated. Checking Type alone would fail
				// every optional-typed field in this file.
				if prop.Type == "" && len(prop.Types) == 0 && prop.Ref == "" {
					t.Errorf("required field %q has no type (Type=%q Types=%v Ref=%q)",
						name, prop.Type, prop.Types, prop.Ref)
				}
			}
		})
	}
}

// Optional must not mean invisible. `delete_transactions` is the one to watch: it deletes
// and inverts balances, and its filters are all optional, so a caller who cannot see them
// has no way to narrow a delete.
func TestOptionalFieldsAreStillDocumented(t *testing.T) {
	optional := map[string][]string{
		"create_account":      {"type", "balance_paise"},
		"edit_account":        {"new_name", "type", "opening_balance_paise"},
		"create_category":     {},
		"list_categories":     {},
		"list_accounts":       {"include_archived"},
		"log_transaction":     {"sending_to", "note", "date", "idempotency_key"},
		"query_transactions":  {"id", "type", "category", "account", "sort_by", "sort", "limit", "offset"},
		"delete_transactions": {"id", "amount_paise", "type", "category", "date", "account", "dry_run"},
		"delete_category":     {"reassign_to", "dry_run"},
		"money_totals":        {"category", "account"},
		"money_breakdown":     {"side", "categories", "accounts", "sort_by", "order"},
		"money_top_expenses":  {"category", "account", "limit", "sort_by", "order"},
		"money_series":        {"category", "account", "order"},
		"money_compare":       {"category", "categories", "account", "sort_by", "order"},
		"money_recurring":     {"category", "account", "min_occurrences", "amount_tolerance_pct", "limit", "sort_by", "order"},
		"money_anomalies":     {"category", "account", "sensitivity", "min_amount_paise", "bucket", "sort_by", "order"},
		"budget_status":       {"categories", "sort_by", "order"},
		"list_budgets":        {"month_from", "month_to", "category"},
		"delete_budget":       {"dry_run"},
	}
	inputs := map[string]any{
		"create_account":      createAccountInput{},
		"list_accounts":       listAccountsInput{},
		"log_transaction":     logTransactionInput{},
		"query_transactions":  queryTransactionsInput{},
		"delete_transactions": deleteTransactionsInput{},
		"money_totals":        moneyTotalsInput{},
		"money_breakdown":     moneyBreakdownInput{},
		"money_top_expenses":  moneyTopExpensesInput{},
		"money_series":        moneySeriesInput{},
		"money_compare":       moneyCompareInput{},
		"money_recurring":     moneyRecurringInput{},
		"money_anomalies":     moneyAnomaliesInput{},
		"budget_status":       budgetStatusInput{},
		"list_budgets":        listBudgetsInput{},
		"delete_budget":       deleteBudgetInput{},
		"edit_account":        editAccountInput{},
		"create_category":     createCategoryInput{},
		"list_categories":     listCategoriesInput{},
		"update_category":     updateCategoryInput{},
		"delete_category":     deleteCategoryInput{},
	}

	for tool, names := range optional {
		t.Run(tool, func(t *testing.T) {
			schema := schemaFor(t, inputs[tool])
			for _, name := range names {
				if slices.Contains(schema.Required, name) {
					t.Errorf("%s is required but should be optional", name)
				}
				if _, ok := schema.Properties[name]; !ok {
					t.Errorf("%s is missing from the schema entirely", name)
				}
			}
		})
	}
}

// get_balances takes no arguments and is the only tool here that does. If that ever
// changes by accident, a caller would be sending fields the handler ignores.
func TestGetBalancesTakesNoArguments(t *testing.T) {
	schema := schemaFor(t, getBalancesInput{})
	if len(schema.Properties) != 0 {
		t.Errorf("get_balances accepts %v but its handler ignores all of it",
			slices.Collect(maps.Keys(schema.Properties)))
	}
}

// schemaFor asks the SDK's own inferencer for a type's schema — the same call AddTool
// makes, so this asserts the published contract rather than a hand-written copy of it.
func schemaFor(t *testing.T, input any) *jsonschema.Schema {
	t.Helper()
	schema, err := jsonschema.ForType(reflect.TypeOf(input), nil)
	if err != nil {
		t.Fatalf("inferring schema: %v", err)
	}
	return schema
}

// Sort is two enums, not one string: sort_by names the field (date|amount),
// sort the direction (asc|desc). Both are published in the schema so unknown
// values throw before the handler; the store re-validates for direct callers.
func TestQuerySortEnumsPublished(t *testing.T) {
	schema := mcpschema.EnumSchema(queryTransactionsInput{},
		map[string][]string{"sort_by": money.TxSortFields, "sort": {"asc", "desc"}})
	for field, want := range map[string][]string{"sort_by": money.TxSortFields, "sort": {"asc", "desc"}} {
		prop, ok := schema.Properties[field]
		if !ok {
			t.Fatalf("%s missing from schema", field)
		}
		var got []string
		for _, v := range prop.Enum {
			s, ok := v.(string)
			if !ok {
				t.Fatalf("%s enum value %v is not a string", field, v)
			}
			got = append(got, s)
		}
		slices.Sort(got)
		wantSorted := slices.Clone(want)
		slices.Sort(wantSorted)
		if !slices.Equal(got, wantSorted) {
			t.Errorf("%s enum = %v, want %v", field, got, wantSorted)
		}
	}
}
