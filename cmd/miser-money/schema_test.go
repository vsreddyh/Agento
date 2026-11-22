package main

import (
	"maps"
	"reflect"
	"slices"
	"testing"

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
			[]string{"type", "amount", "account"}},
		// Conditionally required: an id OR a start/end pair. Absent range
		// bounds are silently wrong (empty result, not an error), so range
		// mode needs both — but no single field can be marked required
		// without forbidding by-id mode. Enforced server-side.
		{"query_transactions", queryTransactionsInput{}, nil},
		{"edit_transaction", editTransactionInput{}, []string{"id"}},
		// "At least one filter required", so no single field can be.
		{"delete_transactions", deleteTransactionsInput{}, nil},
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
		"log_transaction":     {"sending_to", "note", "date", "idempotency_key"},
		"query_transactions":  {"id", "type", "category", "account", "limit", "offset"},
		"delete_transactions": {"id", "amount", "type", "category", "date", "account", "dry_run"},
	}
	inputs := map[string]any{
		"create_account":      createAccountInput{},
		"list_accounts":       listAccountsInput{},
		"log_transaction":     logTransactionInput{},
		"query_transactions":  queryTransactionsInput{},
		"delete_transactions": deleteTransactionsInput{},
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
