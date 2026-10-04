package main

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// Nothing in this file carried `omitempty`, so all thirty fields across ten tools were
// advertised as required. The required set below is drawn from store validation, from the
// tool descriptions, and — for `query_transactions` — from the fact that absent date
// bounds produce a silently empty result rather than an error, which is a stronger reason
// to require a field than a rejection is.
//
// Two tools deliberately require nothing: `summarize` and `delete_transactions` both take
// "at least one of" arguments, which no field can express without forbidding a legitimate
// combination. The constraint is enforced server-side and documented in the type comment.
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		// store.CreateAccount rejects an empty name; the handler defaults type to cash.
		{"create_account", createAccountInput{}, []string{"name"}},
		{"list_accounts", listAccountsInput{}, nil},
		{"archive_account", archiveAccountInput{}, []string{"name"}},
		{"get_balances", getBalancesInput{}, nil},
		// store.Insert rejects amount <= 0 and an unknown type; `category` is NOT in this
		// list because an empty or unrecognised one is filed under "other" rather than
		// rejected. `account` is rejected with a list of valid names. date is filled in
		// by the handler, and sending_to is required by the store only for a transfer.
		{"log_transaction", logTransactionInput{},
			[]string{"type", "amount", "account"}},
		{"log_text", logTextInput{}, []string{"text"}},
		// Required because absence is silently wrong, not because the store rejects it.
		{"query_transactions", queryTransactionsInput{}, []string{"start", "end"}},
		// "period phrase OR explicit start/end" — at least one, so none.
		{"summarize", summarizeInput{}, nil},
		{"fix_last_transaction", fixLastTransactionInput{}, []string{"amount"}},
		// "At least one filter required", so no single field can be.
		{"delete_transactions", deleteTransactionsInput{}, nil},
		// days=0 means 90 in the handler; dry_run=nil means dry run.
		{"prune_old", pruneOldInput{}, nil},
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
		"create_account":      {"type", "balance"},
		"list_accounts":       {"include_archived"},
		"log_transaction":     {"sending_to", "note", "date"},
		"log_text":            {"account"},
		"query_transactions":  {"type", "category", "account"},
		"summarize":           {"period", "start", "end", "account"},
		"delete_transactions": {"amount", "category", "date", "account"},
		"prune_old":           {"days", "dry_run"},
	}
	inputs := map[string]any{
		"create_account":      createAccountInput{},
		"list_accounts":       listAccountsInput{},
		"log_transaction":     logTransactionInput{},
		"log_text":            logTextInput{},
		"query_transactions":  queryTransactionsInput{},
		"summarize":           summarizeInput{},
		"delete_transactions": deleteTransactionsInput{},
		"prune_old":           pruneOldInput{},
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
